package docker

import (
	"strings"
	"testing"
)

// addHosts pulls every --add-host value out of a command line.
func addHosts(args []string) []string {
	var out []string
	for i, a := range args {
		if a == "--add-host" && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

func envOf(args []string, name string) []string {
	var out []string
	for i, a := range args {
		if a == "-e" && i+1 < len(args) && strings.HasPrefix(args[i+1], name+"=") {
			out = append(out, strings.TrimPrefix(args[i+1], name+"="))
		}
	}
	return out
}

// An ADDRESSABLE data plane cannot be reached from a container without
// help, and this is the help.
//
// Azure's blob endpoint is a hostname -- {account}.blob.storage.nimbus
// .localhost -- and the SDK builds that URL from the account name, so it
// cannot be told to connect somewhere else. Nested *.localhost resolves to
// 127.0.0.1, which inside a container is the CONTAINER. Measured from
// inside a function app container before this existed:
//
//	stxcloudnimbus.blob.storage.nimbus.localhost -> 127.0.0.1
//	URLError [Errno 111] Connection refused
//
// A pod could be fixed by hand with hostAliases, which is how the real
// azure-storage-blob SDK was first proven to work at all; a Lambda or a
// Container App could not, because Nimbus builds the docker command line
// and nothing exposed it.
func TestAContainerResolvesTheAddressableDataPlanes(t *testing.T) {
	args := runArgs(RunOptions{
		Name:  "nimbus-test-1",
		Image: "alpine",
		HostAliases: []string{
			"st1.blob.storage.nimbus.localhost",
			"kv1.vault.nimbus.localhost",
		},
	})

	got := strings.Join(addHosts(args), " ")
	for _, want := range []string{
		// Still unconditional, and still first.
		"host.docker.internal:host-gateway",
		"st1.blob.storage.nimbus.localhost:host-gateway",
		"kv1.vault.nimbus.localhost:host-gateway",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q; --add-host values were %q", want, got)
		}
	}
}

// The sidecar exclusion has to cover the aliases too, or it is no
// exclusion at all: docker refuses ANY --add-host together with
// `--network container:`, with "conflicting options: custom host-to-IP
// mapping and the network mode". A sidecar needs none of them -- it has
// the main container's /etc/hosts, which was written with the full list.
func TestASidecarGetsNoAliasesEither(t *testing.T) {
	args := runArgs(RunOptions{
		Name:        "nimbus-app-redis",
		Image:       "redis:alpine",
		Network:     "container:nimbus-app-web",
		HostAliases: []string{"st1.blob.storage.nimbus.localhost"},
	})

	if got := addHosts(args); len(got) != 0 {
		t.Fatalf("a shared namespace got --add-host %q; docker refuses the combination", got)
	}
}

// Aliasing the hostname is only half of reaching Azure: it is served over
// TLS by a CA that is trusted on the MACHINE and is in no container's
// trust store. Fixing only the name would move the failure from "cannot
// resolve" to "certificate verify failed" -- which is this project's
// recurring trap of making something fail in a new place instead of work.
func TestACAIsMountedAndPointedAt(t *testing.T) {
	args := runArgs(RunOptions{
		Name:   "nimbus-test-1",
		Image:  "alpine",
		CAFile: "/home/u/.nimbus/certs/nimbus-ca.pem",
	})

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-v /home/u/.nimbus/certs/nimbus-ca.pem:"+CAPath+":ro") {
		t.Errorf("the CA is not mounted read-only: %v", args)
	}
	// Go reads SSL_CERT_FILE, Python's requests reads REQUESTS_CA_BUNDLE,
	// and a client that finds neither fails verification rather than
	// falling back to anything.
	for _, name := range []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE"} {
		if got := envOf(args, name); len(got) != 1 || got[0] != CAPath {
			t.Errorf("%s = %v, want [%s]", name, got, CAPath)
		}
	}
}

// A caller that has its own bundle is not second-guessed. docker takes the
// LAST -e for a name, so the CA environment has to be appended before
// opts.Env -- and if that order is ever reversed, a handler given a
// deliberate value would silently get ours instead.
func TestACallersOwnCABundleWins(t *testing.T) {
	args := runArgs(RunOptions{
		Name:   "nimbus-test-1",
		Image:  "alpine",
		CAFile: "/ca.pem",
		Env:    map[string]string{"REQUESTS_CA_BUNDLE": "/their/own.pem"},
	})

	got := envOf(args, "REQUESTS_CA_BUNDLE")
	if len(got) == 0 || got[len(got)-1] != "/their/own.pem" {
		t.Errorf("REQUESTS_CA_BUNDLE resolved to %v, want the caller's /their/own.pem last", got)
	}
}

// No provider means no change at all, which is what keeps every existing
// test and every cloud that needs none of this exactly as it was.
func TestWithNoProviderNothingIsAdded(t *testing.T) {
	SetHostExtras(nil)
	got := hostExtras()
	if len(got.Aliases) != 0 || got.CAFile != "" {
		t.Errorf("hostExtras() = %+v with no provider, want empty", got)
	}
}

// The names come from walking a store, which hands them back in map order,
// so they are sorted and de-duplicated here. Without it the same container
// would be started with the flags in a different order each time, and any
// assertion about them would be a coin toss.
//
// host.docker.internal is dropped because it is already passed
// unconditionally and docker refuses a duplicate --add-host.
func TestAliasesAreSortedAndDeduplicated(t *testing.T) {
	t.Cleanup(func() { SetHostExtras(nil) })
	SetHostExtras(func() HostExtras {
		return HostExtras{Aliases: []string{
			"zz.vault.nimbus.localhost",
			"aa.blob.storage.nimbus.localhost",
			"zz.vault.nimbus.localhost",
			"host.docker.internal",
			"",
		}}
	})

	got := hostExtras().Aliases
	want := []string{"aa.blob.storage.nimbus.localhost", "zz.vault.nimbus.localhost"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

// The provider is consulted by Run, so no engine has to remember it.
//
// This is the assertion that would have caught Milestone 20's missed
// swap: a facility that works and is called by nobody passes its own unit
// test perfectly. Run is where the wiring is, so this asserts there.
func TestRunConsultsTheProvider(t *testing.T) {
	t.Cleanup(func() { SetHostExtras(nil) })
	SetHostExtras(func() HostExtras {
		return HostExtras{
			Aliases: []string{"st1.blob.storage.nimbus.localhost"},
			CAFile:  "/ca.pem",
		}
	})

	// runArgsFor is the line Run passes to docker, so this asserts about
	// the real path rather than rebuilding it.
	c := &CLI{}
	joined := strings.Join(c.runArgsFor(RunOptions{Name: "nimbus-test-1", Image: "alpine"}), " ")

	if !strings.Contains(joined, "st1.blob.storage.nimbus.localhost:host-gateway") {
		t.Errorf("the provider's names did not reach the command line: %s", joined)
	}
	if !strings.Contains(joined, "-v /ca.pem:"+CAPath+":ro") {
		t.Errorf("the provider's CA did not reach the command line: %s", joined)
	}
}

// Nothing a caller set is overwritten by the provider: the two are added
// together, so an engine that one day needs a name of its own keeps it.
func TestTheProviderAddsToWhatACallerAskedFor(t *testing.T) {
	t.Cleanup(func() { SetHostExtras(nil) })
	SetHostExtras(func() HostExtras {
		return HostExtras{Aliases: []string{"st1.blob.storage.nimbus.localhost"}}
	})

	c := &CLI{}
	got := strings.Join(addHosts(c.runArgsFor(RunOptions{
		Name: "nimbus-test-1", Image: "alpine",
		HostAliases: []string{"something.a.caller.wanted"},
	})), " ")

	for _, want := range []string{"something.a.caller.wanted", "st1.blob.storage.nimbus.localhost"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}
