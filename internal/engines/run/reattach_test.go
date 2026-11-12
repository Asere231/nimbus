package run

import (
	"context"
	"testing"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// A service's container outlives Nimbus with --persist, so a fresh engine
// has to adopt it -- and the port is what makes the adoption worth
// anything.
//
// proxyFor builds a proxy lazily from svc.Port, so a Service restored
// without one leaves the FQDN answering nothing while the container sits
// plainly Up. That was the old behaviour: the resource read back perfectly
// and the address did not work.
//
// The container port is recovered from the published mapping rather than
// assumed, because unlike a Lambda's fixed 8080 a client chooses what its
// image listens on.
func TestReattachAdoptsServiceContainers(t *testing.T) {
	f := docker.NewFake()
	ctx := context.Background()

	if _, err := f.Run(ctx, docker.RunOptions{
		Name:  "nimbus-aca-web-f0833c99",
		Image: "nginx:alpine",
		Ports: map[string]string{"127.0.0.1:4820": "8080"},
		Labels: map[string]string{
			docker.LabelService:  "web",
			docker.LabelResource: "/subscriptions/x/containerApps/web",
			docker.LabelRole:     "service",
		},
	}); err != nil {
		t.Fatal(err)
	}

	// A completely fresh engine over the same daemon: this is a restart.
	e := New(Config{NamePrefix: "nimbus-aca"}, f)
	found := e.Reattach(ctx, func(string) bool { return true })

	if len(found) != 1 {
		t.Fatalf("adopted %d services, want 1: %v", len(found), found)
	}
	svc := e.Get("/subscriptions/x/containerApps/web")
	if svc == nil {
		t.Fatal("the service is not in the registry after reattaching")
	}
	if svc.Name != "web" {
		t.Errorf("name = %q, want web", svc.Name)
	}
	if svc.Container != "nimbus-aca-web-f0833c99" {
		t.Errorf("container = %q", svc.Container)
	}
	if svc.Port != 4820 {
		t.Errorf("host port = %d, want 4820", svc.Port)
	}
	if svc.ContainerPort != 8080 {
		t.Errorf("container port = %d, want 8080 (read back from the mapping)", svc.ContainerPort)
	}
}

// The two clouds that share this engine must not adopt each other's
// services. `keep` is the divider, not the container name.
func TestReattachLeavesServicesWhoseResourceIsGone(t *testing.T) {
	f := docker.NewFake()
	ctx := context.Background()
	if _, err := f.Run(ctx, docker.RunOptions{
		Name:  "nimbus-run-other-1",
		Image: "nginx:alpine",
		Ports: map[string]string{"127.0.0.1:4821": "8080"},
		Labels: map[string]string{
			docker.LabelService:  "other",
			docker.LabelResource: "projects/p/services/other",
			docker.LabelRole:     "service",
		},
	}); err != nil {
		t.Fatal(err)
	}

	e := New(Config{NamePrefix: "nimbus-aca"}, f)
	if found := e.Reattach(ctx, func(string) bool { return false }); len(found) != 0 {
		t.Errorf("adopted %d services it should not own, want 0", len(found))
	}
	if e.Get("projects/p/services/other") != nil {
		t.Error("a service whose resource is gone was put in the registry")
	}
}
