package aws

import (
	"net/http"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
	"github.com/Asere231/nimbus/internal/engines/funcs"
)

// Deleting a function removes its container, even with --persist.
//
// KeepClusters is what --persist sets, and it means "leave containers up when
// NIMBUS stops" -- a handler or a cluster that survives a restart of the
// emulator is the whole point of the flag. It does not mean "ignore a client
// that deleted the function", and funcs.Engine.Stop used to conflate the two
// by checking the flag itself, one level below the only caller that should
// respect it.
//
// Measured on Windows with --persist: `terraform destroy` of
// examples/terraform/aws-serverless reported "4 destroyed", emptied the
// store, and the function's container was still Up a minute later, with
// nothing left to collect it. Platform-independent, and the same conflation
// was in internal/engines/run, so destroying a Container App or a Cloud Run
// service leaked one too. vm.Engine.Delete and k3s.Engine.Stop had always
// removed unconditionally, which is what made the shape obvious.
//
// The test lives HERE rather than in the engine because this is the level the
// bug bit at -- a client deleting a resource -- and it keeps working however
// the engine's configuration is arranged. The engine no longer has a
// keep-running knob to set, so an engine-level test could not express the
// regression at all.
func TestDeletingAFunctionRemovesItsContainerWithPersist(t *testing.T) {
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{
		PublicURL: "http://localhost:4702", DataDir: t.TempDir(),
		ClusterTimeout: 50 * time.Millisecond,
		KeepClusters:   true, // what --persist sets
	}, store, bus)
	f := docker.NewFake()
	for _, image := range runtimeImages {
		f.Images[image] = true
	}
	s.UseDocker(f)

	createWriter(t, s)
	started := f.Containers()
	if len(started) != 1 {
		t.Fatalf("expected one container from the create, got %v", started)
	}

	// Stand the function in as RUNNING, which is what Put is for.
	//
	// The create's own start cannot get there against the Fake: the
	// readiness probe wants a real port, so Start returns an error and never
	// registers the function -- and it deliberately leaves the container
	// behind, because the error it returns tells you to read `docker logs`
	// on it. Without this the delete would find nothing registered and the
	// test would pass or fail for a reason that has nothing to do with the
	// bug it is about.
	s.lambda.Put(&funcs.Function{
		ResourceID: s.lambdaARN("writer"),
		Name:       "writer",
		Container:  started[0].Name,
	})

	rec, _ := lambdaCall(t, s, http.MethodDelete, "/2015-03-31/functions/writer", "")
	// 204 with no body, which is what the SDK expects for this one.
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d: %s", rec.Code, rec.Body)
	}

	if got := f.Containers(); len(got) != 0 {
		t.Errorf("the function's container survived the delete: %v", got)
	}
}
