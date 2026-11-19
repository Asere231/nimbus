package azure

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Asere231/nimbus/internal/engines/run"
)

// The ARM surface azurerm actually exercises for a function app.
//
// Every assertion here was found by running `terraform apply` against
// Nimbus and reading what the provider refused. None of it is in the
// Microsoft specs in a form that would have predicted it, which is why
// this project treats real clients as the spec.

// --- the statuses azurerm insists on --------------------------------------

// A service plan's create answers 202, and a function app's does too.
//
// Both were measured by watching azurerm refuse the 201 outright:
//
//	performing CreateOrUpdate: unexpected status 201 (201 Created)
//
// which is the generated-long-running-client trap Hook.CreateStatus exists
// for, and which storage accounts hit first. Nothing in the body is wrong
// when this happens -- the provider quotes the whole correct resource back
// at you -- so it reads as a content problem and is not one.
func TestWebCreatesAnswer202(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	base := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg +
		"/providers/Microsoft.Web/"

	for _, c := range []struct{ path, body string }{
		{"serverfarms/asp1", `{"location":"eastus","sku":{"name":"Y1"}}`},
		{"sites/fn3", `{"location":"eastus","kind":"functionapp,linux",
			"properties":{"siteConfig":{"linuxFxVersion":"PYTHON|3.11"}}}`},
	} {
		req := httptest.NewRequest(http.MethodPut,
			base+c.path+"?api-version=2024-11-01", strings.NewReader(c.body))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != 202 {
			t.Errorf("PUT %s = %d, want 202 -- azurerm refuses anything else: %s",
				c.path, rec.Code, rec.Body)
		}
	}
}

// A site's config child answers 200 and carries NO provisioningState.
//
// The generic write fills properties.provisioningState for every type,
// which is right for a resource and wrong here: a config child's
// `properties` is often a DICTIONARY, so one extra string key makes the
// whole document fail to unmarshal in the client --
//
//	json: cannot unmarshal string into Go struct field
//	AzureStoragePropertyDictionaryResource.properties of type
//	webapps.AzureStorageInfoValue
//
// -- and azurerm refuses the 201 on this path as well.
func TestASiteConfigChildAnswers200WithNoProvisioningState(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	site := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg +
		"/providers/Microsoft.Web/sites/fn4"
	putResource(t, s, site, `{"location":"eastus","kind":"functionapp,linux",
		"properties":{"siteConfig":{"linuxFxVersion":"PYTHON|3.11"}}}`)

	req := httptest.NewRequest(http.MethodPut,
		site+"/config/azurestorageaccounts?api-version=2024-11-01",
		strings.NewReader(`{"properties":{}}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("PUT config/azurestorageaccounts = %d, want 200: %s", rec.Code, rec.Body)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	props, _ := doc["properties"].(map[string]any)
	if _, found := props["provisioningState"]; found {
		t.Errorf("a config child reports a provisioningState; its properties is a "+
			"dictionary and the extra key breaks the client's unmarshal: %v", doc)
	}
}

// The publishing-credential policies answer, because azurerm reads them
// straight after the create and a 404 fails the apply with "retrieving
// state of FTP Basic Auth for App Service".
func TestPublishingCredentialPoliciesAnswer(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	site := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg +
		"/providers/Microsoft.Web/sites/fn5"
	putResource(t, s, site, `{"location":"eastus","kind":"functionapp,linux",
		"properties":{"siteConfig":{"linuxFxVersion":"PYTHON|3.11"}}}`)

	for _, which := range []string{"ftp", "scm"} {
		req := httptest.NewRequest(http.MethodGet,
			site+"/basicPublishingCredentialsPolicies/"+which+"?api-version=2024-11-01", nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("GET basicPublishingCredentialsPolicies/%s = %d, want 200: %s",
				which, rec.Code, rec.Body)
		}
	}
}

// --- the deployment site --------------------------------------------------

// A site reports a SECOND hostname whose hostType is Repository.
//
// That is how azurerm finds the deployment site, and without one the
// provider refuses before sending anything:
//
//	could not determine SCM Site name for Site App Service ... for Zip Deployment
func TestASiteReportsItsDeploymentHostname(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	site := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg +
		"/providers/Microsoft.Web/sites/fn6"
	putResource(t, s, site, `{"location":"eastus","kind":"functionapp,linux",
		"properties":{"siteConfig":{"linuxFxVersion":"PYTHON|3.11"}}}`)

	req := httptest.NewRequest(http.MethodGet, site+"?api-version=2024-11-01", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	props, _ := doc["properties"].(map[string]any)
	states, _ := props["hostNameSslStates"].([]any)

	var repo string
	for _, raw := range states {
		e, _ := raw.(map[string]any)
		if e["hostType"] == "Repository" {
			repo, _ = e["name"].(string)
		}
	}
	if repo == "" {
		t.Fatalf("no Repository hostname; azurerm cannot find the deployment site: %v", states)
	}
	if !strings.Contains(repo, ".scm.") {
		t.Errorf("the deployment hostname is not an scm name: %q", repo)
	}
}

// The deployment hostname is TWO labels deep, so it needs its own
// wildcard: "*.azurewebsites.nimbus.localhost" does not cover
// "fn.scm.azurewebsites.nimbus.localhost". The same rule that gives the
// storage data plane one SAN per service level.
func TestTheCertificateCoversTheDeploymentSite(t *testing.T) {
	want := "*." + ScmSuffix
	for _, n := range DNSNames() {
		if n == want {
			return
		}
	}
	t.Errorf("%q is not in the certificate's names, so a zip deploy cannot verify TLS: %v",
		want, DNSNames())
}

// The deployment site implements zipdeploy and REFUSES everything else.
//
// Kudu's real surface is enormous -- a command runner, a file browser, a
// process explorer -- and none of it is needed to deploy code. Answering
// an unimplemented path with something success-shaped is how a client
// comes to believe it did something it did not.
func TestTheDeploymentSiteRefusesWhatItDoesNotImplement(t *testing.T) {
	s := newTestServerForFunctions(t)
	// Stand the app in as running, which is what the engine's Put is for.
	s.funcapps.Put(&run.Service{
		ResourceID: "/subscriptions/x/sites/fn7", Name: "fn7",
		Container: "nimbus-fnapp-fn7-abc", Port: 9700,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/command", strings.NewReader(`{}`))
	req.Host = "fn7." + s.scmSuffix()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code == 200 {
		t.Error("an unimplemented Kudu endpoint answered 200; a client would believe it ran")
	}
	if !strings.Contains(rec.Body.String(), "zipdeploy") {
		t.Errorf("the refusal does not say what IS implemented: %s", rec.Body)
	}
}

// A zip with no vendored dependencies is refused BY THE DEPLOY, so the
// apply fails rather than the first invocation.
func TestAZipDeployWithoutDependenciesIsRefused(t *testing.T) {
	s := newTestServerForFunctions(t)
	s.funcapps.Put(&run.Service{
		ResourceID: "/subscriptions/x/sites/fn8", Name: "fn8",
		Container: "nimbus-fnapp-fn8-abc", Port: 9701,
	})

	archive := zipOf(t, map[string]string{
		"host.json":       `{"version":"2.0"}`,
		"function_app.py": "import azure.functions as func\n",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/zipdeploy", bytes.NewReader(archive))
	req.Host = "fn8." + s.scmSuffix()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code == 200 {
		t.Fatal("a deploy with no vendored dependencies succeeded; every invocation would fail")
	}
	if !strings.Contains(rec.Body.String(), ".python_packages") {
		t.Errorf("the refusal does not say what is missing: %s", rec.Body)
	}
}

// azurerm PATCHes a site's config on UPDATE, and a config view has
// nothing stored to patch.
//
// `put(..., patch: true)` refuses when nothing exists, which is right for
// an ordinary resource -- you cannot patch what is not there -- and wrong
// for these: in ARM a site's `config/web` exists as soon as the site
// does. So an update of a function app failed with
//
//	updating Site Config for Linux App Service ...: unexpected status 404
//	(404 Not Found) ... 'Microsoft.Web/sites/config/web' was not found
//
// Found by adding app settings to an EXISTING function app, which is the
// first time anything exercised the update path: create, plan, restart
// and destroy were all covered and none of them PATCHes.
func TestPatchingASiteConfigCreatesIt(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	site := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg +
		"/providers/Microsoft.Web/sites/fn9"
	putResource(t, s, site, `{"location":"eastus","kind":"functionapp,linux",
		"properties":{"siteConfig":{"linuxFxVersion":"PYTHON|3.11"}}}`)

	// Nothing has ever been written to config/web: azurerm PATCHes it.
	req := httptest.NewRequest(http.MethodPatch,
		site+"/config/web?api-version=2024-11-01",
		strings.NewReader(`{"properties":{"alwaysOn":false}}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code == 404 {
		t.Fatalf("PATCH of a site config 404s, so updating a function app fails: %s", rec.Body)
	}
	if rec.Code != 200 {
		t.Fatalf("PATCH config/web = %d, want 200: %s", rec.Code, rec.Body)
	}
}
