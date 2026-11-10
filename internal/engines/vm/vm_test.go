package vm

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

func testEngine(t *testing.T) (*Engine, *docker.Fake) {
	t.Helper()
	f := docker.NewFake()
	f.Images["alpine:3.20"] = true
	f.ExecAny = true
	e := New(Config{NamePrefix: "nimbus-test-vm"}, f)
	t.Cleanup(func() { e.Shutdown(context.Background()) })
	return e, f
}

func inst() *Instance {
	return &Instance{
		ResourceID: "i-0123456789abcdef0",
		Name:       "web",
		Image:      "alpine:3.20",
	}
}

// A machine does not exit when its boot script finishes. A container does,
// and a container that exits reads as an instance that crashed seconds
// after the apply -- which looks like a Nimbus bug and is the first thing
// anyone would hit. So the bootstrap must end in something that blocks.
func TestAnInstanceStaysUpAfterItsBootScriptFinishes(t *testing.T) {
	e, f := testEngine(t)
	if err := e.Start(context.Background(), inst()); err != nil {
		t.Fatal(err)
	}
	if len(f.Runs) != 1 {
		t.Fatalf("started %d containers, want 1", len(f.Runs))
	}
	script := strings.Join(f.Runs[0].Args, " ")
	if !strings.Contains(script, "while true") {
		t.Errorf("the bootstrap does not block at the end:\n%s", script)
	}
}

// User data is whatever someone wrote, including quotes, backticks, dollar
// signs and newlines. Interpolating it into the `sh -c` string would let a
// perfectly ordinary script break the boot -- or worse, run something the
// bootstrap meant to keep separate. It travels base64-encoded in an
// environment variable and is decoded inside the container instead, so the
// shell never parses it as part of the command.
func TestUserDataTravelsEncodedAndIsNeverInterpolated(t *testing.T) {
	e, f := testEngine(t)
	i := inst()
	i.UserData = "#!/bin/sh\necho \"it's $HOME `whoami`\" > /tmp/out\n"

	if err := e.Start(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	opts := f.Runs[0]
	command := strings.Join(opts.Args, " ")
	if strings.Contains(command, "whoami") || strings.Contains(command, "it's") {
		t.Errorf("user data was interpolated into the command:\n%s", command)
	}
	got, ok := opts.Env[envUserData]
	if !ok {
		t.Fatalf("%s is not in the container's environment: %v", envUserData, opts.Env)
	}
	decoded, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("%s is not base64: %v", envUserData, err)
	}
	if string(decoded) != i.UserData {
		t.Errorf("decoded user data = %q, want %q", decoded, i.UserData)
	}
}

// An instance with no user data must still boot. The guard for that is in
// the script, not in Go, so it is worth asserting that nothing is passed
// rather than an empty string that the bootstrap would try to run.
func TestAnInstanceWithoutUserDataStillBoots(t *testing.T) {
	e, f := testEngine(t)
	if err := e.Start(context.Background(), inst()); err != nil {
		t.Fatal(err)
	}
	if v, ok := f.Runs[0].Env[envUserData]; ok && v != "" {
		t.Errorf("%s = %q, want it absent", envUserData, v)
	}
}

// Stopping and starting an instance is the same machine twice, so it must
// go through docker stop/start. Remove-then-Run would also leave something
// running and would silently discard the root filesystem -- the bug this
// asserts against, and the reason docker.Client grew a Start verb.
func TestStopAndResumeKeepTheSameContainer(t *testing.T) {
	e, f := testEngine(t)
	ctx := context.Background()
	i := inst()
	if err := e.Start(ctx, i); err != nil {
		t.Fatal(err)
	}
	container := i.Container
	// Only what happens from here matters: Start removes any container left
	// from a previous boot, and that is not the remove under test.
	f.Calls = nil

	if err := e.Stop(ctx, i); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(ctx, i); err != nil {
		t.Fatal(err)
	}

	if i.Container != container {
		t.Errorf("the container changed across a stop/start: %q then %q", container, i.Container)
	}
	calls := strings.Join(f.Calls, "; ")
	if !strings.Contains(calls, "start "+container) {
		t.Errorf("Resume did not start the container: %s", calls)
	}
	if strings.Contains(calls, "remove "+container) {
		t.Errorf("Resume removed the container, which discards its root disk: %s", calls)
	}
	if n := len(f.Runs); n != 1 {
		t.Errorf("ran %d containers, want the original one only", n)
	}
}

// The metadata service is the only way an SDK inside an instance can learn
// which instance it is, and 169.254.169.254 is unreachable from a
// container. Each instance therefore gets its own listener, and the plugin
// decides what the environment variable pointing at it is called -- AWS and
// GCP disagree on both the name and whether the value carries a scheme, so
// the engine must not choose for them.
func TestEachInstanceGetsItsOwnReachableMetadataEndpoint(t *testing.T) {
	e, f := testEngine(t)
	i := inst()
	i.Metadata = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "instance-id="+r.URL.Path)
	})
	i.MetadataEnv = func(endpoint, hostPort string) map[string]string {
		return map[string]string{"TEST_METADATA_URL": endpoint, "TEST_METADATA_HOST": hostPort}
	}

	if err := e.Start(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	if i.MetadataPort == 0 {
		t.Fatal("no metadata port was allocated")
	}

	// Reachable, and serving the plugin's handler.
	resp, err := http.Get("http://127.0.0.1:" + itoa(i.MetadataPort) + "/latest/meta-data/")
	if err != nil {
		t.Fatalf("the metadata endpoint is not reachable: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "/latest/meta-data/") {
		t.Errorf("metadata body = %q", body)
	}

	// The container is told where it is, by the plugin's own names, and
	// through host.docker.internal rather than localhost -- inside a
	// container localhost is the container.
	env := f.Runs[0].Env
	if !strings.HasPrefix(env["TEST_METADATA_URL"], "http://host.docker.internal:") {
		t.Errorf("TEST_METADATA_URL = %q", env["TEST_METADATA_URL"])
	}
	if !strings.HasPrefix(env["TEST_METADATA_HOST"], "host.docker.internal:") {
		t.Errorf("TEST_METADATA_HOST = %q", env["TEST_METADATA_HOST"])
	}
}

// Deleting an instance must take its listener with it, or a long session
// leaks a port and a goroutine per instance it ever created.
func TestDeleteClosesTheMetadataListener(t *testing.T) {
	e, _ := testEngine(t)
	i := inst()
	i.Metadata = http.NotFoundHandler()
	i.MetadataEnv = func(endpoint, hostPort string) map[string]string { return nil }

	ctx := context.Background()
	if err := e.Start(ctx, i); err != nil {
		t.Fatal(err)
	}
	port := i.MetadataPort
	if err := e.Delete(ctx, i); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get("http://127.0.0.1:" + itoa(port) + "/"); err == nil {
		t.Error("the metadata endpoint still answers after the instance was deleted")
	}
	if e.Get(i.ResourceID) != nil {
		t.Error("the instance is still registered after being deleted")
	}
}

// With KeepRunning set the containers outlive the process, so a fresh
// Nimbus adopts them instead of starting a second set. Everything needed is
// on the container: the resource id is a label.
func TestReattachAdoptsContainersItFinds(t *testing.T) {
	e, f := testEngine(t)
	ctx := context.Background()
	i := inst()
	if err := e.Start(ctx, i); err != nil {
		t.Fatal(err)
	}
	container := i.Container

	// A second engine over the same daemon, as a restart would be.
	fresh := New(Config{NamePrefix: "nimbus-test-vm"}, f)
	t.Cleanup(func() { fresh.Shutdown(ctx) })

	found := fresh.Reattach(ctx, func(string) bool { return true })
	if len(found) != 1 {
		t.Fatalf("adopted %d instances, want 1", len(found))
	}
	if found[0].Container != container {
		t.Errorf("adopted container %q, want %q", found[0].Container, container)
	}
	if !found[0].Running {
		t.Error("the adopted instance should be running")
	}
	if fresh.Get(i.ResourceID) == nil {
		t.Error("the adopted instance was not registered")
	}

	// An instance whose resource has been deleted is left alone: something
	// else is responsible for cleaning it up.
	other := New(Config{NamePrefix: "nimbus-test-vm"}, f)
	t.Cleanup(func() { other.Shutdown(ctx) })
	if got := other.Reattach(ctx, func(string) bool { return false }); len(got) != 0 {
		t.Errorf("adopted %d unwanted instances", len(got))
	}
}

// Shutdown REMOVES what the engine started, and leaves it alone when the
// caller asked for that -- the same choice k3s gives, for the same reason:
// a restart is much faster when the containers are still there.
//
// Removed rather than stopped: a stopped container is still a container,
// and "no containers left behind" is a done-criterion of every example and
// a step in CI. Resume is where stopping without removing belongs.
func TestShutdownHonoursKeepRunning(t *testing.T) {
	for _, keep := range []bool{false, true} {
		f := docker.NewFake()
		f.Images["alpine:3.20"] = true
		e := New(Config{NamePrefix: "nimbus-test-vm", KeepRunning: keep}, f)
		ctx := context.Background()
		i := inst()
		if err := e.Start(ctx, i); err != nil {
			t.Fatal(err)
		}
		f.Calls = nil // the Start's own cleanup Remove is not the one at issue
		e.Shutdown(ctx)

		removed := strings.Contains(strings.Join(f.Calls, "; "), "remove "+i.Container)
		if removed == keep {
			t.Errorf("KeepRunning=%v: removed=%v", keep, removed)
		}
	}
}
