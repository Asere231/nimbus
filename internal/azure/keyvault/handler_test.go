package keyvault

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests replay the calls the Key Vault SDKs and the az CLI make.

func newTestVault(t *testing.T) (*httptest.Server, *Handler) {
	t.Helper()
	h := NewHandler(NewStore())
	h.Login = "https://localhost:4701"
	h.Audience = "https://vault.azure.net"
	h.BaseURI = func(name string) string {
		return "https://" + strings.ToLower(name) + ".vault.nimbus.localhost:4701/"
	}
	h.Store().CreateVault("kv-nimbus")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Serve(w, r, "kv-nimbus")
	}))
	t.Cleanup(srv.Close)
	return srv, h
}

func call(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]any, http.Header) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rdr)
	// Every real client sends a token after the first challenge; these tests
	// send one from the start except where the challenge is what is tested.
	req.Header.Set("Authorization", "Bearer nimbus-test-token")
	req.Header.Set("Content-Type", "application/json")
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

func TestParseHost(t *testing.T) {
	const suffix = "vault.nimbus.localhost:4701"
	cases := []struct {
		host string
		want string
		ok   bool
	}{
		{"kv-nimbus.vault.nimbus.localhost:4701", "kv-nimbus", true},
		{"KV-Nimbus.Vault.Nimbus.Localhost:4701", "kv-nimbus", true},
		// The Host header may or may not carry a port, whatever the suffix does.
		{"kv-nimbus.vault.nimbus.localhost", "kv-nimbus", true},
		// ARM requests, which must not be mistaken for the data plane.
		{"localhost:4701", "", false},
		{"stnimbus.blob.storage.nimbus.localhost:4701", "", false},
		// A vault name is one label: anything deeper is not ours.
		{"a.b.vault.nimbus.localhost:4701", "", false},
		{"vault.nimbus.localhost:4701", "", false},
	}
	for _, c := range cases {
		got, ok := ParseHost(c.host, suffix)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseHost(%q) = %q,%v; want %q,%v", c.host, got, ok, c.want, c.ok)
		}
	}
}

// Key Vault clients deliberately send their first request without a token and
// read the authority out of the 401. Answering such a request would work by
// accident and break the SDKs' challenge policies, which refuse to send a
// token to a service that never asked for one.
func TestUnauthenticatedRequestIsChallenged(t *testing.T) {
	srv, _ := newTestVault(t)
	req, _ := http.NewRequest("GET", srv.URL+"/secrets/db-password?api-version=7.4", nil)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("unauthenticated GET = %d, want 401", res.StatusCode)
	}
	challenge := res.Header.Get("WWW-Authenticate")
	if !strings.HasPrefix(challenge, "Bearer ") {
		t.Fatalf("WWW-Authenticate = %q", challenge)
	}
	for _, want := range []string{
		`authorization="https://localhost:4701"`,
		`resource="https://vault.azure.net"`,
		`scope="https://vault.azure.net/.default"`,
	} {
		if !strings.Contains(challenge, want) {
			t.Errorf("challenge %q is missing %s", challenge, want)
		}
	}
}

func TestSecretLifecycle(t *testing.T) {
	srv, _ := newTestVault(t)

	code, body, _ := call(t, srv, "PUT", "/secrets/db-password?api-version=7.4",
		`{"value":"s3cret","contentType":"text/plain","tags":{"env":"dev"}}`)
	if code != 200 {
		t.Fatalf("set = %d: %v", code, body)
	}
	id, _ := body["id"].(string)
	if !strings.HasPrefix(id, "https://kv-nimbus.vault.nimbus.localhost:4701/secrets/db-password/") {
		t.Fatalf("secret id = %q", id)
	}
	version := id[strings.LastIndex(id, "/")+1:]
	if len(version) != 32 {
		t.Errorf("version = %q, want 32 hex characters like Key Vault's", version)
	}
	if body["value"] != "s3cret" || body["contentType"] != "text/plain" {
		t.Errorf("set response = %v", body)
	}
	attrs := body["attributes"].(map[string]any)
	if attrs["enabled"] != true || attrs["created"] == nil || attrs["recoveryLevel"] == nil {
		t.Errorf("attributes = %v", attrs)
	}

	// Read back, both ways round.
	code, got, _ := call(t, srv, "GET", "/secrets/db-password?api-version=7.4", "")
	if code != 200 || got["value"] != "s3cret" {
		t.Fatalf("get latest = %d %v", code, got)
	}
	code, got, _ = call(t, srv, "GET", "/secrets/db-password/"+version+"?api-version=7.4", "")
	if code != 200 || got["value"] != "s3cret" {
		t.Fatalf("get by version = %d %v", code, got)
	}

	// Every set makes a new version, and the old one stays readable: that is
	// what a client holding a versioned ID in state depends on.
	_, second, _ := call(t, srv, "PUT", "/secrets/db-password?api-version=7.4", `{"value":"rotated"}`)
	secondID := second["id"].(string)
	if secondID == id {
		t.Error("re-setting a secret reused the version")
	}
	_, old, _ := call(t, srv, "GET", "/secrets/db-password/"+version+"?api-version=7.4", "")
	if old["value"] != "s3cret" {
		t.Errorf("old version changed: %v", old)
	}
	_, latest, _ := call(t, srv, "GET", "/secrets/db-password?api-version=7.4", "")
	if latest["value"] != "rotated" {
		t.Errorf("latest = %v", latest)
	}

	_, versions, _ := call(t, srv, "GET", "/secrets/db-password/versions?api-version=7.4", "")
	if l, _ := versions["value"].([]any); len(l) != 2 {
		t.Errorf("versions returned %d entries: %v", len(l), versions)
	}

	// A list never carries values, and its ids are versionless.
	_, list, _ := call(t, srv, "GET", "/secrets?api-version=7.4", "")
	items, _ := list["value"].([]any)
	if len(items) != 1 {
		t.Fatalf("list returned %d secrets: %v", len(items), list)
	}
	item := items[0].(map[string]any)
	if _, ok := item["value"]; ok {
		t.Error("a secret list must not include values")
	}
	if item["id"] != "https://kv-nimbus.vault.nimbus.localhost:4701/secrets/db-password" {
		t.Errorf("list item id = %v, want the versionless id", item["id"])
	}

	// PATCH updates attributes in place, without a new version and without
	// echoing the value.
	code, patched, _ := call(t, srv, "PATCH", "/secrets/db-password?api-version=7.4",
		`{"attributes":{"enabled":false},"tags":{"env":"prod"}}`)
	if code != 200 {
		t.Fatalf("patch = %d: %v", code, patched)
	}
	if patched["id"] != secondID {
		t.Errorf("patch made a new version: %v, want %v", patched["id"], secondID)
	}
	if _, ok := patched["value"]; ok {
		t.Error("a patch response must not include the value")
	}
	if patched["attributes"].(map[string]any)["enabled"] != false {
		t.Errorf("enabled did not change: %v", patched["attributes"])
	}
}

// Deleting a secret is a soft delete, like the vault itself: the name stays
// taken until it is purged or recovered.
func TestSecretSoftDeleteRecoverPurge(t *testing.T) {
	srv, _ := newTestVault(t)
	call(t, srv, "PUT", "/secrets/api-key?api-version=7.4", `{"value":"abc"}`)

	code, deleted, _ := call(t, srv, "DELETE", "/secrets/api-key?api-version=7.4", "")
	if code != 200 {
		t.Fatalf("delete = %d: %v", code, deleted)
	}
	if deleted["recoveryId"] == nil || deleted["deletedDate"] == nil || deleted["scheduledPurgeDate"] == nil {
		t.Errorf("deleted bundle = %v", deleted)
	}

	// Gone from the live surface, with a message that says why.
	code, body, _ := call(t, srv, "GET", "/secrets/api-key?api-version=7.4", "")
	if code != 404 {
		t.Fatalf("get deleted secret = %d", code)
	}
	if msg, _ := body["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "recoverable") {
		t.Errorf("message does not mention the recoverable state: %v", body)
	}
	// And its name is still taken.
	if code, _, _ := call(t, srv, "PUT", "/secrets/api-key?api-version=7.4", `{"value":"x"}`); code != 409 {
		t.Errorf("setting a deleted secret = %d, want 409", code)
	}

	code, _, _ = call(t, srv, "GET", "/deletedsecrets/api-key?api-version=7.4", "")
	if code != 200 {
		t.Errorf("get from the recycle bin = %d", code)
	}
	_, bin, _ := call(t, srv, "GET", "/deletedsecrets?api-version=7.4", "")
	if l, _ := bin["value"].([]any); len(l) != 1 {
		t.Errorf("recycle bin has %d entries", len(l))
	}

	// Recover, and the value comes back with it.
	if code, _, _ := call(t, srv, "POST", "/deletedsecrets/api-key/recover?api-version=7.4", ""); code != 200 {
		t.Fatalf("recover = %d", code)
	}
	_, got, _ := call(t, srv, "GET", "/secrets/api-key?api-version=7.4", "")
	if got["value"] != "abc" {
		t.Errorf("recovered secret = %v", got)
	}

	// Delete again, then purge: now the name is free.
	call(t, srv, "DELETE", "/secrets/api-key?api-version=7.4", "")
	if code, _, _ := call(t, srv, "DELETE", "/deletedsecrets/api-key?api-version=7.4", ""); code != 204 {
		t.Errorf("purge = %d, want 204", code)
	}
	if code, _, _ := call(t, srv, "GET", "/deletedsecrets/api-key?api-version=7.4", ""); code != 404 {
		t.Error("purged secret is still in the recycle bin")
	}
	if code, _, _ := call(t, srv, "PUT", "/secrets/api-key?api-version=7.4", `{"value":"fresh"}`); code != 200 {
		t.Error("the name is still taken after a purge")
	}
}

// Keys and certificates are not emulated, and must say so rather than
// returning an empty success: a client that believes it wrapped a key with a
// key that does not exist is in a much worse place.
func TestKeysAndCertificatesAreRefusedClearly(t *testing.T) {
	srv, _ := newTestVault(t)
	for _, path := range []string{"/keys/k1/create?api-version=7.4", "/certificates/c1?api-version=7.4"} {
		code, body, _ := call(t, srv, "POST", path, "{}")
		if code != 501 {
			t.Errorf("%s = %d, want 501: %v", path, code, body)
		}
		if body["error"].(map[string]any)["code"] != "NotSupported" {
			t.Errorf("%s error = %v", path, body["error"])
		}
	}
}

func TestUnknownVaultAndSecretErrors(t *testing.T) {
	h := NewHandler(NewStore())
	h.Login = "https://localhost:4701"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Serve(w, r, "nope")
	}))
	t.Cleanup(srv.Close)

	code, body, _ := call(t, srv, "GET", "/secrets/x?api-version=7.4", "")
	if code != 404 || body["error"].(map[string]any)["code"] != "VaultNotFound" {
		t.Errorf("unknown vault = %d %v", code, body)
	}
}

func TestSecretNamesAreValidated(t *testing.T) {
	srv, _ := newTestVault(t)
	for _, name := range []string{"has.dot", "has_underscore", "has%20space"} {
		code, _, _ := call(t, srv, "PUT", "/secrets/"+name+"?api-version=7.4", `{"value":"x"}`)
		if code != 400 && code != 404 {
			t.Errorf("secret name %q accepted with %d", name, code)
		}
	}
}

// Persistence: a restart must find the secrets, including the recycle bin.
func TestSecretsSurviveAReload(t *testing.T) {
	dir := t.TempDir()
	store := NewStore()
	if err := store.Persist(dir); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(store)
	h.Login = "https://localhost:4701"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Serve(w, r, "kv-persist")
	}))
	t.Cleanup(srv.Close)
	store.CreateVault("kv-persist")

	call(t, srv, "PUT", "/secrets/keeper?api-version=7.4", `{"value":"kept","contentType":"text/plain"}`)
	call(t, srv, "PUT", "/secrets/goner?api-version=7.4", `{"value":"bin"}`)
	call(t, srv, "DELETE", "/secrets/goner?api-version=7.4", "")

	reloaded := NewStore()
	if err := reloaded.Persist(dir); err != nil {
		t.Fatal(err)
	}
	v := reloaded.Vault("kv-persist")
	if v == nil {
		t.Fatal("vault did not survive the reload")
	}
	sec := v.Secret("keeper")
	if sec == nil || sec.Latest().Value != "kept" || sec.Latest().ContentType != "text/plain" {
		t.Fatalf("secret did not survive: %+v", sec)
	}
	if v.DeletedSecret("goner") == nil {
		t.Error("the recycle bin did not survive the reload")
	}
	if v.Secret("goner") != nil {
		t.Error("a deleted secret came back alive")
	}
}

// A purge of the whole vault removes its file, so a vault of the same name
// created later does not inherit the old secrets.
func TestDeletingAVaultRemovesItsFile(t *testing.T) {
	dir := t.TempDir()
	store := NewStore()
	if err := store.Persist(dir); err != nil {
		t.Fatal(err)
	}
	v := store.CreateVault("kv-gone")
	v.SetSecret("s", "v", "", nil, Attributes{})
	store.Save(v)

	store.DeleteVault("kv-gone")
	reloaded := NewStore()
	if err := reloaded.Persist(dir); err != nil {
		t.Fatal(err)
	}
	if reloaded.Vault("kv-gone") != nil {
		t.Error("a purged vault came back after a reload")
	}
}

func TestAttributesRoundTrip(t *testing.T) {
	srv, _ := newTestVault(t)
	// nbf and exp are unix seconds in Key Vault's wire format, not RFC 3339.
	_, body, _ := call(t, srv, "PUT", "/secrets/timed?api-version=7.4",
		`{"value":"x","attributes":{"enabled":true,"nbf":1700000000,"exp":1900000000}}`)
	attrs, _ := body["attributes"].(map[string]any)
	if attrs["nbf"] != float64(1700000000) || attrs["exp"] != float64(1900000000) {
		t.Errorf("attributes = %v", attrs)
	}
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(body)
	if strings.Contains(buf.String(), `"nbf":"`) {
		t.Error("nbf must be a number, not a string")
	}
}
