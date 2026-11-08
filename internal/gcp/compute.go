package gcp

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/Asere231/nimbus/internal/core"
)

// Compute: networks, subnetworks, firewalls, routes and routers.
//
// The paths are regular enough to route generically, which is what this
// file does: everything is
//
//	/compute/v1/projects/{project}/{scope}/{collection}[/{name}]
//
// where scope is `global`, `regions/{region}` or `zones/{zone}`. A POST to
// the collection creates, a GET on the name reads, a PATCH applies an
// update mask and a DELETE removes -- and each write answers with an
// Operation from the matching scope.
//
// Per-type behaviour (what a create fills in, what a name must look like)
// lives in the table below rather than in a switch, the same shape the
// Azure plugin's hooks have.

// computeKind describes one Compute collection.
type computeKind struct {
	// collection is the path segment: "networks", "subnetworks", ...
	collection string
	// scope is "global" or "regions" -- the two Nimbus serves. Nothing
	// zonal exists yet, because nothing here runs an instance.
	regional bool
	// resourceType is what the store and the visualizer call it.
	resourceType string
	// kind is the `kind` field in the response: "compute#network".
	kind string
	// fill adds the server-generated fields to a body on create.
	fill func(s *Server, scope string, body map[string]any)
	// parent returns the resource this one hangs under in the graph. Empty
	// means the project.
	parent func(s *Server, body map[string]any) string
}

var computeKinds = []computeKind{
	{
		collection: "networks", resourceType: TypeNetwork, kind: "compute#network",
		fill: func(s *Server, _ string, body map[string]any) {
			// An auto-mode network creates a subnetwork per region; a
			// custom-mode one does not. The provider sends
			// autoCreateSubnetworks explicitly, so the field is never
			// absent in practice -- but a default of true matches the API.
			if _, ok := body["autoCreateSubnetworks"]; !ok {
				body["autoCreateSubnetworks"] = true
			}
			body["routingConfig"] = orDefault(body["routingConfig"],
				map[string]any{"routingMode": "REGIONAL"})
			body["x_gateway_hint"] = nil
			delete(body, "x_gateway_hint")
			body["subnetworks"] = orDefault(body["subnetworks"], []any{})
			body["peerings"] = orDefault(body["peerings"], []any{})
			// Measured: the provider sends this on every create, and reads
			// it straight back. Echoing what arrived is right; inventing a
			// different default would be a diff on the first plan.
			body["networkFirewallPolicyEnforcementOrder"] = orDefault(
				body["networkFirewallPolicyEnforcementOrder"], "AFTER_CLASSIC_FIREWALL")
		},
	},
	{
		collection: "subnetworks", regional: true,
		resourceType: TypeSubnetwork, kind: "compute#subnetwork",
		fill: func(s *Server, scope string, body map[string]any) {
			body["gatewayAddress"] = orDefault(body["gatewayAddress"], firstAddress(str(body["ipCidrRange"])))
			body["privateIpGoogleAccess"] = orDefault(body["privateIpGoogleAccess"], false)
			body["purpose"] = orDefault(body["purpose"], "PRIVATE")
			body["stackType"] = orDefault(body["stackType"], "IPV4_ONLY")
			// The provider sends `region` as a partial URL; it must come
			// back as a FULL self link or the resource reads as being in a
			// different region than it was created in.
			body["region"] = s.base() + "/compute/v1/projects/" + s.cfg.ProjectID +
				"/" + scope
		},
		parent: func(s *Server, body map[string]any) string {
			return s.linkFor(str(body["network"]))
		},
	},
	{
		collection: "firewalls", resourceType: TypeFirewall, kind: "compute#firewall",
		fill: func(s *Server, _ string, body map[string]any) {
			body["direction"] = orDefault(body["direction"], "INGRESS")
			body["priority"] = orDefault(body["priority"], float64(1000))
			body["disabled"] = orDefault(body["disabled"], false)
		},
		parent: func(s *Server, body map[string]any) string {
			return s.linkFor(str(body["network"]))
		},
	},
	{
		collection: "routes", resourceType: TypeRoute, kind: "compute#route",
		fill: func(s *Server, _ string, body map[string]any) {
			body["priority"] = orDefault(body["priority"], float64(1000))
		},
		parent: func(s *Server, body map[string]any) string {
			return s.linkFor(str(body["network"]))
		},
	},
	{
		collection: "routers", regional: true,
		resourceType: TypeRouter, kind: "compute#router",
		fill: func(s *Server, scope string, body map[string]any) {
			// Cloud NAT is recorded, never performed: nothing in Nimbus
			// routes a packet. The config round-trips so a plan settles.
			body["region"] = s.base() + "/compute/v1/projects/" + s.cfg.ProjectID + "/" + scope
			body["nats"] = orDefault(body["nats"], []any{})
		},
		parent: func(s *Server, body map[string]any) string {
			return s.linkFor(str(body["network"]))
		},
	},
}

func kindFor(collection string) *computeKind {
	for i := range computeKinds {
		if computeKinds[i].collection == collection {
			return &computeKinds[i]
		}
	}
	return nil
}

// serveCompute routes a Compute request.
//
// parts is the whole path: compute / v1 / projects / {p} / {scope...} /
// {collection} / [name].
func (s *Server) serveCompute(w http.ResponseWriter, r *http.Request, parts []string) string {
	if len(parts) < 4 || parts[1] != "v1" || parts[2] != "projects" {
		writeError(w, notFound("Nimbus does not implement %s %s yet", r.Method, r.URL.Path))
		return "compute " + r.URL.Path
	}
	rest := parts[4:] // after the project

	// `GET /compute/v1/projects/{p}` -- the provider fetches this to turn a
	// project number back into a project id, which it does after reading a
	// bucket. Measured; it is the only Resource Manager-shaped call Compute
	// makes.
	if len(rest) == 0 {
		return s.computeProject(w, parts[3])
	}

	scope, collection, name := splitComputeScope(rest)
	if collection == "" {
		// A bare scope listing: /regions, /zones, /global.
		return s.computeScopeList(w, rest)
	}

	switch collection {
	case "operations":
		if r.Method == http.MethodGet && name != "" {
			return s.getOperation(w, s.computeLink(scope, "operations", name))
		}
	case "instanceGroupManagers":
		// A GKE node pool's nodes, as the Compute API sees them. Derived
		// from the pool rather than stored; see gke_instancegroup.go.
		if r.Method == http.MethodGet && name != "" {
			return s.getInstanceGroupManager(w, scope, name)
		}
	case "regions", "zones":
		// `/regions` lists them, `/regions/{name}` describes one. The
		// scope splitter cannot tell those apart on its own, because
		// `regions` is also the first half of a regional SCOPE.
		if name == "" {
			return s.computeScopeList(w, []string{collection})
		}
		if collection == "regions" {
			return s.describeRegion(w, name)
		}
		return s.describeZone(w, name)
	}

	kind := kindFor(collection)
	if kind == nil {
		writeError(w, notFound("Nimbus does not implement the Compute collection %q yet", collection))
		return "compute collection " + collection
	}
	if kind.regional && !strings.HasPrefix(scope, "regions/") {
		writeError(w, badRequest("%s is a regional resource", collection))
		return "compute scope"
	}

	switch {
	case name == "" && r.Method == http.MethodGet:
		return s.listCompute(w, kind, scope)
	case name == "" && r.Method == http.MethodPost:
		return s.insertCompute(w, r, kind, scope)
	case name != "" && r.Method == http.MethodGet:
		return s.getCompute(w, kind, scope, name)
	case name != "" && (r.Method == http.MethodPatch || r.Method == http.MethodPut):
		return s.patchCompute(w, r, kind, scope, name)
	case name != "" && r.Method == http.MethodDelete:
		return s.deleteCompute(w, kind, scope, name)
	}
	writeError(w, notFound("Nimbus does not implement %s %s yet", r.Method, r.URL.Path))
	return "compute " + r.Method + " " + collection
}

// splitComputeScope reads the scope, the collection and the name out of
// what follows the project. The scope is one or two segments -- `global`,
// `regions/{r}`, `zones/{z}` -- and what follows is the collection.
//
// `/regions` and `/zones` with nothing after them are LISTS of regions and
// zones, not a scope, which is why the caller checks for an empty
// collection before trusting this.
func splitComputeScope(rest []string) (scope, collection, name string) {
	switch {
	case rest[0] == "global":
		scope = "global"
		rest = rest[1:]
	case (rest[0] == "regions" || rest[0] == "zones") && len(rest) >= 2:
		// /regions/{r}/subnetworks -- a scope. /regions/{r} alone is a
		// region lookup, which the caller sends to describeRegion.
		if len(rest) == 2 {
			return "", rest[0], rest[1]
		}
		scope = rest[0] + "/" + rest[1]
		rest = rest[2:]
	default:
		// /aggregated/subnetworks and anything else unprefixed.
		scope = "global"
	}
	if len(rest) > 0 {
		collection = rest[0]
	}
	if len(rest) > 1 {
		name = rest[1]
	}
	return scope, collection, name
}

// --- the generic verbs ----------------------------------------------------

func (s *Server) computeSelfLink(kind *computeKind, scope, name string) string {
	return s.computeLink(scope, kind.collection, name)
}

func (s *Server) insertCompute(w http.ResponseWriter, r *http.Request, kind *computeKind, scope string) string {
	var body map[string]any
	if err := readJSON(r, &body); err != nil {
		writeError(w, badRequest("%v", err))
		return "compute insert: bad body"
	}
	if body == nil {
		body = map[string]any{}
	}
	name := str(body["name"])
	if name == "" {
		writeError(w, badRequest("Required field 'name' not specified"))
		return "compute insert: no name"
	}
	selfLink := s.computeSelfLink(kind, scope, name)
	if s.store.Exists(selfLink) {
		writeError(w, alreadyExists(kind.collection+"/"+name, s.cfg.ProjectID))
		return "compute insert: exists"
	}

	body["kind"] = kind.kind
	body["name"] = name
	body["selfLink"] = selfLink
	body["id"] = numericID(selfLink)
	body["creationTimestamp"] = timestamp()
	if kind.fill != nil {
		kind.fill(s, scope, body)
	}
	// A network reference arrives in any of three spellings; it goes back
	// out as a full self link, which is what the provider stores and
	// compares against.
	if ref := str(body["network"]); ref != "" {
		body["network"] = s.linkFor(ref)
	}

	if aerr := s.putCompute(kind, selfLink, name, scope, body); aerr != nil {
		writeError(w, aerr)
		return "compute insert: " + aerr.Message
	}
	// A subnetwork belongs to its network's list, which the provider reads
	// off the PARENT on a later refresh -- the same parent/child mirror
	// that AKS node pools and VNet subnets need, and it goes through
	// core.Store.Update for the same reason: two creates in one apply
	// overlap, and a get-then-put loses one of them.
	s.mirrorIntoNetwork(kind, body, true)

	op := s.newOperation(scope, "insert", selfLink)
	writeJSON(w, http.StatusOK, op)
	return ""
}

func (s *Server) getCompute(w http.ResponseWriter, kind *computeKind, scope, name string) string {
	res, err := s.store.Get(s.computeSelfLink(kind, scope, name))
	if err != nil || res.Type != kind.resourceType {
		writeError(w, notFound("The resource 'projects/%s/%s/%s' was not found",
			s.cfg.ProjectID, kind.collection, name))
		return "compute get: not found"
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Body)
	return ""
}

func (s *Server) listCompute(w http.ResponseWriter, kind *computeKind, scope string) string {
	prefix := s.computeLink(scope, kind.collection, "")
	var items []json.RawMessage
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return r.Type == kind.resourceType && strings.HasPrefix(strings.ToLower(r.ID), strings.ToLower(prefix))
	}) {
		items = append(items, json.RawMessage(res.Body))
	}
	sort.Slice(items, func(i, j int) bool { return string(items[i]) < string(items[j]) })
	if items == nil {
		items = []json.RawMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind":     kind.kind + "List",
		"id":       "projects/" + s.cfg.ProjectID + "/" + kind.collection,
		"items":    items,
		"selfLink": strings.TrimSuffix(prefix, "/"),
	})
	return ""
}

// patchCompute applies an update mask.
//
// A PATCH names the fields it means in `updateMask`, and ONLY those may be
// touched: applying the whole body would silently clear everything the
// client left out, which is how a partial update turns into data loss that
// nothing reports.
func (s *Server) patchCompute(w http.ResponseWriter, r *http.Request, kind *computeKind, scope, name string) string {
	selfLink := s.computeSelfLink(kind, scope, name)
	if !s.store.Exists(selfLink) {
		writeError(w, notFound("The resource 'projects/%s/%s/%s' was not found",
			s.cfg.ProjectID, kind.collection, name))
		return "compute patch: not found"
	}
	var patch map[string]any
	if err := readJSON(r, &patch); err != nil {
		writeError(w, badRequest("%v", err))
		return "compute patch: bad body"
	}
	mask := maskFields(r.URL.Query().Get("updateMask"))

	err := s.store.Update(selfLink, func(doc map[string]any) error {
		for field, value := range patch {
			if !mask.allows(field) {
				continue
			}
			switch field {
			// Never patchable: they are the resource's identity.
			case "name", "id", "selfLink", "kind", "creationTimestamp":
				continue
			}
			doc[field] = value
		}
		return nil
	})
	if err != nil {
		writeError(w, internalError(err))
		return "compute patch: " + err.Error()
	}
	op := s.newOperation(scope, "patch", selfLink)
	writeJSON(w, http.StatusOK, op)
	return ""
}

func (s *Server) deleteCompute(w http.ResponseWriter, kind *computeKind, scope, name string) string {
	selfLink := s.computeSelfLink(kind, scope, name)
	res, err := s.store.Get(selfLink)
	if err != nil {
		writeError(w, notFound("The resource 'projects/%s/%s/%s' was not found",
			s.cfg.ProjectID, kind.collection, name))
		return "compute delete: not found"
	}
	var body map[string]any
	_ = json.Unmarshal(res.Body, &body)
	s.mirrorIntoNetwork(kind, body, false)
	_, _ = s.store.Delete(selfLink)

	op := s.newOperation(scope, "delete", selfLink)
	writeJSON(w, http.StatusOK, op)
	return ""
}

func (s *Server) putCompute(kind *computeKind, selfLink, name, scope string, body map[string]any) *apiError {
	raw, err := json.Marshal(body)
	if err != nil {
		return internalError(err)
	}
	parent := s.projectSelfLink()
	if kind.parent != nil {
		if p := kind.parent(s, body); p != "" {
			parent = p
		}
	}
	location := ""
	if strings.HasPrefix(scope, "regions/") {
		location = strings.TrimPrefix(scope, "regions/")
	}
	s.store.Put(&core.Resource{
		Cloud: "gcp", ID: selfLink, Type: kind.resourceType, Name: name,
		ParentID: parent, Location: location, Backing: "state", Body: raw,
	})
	return nil
}

// mirrorIntoNetwork keeps a network's `subnetworks` list in step with the
// subnetworks that exist.
//
// The provider reads that list off the network on a refresh, so without the
// mirror a subnetwork created seconds ago is invisible from its parent --
// the identical shape AKS node pools and Azure subnets needed. It goes
// through core.Store.Update because two subnetworks created in one apply
// overlap, and a read-change-write pair would lose one of them: measured on
// the Azure side, not theorised.
func (s *Server) mirrorIntoNetwork(kind *computeKind, body map[string]any, add bool) {
	if kind.collection != "subnetworks" {
		return
	}
	network := s.linkFor(str(body["network"]))
	if network == "" {
		return
	}
	self := str(body["selfLink"])
	_ = s.store.Update(network, func(doc map[string]any) error {
		have, _ := doc["subnetworks"].([]any)
		var kept []any
		for _, item := range have {
			if str(item) != self {
				kept = append(kept, item)
			}
		}
		if add {
			kept = append(kept, self)
		}
		if kept == nil {
			kept = []any{}
		}
		doc["subnetworks"] = kept
		return nil
	})
}

// linkFor turns any of the three spellings of a reference into the self
// link Nimbus uses as an id: a bare name, a `projects/...` relative name,
// or a full URL that may point at a different host than this one.
func (s *Server) linkFor(ref string) string {
	if ref == "" {
		return ""
	}
	rel := relativeName(ref)
	if strings.HasPrefix(rel, "projects/") {
		return s.base() + "/compute/v1/" + rel
	}
	// A bare name can only be a network: it is the only reference the
	// provider ever sends unqualified.
	return s.globalLink("networks", ref)
}

// --- regions, zones and the project ---------------------------------------

// regions is the list a client may choose from. Short on purpose: these are
// names, nothing is deployed to them, and a client that asks for one that
// is not here gets a clear 404 instead of silently working.
var regions = []string{
	"us-central1", "us-east1", "us-west1",
	"europe-west1", "europe-west4", "asia-east1",
}

func zonesIn(region string) []string {
	return []string{region + "-a", region + "-b", region + "-c"}
}

func (s *Server) computeProject(w http.ResponseWriter, project string) string {
	// The provider arrives here with either the project id or its NUMBER,
	// because it reads the number off a bucket and then looks it up.
	if project != s.cfg.ProjectID && project != DefaultProjectNumber {
		writeError(w, notFound("The resource 'projects/%s' was not found", project))
		return "no such project " + project
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind":              "compute#project",
		"id":                DefaultProjectNumber,
		"name":              s.cfg.ProjectID,
		"creationTimestamp": timestamp(),
		"selfLink":          s.computeProjectLink(),
		"defaultServiceAccount": DefaultProjectNumber +
			"-compute@developer.gserviceaccount.com",
		"xpnProjectStatus": "UNSPECIFIED_XPN_PROJECT_STATUS",
	})
	return ""
}

func (s *Server) computeScopeList(w http.ResponseWriter, rest []string) string {
	switch rest[0] {
	case "regions":
		var items []any
		for _, name := range regions {
			items = append(items, s.regionDoc(name))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"kind": "compute#regionList", "id": "projects/" + s.cfg.ProjectID + "/regions",
			"items": items, "selfLink": s.base() + "/compute/v1/projects/" + s.cfg.ProjectID + "/regions",
		})
		return ""
	case "zones":
		var items []any
		for _, region := range regions {
			for _, zone := range zonesIn(region) {
				items = append(items, s.zoneDoc(zone, region))
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"kind": "compute#zoneList", "id": "projects/" + s.cfg.ProjectID + "/zones",
			"items": items, "selfLink": s.base() + "/compute/v1/projects/" + s.cfg.ProjectID + "/zones",
		})
		return ""
	}
	writeError(w, notFound("Nimbus does not implement %q yet", strings.Join(rest, "/")))
	return "compute " + strings.Join(rest, "/")
}

func (s *Server) regionDoc(name string) map[string]any {
	link := s.base() + "/compute/v1/projects/" + s.cfg.ProjectID + "/regions/" + name
	var zoneLinks []any
	for _, z := range zonesIn(name) {
		zoneLinks = append(zoneLinks, s.base()+"/compute/v1/projects/"+s.cfg.ProjectID+"/zones/"+z)
	}
	return map[string]any{
		"kind": "compute#region", "id": numericID(link), "name": name,
		"description": name, "status": "UP", "zones": zoneLinks, "selfLink": link,
		"creationTimestamp": timestamp(),
	}
}

func (s *Server) zoneDoc(name, region string) map[string]any {
	link := s.base() + "/compute/v1/projects/" + s.cfg.ProjectID + "/zones/" + name
	return map[string]any{
		"kind": "compute#zone", "id": numericID(link), "name": name,
		"description": name, "status": "UP", "selfLink": link,
		"region":            s.base() + "/compute/v1/projects/" + s.cfg.ProjectID + "/regions/" + region,
		"creationTimestamp": timestamp(),
	}
}

func (s *Server) describeRegion(w http.ResponseWriter, name string) string {
	for _, r := range regions {
		if r == name {
			writeJSON(w, http.StatusOK, s.regionDoc(name))
			return ""
		}
	}
	writeError(w, notFound("The resource 'projects/%s/regions/%s' was not found", s.cfg.ProjectID, name))
	return "no such region " + name
}

func (s *Server) describeZone(w http.ResponseWriter, name string) string {
	for _, r := range regions {
		for _, z := range zonesIn(r) {
			if z == name {
				writeJSON(w, http.StatusOK, s.zoneDoc(name, r))
				return ""
			}
		}
	}
	writeError(w, notFound("The resource 'projects/%s/zones/%s' was not found", s.cfg.ProjectID, name))
	return "no such zone " + name
}

// --- small helpers --------------------------------------------------------

func str(v any) string {
	s, _ := v.(string)
	return s
}

func orDefault(have, fallback any) any {
	if have == nil {
		return fallback
	}
	return have
}

// firstAddress is a subnetwork's gateway: the first usable address in its
// range. Derived from the CIDR rather than stored, because the provider
// reads it back and compares.
func firstAddress(cidr string) string {
	ip, _, ok := strings.Cut(cidr, "/")
	if !ok {
		return ""
	}
	octets := strings.Split(ip, ".")
	if len(octets) != 4 {
		return ""
	}
	octets[3] = "1"
	return strings.Join(octets, ".")
}

// updateMask is the set of fields a PATCH may touch.
type updateMask struct {
	fields map[string]bool
	all    bool
}

// maskFields parses `updateMask=a,b.c`. An ABSENT mask means the whole body
// applies, which is what the API does -- the mask narrows, it does not
// enable.
func maskFields(raw string) updateMask {
	if strings.TrimSpace(raw) == "" {
		return updateMask{all: true}
	}
	m := updateMask{fields: map[string]bool{}}
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		// A mask may name a nested path; the top-level field is what
		// decides whether this body key is applied at all.
		top, _, _ := strings.Cut(field, ".")
		m.fields[top] = true
	}
	return m
}

func (m updateMask) allows(field string) bool {
	return m.all || m.fields[field]
}
