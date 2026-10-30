package azure

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Asere231/nimbus/internal/core"
)

// These tests replay the HTTP calls the Terraform azurerm provider (via
// hashicorp/go-azure-sdk) makes, in the same order and shapes.

const sub = "/subscriptions/" + DefaultSubscriptionID

func newTestServer(t *testing.T) (*httptest.Server, *core.Store) {
	t.Helper()
	bus := core.NewBus(100)
	store, _ := core.NewStore(bus, "")
	var az *Server
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { az.ServeHTTP(w, r) }))
	az = New(Config{PublicURL: srv.URL}, store, bus)
	t.Cleanup(srv.Close)
	return srv, store
}

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

// Writes advertise an async operation that is already Succeeded, with
// Retry-After: 1, so the SDK polls once after a second instead of waiting 10.
func TestWritesAdvertiseFastAsyncOperation(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-lro"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	req, _ := http.NewRequest("PUT", srv.URL+rg+"/providers/Microsoft.Network/routeTables/rt?api-version=2023-09-01", strings.NewReader(`{"location":"eastus"}`))
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
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
