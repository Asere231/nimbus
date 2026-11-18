package azure

import (
	"context"
	"testing"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
)

// A restart adopts a function app into the RIGHT engine.
//
// Azure runs two instances of the run engine -- one for container apps,
// one for function apps -- and the first version of this gave both the
// same `service` role label while reattaching only the container-app one.
// Two things went wrong at once, and both are Milestone 18's lesson again:
//
//   - s.funcapps.Reattach was never called, so after a restart no function
//     app was registered: every request to one answered "no function app
//     named X is running" while its container sat plainly Up in `docker
//     ps`, and a delete could not reach the container because
//     funcapps.Get returned nil -- the --persist leak exactly.
//   - s.apps.Reattach, filtered only on "does this resource exist",
//     adopted the function app's container INTO the container-app engine,
//     because the role label was the same.
//
// So the role label is per engine instance now. The assertion lives here,
// at the cloud, rather than in the engine, because the wiring is what was
// wrong -- a unit test proving Reattach works while nothing calls it is
// this repo's purest "passes for the wrong reason".
func TestARestartAdoptsAFunctionAppIntoItsOwnEngine(t *testing.T) {
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{PublicURL: "https://localhost:4701", DataDir: t.TempDir()}, store, bus)
	f := docker.NewFake()
	s.useDocker(f)

	const siteID = "/subscriptions/0/resourceGroups/rg/providers/Microsoft.Web/sites/fn1"
	const appID = "/subscriptions/0/resourceGroups/rg/providers/Microsoft.App/containerApps/web1"
	store.Put(&core.Resource{Cloud: "azure", ID: siteID, Type: typeFunctionApp, Name: "fn1"})
	store.Put(&core.Resource{Cloud: "azure", ID: appID, Type: typeContainerApp, Name: "web1"})

	// The two containers a previous Nimbus left running.
	ctx := context.Background()
	if _, err := f.Run(ctx, docker.RunOptions{
		Name:  "nimbus-fnapp-fn1-aaaa",
		Image: "nimbus-fnapp-fn1-aaaa:latest",
		Ports: map[string]string{"127.0.0.1:9700": "80"},
		Labels: map[string]string{
			docker.LabelService: "fn1", docker.LabelResource: siteID,
			docker.LabelRole: roleFunctionApp,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Run(ctx, docker.RunOptions{
		Name:  "nimbus-aca-web1-bbbb",
		Image: "nginx:alpine",
		Ports: map[string]string{"127.0.0.1:9500": "8080"},
		Labels: map[string]string{
			docker.LabelService: "web1", docker.LabelResource: appID,
			docker.LabelRole: "service",
		},
	}); err != nil {
		t.Fatal(err)
	}

	s.reattach(ctx)

	// The function app is in the function-app engine, with its port.
	fn := s.funcapps.Get(siteID)
	if fn == nil {
		t.Fatal("the function app was not adopted; every request to it would 404 and a " +
			"delete could not reach its container")
	}
	if fn.Port != 9700 {
		t.Errorf("the function app came back with port %d, want 9700 -- without it the "+
			"proxy has nothing to forward to", fn.Port)
	}
	if fn.Container != "nimbus-fnapp-fn1-aaaa" {
		t.Errorf("wrong container adopted: %q", fn.Container)
	}

	// And NOT in the container-app engine, nor the other way round.
	if s.apps.Get(siteID) != nil {
		t.Error("the container-app engine adopted a function app; the two must not share " +
			"a role label")
	}
	if s.funcapps.Get(appID) != nil {
		t.Error("the function-app engine adopted a container app")
	}
	if s.apps.Get(appID) == nil {
		t.Error("the container app was not adopted into its own engine")
	}
}

// Deleting an adopted function app reaches its container.
//
// This is the half that actually bit in Milestone 18: the store survives a
// restart and the engine's registry does not, so a delete that looks the
// service up by resource id finds nothing and leaves the container
// running for ever, with nothing left that refers to it.
func TestDeletingAnAdoptedFunctionAppRemovesItsContainer(t *testing.T) {
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{PublicURL: "https://localhost:4701", DataDir: t.TempDir()}, store, bus)
	f := docker.NewFake()
	s.useDocker(f)

	const siteID = "/subscriptions/0/resourceGroups/rg/providers/Microsoft.Web/sites/fn2"
	store.Put(&core.Resource{Cloud: "azure", ID: siteID, Type: typeFunctionApp, Name: "fn2"})

	ctx := context.Background()
	if _, err := f.Run(ctx, docker.RunOptions{
		Name:  "nimbus-fnapp-fn2-cccc",
		Image: "nimbus-fnapp-fn2-cccc:latest",
		Ports: map[string]string{"127.0.0.1:9701": "80"},
		Labels: map[string]string{
			docker.LabelService: "fn2", docker.LabelResource: siteID,
			docker.LabelRole: roleFunctionApp,
		},
	}); err != nil {
		t.Fatal(err)
	}
	s.reattach(ctx)

	deprovisionFunctionApp(s, target{ID: siteID, Name: "fn2"}, map[string]any{
		"kind": "functionapp,linux",
	})

	if left := f.Containers(); len(left) != 0 {
		t.Errorf("deleting an adopted function app left %d container(s) running", len(left))
	}
}

// Deleting an ADOPTED function app removes its image too.
//
// The engine removes an image it remembers building, and after a restart
// it remembers nothing: reattach adopts a container and cannot know
// whether its image was built or pulled. Measured on a real destroy after
// a real restart -- the container went and a 3GB image stayed, which is
// the quietest kind of leak because nothing in `docker ps` shows it.
func TestDeletingAnAdoptedFunctionAppRemovesItsImage(t *testing.T) {
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{PublicURL: "https://localhost:4701", DataDir: t.TempDir()}, store, bus)
	f := docker.NewFake()
	s.useDocker(f)

	const siteID = "/subscriptions/0/resourceGroups/rg/providers/Microsoft.Web/sites/fn3"
	store.Put(&core.Resource{Cloud: "azure", ID: siteID, Type: typeFunctionApp, Name: "fn3"})

	tag := docker.ImageTag("nimbus-fnapp", siteID, "fn3")
	f.Images[tag] = true

	ctx := context.Background()
	if _, err := f.Run(ctx, docker.RunOptions{
		Name: "nimbus-fnapp-fn3-dddd", Image: tag,
		Ports: map[string]string{"127.0.0.1:9702": "80"},
		Labels: map[string]string{
			docker.LabelService: "fn3", docker.LabelResource: siteID,
			docker.LabelRole: roleFunctionApp,
		},
	}); err != nil {
		t.Fatal(err)
	}
	// The restart: the service is adopted, so the engine knows nothing
	// about having built the image.
	s.reattach(ctx)

	deprovisionFunctionApp(s, target{ID: siteID, Name: "fn3"},
		map[string]any{"kind": "functionapp,linux"})

	if present, _ := f.ImagePresent(ctx, tag); present {
		t.Errorf("the built image %s survived the delete; nothing in docker ps would show it", tag)
	}
}
