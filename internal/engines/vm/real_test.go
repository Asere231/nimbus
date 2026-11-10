package vm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// The bootstrap against a REAL stock image.
//
// Everything in vm_test.go asserts what Nimbus sends; this asserts that
// what it sends works. The script relies on `base64 -d`, `tee` and a
// `while true` loop being present and behaving the same in busybox as in
// coreutils, which is not something a Fake can tell you -- and if the boot
// script is wrong then every cloud plugin built on it is wrong too.
func TestARealInstanceBootsRunsUserDataAndStaysUp(t *testing.T) {
	if os.Getenv("NIMBUS_DOCKER_TESTS") != "1" {
		t.Skip("set NIMBUS_DOCKER_TESTS=1 to run tests that need a real Docker daemon")
	}
	ctx := context.Background()
	d := docker.NewCLI()
	if err := d.Available(ctx); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	const image = "alpine:3.20"
	if present, err := d.ImagePresent(ctx, image); err != nil {
		t.Fatal(err)
	} else if !present {
		if err := d.Pull(ctx, image); err != nil {
			t.Skipf("cannot pull %s (offline?): %v", image, err)
		}
	}

	e := New(Config{NamePrefix: "nimbus-vmtest"}, d)
	i := &Instance{
		ResourceID: "i-real-boot-test",
		Name:       "boot",
		Image:      image,
		// Quotes, a backtick, a dollar sign and a newline: an ordinary
		// script that would wreck an interpolated `sh -c` command.
		UserData: "#!/bin/sh\necho \"hello from `id -un`, HOME=$HOME\"\n" +
			"echo second-line\n",
		PublicKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAtest nimbus@test"},
	}
	t.Cleanup(func() { _ = e.Delete(context.Background(), i) })

	if err := e.Start(ctx, i); err != nil {
		t.Fatal(err)
	}

	// The console log is the proof the script ran. Give the boot a moment:
	// this is a real container starting, not a Fake.
	var console string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		out, err := e.Logs(ctx, i, time.Time{})
		if err == nil {
			console = string(out)
			if strings.Contains(console, "nimbus: instance ready") {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !strings.Contains(console, "nimbus: instance ready") {
		t.Fatalf("the instance never finished booting. console:\n%s", console)
	}
	if !strings.Contains(console, "hello from root") {
		t.Errorf("user data did not run, or its output is not on the console:\n%s", console)
	}
	if !strings.Contains(console, "second-line") {
		t.Errorf("only part of the user data ran:\n%s", console)
	}

	// It must still be up after its script finished -- the whole point of
	// the blocking tail. A container that exited here would read as an
	// instance that crashed.
	if !e.Running(ctx, i) {
		t.Fatal("the instance is not running after its boot script finished")
	}

	// The keys landed where a machine's keys go, and user data was written
	// where the console says it was.
	if out, err := e.Exec(ctx, i, "cat", "/root/.ssh/authorized_keys"); err != nil {
		t.Errorf("reading authorized_keys: %v", err)
	} else if !strings.Contains(string(out), "nimbus@test") {
		t.Errorf("authorized_keys = %q", out)
	}
	if out, err := e.Exec(ctx, i, "cat", "/var/log/nimbus-boot.log"); err != nil {
		t.Errorf("reading the user-data log: %v", err)
	} else if !strings.Contains(string(out), "second-line") {
		t.Errorf("the user-data log = %q", out)
	}

	// And a stop/start keeps the filesystem, which is what makes this an
	// instance rather than a fresh container each time.
	if _, err := e.Exec(ctx, i, "sh", "-c", "echo kept > /root-disk"); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(ctx, i); err != nil {
		t.Fatal(err)
	}
	if e.Running(ctx, i) {
		t.Fatal("the instance is still running after Stop")
	}
	if err := e.Resume(ctx, i); err != nil {
		t.Fatal(err)
	}
	if out, err := e.Exec(ctx, i, "cat", "/root-disk"); err != nil {
		t.Errorf("after a stop and start: %v", err)
	} else if strings.TrimSpace(string(out)) != "kept" {
		t.Errorf("the root disk did not survive a stop/start: %q", out)
	}
}
