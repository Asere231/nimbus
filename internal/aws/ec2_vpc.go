package aws

import (
	"encoding/xml"
	"net/http"
	"strings"
)

// VPCs and subnets.

type cidrAssociation struct {
	AssociationID  string `xml:"associationId" json:"associationId"`
	CidrBlock      string `xml:"cidrBlock" json:"cidrBlock"`
	CidrBlockState struct {
		State string `xml:"state" json:"state"`
	} `xml:"cidrBlockState" json:"cidrBlockState"`
}

type vpc struct {
	VpcID                   string            `xml:"vpcId" json:"vpcId"`
	State                   string            `xml:"state" json:"state"`
	CidrBlock               string            `xml:"cidrBlock" json:"cidrBlock"`
	DhcpOptionsID           string            `xml:"dhcpOptionsId" json:"dhcpOptionsId"`
	InstanceTenancy         string            `xml:"instanceTenancy" json:"instanceTenancy"`
	IsDefault               bool              `xml:"isDefault" json:"isDefault"`
	OwnerID                 string            `xml:"ownerId" json:"ownerId"`
	CidrBlockAssociationSet []cidrAssociation `xml:"cidrBlockAssociationSet>item" json:"cidrBlockAssociationSet"`
	TagSet                  []tag             `xml:"tagSet>item" json:"tagSet"`

	// Attributes live on the VPC but are not part of its describe output --
	// the provider reads them with DescribeVpcAttribute, ONE CALL EACH, and
	// there are more of them than anyone expects. Keeping them in a map
	// rather than as named fields means a provider version that reads a new
	// one does not need a code change here.
	Attributes map[string]bool `xml:"-" json:"attributes"`
}

func (v *vpc) id() string      { return v.VpcID }
func (v *vpc) tags() []tag     { return v.TagSet }
func (v *vpc) setTags(t []tag) { v.TagSet = t }
func (v *vpc) field(name string) ([]string, bool) {
	switch name {
	case "vpc-id":
		return []string{v.VpcID}, true
	case "cidr", "cidr-block", "cidr-block-association.cidr-block":
		return []string{v.CidrBlock}, true
	case "state":
		return []string{v.State}, true
	case "isDefault", "is-default":
		return []string{boolString(v.IsDefault)}, true
	case "owner-id":
		return []string{v.OwnerID}, true
	case "dhcp-options-id":
		return []string{v.DhcpOptionsID}, true
	}
	return nil, false
}

type createVPCResponse struct {
	XMLName   xml.Name `xml:"CreateVpcResponse"`
	Xmlns     string   `xml:"xmlns,attr"`
	RequestID string   `xml:"requestId"`
	VPC       vpc      `xml:"vpc"`
}

func (s *Server) createVPC(w http.ResponseWriter, p params) *apiError {
	cidr := p.str("CidrBlock")
	if cidr == "" {
		return errorf(400, "MissingParameter", "the request must contain the parameter CidrBlock")
	}
	v := &vpc{
		VpcID:           newID("vpc"),
		State:           "available",
		CidrBlock:       cidr,
		DhcpOptionsID:   "dopt-" + stableID(s.cfg.AccountID, 17),
		InstanceTenancy: p.strOr("InstanceTenancy", "default"),
		OwnerID:         s.cfg.AccountID,
		TagSet:          readTagSpecifications(p),
		// AWS's own defaults for a non-default VPC. The provider reads
		// these back and shows a diff if they are not what it expects.
		Attributes: map[string]bool{
			"enableDnsSupport":                 true,
			"enableDnsHostnames":               false,
			"enableNetworkAddressUsageMetrics": false,
		},
	}
	assoc := cidrAssociation{AssociationID: "vpc-cidr-assoc-0" + hexID(16), CidrBlock: cidr}
	assoc.CidrBlockState.State = "associated"
	v.CidrBlockAssociationSet = []cidrAssociation{assoc}

	if err := s.save(v, typeVPC, nameOf(v), s.accountARN()); err != nil {
		return errorf(500, "InternalError", "storing the vpc: %v", err)
	}
	writeXML(w, 200, createVPCResponse{Xmlns: namespaceFor("ec2"), RequestID: requestID(), VPC: *v})
	return nil
}

type describeVPCsResponse struct {
	XMLName   xml.Name `xml:"DescribeVpcsResponse"`
	Xmlns     string   `xml:"xmlns,attr"`
	RequestID string   `xml:"requestId"`
	VPCSet    []vpc    `xml:"vpcSet>item"`
}

func (s *Server) describeVPCs(w http.ResponseWriter, p params) *apiError {
	fs := readFilters(p)
	wanted := p.list("VpcId", "")
	out := []vpc{}
	for _, v := range list[vpc](s, typeVPC) {
		v := v
		if len(wanted) > 0 && !contains(wanted, v.VpcID) {
			continue
		}
		if !matches(&v, fs) {
			continue
		}
		out = append(out, v)
	}
	// Asking for an id that is not there is an error, not an empty list --
	// the provider relies on it to notice a VPC somebody deleted.
	if len(wanted) > 0 && len(out) == 0 {
		return notFound(typeVPC, wanted[0])
	}
	writeXML(w, 200, describeVPCsResponse{Xmlns: namespaceFor("ec2"), RequestID: requestID(), VPCSet: out})
	return nil
}

// DescribeVpcAttribute asks for ONE attribute at a time and the response
// carries only that one, so the two cannot be merged into the describe.
type describeVPCAttributeResponse struct {
	XMLName   xml.Name   `xml:"DescribeVpcAttributeResponse"`
	Xmlns     string     `xml:"xmlns,attr"`
	RequestID string     `xml:"requestId"`
	VpcID     string     `xml:"vpcId"`
	Value     *boolValue `xml:",omitempty"`
}

type boolValue struct {
	XMLName xml.Name
	Value   bool `xml:"value"`
}

func (s *Server) describeVPCAttribute(w http.ResponseWriter, p params) *apiError {
	v, err := load(s, p.str("VpcId"), &vpc{})
	if err != nil {
		return notFound(typeVPC, p.str("VpcId"))
	}
	attr := p.str("Attribute")
	if attr == "" {
		return errorf(400, "MissingParameter", "the request must contain the parameter Attribute")
	}
	// An attribute nobody has modelled answers false rather than failing.
	//
	// Measured: aws provider 5.100.0 reads three of these after creating a
	// VPC -- enableDnsSupport, enableDnsHostnames and the one nobody would
	// guess, enableNetworkAddressUsageMetrics -- and a 400 on any of them
	// fails the whole apply. The list only grows with provider versions, so
	// refusing an unknown name buys nothing and costs an apply; false is
	// also the right answer for every attribute AWS has added so far.
	doc := describeVPCAttributeResponse{Xmlns: namespaceFor("ec2"), RequestID: requestID(), VpcID: v.VpcID}
	doc.Value = &boolValue{XMLName: xml.Name{Local: attr}, Value: v.Attributes[attr]}
	writeXML(w, 200, doc)
	return nil
}

func (s *Server) modifyVPCAttribute(w http.ResponseWriter, p params) *apiError {
	id := p.str("VpcId")
	v, err := load(s, id, &vpc{})
	if err != nil {
		return notFound(typeVPC, id)
	}
	// Each attribute arrives as Attribute.Value, one per call, with the
	// first letter capitalised on the way in and lower-cased on the way
	// out: ModifyVpcAttribute takes EnableDnsSupport.Value, while
	// DescribeVpcAttribute answers to enableDnsSupport.
	if v.Attributes == nil {
		v.Attributes = map[string]bool{}
	}
	for key := range p.Values {
		name, ok := strings.CutSuffix(key, ".Value")
		if !ok || name == "" {
			continue
		}
		lower := strings.ToLower(name[:1]) + name[1:]
		v.Attributes[lower] = p.str(key) == "true"
	}
	if err := s.save(v, typeVPC, nameOf(v), s.accountARN()); err != nil {
		return errorf(500, "InternalError", "storing the vpc: %v", err)
	}
	writeEmpty(w, "ModifyVpcAttributeResponse")
	return nil
}

// --- subnets --------------------------------------------------------------

type subnet struct {
	SubnetID                    string `xml:"subnetId" json:"subnetId"`
	State                       string `xml:"state" json:"state"`
	VpcID                       string `xml:"vpcId" json:"vpcId"`
	CidrBlock                   string `xml:"cidrBlock" json:"cidrBlock"`
	AvailableIPAddressCount     int    `xml:"availableIpAddressCount" json:"availableIpAddressCount"`
	AvailabilityZone            string `xml:"availabilityZone" json:"availabilityZone"`
	AvailabilityZoneID          string `xml:"availabilityZoneId" json:"availabilityZoneId"`
	DefaultForAz                bool   `xml:"defaultForAz" json:"defaultForAz"`
	MapPublicIPOnLaunch         bool   `xml:"mapPublicIpOnLaunch" json:"mapPublicIpOnLaunch"`
	OwnerID                     string `xml:"ownerId" json:"ownerId"`
	AssignIpv6AddressOnCreation bool   `xml:"assignIpv6AddressOnCreation" json:"assignIpv6AddressOnCreation"`
	SubnetArn                   string `xml:"subnetArn" json:"subnetArn"`
	EnableDNS64                 bool   `xml:"enableDns64" json:"enableDns64"`
	TagSet                      []tag  `xml:"tagSet>item" json:"tagSet"`
}

func (n *subnet) id() string      { return n.SubnetID }
func (n *subnet) tags() []tag     { return n.TagSet }
func (n *subnet) setTags(t []tag) { n.TagSet = t }
func (n *subnet) field(name string) ([]string, bool) {
	switch name {
	case "subnet-id":
		return []string{n.SubnetID}, true
	case "vpc-id":
		return []string{n.VpcID}, true
	case "cidr", "cidr-block", "cidrBlock":
		return []string{n.CidrBlock}, true
	case "availability-zone", "availabilityZone":
		return []string{n.AvailabilityZone}, true
	case "state":
		return []string{n.State}, true
	case "owner-id":
		return []string{n.OwnerID}, true
	case "default-for-az", "defaultForAz":
		return []string{boolString(n.DefaultForAz)}, true
	}
	return nil, false
}

type createSubnetResponse struct {
	XMLName   xml.Name `xml:"CreateSubnetResponse"`
	Xmlns     string   `xml:"xmlns,attr"`
	RequestID string   `xml:"requestId"`
	Subnet    subnet   `xml:"subnet"`
}

func (s *Server) createSubnet(w http.ResponseWriter, p params) *apiError {
	vpcID := p.str("VpcId")
	if _, err := load(s, vpcID, &vpc{}); err != nil {
		return notFound(typeVPC, vpcID)
	}
	cidr := p.str("CidrBlock")
	if cidr == "" {
		return errorf(400, "MissingParameter", "the request must contain the parameter CidrBlock")
	}
	az := p.strOr("AvailabilityZone", s.cfg.Region+"a")
	n := &subnet{
		SubnetID:  newID("subnet"),
		State:     "available",
		VpcID:     vpcID,
		CidrBlock: cidr,
		// A /24 has 256 addresses, five of which AWS keeps. Nobody checks
		// the arithmetic, but a plausible number beats a zero.
		AvailableIPAddressCount: addressesIn(cidr) - 5,
		AvailabilityZone:        az,
		AvailabilityZoneID:      "use1-az1",
		OwnerID:                 s.cfg.AccountID,
		TagSet:                  readTagSpecifications(p),
	}
	n.SubnetArn = arn("ec2", s.cfg.Region, s.cfg.AccountID, "subnet/"+n.SubnetID)
	if err := s.save(n, typeSubnet, nameOf(n), vpcID); err != nil {
		return errorf(500, "InternalError", "storing the subnet: %v", err)
	}
	writeXML(w, 200, createSubnetResponse{Xmlns: namespaceFor("ec2"), RequestID: requestID(), Subnet: *n})
	return nil
}

type describeSubnetsResponse struct {
	XMLName   xml.Name `xml:"DescribeSubnetsResponse"`
	Xmlns     string   `xml:"xmlns,attr"`
	RequestID string   `xml:"requestId"`
	SubnetSet []subnet `xml:"subnetSet>item"`
}

func (s *Server) describeSubnets(w http.ResponseWriter, p params) *apiError {
	fs := readFilters(p)
	wanted := p.list("SubnetId", "")
	out := []subnet{}
	for _, n := range list[subnet](s, typeSubnet) {
		n := n
		if len(wanted) > 0 && !contains(wanted, n.SubnetID) {
			continue
		}
		if !matches(&n, fs) {
			continue
		}
		out = append(out, n)
	}
	if len(wanted) > 0 && len(out) == 0 {
		return notFound(typeSubnet, wanted[0])
	}
	writeXML(w, 200, describeSubnetsResponse{Xmlns: namespaceFor("ec2"), RequestID: requestID(), SubnetSet: out})
	return nil
}

func (s *Server) modifySubnetAttribute(w http.ResponseWriter, p params) *apiError {
	id := p.str("SubnetId")
	n, err := load(s, id, &subnet{})
	if err != nil {
		return notFound(typeSubnet, id)
	}
	if got := p.str("MapPublicIpOnLaunch.Value"); got != "" {
		n.MapPublicIPOnLaunch = got == "true"
	}
	if got := p.str("AssignIpv6AddressOnCreation.Value"); got != "" {
		n.AssignIpv6AddressOnCreation = got == "true"
	}
	if got := p.str("EnableDns64.Value"); got != "" {
		n.EnableDNS64 = got == "true"
	}
	if err := s.save(n, typeSubnet, nameOf(n), n.VpcID); err != nil {
		return errorf(500, "InternalError", "storing the subnet: %v", err)
	}
	writeEmpty(w, "ModifySubnetAttributeResponse")
	return nil
}

// addressesIn is 2^(32-prefix) for an IPv4 CIDR, defaulting to a /24 when
// the string is not one.
func addressesIn(cidr string) int {
	i := strings.LastIndex(cidr, "/")
	if i < 0 {
		return 256
	}
	bits := 0
	for _, c := range cidr[i+1:] {
		if c < '0' || c > '9' {
			return 256
		}
		bits = bits*10 + int(c-'0')
	}
	if bits < 0 || bits > 32 {
		return 256
	}
	return 1 << (32 - bits)
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
