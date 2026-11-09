package docker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The whole point of classifying errors is that "Docker is not running"
// reaches the user as advice, not as a timeout or a wall of stderr. These are
// the real messages the various runtimes print.
func TestDaemonDownIsRecognisedAcrossRuntimes(t *testing.T) {
	for _, msg := range []string{
		// Docker Desktop / dockerd on macOS and Linux
		"Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?",
		// Docker Desktop on Windows
		"error during connect: Get \"http://%2F%2F.%2Fpipe%2Fdocker_engine/_ping\": open //./pipe/docker_engine: The system cannot find the file specified.",
		// Colima / OrbStack with the VM stopped
		"Cannot connect to the Docker daemon at unix:///Users/x/.colima/docker.sock. Is the docker daemon running?",
		// A socket that exists but nothing is listening
		"dial unix /var/run/docker.sock: connect: connection refused",
		// Rootless setups
		"permission denied while trying to connect to the Docker daemon socket",
	} {
		if !isDaemonDown(msg) {
			t.Errorf("daemon-down message not recognised:\n  %s", msg)
		}
	}

	// A genuine operational failure must NOT be mistaken for the daemon being
	// down, or we would tell the user to start Docker when it is already up.
	for _, msg := range []string{
		"Error response from daemon: Conflict. The container name \"/nimbus-aks-1\" is already in use",
		"docker: invalid reference format",
		"Error response from daemon: driver failed programming external connectivity",
	} {
		if isDaemonDown(msg) {
			t.Errorf("operational failure misread as daemon-down:\n  %s", msg)
		}
	}
}

func TestNotFoundIsRecognised(t *testing.T) {
	for _, msg := range []string{
		"Error response from daemon: No such container: nimbus-aks-1",
		"Error: No such object: nimbus-aks-1",
		"Error response from daemon: network nimbus-net not found: no such network",
	} {
		if !isNotFound(msg) {
			t.Errorf("not-found message not recognised: %s", msg)
		}
	}
}

// A missing docker binary must be ErrUnavailable, not an opaque exec error.
func TestMissingBinaryIsUnavailable(t *testing.T) {
	c := &CLI{Binary: "definitely-not-a-real-binary-nimbus", Timeout: 5 * time.Second}
	err := c.Available(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("missing binary gave %v, want ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), "PATH") {
		t.Errorf("the error should say the binary was not found: %v", err)
	}
}

// A docker call that hangs must time out rather than block an ARM request
// forever. Terraform sitting on "Still creating..." with no end is a far worse
// failure than an error, because nothing tells you to look at Docker.
func TestSlowDockerTimesOutInsteadOfHanging(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	c := &CLI{Binary: sleep, Timeout: 150 * time.Millisecond}

	start := time.Now()
	_, err = c.run(context.Background(), "30")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a hanging docker call should fail, not succeed")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("timeout gave %v, want ErrUnavailable", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %s; the timeout was not applied", elapsed)
	}
}

func TestInspectParsesPortsAndLabels(t *testing.T) {
	var j inspectJSON
	j.ID = "abc123"
	j.Name = "/nimbus-aks-1"
	j.State.Running = true
	j.State.Status = "running"
	j.Config.Image = "rancher/k3s:v1.34.12-k3s1"
	j.Config.Labels = map[string]string{"nimbus.cluster": "demo"}
	j.NetworkSettings.Ports = map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}{
		"6443/tcp": {{HostIP: "127.0.0.1", HostPort: "6443"}},
		"80/tcp":   nil, // exposed but not published
	}

	c := toContainer(j)
	if c.Name != "nimbus-aks-1" {
		t.Errorf("name = %q; the leading slash must be trimmed", c.Name)
	}
	if c.Ports["6443/tcp"] != "127.0.0.1:6443" {
		t.Errorf("ports = %v", c.Ports)
	}
	if _, present := c.Ports["80/tcp"]; present {
		t.Error("an unpublished port should not appear in Ports")
	}
	if !c.Running || c.Labels["nimbus.cluster"] != "demo" {
		t.Errorf("container = %+v", c)
	}
}

// --- the Fake ---------------------------------------------------------------

func TestFakeBehavesLikeTheRealClientForTheCasesWeRelyOn(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	if err := f.Available(ctx); err != nil {
		t.Fatalf("a healthy fake should be available: %v", err)
	}

	if _, err := f.Run(ctx, RunOptions{
		Name: "nimbus-aks-1", Image: "rancher/k3s:v1.34.12-k3s1",
		Args: []string{"server"}, Privileged: true,
		Ports:  map[string]string{"127.0.0.1:6443": "6443"},
		Labels: map[string]string{"nimbus.cluster": "demo"},
	}); err != nil {
		t.Fatal(err)
	}

	c, err := f.Inspect(ctx, "nimbus-aks-1")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Running || c.Ports["6443/tcp"] != "127.0.0.1:6443" {
		t.Errorf("fake container = %+v", c)
	}

	// Label filtering, as List is used to reattach to existing clusters.
	if got, _ := f.List(ctx, "nimbus.cluster=demo"); len(got) != 1 {
		t.Errorf("list by label returned %d, want 1", len(got))
	}
	if got, _ := f.List(ctx, "nimbus.cluster=other"); len(got) != 0 {
		t.Errorf("list by a non-matching label returned %d, want 0", len(got))
	}

	// Stopping something already gone is not an error, matching the real client.
	if err := f.Stop(ctx, "never-existed"); err != nil {
		t.Errorf("stopping a missing container should be a no-op: %v", err)
	}
	if err := f.Remove(ctx, "never-existed"); err != nil {
		t.Errorf("removing a missing container should be a no-op: %v", err)
	}

	// Inspecting something gone is a typed NotFoundError.
	_ = f.Remove(ctx, "nimbus-aks-1")
	_, err = f.Inspect(ctx, "nimbus-aks-1")
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Errorf("inspect of a removed container gave %v, want *NotFoundError", err)
	}
}

func TestFakeUnavailableFailsEverything(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	f.Unavailable = true

	for name, call := range map[string]func() error{
		"Available": func() error { return f.Available(ctx) },
		"Run": func() error {
			_, err := f.Run(ctx, RunOptions{Name: "x", Image: "y"})
			return err
		},
		"Inspect": func() error { _, err := f.Inspect(ctx, "x"); return err },
		"List":    func() error { _, err := f.List(ctx, "k=v"); return err },
		"Remove":  func() error { return f.Remove(ctx, "x") },
		"Pull":    func() error { return f.Pull(ctx, "img") },
	} {
		if err := call(); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s with a stopped daemon gave %v, want ErrUnavailable", name, err)
		}
	}
}

// The opt-in integration test. `go test ./...` must not need Docker, so this
// is skipped unless explicitly asked for.
func TestRealDockerIsReachable(t *testing.T) {
	if os.Getenv("NIMBUS_DOCKER_TESTS") != "1" {
		t.Skip("set NIMBUS_DOCKER_TESTS=1 to run tests that need a real Docker daemon")
	}
	if err := NewCLI().Available(context.Background()); err != nil {
		t.Fatalf("docker is not available: %v", err)
	}
}

// A real container lifecycle, to prove the CLI wrapper's argument building
// matches what docker actually accepts -- the Fake cannot catch a malformed
// flag. Uses a tiny image rather than k3s so it stays quick.
func TestRealContainerLifecycle(t *testing.T) {
	if os.Getenv("NIMBUS_DOCKER_TESTS") != "1" {
		t.Skip("set NIMBUS_DOCKER_TESTS=1 to run tests that need a real Docker daemon")
	}
	ctx := context.Background()
	c := NewCLI()
	if err := c.Available(ctx); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	const image = "busybox:latest"
	if present, err := c.ImagePresent(ctx, image); err != nil {
		t.Fatal(err)
	} else if !present {
		if err := c.Pull(ctx, image); err != nil {
			t.Skipf("cannot pull %s (offline?): %v", image, err)
		}
	}

	const name = "nimbus-engine-test"
	const network = "nimbus-engine-test-net"
	_ = c.Remove(ctx, name)
	_ = c.RemoveNetwork(ctx, network)

	if err := c.EnsureNetwork(ctx, network); err != nil {
		t.Fatal(err)
	}
	// Calling it twice must be a no-op, since reattaching does exactly that.
	if err := c.EnsureNetwork(ctx, network); err != nil {
		t.Errorf("EnsureNetwork is not idempotent: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Remove(ctx, name)
		_ = c.RemoveNetwork(ctx, network)
	})

	id, err := c.Run(ctx, RunOptions{
		Name: name, Image: image, Network: network,
		Args:   []string{"sleep", "60"},
		Labels: map[string]string{"nimbus.test": "1"},
		Env:    map[string]string{"NIMBUS": "yes"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if id == "" {
		t.Error("Run returned an empty container ID")
	}

	got, err := c.Inspect(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Running || got.Labels["nimbus.test"] != "1" {
		t.Errorf("inspect = %+v", got)
	}

	out, err := c.Exec(ctx, name, "sh", "-c", "echo $NIMBUS")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if strings.TrimSpace(string(out)) != "yes" {
		t.Errorf("exec output = %q, want \"yes\"", out)
	}

	found, err := c.List(ctx, "nimbus.test=1")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Name != name {
		t.Errorf("list by label = %+v", found)
	}

	if err := c.Remove(ctx, name); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := c.Inspect(ctx, name); err == nil {
		t.Error("container still exists after removal")
	}
}

// Every container Nimbus starts has to be able to reach back to Nimbus:
// a Lambda handler reads AWS_ENDPOINT_URL, a k3s API server calls the
// TokenReview webhook, and both of those addresses say
// host.docker.internal. That name is a DOCKER DESKTOP convenience and does
// not exist on Docker Engine, which is what Linux CI runs -- so the flag
// has to be passed explicitly, and the one place that cannot be forgotten
// per cloud is here.
//
// Measured: without it, the EKS and GKE jobs failed with
// "You must be logged in to the server" -- a 401, because the API server
// could not resolve the webhook host, NOT a connection error. The AKS job
// passed throughout, because Azure authenticates with k3s's own kubeconfig
// and never calls back out. A 401 from a cluster you just created is this
// bug's signature.
func TestEveryContainerCanReachTheHostThatStartedIt(t *testing.T) {
	args := runArgs(RunOptions{Name: "nimbus-test-1", Image: "alpine"})

	found := ""
	for i, a := range args {
		if a == "--add-host" && i+1 < len(args) {
			found = args[i+1]
		}
	}
	if found != "host.docker.internal:host-gateway" {
		t.Errorf("no host-gateway mapping in %v", args)
	}
}

// The image and its arguments must stay LAST: anything after the image name
// is the container's command, so a flag appended there silently becomes an
// argument to the entrypoint instead of to docker.
func TestTheImageAndItsArgumentsComeLast(t *testing.T) {
	args := runArgs(RunOptions{
		Name:  "nimbus-test-1",
		Image: "rancher/k3s:v1.34.12-k3s1",
		Args:  []string{"server", "--disable=traefik"},
		Ports: map[string]string{"127.0.0.1:6443": "6443"},
		Env:   map[string]string{"K3S_TOKEN": "x"},
	})

	tail := args[len(args)-3:]
	want := []string{"rancher/k3s:v1.34.12-k3s1", "server", "--disable=traefik"}
	for i := range want {
		if tail[i] != want[i] {
			t.Fatalf("args end %v, want them to end %v", tail, want)
		}
	}
}
