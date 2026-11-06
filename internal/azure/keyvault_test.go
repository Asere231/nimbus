package azure

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The vault's data-plane address comes from properties.vaultUri: azurerm
// reads the vault through ARM and uses that URI for every secret operation
// ("looking up base uri for secret ... in ..."). It therefore has to carry our
// port, exactly like the storage endpoints do -- binding 443 needs root.
func TestVaultServerFieldsAndURI(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-kv"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	vault := rg + "/providers/Microsoft.KeyVault/vaults/kv-nimbus"

	code, body := do(t, srv, "PUT", vault, map[string]any{
		"location": "eastus",
		"properties": map[string]any{
			"tenantId": DefaultTenantID,
			"sku":      map[string]any{"family": "A", "name": "standard"},
		},
	})
	if code != 201 {
		t.Fatalf("PUT vault = %d: %v", code, body)
	}
	props := body["properties"].(map[string]any)
	uri := str(props["vaultUri"])
	u, _ := url.Parse(srv.URL)
	want := "https://kv-nimbus." + KeyVaultSuffix + ":" + u.Port() + "/"
	if uri != want {
		t.Errorf("vaultUri = %q, want %q", uri, want)
	}
	for _, f := range []string{"tenantId", "sku", "accessPolicies", "enableSoftDelete", "softDeleteRetentionInDays"} {
		if props[f] == nil {
			t.Errorf("missing %q: %v", f, props)
		}
	}
	// Soft delete is on by default in Azure and cannot be turned off; azurerm
	// relies on that when it purges on destroy.
	if props["enableSoftDelete"] != true {
		t.Errorf("enableSoftDelete = %v", props["enableSoftDelete"])
	}

	// Stable across reads: a moving vaultUri would be a permanent diff.
	_, got := do(t, srv, "GET", vault, nil)
	if got["properties"].(map[string]any)["vaultUri"] != uri {
		t.Error("vaultUri changed between reads")
	}
}

// The metadata suffix is what the az CLI and the SDKs build vault hostnames
// from, so it carries the port for the same reason the storage suffix does.
func TestKeyVaultSuffixCarriesThePort(t *testing.T) {
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
	suffixes := m["suffixes"].(map[string]any)
	u, _ := url.Parse(srv.URL)
	want := KeyVaultSuffix + ":" + u.Port()
	if suffixes["keyVaultDns"] != want {
		t.Errorf("keyVaultDns = %v, want %v", suffixes["keyVaultDns"], want)
	}
}

// Vault names end up in a hostname, so a name that is not a valid DNS label
// has to be rejected here rather than producing an unreachable vault.
func TestVaultNameIsValidated(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-kvbad"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})

	for _, name := range []string{"kv", "has.dot", "UPPER_CASE", "-leading", strings.Repeat("v", 25)} {
		code, body := do(t, srv, "PUT", rg+"/providers/Microsoft.KeyVault/vaults/"+name,
			map[string]any{"location": "eastus", "properties": map[string]any{}})
		if code != 400 {
			t.Errorf("vault name %q accepted with %d (%v)", name, code, errCode(body))
		}
	}
	// Upper case in a name Azure would accept: vault names are
	// case-insensitive for DNS, so this one is fine.
	if code, body := do(t, srv, "PUT", rg+"/providers/Microsoft.KeyVault/vaults/KV-Mixed",
		map[string]any{"location": "eastus", "properties": map[string]any{}}); code != 201 {
		t.Errorf("mixed-case vault name rejected: %d %v", code, body)
	}
}

// Destroying a vault soft-deletes it: the name stays taken until it is purged,
// which is exactly what azurerm does on destroy (purge_soft_delete_on_destroy
// defaults to true). Getting this wrong means the second apply of the same
// config fails with a name conflict.
func TestVaultSoftDeleteAndPurge(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-kvsd"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	vault := rg + "/providers/Microsoft.KeyVault/vaults/kv-soft"
	do(t, srv, "PUT", vault, map[string]any{"location": "eastus", "properties": map[string]any{}})

	if code, _ := do(t, srv, "DELETE", vault, nil); code != 200 && code != 202 {
		t.Fatalf("DELETE vault = %d", code)
	}
	if code, _ := do(t, srv, "GET", vault, nil); code != 404 {
		t.Errorf("deleted vault still readable through ARM: %d", code)
	}

	deleted := sub + "/providers/Microsoft.KeyVault/locations/eastus/deletedVaults/kv-soft"
	code, body := do(t, srv, "GET", deleted, nil)
	if code != 200 {
		t.Fatalf("GET deleted vault = %d: %v", code, body)
	}
	dp := body["properties"].(map[string]any)
	if str(dp["vaultId"]) == "" || str(dp["deletionDate"]) == "" || str(dp["scheduledPurgeDate"]) == "" {
		t.Errorf("deleted vault properties = %v", dp)
	}

	// Listed subscription-wide, which is how a client discovers them.
	_, list := do(t, srv, "GET", sub+"/providers/Microsoft.KeyVault/deletedVaults", nil)
	if l, _ := list["value"].([]any); len(l) != 1 {
		t.Errorf("deletedVaults list returned %d entries: %v", len(l), list)
	}

	// The name is taken while it is soft-deleted.
	_, avail := do(t, srv, "POST", sub+"/providers/Microsoft.KeyVault/checkNameAvailability",
		map[string]any{"name": "kv-soft", "type": "Microsoft.KeyVault/vaults"})
	if avail["nameAvailable"] != false {
		t.Errorf("soft-deleted name reported available: %v", avail)
	}

	// Purge is long-running: the client polls afterwards, and must be sent to
	// an operation rather than to the vault URL, where our own 404 would read
	// as a failed purge.
	purge := raw(t, srv, "POST", deleted+"/purge", "")
	if purge.StatusCode != 202 {
		t.Fatalf("purge = %d, want 202", purge.StatusCode)
	}
	if purge.Header.Get("Azure-AsyncOperation") == "" {
		t.Error("purge must advertise an async operation for the client to poll")
	}
	if code, _ := do(t, srv, "GET", deleted, nil); code != 404 {
		t.Errorf("purged vault still listed: %d", code)
	}
	_, avail = do(t, srv, "POST", sub+"/providers/Microsoft.KeyVault/checkNameAvailability",
		map[string]any{"name": "kv-soft", "type": "Microsoft.KeyVault/vaults"})
	if avail["nameAvailable"] != true {
		t.Errorf("purged name still unavailable: %v", avail)
	}

	// And the vault can be created again, which is the whole point.
	if code, body := do(t, srv, "PUT", vault, map[string]any{"location": "eastus", "properties": map[string]any{}}); code != 201 {
		t.Errorf("re-create after purge = %d: %v", code, body)
	}
}

// A soft-deleted vault can also be recovered, which is what
// createMode = "recover" and the recover action are for.
func TestDeletedVaultCanBeRecovered(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-kvrec"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	vault := rg + "/providers/Microsoft.KeyVault/vaults/kv-rec"
	_, created := do(t, srv, "PUT", vault, map[string]any{"location": "eastus",
		"properties": map[string]any{"tags": map[string]any{}}})
	uri := created["properties"].(map[string]any)["vaultUri"]
	do(t, srv, "DELETE", vault, nil)

	code, body := do(t, srv, "PUT", vault, map[string]any{"location": "eastus",
		"properties": map[string]any{"createMode": "recover"}})
	if code != 200 && code != 201 {
		t.Fatalf("recover = %d: %v", code, body)
	}
	if body["properties"].(map[string]any)["vaultUri"] != uri {
		t.Error("recovered vault has a different vaultUri")
	}
	if code, _ := do(t, srv, "GET", sub+"/providers/Microsoft.KeyVault/locations/eastus/deletedVaults/kv-rec", nil); code != 404 {
		t.Error("recovered vault is still in the deleted list")
	}
}

// Deleting the resource group takes the vault with it, and the vault must
// still end up soft-deleted: a cascade that skipped the hook would leave the
// name free in Nimbus but taken in Azure.
func TestDeletingTheGroupSoftDeletesTheVault(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-kvcascade"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	do(t, srv, "PUT", rg+"/providers/Microsoft.KeyVault/vaults/kv-casc",
		map[string]any{"location": "eastus", "properties": map[string]any{}})
	do(t, srv, "DELETE", rg, nil)

	if code, _ := do(t, srv, "GET", sub+"/providers/Microsoft.KeyVault/locations/eastus/deletedVaults/kv-casc", nil); code != 200 {
		t.Errorf("vault removed by a cascade was not soft-deleted: %d", code)
	}
}

// The end-to-end address: create a vault through ARM, then reach its data
// plane on the host from its own vaultUri, on the same listener. This is the
// join that M2 had to get right for storage, and the one a client actually
// walks: ARM read, then Host-based routing.
func TestVaultDataPlaneIsReachableAtItsVaultURI(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-kvdp"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	vault := rg + "/providers/Microsoft.KeyVault/vaults/kv-data"
	_, body := do(t, srv, "PUT", vault, map[string]any{"location": "eastus", "properties": map[string]any{}})
	uri, _ := url.Parse(str(body["properties"].(map[string]any)["vaultUri"]))

	// Unauthenticated first, as every Key Vault client does.
	code, _, hdr := vaultCall(t, srv, uri.Host, "GET", "/secrets/db?api-version=7.4", "", false)
	if code != 401 || !strings.Contains(hdr.Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("unauthenticated data-plane GET = %d, challenge %q", code, hdr.Get("WWW-Authenticate"))
	}

	code, set, _ := vaultCall(t, srv, uri.Host, "PUT", "/secrets/db?api-version=7.4", `{"value":"hunter2"}`, true)
	if code != 200 {
		t.Fatalf("set through the data plane = %d: %v", code, set)
	}
	if id, _ := set["id"].(string); !strings.HasPrefix(id, str(body["properties"].(map[string]any)["vaultUri"])) {
		t.Errorf("secret id %q is not under the vault's own vaultUri", id)
	}

	// ARM delete soft-deletes the vault, so the secrets must survive for a
	// recovery; purging the vault is what destroys them.
	do(t, srv, "DELETE", vault, nil)
	az := serverOf(t, srv)
	if v := az.keyvault.Store().Vault("kv-data"); v == nil || v.Secret("db") == nil {
		t.Error("soft-deleting the vault threw away its secrets")
	}
	do(t, srv, "POST", sub+"/providers/Microsoft.KeyVault/locations/eastus/deletedVaults/kv-data/purge", nil)
	if az.keyvault.Store().Vault("kv-data") != nil {
		t.Error("purging the vault left its data behind")
	}
}

// vaultCall sends a data-plane request to the Azure listener with a vault Host
// header, which is how the two planes share one port.
func vaultCall(t *testing.T, srv *httptest.Server, host, method, path, body string, withToken bool) (int, map[string]any, http.Header) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rdr)
	req.Host = host
	if withToken {
		req.Header.Set("Authorization", "Bearer nimbus-test-token")
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out, res.Header
}
