package docker

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The flags being well formed is not the same as them WORKING, and until
// this test existed CI could only prove the first.
//
// `hostextras_test.go` asserts what Nimbus puts on the command line; this
// asserts that what it puts there has the effect it is for. The gap
// matters because `host-gateway` is Docker's own per-platform
// indirection: the whole mechanism was proven by hand on Docker DESKTOP,
// and every Docker-backed CI job runs on Docker ENGINE, which is exactly
// the divide that left the EKS and GKE jobs red for three milestones
// while a Mac said everything was fine.
//
// The assertion is deliberately NOT "the address is not 127.0.0.1". That
// is loose, and the right answer differs per platform -- 192.168.65.254
// on Docker Desktop, the bridge gateway on Engine -- so it would mean
// hard-coding one and skipping the other. Instead the alias must resolve
// to the SAME address as host.docker.internal, which Docker itself
// populates. That is exact, platform-independent, and says what is meant:
// this name points at the host. If host.docker.internal were itself
// wrong, that is a different and already-signposted bug (a cluster you
// just created answering 401).
func TestARealContainerResolvesAnAliasToTheHost(t *testing.T) {
	ctx, d, image := realDockerOrSkip(t)

	// A name shaped like the thing this exists for -- an Azure storage
	// data plane -- but belonging to no resource, so the test depends on
	// no cloud. The docker package knows about none.
	const alias = "stnimbustest.blob.storage.nimbus.localhost"

	t.Cleanup(func() { SetHostExtras(nil) })
	SetHostExtras(func() HostExtras { return HostExtras{Aliases: []string{alias}} })

	name := "nimbus-hostextras-resolve-test"
	_ = d.Remove(ctx, name)
	t.Cleanup(func() { _ = d.Remove(context.Background(), name) })

	// `sleep` rather than the image's own command: the container only has
	// to stay up long enough to be asked a question.
	if _, err := d.Run(ctx, RunOptions{
		Name:  name,
		Image: image,
		Args:  []string{"sleep", "120"},
	}); err != nil {
		t.Fatalf("starting the container: %v", err)
	}

	got := resolveIn(t, ctx, d, name, alias)
	want := resolveIn(t, ctx, d, name, "host.docker.internal")

	if len(want) == 0 {
		t.Fatal("host.docker.internal did not resolve inside the container; " +
			"--add-host host-gateway is not working at all here")
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s resolved to %v, want the same as host.docker.internal (%v)",
			alias, got, want)
	}
}

// The CA is the other half, and it is platform-specific in a way the
// aliases are not: a bind mount's source path is a host path, and this
// project has already been bitten by Windows and Unix disagreeing about
// those. A mounted-but-unreadable file starts the container perfectly
// happily, so nothing short of reading it inside proves the mount.
func TestARealContainerCanReadTheMountedCA(t *testing.T) {
	ctx, d, image := realDockerOrSkip(t)

	// Written here rather than reusing the machine's real CA, so the test
	// asserts about the mount and not about what happens to be installed.
	dir := t.TempDir()
	ca := filepath.Join(dir, "nimbus-ca.pem")
	const marker = "-----BEGIN CERTIFICATE----- nimbus-mount-test"
	if err := os.WriteFile(ca, []byte(marker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { SetHostExtras(nil) })
	SetHostExtras(func() HostExtras { return HostExtras{CAFile: ca} })

	name := "nimbus-hostextras-ca-test"
	_ = d.Remove(ctx, name)
	t.Cleanup(func() { _ = d.Remove(context.Background(), name) })

	if _, err := d.Run(ctx, RunOptions{
		Name:  name,
		Image: image,
		Args:  []string{"sleep", "120"},
	}); err != nil {
		t.Fatalf("starting the container: %v", err)
	}

	out, err := d.Exec(ctx, name, "cat", CAPath)
	if err != nil {
		t.Fatalf("reading %s inside the container: %v", CAPath, err)
	}
	if !strings.Contains(string(out), marker) {
		t.Errorf("%s holds %q, want the mounted file", CAPath, string(out))
	}

	// And the environment has to point at it, or a client will never look.
	env, err := d.Exec(ctx, name, "printenv", "SSL_CERT_FILE")
	if err != nil {
		t.Fatalf("reading SSL_CERT_FILE: %v", err)
	}
	if strings.TrimSpace(string(env)) != CAPath {
		t.Errorf("SSL_CERT_FILE = %q, want %q", strings.TrimSpace(string(env)), CAPath)
	}
}

// realDockerOrSkip is the preamble both tests need: a reachable daemon
// and an image to run.
func realDockerOrSkip(t *testing.T) (context.Context, *CLI, string) {
	t.Helper()
	if os.Getenv("NIMBUS_DOCKER_TESTS") != "1" {
		t.Skip("set NIMBUS_DOCKER_TESTS=1 to run tests that need a real Docker daemon")
	}
	ctx := context.Background()
	d := NewCLI()
	if err := d.Available(ctx); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	// The same image the vm engine's real test uses, so a job running
	// both pulls nothing extra.
	const image = "alpine:3.20"
	if present, err := d.ImagePresent(ctx, image); err != nil {
		t.Fatal(err)
	} else if !present {
		if err := d.Pull(ctx, image); err != nil {
			t.Skipf("cannot pull %s (offline?): %v", image, err)
		}
	}
	return ctx, d, image
}

// resolveIn asks a running container to resolve a name and returns the
// addresses, sorted.
//
// Sorted and plural because a name mapped with --add-host gets BOTH an
// IPv4 and an IPv6 entry -- measured: host.docker.internal resolves to
// 192.168.65.254 and fdc4:f303:9324::254 -- so comparing one line against
// another would be a coin toss.
func resolveIn(t *testing.T, ctx context.Context, d *CLI, container, name string) []string {
	t.Helper()
	// `getent hosts` is in busybox and in glibc, and unlike a ping or a
	// curl it asks only the question this test is about.
	out, err := d.Exec(ctx, container, "getent", "hosts", name)
	if err != nil {
		// A name that does not resolve exits non-zero, which is an
		// answer rather than a failure to run.
		return nil
	}
	var addrs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			addrs = append(addrs, f[0])
		}
	}
	sort.Strings(addrs)
	return addrs
}
