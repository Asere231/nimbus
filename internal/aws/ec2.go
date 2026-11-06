package aws

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/Asere231/nimbus/internal/core"
)

// EC2's networking: VPCs, subnets, gateways, route tables, security groups.
//
// Everything here is Query/XML (see query.go) and everything is stored as
// an ordinary core.Resource, so the visualizer draws a VPC and its subnets
// with no AWS knowledge at all -- the subnet's ParentID is the VPC, which
// is the only thing the graph needs.
//
// Each kind is a Go struct with both xml and json tags. The struct is the
// schema: the JSON goes in the store, the XML goes on the wire, and the two
// can never drift apart because there is only one declaration.

const ec2Version = "2016-11-15"

// Resource types, named the way CloudFormation names them -- the closest
// thing AWS has to a canonical type string, and what the visualizer's
// types-aws.js table is keyed by.
const (
	typeVPC      = "AWS::EC2::VPC"
	typeSubnet   = "AWS::EC2::Subnet"
	typeIGW      = "AWS::EC2::InternetGateway"
	typeRouteTbl = "AWS::EC2::RouteTable"
	typeSG       = "AWS::EC2::SecurityGroup"
	typeNAT      = "AWS::EC2::NatGateway"
)

func (s *Server) serveEC2(w http.ResponseWriter, r *http.Request) string {
	p, err := readParams(r)
	if err != nil {
		writeError(w, "ec2", errorf(400, "MalformedInput", "reading the request: %v", err))
		return "bad ec2 request"
	}
	action := p.str("Action")

	var aerr *apiError
	switch action {
	// --- VPC
	case "CreateVpc":
		aerr = s.createVPC(w, p)
	case "DescribeVpcs":
		aerr = s.describeVPCs(w, p)
	case "DeleteVpc":
		aerr = s.deleteEntity(w, p, "VpcId", typeVPC, "DeleteVpcResponse")
	case "ModifyVpcAttribute":
		aerr = s.modifyVPCAttribute(w, p)
	case "DescribeVpcAttribute":
		aerr = s.describeVPCAttribute(w, p)

	// --- subnets
	case "CreateSubnet":
		aerr = s.createSubnet(w, p)
	case "DescribeSubnets":
		aerr = s.describeSubnets(w, p)
	case "DeleteSubnet":
		aerr = s.deleteEntity(w, p, "SubnetId", typeSubnet, "DeleteSubnetResponse")
	case "ModifySubnetAttribute":
		aerr = s.modifySubnetAttribute(w, p)

	// --- internet gateways
	case "CreateInternetGateway":
		aerr = s.createIGW(w, p)
	case "DescribeInternetGateways":
		aerr = s.describeIGWs(w, p)
	case "AttachInternetGateway":
		aerr = s.attachIGW(w, p, true)
	case "DetachInternetGateway":
		aerr = s.attachIGW(w, p, false)
	case "DeleteInternetGateway":
		aerr = s.deleteEntity(w, p, "InternetGatewayId", typeIGW, "DeleteInternetGatewayResponse")

	// --- route tables
	case "CreateRouteTable":
		aerr = s.createRouteTable(w, p)
	case "DescribeRouteTables":
		aerr = s.describeRouteTables(w, p)
	case "DeleteRouteTable":
		aerr = s.deleteEntity(w, p, "RouteTableId", typeRouteTbl, "DeleteRouteTableResponse")
	case "AssociateRouteTable":
		aerr = s.associateRouteTable(w, p)
	case "DisassociateRouteTable":
		aerr = s.disassociateRouteTable(w, p)
	case "CreateRoute":
		aerr = s.createRoute(w, p)
	case "DeleteRoute":
		aerr = s.deleteRoute(w, p)

	// --- security groups
	case "CreateSecurityGroup":
		aerr = s.createSecurityGroup(w, p)
	case "DescribeSecurityGroups":
		aerr = s.describeSecurityGroups(w, p)
	case "DeleteSecurityGroup":
		aerr = s.deleteEntity(w, p, "GroupId", typeSG, "DeleteSecurityGroupResponse")
	case "AuthorizeSecurityGroupIngress":
		aerr = s.authorizeSG(w, p, "ingress", true)
	case "AuthorizeSecurityGroupEgress":
		aerr = s.authorizeSG(w, p, "egress", true)
	case "RevokeSecurityGroupIngress":
		aerr = s.authorizeSG(w, p, "ingress", false)
	case "RevokeSecurityGroupEgress":
		aerr = s.authorizeSG(w, p, "egress", false)

	// --- NAT gateways (recorded, nothing runs)
	case "CreateNatGateway":
		aerr = s.createNAT(w, p)
	case "DescribeNatGateways":
		aerr = s.describeNATs(w, p)
	case "DeleteNatGateway":
		aerr = s.deleteEntity(w, p, "NatGatewayId", typeNAT, "DeleteNatGatewayResponse")

	// --- tags and account-level reads
	case "CreateTags":
		aerr = s.createTags(w, p, true)
	case "DeleteTags":
		aerr = s.createTags(w, p, false)
	case "DescribeTags":
		aerr = s.describeTags(w, p)
	case "DescribeNetworkAcls":
		aerr = s.describeNetworkAcls(w, p)
	case "DescribeNetworkInterfaces":
		aerr = s.describeNetworkInterfaces(w, p)
	case "DescribeAvailabilityZones":
		aerr = s.describeAZs(w, p)
	case "DescribeAccountAttributes":
		aerr = s.describeAccountAttributes(w, p)

	default:
		writeError(w, "ec2", invalidAction("ec2", action))
		return "unimplemented: ec2 " + action
	}

	if aerr != nil {
		writeError(w, "ec2", aerr)
		return "ec2 " + action + ": " + aerr.Code
	}
	return ""
}

// --- the pieces every kind shares ----------------------------------------

// tag is EC2's key/value pair, which appears in a <tagSet> on everything.
type tag struct {
	Key   string `xml:"key" json:"key"`
	Value string `xml:"value" json:"value"`
}

// entity is what every EC2 kind can do, so the generic helpers below --
// load, save, filter, tag, delete -- work for all of them.
type entity interface {
	id() string
	tags() []tag
	setTags([]tag)
	// field returns the value an EC2 filter name refers to, and whether
	// this kind has such a field at all.
	field(name string) ([]string, bool)
}

// load reads one entity out of the store.
func load[T entity](s *Server, id string, into T) (T, error) {
	res, err := s.store.Get(id)
	if err != nil {
		return into, err
	}
	err = json.Unmarshal(res.Body, into)
	return into, err
}

// save writes an entity, creating or replacing the stored resource.
func (s *Server) save(e entity, typ, name, parent string) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	s.store.Put(&core.Resource{
		Cloud: "aws", ID: e.id(), Type: typ, Name: name, ParentID: parent,
		Location: s.cfg.Region, Backing: "modelled", Body: body,
	})
	return nil
}

// list returns every stored entity of a type, oldest first, decoded.
func list[T any](s *Server, typ string) []T {
	var out []T
	for _, res := range s.store.List(func(r *core.Resource) bool { return r.Cloud == "aws" && r.Type == typ }) {
		var v T
		if json.Unmarshal(res.Body, &v) == nil {
			out = append(out, v)
		}
	}
	return out
}

// filters pulls the Filter.N.Name / Filter.N.Value.M parameters out of a
// request.
//
// A describe call with a filter nobody implemented must return everything
// rather than fail: the provider sends filters we have not thought of, and
// an error there fails an apply for no reason. Ignoring one can only ever
// return too much, which the client then narrows itself.
type filter struct {
	Name   string
	Values []string
}

func readFilters(p params) []filter {
	var out []filter
	for i := 1; ; i++ {
		name := p.str(fmt.Sprintf("Filter.%d.Name", i))
		if name == "" {
			return out
		}
		f := filter{Name: name}
		for j := 1; ; j++ {
			v := p.str(fmt.Sprintf("Filter.%d.Value.%d", i, j))
			if v == "" {
				break
			}
			f.Values = append(f.Values, v)
		}
		out = append(out, f)
	}
}

// matches reports whether an entity satisfies every filter that it
// recognises. "tag:Key" and "tag-key" are handled here, because every kind
// has tags.
func matches(e entity, fs []filter) bool {
	for _, f := range fs {
		var have []string
		var known bool
		switch {
		case strings.HasPrefix(f.Name, "tag:"):
			want := strings.TrimPrefix(f.Name, "tag:")
			known = true
			for _, t := range e.tags() {
				if t.Key == want {
					have = append(have, t.Value)
				}
			}
		case f.Name == "tag-key":
			known = true
			for _, t := range e.tags() {
				have = append(have, t.Key)
			}
		default:
			have, known = e.field(f.Name)
		}
		if !known {
			continue // a filter this kind does not have is ignored, not an error
		}
		if !anyMatch(have, f.Values) {
			return false
		}
	}
	return true
}

// anyMatch is EC2's filter semantics: the values of one filter are ORed,
// and "*" globs are allowed.
func anyMatch(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	for _, w := range want {
		for _, h := range have {
			if globMatch(w, h) {
				return true
			}
		}
	}
	return false
}

func globMatch(pattern, s string) bool {
	if !strings.ContainsAny(pattern, "*?") {
		return pattern == s
	}
	// A small glob: * is any run, ? is one character.
	var rec func(p, v string) bool
	rec = func(p, v string) bool {
		for len(p) > 0 {
			switch p[0] {
			case '*':
				for i := 0; i <= len(v); i++ {
					if rec(p[1:], v[i:]) {
						return true
					}
				}
				return false
			case '?':
				if len(v) == 0 {
					return false
				}
				p, v = p[1:], v[1:]
			default:
				if len(v) == 0 || v[0] != p[0] {
					return false
				}
				p, v = p[1:], v[1:]
			}
		}
		return len(v) == 0
	}
	return rec(pattern, s)
}

// readTagSpecifications reads the tags a create call carries inline:
// TagSpecification.1.ResourceType=vpc & TagSpecification.1.Tag.1.Key=Name.
// The provider tags on create rather than with a second CreateTags call,
// so a resource whose tags appeared only after a CreateTags would show the
// wrong name in the graph for the life of the apply.
func readTagSpecifications(p params) []tag {
	var out []tag
	for i := 1; ; i++ {
		prefix := fmt.Sprintf("TagSpecification.%d", i)
		if p.str(prefix+".ResourceType") == "" && p.str(prefix+".Tag.1.Key") == "" {
			return out
		}
		for j := 1; ; j++ {
			key := p.str(fmt.Sprintf("%s.Tag.%d.Key", prefix, j))
			if key == "" {
				break
			}
			out = append(out, tag{Key: key, Value: p.str(fmt.Sprintf("%s.Tag.%d.Value", prefix, j))})
		}
	}
}

// nameOf is what the visualizer labels a resource with: its Name tag if it
// has one, otherwise its id. Without this every card in the graph would
// read "vpc-0a1b2c3d", which is true and useless.
func nameOf(e entity) string {
	for _, t := range e.tags() {
		if t.Key == "Name" && t.Value != "" {
			return t.Value
		}
	}
	return e.id()
}

// mergeTags adds or replaces tags by key, and with add=false removes them.
func mergeTags(existing, incoming []tag, add bool) []tag {
	out := append([]tag(nil), existing...)
	for _, t := range incoming {
		found := -1
		for i, e := range out {
			if e.Key == t.Key {
				found = i
				break
			}
		}
		switch {
		case !add && found >= 0:
			out = append(out[:found], out[found+1:]...)
		case add && found >= 0:
			out[found] = t
		case add:
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// emptyResponse is the shape every delete and modify answers with.
type emptyResponse struct {
	XMLName   xml.Name
	Xmlns     string `xml:"xmlns,attr"`
	RequestID string `xml:"requestId"`
	Return    bool   `xml:"return"`
}

func writeEmpty(w http.ResponseWriter, element string) {
	writeXML(w, 200, emptyResponse{
		XMLName:   xml.Name{Local: element},
		Xmlns:     namespaceFor("ec2"),
		RequestID: requestID(),
		Return:    true,
	})
}

// deleteEntity removes a resource by id, with the cascade the store already
// does for children (a VPC's subnets go with it).
func (s *Server) deleteEntity(w http.ResponseWriter, p params, idParam, typ, element string) *apiError {
	id := p.str(idParam)
	if id == "" {
		return errorf(400, "MissingParameter", "the request must contain the parameter %s", idParam)
	}
	res, err := s.store.Get(id)
	if err != nil || res.Type != typ {
		return notFound(typ, id)
	}
	if _, err := s.store.Delete(id); err != nil {
		return notFound(typ, id)
	}
	writeEmpty(w, element)
	return nil
}

// notFound is the error code EC2 uses per kind. They are not
// interchangeable: the provider matches on the code to tell "already gone"
// from "something is wrong", and a destroy that gets the wrong one retries
// until it times out.
func notFound(typ, id string) *apiError {
	code := map[string]string{
		typeVPC:      "InvalidVpcID.NotFound",
		typeSubnet:   "InvalidSubnetID.NotFound",
		typeIGW:      "InvalidInternetGatewayID.NotFound",
		typeRouteTbl: "InvalidRouteTableID.NotFound",
		typeSG:       "InvalidGroup.NotFound",
		typeNAT:      "NatGatewayNotFound",
	}[typ]
	if code == "" {
		code = "InvalidParameterValue"
	}
	return errorf(400, code, "The ID %q does not exist", id)
}
