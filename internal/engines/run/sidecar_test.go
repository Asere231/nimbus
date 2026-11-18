package run

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// A service runs every container in its template, not just the first.
//
// Real Azure Container Apps -- and real Cloud Run -- run the containers in
// a template as a GROUP that shares a network namespace, so they reach each
// other on localhost. Nimbus ran the first one and stored the rest, which
// meant an app with a cache or a log shipper beside it applied cleanly and
// then behaved as though half its configuration was not there.
//
// `--network container:<main>` is the Docker spelling of that namespace,
// which is also why docker.runArgs has to drop --add-host for a sidecar:
// Docker refuses the two together.
// Every Start below bounds StartTimeout tightly, because the readiness
// probe CANNOT succeed against the Fake -- it is a real HTTP GET against a
// published port, deliberately, since a service must not report ready
// before anything is listening. With the default three minutes each of
// these tests would wait one out: the first draft of this file took 601
// seconds to fail, which is the trap CLAUDE.md already records about the
// spec round-trip test.
func TestAServiceStartsItsSidecars(t *testing.T) {
	f := docker.NewFake()
	f.Images["nginx:alpine"] = true
	f.Images["redis:alpine"] = true
	e := New(Config{NamePrefix: "nimbus-app", StartTimeout: 50 * time.Millisecond}, f)

	svc := &Service{
		ResourceID: "/subscriptions/x/app/web",
		Name:       "web",
		Image:      "nginx:alpine",
		Sidecars: []Sidecar{
			{Name: "cache", Image: "redis:alpine"},
			{Name: "shipper", Image: "nginx:alpine", Env: map[string]string{"MODE": "ship"}},
		},
	}
	// The readiness probe cannot succeed against the Fake -- deliberately,
	// since a service must not report ready before anything is listening --
	// so the error is expected and what matters is what was started.
	_ = e.Start(context.Background(), svc)

	started := f.Containers()
	if len(started) != 3 {
		t.Fatalf("expected the main container and two sidecars, got %d: %v",
			len(started), names(started))
	}

	main := svc.Container
	var sidecars []docker.Container
	for _, c := range started {
		if c.Name != main {
			sidecars = append(sidecars, c)
		}
	}
	if len(sidecars) != 2 {
		t.Fatalf("could not tell the sidecars apart from %v", names(started))
	}
	for _, c := range sidecars {
		if c.Labels[docker.LabelRole] != "sidecar" {
			t.Errorf("%s is not labelled a sidecar: %v", c.Name, c.Labels)
		}
		if c.Labels[docker.LabelResource] != svc.ResourceID {
			t.Errorf("%s does not carry its app's resource id: %v", c.Name, c.Labels)
		}
	}
}

// A sidecar shares the main container's network namespace, which is the
// whole point: in Azure the containers in one app talk over localhost.
func TestASidecarSharesTheMainContainersNamespace(t *testing.T) {
	f := docker.NewFake()
	f.Images["nginx:alpine"] = true
	f.Images["redis:alpine"] = true
	e := New(Config{NamePrefix: "nimbus-app", StartTimeout: 50 * time.Millisecond}, f)

	svc := &Service{
		ResourceID: "/subscriptions/x/app/web", Name: "web", Image: "nginx:alpine",
		Sidecars: []Sidecar{{Name: "cache", Image: "redis:alpine"}},
	}
	_ = e.Start(context.Background(), svc)

	opts, ok := f.RunOptionsFor(sidecarName(e.cfg.NamePrefix, svc, 0, "cache"))
	if !ok {
		t.Fatalf("the sidecar was not started; containers: %v", names(f.Containers()))
	}
	if opts.Network != docker.NamespaceOf(svc.Container) {
		t.Errorf("the sidecar's network is %q, want the main container's namespace %q",
			opts.Network, docker.NamespaceOf(svc.Container))
	}
	// And it must publish nothing: Docker refuses -p with a shared
	// namespace, and the ports belong to the container that owns it.
	if len(opts.Ports) != 0 {
		t.Errorf("a sidecar published ports (%v); docker refuses that with a shared namespace",
			opts.Ports)
	}
}

// Stopping the service takes the sidecars with it.
//
// A sidecar outliving its app is the --persist leak one layer down: the
// store would forget the app while its containers kept running, with
// nothing left that knew about them.
func TestStoppingAServiceRemovesItsSidecars(t *testing.T) {
	f := docker.NewFake()
	f.Images["nginx:alpine"] = true
	f.Images["redis:alpine"] = true
	e := New(Config{NamePrefix: "nimbus-app", StartTimeout: 50 * time.Millisecond}, f)

	svc := &Service{
		ResourceID: "/subscriptions/x/app/web", Name: "web", Image: "nginx:alpine",
		Sidecars: []Sidecar{{Name: "cache", Image: "redis:alpine"}},
	}
	_ = e.Start(context.Background(), svc)
	if len(f.Containers()) != 2 {
		t.Fatalf("setup: expected two containers, got %v", names(f.Containers()))
	}

	e.Stop(context.Background(), svc)
	if left := f.Containers(); len(left) != 0 {
		t.Errorf("stopping the service left %v behind", names(left))
	}
}

// Redeploying replaces the sidecars too, rather than leaving the previous
// revision's beside the new ones.
func TestRestartingAServiceReplacesItsSidecars(t *testing.T) {
	f := docker.NewFake()
	f.Images["nginx:alpine"] = true
	f.Images["redis:alpine"] = true
	e := New(Config{NamePrefix: "nimbus-app", StartTimeout: 50 * time.Millisecond}, f)

	svc := &Service{
		ResourceID: "/subscriptions/x/app/web", Name: "web", Image: "nginx:alpine",
		Sidecars: []Sidecar{{Name: "cache", Image: "redis:alpine"}},
	}
	_ = e.Start(context.Background(), svc)
	_ = e.Start(context.Background(), svc)

	if got := f.Containers(); len(got) != 2 {
		t.Errorf("a second deploy left %d containers, expected 2: %v", len(got), names(got))
	}
}

// A service with no sidecars starts exactly one container, which is every
// existing caller and must not change.
func TestAServiceWithNoSidecarsStartsOneContainer(t *testing.T) {
	f := docker.NewFake()
	f.Images["nginx:alpine"] = true
	e := New(Config{NamePrefix: "nimbus-app", StartTimeout: 50 * time.Millisecond}, f)

	_ = e.Start(context.Background(), &Service{
		ResourceID: "/subscriptions/x/app/web", Name: "web", Image: "nginx:alpine",
	})
	if got := f.Containers(); len(got) != 1 {
		t.Errorf("expected one container, got %v", names(got))
	}
}

// A sidecar's own image is pulled, or the first deploy of an app whose
// cache image is not local fails on the sidecar rather than the app.
func TestASidecarsImageIsPulled(t *testing.T) {
	f := docker.NewFake()
	f.Images["nginx:alpine"] = true // the main image only
	e := New(Config{NamePrefix: "nimbus-app", StartTimeout: 50 * time.Millisecond}, f)

	_ = e.Start(context.Background(), &Service{
		ResourceID: "/subscriptions/x/app/web", Name: "web", Image: "nginx:alpine",
		Sidecars: []Sidecar{{Name: "cache", Image: "redis:alpine"}},
	})
	pulled := false
	for _, call := range f.Calls {
		if call == "pull redis:alpine" {
			pulled = true
		}
	}
	if !pulled {
		t.Errorf("the sidecar's image was not pulled; calls: %v", f.Calls)
	}
}

func names(cs []docker.Container) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, strings.TrimPrefix(c.Name, "/"))
	}
	return out
}

// A sidecar is collected even when nothing in memory knows about it.
//
// This is the Milestone 18 lesson applied before it bites: with --persist
// the containers outlive the process, so after a restart the store knows
// the app and the registry knows nothing about its sidecars. If removal
// relied on the Sidecars list, a delete after a restart would take the
// main container and leave the sidecars running for ever, with nothing
// left that referred to them -- exactly the --persist leak that cost
// Milestone 18 a bug in funcs and run alike.
//
// So removeGroup also sweeps by LABEL, and that sweep is what this test
// pins. The Service here has an empty Sidecars list on purpose: it stands
// for the one a reattach reconstructs.
func TestASidecarIsCollectedEvenWhenUnregistered(t *testing.T) {
	f := docker.NewFake()
	f.Images["nginx:alpine"] = true
	f.Images["redis:alpine"] = true
	e := New(Config{NamePrefix: "nimbus-app", StartTimeout: 50 * time.Millisecond}, f)

	const id = "/subscriptions/x/app/web"
	// What a previous process left behind: a main container and a sidecar.
	svc := &Service{
		ResourceID: id, Name: "web", Image: "nginx:alpine",
		Sidecars: []Sidecar{{Name: "cache", Image: "redis:alpine"}},
	}
	_ = e.Start(context.Background(), svc)
	if len(f.Containers()) != 2 {
		t.Fatalf("setup: expected two containers, got %v", names(f.Containers()))
	}

	// The restart: a service with the port recovered and NO sidecars
	// listed, which is all a reattach can know.
	adopted := &Service{
		ResourceID: id, Name: "web", Image: "nginx:alpine",
		Container: svc.Container, Port: svc.Port,
	}
	e.Stop(context.Background(), adopted)

	if left := f.Containers(); len(left) != 0 {
		t.Errorf("deleting an adopted app left %v behind; a sidecar outlived its app",
			names(left))
	}
}

// A sidecar REMOVED from the template does not keep running.
//
// The containers are named from the resource id, so a redeploy whose
// template no longer mentions "cache" would otherwise leave that container
// up with nothing referring to it -- the same leak by a different route.
func TestASidecarDroppedFromTheTemplateIsRemoved(t *testing.T) {
	f := docker.NewFake()
	f.Images["nginx:alpine"] = true
	f.Images["redis:alpine"] = true
	e := New(Config{NamePrefix: "nimbus-app", StartTimeout: 50 * time.Millisecond}, f)

	const id = "/subscriptions/x/app/web"
	_ = e.Start(context.Background(), &Service{
		ResourceID: id, Name: "web", Image: "nginx:alpine",
		Sidecars: []Sidecar{{Name: "cache", Image: "redis:alpine"}},
	})
	if len(f.Containers()) != 2 {
		t.Fatalf("setup: expected two containers, got %v", names(f.Containers()))
	}

	// The next deploy has no sidecar at all.
	_ = e.Start(context.Background(), &Service{
		ResourceID: id, Name: "web", Image: "nginx:alpine",
	})

	if got := f.Containers(); len(got) != 1 {
		t.Errorf("dropping a sidecar from the template left %v running", names(got))
	}
}
