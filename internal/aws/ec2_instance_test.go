package aws

import (
	"encoding/base64"
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// The exact body aws provider 5.100.0 sends for an aws_instance, captured
// from a debug log rather than written from the documentation:
//
//	Action=RunInstances&ClientToken=terraform-2026...&CreditSpecification.CpuCredits=unlimited
//	&DisableApiTermination=false&EbsOptimized=false&HibernationOptions.Configured=false
//	&ImageId=ami-0abcdef1234567890&InstanceType=t3.micro&KeyName=measure-key
//	&MaxCount=1&MinCount=1&Monitoring.Enabled=false&Placement.AvailabilityZone=
//	&SecurityGroupId.1=sg-...&SubnetId=subnet-...&TagSpecification.1.ResourceType=instance
//	&TagSpecification.1.Tag.1.Key=Name&TagSpecification.1.Tag.1.Value=measure-web
//	&UserData=IyEvYmluL3NoCmVjaG8g...
//
// Worth noting what is NOT there: the provider never calls DescribeImages.
// It takes the AMI id entirely on faith, so there is nothing to look up and
// no image catalogue to model.
func runInstancesBody(subnet, sg, key string) string {
	return "Action=RunInstances" +
		"&ClientToken=terraform-20261110011137894700000001" +
		"&CreditSpecification.CpuCredits=unlimited" +
		"&DisableApiTermination=false&EbsOptimized=false" +
		"&HibernationOptions.Configured=false" +
		"&ImageId=ami-0abcdef1234567890&InstanceType=t3.micro" +
		"&KeyName=" + key +
		"&MaxCount=1&MinCount=1&Monitoring.Enabled=false" +
		"&Placement.AvailabilityZone=" +
		"&SecurityGroupId.1=" + sg +
		"&SubnetId=" + subnet +
		"&TagSpecification.1.ResourceType=instance" +
		"&TagSpecification.1.Tag.1.Key=Name&TagSpecification.1.Tag.1.Value=measure-web" +
		"&UserData=" + base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\necho hi\n"))
}

// instanceServer is a server whose vm engine runs on the Fake, so nothing
// here starts a container.
func instanceServer(t *testing.T) (*Server, *docker.Fake) {
	t.Helper()
	s := newTestServer(t)
	f := docker.NewFake()
	f.Images[DefaultInstanceImage] = true
	s.vms.SetDocker(f)
	return s, f
}

type xmlInstance struct {
	InstanceID string `xml:"instanceId"`
	ImageID    string `xml:"imageId"`
	State      struct {
		Code int    `xml:"code"`
		Name string `xml:"name"`
	} `xml:"instanceState"`
	InstanceType string `xml:"instanceType"`
	SubnetID     string `xml:"subnetId"`
	VpcID        string `xml:"vpcId"`
	PrivateIP    string `xml:"privateIpAddress"`
	KeyName      string `xml:"keyName"`
	Groups       []struct {
		ID string `xml:"groupId"`
	} `xml:"groupSet>item"`
	Tags []struct {
		Key   string `xml:"key"`
		Value string `xml:"value"`
	} `xml:"tagSet>item"`
}

func runOneInstance(t *testing.T, s *Server) (id, subnet, sg string) {
	t.Helper()
	vpc := makeVPC(t, s, "10.0.0.0/16", "main")
	subnet = makeSubnet(t, s, vpc, "10.0.1.0/24")
	sg = makeSG(t, s, vpc, "ssh")
	importKey(t, s, "measure-key", testPubKey)

	body := mustEC2(t, s, runInstancesBody(subnet, sg, "measure-key"))
	var got struct {
		XMLName       xml.Name      `xml:"RunInstancesResponse"`
		ReservationID string        `xml:"reservationId"`
		Instances     []xmlInstance `xml:"instancesSet>item"`
	}
	if err := xml.Unmarshal(body, &got); err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	if !strings.HasPrefix(got.ReservationID, "r-0") {
		t.Errorf("reservationId = %q, want an r-0 prefix", got.ReservationID)
	}
	if len(got.Instances) != 1 {
		t.Fatalf("RunInstances returned %d instances, want 1:\n%s", len(got.Instances), body)
	}
	return got.Instances[0].InstanceID, subnet, sg
}

func TestRunInstancesAnswersWhatTheProviderSent(t *testing.T) {
	s, _ := instanceServer(t)
	id, subnet, sg := runOneInstance(t, s)

	if !strings.HasPrefix(id, "i-0") {
		t.Errorf("instance id = %q, want the i-0 prefix of a modern id", id)
	}

	i := describeOneInstance(t, s, id)
	if i.ImageID != "ami-0abcdef1234567890" {
		t.Errorf("imageId = %q", i.ImageID)
	}
	if i.InstanceType != "t3.micro" {
		t.Errorf("instanceType = %q", i.InstanceType)
	}
	if i.SubnetID != subnet {
		t.Errorf("subnetId = %q, want %q", i.SubnetID, subnet)
	}
	if i.VpcID == "" {
		t.Error("vpcId is empty; the provider reads it and would show a diff")
	}
	if i.KeyName != "measure-key" {
		t.Errorf("keyName = %q", i.KeyName)
	}
	if len(i.Groups) != 1 || i.Groups[0].ID != sg {
		t.Errorf("groupSet = %+v, want just %s", i.Groups, sg)
	}
	if len(i.Tags) != 1 || i.Tags[0].Value != "measure-web" {
		t.Errorf("tagSet = %+v", i.Tags)
	}
}

// DescribeInstances nests twice: reservations, and instances inside them.
// A client parsing one level finds nothing and reports no instances, with
// no error to point at.
func TestDescribeInstancesNestsInstancesInsideReservations(t *testing.T) {
	s, _ := instanceServer(t)
	id, _, _ := runOneInstance(t, s)

	body := mustEC2(t, s, "Action=DescribeInstances")
	var got struct {
		XMLName      xml.Name `xml:"DescribeInstancesResponse"`
		Reservations []struct {
			ReservationID string        `xml:"reservationId"`
			OwnerID       string        `xml:"ownerId"`
			Instances     []xmlInstance `xml:"instancesSet>item"`
		} `xml:"reservationSet>item"`
	}
	if err := xml.Unmarshal(body, &got); err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	if len(got.Reservations) != 1 {
		t.Fatalf("reservationSet has %d items:\n%s", len(got.Reservations), body)
	}
	r := got.Reservations[0]
	if r.OwnerID == "" || r.ReservationID == "" {
		t.Errorf("reservation = %+v, want an owner and an id", r)
	}
	if len(r.Instances) != 1 || r.Instances[0].InstanceID != id {
		t.Errorf("instancesSet = %+v, want just %s", r.Instances, id)
	}
}

// An instance's private IP is DERIVED from its id inside its subnet's
// CIDR, never allocated at random: the provider reads private_ip on every
// refresh, so a value that differed between reads is a plan that never
// settles. The Content-MD5 lesson again.
func TestAPrivateIPIsDerivedAndInsideTheSubnet(t *testing.T) {
	s, _ := instanceServer(t)
	id, _, _ := runOneInstance(t, s)

	first := describeOneInstance(t, s, id).PrivateIP
	if !strings.HasPrefix(first, "10.0.1.") {
		t.Errorf("privateIpAddress = %q, want it inside the subnet's 10.0.1.0/24", first)
	}
	for i := 0; i < 3; i++ {
		if got := describeOneInstance(t, s, id).PrivateIP; got != first {
			t.Fatalf("the private IP changed between reads: %q then %q", first, got)
		}
	}
}

// The graph nests an instance under its subnet, which is all the
// visualizer needs -- no AWS knowledge, just ParentID.
func TestAnInstanceIsParentedToItsSubnet(t *testing.T) {
	s, _ := instanceServer(t)
	id, subnet, _ := runOneInstance(t, s)

	res, err := s.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if res.ParentID != subnet {
		t.Errorf("ParentID = %q, want the subnet %q", res.ParentID, subnet)
	}
	if res.Type != typeInstance {
		t.Errorf("Type = %q", res.Type)
	}
}

// The provider's waiters read these state names, so the transitions have to
// be the real ones -- including the codes, which some clients switch on
// instead of the names.
func TestTheLifecycleMovesThroughTheStatesTheProviderWaitsFor(t *testing.T) {
	s, _ := instanceServer(t)
	id, _, _ := runOneInstance(t, s)
	waitForState(t, s, id, "running")

	mustEC2(t, s, "Action=StopInstances&InstanceId.1="+id)
	waitForState(t, s, id, "stopped")
	if code := describeOneInstance(t, s, id).State.Code; code != 80 {
		t.Errorf("stopped state code = %d, want 80", code)
	}

	mustEC2(t, s, "Action=StartInstances&InstanceId.1="+id)
	waitForState(t, s, id, "running")
	if code := describeOneInstance(t, s, id).State.Code; code != 16 {
		t.Errorf("running state code = %d, want 16", code)
	}

	// A terminated instance stays DESCRIBABLE, in state terminated. The
	// provider's delete waiter polls for exactly that and does not accept
	// a not-found as success -- measured: deleting the record immediately
	// failed a destroy with "couldn't find resource (21 retries)" after
	// three minutes.
	mustEC2(t, s, "Action=TerminateInstances&InstanceId.1="+id)
	waitForState(t, s, id, "terminated")
	if code := describeOneInstance(t, s, id).State.Code; code != 48 {
		t.Errorf("terminated state code = %d, want 48", code)
	}
}

// Stopping and starting must not replace the container, or the root disk
// goes with it. The engine enforces this; the API path has to call the
// right verb to get it.
func TestStopAndStartResumeTheSameContainer(t *testing.T) {
	s, f := instanceServer(t)
	id, _, _ := runOneInstance(t, s)
	waitForState(t, s, id, "running")

	before := s.vms.Get(id)
	if before == nil || before.Container == "" {
		t.Fatal("the instance has no container")
	}
	container := before.Container
	f.Calls = nil

	mustEC2(t, s, "Action=StopInstances&InstanceId.1="+id)
	waitForState(t, s, id, "stopped")
	mustEC2(t, s, "Action=StartInstances&InstanceId.1="+id)
	waitForState(t, s, id, "running")

	if got := s.vms.Get(id); got == nil || got.Container != container {
		t.Errorf("the container changed across a stop/start: %q then %v", container, got)
	}
	calls := strings.Join(f.Calls, "; ")
	if !strings.Contains(calls, "start "+container) {
		t.Errorf("the instance was not resumed: %s", calls)
	}
	if strings.Contains(calls, "run ") {
		t.Errorf("a new container was created instead of resuming: %s", calls)
	}
}

// A describe for an instance that is not there must error with this kind's
// own code, not answer an empty reservation set.
func TestDescribingAMissingInstanceErrorsWithItsOwnCode(t *testing.T) {
	s, _ := instanceServer(t)
	code, out := ec2(t, s, "Action=DescribeInstances&InstanceId.1=i-0000000000000000f")
	if code != 400 || !strings.Contains(string(out), "InvalidInstanceID.NotFound") {
		t.Errorf("status %d:\n%s", code, out)
	}
}

// User data arrives base64 and reaches the engine decoded, because that is
// what a boot script is. The engine then re-encodes it to get it into the
// container without the shell ever parsing it -- see internal/engines/vm.
func TestUserDataReachesTheEngineDecoded(t *testing.T) {
	s, _ := instanceServer(t)
	id, _, _ := runOneInstance(t, s)
	waitForState(t, s, id, "running")

	got := s.vms.Get(id)
	if got == nil {
		t.Fatal("the engine does not know about the instance")
	}
	if got.UserData != "#!/bin/sh\necho hi\n" {
		t.Errorf("the engine got user data %q", got.UserData)
	}
	if len(got.PublicKeys) != 1 || !strings.Contains(got.PublicKeys[0], "nimbus@test") {
		t.Errorf("the key pair's public key did not reach the instance: %v", got.PublicKeys)
	}
}

// The image is not derived from the AMI id -- it cannot be, the id is
// opaque and the provider never asks what it means. A tag is the escape
// hatch for naming a real image.
func TestTheImageDefaultsAndCanBeOverriddenWithATag(t *testing.T) {
	s, _ := instanceServer(t)
	id, _, _ := runOneInstance(t, s)
	waitForState(t, s, id, "running")
	if got := s.vms.Get(id); got == nil || got.Image != DefaultInstanceImage {
		t.Errorf("image = %v, want the default %q", got, DefaultInstanceImage)
	}

	vpc := makeVPC(t, s, "10.1.0.0/16", "other")
	subnet := makeSubnet(t, s, vpc, "10.1.1.0/24")
	body := mustEC2(t, s, "Action=RunInstances&ImageId=ami-x&InstanceType=t3.micro"+
		"&MinCount=1&MaxCount=1&SubnetId="+subnet+
		"&TagSpecification.1.ResourceType=instance"+
		"&TagSpecification.1.Tag.1.Key="+instanceImageTag+
		"&TagSpecification.1.Tag.1.Value=busybox:latest")
	var got struct {
		Instances []xmlInstance `xml:"instancesSet>item"`
	}
	if err := xml.Unmarshal(body, &got); err != nil || len(got.Instances) != 1 {
		t.Fatalf("%v\n%s", err, body)
	}
	waitForState(t, s, got.Instances[0].InstanceID, "running")
	if i := s.vms.Get(got.Instances[0].InstanceID); i == nil || i.Image != "busybox:latest" {
		t.Errorf("the %s tag did not pick the image: %v", instanceImageTag, i)
	}
}

// makeSubnet and makeSG give these tests the network an instance needs.
// makeVPC already exists in ec2_test.go; these are the two that did not.
func makeSubnet(t *testing.T, s *Server, vpcID, cidr string) string {
	t.Helper()
	var got struct {
		ID string `xml:"subnet>subnetId"`
	}
	body := mustEC2(t, s, "Action=CreateSubnet&VpcId="+vpcID+"&CidrBlock="+cidr+
		"&AvailabilityZone=us-east-1a")
	if err := xml.Unmarshal(body, &got); err != nil || got.ID == "" {
		t.Fatalf("creating the subnet: %v\n%s", err, body)
	}
	return got.ID
}

func makeSG(t *testing.T, s *Server, vpcID, name string) string {
	t.Helper()
	var got struct {
		ID string `xml:"groupId"`
	}
	body := mustEC2(t, s, "Action=CreateSecurityGroup&VpcId="+vpcID+
		"&GroupName="+name+"&GroupDescription="+name)
	if err := xml.Unmarshal(body, &got); err != nil || got.ID == "" {
		t.Fatalf("creating the security group: %v\n%s", err, body)
	}
	return got.ID
}

func describeOneInstance(t *testing.T, s *Server, id string) xmlInstance {
	t.Helper()
	body := mustEC2(t, s, "Action=DescribeInstances&InstanceId.1="+id)
	var got struct {
		Reservations []struct {
			Instances []xmlInstance `xml:"instancesSet>item"`
		} `xml:"reservationSet>item"`
	}
	if err := xml.Unmarshal(body, &got); err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	if len(got.Reservations) != 1 || len(got.Reservations[0].Instances) != 1 {
		t.Fatalf("describing %s gave %+v\n%s", id, got.Reservations, body)
	}
	return got.Reservations[0].Instances[0]
}

// waitForState polls as the provider's own waiter does: the state moves in
// the background, because that is what pending -> running IS.
func waitForState(t *testing.T, s *Server, id, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = describeOneInstance(t, s, id).State.Name
		if last == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("instance %s is %q, want %q", id, last, want)
}

// DescribeInstanceTypes is called AFTER the instance is created, as part of
// the provider's read-back -- not before, to validate the type. Measured:
// the apply created the instance, waited for it to run, and only then
// failed on this action.
//
// An unknown type must be answered rather than refused. The list of real
// EC2 instance types is long, changes constantly, and nothing here limits
// CPU or memory to match one anyway -- so refusing an unmodelled type
// would fail an apply over a number that means nothing locally.
func TestInstanceTypesAreDescribedIncludingOnesNobodyModelled(t *testing.T) {
	s, _ := instanceServer(t)

	for _, typ := range []string{"t3.micro", "m5.4xlarge", "wat.nonsense"} {
		body := mustEC2(t, s, "Action=DescribeInstanceTypes&InstanceType.1="+typ)
		var got struct {
			XMLName xml.Name `xml:"DescribeInstanceTypesResponse"`
			Types   []struct {
				InstanceType string `xml:"instanceType"`
				VCPU         struct {
					DefaultVCpus   int `xml:"defaultVCpus"`
					DefaultCores   int `xml:"defaultCores"`
					ThreadsPerCore int `xml:"defaultThreadsPerCore"`
				} `xml:"vCpuInfo"`
				Memory struct {
					SizeInMiB int `xml:"sizeInMiB"`
				} `xml:"memoryInfo"`
				Architectures []string `xml:"processorInfo>supportedArchitectures>item"`
				Hypervisor    string   `xml:"hypervisor"`
			} `xml:"instanceTypeSet>item"`
		}
		if err := xml.Unmarshal(body, &got); err != nil {
			t.Fatalf("%s: %v\n%s", typ, err, body)
		}
		if len(got.Types) != 1 {
			t.Fatalf("%s: got %d types:\n%s", typ, len(got.Types), body)
		}
		d := got.Types[0]
		if d.InstanceType != typ {
			t.Errorf("instanceType = %q, want %q", d.InstanceType, typ)
		}
		if d.VCPU.DefaultVCpus < 1 || d.Memory.SizeInMiB < 1 {
			t.Errorf("%s: vCPU and memory must be plausible, got %+v", typ, d)
		}
		// cpu_options in the provider is built from these three, and a
		// zero would make it plan a change to something it cannot set.
		if d.VCPU.DefaultCores < 1 || d.VCPU.ThreadsPerCore < 1 {
			t.Errorf("%s: cores/threads = %+v", typ, d.VCPU)
		}
		if len(d.Architectures) == 0 || d.Hypervisor == "" {
			t.Errorf("%s: architectures = %v, hypervisor = %q", typ, d.Architectures, d.Hypervisor)
		}
	}
}

// A size in the name is honoured where it is recognisable, so m5.4xlarge
// does not report the same two vCPUs as t3.micro. It is a convenience, not
// a catalogue: nothing enforces it.
func TestABiggerInstanceTypeReportsMoreThanASmallerOne(t *testing.T) {
	small := instanceTypeDetails("t3.micro")
	big := instanceTypeDetails("m5.4xlarge")
	if big.VCPUInfo.DefaultVCpus <= small.VCPUInfo.DefaultVCpus {
		t.Errorf("m5.4xlarge has %d vCPUs, t3.micro has %d",
			big.VCPUInfo.DefaultVCpus, small.VCPUInfo.DefaultVCpus)
	}
	if big.MemoryInfo.SizeInMiB <= small.MemoryInfo.SizeInMiB {
		t.Errorf("m5.4xlarge has %d MiB, t3.micro has %d",
			big.MemoryInfo.SizeInMiB, small.MemoryInfo.SizeInMiB)
	}
}

// DescribeTags must honour its filters, and this is the exception to the
// "ignore an unknown filter" rule.
//
// That rule holds because returning too much is something a client narrows
// itself. But the provider uses a FILTERED DescribeTags as a question with
// an expected cardinality -- "what is this instance's
// aws:ec2launchtemplate:id tag?" -- and answers of more than one are an
// error it reports rather than something it filters:
//
//	reading EC2 Instance (i-0372...) launch template ID tag:
//	too many results: wanted 1, got 2
//
// Measured: before instances existed nothing asked DescribeTags a question
// like that, so returning every tag on every resource was harmless. The
// two it got back were the VPC's Name and the instance's Name.
func TestDescribeTagsHonoursItsFiltersBecauseACountIsTheAnswer(t *testing.T) {
	s, _ := instanceServer(t)
	id, _, _ := runOneInstance(t, s) // the VPC gets a Name tag too

	count := func(body string) int {
		t.Helper()
		out := mustEC2(t, s, body)
		var got struct {
			Tags []struct {
				ResourceID string `xml:"resourceId"`
				Key        string `xml:"key"`
			} `xml:"tagSet>item"`
		}
		if err := xml.Unmarshal(out, &got); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return len(got.Tags)
	}

	// Unfiltered, there is more than one tag in the store -- which is what
	// made the unfiltered answer wrong rather than merely generous.
	if all := count("Action=DescribeTags"); all < 2 {
		t.Fatalf("the store should hold several tags, got %d", all)
	}

	if n := count("Action=DescribeTags&Filter.1.Name=resource-id&Filter.1.Value.1=" + id); n != 1 {
		t.Errorf("tags for just the instance = %d, want 1", n)
	}
	// The question the provider actually asks: one resource, one key, and
	// a tag nobody set. The answer has to be none.
	if n := count("Action=DescribeTags&Filter.1.Name=resource-id&Filter.1.Value.1=" + id +
		"&Filter.2.Name=key&Filter.2.Value.1=aws:ec2launchtemplate:id"); n != 0 {
		t.Errorf("tags for an unset key = %d, want 0", n)
	}
	if n := count("Action=DescribeTags&Filter.1.Name=key&Filter.1.Value.1=Name"); n < 2 {
		t.Errorf("tags with key Name = %d, want every resource that has one", n)
	}
	if n := count("Action=DescribeTags&Filter.1.Name=resource-type&Filter.1.Value.1=instance"); n != 1 {
		t.Errorf("tags on instances = %d, want 1", n)
	}
}

// DescribeInstanceAttribute is the VPC-attribute lesson repeating, one
// resource later: ONE attribute per call, several calls after a create, and
// a 400 on any of them fails the whole apply. Measured: the first one aws
// provider 5.100.0 asks for is instanceInitiatedShutdownBehavior, which is
// not a boolean -- so unlike a VPC's, these attributes are not all the same
// type and the answer cannot be a bool.
//
// An unmodelled attribute answers with an empty value rather than refusing,
// for the same reason it does on a VPC: the list only grows with provider
// versions, and refusing one costs an apply.
func TestInstanceAttributesAnswerOneAtATimeAndNeverRefuse(t *testing.T) {
	s, _ := instanceServer(t)
	id, _, sg := runOneInstance(t, s)

	attr := func(name string) string {
		t.Helper()
		out := mustEC2(t, s, "Action=DescribeInstanceAttribute&InstanceId="+id+"&Attribute="+name)
		var got struct {
			InstanceID string `xml:"instanceId"`
			Value      struct {
				Value string `xml:"value"`
			} `xml:",any"`
		}
		if err := xml.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		if got.InstanceID != id {
			t.Errorf("%s: instanceId = %q, want %q", name, got.InstanceID, id)
		}
		return got.Value.Value
	}

	// The one the provider actually asked for first, and it is a string.
	if v := attr("instanceInitiatedShutdownBehavior"); v != "stop" {
		t.Errorf("instanceInitiatedShutdownBehavior = %q, want \"stop\"", v)
	}
	if v := attr("instanceType"); v != "t3.micro" {
		t.Errorf("instanceType = %q", v)
	}
	if v := attr("disableApiTermination"); v != "false" {
		t.Errorf("disableApiTermination = %q, want \"false\"", v)
	}
	// Whatever a future provider version invents must not fail the apply.
	if code, out := ec2(t, s, "Action=DescribeInstanceAttribute&InstanceId="+id+
		"&Attribute=somethingNobodyHasHeardOf"); code != 200 {
		t.Errorf("an unmodelled attribute gave %d, want 200:\n%s", code, out)
	}

	// groupSet is the one attribute that is a LIST, not a value. Answering
	// it with an empty <value> would read as "this instance has no security
	// groups" and plan a change to put them back.
	out := mustEC2(t, s, "Action=DescribeInstanceAttribute&InstanceId="+id+"&Attribute=groupSet")
	var groups struct {
		Groups []struct {
			GroupID string `xml:"groupId"`
		} `xml:"groupSet>item"`
	}
	if err := xml.Unmarshal(out, &groups); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(groups.Groups) != 1 || groups.Groups[0].GroupID != sg {
		t.Errorf("groupSet = %+v, want just %s:\n%s", groups.Groups, sg, out)
	}

	// userData comes back base64, as it went in.
	if v := attr("userData"); v == "" {
		t.Error("userData is empty; the provider stores it and would see a diff")
	} else if decoded, err := base64.StdEncoding.DecodeString(v); err != nil {
		t.Errorf("userData is not base64: %q", v)
	} else if string(decoded) != "#!/bin/sh\necho hi\n" {
		t.Errorf("userData decoded to %q", decoded)
	}
}
