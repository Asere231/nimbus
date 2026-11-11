package azure

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// Container Apps: the first Azure type that actually runs code.
//
// What is worth testing here is not that a container starts -- the `run`
// engine already has its own tests, and Azure is its second caller rather
// than its first. It is the fields azurerm READS BACK, because those are
// where this goes wrong silently: a value that differs between two reads
// is a permanent diff, and `properties.configuration.ingress.fqdn` is both
// read back and put in a Terraform output somebody curls.

const envPath = "/subscriptions/" + DefaultSubscriptionID +
	"/resourceGroups/rg/providers/Microsoft.App/managedEnvironments/env"

func appPath(name string) string {
	return "/subscriptions/" + DefaultSubscriptionID +
		"/resourceGroups/rg/providers/Microsoft.App/containerApps/" + name
}

// containerAppBody is the shape azurerm sends, reduced to the fields that
// matter here: the image to run and the environment it belongs to.
func containerAppBody(image string) map[string]any {
	return map[string]any{
		"location": "eastus",
		"properties": map[string]any{
			"managedEnvironmentId": "/subscriptions/" + DefaultSubscriptionID +
				"/resourceGroups/rg/providers/Microsoft.App/managedEnvironments/env",
			"configuration": map[string]any{
				"ingress": map[string]any{
					"external":   true,
					"targetPort": float64(80),
				},
			},
			"template": map[string]any{
				"containers": []any{map[string]any{
					"name":  "main",
					"image": image,
					"env": []any{
						map[string]any{"name": "GREETING", "value": "hello"},
					},
				}},
			},
		},
	}
}

// newAppServer is a test server whose Docker is a Fake that reports
// containers as started, so Provision completes without a daemon.
func newAppServer(t *testing.T) (*httptest.Server, *docker.Fake) {
	t.Helper()
	srv, _ := newTestServer(t)
	f := docker.NewFake()
	f.Images["nginx:alpine"] = true
	serverOf(t, srv).useDocker(f)
	return srv, f
}

func TestAContainerAppGetsAStableFQDN(t *testing.T) {
	srv, _ := newAppServer(t)
	mustPut(t, srv, rgPath("rg"), map[string]any{"location": "eastus"})
	mustPut(t, srv, envPath, map[string]any{"location": "eastus"})

	code, first := do(t, srv, http.MethodPut, appPath("web"), containerAppBody("nginx:alpine"))
	if code >= 300 {
		t.Fatalf("creating the app: %d %v", code, first)
	}
	fqdn := ingressFQDN(t, first)

	// One label for the app, then the shared suffix. Real Azure puts the
	// environment and the region in there too, but a TLS wildcard matches
	// exactly ONE label, so an <app>.<env>. shape would need a
	// certificate per environment. The suffix carries our port, as the
	// storage and Key Vault ones already do.
	if !strings.HasPrefix(fqdn, "web.") {
		t.Errorf("fqdn %q does not start with the app name", fqdn)
	}
	if !strings.Contains(fqdn, ContainerAppsSuffix) {
		t.Errorf("fqdn %q is not under %s", fqdn, ContainerAppsSuffix)
	}

	// The thing that makes this worth a test: a second read must agree.
	// azurerm puts the fqdn in state and in an output, so a value that
	// changes between reads is a diff that never settles.
	_, second := do(t, srv, http.MethodGet, appPath("web"), nil)
	if again := ingressFQDN(t, second); again != fqdn {
		t.Errorf("fqdn changed between reads: %q then %q", fqdn, again)
	}

	// And an update must not move it either.
	_, third := do(t, srv, http.MethodPut, appPath("web"), containerAppBody("nginx:alpine"))
	if again := ingressFQDN(t, third); again != fqdn {
		t.Errorf("fqdn changed after an update: %q then %q", fqdn, again)
	}
}

// Every read-only string the spec declares has to be filled, because
// spec.go deliberately fills only provisioningState, etags, GUIDs and
// empty arrays -- a read-only string falls through. An absent field that
// azurerm expects reads as an empty string in state and plans forever.
func TestAContainerAppFillsTheFieldsAzurermReadsBack(t *testing.T) {
	srv, _ := newAppServer(t)
	mustPut(t, srv, rgPath("rg"), map[string]any{"location": "eastus"})
	mustPut(t, srv, envPath, map[string]any{"location": "eastus"})
	_, body := do(t, srv, http.MethodPut, appPath("web"), containerAppBody("nginx:alpine"))

	props, _ := body["properties"].(map[string]any)
	if props == nil {
		t.Fatalf("no properties: %v", body)
	}
	for _, field := range []string{
		"latestRevisionName", "latestRevisionFqdn", "runningStatus",
		"eventStreamEndpoint", "customDomainVerificationId",
	} {
		if str(props[field]) == "" {
			t.Errorf("properties.%s is empty; azurerm reads it back", field)
		}
	}
	if _, ok := props["outboundIpAddresses"].([]any); !ok {
		t.Errorf("outboundIpAddresses is not a list: %v", props["outboundIpAddresses"])
	}
}

// The environment owns the shared domain, which is what an app's fqdn is
// built from -- so it has to be reported, and it has to be the same one.
func TestAnEnvironmentReportsItsDefaultDomain(t *testing.T) {
	srv, _ := newAppServer(t)
	mustPut(t, srv, rgPath("rg"), map[string]any{"location": "eastus"})
	code, body := do(t, srv, http.MethodPut, envPath, map[string]any{"location": "eastus"})
	if code >= 300 {
		t.Fatalf("creating the environment: %d %v", code, body)
	}
	props, _ := body["properties"].(map[string]any)
	// With OUR PORT on it. Real Azure's default domain never carries one,
	// but 443 needs root to bind, so every Azure data-plane suffix here
	// does -- storage and keyVaultDns already, and this makes three.
	want := serverOf(t, srv).containerAppsSuffix()
	if got := str(props["defaultDomain"]); got != want {
		t.Errorf("defaultDomain = %q, want %q", got, want)
	}
	if str(props["staticIp"]) == "" {
		t.Error("staticIp is empty")
	}
}

// A request to an app's hostname reaches the app, not ARM. Host-based
// routing is the pattern storage and Key Vault already use, and it has to
// be matched before anything looks at an ARM path.
func TestAnAppsHostnameRoutesToTheApp(t *testing.T) {
	srv, _ := newAppServer(t)
	s := serverOf(t, srv)
	mustPut(t, srv, rgPath("rg"), map[string]any{"location": "eastus"})
	mustPut(t, srv, envPath, map[string]any{"location": "eastus"})
	do(t, srv, http.MethodPut, appPath("web"), containerAppBody("nginx:alpine"))

	// The app's own hostname is an app request...
	if _, ok := parseAppHost("web."+s.containerAppsSuffix(), s.containerAppsSuffix()); !ok {
		t.Error("an app hostname was not recognised")
	}
	// ...and the plain host is not, or every ARM call would be shadowed.
	if _, ok := parseAppHost("localhost:4701", s.containerAppsSuffix()); ok {
		t.Error("the ARM host was taken for an app hostname")
	}
	// Two labels before the suffix is not an app either: one label is the
	// whole point, since the certificate can only cover one.
	if _, ok := parseAppHost("web.env."+s.containerAppsSuffix(), s.containerAppsSuffix()); ok {
		t.Error("a two-label name was taken for an app hostname")
	}
}

// The certificate has to cover the names clients will use, or every curl
// fails with a verification error that mentions nothing about Container
// Apps.
func TestTheCertificateCoversAppHostnames(t *testing.T) {
	names := DNSNames()
	for _, want := range []string{ContainerAppsSuffix, "*." + ContainerAppsSuffix} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("DNSNames() is missing %q", want)
		}
	}
}

// --- helpers ---------------------------------------------------------------

func ingressFQDN(t *testing.T, body map[string]any) string {
	t.Helper()
	props, _ := body["properties"].(map[string]any)
	cfg, _ := props["configuration"].(map[string]any)
	ingress, _ := cfg["ingress"].(map[string]any)
	fqdn := str(ingress["fqdn"])
	if fqdn == "" {
		raw, _ := json.Marshal(body)
		t.Fatalf("no ingress.fqdn in %s", raw)
	}
	return fqdn
}

func mustPut(t *testing.T, srv *httptest.Server, path string, body map[string]any) {
	t.Helper()
	if code, out := do(t, srv, http.MethodPut, path, body); code >= 300 {
		t.Fatalf("PUT %s: %d %v", path, code, out)
	}
}
