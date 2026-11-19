package azure

import (
	"encoding/json"
	"strings"

	"github.com/Asere231/nimbus/internal/core"
)

// The hostnames Azure's data planes live under.
//
// These are Azure's business, not the core's: a client builds
// {account}.{service}.{suffix} or {vault}.{suffix} from the metadata
// document and connects to it, so the names have to resolve to loopback and
// the server certificate has to cover them. Nested *.localhost names do
// resolve to loopback on macOS and Windows with nothing installed, which is
// why no DNS tooling is needed -- see CLAUDE.md.
//
// A TLS wildcard matches exactly ONE label, so each level needs its own
// entry: "*.storage.nimbus.localhost" does not cover
// "st1.blob.storage.nimbus.localhost".

// StorageSuffix is the DNS suffix the storage data plane lives under.
const StorageSuffix = "storage.nimbus.localhost"

// KeyVaultSuffix is the DNS suffix Key Vault's data plane lives under.
const KeyVaultSuffix = "vault.nimbus.localhost"

// DNSNames is every name a certificate needs for Azure's data planes.
// cmd/nimbus passes it to core.EnsureCerts when the azure cloud is running.
func DNSNames() []string {
	return []string{
		StorageSuffix,
		"*." + StorageSuffix,
		"*.blob." + StorageSuffix,
		"*.queue." + StorageSuffix,
		"*.table." + StorageSuffix,
		"*.file." + StorageSuffix,
		"*.dfs." + StorageSuffix,
		KeyVaultSuffix,
		"*." + KeyVaultSuffix,
		ContainerAppsSuffix,
		"*." + ContainerAppsSuffix,
		FunctionsSuffix,
		"*." + FunctionsSuffix,
		// The deployment (Kudu) site is TWO labels deep, so it needs its
		// own wildcard: the one above does not cover it.
		"*." + ScmSuffix,
	}
}

// DataPlaneHosts is every CONCRETE data-plane hostname that exists right
// now: the names above are wildcards, which a certificate accepts and
// /etc/hosts does not.
//
// A container cannot reach these otherwise. Nested *.localhost resolves to
// 127.0.0.1, which inside a container is the container, and an SDK builds
// its data-plane URL from the resource name and cannot be told to connect
// elsewhere -- so without aliasing these to the host gateway, the real
// Azure SDK could not reach blob storage from a Lambda or a Container App.
// cmd/nimbus hands this to docker.SetHostExtras.
//
// Carries no port, because /etc/hosts holds hostnames. The metadata
// document's suffixes DO carry our port, which is why they cannot be
// reused here.
//
// Two limits worth stating rather than hiding:
//   - A resource created AFTER a container started is not in that
//     container's /etc/hosts. Nothing can be done from here; the container
//     has to be replaced. In practice an apply creates the storage account
//     before the thing that uses it, which is the common case.
//   - A POD does not inherit its node's /etc/hosts, so this does not reach
//     one. See docs/CROSS-CLOUD.md for the hostAliases a pod needs.
func (s *Server) DataPlaneHosts() []string {
	var out []string
	// Every name goes through hostOnly, by construction rather than by
	// each case remembering. A container app's fqdn is read back from a
	// body where the port was already baked in -- the suffix carries it
	// deliberately, as storage's and keyVaultDns's do -- so one case
	// genuinely needs stripping, and the next one to need it will not
	// announce itself. CI found this with a flat refusal from docker:
	// `invalid IP address in add-host: "4701:host-gateway"`.
	add := func(names ...string) {
		for _, n := range names {
			if h := hostOnly(n); h != "" {
				out = append(out, h)
			}
		}
	}
	for _, r := range s.store.List(func(r *core.Resource) bool { return r.Cloud == "azure" }) {
		switch strings.ToLower(r.Type) {
		case strings.ToLower(typeStorageAccount):
			// Every service level is its own name, because a wildcard
			// matches one label and these are each a level deeper.
			for _, svc := range []string{"blob", "queue", "table", "file", "dfs"} {
				add(r.Name + "." + svc + "." + StorageSuffix)
			}
		case strings.ToLower(typeKeyVault):
			add(r.Name + "." + KeyVaultSuffix)
		case strings.ToLower(typeFunctionApp):
			// The deployment site is a second hostname, two labels deep.
			add(r.Name+"."+FunctionsSuffix, r.Name+"."+ScmSuffix)
		case strings.ToLower(typeContainerApp):
			// Read back rather than recomposed: an app with no ingress
			// block has no fqdn, as in real Azure, and inventing one
			// here would disagree with what the client was told.
			add(containerAppFQDN(r.Body))
		}
	}
	return out
}

// containerAppFQDN reads properties.configuration.ingress.fqdn, or "".
func containerAppFQDN(body []byte) string {
	var doc struct {
		Properties struct {
			Configuration struct {
				Ingress struct {
					FQDN string `json:"fqdn"`
				} `json:"ingress"`
			} `json:"configuration"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return ""
	}
	return doc.Properties.Configuration.Ingress.FQDN
}

// hostOnly drops a :port, because /etc/hosts holds hostnames and docker
// refuses anything else outright: `invalid IP address in add-host:
// "4701:host-gateway"`. Several Nimbus suffixes carry the port on
// purpose, so a name read back from a resource body may well have one.
//
// Judged as a NAME, not parsed as a URL: these have no scheme. The last
// colon is the port separator, and a bracketed IPv6 literal cannot occur
// here -- every one of these is a DNS name -- so a name with more than
// one colon is refused rather than guessed at.
func hostOnly(name string) string {
	if name == "" {
		return ""
	}
	i := strings.LastIndex(name, ":")
	if i < 0 {
		return name
	}
	if strings.Contains(name[:i], ":") {
		return "" // not a shape this understands; better nothing than wrong
	}
	return name[:i]
}
