package aws

import (
	"encoding/xml"
	"net/http"
)

// Internet gateways, route tables and NAT gateways.

// --- internet gateways ----------------------------------------------------

type igwAttachment struct {
	VpcID string `xml:"vpcId" json:"vpcId"`
	State string `xml:"state" json:"state"`
}

type internetGateway struct {
	InternetGatewayID string          `xml:"internetGatewayId" json:"internetGatewayId"`
	OwnerID           string          `xml:"ownerId" json:"ownerId"`
	AttachmentSet     []igwAttachment `xml:"attachmentSet>item" json:"attachmentSet"`
	TagSet            []tag           `xml:"tagSet>item" json:"tagSet"`
}

func (g *internetGateway) id() string      { return g.InternetGatewayID }
func (g *internetGateway) tags() []tag     { return g.TagSet }
func (g *internetGateway) setTags(t []tag) { g.TagSet = t }
func (g *internetGateway) field(name string) ([]string, bool) {
	switch name {
	case "internet-gateway-id":
		return []string{g.InternetGatewayID}, true
	case "attachment.vpc-id":
		var out []string
		for _, a := range g.AttachmentSet {
			out = append(out, a.VpcID)
		}
		return out, true
	case "attachment.state":
		var out []string
		for _, a := range g.AttachmentSet {
			out = append(out, a.State)
		}
		return out, true
	case "owner-id":
		return []string{g.OwnerID}, true
	}
	return nil, false
}

// attachedTo is where the gateway hangs in the graph: under its VPC once
// attached, under the account before that.
func (s *Server) igwParent(g *internetGateway) string {
	if len(g.AttachmentSet) > 0 {
		return g.AttachmentSet[0].VpcID
	}
	return s.accountARN()
}

type createIGWResponse struct {
	XMLName         xml.Name        `xml:"CreateInternetGatewayResponse"`
	Xmlns           string          `xml:"xmlns,attr"`
	RequestID       string          `xml:"requestId"`
	InternetGateway internetGateway `xml:"internetGateway"`
}

func (s *Server) createIGW(w http.ResponseWriter, p params) *apiError {
	g := &internetGateway{
		InternetGatewayID: newID("igw"),
		OwnerID:           s.cfg.AccountID,
		AttachmentSet:     []igwAttachment{},
		TagSet:            readTagSpecifications(p),
	}
	if err := s.save(g, typeIGW, nameOf(g), s.igwParent(g)); err != nil {
		return errorf(500, "InternalError", "storing the gateway: %v", err)
	}
	writeXML(w, 200, createIGWResponse{Xmlns: namespaceFor("ec2"), RequestID: requestID(), InternetGateway: *g})
	return nil
}

type describeIGWsResponse struct {
	XMLName            xml.Name          `xml:"DescribeInternetGatewaysResponse"`
	Xmlns              string            `xml:"xmlns,attr"`
	RequestID          string            `xml:"requestId"`
	InternetGatewaySet []internetGateway `xml:"internetGatewaySet>item"`
}

func (s *Server) describeIGWs(w http.ResponseWriter, p params) *apiError {
	fs := readFilters(p)
	wanted := p.list("InternetGatewayId", "")
	out := []internetGateway{}
	for _, g := range list[internetGateway](s, typeIGW) {
		g := g
		if len(wanted) > 0 && !contains(wanted, g.InternetGatewayID) {
			continue
		}
		if !matches(&g, fs) {
			continue
		}
		out = append(out, g)
	}
	if len(wanted) > 0 && len(out) == 0 {
		return notFound(typeIGW, wanted[0])
	}
	writeXML(w, 200, describeIGWsResponse{Xmlns: namespaceFor("ec2"), RequestID: requestID(), InternetGatewaySet: out})
	return nil
}

// attachIGW moves the gateway under its VPC in the graph as well as
// recording the attachment, so detaching puts it back at the account.
func (s *Server) attachIGW(w http.ResponseWriter, p params, attach bool) *apiError {
	id := p.str("InternetGatewayId")
	g, err := load(s, id, &internetGateway{})
	if err != nil {
		return notFound(typeIGW, id)
	}
	vpcID := p.str("VpcId")
	if _, err := load(s, vpcID, &vpc{}); err != nil {
		return notFound(typeVPC, vpcID)
	}
	if attach {
		g.AttachmentSet = []igwAttachment{{VpcID: vpcID, State: "available"}}
	} else {
		g.AttachmentSet = []igwAttachment{}
	}
	if err := s.save(g, typeIGW, nameOf(g), s.igwParent(g)); err != nil {
		return errorf(500, "InternalError", "storing the gateway: %v", err)
	}
	if attach {
		writeEmpty(w, "AttachInternetGatewayResponse")
	} else {
		writeEmpty(w, "DetachInternetGatewayResponse")
	}
	return nil
}

// --- route tables ---------------------------------------------------------

type route struct {
	DestinationCidrBlock string `xml:"destinationCidrBlock,omitempty" json:"destinationCidrBlock,omitempty"`
	GatewayID            string `xml:"gatewayId,omitempty" json:"gatewayId,omitempty"`
	NatGatewayID         string `xml:"natGatewayId,omitempty" json:"natGatewayId,omitempty"`
	State                string `xml:"state" json:"state"`
	Origin               string `xml:"origin" json:"origin"`
}

type routeAssociation struct {
	RouteTableAssociationID string `xml:"routeTableAssociationId" json:"routeTableAssociationId"`
	RouteTableID            string `xml:"routeTableId" json:"routeTableId"`
	SubnetID                string `xml:"subnetId,omitempty" json:"subnetId,omitempty"`
	Main                    bool   `xml:"main" json:"main"`
	AssociationState        struct {
		State string `xml:"state" json:"state"`
	} `xml:"associationState" json:"associationState"`
}

type routeTable struct {
	RouteTableID      string             `xml:"routeTableId" json:"routeTableId"`
	VpcID             string             `xml:"vpcId" json:"vpcId"`
	OwnerID           string             `xml:"ownerId" json:"ownerId"`
	RouteSet          []route            `xml:"routeSet>item" json:"routeSet"`
	AssociationSet    []routeAssociation `xml:"associationSet>item" json:"associationSet"`
	PropagatingVgwSet []struct{}         `xml:"propagatingVgwSet>item,omitempty" json:"-"`
	TagSet            []tag              `xml:"tagSet>item" json:"tagSet"`
}

func (t *routeTable) id() string      { return t.RouteTableID }
func (t *routeTable) tags() []tag     { return t.TagSet }
func (t *routeTable) setTags(x []tag) { t.TagSet = x }
func (t *routeTable) field(name string) ([]string, bool) {
	switch name {
	case "route-table-id":
		return []string{t.RouteTableID}, true
	case "vpc-id":
		return []string{t.VpcID}, true
	case "association.subnet-id":
		var out []string
		for _, a := range t.AssociationSet {
			if a.SubnetID != "" {
				out = append(out, a.SubnetID)
			}
		}
		return out, true
	case "association.main":
		var out []string
		for _, a := range t.AssociationSet {
			out = append(out, boolString(a.Main))
		}
		return out, true
	case "owner-id":
		return []string{t.OwnerID}, true
	}
	return nil, false
}

type createRouteTableResponse struct {
	XMLName    xml.Name   `xml:"CreateRouteTableResponse"`
	Xmlns      string     `xml:"xmlns,attr"`
	RequestID  string     `xml:"requestId"`
	RouteTable routeTable `xml:"routeTable"`
}

func (s *Server) createRouteTable(w http.ResponseWriter, p params) *apiError {
	vpcID := p.str("VpcId")
	v, err := load(s, vpcID, &vpc{})
	if err != nil {
		return notFound(typeVPC, vpcID)
	}
	t := &routeTable{
		RouteTableID: newID("rtb"),
		VpcID:        vpcID,
		OwnerID:      s.cfg.AccountID,
		// Every route table starts with the VPC's own local route, which
		// the provider expects to find and does not manage.
		RouteSet: []route{{
			DestinationCidrBlock: v.CidrBlock,
			GatewayID:            "local",
			State:                "active",
			Origin:               "CreateRouteTable",
		}},
		AssociationSet: []routeAssociation{},
		TagSet:         readTagSpecifications(p),
	}
	if err := s.save(t, typeRouteTbl, nameOf(t), vpcID); err != nil {
		return errorf(500, "InternalError", "storing the route table: %v", err)
	}
	writeXML(w, 200, createRouteTableResponse{Xmlns: namespaceFor("ec2"), RequestID: requestID(), RouteTable: *t})
	return nil
}

type describeRouteTablesResponse struct {
	XMLName       xml.Name     `xml:"DescribeRouteTablesResponse"`
	Xmlns         string       `xml:"xmlns,attr"`
	RequestID     string       `xml:"requestId"`
	RouteTableSet []routeTable `xml:"routeTableSet>item"`
}

func (s *Server) describeRouteTables(w http.ResponseWriter, p params) *apiError {
	fs := readFilters(p)
	wanted := p.list("RouteTableId", "")
	out := []routeTable{}
	for _, t := range list[routeTable](s, typeRouteTbl) {
		t := t
		if len(wanted) > 0 && !contains(wanted, t.RouteTableID) {
			continue
		}
		if !matches(&t, fs) {
			continue
		}
		out = append(out, t)
	}
	if len(wanted) > 0 && len(out) == 0 {
		return notFound(typeRouteTbl, wanted[0])
	}
	writeXML(w, 200, describeRouteTablesResponse{Xmlns: namespaceFor("ec2"), RequestID: requestID(), RouteTableSet: out})
	return nil
}

type associateRouteTableResponse struct {
	XMLName          xml.Name `xml:"AssociateRouteTableResponse"`
	Xmlns            string   `xml:"xmlns,attr"`
	RequestID        string   `xml:"requestId"`
	AssociationID    string   `xml:"associationId"`
	AssociationState struct {
		State string `xml:"state"`
	} `xml:"associationState"`
}

func (s *Server) associateRouteTable(w http.ResponseWriter, p params) *apiError {
	id := p.str("RouteTableId")
	t, err := load(s, id, &routeTable{})
	if err != nil {
		return notFound(typeRouteTbl, id)
	}
	subnetID := p.str("SubnetId")
	if subnetID != "" {
		if _, err := load(s, subnetID, &subnet{}); err != nil {
			return notFound(typeSubnet, subnetID)
		}
	}
	a := routeAssociation{
		RouteTableAssociationID: "rtbassoc-0" + hexID(16),
		RouteTableID:            id,
		SubnetID:                subnetID,
	}
	a.AssociationState.State = "associated"
	t.AssociationSet = append(t.AssociationSet, a)
	if err := s.save(t, typeRouteTbl, nameOf(t), t.VpcID); err != nil {
		return errorf(500, "InternalError", "storing the route table: %v", err)
	}
	doc := associateRouteTableResponse{Xmlns: namespaceFor("ec2"), RequestID: requestID(), AssociationID: a.RouteTableAssociationID}
	doc.AssociationState.State = "associated"
	writeXML(w, 200, doc)
	return nil
}

func (s *Server) disassociateRouteTable(w http.ResponseWriter, p params) *apiError {
	want := p.str("AssociationId")
	for _, t := range list[routeTable](s, typeRouteTbl) {
		t := t
		for i, a := range t.AssociationSet {
			if a.RouteTableAssociationID != want {
				continue
			}
			t.AssociationSet = append(t.AssociationSet[:i], t.AssociationSet[i+1:]...)
			if err := s.save(&t, typeRouteTbl, nameOf(&t), t.VpcID); err != nil {
				return errorf(500, "InternalError", "storing the route table: %v", err)
			}
			writeEmpty(w, "DisassociateRouteTableResponse")
			return nil
		}
	}
	return errorf(400, "InvalidAssociationID.NotFound", "The association ID %q does not exist", want)
}

func (s *Server) createRoute(w http.ResponseWriter, p params) *apiError {
	id := p.str("RouteTableId")
	t, err := load(s, id, &routeTable{})
	if err != nil {
		return notFound(typeRouteTbl, id)
	}
	r := route{
		DestinationCidrBlock: p.str("DestinationCidrBlock"),
		GatewayID:            p.str("GatewayId"),
		NatGatewayID:         p.str("NatGatewayId"),
		State:                "active",
		Origin:               "CreateRoute",
	}
	for _, existing := range t.RouteSet {
		if existing.DestinationCidrBlock == r.DestinationCidrBlock {
			return errorf(400, "RouteAlreadyExists",
				"The route identified by %s already exists", r.DestinationCidrBlock)
		}
	}
	t.RouteSet = append(t.RouteSet, r)
	if err := s.save(t, typeRouteTbl, nameOf(t), t.VpcID); err != nil {
		return errorf(500, "InternalError", "storing the route table: %v", err)
	}
	writeEmpty(w, "CreateRouteResponse")
	return nil
}

func (s *Server) deleteRoute(w http.ResponseWriter, p params) *apiError {
	id := p.str("RouteTableId")
	t, err := load(s, id, &routeTable{})
	if err != nil {
		return notFound(typeRouteTbl, id)
	}
	cidr := p.str("DestinationCidrBlock")
	for i, r := range t.RouteSet {
		if r.DestinationCidrBlock == cidr {
			t.RouteSet = append(t.RouteSet[:i], t.RouteSet[i+1:]...)
			if err := s.save(t, typeRouteTbl, nameOf(t), t.VpcID); err != nil {
				return errorf(500, "InternalError", "storing the route table: %v", err)
			}
			writeEmpty(w, "DeleteRouteResponse")
			return nil
		}
	}
	return errorf(400, "InvalidRoute.NotFound", "no route with destination %q", cidr)
}

// --- NAT gateways (recorded, nothing runs) --------------------------------

type natAddress struct {
	AllocationID       string `xml:"allocationId,omitempty" json:"allocationId,omitempty"`
	NetworkInterfaceID string `xml:"networkInterfaceId,omitempty" json:"networkInterfaceId,omitempty"`
	PrivateIP          string `xml:"privateIp,omitempty" json:"privateIp,omitempty"`
	PublicIP           string `xml:"publicIp,omitempty" json:"publicIp,omitempty"`
}

type natGateway struct {
	NatGatewayID        string       `xml:"natGatewayId" json:"natGatewayId"`
	State               string       `xml:"state" json:"state"`
	SubnetID            string       `xml:"subnetId" json:"subnetId"`
	VpcID               string       `xml:"vpcId" json:"vpcId"`
	ConnectivityType    string       `xml:"connectivityType" json:"connectivityType"`
	CreateTime          string       `xml:"createTime" json:"createTime"`
	NatGatewayAddresses []natAddress `xml:"natGatewayAddressSet>item" json:"natGatewayAddressSet"`
	TagSet              []tag        `xml:"tagSet>item" json:"tagSet"`
}

func (n *natGateway) id() string      { return n.NatGatewayID }
func (n *natGateway) tags() []tag     { return n.TagSet }
func (n *natGateway) setTags(t []tag) { n.TagSet = t }
func (n *natGateway) field(name string) ([]string, bool) {
	switch name {
	case "nat-gateway-id":
		return []string{n.NatGatewayID}, true
	case "subnet-id":
		return []string{n.SubnetID}, true
	case "vpc-id":
		return []string{n.VpcID}, true
	case "state":
		return []string{n.State}, true
	}
	return nil, false
}

type createNATResponse struct {
	XMLName    xml.Name   `xml:"CreateNatGatewayResponse"`
	Xmlns      string     `xml:"xmlns,attr"`
	RequestID  string     `xml:"requestId"`
	NatGateway natGateway `xml:"natGateway"`
}

func (s *Server) createNAT(w http.ResponseWriter, p params) *apiError {
	subnetID := p.str("SubnetId")
	n, err := load(s, subnetID, &subnet{})
	if err != nil {
		return notFound(typeSubnet, subnetID)
	}
	g := &natGateway{
		NatGatewayID: newID("nat"),
		// "available" straight away: nothing is really started, and a
		// client that polls for readiness would otherwise wait forever.
		State:            "available",
		SubnetID:         subnetID,
		VpcID:            n.VpcID,
		ConnectivityType: p.strOr("ConnectivityType", "public"),
		CreateTime:       nowRFC3339(),
		NatGatewayAddresses: []natAddress{{
			AllocationID:       p.str("AllocationId"),
			NetworkInterfaceID: newID("eni"),
			PrivateIP:          "10.0.0.10",
			PublicIP:           "198.51.100.10",
		}},
		TagSet: readTagSpecifications(p),
	}
	if err := s.save(g, typeNAT, nameOf(g), subnetID); err != nil {
		return errorf(500, "InternalError", "storing the nat gateway: %v", err)
	}
	writeXML(w, 200, createNATResponse{Xmlns: namespaceFor("ec2"), RequestID: requestID(), NatGateway: *g})
	return nil
}

type describeNATsResponse struct {
	XMLName       xml.Name     `xml:"DescribeNatGatewaysResponse"`
	Xmlns         string       `xml:"xmlns,attr"`
	RequestID     string       `xml:"requestId"`
	NatGatewaySet []natGateway `xml:"natGatewaySet>item"`
}

func (s *Server) describeNATs(w http.ResponseWriter, p params) *apiError {
	fs := readFilters(p)
	wanted := p.list("NatGatewayId", "")
	out := []natGateway{}
	for _, g := range list[natGateway](s, typeNAT) {
		g := g
		if len(wanted) > 0 && !contains(wanted, g.NatGatewayID) {
			continue
		}
		if !matches(&g, fs) {
			continue
		}
		out = append(out, g)
	}
	if len(wanted) > 0 && len(out) == 0 {
		return notFound(typeNAT, wanted[0])
	}
	writeXML(w, 200, describeNATsResponse{Xmlns: namespaceFor("ec2"), RequestID: requestID(), NatGatewaySet: out})
	return nil
}
