package funcs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// Deploying the same function TWICE, with it having actually run in
// between. That sequence is the whole test, and nothing had ever run it.
//
// A Python handler writes __pycache__ on its first import. While the code
// directory was mounted read-WRITE, those files landed on the host owned
// by root, because the container runs as root -- and on any host that
// enforces Unix ownership (Linux, and WSL) the ordinary user Nimbus runs
// as then cannot unlink them. So Unpack's RemoveAll failed and the next
// deploy of that function failed with it, while the destroy before it
// failed SILENTLY, the error having been discarded.
//
// Measured before the fix:
//
//	unlinkat /tmp/nimbus-aws/lambda/.../__pycache__/app.cpython-312.pyc:
//	  permission denied
//
// Windows and macOS never saw it, because Docker Desktop presents a bind
// mount permissively, and CI never saw it because no job deploys the same
// function twice -- apply, invoke and destroy, each once, in a fresh
// runner. This test is that gap closed: it only fails where the bug was
// real, which is the platform CI runs on.
func TestRedeployingAFunctionThatHasRunSucceeds(t *testing.T) {
	ctx, d, image := realLambdaOrSkip(t)

	dir := t.TempDir()
	fn := &Function{
		ResourceID: "arn:aws:lambda:us-east-1:000000000000:function:redeploy-test",
		Name:       "redeploy-test",
		Image:      image,
		Command:    []string{"app.handler"},
		Dir:        dir,
	}
	writeHandler(t, dir)
	t.Cleanup(func() { e := New(Config{}, d); e.Stop(context.Background(), fn) })

	e := New(Config{}, d)
	if err := e.Start(ctx, fn); err != nil {
		t.Fatalf("starting the function: %v", err)
	}

	// It must really RUN: the bytecode only appears on first import, so a
	// test that merely started the container would pass with the bug in
	// place. Start's readiness probe is itself a real invocation, so this
	// is belt and braces -- and it also proves a read-only /var/task does
	// not stop the handler working, which was the risk of the fix.
	res, err := e.Invoke(ctx, fn, []byte(`{"hello":"world"}`), 60*time.Second)
	if err != nil {
		t.Fatalf("invoking: %v", err)
	}
	if !strings.Contains(string(res.Payload), "ok") {
		t.Fatalf("handler returned %q, want it to contain ok", res.Payload)
	}

	// THE assertion: deploy again over the same directory, which is what
	// `terraform apply` does after a handler is edited.
	if err := Unpack(zipWith(t, map[string]string{"app.py": handlerSource}), dir); err != nil {
		t.Fatalf("redeploying over a directory the function has run from: %v", err)
	}
}

const handlerSource = `
def handler(event, context):
    return {"ok": True, "event": event}
`

func writeHandler(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte(handlerSource), 0o644); err != nil {
		t.Fatal(err)
	}
}

func realLambdaOrSkip(t *testing.T) (context.Context, *docker.CLI, string) {
	t.Helper()
	if os.Getenv("NIMBUS_DOCKER_TESTS") != "1" {
		t.Skip("set NIMBUS_DOCKER_TESTS=1 to run tests that need a real Docker daemon")
	}
	ctx := context.Background()
	d := docker.NewCLI()
	if err := d.Available(ctx); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	// AWS's own base image, which ships the Runtime Interface Emulator --
	// the whole reason a handler runs here on the real runtime interface.
	const image = "public.ecr.aws/lambda/python:3.12"
	if present, err := d.ImagePresent(ctx, image); err != nil {
		t.Fatal(err)
	} else if !present {
		if err := d.Pull(ctx, image); err != nil {
			t.Skipf("cannot pull %s (offline?): %v", image, err)
		}
	}
	return ctx, d, image
}
