package aws

import (
	"encoding/xml"
	"strings"
	"testing"
)

// ec2 replays a Query request the way aws-sdk-go-v2 sends it.
func ec2(t *testing.T, s *Server, body string) (int, []byte) {
	t.Helper()
	return query(t, s, "ec2", body+"&Version="+ec2Version)
}

func mustEC2(t *testing.T, s *Server, body string) []byte {
	t.Helper()
	code, out := ec2(t, s, body)
	if code != 200 {
		t.Fatalf("status %d for %q:\n%s", code, body, out)
	}
	return out
}

// makeVPC creates one and returns its id.
func makeVPC(t *testing.T, s *Server, cidr, name string) string {
	t.Helper()
	body := mustEC2(t, s, "Action=CreateVpc&CidrBlock="+cidr+
		"&TagSpecification.1.ResourceType=vpc&TagSpecification.1.Tag.1.Key=Name"+
		"&TagSpecification.1.Tag.1.Value="+name)
	var got struct {
		ID string `xml:"vpc>vpcId"`
	}
	if err := xml.Unmarshal(body, &got); err != nil || got.ID == "" {
		t.Fatalf("creating the vpc: %v\n%s", err, body)
	}
	return got.ID
}

func TestCreateAndDescribeVPC(t *testing.T) {
	s := newTestServer(t)
	id := makeVPC(t, s, "10.0.0.0/16", "main")
	if !strings.HasPrefix(id, "vpc-0") {
		t.Errorf("vpc id = %q, want the vpc-0 prefix of a modern id", id)
	}

	var got struct {
		VPCs []struct {
			ID        string `xml:"vpcId"`
			State     string `xml:"state"`
			Cidr      string `xml:"cidrBlock"`
			OwnerID   string `xml:"ownerId"`
			IsDefault bool   `xml:"isDefault"`
			Tags      []struct {
				Key   string `xml:"key"`
				Value string `xml:"value"`
			} `xml:"tagSet>item"`
		} `xml:"vpcSet>item"`
	}
	if err := xml.Unmarshal(mustEC2(t, s, "Action=DescribeVpcs"), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.VPCs) != 1 {
		t.Fatalf("described %d vpcs, want 1", len(got.VPCs))
	}
	v := got.VPCs[0]
	if v.ID != id || v.State != "available" || v.Cidr != "10.0.0.0/16" || v.OwnerID != DefaultAccountID {
		t.Errorf("vpc = %+v", v)
	}
	// The tags a create carries inline must be there at once: the provider
	// tags on create, and the graph labels the card from the Name tag.
	if len(v.Tags) != 1 || v.Tags[0].Key != "Name" || v.Tags[0].Value != "main" {
		t.Errorf("tags = %+v", v.Tags)
	}
	res, err := s.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "main" {
		t.Errorf("the graph labels the vpc %q, want its Name tag", res.Name)
	}
	if res.ParentID != s.accountARN() {
		t.Errorf("a vpc hangs off the account, not %q", res.ParentID)
	}
}

// Measured against aws provider 5.100.0: after creating a VPC it reads
// THREE attributes, one call each, including the one nobody would guess.
// A 400 on any of them fails the apply.
func TestDescribeVpcAttributeAnswersAnythingItIsAsked(t *testing.T) {
	s := newTestServer(t)
	id := makeVPC(t, s, "10.0.0.0/16", "main")

	for _, attr := range []string{"enableDnsSupport", "enableDnsHostnames", "enableNetworkAddressUsageMetrics"} {
		code, body := ec2(t, s, "Action=DescribeVpcAttribute&VpcId="+id+"&Attribute="+attr)
		if code != 200 {
			t.Errorf("%s: status %d, which fails the apply:\n%s", attr, code, body)
		}
		if !strings.Contains(string(body), "<"+attr+">") {
			t.Errorf("%s: the answer should carry the attribute's own element:\n%s", attr, body)
		}
	}
	// And one nobody has heard of, which a future provider version will
	// invent: false beats failing.
	if code, _ := ec2(t, s, "Action=DescribeVpcAttribute&VpcId="+id+"&Attribute=enableSomethingNew"); code != 200 {
		t.Errorf("an unmodelled attribute = %d, want 200 with false", code)
	}
}

func TestModifyVpcAttributeRoundTrips(t *testing.T) {
	s := newTestServer(t)
	id := makeVPC(t, s, "10.0.0.0/16", "main")
	mustEC2(t, s, "Action=ModifyVpcAttribute&VpcId="+id+"&EnableDnsHostnames.Value=true")

	body := mustEC2(t, s, "Action=DescribeVpcAttribute&VpcId="+id+"&Attribute=enableDnsHostnames")
	// ModifyVpcAttribute capitalises the first letter, DescribeVpcAttribute
	// does not; the two have to meet in the middle.
	if !strings.Contains(string(body), "<value>true</value>") {
		t.Errorf("the attribute did not round-trip:\n%s", body)
	}
}

func TestSubnetsHangOffTheirVPC(t *testing.T) {
	s := newTestServer(t)
	vpcID := makeVPC(t, s, "10.0.0.0/16", "main")
	body := mustEC2(t, s, "Action=CreateSubnet&VpcId="+vpcID+"&CidrBlock=10.0.1.0/24&AvailabilityZone=us-east-1a")
	var got struct {
		ID        string `xml:"subnet>subnetId"`
		Available int    `xml:"subnet>availableIpAddressCount"`
		Arn       string `xml:"subnet>subnetArn"`
	}
	if err := xml.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Available != 251 {
		t.Errorf("a /24 reports %d free addresses, want 251 (256 less the five AWS keeps)", got.Available)
	}
	if !strings.HasPrefix(got.Arn, "arn:aws:ec2:") {
		t.Errorf("subnet arn = %q", got.Arn)
	}
	res, err := s.store.Get(got.ID)
	if err != nil {
		t.Fatal(err)
	}
	// This is the whole reason the graph needs no AWS knowledge.
	if res.ParentID != vpcID {
		t.Errorf("subnet parent = %q, want the vpc", res.ParentID)
	}

	// A subnet in a VPC that is not there is an error, not a silent create.
	if code, _ := ec2(t, s, "Action=CreateSubnet&VpcId=vpc-0nope&CidrBlock=10.0.9.0/24"); code != 400 {
		t.Errorf("creating a subnet in a missing vpc = %d, want 400", code)
	}
}

// Deleting a VPC takes its subnets with it, which the store already does
// by parent -- the AWS side has to add nothing.
func TestDeletingAVPCCascades(t *testing.T) {
	s := newTestServer(t)
	vpcID := makeVPC(t, s, "10.0.0.0/16", "main")
	mustEC2(t, s, "Action=CreateSubnet&VpcId="+vpcID+"&CidrBlock=10.0.1.0/24")
	mustEC2(t, s, "Action=DeleteVpc&VpcId="+vpcID)

	var got struct {
		Subnets []struct{} `xml:"subnetSet>item"`
	}
	_ = xml.Unmarshal(mustEC2(t, s, "Action=DescribeSubnets"), &got)
	if len(got.Subnets) != 0 {
		t.Errorf("%d subnets survived their vpc", len(got.Subnets))
	}
}

// The provider asks for a resource by id to find out whether it is still
// there. An empty list would read as "it exists but is empty"; the error
// code is how it tells "gone" from "wrong".
func TestDescribingAMissingIDIsAnError(t *testing.T) {
	s := newTestServer(t)
	code, body := ec2(t, s, "Action=DescribeVpcs&VpcId.1=vpc-0deadbeef")
	if code != 400 {
		t.Fatalf("status %d, want 400", code)
	}
	if !strings.Contains(string(body), "InvalidVpcID.NotFound") {
		t.Errorf("the code must be the one the provider matches on:\n%s", body)
	}
	// Each kind has its own code, and they are not interchangeable.
	_, body = ec2(t, s, "Action=DescribeSubnets&SubnetId.1=subnet-0deadbeef")
	if !strings.Contains(string(body), "InvalidSubnetID.NotFound") {
		t.Errorf("subnet error code:\n%s", body)
	}
}

func TestFiltersNarrowAndUnknownOnesAreIgnored(t *testing.T) {
	s := newTestServer(t)
	a := makeVPC(t, s, "10.0.0.0/16", "alpha")
	makeVPC(t, s, "10.1.0.0/16", "beta")

	count := func(body string) int {
		var got struct {
			VPCs []struct{} `xml:"vpcSet>item"`
		}
		_ = xml.Unmarshal(mustEC2(t, s, body), &got)
		return len(got.VPCs)
	}
	if n := count("Action=DescribeVpcs&Filter.1.Name=cidr&Filter.1.Value.1=10.0.0.0/16"); n != 1 {
		t.Errorf("filtering by cidr gave %d, want 1", n)
	}
	if n := count("Action=DescribeVpcs&Filter.1.Name=tag:Name&Filter.1.Value.1=beta"); n != 1 {
		t.Errorf("filtering by tag gave %d, want 1", n)
	}
	if n := count("Action=DescribeVpcs&Filter.1.Name=tag:Name&Filter.1.Value.1=al*"); n != 1 {
		t.Errorf("a glob gave %d, want 1", n)
	}
	// Two values of one filter are ORed.
	if n := count("Action=DescribeVpcs&Filter.1.Name=cidr&Filter.1.Value.1=10.0.0.0/16&Filter.1.Value.2=10.1.0.0/16"); n != 2 {
		t.Errorf("two values gave %d, want both", n)
	}
	// Two filters are ANDed.
	if n := count("Action=DescribeVpcs&Filter.1.Name=cidr&Filter.1.Value.1=10.0.0.0/16" +
		"&Filter.2.Name=tag:Name&Filter.2.Value.1=beta"); n != 0 {
		t.Errorf("contradictory filters gave %d, want 0", n)
	}
	// A filter this kind does not have returns everything rather than
	// failing: the provider sends filters nobody has implemented, and an
	// error there fails an apply for no reason.
	if n := count("Action=DescribeVpcs&Filter.1.Name=something-invented&Filter.1.Value.1=x"); n != 2 {
		t.Errorf("an unknown filter gave %d, want everything", n)
	}
	_ = a
}

func TestTagsAreMergedAndRenameTheCard(t *testing.T) {
	s := newTestServer(t)
	id := makeVPC(t, s, "10.0.0.0/16", "before")
	mustEC2(t, s, "Action=CreateTags&ResourceId.1="+id+
		"&Tag.1.Key=Name&Tag.1.Value=after&Tag.2.Key=env&Tag.2.Value=dev")

	res, err := s.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "after" {
		t.Errorf("the card still reads %q; a retag must rename it", res.Name)
	}
	// A fresh target per decode: encoding/xml APPENDS to a slice that is
	// already there rather than replacing it, so reusing one accumulates
	// the results of every call.
	if tags := vpcTags(t, s); len(tags) != 2 {
		t.Fatalf("tags = %+v, want Name replaced and env added", tags)
	}

	mustEC2(t, s, "Action=DeleteTags&ResourceId.1="+id+"&Tag.1.Key=env")
	if tags := vpcTags(t, s); len(tags) != 1 || tags[0].Key != "Name" {
		t.Errorf("after deleting env, tags = %+v", tags)
	}
}

func TestInternetGatewayMovesUnderItsVPCWhenAttached(t *testing.T) {
	s := newTestServer(t)
	vpcID := makeVPC(t, s, "10.0.0.0/16", "main")
	var created struct {
		ID string `xml:"internetGateway>internetGatewayId"`
	}
	_ = xml.Unmarshal(mustEC2(t, s, "Action=CreateInternetGateway"), &created)

	res, _ := s.store.Get(created.ID)
	if res.ParentID != s.accountARN() {
		t.Errorf("an unattached gateway hangs off the account, not %q", res.ParentID)
	}
	mustEC2(t, s, "Action=AttachInternetGateway&InternetGatewayId="+created.ID+"&VpcId="+vpcID)
	res, _ = s.store.Get(created.ID)
	if res.ParentID != vpcID {
		t.Errorf("after attaching, parent = %q, want the vpc", res.ParentID)
	}
	mustEC2(t, s, "Action=DetachInternetGateway&InternetGatewayId="+created.ID+"&VpcId="+vpcID)
	res, _ = s.store.Get(created.ID)
	if res.ParentID != s.accountARN() {
		t.Errorf("after detaching, parent = %q, want the account again", res.ParentID)
	}
}

func TestRouteTableStartsWithTheLocalRoute(t *testing.T) {
	s := newTestServer(t)
	vpcID := makeVPC(t, s, "10.0.0.0/16", "main")
	var got struct {
		ID     string `xml:"routeTable>routeTableId"`
		Routes []struct {
			Cidr    string `xml:"destinationCidrBlock"`
			Gateway string `xml:"gatewayId"`
		} `xml:"routeTable>routeSet>item"`
	}
	_ = xml.Unmarshal(mustEC2(t, s, "Action=CreateRouteTable&VpcId="+vpcID), &got)
	// Every route table has the VPC's local route, which the provider
	// expects to find and does not manage.
	if len(got.Routes) != 1 || got.Routes[0].Gateway != "local" || got.Routes[0].Cidr != "10.0.0.0/16" {
		t.Fatalf("routes = %+v, want the local route", got.Routes)
	}

	mustEC2(t, s, "Action=CreateRoute&RouteTableId="+got.ID+"&DestinationCidrBlock=0.0.0.0/0&GatewayId=igw-01234")
	code, body := ec2(t, s, "Action=CreateRoute&RouteTableId="+got.ID+"&DestinationCidrBlock=0.0.0.0/0&GatewayId=igw-01234")
	if code != 400 || !strings.Contains(string(body), "RouteAlreadyExists") {
		t.Errorf("a duplicate route = %d:\n%s", code, body)
	}
	mustEC2(t, s, "Action=DeleteRoute&RouteTableId="+got.ID+"&DestinationCidrBlock=0.0.0.0/0")
}

func TestRouteTableAssociation(t *testing.T) {
	s := newTestServer(t)
	vpcID := makeVPC(t, s, "10.0.0.0/16", "main")
	var sn struct {
		ID string `xml:"subnet>subnetId"`
	}
	_ = xml.Unmarshal(mustEC2(t, s, "Action=CreateSubnet&VpcId="+vpcID+"&CidrBlock=10.0.1.0/24"), &sn)
	var rt struct {
		ID string `xml:"routeTable>routeTableId"`
	}
	_ = xml.Unmarshal(mustEC2(t, s, "Action=CreateRouteTable&VpcId="+vpcID), &rt)

	var assoc struct {
		ID string `xml:"associationId"`
	}
	_ = xml.Unmarshal(mustEC2(t, s,
		"Action=AssociateRouteTable&RouteTableId="+rt.ID+"&SubnetId="+sn.ID), &assoc)
	if !strings.HasPrefix(assoc.ID, "rtbassoc-") {
		t.Fatalf("association id = %q", assoc.ID)
	}
	// The provider finds a table by the subnet it serves.
	var found struct {
		Tables []struct{} `xml:"routeTableSet>item"`
	}
	_ = xml.Unmarshal(mustEC2(t, s,
		"Action=DescribeRouteTables&Filter.1.Name=association.subnet-id&Filter.1.Value.1="+sn.ID), &found)
	if len(found.Tables) != 1 {
		t.Errorf("filtering by association.subnet-id gave %d, want 1", len(found.Tables))
	}
	mustEC2(t, s, "Action=DisassociateRouteTable&AssociationId="+assoc.ID)
	if code, _ := ec2(t, s, "Action=DisassociateRouteTable&AssociationId="+assoc.ID); code != 400 {
		t.Errorf("disassociating twice = %d, want 400", code)
	}
}

// The bug that stopped every aws_security_group from being created.
//
// AWS gives a new group an allow-all egress rule and the provider revokes
// it before adding its own. It sends FromPort=0 and ToPort=0 with
// IpProtocol=-1, while DescribeSecurityGroups reports that same rule with
// no ports at all -- so a strict comparison never matches and the create
// fails with InvalidPermission.NotFound.
func TestRevokingTheDefaultEgressRuleMatchesDespiteThePorts(t *testing.T) {
	s := newTestServer(t)
	vpcID := makeVPC(t, s, "10.0.0.0/16", "main")
	var sg struct {
		ID string `xml:"groupId"`
	}
	_ = xml.Unmarshal(mustEC2(t, s,
		"Action=CreateSecurityGroup&GroupName=web&GroupDescription=test&VpcId="+vpcID), &sg)

	code, body := ec2(t, s, "Action=RevokeSecurityGroupEgress&GroupId="+sg.ID+
		"&IpPermissions.1.FromPort=0&IpPermissions.1.IpProtocol=-1"+
		"&IpPermissions.1.IpRanges.1.CidrIp=0.0.0.0%2F0&IpPermissions.1.ToPort=0")
	if code != 200 {
		t.Fatalf("revoking the default egress rule failed with %d:\n%s", code, body)
	}

	var got struct {
		Egress []struct{} `xml:"securityGroupInfo>item>ipPermissionsEgress>item"`
	}
	_ = xml.Unmarshal(mustEC2(t, s, "Action=DescribeSecurityGroups&GroupId.1="+sg.ID), &got)
	if len(got.Egress) != 0 {
		t.Errorf("%d egress rules left, want none", len(got.Egress))
	}
}

func TestSecurityGroupRules(t *testing.T) {
	s := newTestServer(t)
	vpcID := makeVPC(t, s, "10.0.0.0/16", "main")
	var sg struct {
		ID string `xml:"groupId"`
	}
	_ = xml.Unmarshal(mustEC2(t, s,
		"Action=CreateSecurityGroup&GroupName=web&GroupDescription=test&VpcId="+vpcID), &sg)

	rule := "&IpPermissions.1.IpProtocol=tcp&IpPermissions.1.FromPort=443&IpPermissions.1.ToPort=443" +
		"&IpPermissions.1.IpRanges.1.CidrIp=0.0.0.0%2F0"
	mustEC2(t, s, "Action=AuthorizeSecurityGroupIngress&GroupId="+sg.ID+rule)

	ingress := func() []sgRule { return sgIngress(t, s, sg.ID) }
	if got := ingress(); len(got) != 1 || got[0].Protocol != "tcp" || got[0].From != 443 {
		t.Fatalf("ingress = %+v", got)
	}

	// Adding the same rule twice is an error, as it is in AWS.
	if code, _ := ec2(t, s, "Action=AuthorizeSecurityGroupIngress&GroupId="+sg.ID+rule); code != 400 {
		t.Error("a duplicate rule should be refused")
	}
	mustEC2(t, s, "Action=RevokeSecurityGroupIngress&GroupId="+sg.ID+rule)
	if got := ingress(); len(got) != 0 {
		t.Errorf("%d ingress rules left", len(got))
	}
}

// The provider reads these while working with a VPC. Nimbus models
// neither; an empty set is the truthful answer and an error would fail the
// apply over something nobody asked for.
func TestTheEmptyDescribesAnswerRatherThanFail(t *testing.T) {
	s := newTestServer(t)
	for _, action := range []string{"DescribeNetworkAcls", "DescribeNetworkInterfaces"} {
		if code, body := ec2(t, s, "Action="+action); code != 200 {
			t.Errorf("%s = %d:\n%s", action, code, body)
		}
	}
}

// EC2-Classic has been gone for years, and a provider that sees "EC2" in
// supported-platforms takes a different, older code path.
func TestAccountAttributesSayVPCOnly(t *testing.T) {
	s := newTestServer(t)
	body := mustEC2(t, s, "Action=DescribeAccountAttributes")
	if !strings.Contains(string(body), "<attributeValue>VPC</attributeValue>") {
		t.Errorf("supported-platforms should be VPC:\n%s", body)
	}
	if strings.Contains(string(body), ">EC2<") {
		t.Error("EC2-Classic must not be advertised")
	}
}

func TestAvailabilityZonesFollowTheRegion(t *testing.T) {
	s := newTestServer(t)
	var got struct {
		Zones []struct {
			Name   string `xml:"zoneName"`
			Region string `xml:"regionName"`
			State  string `xml:"zoneState"`
		} `xml:"availabilityZoneInfo>item"`
	}
	_ = xml.Unmarshal(mustEC2(t, s, "Action=DescribeAvailabilityZones"), &got)
	if len(got.Zones) != 3 {
		t.Fatalf("%d zones, want 3", len(got.Zones))
	}
	if got.Zones[0].Name != DefaultRegion+"a" || got.Zones[0].State != "available" {
		t.Errorf("zone = %+v", got.Zones[0])
	}
}

func TestNatGatewayIsAvailableImmediately(t *testing.T) {
	s := newTestServer(t)
	vpcID := makeVPC(t, s, "10.0.0.0/16", "main")
	var sn struct {
		ID string `xml:"subnet>subnetId"`
	}
	_ = xml.Unmarshal(mustEC2(t, s, "Action=CreateSubnet&VpcId="+vpcID+"&CidrBlock=10.0.1.0/24"), &sn)
	var got struct {
		ID    string `xml:"natGateway>natGatewayId"`
		State string `xml:"natGateway>state"`
	}
	_ = xml.Unmarshal(mustEC2(t, s, "Action=CreateNatGateway&SubnetId="+sn.ID), &got)
	// Nothing is really started, so a client polling for readiness would
	// otherwise wait forever.
	if got.State != "available" {
		t.Errorf("state = %q, want available straight away", got.State)
	}
	res, _ := s.store.Get(got.ID)
	if res.ParentID != sn.ID {
		t.Errorf("a nat gateway hangs off its subnet, not %q", res.ParentID)
	}
}

// Decoding helpers that build a FRESH target each time. encoding/xml
// appends to a slice it finds already populated, so a shared variable
// silently accumulates every response -- which is what made this file's
// first draft report three tags where there were two.

type kv struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

func vpcTags(t *testing.T, s *Server) []kv {
	t.Helper()
	var got struct {
		Tags []kv `xml:"vpcSet>item>tagSet>item"`
	}
	if err := xml.Unmarshal(mustEC2(t, s, "Action=DescribeVpcs"), &got); err != nil {
		t.Fatal(err)
	}
	return got.Tags
}

type sgRule struct {
	Protocol string `xml:"ipProtocol"`
	From     int    `xml:"fromPort"`
}

func sgIngress(t *testing.T, s *Server, id string) []sgRule {
	t.Helper()
	var got struct {
		Ingress []sgRule `xml:"securityGroupInfo>item>ipPermissions>item"`
	}
	if err := xml.Unmarshal(mustEC2(t, s, "Action=DescribeSecurityGroups&GroupId.1="+id), &got); err != nil {
		t.Fatal(err)
	}
	return got.Ingress
}

// The provider revokes BOTH default egress rules after creating a group:
// the IPv4 0.0.0.0/0 one and an IPv6 ::/0 one. AWS only creates the second
// in an IPv6-enabled VPC, so against a v4-only VPC the second revoke fails
// with InvalidPermission.NotFound -- against real AWS too, which is why
// Terraform tolerates it and the apply succeeds. Nimbus must behave the
// same way rather than inventing a ::/0 rule to keep the log clean.
func TestTheIPv6DefaultEgressRevokeFailsLikeAWS(t *testing.T) {
	s := newTestServer(t)
	vpcID := makeVPC(t, s, "10.0.0.0/16", "main")
	var sg struct {
		ID string `xml:"groupId"`
	}
	_ = xml.Unmarshal(mustEC2(t, s,
		"Action=CreateSecurityGroup&GroupName=web&GroupDescription=test&VpcId="+vpcID), &sg)

	mustEC2(t, s, "Action=RevokeSecurityGroupEgress&GroupId="+sg.ID+
		"&IpPermissions.1.FromPort=0&IpPermissions.1.IpProtocol=-1"+
		"&IpPermissions.1.IpRanges.1.CidrIp=0.0.0.0%2F0&IpPermissions.1.ToPort=0")

	code, body := ec2(t, s, "Action=RevokeSecurityGroupEgress&GroupId="+sg.ID+
		"&IpPermissions.1.FromPort=0&IpPermissions.1.IpProtocol=-1"+
		"&IpPermissions.1.Ipv6Ranges.1.CidrIpv6=%3A%3A%2F0&IpPermissions.1.ToPort=0")
	if code != 400 || !strings.Contains(string(body), "InvalidPermission.NotFound") {
		t.Errorf("the ipv6 revoke = %d, want the same 400 AWS gives:\n%s", code, body)
	}
}

// An IPv6 rule that IS there round-trips, so the ranges are modelled
// rather than merely tolerated.
func TestIPv6RulesRoundTrip(t *testing.T) {
	s := newTestServer(t)
	vpcID := makeVPC(t, s, "10.0.0.0/16", "main")
	var sg struct {
		ID string `xml:"groupId"`
	}
	_ = xml.Unmarshal(mustEC2(t, s,
		"Action=CreateSecurityGroup&GroupName=web&GroupDescription=test&VpcId="+vpcID), &sg)

	rule := "&IpPermissions.1.IpProtocol=tcp&IpPermissions.1.FromPort=443&IpPermissions.1.ToPort=443" +
		"&IpPermissions.1.Ipv6Ranges.1.CidrIpv6=%3A%3A%2F0"
	mustEC2(t, s, "Action=AuthorizeSecurityGroupIngress&GroupId="+sg.ID+rule)

	var got struct {
		Cidrs []string `xml:"securityGroupInfo>item>ipPermissions>item>ipv6Ranges>item>cidrIpv6"`
	}
	_ = xml.Unmarshal(mustEC2(t, s, "Action=DescribeSecurityGroups&GroupId.1="+sg.ID), &got)
	if len(got.Cidrs) != 1 || got.Cidrs[0] != "::/0" {
		t.Fatalf("ipv6 ranges = %v", got.Cidrs)
	}
	mustEC2(t, s, "Action=RevokeSecurityGroupIngress&GroupId="+sg.ID+rule)
}
