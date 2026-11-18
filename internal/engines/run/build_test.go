package run

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// A service whose image has to be BUILT first.
//
// Every caller until now supplied a published image to pull. The Azure
// Functions host cannot work that way: its own base image reports zero
// functions when the code is bind-mounted and loads them when the same
// tree is baked in with a COPY, so a function app's image is built per
// deployment from the unpacked code.
//
// It belongs in this engine rather than a new one because the Functions
// host's contract IS this engine's: one long-lived container, listening on
// a port, every method and path proxied through, readiness by HTTP GET.
// The only thing missing was "build the image first", which names no
// cloud. What writes the Dockerfile is the plugin's business -- the base
// image and /home/site/wwwroot are Azure's vocabulary and do not belong
// here.

func newBuildEngine(t *testing.T) (*Engine, *docker.Fake) {
	t.Helper()
	f := docker.NewFake()
	// Bounded: the readiness probe is a real HTTP GET and cannot succeed
	// against the Fake, so the default three minutes would be waited out.
	return New(Config{NamePrefix: "nimbus-fnapp", StartTimeout: 50 * time.Millisecond}, f), f
}

func TestAServiceWithABuildDirIsBuiltNotPulled(t *testing.T) {
	e, f := newBuildEngine(t)

	svc := &Service{
		ResourceID: "/subscriptions/x/sites/app", Name: "app",
		Image:    "nimbus-fnapp-app-abc12345:latest",
		BuildDir: "/tmp/app-code",
	}
	_ = e.Start(context.Background(), svc)

	dir, _, ok := f.BuiltImage(svc.Image)
	if !ok {
		t.Fatalf("the image was not built; calls: %v", f.Calls)
	}
	if dir != "/tmp/app-code" {
		t.Errorf("built from %q, want the service's BuildDir", dir)
	}

	// And it must NOT have been pulled: the tag exists only locally, so a
	// pull would fail against a registry that has never heard of it.
	for _, call := range f.Calls {
		if call == "pull "+svc.Image {
			t.Error("a built image was also pulled; the pull can only fail")
		}
	}
}

// A service with no BuildDir is pulled exactly as before, which is every
// Container App and every Cloud Run service.
func TestAServiceWithoutABuildDirIsUnchanged(t *testing.T) {
	e, f := newBuildEngine(t)

	_ = e.Start(context.Background(), &Service{
		ResourceID: "/subscriptions/x/app/web", Name: "web", Image: "nginx:alpine",
	})

	if len(f.Builds) != 0 {
		t.Errorf("a service with no BuildDir triggered a build: %+v", f.Builds)
	}
	pulled := false
	for _, call := range f.Calls {
		if call == "pull nginx:alpine" {
			pulled = true
		}
	}
	if !pulled {
		t.Errorf("the image was not pulled; calls: %v", f.Calls)
	}
}

// A build that fails fails the START, naming the image.
//
// The alternative is a container that never appears and a resource that
// reports Succeeded, which is the shape this project keeps removing. A
// generated Dockerfile with a mistake in it, or a base image that is not
// present, both land here.
func TestAFailedBuildFailsTheStart(t *testing.T) {
	e, f := newBuildEngine(t)
	f.FailBuild = docker.ErrUnavailable

	err := e.Start(context.Background(), &Service{
		ResourceID: "/subscriptions/x/sites/app", Name: "app",
		Image: "nimbus-fnapp-app-abc:latest", BuildDir: "/tmp/code",
	})
	if err == nil {
		t.Fatal("a failed build should fail the start")
	}
	if !strings.Contains(err.Error(), "nimbus-fnapp-app-abc:latest") {
		t.Errorf("the error does not name the image it could not build: %v", err)
	}
	// Nothing should be running.
	if got := f.Containers(); len(got) != 0 {
		t.Errorf("a failed build still started %v", names(got))
	}
}

// The base image is pulled before the build, when one is named.
//
// A `docker build` whose FROM is not present locally reaches out to a
// registry, which is the one thing this project will not do -- so the base
// is pulled through the ordinary path first, where a missing one produces
// the "pre-pull it while online" message instead of a build that silently
// needs the network.
func TestTheBaseImageIsPulledBeforeTheBuild(t *testing.T) {
	e, f := newBuildEngine(t)

	svc := &Service{
		ResourceID: "/subscriptions/x/sites/app", Name: "app",
		Image:     "nimbus-fnapp-app-abc:latest",
		BuildDir:  "/tmp/code",
		BuildBase: "mcr.microsoft.com/azure-functions/python:4-python3.11",
	}
	_ = e.Start(context.Background(), svc)

	pulledBase := false
	for _, call := range f.Calls {
		if call == "pull "+svc.BuildBase {
			pulledBase = true
		}
	}
	if !pulledBase {
		t.Errorf("the base image was not pulled before the build; calls: %v", f.Calls)
	}
}

// A base image already present is not pulled again, so a second deploy
// costs a build and nothing else.
func TestAPresentBaseImageIsNotPulledAgain(t *testing.T) {
	e, f := newBuildEngine(t)
	const base = "mcr.microsoft.com/azure-functions/python:4-python3.11"
	f.Images[base] = true

	_ = e.Start(context.Background(), &Service{
		ResourceID: "/subscriptions/x/sites/app", Name: "app",
		Image: "nimbus-fnapp-app-abc:latest", BuildDir: "/tmp/code", BuildBase: base,
	})

	for _, call := range f.Calls {
		if call == "pull "+base {
			t.Error("a base image that is already present was pulled again")
		}
	}
}

// Deleting the service removes the image it built.
//
// A built image is new rubbish that no previous engine produced: a pulled
// image is shared and must stay, but one built for a single resource is
// that resource's own and would otherwise accumulate per app, invisibly,
// at roughly a gigabyte each.
func TestStoppingAServiceRemovesTheImageItBuilt(t *testing.T) {
	e, f := newBuildEngine(t)

	svc := &Service{
		ResourceID: "/subscriptions/x/sites/app", Name: "app",
		Image: "nimbus-fnapp-app-abc:latest", BuildDir: "/tmp/code",
	}
	_ = e.Start(context.Background(), svc)
	if present, _ := f.ImagePresent(context.Background(), svc.Image); !present {
		t.Fatal("setup: the image should be present after the build")
	}

	e.Stop(context.Background(), svc)

	if present, _ := f.ImagePresent(context.Background(), svc.Image); present {
		t.Error("the built image survived the delete; it belongs to that resource alone")
	}
}

// A PULLED image is not removed on delete, because it is shared.
//
// Removing nginx:alpine because one Container App used it would break
// every other app on the machine and cost a re-pull. The distinction is
// exactly whether Nimbus built it.
func TestStoppingAServiceKeepsAPulledImage(t *testing.T) {
	e, f := newBuildEngine(t)

	svc := &Service{
		ResourceID: "/subscriptions/x/app/web", Name: "web", Image: "nginx:alpine",
	}
	_ = e.Start(context.Background(), svc)
	e.Stop(context.Background(), svc)

	if present, _ := f.ImagePresent(context.Background(), "nginx:alpine"); !present {
		t.Error("a pulled image was removed; it is shared with everything else on the machine")
	}
}
