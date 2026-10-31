package azure

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Asere231/nimbus/internal/core"
)

// The spec table covers types nobody wrote a hook for. These tests drive it
// through the real server, because what matters is what a client reads back.

// newTestServerWithGroup returns a Server and a resource group that already
// exists, since every resource below needs a parent.
func newTestServerWithGroup(t *testing.T) (*Server, string) {
	t.Helper()
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{PublicURL: "https://localhost:4701"}, store, bus)
	const rg = "rg-spec"
	putResource(t, s, "/subscriptions/"+DefaultSubscriptionID+"/resourceGroups/"+rg, `{"location":"eastus"}`)
	return s, rg
}

func putResource(t *testing.T, s *Server, id, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, id+"?api-version=2024-01-01", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("PUT %s: %d %s", id, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("PUT %s: %v", id, err)
	}
	return out
}

func getResource(t *testing.T, s *Server, id string) (map[string]any, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, id+"?api-version=2024-01-01", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET %s: %d %s", id, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET %s: %v", id, err)
	}
	return out, rec.Body.Bytes()
}

func natGatewayID(rg string) string {
	return "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg +
		"/providers/Microsoft.Network/natGateways/nat1"
}

func TestSpecFieldsAreFilledOnCreate(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	id := natGatewayID(rg)

	got := putResource(t, s, id, `{"location":"eastus","properties":{}}`)
	props, _ := got["properties"].(map[string]any)
	if props == nil {
		t.Fatal("no properties in the response")
	}
	// resourceGuid is marked readOnly in natGateway.json: the server owns it.
	if guid, _ := props["resourceGuid"].(string); guid == "" {
		t.Error("properties.resourceGuid was not filled from the spec table")
	}
	if props["provisioningState"] != "Succeeded" {
		t.Errorf("provisioningState = %v", props["provisioningState"])
	}
	if _, ok := got["etag"].(string); !ok {
		t.Error("etag was not filled")
	}
}

// The lesson from Content-MD5 and the AKS kubeconfig, applied to every type
// the table covers: a value that differs between reads is a permanent diff.
func TestSpecFieldsAreStableAcrossWrites(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	id := natGatewayID(rg)
	body := `{"location":"eastus","properties":{}}`

	putResource(t, s, id, body)
	_, first := getResource(t, s, id)
	putResource(t, s, id, body)
	_, second := getResource(t, s, id)

	if string(first) != string(second) {
		t.Errorf("a repeated identical PUT changed the resource\nfirst:  %s\nsecond: %s", first, second)
	}
}

// A client's own value always wins: the table fills gaps, it does not correct
// anyone.
func TestSpecFieldsNeverOverwriteTheClient(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	id := natGatewayID(rg)

	got := putResource(t, s, id, `{"location":"eastus","properties":{"resourceGuid":"mine","idleTimeoutInMinutes":17}}`)
	props, _ := got["properties"].(map[string]any)
	if props["resourceGuid"] != "mine" {
		t.Errorf("resourceGuid = %v, want the value the client sent", props["resourceGuid"])
	}
	if props["idleTimeoutInMinutes"] != float64(17) {
		t.Errorf("idleTimeoutInMinutes = %v, want 17", props["idleTimeoutInMinutes"])
	}
}

// Spec defaults are applied, but only where the client already established the
// surrounding object: inventing a whole nested block nobody asked for would be
// worse than leaving it out, because the client then sees configuration it
// never wrote.
func TestSpecDefaultsDoNotInventNestedObjects(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	id := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg +
		"/providers/Microsoft.App/containerApps/app1"

	got := putResource(t, s, id, `{"location":"eastus","properties":{}}`)
	props, _ := got["properties"].(map[string]any)
	if _, invented := props["configuration"]; invented {
		t.Error("properties.configuration was invented from defaults the client never asked for")
	}

	// ...but when the client DOES send the object, its defaults get filled.
	got = putResource(t, s, id, `{"location":"eastus","properties":{"configuration":{"ingress":{}}}}`)
	props, _ = got["properties"].(map[string]any)
	cfg, _ := props["configuration"].(map[string]any)
	if cfg["activeRevisionsMode"] != "Single" {
		t.Errorf("activeRevisionsMode = %v, want the spec default Single", cfg["activeRevisionsMode"])
	}
	ingress, _ := cfg["ingress"].(map[string]any)
	if ingress["transport"] != "auto" {
		t.Errorf("ingress.transport = %v, want the spec default auto", ingress["transport"])
	}
}

// A hand-written hook is the authority for its type. The table runs first and
// must not get in its way.
func TestHooksWinOverTheSpecTable(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	id := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg +
		"/providers/Microsoft.Network/virtualNetworks/vnet1"

	got := putResource(t, s, id, `{"location":"eastus","properties":{"addressSpace":{"addressPrefixes":["10.0.0.0/16"]}}}`)
	props, _ := got["properties"].(map[string]any)
	// The VNet hook guarantees subnets exists; nothing in the table removes it.
	if _, ok := props["subnets"]; !ok {
		t.Error("the VNet hook's subnets array is missing")
	}
}

// The visualizer's three tiers have to be distinguishable, because the whole
// point of M5 is watching the accepted-only count fall.
func TestBackingTiers(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	base := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg + "/providers/"

	cases := []struct {
		id   string
		body string
		want string
	}{
		{base + "Microsoft.Network/natGateways/nat2", `{"location":"eastus","properties":{}}`, "modelled"},
		{base + "Microsoft.Network/virtualNetworks/vnet2",
			`{"location":"eastus","properties":{"addressSpace":{"addressPrefixes":["10.0.0.0/16"]}}}`, "modelled"},
		{base + "Nimbus.Unknown/things/thing1", `{"location":"eastus","properties":{}}`, "state"},
	}
	for _, c := range cases {
		putResource(t, s, c.id, c.body)
		res, err := s.store.Get(c.id)
		if err != nil {
			t.Fatalf("%s: %v", c.id, err)
		}
		if res.Backing != c.want {
			t.Errorf("%s: backing = %q, want %q", c.id, res.Backing, c.want)
		}
	}
}

// Every type in the generated table must survive the full client lifecycle.
// This is the matrix the milestone asks for: 50+ types through PUT, GET, list
// and DELETE, with the spec's read-only fields present afterwards.
func TestEverySpecTypeRoundTrips(t *testing.T) {
	if len(specTypes) < 50 {
		t.Fatalf("the spec table has only %d types; the milestone calls for 50+", len(specTypes))
	}
	for typ := range specTypes {
		t.Run(typ, func(t *testing.T) {
			s, rg := newTestServerWithGroup(t)
			id := sampleID(rg, typ)
			if id == "" {
				t.Skipf("%s is not a resource-group-scoped type", typ)
			}
			// A child type needs its parent: PUT every ancestor first. The
			// address space is there for the one parent that validates its
			// body (a VNet); every other type ignores the extra property.
			for _, parent := range ancestors(id) {
				putResource(t, s, parent,
					`{"location":"eastus","properties":{"addressSpace":{"addressPrefixes":["10.0.0.0/16"]}}}`)
			}
			created := putResource(t, s, id, `{"location":"eastus","properties":{}}`)
			if created["id"] == nil || created["name"] == nil {
				t.Fatalf("create returned no id/name: %v", created)
			}

			read, _ := getResource(t, s, id)
			props, _ := read["properties"].(map[string]any)
			if props["provisioningState"] != "Succeeded" {
				t.Errorf("provisioningState = %v", props["provisioningState"])
			}

			// List the collection it belongs to.
			collection := id[:strings.LastIndex(id, "/")]
			req := httptest.NewRequest(http.MethodGet, collection+"?api-version=2024-01-01", nil)
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
			}
			var list struct {
				Value []map[string]any `json:"value"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(list.Value) != 1 {
				t.Errorf("list returned %d resources, want 1", len(list.Value))
			}

			req = httptest.NewRequest(http.MethodDelete, id+"?api-version=2024-01-01", nil)
			rec = httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != 200 && rec.Code != 202 && rec.Code != 204 {
				t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
			}
			if s.store.Exists(id) {
				t.Error("the resource still exists after a delete")
			}
		})
	}
}

// ancestors lists the resource IDs above this one, outermost first. A route
// lives under a route table, a queue under a namespace.
func ancestors(id string) []string {
	i := strings.Index(id, "/providers/")
	if i < 0 {
		return nil
	}
	head, rest := id[:i+len("/providers/")], id[i+len("/providers/"):]
	segs := strings.Split(rest, "/")
	var out []string
	// segs is {namespace, type, name, type, name, ...}: each extra pair after
	// the first is one level deeper.
	for n := 3; n+2 <= len(segs); n += 2 {
		out = append(out, head+strings.Join(segs[:n], "/"))
	}
	return out
}

// sampleID builds a plausible ARM ID for a type in the table. Child types get
// a parent segment named after the parent type.
func sampleID(rg, typ string) string {
	parts := strings.Split(typ, "/")
	if len(parts) < 2 {
		return ""
	}
	id := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg + "/providers/" + parts[0]
	for i, seg := range parts[1:] {
		id += "/" + seg + "/n" + string(rune('a'+i))
	}
	return id
}

// A type nobody gave a badge renders in the visualizer as a bare namespace,
// which is the one part of adding types that is easy to forget. The TYPES map
// lives in JavaScript, so this reads the file rather than importing anything.
func TestEverySpecTypeHasABadge(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "ui", "static", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	app := string(raw)
	for typ := range specTypes {
		if !strings.Contains(app, `"`+typ+`"`) {
			t.Errorf("%s has no badge in internal/ui/static/app.js (TYPES)", typ)
		}
	}
}

// A fresh Nimbus must already know about its subscription. Nothing had asked
// for one before, so the store was empty, `az login` saw no subscriptions and
// fell back to a tenant-level account -- after which az uses the TENANT id as
// the subscription id and every resource lands under the wrong one. Terraform
// never noticed, because it is always configured with an explicit
// subscription_id.
func TestSubscriptionExistsBeforeAnyRequest(t *testing.T) {
	bus := core.NewBus(10)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{PublicURL: "https://localhost:4701"}, store, bus)

	req := httptest.NewRequest(http.MethodGet, "/subscriptions?api-version=2022-12-01", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list subscriptions: %d", rec.Code)
	}
	var list struct {
		Value []struct {
			SubscriptionID string `json:"subscriptionId"`
			DisplayName    string `json:"displayName"`
		} `json:"value"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Value) != 1 {
		t.Fatalf("a fresh server lists %d subscriptions, want 1", len(list.Value))
	}
	if list.Value[0].SubscriptionID != DefaultSubscriptionID {
		t.Errorf("subscriptionId = %s, want %s (not the tenant id)",
			list.Value[0].SubscriptionID, DefaultSubscriptionID)
	}
}
