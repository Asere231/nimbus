package docker

import (
	"sort"
	"sync"
)

// CAPath is where the Nimbus CA is mounted inside every container, when
// there is one. A fixed path rather than a per-cloud choice, so a handler
// or an image can name it without knowing which cloud started it.
const CAPath = "/etc/nimbus/nimbus-ca.pem"

// HostExtras is what a container needs in order to reach Nimbus for
// anything more than an IP address: NAMES, and a certificate authority.
//
// Both exist because of Azure, and neither is Azure-specific here -- an
// engine knows about no cloud, so this carries hostnames and a file path
// and nothing about what they mean.
type HostExtras struct {
	// Aliases are hostnames that must resolve to the machine running
	// Nimbus from inside a container.
	//
	// host.docker.internal is passed unconditionally and is not in this
	// list. These are the ADDRESSABLE data planes: Azure's blob, queue,
	// table and file endpoints, a Key Vault's vaultUri host, a function
	// app and its SCM site. Those are reached by hostname, and nested
	// *.localhost names resolve to 127.0.0.1 -- which inside a container
	// is the container, so the real Azure SDK could not reach blob
	// storage from a Lambda or a Container App at all.
	//
	// There is no wildcard: /etc/hosts holds names, not patterns, so the
	// list is the concrete names that exist when the container starts.
	Aliases []string
	// CAFile is a path on the HOST to the PEM the Azure endpoint's
	// certificate is signed by. It is mounted read-only at CAPath and
	// pointed at by the usual environment variables.
	CAFile string
}

// The provider is process-wide, set once at startup, and consulted before
// every `docker run`.
//
// A field on RunOptions or on CLI would be a per-cloud opt-in, and this
// package's own history says what that costs: --add-host was once going to
// be per-caller, every cloud that needed it had to remember, and the two
// that did not remember were red in CI for three milestones. Each engine
// builds its own docker.Client -- AWS alone builds four -- so there is no
// single client to hang this on either. Here it reaches every container
// from every cloud, including the next one somebody adds.
var (
	extrasMu sync.RWMutex
	extrasFn func() HostExtras
)

// SetHostExtras registers the provider. Passing nil clears it, which is
// what a test does when it is finished.
//
// The provider is called on every Run rather than its result being cached,
// because the set of names GROWS: a storage account created after a
// container started is not in that container's /etc/hosts, and the only
// thing that can be done about it is to be right for the next one. See
// docs/CROSS-CLOUD.md.
func SetHostExtras(fn func() HostExtras) {
	extrasMu.Lock()
	defer extrasMu.Unlock()
	extrasFn = fn
}

// hostExtras resolves the provider, sorted and de-duplicated so the
// command line is stable. A store walked for names hands them back in map
// order otherwise, which would make the flags differ between two runs of
// the same container and any assertion about them a coin toss.
func hostExtras() HostExtras {
	extrasMu.RLock()
	fn := extrasFn
	extrasMu.RUnlock()
	if fn == nil {
		return HostExtras{}
	}
	got := fn()

	seen := map[string]bool{
		// Already passed unconditionally; a provider that repeats it
		// would make docker refuse the duplicate.
		"host.docker.internal": true,
	}
	out := HostExtras{CAFile: got.CAFile}
	for _, name := range got.Aliases {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out.Aliases = append(out.Aliases, name)
	}
	sort.Strings(out.Aliases)
	return out
}

// caEnv is the environment every container gets when a CA is mounted, in a
// fixed order so the command line is stable.
//
// SSL_CERT_FILE and REQUESTS_CA_BUNDLE REPLACE the trust store rather than
// adding to it -- the trap that broke `az` in Milestone 5 -- and that is
// acceptable here and nowhere else: Nimbus is an offline emulator, the
// only TLS endpoint a container can reach is Nimbus itself, and a
// container that cannot verify it fails instead. NODE_EXTRA_CA_CERTS adds
// rather than replaces, which is simply how Node spells it.
//
// A caller's own value WINS, because these are appended before opts.Env
// and docker takes the last -e for a given name. A handler that has been
// given a bundle of its own is not second-guessed.
func caEnv() []string {
	return []string{
		"SSL_CERT_FILE",
		"REQUESTS_CA_BUNDLE",
		"CURL_CA_BUNDLE",
		"NODE_EXTRA_CA_CERTS",
	}
}
