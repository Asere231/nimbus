package azure

import (
	"encoding/json"
	"github.com/Asere231/nimbus/internal/azure/keyvault"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Azure's OTHER action axis.
//
// `dataActions` are not a resource policy -- they hang off the same
// caller-side role assignments everything else does -- but they are the
// reason an Azure learner cannot otherwise see why reading a SECRET is a
// different permission from reading the vault. Owner and Contributor
// hold `Actions: ["*"]` and no data actions at all, which is the single
// most surprising true thing about Azure RBAC.
//
// Only Key Vault's data plane is enforced. Storage's Shared Key carries
// no principal to evaluate anything against -- see the roadmap.

const (
	roleKVSecretsUser    = "4633458b-17de-408a-b874-0445c86b69e6"
	roleKVSecretsOfficer = "b86a8fe4-44ce-4948-aee5-eccb2c155cd7"
	roleKVReader         = "21090545-7ca7-4776-b22c-e363652d74d2"
)

// makeVault creates a vault through ARM as the unrestricted default
// identity, and returns its data-plane host.
func makeVault(t *testing.T, srv *httptest.Server, name string) (host, id string) {
	t.Helper()
	rg := rgPath("rg-kv")
	if code, body := do(t, srv, http.MethodPut, rg, map[string]any{"location": "eastus"}); code >= 300 {
		t.Fatalf("creating the resource group: %d %v", code, body)
	}
	id = rg + "/providers/Microsoft.KeyVault/vaults/" + name
	code, body := do(t, srv, http.MethodPut, id, map[string]any{
		"location": "eastus",
		"properties": map[string]any{
			"tenantId": DefaultTenantID,
			"sku":      map[string]any{"family": "A", "name": "standard"},
		},
	})
	if code >= 300 {
		t.Fatalf("creating the vault: %d %v", code, body)
	}
	az := serverOf(t, srv)
	return name + "." + az.keyVaultSuffix(), id
}

// vaultAs is vaultCall with a token claiming a particular object id,
// which is what a data action is evaluated against.
func vaultAs(t *testing.T, srv *httptest.Server, host, oid, method, path, body string) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rdr)
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+fakeJWT(map[string]any{
		"oid": oid, "tid": DefaultTenantID,
	}))
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

func setSecretAsDefault(t *testing.T, srv *httptest.Server, host, name, value string) {
	t.Helper()
	code, out, _ := vaultCall(t, srv, host, http.MethodPut,
		"/secrets/"+name+"?api-version=7.4", `{"value":"`+value+`"}`, true)
	if code >= 300 {
		t.Fatalf("setting the secret: %d %v", code, out)
	}
}

// The headline, and the thing people get wrong about Azure: Contributor
// can do anything to the vault RESOURCE and cannot read a secret out of
// it. Actions and dataActions are separate axes and do not mix.
func TestAContributorCannotReadASecret(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")
	host, id := makeVault(t, srv, "kv-data")
	setSecretAsDefault(t, srv, host, "db", "hunter2")
	assign(t, srv, id, roleContrib, appPrincipal)

	// It may read the vault through ARM...
	if code, body := asPrincipal(t, srv, appPrincipal, http.MethodGet, id, nil); code >= 300 {
		t.Fatalf("Contributor cannot read the vault resource: %d %v", code, body)
	}
	// ...and may not read what is in it.
	code, out := vaultAs(t, srv, host, appPrincipal, http.MethodGet, "/secrets/db?api-version=7.4", "")
	if code != 403 {
		t.Fatalf("Contributor reading a secret = %d %v, want 403", code, out)
	}
}

// And the role that exists for exactly this.
func TestKeyVaultSecretsUserCanReadASecret(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")
	host, id := makeVault(t, srv, "kv-data")
	setSecretAsDefault(t, srv, host, "db", "hunter2")
	assign(t, srv, id, roleKVSecretsUser, appPrincipal)

	code, out := vaultAs(t, srv, host, appPrincipal, http.MethodGet, "/secrets/db?api-version=7.4", "")
	if code != 200 {
		t.Fatalf("Key Vault Secrets User reading a secret = %d %v, want 200", code, out)
	}
	if out["value"] != "hunter2" {
		t.Errorf("the secret read back as %v", out["value"])
	}
}

// Reading is not writing. Secrets User holds getSecret and
// readMetadata, and nothing that sets one.
func TestKeyVaultSecretsUserCannotWriteASecret(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")
	host, id := makeVault(t, srv, "kv-data")
	assign(t, srv, id, roleKVSecretsUser, appPrincipal)

	code, out := vaultAs(t, srv, host, appPrincipal, http.MethodPut,
		"/secrets/new?api-version=7.4", `{"value":"x"}`)
	if code != 403 {
		t.Errorf("Secrets User writing a secret = %d %v, want 403", code, out)
	}
}

func TestKeyVaultSecretsOfficerCanWriteASecret(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")
	host, id := makeVault(t, srv, "kv-data")
	assign(t, srv, id, roleKVSecretsOfficer, appPrincipal)

	code, out := vaultAs(t, srv, host, appPrincipal, http.MethodPut,
		"/secrets/new?api-version=7.4", `{"value":"x"}`)
	if code >= 300 {
		t.Errorf("Secrets Officer writing a secret = %d %v", code, out)
	}
}

// Key Vault Reader is the role whose whole point is the distinction:
// metadata yes, values no. If listing and reading were one action it
// could not exist.
func TestKeyVaultReaderSeesMetadataButNotValues(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")
	host, id := makeVault(t, srv, "kv-data")
	setSecretAsDefault(t, srv, host, "db", "hunter2")
	assign(t, srv, id, roleKVReader, appPrincipal)

	if code, out := vaultAs(t, srv, host, appPrincipal, http.MethodGet,
		"/secrets?api-version=7.4", ""); code != 200 {
		t.Errorf("Key Vault Reader listing secrets = %d %v, want 200", code, out)
	}
	if code, _ := vaultAs(t, srv, host, appPrincipal, http.MethodGet,
		"/secrets/db?api-version=7.4", ""); code != 403 {
		t.Errorf("Key Vault Reader reading a secret VALUE = %d, want 403", code)
	}
}

// An assignment at the resource group reaches the vault's data plane
// too: a data action is scoped exactly as a control-plane one is.
func TestADataActionInheritsFromTheResourceGroup(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")
	host, _ := makeVault(t, srv, "kv-data")
	setSecretAsDefault(t, srv, host, "db", "hunter2")
	assign(t, srv, rgPath("rg-kv"), roleKVSecretsUser, appPrincipal)

	if code, out := vaultAs(t, srv, host, appPrincipal, http.MethodGet,
		"/secrets/db?api-version=7.4", ""); code != 200 {
		t.Errorf("an assignment at the resource group = %d %v, want 200", code, out)
	}
}

// ...and an assignment on a DIFFERENT vault does not.
func TestADataActionDoesNotReachAnotherVault(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")
	host, id := makeVault(t, srv, "kv-data")
	other, _ := makeVault(t, srv, "kv-other")
	setSecretAsDefault(t, srv, host, "db", "hunter2")
	setSecretAsDefault(t, srv, other, "db", "hunter2")
	assign(t, srv, id, roleKVSecretsUser, appPrincipal)

	if code, _ := vaultAs(t, srv, other, appPrincipal, http.MethodGet,
		"/secrets/db?api-version=7.4", ""); code != 403 {
		t.Errorf("reading the other vault's secret = %d, want 403", code)
	}
}

// The refusal has to be Key Vault's own shape, with a reason. A client
// that cannot recognise a denial retries it.
func TestADeniedDataActionIsAKeyVaultForbidden(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")
	host, id := makeVault(t, srv, "kv-data")
	setSecretAsDefault(t, srv, host, "db", "hunter2")
	assign(t, srv, id, roleContrib, appPrincipal)

	code, out := vaultAs(t, srv, host, appPrincipal, http.MethodGet, "/secrets/db?api-version=7.4", "")
	if code != 403 {
		t.Fatalf("= %d, want 403", code)
	}
	errObj, _ := out["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("the refusal is not a Key Vault error document: %v", out)
	}
	if errObj["code"] != "Forbidden" {
		t.Errorf("the code is %v, want Forbidden", errObj["code"])
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "getSecret") {
		t.Errorf("the message does not name the data action: %q", msg)
	}
}

// The challenge must still come first. A request with no token at all
// gets the 401 that starts the dance -- not a 403, which would tell a
// client it had been refused rather than that it should authenticate,
// and would break every Key Vault SDK.
func TestTheChallengeStillComesBeforeEnforcement(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")
	host, _ := makeVault(t, srv, "kv-data")

	code, _, header := vaultCall(t, srv, host, http.MethodGet, "/secrets/db?api-version=7.4", "", false)
	if code != 401 {
		t.Fatalf("an unauthenticated data-plane request = %d, want 401", code)
	}
	if !strings.Contains(header.Get("WWW-Authenticate"), "authorization=") {
		t.Errorf("the challenge is missing: %q", header.Get("WWW-Authenticate"))
	}
}

// With the flag off nothing is refused, including a data action -- the
// azure-storage example and the SDK harness depend on it.
func TestWithEnforcementOffADataActionIsNotRefused(t *testing.T) {
	srv, _ := newTestServer(t)
	host, _ := makeVault(t, srv, "kv-data")
	setSecretAsDefault(t, srv, host, "db", "hunter2")

	if code, out := vaultAs(t, srv, host, appPrincipal, http.MethodGet,
		"/secrets/db?api-version=7.4", ""); code != 200 {
		t.Errorf("a data action with enforcement off = %d %v, want 200", code, out)
	}
}

// The unrestricted default identity reaches the data plane too, which is
// what keeps the existing azure-app example applying with the flag on.
func TestTheDefaultIdentityCanReadASecret(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")
	host, _ := makeVault(t, srv, "kv-data")
	setSecretAsDefault(t, srv, host, "db", "hunter2")

	oid := principalFromClientID(DefaultClientID)
	if code, out := vaultAs(t, srv, host, oid, http.MethodGet,
		"/secrets/db?api-version=7.4", ""); code != 200 {
		t.Errorf("the default identity = %d %v, want 200", code, out)
	}
}

// Certificate contacts is the call azurerm makes right after reading a
// vault, and it tolerates exactly 404 ContactsNotFound. A 403 there
// crashes its Key Vault client, so a principal that can read the vault
// must not be refused this.
func TestCertificateContactsIsNotRefusedForAVaultReader(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")
	host, id := makeVault(t, srv, "kv-data")
	assign(t, srv, id, roleKVReader, appPrincipal)

	code, _ := vaultAs(t, srv, host, appPrincipal, http.MethodGet,
		"/certificates/contacts?api-version=7.4", "")
	if code == 403 {
		t.Error("a vault reader was refused certificate contacts, which crashes azurerm")
	}
}

// The explanation must use the SAME axis the enforcement does. Asked
// whether a Contributor may read a secret, a control-plane evaluation
// says yes -- Contributor holds Actions ["*"] -- which is exactly the
// confusion dataActions remove. An explainer that said yes while the
// server said no would teach the thing the enforcement just stopped.
func TestTheExplainerAgreesWithTheDataPlane(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")
	host, id := makeVault(t, srv, "kv-data")
	setSecretAsDefault(t, srv, host, "db", "hunter2")
	assign(t, srv, id, roleContrib, appPrincipal)
	az := serverOf(t, srv)

	const getSecret = "Microsoft.KeyVault/vaults/secrets/getSecret/action"
	check := az.ExplainOne(appPrincipal, getSecret, id)
	if check.Allowed {
		t.Errorf("the explainer says a Contributor may read a secret: %s", check.Reason)
	}
	// ...and the request agrees.
	if code, _ := vaultAs(t, srv, host, appPrincipal, http.MethodGet,
		"/secrets/db?api-version=7.4", ""); code != 403 {
		t.Errorf("the request was not refused: %d", code)
	}

	// The control-plane action on the same vault IS allowed, which is
	// what makes the pair worth asserting together.
	if c := az.ExplainOne(appPrincipal, "Microsoft.KeyVault/vaults/read", id); !c.Allowed {
		t.Errorf("a Contributor cannot read the vault resource: %s", c.Reason)
	}
}

// A vault's report shows both axes, because its control-plane
// permissions say nothing about what is inside it -- the same reason a
// bucket's own permissions are not the interesting ones.
func TestAVaultsReportCoversItsDataActions(t *testing.T) {
	srv, _ := newEnforcingTestServer(t, "")
	_, id := makeVault(t, srv, "kv-data")
	assign(t, srv, id, roleKVSecretsUser, appPrincipal)
	az := serverOf(t, srv)

	report := az.ExplainAccess(appPrincipal, id, typeKeyVault)
	var sawGet, sawWrite bool
	for _, c := range report.Checks {
		switch {
		case strings.Contains(c.Action, "getSecret"):
			sawGet = true
			if !c.Allowed {
				t.Errorf("Secrets User cannot getSecret: %s", c.Reason)
			}
		case strings.Contains(c.Action, "setSecret"):
			sawWrite = true
			if c.Allowed {
				t.Error("Secrets User was reported able to setSecret")
			}
		}
	}
	if !sawGet || !sawWrite {
		t.Errorf("the report does not cover the data actions: %+v", report.Checks)
	}
}

// Every data action the enforcement point can require must be one the
// explainer recognises. They are derived from the same function, so
// this pins that they stay derived rather than being listed twice.
func TestEveryDataActionIsRecognisedByTheExplainer(t *testing.T) {
	actions := keyvault.DataActions()
	if len(actions) < 5 {
		t.Fatalf("expected the data plane's actions, got %v", actions)
	}
	for _, a := range actions {
		if !isDataAction(a) {
			t.Errorf("%s is required by the data plane and unknown to the explainer", a)
		}
	}
	// ...and a control-plane action is not mistaken for one.
	if isDataAction("Microsoft.KeyVault/vaults/delete") {
		t.Error("a control-plane delete was classified as a data action")
	}
}
