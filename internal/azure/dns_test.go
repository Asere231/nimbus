package azure

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"strings"
	"testing"

	"github.com/Asere231/nimbus/internal/core"
)

// Azure's data-plane names are passed to core.EnsureCerts by cmd/nimbus, and
// a client reaching {account}.blob.{suffix} needs every level covered. A
// wildcard matches exactly ONE label, so "*.storage.nimbus.localhost" does
// NOT cover "st1.blob.storage.nimbus.localhost" -- each level has its own.
func TestCertificateCoversEveryAzureDataPlaneName(t *testing.T) {
	certs, err := core.EnsureCerts(t.TempDir(), DNSNames()...)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(certs.Cert)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatal("the server certificate is not PEM")
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}

	for _, host := range []string{
		"localhost",
		"storage.nimbus.localhost",
		"st1.blob.storage.nimbus.localhost",
		"st1.queue.storage.nimbus.localhost",
		"st1.table.storage.nimbus.localhost",
		"st1.file.storage.nimbus.localhost",
		"kv1.vault.nimbus.localhost",
	} {
		if err := crt.VerifyHostname(host); err != nil {
			t.Errorf("certificate does not cover %s: %v\n  SANs: %v", host, err, crt.DNSNames)
		}
	}
}

// hostSet is DataPlaneHosts as a set, for readable assertions.
func hostSet(t *testing.T, s *Server) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, h := range s.DataPlaneHosts() {
		out[h] = true
	}
	return out
}

// Every addressable data plane contributes a CONCRETE name, because that
// is what /etc/hosts holds: DNSNames is wildcards, which a certificate
// accepts and a hosts file does not.
//
// Without these a container cannot reach Azure at all. The account
// hostname resolves to 127.0.0.1, which inside a container is the
// container -- measured from inside a function app container before this
// existed:
//
//	stxcloudnimbus.blob.storage.nimbus.localhost -> 127.0.0.1
//	URLError [Errno 111] Connection refused
func TestEveryAddressableDataPlaneGetsAConcreteHostname(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	base := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg + "/providers/"

	putResource(t, s, base+"Microsoft.Storage/storageAccounts/st1",
		`{"location":"eastus","sku":{"name":"Standard_LRS"},"kind":"StorageV2","properties":{}}`)
	putResource(t, s, base+"Microsoft.KeyVault/vaults/kv1",
		`{"location":"eastus","properties":{"tenantId":"`+DefaultTenantID+
			`","sku":{"family":"A","name":"standard"}}}`)

	got := hostSet(t, s)

	// A wildcard matches exactly one label, which is why each storage
	// service level is its own name rather than one entry for the account.
	for _, want := range []string{
		"st1.blob." + StorageSuffix,
		"st1.queue." + StorageSuffix,
		"st1.table." + StorageSuffix,
		"kv1." + KeyVaultSuffix,
	} {
		if !got[want] {
			t.Errorf("missing %q from %v", want, s.DataPlaneHosts())
		}
	}

	// And nothing may carry a port: the metadata document's suffixes do,
	// deliberately, which is exactly why they cannot be reused here.
	for _, h := range s.DataPlaneHosts() {
		if strings.Contains(h, ":") {
			t.Errorf("%q carries a port; /etc/hosts holds hostnames", h)
		}
	}
}

// A function app has TWO hostnames, and the deployment site is the one a
// publish goes to. azurerm finds it by scanning hostNameSslStates for the
// Repository entry and refuses to deploy without one.
func TestAFunctionAppContributesItsDeploymentSiteToo(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	base := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg + "/providers/"

	putResource(t, s, base+"Microsoft.Web/serverfarms/asp1",
		`{"location":"eastus","sku":{"name":"Y1"},"kind":"linux","properties":{"reserved":true}}`)
	putResource(t, s, base+"Microsoft.Web/sites/fn1",
		`{"location":"eastus","kind":"functionapp,linux","properties":{"serverFarmId":"`+
			base+`Microsoft.Web/serverfarms/asp1"}}`)

	got := hostSet(t, s)
	for _, want := range []string{"fn1." + FunctionsSuffix, "fn1." + ScmSuffix} {
		if !got[want] {
			t.Errorf("missing %q from %v", want, s.DataPlaneHosts())
		}
	}
}

// A container app's fqdn is READ BACK rather than recomposed, because an
// app with no ingress block has none -- as in real Azure -- and inventing
// one here would disagree with what the client was told. So an app with no
// fqdn contributes no name rather than a half-formed one.
func TestAContainerAppWithNoIngressContributesNoName(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	base := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg + "/providers/"

	putResource(t, s, base+"Microsoft.App/containerApps/app1",
		`{"location":"eastus","properties":{"template":{"containers":`+
			`[{"name":"c","image":"nginx:alpine"}]}}}`)

	for _, h := range s.DataPlaneHosts() {
		if strings.Contains(h, "app1") {
			t.Errorf("an app with no ingress contributed %q", h)
		}
	}
}

// Nothing is contributed by a server holding nothing addressable, which is
// what keeps every container's command line unchanged until there is
// something for it to reach. A subscription and a resource group exist
// here and neither is addressable.
func TestNothingAddressableContributesNoNames(t *testing.T) {
	s, _ := newTestServerWithGroup(t)
	if got := s.DataPlaneHosts(); len(got) != 0 {
		t.Errorf("DataPlaneHosts() = %v with nothing addressable, want none", got)
	}
}

// A container app WITH ingress contributes its fqdn, and WITHOUT the port.
//
// This is the case the test above did not cover, and CI found the bug for
// me. `TestEveryAddressableDataPlaneGetsAConcreteHostname` already
// asserted that no name carries a colon -- but it only created a storage
// account and a vault, whose suffixes are constants, and the container
// app test asserted the ABSENCE of a name. So the one source that really
// does carry a port was never exercised by either, and docker refused the
// flag outright:
//
//	invalid IP address in add-host: "4701:host-gateway"
//
// A container app's fqdn is read back from a body where the port is
// already baked in -- the suffix carries it deliberately, as storage's
// and keyVaultDns's do.
func TestAContainerAppsFQDNContributesItsHostWithoutThePort(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	base := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg + "/providers/"

	putResource(t, s, base+"Microsoft.App/managedEnvironments/env1", `{"location":"eastus"}`)
	putResource(t, s, base+"Microsoft.App/containerApps/app1",
		`{"location":"eastus","properties":{"managedEnvironmentId":"`+base+
			`Microsoft.App/managedEnvironments/env1","configuration":{"ingress":`+
			`{"external":true,"targetPort":80}},"template":{"containers":`+
			`[{"name":"c","image":"nginx:alpine"}]}}}`)

	var found string
	for _, h := range s.DataPlaneHosts() {
		if strings.Contains(h, "app1") {
			found = h
		}
	}
	if found == "" {
		t.Fatalf("an app with ingress contributed no name: %v", s.DataPlaneHosts())
	}
	if strings.Contains(found, ":") {
		t.Errorf("%q carries a port; docker refuses it as an invalid IP address", found)
	}
	if !strings.HasPrefix(found, "app1."+ContainerAppsSuffix) {
		t.Errorf("fqdn host = %q, want it under %q", found, ContainerAppsSuffix)
	}
}

// hostOnly judges a NAME rather than parsing a URL, because these have no
// scheme. A name it cannot read contributes nothing: a wrong alias is
// worse than a missing one, since docker refuses the whole `docker run`
// and the resource fails to start.
func TestHostOnlyDropsAPortAndRefusesWhatItCannotRead(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"app1.containerapps.nimbus.localhost:4701", "app1.containerapps.nimbus.localhost"},
		{"st1.blob.storage.nimbus.localhost", "st1.blob.storage.nimbus.localhost"},
		{"", ""},
		{"::1", ""},       // more than one colon: not a shape this understands
		{"a:b:c", ""},     //
		{"host:", "host"}, // a trailing separator still names the host
	} {
		if got := hostOnly(tc.in); got != tc.want {
			t.Errorf("hostOnly(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
