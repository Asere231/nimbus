package azure

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/azure/storage"
	"github.com/Asere231/nimbus/internal/core"
)

// These tests replay the HTTP calls the Terraform azurerm provider (via
// hashicorp/go-azure-sdk) makes, in the same order and shapes.

const sub = "/subscriptions/" + DefaultSubscriptionID

func newTestServer(t *testing.T) (*httptest.Server, *core.Store) {
	t.Helper()
	srv, store, _ := newTestServerWithBus(t)
	return srv, store
}

func newTestServerWithBus(t *testing.T) (*httptest.Server, *core.Store, *core.Bus) {
	t.Helper()
	bus := core.NewBus(100)
	store, _ := core.NewStore(bus, "")
	var az *Server
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { az.ServeHTTP(w, r) }))
	az = New(Config{PublicURL: srv.URL}, store, bus)
	testServers[srv] = az
	t.Cleanup(func() { delete(testServers, srv); srv.Close() })
	return srv, store, bus
}

// serverOf returns the azure Server behind a test server, for assertions that
// need to look at the data-plane store directly.
func serverOf(t *testing.T, srv *httptest.Server) *Server {
	t.Helper()
	s := testServers[srv]
	if s == nil {
		t.Fatal("no Server registered for this test server")
	}
	return s
}

var testServers = map[*httptest.Server]*Server{}

func do(t *testing.T, srv *httptest.Server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path+"?api-version=2023-09-01", rdr)
	req.Header.Set("Content-Type", "application/json")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	var out map[string]any
	data, _ := io.ReadAll(res.Body)
	_ = json.Unmarshal(data, &out)
	if res.StatusCode < 300 && len(data) > 0 && !strings.Contains(res.Header.Get("Content-Type"), "application/json") {
		t.Errorf("%s %s: Content-Type %q, the SDK's poller needs application/json", method, path, res.Header.Get("Content-Type"))
	}
	return res.StatusCode, out
}

func TestMetadataIsSingleObjectWithRequiredFields(t *testing.T) {
	srv, _ := newTestServer(t)
	res, err := srv.Client().Get(srv.URL + "/metadata/endpoints?api-version=2022-09-01")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any // go-azure-sdk decodes into a single object, not an array
	if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
		t.Fatalf("metadata must be a JSON object: %v", err)
	}
	for _, k := range []string{"name", "resourceManager", "microsoftGraphResourceId"} {
		if m[k] == "" || m[k] == nil {
			t.Errorf("metadata is missing %q (go-azure-sdk rejects it)", k)
		}
	}
	auth := m["authentication"].(map[string]any)
	if auth["loginEndpoint"] != srv.URL {
		t.Errorf("loginEndpoint = %v, want %s", auth["loginEndpoint"], srv.URL)
	}
}

func TestClientSecretTokenHasParsableClaims(t *testing.T) {
	srv, _ := newTestServer(t)
	form := url.Values{"client_id": {"abc"}, "client_secret": {"x"}, "grant_type": {"client_credentials"}, "scope": {srv.URL + "/.default"}}
	res, err := srv.Client().PostForm(srv.URL+"/my-tenant/oauth2/v2.0/token", form)
	if err != nil {
		t.Fatal(err)
	}
	var tok map[string]any
	_ = json.NewDecoder(res.Body).Decode(&tok)
	parts := strings.Split(tok["access_token"].(string), ".")
	if len(parts) != 3 {
		t.Fatalf("token must have 3 parts, got %d", len(parts))
	}
	claims := map[string]any{}
	raw, err := decodeSeg(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(raw, &claims)
	if claims["tid"] != "my-tenant" || claims["appid"] != "abc" || claims["oid"] == nil {
		t.Errorf("unexpected claims: %v", claims)
	}
	if claims["aud"] != srv.URL+"/" {
		t.Errorf("aud = %v", claims["aud"])
	}
}

func TestResourceGroupLifecycle(t *testing.T) {
	srv, store := newTestServer(t)
	rg := sub + "/resourceGroups/rg-demo"

	if code, _ := do(t, srv, "HEAD", rg, nil); code != 404 {
		t.Fatalf("HEAD absent rg = %d, want 404", code)
	}
	code, body := do(t, srv, "PUT", rg, map[string]any{"location": "East US", "tags": map[string]any{"env": "dev"}})
	if code != 201 {
		t.Fatalf("PUT rg = %d", code)
	}
	if body["location"] != "eastus" || body["id"] != rg || body["type"] != "Microsoft.Resources/resourceGroups" {
		t.Errorf("rg body: %v", body)
	}
	if code, _ := do(t, srv, "PUT", rg, map[string]any{"location": "eastus"}); code != 200 {
		t.Errorf("second PUT should be 200, got %d", code)
	}
	if code, _ := do(t, srv, "HEAD", rg, nil); code != 204 {
		t.Errorf("HEAD = %d, want 204", code)
	}
	// Casing of path segments must not matter.
	if code, _ := do(t, srv, "GET", strings.Replace(rg, "resourceGroups", "resourcegroups", 1), nil); code != 200 {
		t.Errorf("GET with lowercase segment = %d", code)
	}
	_, list := do(t, srv, "GET", sub+"/resourcegroups", nil)
	if n := len(list["value"].([]any)); n != 1 {
		t.Errorf("list rgs = %d", n)
	}

	vnet := rg + "/providers/Microsoft.Network/virtualNetworks/vnet-demo"
	do(t, srv, "PUT", vnet, map[string]any{"location": "eastus", "properties": map[string]any{"addressSpace": map[string]any{"addressPrefixes": []string{"10.0.0.0/16"}}}})

	_, res := do(t, srv, "GET", rg+"/resources", nil)
	if n := len(res["value"].([]any)); n != 1 {
		t.Errorf("rg resources = %d, want 1", n)
	}

	if code, _ := do(t, srv, "DELETE", rg, nil); code != 202 {
		t.Fatalf("DELETE rg = %d", code)
	}
	if store.Exists(vnet) {
		t.Error("deleting a resource group must delete what is in it")
	}
	if code, _ := do(t, srv, "GET", rg, nil); code != 404 {
		t.Errorf("GET deleted rg = %d", code)
	}
	if code, _ := do(t, srv, "DELETE", rg, nil); code != 204 {
		t.Errorf("DELETE absent = %d, want 204", code)
	}
}

func TestVNetAndSubnets(t *testing.T) {
	srv, store := newTestServer(t)
	rg := sub + "/resourceGroups/rg-net"
	vnet := rg + "/providers/Microsoft.Network/virtualNetworks/vnet1"
	space := map[string]any{"addressSpace": map[string]any{"addressPrefixes": []string{"10.0.0.0/16"}}}

	if code, body := do(t, srv, "PUT", vnet, map[string]any{"location": "eastus", "properties": space}); code != 404 || body["error"].(map[string]any)["code"] != "ResourceGroupNotFound" {
		t.Fatalf("vnet without rg: %d %v", code, body)
	}
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})

	code, body := do(t, srv, "PUT", vnet, map[string]any{"location": "eastus", "properties": space})
	if code != 201 {
		t.Fatalf("PUT vnet = %d %v", code, body)
	}
	props := body["properties"].(map[string]any)
	if props["provisioningState"] != "Succeeded" || props["resourceGuid"] == nil || body["etag"] == nil {
		t.Errorf("vnet is missing server fields: %v", body)
	}

	// azurerm_subnet: separate resource.
	sn := vnet + "/subnets/app"
	code, body = do(t, srv, "PUT", sn, map[string]any{"properties": map[string]any{"addressPrefixes": []string{"10.0.1.0/24"}}})
	if code != 201 || body["type"] != typeSubnet {
		t.Fatalf("PUT subnet = %d %v", code, body)
	}
	_, v := do(t, srv, "GET", vnet, nil)
	if n := len(v["properties"].(map[string]any)["subnets"].([]any)); n != 1 {
		t.Errorf("vnet should embed 1 subnet, has %d", n)
	}

	// Updating the VNet without a subnets field must keep the subnet.
	do(t, srv, "PUT", vnet, map[string]any{"location": "eastus", "tags": map[string]any{"a": "b"}, "properties": space})
	if !store.Exists(sn) {
		t.Error("vnet update without subnets deleted the subnet")
	}

	_, l := do(t, srv, "GET", vnet+"/subnets", nil)
	if n := len(l["value"].([]any)); n != 1 {
		t.Errorf("list subnets = %d", n)
	}

	do(t, srv, "DELETE", sn, nil)
	_, v = do(t, srv, "GET", vnet, nil)
	if n := len(v["properties"].(map[string]any)["subnets"].([]any)); n != 0 {
		t.Errorf("deleted subnet still embedded in vnet (%d)", n)
	}

	// Inline subnets become child resources too.
	do(t, srv, "PUT", vnet, map[string]any{"location": "eastus", "properties": map[string]any{
		"addressSpace": space["addressSpace"],
		"subnets":      []any{map[string]any{"name": "inline", "properties": map[string]any{"addressPrefix": "10.0.9.0/24"}}},
	}})
	if !store.Exists(vnet + "/subnets/inline") {
		t.Error("inline subnet was not mirrored as a child resource")
	}

	// Subscription-wide list of a type.
	_, all := do(t, srv, "GET", sub+"/providers/Microsoft.Network/virtualNetworks", nil)
	if n := len(all["value"].([]any)); n != 1 {
		t.Errorf("subscription-wide vnet list = %d", n)
	}

	// Validation.
	if code, _ := do(t, srv, "PUT", rg+"/providers/Microsoft.Network/virtualNetworks/bad", map[string]any{"location": "eastus"}); code != 400 {
		t.Errorf("vnet without address space = %d, want 400", code)
	}
}

func TestUnknownTypesAreAcceptedAndPatchMerges(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-any"
	do(t, srv, "PUT", rg, map[string]any{"location": "westus2"})
	kv := rg + "/providers/Microsoft.KeyVault/vaults/kv1"
	code, body := do(t, srv, "PUT", kv, map[string]any{"location": "westus2", "properties": map[string]any{"tenantId": DefaultTenantID, "sku": map[string]any{"name": "standard"}}})
	if code != 201 || body["properties"].(map[string]any)["provisioningState"] != "Succeeded" {
		t.Fatalf("generic PUT = %d %v", code, body)
	}
	code, body = do(t, srv, "PATCH", kv, map[string]any{"tags": map[string]any{"owner": "bryan"}})
	if code != 200 || body["tags"].(map[string]any)["owner"] != "bryan" || body["properties"].(map[string]any)["tenantId"] == nil {
		t.Errorf("PATCH should merge: %d %v", code, body)
	}
	if code, _ := do(t, srv, "POST", kv+"/someAction", nil); code != 200 {
		t.Errorf("unknown action = %d", code)
	}
	if code, body := do(t, srv, "GET", rg+"/providers/Microsoft.KeyVault/vaults/missing", nil); code != 404 || body["error"].(map[string]any)["code"] != "ResourceNotFound" {
		t.Errorf("missing resource = %d %v", code, body)
	}
}

func TestProvidersAndSubscription(t *testing.T) {
	srv, _ := newTestServer(t)
	_, p := do(t, srv, "GET", sub+"/providers", nil)
	for _, raw := range p["value"].([]any) {
		if raw.(map[string]any)["registrationState"] != "Registered" {
			t.Fatalf("every provider must report Registered: %v", raw)
		}
	}
	if code, body := do(t, srv, "POST", sub+"/providers/Microsoft.Network/register", nil); code != 200 || body["registrationState"] != "Registered" {
		t.Errorf("register = %d %v", code, body)
	}
	_, s := do(t, srv, "GET", sub, nil)
	if s["subscriptionId"] != DefaultSubscriptionID || s["state"] != "Enabled" {
		t.Errorf("subscription = %v", s)
	}
}

// raw performs a request and returns the response so headers can be asserted.
func raw(t *testing.T, srv *httptest.Server, method, path, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, srv.URL+path+"?api-version=2023-09-01", rdr)
	req.Header.Set("Content-Type", "application/json")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

// Writes advertise an async operation that is already Succeeded, with
// Retry-After: 1, so the SDK polls once after a second instead of waiting 10.
func TestWritesAdvertiseFastAsyncOperation(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-lro"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	res := raw(t, srv, "PUT", rg+"/providers/Microsoft.Network/routeTables/rt", `{"location":"eastus"}`)
	op := res.Header.Get("Azure-AsyncOperation")
	if op == "" || res.Header.Get("Retry-After") != "1" {
		t.Fatalf("missing async headers: %v", res.Header)
	}
	u, _ := url.Parse(op)
	code, body := do(t, srv, "GET", u.Path, nil)
	if code != 200 || body["status"] != "Succeeded" {
		t.Errorf("operation status = %d %v", code, body)
	}
}

// A resource group PUT is synchronous, as it is in real ARM: no async header.
//
// Do not "optimise" this. Measured against azurerm 4.81.0: adding
// Azure-AsyncOperation + Retry-After to resource-group writes changes nothing,
// because azurerm_resource_group does not use the SDK's async poller. It runs
// its own client-side wait that GETs the resource group four times across 20 s
// and requires the target state several times in a row; every response already
// said Succeeded. The 20 s is a constant inside the provider, so it cannot be
// shortened from this side — only by the provider, or by not using it.
//
// The same holds for DELETE: each one costs a flat 10 s because azurerm waits,
// then GETs until the resource 404s, ignoring the operation URL we advertise.
// Setting Content-Type on the empty 202 makes no difference either.
func TestResourceGroupWritesAreSynchronous(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-sync"

	res := raw(t, srv, "PUT", rg, `{"location":"eastus"}`)
	if res.StatusCode != 201 {
		t.Fatalf("PUT rg = %d", res.StatusCode)
	}
	if op := res.Header.Get("Azure-AsyncOperation"); op != "" {
		t.Errorf("resource-group PUT advertised an async operation (%s); ARM answers these synchronously", op)
	}

	// A resource under it, by contrast, does advertise one.
	res = raw(t, srv, "PUT", rg+"/providers/Microsoft.Network/routeTables/rt", `{"location":"eastus"}`)
	if res.Header.Get("Azure-AsyncOperation") == "" {
		t.Error("resource PUT must advertise an async operation")
	}
}

// --- hardening -------------------------------------------------------------

func TestPatchOnMissingResourceIs404(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-patch"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})

	code, body := do(t, srv, "PATCH", rg+"/providers/Microsoft.Network/routeTables/ghost", map[string]any{"tags": map[string]any{"a": "b"}})
	if code != 404 {
		t.Errorf("PATCH missing resource = %d, want 404", code)
	}
	if got := errCode(body); got != "ResourceNotFound" {
		t.Errorf("error code = %q, want ResourceNotFound", got)
	}

	// A PATCH on a missing resource group names the group, not the resource.
	code, body = do(t, srv, "PATCH", sub+"/resourceGroups/rg-ghost", map[string]any{"tags": map[string]any{}})
	if code != 404 || errCode(body) != "ResourceGroupNotFound" {
		t.Errorf("PATCH missing rg = %d %v, want 404 ResourceGroupNotFound", code, body)
	}
}

func errCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	if e == nil {
		return ""
	}
	s, _ := e["code"].(string)
	return s
}

func TestPutWithInvalidJSONIs400(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-badjson"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	rt := rg + "/providers/Microsoft.Network/routeTables/rt"

	for _, bad := range []string{`{"location":`, `not json at all`, `[1,2,3]`, `"a string"`} {
		res := raw(t, srv, "PUT", rt, bad)
		if res.StatusCode != 400 {
			t.Errorf("PUT %q = %d, want 400", bad, res.StatusCode)
		}
		var body map[string]any
		data, _ := io.ReadAll(res.Body)
		_ = json.Unmarshal(data, &body)
		if got := errCode(body); got != "InvalidRequestContent" {
			t.Errorf("PUT %q: error code %q, want InvalidRequestContent", bad, got)
		}
	}

	// An empty body is legal: some clients PUT nothing for a resource whose
	// fields are all defaults.
	if res := raw(t, srv, "PUT", rg+"/providers/Microsoft.Network/routeTables/rt2", ""); res.StatusCode != 201 {
		t.Errorf("PUT with empty body = %d, want 201", res.StatusCode)
	}
}

// Deleting a resource group deletes everything in it, and the visualizer must
// see a delete event for every one of them, leaves before parents, or the
// graph keeps orphaned nodes.
func TestDeletingResourceGroupCascadesAndPublishesEvents(t *testing.T) {
	srv, store, bus := newTestServerWithBus(t)
	rg := sub + "/resourceGroups/rg-cascade"
	vnet := rg + "/providers/Microsoft.Network/virtualNetworks/vnet"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	do(t, srv, "PUT", vnet, map[string]any{"location": "eastus", "properties": map[string]any{
		"addressSpace": map[string]any{"addressPrefixes": []string{"10.0.0.0/16"}},
		"subnets":      []any{map[string]any{"name": "a", "properties": map[string]any{"addressPrefix": "10.0.1.0/24"}}},
	}})
	do(t, srv, "PUT", rg+"/providers/Microsoft.Network/networkSecurityGroups/nsg", map[string]any{"location": "eastus"})

	ch, _, cancel := bus.Subscribe()
	defer cancel()

	if code, _ := do(t, srv, "DELETE", rg, nil); code != 202 {
		t.Fatalf("DELETE rg = %d", code)
	}

	for _, id := range []string{rg, vnet, vnet + "/subnets/a", rg + "/providers/Microsoft.Network/networkSecurityGroups/nsg"} {
		if store.Exists(id) {
			t.Errorf("%s survived the cascade", id)
		}
	}

	var deleted []string
	for len(ch) > 0 {
		e := <-ch
		if e.Kind == "resource" && e.Action == "delete" {
			deleted = append(deleted, e.ResourceID)
		}
	}
	if len(deleted) != 4 {
		t.Errorf("got %d delete events, want 4: %v", len(deleted), deleted)
	}
	// Leaves first: an ancestor must never be announced before its descendant,
	// or the visualizer removes a parent node while its children still hang off
	// it.
	for i, id := range deleted {
		for _, earlier := range deleted[:i] {
			if strings.HasPrefix(strings.ToLower(id), strings.ToLower(earlier)+"/") {
				t.Errorf("ancestor %s was announced before its descendant %s", earlier, id)
			}
		}
	}
}

// A resource's name is fixed when it is created. ARM resolves IDs
// case-insensitively but keeps answering with the casing used at creation, so a
// client that writes with different casing must not change what later GETs
// return -- Terraform compares the name it has in state against what we send.
func TestResourceIdentityKeepsItsOriginalCasing(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-Casing"
	code, body := do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	if code != 201 || body["name"] != "rg-Casing" {
		t.Fatalf("create = %d %v", code, body)
	}

	// Re-PUT addressing the same group in lower case.
	_, body = do(t, srv, "PUT", sub+"/resourcegroups/rg-casing", map[string]any{"location": "eastus"})
	if body["name"] != "rg-Casing" || body["id"] != rg {
		t.Errorf("a re-PUT with different casing renamed the group: name=%v id=%v", body["name"], body["id"])
	}
	// And a later read still reports the original casing.
	_, body = do(t, srv, "GET", sub+"/resourcegroups/rg-casing", nil)
	if body["name"] != "rg-Casing" || body["id"] != rg {
		t.Errorf("GET after the re-PUT reports %v / %v", body["name"], body["id"])
	}
}

// Every verb must resolve a differently-cased ID to the same resource.
func TestEveryHandlerResolvesIDsCaseInsensitively(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-Case"
	vnet := rg + "/providers/Microsoft.Network/virtualNetworks/VNet1"
	space := map[string]any{"addressSpace": map[string]any{"addressPrefixes": []string{"10.0.0.0/16"}}}
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	do(t, srv, "PUT", vnet, map[string]any{"location": "eastus", "properties": space})
	do(t, srv, "PUT", vnet+"/subnets/App", map[string]any{"properties": map[string]any{"addressPrefix": "10.0.1.0/24"}})

	// Mangle the casing of every segment a client might differ on.
	lower := strings.ToLower(vnet)

	if code, _ := do(t, srv, "GET", lower, nil); code != 200 {
		t.Errorf("GET lowercased = %d", code)
	}
	if code, _ := do(t, srv, "HEAD", lower, nil); code != 204 {
		t.Errorf("HEAD lowercased = %d, want 204", code)
	}
	if code, _ := do(t, srv, "PATCH", lower, map[string]any{"tags": map[string]any{"k": "v"}}); code != 200 {
		t.Errorf("PATCH lowercased = %d", code)
	}
	if code, l := do(t, srv, "GET", lower+"/subnets", nil); code != 200 || len(l["value"].([]any)) != 1 {
		t.Errorf("list subnets lowercased = %d %v", code, l)
	}
	if code, _ := do(t, srv, "GET", strings.ToLower(rg)+"/resources", nil); code != 200 {
		t.Errorf("rg resources lowercased = %d", code)
	}
	// An action on a lowercased resource path must find the resource.
	if code, body := do(t, srv, "POST", lower+"/someAction", nil); code != 200 {
		t.Errorf("action on lowercased resource = %d %v", code, body)
	}
	// Uppercased subscription segment too.
	if code, _ := do(t, srv, "GET", strings.Replace(lower, "/subscriptions/", "/SUBSCRIPTIONS/", 1), nil); code != 200 {
		t.Errorf("GET with uppercased /SUBSCRIPTIONS/ = %d", code)
	}
	if code, _ := do(t, srv, "DELETE", lower+"/subnets/app", nil); code != 202 {
		t.Errorf("DELETE lowercased subnet = %d, want 202", code)
	}
	if code, _ := do(t, srv, "DELETE", lower, nil); code != 202 {
		t.Errorf("DELETE lowercased vnet = %d, want 202", code)
	}
}

// --- storage data-plane addressing -----------------------------------------

// The azurerm provider builds data-plane URLs from the metadata suffix, with
// the literal format strings (read out of the provider binary):
//
//	https://%s.blob.%s/   https://%s.queue.%s/   https://%s.table.%s/
//
// where the second %s is suffixes.storage. It does NOT use primaryEndpoints
// for this. Azure's real suffix is a bare hostname, so the URL lands on port
// 443 -- which Nimbus cannot bind without root. Carrying the port in the
// suffix is what puts the data plane on our own HTTPS port instead.
func TestStorageSuffixCarriesThePort(t *testing.T) {
	srv, _ := newTestServer(t)
	res, err := srv.Client().Get(srv.URL + "/metadata/endpoints?api-version=2022-09-01")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	suffix, _ := m["suffixes"].(map[string]any)["storage"].(string)

	// Whatever port the server is on must be in the suffix, or clients go to 443.
	u, _ := url.Parse(srv.URL)
	if want := core.StorageSuffix + ":" + u.Port(); suffix != want {
		t.Errorf("suffixes.storage = %q, want %q", suffix, want)
	}

	// And the URL azurerm builds from it must be valid and point back at us.
	built := "https://stnimbus.blob." + suffix + "/"
	bu, err := url.Parse(built)
	if err != nil {
		t.Fatalf("azurerm would build an unparseable URL %q: %v", built, err)
	}
	if bu.Port() != u.Port() {
		t.Errorf("built URL %s targets port %q, want %q", built, bu.Port(), u.Port())
	}
	if bu.Hostname() != "stnimbus.blob."+core.StorageSuffix {
		t.Errorf("built URL hostname = %q", bu.Hostname())
	}
}

// Storage account create is declared long-running in the SDK, and its client
// rejects anything but 202 -- azurerm fails the apply outright with
//
//	performing Create: unexpected status 201 (201 Created)
//
// even though the body is a perfectly good account. Most types are happy with
// 201, so this is per-type, which is why Hook carries CreateStatus.
func TestStorageAccountCreateReturns202(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-st"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	acct := rg + "/providers/Microsoft.Storage/storageAccounts/stnimbus1"

	res := raw(t, srv, "PUT", acct, `{"location":"eastus","sku":{"name":"Standard_LRS"},"kind":"StorageV2","properties":{}}`)
	if res.StatusCode != 202 {
		t.Errorf("storage account create = %d, want 202", res.StatusCode)
	}
	if res.Header.Get("Azure-AsyncOperation") == "" {
		t.Error("a 202 must say where to poll")
	}

	// An update of an existing account is not a create and stays 200.
	res = raw(t, srv, "PUT", acct, `{"location":"eastus","sku":{"name":"Standard_LRS"},"kind":"StorageV2","properties":{}}`)
	if res.StatusCode != 200 {
		t.Errorf("storage account update = %d, want 200", res.StatusCode)
	}

	// Network types must keep their 201: this change is per-type.
	res = raw(t, srv, "PUT", rg+"/providers/Microsoft.Network/routeTables/rt", `{"location":"eastus"}`)
	if res.StatusCode != 201 {
		t.Errorf("route table create = %d, want 201 (unchanged)", res.StatusCode)
	}
}

func TestStorageAccountServerFieldsAndKeys(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-st2"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	acct := rg + "/providers/Microsoft.Storage/storageAccounts/stnimbus2"
	_, body := do(t, srv, "PUT", acct, map[string]any{
		"location": "eastus", "kind": "StorageV2",
		"sku": map[string]any{"name": "Standard_LRS"}, "properties": map[string]any{},
	})
	props := body["properties"].(map[string]any)
	for _, f := range []string{"creationTime", "primaryLocation", "statusOfPrimary", "primaryEndpoints"} {
		if props[f] == nil {
			t.Errorf("missing server-generated field %q", f)
		}
	}
	// Endpoints must carry our port, like the metadata suffix does.
	blob := props["primaryEndpoints"].(map[string]any)["blob"].(string)
	u, _ := url.Parse(srv.URL)
	if !strings.Contains(blob, ":"+u.Port()) {
		t.Errorf("primaryEndpoints.blob = %q, must carry port %s", blob, u.Port())
	}

	// listKeys is a POST action; keys never appear in a GET of the account.
	_, got := do(t, srv, "GET", acct, nil)
	if s := fmt.Sprint(got); strings.Contains(s, "keys") {
		t.Error("GET of an account must not expose keys")
	}
	code, keys := do(t, srv, "POST", acct+"/listKeys", nil)
	if code != 200 {
		t.Fatalf("listKeys = %d", code)
	}
	list, _ := keys["keys"].([]any)
	if len(list) != 2 {
		t.Fatalf("listKeys returned %d keys, want 2: %v", len(list), keys)
	}
	k1 := list[0].(map[string]any)
	if k1["keyName"] != "key1" || k1["value"] == "" {
		t.Errorf("unexpected key shape: %v", k1)
	}

	// regenerateKey rotates only the named key.
	before := list[1].(map[string]any)["value"]
	_, after := do(t, srv, "POST", acct+"/regenerateKey", map[string]any{"keyName": "key2"})
	al := after["keys"].([]any)
	if al[0].(map[string]any)["value"] != k1["value"] {
		t.Error("regenerating key2 changed key1")
	}
	if al[1].(map[string]any)["value"] == before {
		t.Error("regenerating key2 did not change it")
	}
}

// A storage account has four ARM child "service" resources that azurerm reads
// right after creating it. They are not something a client creates -- Azure
// materialises them with the account -- and if they 404, azurerm polls them
// forever: the apply hangs with "Still creating..." and never fails.
func TestStorageAccountHasDefaultServiceResources(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-svc"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	acct := rg + "/providers/Microsoft.Storage/storageAccounts/stnimbus3"
	do(t, srv, "PUT", acct, map[string]any{
		"location": "eastus", "kind": "StorageV2",
		"sku": map[string]any{"name": "Standard_LRS"}, "properties": map[string]any{},
	})

	for _, svc := range []string{"blobServices", "fileServices", "queueServices", "tableServices"} {
		code, body := do(t, srv, "GET", acct+"/"+svc+"/default", nil)
		if code != 200 {
			t.Errorf("GET %s/default = %d, want 200 (azurerm polls this until it exists)", svc, code)
			continue
		}
		if body["name"] != "default" {
			t.Errorf("%s/default name = %v", svc, body["name"])
		}
		if want := "Microsoft.Storage/storageAccounts/" + svc; !strings.EqualFold(str2(body["type"]), want) {
			t.Errorf("%s type = %v, want %s", svc, body["type"], want)
		}
		if body["properties"] == nil {
			t.Errorf("%s/default has no properties", svc)
		}
	}

	// They belong to the account, so deleting it takes them with it.
	do(t, srv, "DELETE", acct, nil)
	if code, _ := do(t, srv, "GET", acct+"/blobServices/default", nil); code != 404 {
		t.Errorf("service resource survived account deletion: %d", code)
	}
}

func str2(v any) string { s, _ := v.(string); return s }

// azurerm 4.x creates containers through ARM when the resource is given
// storage_account_id, but azurerm_storage_blob always uses the data plane. A
// container created one way must therefore exist the other way, or the apply
// fails with ContainerNotFound on a container Terraform just made.
func TestContainerCreatedThroughARMExistsInTheDataPlane(t *testing.T) {
	srv, _ := newTestServer(t)
	az := serverOf(t, srv)
	rg := sub + "/resourceGroups/rg-c"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	acct := rg + "/providers/Microsoft.Storage/storageAccounts/stnimbusc"
	do(t, srv, "PUT", acct, map[string]any{"location": "eastus", "kind": "StorageV2",
		"sku": map[string]any{"name": "Standard_LRS"}, "properties": map[string]any{}})

	cid := acct + "/blobServices/default/containers/uploads"
	if code, _ := do(t, srv, "PUT", cid, map[string]any{"properties": map[string]any{"publicAccess": "None"}}); code != 201 {
		t.Fatalf("ARM container create = %d", code)
	}

	a := az.storage.Store().Account("stnimbusc")
	if a == nil {
		t.Fatal("account missing from the data-plane store")
	}
	if a.Container("uploads") == nil {
		t.Error("container created through ARM is not visible to the data plane")
	}

	// And deleting it through ARM removes the data-plane container too.
	do(t, srv, "DELETE", cid, nil)
	if a.Container("uploads") != nil {
		t.Error("container deleted through ARM still exists in the data plane")
	}
}

// Deleting a container through ARM is synchronous and answers 200. Our generic
// delete returns 202 with an async-operation header, which azurerm rejects:
//
//	unexpected status 202 (202 Accepted) received with no body
//
// Resource-group and network deletes are genuinely async and keep their 202,
// so this is per-type like CreateStatus.
func TestARMContainerDeleteIsSynchronous(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-cd"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	acct := rg + "/providers/Microsoft.Storage/storageAccounts/stnimbuscd"
	do(t, srv, "PUT", acct, map[string]any{"location": "eastus", "kind": "StorageV2",
		"sku": map[string]any{"name": "Standard_LRS"}, "properties": map[string]any{}})
	cid := acct + "/blobServices/default/containers/c1"
	do(t, srv, "PUT", cid, map[string]any{"properties": map[string]any{}})

	res := raw(t, srv, "DELETE", cid, "")
	if res.StatusCode != 200 {
		t.Errorf("ARM container delete = %d, want 200", res.StatusCode)
	}
	// The account delete is synchronous too, even though its create is async.
	if res := raw(t, srv, "DELETE", acct, ""); res.StatusCode != 200 {
		t.Errorf("storage account delete = %d, want 200", res.StatusCode)
	}

	// A route table delete is async and must still be 202.
	do(t, srv, "PUT", rg+"/providers/Microsoft.Network/routeTables/rt", map[string]any{"location": "eastus"})
	if res := raw(t, srv, "DELETE", rg+"/providers/Microsoft.Network/routeTables/rt", ""); res.StatusCode != 202 {
		t.Errorf("route table delete = %d, want 202 (unchanged)", res.StatusCode)
	}
}

// Deleting a resource must run AfterDelete for EVERY resource that goes, not
// just the one addressed. store.Delete cascades to descendants, and a type
// whose hook stops a real engine -- a k3s container, in M3 -- would otherwise
// leak it whenever an ancestor is deleted instead of the resource itself.
// Leaves must fire before their parents, so a child can still see its parent
// while cleaning up.
func TestCascadeDeleteRunsHooksForEveryResourceLeavesFirst(t *testing.T) {
	srv, _ := newTestServer(t)
	az := serverOf(t, srv)

	var fired []string
	az.registerHook("Nimbus.Test/parents", Hook{
		AfterDelete: func(s *Server, t target) { fired = append(fired, "parent:"+t.Name) },
	})
	az.registerHook("Nimbus.Test/parents/children", Hook{
		AfterDelete: func(s *Server, t target) { fired = append(fired, "child:"+t.Name) },
	})

	rg := sub + "/resourceGroups/rg-cascade-hooks"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	parent := rg + "/providers/Nimbus.Test/parents/p1"
	do(t, srv, "PUT", parent, map[string]any{"location": "eastus"})
	do(t, srv, "PUT", parent+"/children/c1", map[string]any{"properties": map[string]any{}})
	do(t, srv, "PUT", parent+"/children/c2", map[string]any{"properties": map[string]any{}})

	// Delete the PARENT. Both children go with it, and all three hooks must run.
	if code, _ := do(t, srv, "DELETE", parent, nil); code != 202 {
		t.Fatalf("DELETE parent = %d", code)
	}

	want := map[string]bool{"parent:p1": true, "child:c1": true, "child:c2": true}
	if len(fired) != len(want) {
		t.Fatalf("hooks fired for %v, want all of %v", fired, keysOf(want))
	}
	for _, f := range fired {
		if !want[f] {
			t.Errorf("unexpected hook fired: %s", f)
		}
	}
	// The parent's own hook must come last: children clean up first.
	if fired[len(fired)-1] != "parent:p1" {
		t.Errorf("hook order was %v; the parent must fire last", fired)
	}
}

// Deleting a resource group must reach grandchildren too, not just one level.
func TestCascadeDeleteReachesGrandchildren(t *testing.T) {
	srv, _ := newTestServer(t)
	az := serverOf(t, srv)

	var fired []string
	az.registerHook("Nimbus.Test/parents/children", Hook{
		AfterDelete: func(s *Server, t target) { fired = append(fired, t.Name) },
	})

	rg := sub + "/resourceGroups/rg-deep"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	parent := rg + "/providers/Nimbus.Test/parents/p1"
	do(t, srv, "PUT", parent, map[string]any{"location": "eastus"})
	do(t, srv, "PUT", parent+"/children/c1", map[string]any{"properties": map[string]any{}})

	// Delete the RESOURCE GROUP: the child is two levels down.
	do(t, srv, "DELETE", rg, nil)
	if len(fired) != 1 || fired[0] != "c1" {
		t.Errorf("grandchild hook did not run on a resource-group delete: %v", fired)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The concrete thing the cascade fix buys today: deleting a resource group
// purges the storage data of every account inside it. Before AfterDelete ran
// for descendants, the ARM resources vanished while the account's blobs stayed
// in the data-plane store forever -- invisible, and still served if an account
// of the same name was recreated.
func TestDeletingResourceGroupPurgesStorageData(t *testing.T) {
	srv, _ := newTestServer(t)
	az := serverOf(t, srv)
	rg := sub + "/resourceGroups/rg-purge"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	acct := rg + "/providers/Microsoft.Storage/storageAccounts/stnimbuspurge"
	do(t, srv, "PUT", acct, map[string]any{"location": "eastus", "kind": "StorageV2",
		"sku": map[string]any{"name": "Standard_LRS"}, "properties": map[string]any{}})

	a := az.storage.Store().Account("stnimbuspurge")
	if a == nil {
		t.Fatal("account missing from the data-plane store")
	}
	c, _ := a.CreateContainer("data", nil, "")
	c.PutBlob(&storage.Blob{Name: "secret.txt", Content: []byte("should not survive")})

	// Delete the RESOURCE GROUP, not the account.
	do(t, srv, "DELETE", rg, nil)

	if az.storage.Store().Account("stnimbuspurge") != nil {
		t.Error("storage account data survived deletion of its resource group")
	}
}

// --- async operations -------------------------------------------------------

// Until M3 every write finished before it was answered, so the operation
// endpoint could honestly say Succeeded for any ID. A k3s cluster takes ~30 s
// to boot, so operations now carry real per-operation state.
func TestAsyncOperationReportsRealState(t *testing.T) {
	srv, _ := newTestServer(t)
	az := serverOf(t, srv)

	release := make(chan error, 1)
	az.registerHook("Nimbus.Test/slow", Hook{
		Provision: func(s *Server, t target, body map[string]any) error { return <-release },
	})

	rg := sub + "/resourceGroups/rg-async"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	id := rg + "/providers/Nimbus.Test/slow/s1"

	res := raw(t, srv, "PUT", id, `{"location":"eastus"}`)
	op := res.Header.Get("Azure-AsyncOperation")
	if op == "" {
		t.Fatal("a long-running create must advertise an operation")
	}
	u, _ := url.Parse(op)

	// The write returns immediately, while provisioning is still running.
	if code, body := do(t, srv, "GET", u.Path, nil); code != 200 || body["status"] != "InProgress" {
		t.Errorf("operation while provisioning = %d %v, want InProgress", code, body)
	}
	_, got := do(t, srv, "GET", id, nil)
	if ps := got["properties"].(map[string]any)["provisioningState"]; ps != "Creating" {
		t.Errorf("resource provisioningState = %v, want Creating", ps)
	}

	release <- nil
	waitForOperation(t, srv, u.Path, "Succeeded")

	_, got = do(t, srv, "GET", id, nil)
	if ps := got["properties"].(map[string]any)["provisioningState"]; ps != "Succeeded" {
		t.Errorf("after provisioning, provisioningState = %v, want Succeeded", ps)
	}
}

// A failed provision must surface as a Failed operation carrying the reason,
// not as a resource stuck on Creating forever.
func TestFailedProvisionFailsTheOperation(t *testing.T) {
	srv, _ := newTestServer(t)
	az := serverOf(t, srv)
	az.registerHook("Nimbus.Test/broken", Hook{
		Provision: func(s *Server, t target, body map[string]any) error {
			return errors.New("docker is not available: start Docker Desktop")
		},
	})

	rg := sub + "/resourceGroups/rg-fail"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	id := rg + "/providers/Nimbus.Test/broken/b1"
	res := raw(t, srv, "PUT", id, `{"location":"eastus"}`)
	u, _ := url.Parse(res.Header.Get("Azure-AsyncOperation"))

	body := waitForOperation(t, srv, u.Path, "Failed")
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("a failed operation must carry an error: %v", body)
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "start Docker Desktop") {
		t.Errorf("the failure reason was lost: %v", errObj)
	}
	_, got := do(t, srv, "GET", id, nil)
	if ps := got["properties"].(map[string]any)["provisioningState"]; ps != "Failed" {
		t.Errorf("resource provisioningState = %v, want Failed", ps)
	}
}

// Types without a Provision hook stay synchronous, so M1 and M2 behaviour is
// untouched: their operation URL answers Succeeded straight away.
func TestSynchronousTypesStillReportSucceededImmediately(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-sync-op"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	res := raw(t, srv, "PUT", rg+"/providers/Microsoft.Network/routeTables/rt", `{"location":"eastus"}`)
	u, _ := url.Parse(res.Header.Get("Azure-AsyncOperation"))
	if code, body := do(t, srv, "GET", u.Path, nil); code != 200 || body["status"] != "Succeeded" {
		t.Errorf("synchronous operation = %d %v, want Succeeded", code, body)
	}
}

// An unknown operation ID must 404 rather than claim success, or a client
// polling a stale URL would believe work it never started had finished.
func TestUnknownOperationIs404(t *testing.T) {
	srv, _ := newTestServer(t)
	path := sub + operationsPath + "00000000-0000-0000-0000-00000000dead"
	if code, _ := do(t, srv, "GET", path, nil); code != 404 {
		t.Errorf("unknown operation = %d, want 404", code)
	}
}

func waitForOperation(t *testing.T, srv *httptest.Server, path, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, body := do(t, srv, "GET", path, nil)
		if body["status"] == want {
			return body
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, body := do(t, srv, "GET", path, nil)
	t.Fatalf("operation never reached %s; last state %v", want, body)
	return nil
}
