package aws

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"time"

	"github.com/Asere231/nimbus/internal/core"
)

// Security groups, tags, and the two account-level reads the provider makes
// before it does anything else.

type ipRange struct {
	CidrIP      string `xml:"cidrIp" json:"cidrIp"`
	Description string `xml:"description,omitempty" json:"description,omitempty"`
}

type ipv6Range struct {
	CidrIpv6    string `xml:"cidrIpv6" json:"cidrIpv6"`
	Description string `xml:"description,omitempty" json:"description,omitempty"`
}

type userIDGroupPair struct {
	GroupID     string `xml:"groupId" json:"groupId"`
	UserID      string `xml:"userId,omitempty" json:"userId,omitempty"`
	Description string `xml:"description,omitempty" json:"description,omitempty"`
}

// ipPermission is one rule. The same shape serves ingress and egress.
type ipPermission struct {
	IPProtocol string            `xml:"ipProtocol" json:"ipProtocol"`
	FromPort   *int              `xml:"fromPort,omitempty" json:"fromPort,omitempty"`
	ToPort     *int              `xml:"toPort,omitempty" json:"toPort,omitempty"`
	IPRanges   []ipRange         `xml:"ipRanges>item,omitempty" json:"ipRanges,omitempty"`
	Ipv6Ranges []ipv6Range       `xml:"ipv6Ranges>item,omitempty" json:"ipv6Ranges,omitempty"`
	Groups     []userIDGroupPair `xml:"groups>item,omitempty" json:"groups,omitempty"`
}

// sameAs compares two rules the way a revoke has to: by what they allow,
// not by any identifier, because a revoke names the rule by its contents.
//
// Ports are ignored when the protocol is "-1" (all protocols), and that is
// not tidiness -- it is required. Measured against aws provider 5.100.0,
// revoking the default egress rule sends
//
//	IpPermissions.1.IpProtocol=-1&IpPermissions.1.FromPort=0&IpPermissions.1.ToPort=0
//
// while AWS's own DescribeSecurityGroups reports that same rule with NO
// ports at all. Comparing them strictly means the revoke never matches,
// and every aws_security_group with its own egress rules fails to create
// with InvalidPermission.NotFound.
func (r ipPermission) sameAs(o ipPermission) bool {
	if r.IPProtocol != o.IPProtocol {
		return false
	}
	if r.IPProtocol != "-1" && (!samePort(r.FromPort, o.FromPort) || !samePort(r.ToPort, o.ToPort)) {
		return false
	}
	return sameCIDRs(r.IPRanges, o.IPRanges) && sameCIDRs6(r.Ipv6Ranges, o.Ipv6Ranges)
}

// sameCIDRs compares the ranges as a set: the order they arrive in is the
// client's business, not part of what the rule means.
func sameCIDRs(a, b []ipRange) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, r := range a {
		seen[r.CidrIP]++
	}
	for _, r := range b {
		seen[r.CidrIP]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func sameCIDRs6(a, b []ipv6Range) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, r := range a {
		seen[r.CidrIpv6]++
	}
	for _, r := range b {
		seen[r.CidrIpv6]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func samePort(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

type securityGroup struct {
	GroupID             string         `xml:"groupId" json:"groupId"`
	GroupName           string         `xml:"groupName" json:"groupName"`
	GroupDescription    string         `xml:"groupDescription" json:"groupDescription"`
	VpcID               string         `xml:"vpcId" json:"vpcId"`
	OwnerID             string         `xml:"ownerId" json:"ownerId"`
	IPPermissions       []ipPermission `xml:"ipPermissions>item" json:"ipPermissions"`
	IPPermissionsEgress []ipPermission `xml:"ipPermissionsEgress>item" json:"ipPermissionsEgress"`
	TagSet              []tag          `xml:"tagSet>item" json:"tagSet"`
}

func (g *securityGroup) id() string      { return g.GroupID }
func (g *securityGroup) tags() []tag     { return g.TagSet }
func (g *securityGroup) setTags(t []tag) { g.TagSet = t }
func (g *securityGroup) field(name string) ([]string, bool) {
	switch name {
	case "group-id":
		return []string{g.GroupID}, true
	case "group-name":
		return []string{g.GroupName}, true
	case "vpc-id":
		return []string{g.VpcID}, true
	case "description":
		return []string{g.GroupDescription}, true
	case "owner-id":
		return []string{g.OwnerID}, true
	}
	return nil, false
}

type createSGResponse struct {
	XMLName   xml.Name `xml:"CreateSecurityGroupResponse"`
	Xmlns     string   `xml:"xmlns,attr"`
	RequestID string   `xml:"requestId"`
	GroupID   string   `xml:"groupId"`
	TagSet    []tag    `xml:"tagSet>item,omitempty"`
}

func (s *Server) createSecurityGroup(w http.ResponseWriter, p params) *apiError {
	name := p.str("GroupName")
	if name == "" {
		return errorf(400, "MissingParameter", "the request must contain the parameter GroupName")
	}
	vpcID := p.str("VpcId")
	if vpcID != "" {
		if _, err := load(s, vpcID, &vpc{}); err != nil {
			return notFound(typeVPC, vpcID)
		}
	}
	g := &securityGroup{
		GroupID:          newID("sg"),
		GroupName:        name,
		GroupDescription: p.strOr("GroupDescription", "Managed by Nimbus"),
		VpcID:            vpcID,
		OwnerID:          s.cfg.AccountID,
		IPPermissions:    []ipPermission{},
		// AWS gives every new group an allow-all IPv4 egress rule, and the
		// provider deletes it when the config declares its own. A group
		// created without it shows a permanent diff.
		//
		// There is deliberately no ::/0 rule beside it. AWS only adds one
		// in an IPv6-enabled VPC, and the provider tries to revoke it
		// regardless -- so an apply against a v4-only VPC logs one
		// InvalidPermission.NotFound and carries on, here exactly as
		// against real AWS. The red line in the activity feed is accurate,
		// not a gap.
		IPPermissionsEgress: []ipPermission{{
			IPProtocol: "-1",
			IPRanges:   []ipRange{{CidrIP: "0.0.0.0/0"}},
		}},
		TagSet: readTagSpecifications(p),
	}
	parent := vpcID
	if parent == "" {
		parent = s.accountARN()
	}
	if err := s.save(g, typeSG, nameOf(g), parent); err != nil {
		return errorf(500, "InternalError", "storing the security group: %v", err)
	}
	writeXML(w, 200, createSGResponse{
		Xmlns: namespaceFor("ec2"), RequestID: requestID(), GroupID: g.GroupID, TagSet: g.TagSet,
	})
	return nil
}

type describeSGsResponse struct {
	XMLName          xml.Name        `xml:"DescribeSecurityGroupsResponse"`
	Xmlns            string          `xml:"xmlns,attr"`
	RequestID        string          `xml:"requestId"`
	SecurityGroupSet []securityGroup `xml:"securityGroupInfo>item"`
}

func (s *Server) describeSecurityGroups(w http.ResponseWriter, p params) *apiError {
	fs := readFilters(p)
	wanted := append(p.list("GroupId", ""), p.list("SecurityGroupId", "")...)
	names := p.list("GroupName", "")
	out := []securityGroup{}
	for _, g := range list[securityGroup](s, typeSG) {
		g := g
		if len(wanted) > 0 && !contains(wanted, g.GroupID) {
			continue
		}
		if len(names) > 0 && !contains(names, g.GroupName) {
			continue
		}
		if !matches(&g, fs) {
			continue
		}
		out = append(out, g)
	}
	if len(wanted) > 0 && len(out) == 0 {
		return notFound(typeSG, wanted[0])
	}
	writeXML(w, 200, describeSGsResponse{Xmlns: namespaceFor("ec2"), RequestID: requestID(), SecurityGroupSet: out})
	return nil
}

// readPermissions pulls IpPermissions.N out of a request. A rule's ranges
// are nested one level deeper again: IpPermissions.1.IpRanges.1.CidrIp.
func readPermissions(p params) []ipPermission {
	var out []ipPermission
	for i := 1; ; i++ {
		prefix := fmt.Sprintf("IpPermissions.%d", i)
		proto := p.str(prefix + ".IpProtocol")
		if proto == "" {
			return out
		}
		perm := ipPermission{IPProtocol: proto}
		if v := p.str(prefix + ".FromPort"); v != "" {
			n := p.intOr(prefix+".FromPort", 0)
			perm.FromPort = &n
		}
		if v := p.str(prefix + ".ToPort"); v != "" {
			n := p.intOr(prefix+".ToPort", 0)
			perm.ToPort = &n
		}
		for j := 1; ; j++ {
			cidr := p.str(fmt.Sprintf("%s.IpRanges.%d.CidrIp", prefix, j))
			if cidr == "" {
				break
			}
			perm.IPRanges = append(perm.IPRanges, ipRange{
				CidrIP:      cidr,
				Description: p.str(fmt.Sprintf("%s.IpRanges.%d.Description", prefix, j)),
			})
		}
		for j := 1; ; j++ {
			cidr := p.str(fmt.Sprintf("%s.Ipv6Ranges.%d.CidrIpv6", prefix, j))
			if cidr == "" {
				break
			}
			perm.Ipv6Ranges = append(perm.Ipv6Ranges, ipv6Range{
				CidrIpv6:    cidr,
				Description: p.str(fmt.Sprintf("%s.Ipv6Ranges.%d.Description", prefix, j)),
			})
		}
		for j := 1; ; j++ {
			gid := p.str(fmt.Sprintf("%s.Groups.%d.GroupId", prefix, j))
			if gid == "" {
				break
			}
			perm.Groups = append(perm.Groups, userIDGroupPair{GroupID: gid, UserID: p.str(fmt.Sprintf("%s.Groups.%d.UserId", prefix, j))})
		}
		out = append(out, perm)
	}
}

// authorizeSG adds or revokes rules. The same four actions differ only in
// direction and whether they add or take away.
func (s *Server) authorizeSG(w http.ResponseWriter, p params, direction string, add bool) *apiError {
	id := p.str("GroupId")
	g, err := load(s, id, &securityGroup{})
	if err != nil {
		return notFound(typeSG, id)
	}
	incoming := readPermissions(p)

	target := &g.IPPermissions
	if direction == "egress" {
		target = &g.IPPermissionsEgress
	}
	for _, rule := range incoming {
		found := -1
		for i, existing := range *target {
			if existing.sameAs(rule) {
				found = i
				break
			}
		}
		switch {
		case add && found >= 0:
			return errorf(400, "InvalidPermission.Duplicate",
				"the specified rule already exists for group %s", id)
		case add:
			*target = append(*target, rule)
		case found >= 0:
			*target = append((*target)[:found], (*target)[found+1:]...)
		default:
			return errorf(400, "InvalidPermission.NotFound",
				"the specified rule does not exist in group %s", id)
		}
	}
	parent := g.VpcID
	if parent == "" {
		parent = s.accountARN()
	}
	if err := s.save(g, typeSG, nameOf(g), parent); err != nil {
		return errorf(500, "InternalError", "storing the security group: %v", err)
	}

	element := map[[2]any]string{
		{"ingress", true}:  "AuthorizeSecurityGroupIngressResponse",
		{"egress", true}:   "AuthorizeSecurityGroupEgressResponse",
		{"ingress", false}: "RevokeSecurityGroupIngressResponse",
		{"egress", false}:  "RevokeSecurityGroupEgressResponse",
	}[[2]any{direction, add}]
	writeEmpty(w, element)
	return nil
}

// --- tags -----------------------------------------------------------------

// createTags applies to any resource the store holds, whatever kind, which
// is why it decodes into a bag rather than a type: the only fields it needs
// are the tags.
func (s *Server) createTags(w http.ResponseWriter, p params, add bool) *apiError {
	ids := p.list("ResourceId", "")
	var incoming []tag
	for i := 1; ; i++ {
		key := p.str(fmt.Sprintf("Tag.%d.Key", i))
		if key == "" {
			break
		}
		incoming = append(incoming, tag{Key: key, Value: p.str(fmt.Sprintf("Tag.%d.Value", i))})
	}
	for _, id := range ids {
		res, err := s.store.Get(id)
		if err != nil {
			return errorf(400, "InvalidID", "The ID %q does not exist", id)
		}
		// store.Update keeps this atomic against anything else writing the
		// same resource -- see the comment on core.Store.Update.
		err = s.store.Update(id, func(body map[string]any) error {
			current := decodeTags(body["tagSet"])
			merged := mergeTags(current, incoming, add)
			body["tagSet"] = encodeTags(merged)
			return nil
		})
		if err != nil {
			return errorf(500, "InternalError", "tagging %s: %v", id, err)
		}
		// The card's label follows the Name tag, so the graph has to be
		// told as well as the body.
		if fresh, err := s.store.Get(id); err == nil {
			merged := decodeTags(bodyField(fresh.Body, "tagSet"))
			name := res.Name
			for _, t := range merged {
				if t.Key == "Name" && t.Value != "" {
					name = t.Value
				}
			}
			if name != fresh.Name {
				fresh.Name = name
				s.store.Put(fresh)
			}
		}
	}
	if add {
		writeEmpty(w, "CreateTagsResponse")
	} else {
		writeEmpty(w, "DeleteTagsResponse")
	}
	return nil
}

type describeTagsResponse struct {
	XMLName   xml.Name `xml:"DescribeTagsResponse"`
	Xmlns     string   `xml:"xmlns,attr"`
	RequestID string   `xml:"requestId"`
	TagSet    []struct {
		ResourceID   string `xml:"resourceId"`
		ResourceType string `xml:"resourceType"`
		Key          string `xml:"key"`
		Value        string `xml:"value"`
	} `xml:"tagSet>item"`
}

func (s *Server) describeTags(w http.ResponseWriter, p params) *apiError {
	doc := describeTagsResponse{Xmlns: namespaceFor("ec2"), RequestID: requestID()}
	for _, res := range s.store.List(func(r *core.Resource) bool { return r.Cloud == "aws" }) {
		for _, t := range decodeTags(bodyField(res.Body, "tagSet")) {
			doc.TagSet = append(doc.TagSet, struct {
				ResourceID   string `xml:"resourceId"`
				ResourceType string `xml:"resourceType"`
				Key          string `xml:"key"`
				Value        string `xml:"value"`
			}{res.ID, ec2TypeName(res.Type), t.Key, t.Value})
		}
	}
	writeXML(w, 200, doc)
	return nil
}

// ec2TypeName is what DescribeTags calls a kind: lower-case and hyphenated,
// not the CloudFormation name the store uses.
func ec2TypeName(typ string) string {
	switch typ {
	case typeVPC:
		return "vpc"
	case typeSubnet:
		return "subnet"
	case typeIGW:
		return "internet-gateway"
	case typeRouteTbl:
		return "route-table"
	case typeSG:
		return "security-group"
	case typeNAT:
		return "natgateway"
	}
	return "resource"
}

// --- the two reads the provider makes before anything else ----------------

type describeAZsResponse struct {
	XMLName   xml.Name `xml:"DescribeAvailabilityZonesResponse"`
	Xmlns     string   `xml:"xmlns,attr"`
	RequestID string   `xml:"requestId"`
	AZSet     []struct {
		ZoneName   string `xml:"zoneName"`
		ZoneID     string `xml:"zoneId"`
		ZoneState  string `xml:"zoneState"`
		RegionName string `xml:"regionName"`
		ZoneType   string `xml:"zoneType"`
	} `xml:"availabilityZoneInfo>item"`
}

func (s *Server) describeAZs(w http.ResponseWriter, p params) *apiError {
	doc := describeAZsResponse{Xmlns: namespaceFor("ec2"), RequestID: requestID()}
	for i, suffix := range []string{"a", "b", "c"} {
		doc.AZSet = append(doc.AZSet, struct {
			ZoneName   string `xml:"zoneName"`
			ZoneID     string `xml:"zoneId"`
			ZoneState  string `xml:"zoneState"`
			RegionName string `xml:"regionName"`
			ZoneType   string `xml:"zoneType"`
		}{
			ZoneName:   s.cfg.Region + suffix,
			ZoneID:     fmt.Sprintf("use1-az%d", i+1),
			ZoneState:  "available",
			RegionName: s.cfg.Region,
			ZoneType:   "availability-zone",
		})
	}
	writeXML(w, 200, doc)
	return nil
}

type describeAccountAttributesResponse struct {
	XMLName      xml.Name           `xml:"DescribeAccountAttributesResponse"`
	Xmlns        string             `xml:"xmlns,attr"`
	RequestID    string             `xml:"requestId"`
	AttributeSet []accountAttribute `xml:"accountAttributeSet>item"`
}

type accountAttribute struct {
	AttributeName   string `xml:"attributeName"`
	AttributeValues []struct {
		AttributeValue string `xml:"attributeValue"`
	} `xml:"attributeValueSet>item"`
}

func attr(name string, values ...string) accountAttribute {
	a := accountAttribute{AttributeName: name}
	for _, v := range values {
		a.AttributeValues = append(a.AttributeValues, struct {
			AttributeValue string `xml:"attributeValue"`
		}{v})
	}
	return a
}

func (s *Server) describeAccountAttributes(w http.ResponseWriter, p params) *apiError {
	writeXML(w, 200, describeAccountAttributesResponse{
		Xmlns:     namespaceFor("ec2"),
		RequestID: requestID(),
		AttributeSet: []accountAttribute{
			// Only VPC: EC2-Classic has been gone for years and a provider
			// that sees "EC2" in here takes a different, older code path.
			attr("supported-platforms", "VPC"),
			attr("default-vpc", "none"),
			attr("max-instances", "20"),
			attr("vpc-max-security-groups-per-interface", "5"),
			attr("max-elastic-ips", "5"),
			attr("vpc-max-elastic-ips", "5"),
		},
	})
	return nil
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// --- the reads that exist only so a describe does not fail ---------------
//
// The provider asks about network ACLs and network interfaces while
// reading a VPC and while working out whether a subnet can be deleted.
// Nimbus models neither, and an empty set is the truthful answer: there
// are none. Answering an error instead fails the apply over something
// nobody asked for.

type describeNetworkAclsResponse struct {
	XMLName       xml.Name   `xml:"DescribeNetworkAclsResponse"`
	Xmlns         string     `xml:"xmlns,attr"`
	RequestID     string     `xml:"requestId"`
	NetworkAclSet []struct{} `xml:"networkAclSet>item,omitempty"`
}

func (s *Server) describeNetworkAcls(w http.ResponseWriter, p params) *apiError {
	writeXML(w, 200, describeNetworkAclsResponse{Xmlns: namespaceFor("ec2"), RequestID: requestID()})
	return nil
}

type describeNetworkInterfacesResponse struct {
	XMLName             xml.Name   `xml:"DescribeNetworkInterfacesResponse"`
	Xmlns               string     `xml:"xmlns,attr"`
	RequestID           string     `xml:"requestId"`
	NetworkInterfaceSet []struct{} `xml:"networkInterfaceSet>item,omitempty"`
}

func (s *Server) describeNetworkInterfaces(w http.ResponseWriter, p params) *apiError {
	writeXML(w, 200, describeNetworkInterfacesResponse{Xmlns: namespaceFor("ec2"), RequestID: requestID()})
	return nil
}
