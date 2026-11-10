package aws

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/engines/vm"
)

// EC2 instances, which really run: one container per instance, on the
// cloud-neutral engine in internal/engines/vm.
//
// Measured against aws provider 5.100.0 rather than written from the
// documentation, and two things about that are worth knowing.
//
// **The provider never calls DescribeImages.** It takes the AMI id entirely
// on faith and goes straight from the security group to RunInstances, so
// there is no image catalogue to model and nothing to look an id up in.
// Which also means an AMI id cannot tell us what to run: it is opaque. The
// container image is DefaultInstanceImage unless the instance carries the
// nimbus:image tag, which is the escape hatch for naming a real one.
//
// **RunInstances is a reservation, and DescribeInstances nests twice.**
// Instances come back inside reservationSet>item>instancesSet>item; a
// client parsing one level finds nothing and reports no instances, with no
// error to point at.
//
// What is NOT emulated: there is no kernel of its own, no block device, no
// EBS volume, no AMI, no instance profile credentials and no sshd -- Nimbus
// is stdlib-only and cannot speak SSH, so `nimbus ssh` gives a shell
// through docker exec instead. Instance types are accepted and recorded;
// nothing limits CPU or memory to match one.

const typeInstance = "AWS::EC2::Instance"

// DefaultInstanceImage backs an instance whose tag does not name one.
// Alpine because it is tiny, starts in about a second, and its busybox
// provides the `base64 -d` and `tee` the engine's bootstrap needs.
const DefaultInstanceImage = "alpine:3.20"

// instanceImageTag names the container image for an instance. An AMI id is
// opaque -- the provider never asks what one means, so neither can we --
// and this is how a config says "really run this".
const instanceImageTag = "nimbus:image"

// EC2's instance states, with the codes real EC2 uses. Some clients switch
// on the code rather than the name, and the provider's waiters read the
// name, so both have to be right.
const (
	statePending      = "pending"
	stateRunning      = "running"
	stateShuttingDown = "shutting-down"
	stateStopping     = "stopping"
	stateStopped      = "stopped"
)

func stateCode(name string) int {
	switch name {
	case statePending:
		return 0
	case stateRunning:
		return 16
	case stateShuttingDown:
		return 32
	case "terminated":
		return 48
	case stateStopping:
		return 64
	case stateStopped:
		return 80
	}
	return 0
}

type instanceState struct {
	Code int    `xml:"code" json:"code"`
	Name string `xml:"name" json:"name"`
}

type groupRef struct {
	GroupID   string `xml:"groupId" json:"groupId"`
	GroupName string `xml:"groupName,omitempty" json:"groupName,omitempty"`
}

type placement struct {
	AvailabilityZone string `xml:"availabilityZone" json:"availabilityZone"`
	GroupName        string `xml:"groupName" json:"groupName"`
	Tenancy          string `xml:"tenancy" json:"tenancy"`
}

type instance struct {
	InstanceID   string        `xml:"instanceId" json:"instanceId"`
	ImageID      string        `xml:"imageId" json:"imageId"`
	State        instanceState `xml:"instanceState" json:"instanceState"`
	PrivateDNS   string        `xml:"privateDnsName" json:"privateDnsName"`
	PublicDNS    string        `xml:"dnsName" json:"dnsName"`
	KeyName      string        `xml:"keyName,omitempty" json:"keyName,omitempty"`
	AMILaunchIdx int           `xml:"amiLaunchIndex" json:"amiLaunchIndex"`
	InstanceType string        `xml:"instanceType" json:"instanceType"`
	LaunchTime   string        `xml:"launchTime" json:"launchTime"`
	Placement    placement     `xml:"placement" json:"placement"`
	Monitoring   struct {
		State string `xml:"state" json:"state"`
	} `xml:"monitoring" json:"monitoring"`
	SubnetID  string     `xml:"subnetId" json:"subnetId"`
	VpcID     string     `xml:"vpcId" json:"vpcId"`
	PrivateIP string     `xml:"privateIpAddress" json:"privateIpAddress"`
	Groups    []groupRef `xml:"groupSet>item" json:"groupSet"`

	Architecture       string `xml:"architecture" json:"architecture"`
	RootDeviceType     string `xml:"rootDeviceType" json:"rootDeviceType"`
	RootDeviceName     string `xml:"rootDeviceName" json:"rootDeviceName"`
	VirtualizationType string `xml:"virtualizationType" json:"virtualizationType"`
	Hypervisor         string `xml:"hypervisor" json:"hypervisor"`
	EbsOptimized       bool   `xml:"ebsOptimized" json:"ebsOptimized"`
	EnaSupport         bool   `xml:"enaSupport" json:"enaSupport"`
	SourceDestCheck    bool   `xml:"sourceDestCheck" json:"sourceDestCheck"`
	ClientToken        string `xml:"clientToken,omitempty" json:"clientToken,omitempty"`
	TagSet             []tag  `xml:"tagSet>item" json:"tagSet"`

	// ReservationID groups instances a single RunInstances created, which
	// is the level DescribeInstances answers at.
	ReservationID string `xml:"-" json:"reservationId"`
	// UserData is kept decoded, because that is what a boot script is.
	// Nothing re-encodes it until the engine puts it in the container.
	UserData string `xml:"-" json:"userData,omitempty"`
	// CPUCredits is the burstable credit mode RunInstances sent, read back
	// by DescribeInstanceCreditSpecifications and by nothing else.
	CPUCredits string `xml:"-" json:"cpuCredits,omitempty"`
	// Container is which Docker container backs this instance, recorded so
	// `nimbus ssh` -- a separate process, which cannot see the engine --
	// can find it. JSON only: it never appears on the EC2 wire.
	//
	// Written by the same code that starts the container, so it is not a
	// second thing to keep in step; and named for what any cloud would
	// call it, so the CLI needs no per-cloud table when GCP and Azure
	// record theirs the same way.
	Container string `xml:"-" json:"nimbusContainer,omitempty"`
}

func (i *instance) id() string      { return i.InstanceID }
func (i *instance) tags() []tag     { return i.TagSet }
func (i *instance) setTags(t []tag) { i.TagSet = t }
func (i *instance) field(name string) ([]string, bool) {
	switch name {
	case "instance-id":
		return []string{i.InstanceID}, true
	case "instance-state-name":
		return []string{i.State.Name}, true
	case "instance-state-code":
		return []string{strconv.Itoa(i.State.Code)}, true
	case "image-id":
		return []string{i.ImageID}, true
	case "instance-type":
		return []string{i.InstanceType}, true
	case "subnet-id":
		return []string{i.SubnetID}, true
	case "vpc-id":
		return []string{i.VpcID}, true
	case "private-ip-address":
		return []string{i.PrivateIP}, true
	case "key-name":
		return []string{i.KeyName}, true
	case "availability-zone", "placement-availability-zone":
		return []string{i.Placement.AvailabilityZone}, true
	}
	return nil, false
}

// imageFor picks the container image. Tags are the only input: an AMI id
// says nothing, since the provider never asks what one means.
func imageFor(tags []tag) string {
	for _, t := range tags {
		if strings.EqualFold(t.Key, instanceImageTag) && t.Value != "" {
			return t.Value
		}
	}
	return DefaultInstanceImage
}

type runInstancesResponse struct {
	XMLName       xml.Name   `xml:"RunInstancesResponse"`
	Xmlns         string     `xml:"xmlns,attr"`
	RequestID     string     `xml:"requestId"`
	ReservationID string     `xml:"reservationId"`
	OwnerID       string     `xml:"ownerId"`
	Groups        []groupRef `xml:"groupSet>item,omitempty"`
	Instances     []instance `xml:"instancesSet>item"`
}

func (s *Server) runInstances(w http.ResponseWriter, p params) *apiError {
	imageID := p.str("ImageId")
	if imageID == "" {
		return errorf(400, "MissingParameter", "the request must contain the parameter ImageId")
	}
	subnetID := p.str("SubnetId")
	var sn subnet
	if subnetID != "" {
		got, err := load(s, subnetID, &subnet{})
		if err != nil || got.SubnetID == "" {
			return notFound(typeSubnet, subnetID)
		}
		sn = *got
	}

	count := p.intOr("MaxCount", 1)
	if count < 1 {
		count = 1
	}
	tags := readTagSpecifications(p)
	groups := make([]groupRef, 0, 2)
	for _, id := range p.list("SecurityGroupId", "") {
		groups = append(groups, groupRef{GroupID: id})
	}

	// The provider sends user data base64-encoded. It is decoded here and
	// stays decoded: a boot script is text, and the engine re-encodes it
	// only to get it into the container without a shell parsing it.
	userData := p.str("UserData")
	if decoded, err := base64.StdEncoding.DecodeString(userData); err == nil {
		userData = string(decoded)
	}

	keyName := p.str("KeyName")
	var keys []string
	if k := keyPairByName(s, keyName); k != nil && k.PublicKey != "" {
		keys = []string{k.PublicKey}
	}

	reservation := newID("r")
	out := make([]instance, 0, count)
	for n := 0; n < count; n++ {
		i := &instance{
			InstanceID:   newID("i"),
			ImageID:      imageID,
			State:        instanceState{Code: stateCode(statePending), Name: statePending},
			KeyName:      keyName,
			AMILaunchIdx: n,
			InstanceType: p.strOr("InstanceType", "t3.micro"),
			LaunchTime:   time.Now().UTC().Format(time.RFC3339),
			Placement: placement{
				AvailabilityZone: azOr(p.str("Placement.AvailabilityZone"), sn.AvailabilityZone, s.cfg.Region+"a"),
				Tenancy:          "default",
			},
			SubnetID:      subnetID,
			VpcID:         sn.VpcID,
			Groups:        groups,
			TagSet:        tags,
			ReservationID: reservation,
			UserData:      userData,

			// Fixed shapes a client reads but nothing here varies.
			Architecture:       "x86_64",
			RootDeviceType:     "ebs",
			RootDeviceName:     "/dev/xvda",
			VirtualizationType: "hvm",
			Hypervisor:         "xen",
			EbsOptimized:       p.str("EbsOptimized") == "true",
			EnaSupport:         true,
			SourceDestCheck:    true,
			ClientToken:        p.str("ClientToken"),
			CPUCredits:         p.strOr("CreditSpecification.CpuCredits", defaultCredits(p.strOr("InstanceType", "t3.micro"))),
		}
		i.Monitoring.State = "disabled"
		// Derived, not allocated: the provider reads private_ip on every
		// refresh, so a value that differed between reads would be a plan
		// that never settles.
		i.PrivateIP = privateIPFor(sn.CidrBlock, i.InstanceID)
		i.PrivateDNS = privateDNSFor(i.PrivateIP, s.cfg.Region)

		if err := s.save(i, typeInstance, nameOf(i), subnetID); err != nil {
			return errorf(500, "InternalError", "storing the instance: %v", err)
		}
		go s.bootInstance(i, keys)
		out = append(out, *i)
	}

	writeXML(w, 200, runInstancesResponse{
		Xmlns: namespaceFor("ec2"), RequestID: requestID(),
		ReservationID: reservation, OwnerID: s.cfg.AccountID,
		Groups: groups, Instances: out,
	})
	return nil
}

// bootInstance starts the container and moves the instance from pending to
// running, which is what those states mean. The provider's waiter polls
// DescribeInstances for exactly this.
//
// Under s.vmMu for its whole length, because a terminate that arrives
// meanwhile must not be overtaken: the check and the act have to be one
// step, or a destroy reports success and leaves a container running. That
// is the lesson the GKE node pool paid for.
func (s *Server) bootInstance(i *instance, keys []string) {
	s.vmMu.Lock()
	defer s.vmMu.Unlock()
	if !s.store.Exists(i.InstanceID) {
		return // terminated before it finished booting
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	m := &vm.Instance{
		ResourceID: i.InstanceID,
		Name:       nameOf(i),
		Image:      imageFor(i.TagSet),
		UserData:   i.UserData,
		PublicKeys: keys,
		Metadata:   s.instanceMetadata(i.InstanceID),
		MetadataEnv: func(endpoint, _ string) map[string]string {
			// Every current SDK reads this, so a program inside the
			// instance can find out which instance it is -- as close to
			// IMDS as a container gets, since 169.254.169.254 needs root.
			return map[string]string{"AWS_EC2_METADATA_SERVICE_ENDPOINT": endpoint}
		},
	}
	if err := s.vms.Start(ctx, m); err != nil {
		log.Printf("nimbus: instance %s failed to start: %v", i.InstanceID, err)
		s.setInstanceState(i.InstanceID, stateStopped)
		return
	}
	// One Update for both, so a reader never sees a running instance with
	// no container recorded.
	_ = s.store.Update(i.InstanceID, func(body map[string]any) error {
		body["instanceState"] = map[string]any{"code": stateCode(stateRunning), "name": stateRunning}
		body["nimbusContainer"] = m.Container
		return nil
	})
}

// setInstanceState moves the stored state along, through Store.Update so
// the read, the change and the write are one step: a plain Get-then-Put
// loses whatever else wrote in between.
func (s *Server) setInstanceState(id, name string) {
	_ = s.store.Update(id, func(body map[string]any) error {
		body["instanceState"] = map[string]any{"code": stateCode(name), "name": name}
		return nil
	})
}

type describeInstancesResponse struct {
	XMLName      xml.Name      `xml:"DescribeInstancesResponse"`
	Xmlns        string        `xml:"xmlns,attr"`
	RequestID    string        `xml:"requestId"`
	Reservations []reservation `xml:"reservationSet>item"`
}

type reservation struct {
	ReservationID string     `xml:"reservationId"`
	OwnerID       string     `xml:"ownerId"`
	Groups        []groupRef `xml:"groupSet>item,omitempty"`
	Instances     []instance `xml:"instancesSet>item"`
}

func (s *Server) describeInstances(w http.ResponseWriter, p params) *apiError {
	wanted := p.list("InstanceId", "")
	fs := readFilters(p)

	// Grouped by reservation, in the order the reservations were created,
	// because DescribeInstances answers at that level and not at the
	// instance level.
	var order []string
	byReservation := map[string][]instance{}
	for _, i := range list[instance](s, typeInstance) {
		i := i
		if len(wanted) > 0 && !contains(wanted, i.InstanceID) {
			continue
		}
		if !matches(&i, fs) {
			continue
		}
		r := i.ReservationID
		if _, seen := byReservation[r]; !seen {
			order = append(order, r)
		}
		byReservation[r] = append(byReservation[r], i)
	}
	if len(wanted) > 0 && len(byReservation) == 0 {
		return notFound(typeInstance, wanted[0])
	}

	out := make([]reservation, 0, len(order))
	for _, r := range order {
		out = append(out, reservation{
			ReservationID: r, OwnerID: s.cfg.AccountID, Instances: byReservation[r],
		})
	}
	writeXML(w, 200, describeInstancesResponse{
		Xmlns: namespaceFor("ec2"), RequestID: requestID(), Reservations: out,
	})
	return nil
}

// instanceStateChange is the shape stop, start and terminate all answer
// with: what the state was and what it is becoming.
type instanceStateChange struct {
	InstanceID    string        `xml:"instanceId"`
	CurrentState  instanceState `xml:"currentState"`
	PreviousState instanceState `xml:"previousState"`
}

type stateChangeResponse struct {
	XMLName   xml.Name              `xml:"-"`
	Xmlns     string                `xml:"xmlns,attr"`
	RequestID string                `xml:"requestId"`
	Changes   []instanceStateChange `xml:"instancesSet>item"`
}

// changeInstances is stop, start, reboot and terminate: each reads the
// instances, answers at once with the state they are moving to, and does
// the slow part in the background, as EC2 does.
func (s *Server) changeInstances(w http.ResponseWriter, p params, action string) *apiError {
	ids := p.list("InstanceId", "")
	if len(ids) == 0 {
		return errorf(400, "MissingParameter",
			"the request must contain the parameter InstanceId")
	}

	var changes []instanceStateChange
	for _, id := range ids {
		i, err := load(s, id, &instance{})
		if err != nil || i.InstanceID == "" {
			return notFound(typeInstance, id)
		}
		var next string
		switch action {
		case "StopInstances":
			next = stateStopping
		case "StartInstances":
			next = statePending
		case "TerminateInstances":
			next = stateShuttingDown
		case "RebootInstances":
			next = stateRunning
		}
		changes = append(changes, instanceStateChange{
			InstanceID: id, PreviousState: i.State,
			CurrentState: instanceState{Code: stateCode(next), Name: next},
		})
		if action != "RebootInstances" {
			s.setInstanceState(id, next)
		}
		go s.applyInstanceChange(id, action)
	}

	// RebootInstances answers with a bare return flag, not a state set.
	if action == "RebootInstances" {
		writeEmpty(w, "RebootInstancesResponse")
		return nil
	}
	doc := stateChangeResponse{
		Xmlns: namespaceFor("ec2"), RequestID: requestID(), Changes: changes,
	}
	doc.XMLName = xml.Name{Local: action + "Response"}
	writeXML(w, 200, doc)
	return nil
}

// applyInstanceChange does the container work and then records the state it
// reached. Under s.vmMu, so a terminate and a stop cannot interleave.
func (s *Server) applyInstanceChange(id, action string) {
	s.vmMu.Lock()
	defer s.vmMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	m := s.vms.Get(id)
	if m == nil {
		// Nothing is running for it. Terminate still has to finish, so the
		// resource reaches its final state.
		if action == "TerminateInstances" {
			s.retireInstance(id)
		}
		return
	}

	switch action {
	case "StopInstances":
		if err := s.vms.Stop(ctx, m); err != nil {
			log.Printf("nimbus: stopping instance %s: %v", id, err)
		}
		s.setInstanceState(id, stateStopped)
	case "StartInstances":
		if err := s.vms.Resume(ctx, m); err != nil {
			log.Printf("nimbus: starting instance %s: %v", id, err)
			s.setInstanceState(id, stateStopped)
			return
		}
		s.setInstanceState(id, stateRunning)
	case "RebootInstances":
		if err := s.vms.Restart(ctx, m); err != nil {
			log.Printf("nimbus: rebooting instance %s: %v", id, err)
		}
	case "TerminateInstances":
		if err := s.vms.Delete(ctx, m); err != nil {
			log.Printf("nimbus: terminating instance %s: %v", id, err)
		}
		s.retireInstance(id)
	}
}

// privateIPFor derives an address inside the subnet. Deterministic, so it
// is the same on every read, and above .3 because EC2 reserves the first
// four addresses of a subnet.
func privateIPFor(cidr, instanceID string) string {
	base := [4]int{10, 0, 0, 0}
	if cidr != "" {
		if _, err := fmt.Sscanf(cidr, "%d.%d.%d.%d", &base[0], &base[1], &base[2], &base[3]); err != nil {
			return ""
		}
	}
	// 4..253 of the subnet's third-octet block: enough room, and never a
	// reserved address or the broadcast one.
	host := 4 + int(stableByte(instanceID))%250
	return fmt.Sprintf("%d.%d.%d.%d", base[0], base[1], base[2], host)
}

func privateDNSFor(ip, region string) string {
	if ip == "" {
		return ""
	}
	// us-east-1 uses the legacy ec2.internal; every other region uses
	// <region>.compute.internal.
	domain := region + ".compute.internal"
	if region == "us-east-1" {
		domain = "ec2.internal"
	}
	return "ip-" + strings.ReplaceAll(ip, ".", "-") + "." + domain
}

func stableByte(s string) byte {
	h := stableID(s, 2)
	n, err := strconv.ParseUint(h, 16, 16)
	if err != nil {
		return 0
	}
	return byte(n % 256)
}

func azOr(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// DescribeInstanceAttribute asks for ONE attribute at a time, the way
// DescribeVpcAttribute does, and the provider makes several calls after a
// create -- a 400 on any one fails the whole apply.
//
// Unlike a VPC's, these attributes are not all the same type:
// instanceInitiatedShutdownBehavior is a string, disableApiTermination a
// boolean, userData base64 text and groupSet a LIST. XML has no types, so
// everything except groupSet is written as the text of a <value> element,
// which a client reads back as whatever it expects. groupSet needs its own
// shape: an empty <value> there would read as "this instance has no
// security groups" and plan a change to put them back.
//
// An attribute nobody has modelled answers with an empty value rather than
// refusing -- the list only grows with provider versions, exactly as it
// does for a VPC.
type describeInstanceAttributeResponse struct {
	XMLName    xml.Name     `xml:"DescribeInstanceAttributeResponse"`
	Xmlns      string       `xml:"xmlns,attr"`
	RequestID  string       `xml:"requestId"`
	InstanceID string       `xml:"instanceId"`
	Value      *stringValue `xml:",omitempty"`
	Groups     []groupRef   `xml:"groupSet>item,omitempty"`
}

type stringValue struct {
	XMLName xml.Name
	Value   string `xml:"value"`
}

func (s *Server) describeInstanceAttribute(w http.ResponseWriter, p params) *apiError {
	id := p.str("InstanceId")
	i, err := load(s, id, &instance{})
	if err != nil || i.InstanceID == "" {
		return notFound(typeInstance, id)
	}
	attr := p.str("Attribute")
	if attr == "" {
		return errorf(400, "MissingParameter", "the request must contain the parameter Attribute")
	}

	doc := describeInstanceAttributeResponse{
		Xmlns: namespaceFor("ec2"), RequestID: requestID(), InstanceID: i.InstanceID,
	}
	if attr == "groupSet" {
		doc.Groups = i.Groups
		writeXML(w, 200, doc)
		return nil
	}

	values := map[string]string{
		"instanceType":                      i.InstanceType,
		"kernel":                            "",
		"ramdisk":                           "",
		"userData":                          base64.StdEncoding.EncodeToString([]byte(i.UserData)),
		"disableApiTermination":             "false",
		"disableApiStop":                    "false",
		"instanceInitiatedShutdownBehavior": "stop",
		"rootDeviceName":                    i.RootDeviceName,
		"blockDeviceMapping":                "",
		"productCodes":                      "",
		"sourceDestCheck":                   boolString(i.SourceDestCheck),
		"ebsOptimized":                      boolString(i.EbsOptimized),
		"sriovNetSupport":                   "",
		"enaSupport":                        boolString(i.EnaSupport),
		"enclaveOptions":                    "false",
	}
	doc.Value = &stringValue{XMLName: xml.Name{Local: attr}, Value: values[attr]}
	writeXML(w, 200, doc)
	return nil
}

// DescribeInstanceCreditSpecifications is another read-back after a create:
// the burstable CPU credit mode. RunInstances sends it as
// CreditSpecification.CpuCredits, so this answers what the client asked
// for rather than inventing a default -- a value that differed from the
// request would be a diff on every plan.
//
// Nothing here throttles an instance. A t3 in Nimbus is a container with
// whatever CPU the machine has; the mode round-trips so a plan settles.
type describeCreditSpecificationsResponse struct {
	XMLName   xml.Name              `xml:"DescribeInstanceCreditSpecificationsResponse"`
	Xmlns     string                `xml:"xmlns,attr"`
	RequestID string                `xml:"requestId"`
	Specs     []creditSpecification `xml:"instanceCreditSpecificationSet>item"`
}

type creditSpecification struct {
	InstanceID string `xml:"instanceId"`
	CPUCredits string `xml:"cpuCredits"`
}

func (s *Server) describeCreditSpecifications(w http.ResponseWriter, p params) *apiError {
	wanted := p.list("InstanceId", "")
	out := []creditSpecification{}
	for _, i := range list[instance](s, typeInstance) {
		if len(wanted) > 0 && !contains(wanted, i.InstanceID) {
			continue
		}
		credits := i.CPUCredits
		if credits == "" {
			credits = defaultCredits(i.InstanceType)
		}
		out = append(out, creditSpecification{InstanceID: i.InstanceID, CPUCredits: credits})
	}
	if len(wanted) > 0 && len(out) == 0 {
		return notFound(typeInstance, wanted[0])
	}
	writeXML(w, 200, describeCreditSpecificationsResponse{
		Xmlns: namespaceFor("ec2"), RequestID: requestID(), Specs: out,
	})
	return nil
}

// defaultCredits is what a burstable family gets when nothing said: t2 is
// "standard", every later t family is "unlimited". A non-burstable type has
// no credit mode at all.
func defaultCredits(instanceType string) string {
	switch {
	case strings.HasPrefix(instanceType, "t2."):
		return "standard"
	case strings.HasPrefix(instanceType, "t"):
		return "unlimited"
	}
	return ""
}

// terminatedRetention is how long a terminated instance stays describable.
//
// Real EC2 keeps one visible for about an hour. That is not a detail to
// skip, because the PROVIDER DEPENDS ON IT: its delete waiter polls
// DescribeInstances until it sees state "terminated", and does NOT accept
// a not-found as success. Measured -- deleting the record immediately
// failed a destroy after three minutes:
//
//	waiting for EC2 Instance (i-0fa5...) delete:
//	couldn't find resource (21 retries)
//
// So the record outlives the container, and is swept afterwards rather
// than kept for an hour: long enough for any waiter, short enough that a
// destroyed instance does not sit in the graph.
const terminatedRetention = 2 * time.Minute

// retireInstance puts an instance in its final state and schedules the
// record's removal.
func (s *Server) retireInstance(id string) {
	s.setInstanceState(id, "terminated")
	time.AfterFunc(terminatedRetention, func() { _, _ = s.store.Delete(id) })
}
