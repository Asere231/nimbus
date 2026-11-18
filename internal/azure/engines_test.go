package azure

import (
	"reflect"
	"testing"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
)

// Every engine on the Server gets the swapped Docker client.
//
// This bug has now been made TWICE for the same reason. AWS's UseDocker
// missed s.vms, so that engine kept the real CLI through every test that
// called it -- exactly what the test harness's own comment says must never
// happen, since a handler that starts a container would leave it running
// after the package passed. Then Azure's missed s.funcapps when function
// apps were added, and a reattach test failed because the engine was
// dutifully listing the real daemon's containers.
//
// Reading the list by hand is what let it happen twice, so this counts the
// engines by REFLECTION and compares the count with what useDocker
// actually swaps. Reflection cannot call a method on an unexported field,
// so it cannot check each one directly -- but it can refuse to let a new
// engine be added without this test and useDocker both being updated,
// which is the part that keeps going wrong.
//
// Reading source or structure in a test is what TestEveryModuleImportResolves,
// TestEverySpecTypeHasABadge and TestEveryTestNamedInTheDocsExists already
// do, for the same reason: a claim nothing checks will drift.
func TestEveryEngineGetsTheSwappedDockerClient(t *testing.T) {
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{PublicURL: "https://localhost:4701"}, store, bus)

	// The engines useDocker is known to swap. Keep in step with it.
	swapped := []string{"k3s", "vms", "apps", "funcapps"}

	// Everything on the Server that has a SetDocker method is an engine
	// whose client must be swapped.
	var found []string
	v := reflect.ValueOf(s).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if f.Type.Kind() != reflect.Ptr {
			continue
		}
		if _, ok := f.Type.MethodByName("SetDocker"); ok {
			found = append(found, f.Name)
		}
	}

	if len(found) == 0 {
		t.Fatal("found no engines at all; this test is not looking where it thinks")
	}
	if len(found) != len(swapped) {
		t.Errorf("the Server has %d engines (%v) and useDocker swaps %d (%v).\n\n"+
			"If an engine was just added, add it to useDocker AND to this test's list -- "+
			"an engine left holding the real Docker client leaks containers out of the "+
			"test suite, which is how this was found twice before.",
			len(found), found, len(swapped), swapped)
	}

	// And the swap really takes: a Fake in, nothing real left behind.
	f := docker.NewFake()
	s.useDocker(f)
	if s.docker != docker.Client(f) {
		t.Error("the Server's own client was not swapped")
	}
}
