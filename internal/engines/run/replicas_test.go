package run

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// Replicas, because `minReplicas: 3` used to run one container.
//
// It round-tripped as state and nothing acted on it, so a configuration
// asking for three replicas applied cleanly and ran one -- the same silent
// shape sidecars had. Now the count is real: N groups, each its own
// container with its own published port and its own sidecars in its own
// namespace, and requests are spread across them.
//
// `maxReplicas` is still inert, and that is honest rather than lazy:
// scaling OUT means reacting to load, and nothing here measures load. A
// local emulator has no traffic to autoscale against.

func newReplicaEngine(t *testing.T) (*Engine, *docker.Fake) {
	t.Helper()
	f := docker.NewFake()
	f.Images["nginx:alpine"] = true
	f.Images["redis:alpine"] = true
	// Bounded tightly: the readiness probe is a real HTTP GET and cannot
	// succeed against the Fake, so the default three minutes would be
	// waited out once per replica.
	return New(Config{NamePrefix: "nimbus-app", StartTimeout: 50 * time.Millisecond}, f), f
}

func TestAServiceRunsTheReplicasItWasAskedFor(t *testing.T) {
	e, f := newReplicaEngine(t)

	svc := &Service{
		ResourceID: "/subscriptions/x/app/web", Name: "web",
		Image: "nginx:alpine", Replicas: 3,
	}
	_ = e.Start(context.Background(), svc)

	if got := f.Containers(); len(got) != 3 {
		t.Fatalf("expected three replica containers, got %d: %v", len(got), names(got))
	}
	// Each on its own host port, or two replicas would collide and the
	// second would fail to start.
	ports := map[int]bool{svc.Port: true}
	for _, r := range svc.Extra {
		if r.Port == 0 {
			t.Errorf("replica %s has no port", r.Container)
		}
		if ports[r.Port] {
			t.Errorf("two replicas share port %d", r.Port)
		}
		ports[r.Port] = true
	}
	if len(ports) != 3 {
		t.Errorf("expected three distinct ports, got %v", ports)
	}
}

// One replica is the default and must behave exactly as before: the same
// container name, so a reattach finds it and nothing about the existing
// single-container path changes.
func TestOneReplicaIsUnchanged(t *testing.T) {
	e, f := newReplicaEngine(t)

	plain := &Service{ResourceID: "/subscriptions/x/app/web", Name: "web", Image: "nginx:alpine"}
	_ = e.Start(context.Background(), plain)
	if got := f.Containers(); len(got) != 1 {
		t.Fatalf("a service with no Replicas should run one container, got %v", names(got))
	}
	if len(plain.Extra) != 0 {
		t.Errorf("a single-replica service should have no extra replicas: %+v", plain.Extra)
	}
	plainName := plain.Container

	// Replicas: 1 must name the container identically, so setting the
	// field explicitly is not a redeploy.
	e2, _ := newReplicaEngine(t)
	one := &Service{ResourceID: "/subscriptions/x/app/web", Name: "web",
		Image: "nginx:alpine", Replicas: 1}
	_ = e2.Start(context.Background(), one)
	if one.Container != plainName {
		t.Errorf("Replicas:1 named the container %q, want %q -- the same as no Replicas at all",
			one.Container, plainName)
	}
}

// Every replica gets its own sidecars, in its own namespace.
//
// Sharing one replica's sidecars between replicas would be wrong in the
// way that matters: a sidecar exists to be reachable on localhost from the
// container beside it, and localhost is per namespace.
func TestEachReplicaGetsItsOwnSidecars(t *testing.T) {
	e, f := newReplicaEngine(t)

	svc := &Service{
		ResourceID: "/subscriptions/x/app/web", Name: "web", Image: "nginx:alpine",
		Replicas: 2,
		Sidecars: []Sidecar{{Name: "cache", Image: "redis:alpine"}},
	}
	_ = e.Start(context.Background(), svc)

	// Two replicas, each with one sidecar.
	if got := f.Containers(); len(got) != 4 {
		t.Fatalf("expected two replicas and two sidecars, got %d: %v", len(got), names(got))
	}

	// Each sidecar must join ITS OWN replica's namespace, not the first's.
	namespaces := map[string]bool{}
	for _, opts := range f.Runs {
		if opts.Labels[docker.LabelRole] != "sidecar" {
			continue
		}
		if opts.Network == "" {
			t.Errorf("sidecar %s joined no namespace", opts.Name)
			continue
		}
		if namespaces[opts.Network] {
			t.Errorf("two sidecars share the namespace %q, so one replica has both",
				opts.Network)
		}
		namespaces[opts.Network] = true
	}
	if len(namespaces) != 2 {
		t.Errorf("expected one namespace per replica, got %v", namespaces)
	}
}

// Scaling down removes the replicas that are no longer wanted.
//
// The containers are named from the resource id and an index, so a service
// that goes from three replicas to one would otherwise leave two running
// with nothing referring to them -- the sidecar leak again, by a third
// route.
func TestScalingDownRemovesTheExtraReplicas(t *testing.T) {
	e, f := newReplicaEngine(t)

	const id = "/subscriptions/x/app/web"
	_ = e.Start(context.Background(), &Service{
		ResourceID: id, Name: "web", Image: "nginx:alpine", Replicas: 3,
	})
	if got := f.Containers(); len(got) != 3 {
		t.Fatalf("setup: expected three containers, got %v", names(got))
	}

	_ = e.Start(context.Background(), &Service{
		ResourceID: id, Name: "web", Image: "nginx:alpine", Replicas: 1,
	})
	if got := f.Containers(); len(got) != 1 {
		t.Errorf("scaling down to one left %v running", names(got))
	}
}

// Deleting the service takes every replica with it.
func TestStoppingAServiceRemovesEveryReplica(t *testing.T) {
	e, f := newReplicaEngine(t)

	svc := &Service{
		ResourceID: "/subscriptions/x/app/web", Name: "web",
		Image: "nginx:alpine", Replicas: 3,
	}
	_ = e.Start(context.Background(), svc)
	e.Stop(context.Background(), svc)

	if left := f.Containers(); len(left) != 0 {
		t.Errorf("stopping a three-replica service left %v behind", names(left))
	}
}

// A replica is collected even when nothing in memory knows about it, which
// is the state a restart leaves: the store knows the app and the registry
// knows nothing about the extra replicas.
func TestAReplicaIsCollectedEvenWhenUnregistered(t *testing.T) {
	e, f := newReplicaEngine(t)

	const id = "/subscriptions/x/app/web"
	svc := &Service{ResourceID: id, Name: "web", Image: "nginx:alpine", Replicas: 3}
	_ = e.Start(context.Background(), svc)

	// What a reattach can reconstruct: the main container and its port,
	// and nothing about the others.
	adopted := &Service{ResourceID: id, Name: "web", Image: "nginx:alpine",
		Container: svc.Container, Port: svc.Port}
	e.Stop(context.Background(), adopted)

	if left := f.Containers(); len(left) != 0 {
		t.Errorf("deleting an adopted app left %v behind; replicas outlived their app",
			names(left))
	}
}

// A ridiculous replica count is refused rather than attempted.
//
// `minReplicas` goes to 1000 in real Azure. Starting a thousand containers
// on a laptop is not a scaling test, it is a way to lose the machine -- so
// the count is capped and the refusal says so, instead of grinding to a
// halt with nothing explaining why.
func TestAnAbsurdReplicaCountIsRefusedAndSaysSo(t *testing.T) {
	e, _ := newReplicaEngine(t)

	err := e.Start(context.Background(), &Service{
		ResourceID: "/subscriptions/x/app/web", Name: "web",
		Image: "nginx:alpine", Replicas: 500,
	})
	if err == nil {
		t.Fatal("500 replicas was attempted rather than refused")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "MaxReplicas") {
		t.Errorf("the refusal does not name the count and the cap: %v", err)
	}
}

// Requests are spread across the replicas.
//
// Without this a three-replica service runs three containers and sends
// every request to the first, which is a worse lie than running one: the
// configuration looks honoured and the behaviour is not.
func TestRequestsAreSpreadAcrossReplicas(t *testing.T) {
	// Three real servers standing in for three replicas, each naming
	// itself, so the test can see which one answered.
	var servers []*httptest.Server
	ports := []int{}
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("replica-%d", i)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, name)
		}))
		t.Cleanup(srv.Close)
		servers = append(servers, srv)
		ports = append(ports, portOf(t, srv.URL))
	}

	e, _ := newReplicaEngine(t)
	svc := &Service{
		ResourceID: "/subscriptions/x/app/web", Name: "web", Image: "nginx:alpine",
		Container: "nimbus-app-web-1", Port: ports[0], Replicas: 3,
		Extra: []Replica{
			{Container: "nimbus-app-web-2", Port: ports[1]},
			{Container: "nimbus-app-web-3", Port: ports[2]},
		},
	}
	e.Put(svc)

	seen := map[string]int{}
	for i := 0; i < 9; i++ {
		rec := httptest.NewRecorder()
		if err := e.Proxy(rec, httptest.NewRequest(http.MethodGet, "/", nil), svc, ""); err != nil {
			t.Fatalf("proxying: %v", err)
		}
		seen[rec.Body.String()]++
	}
	if len(seen) != 3 {
		t.Errorf("nine requests reached %d of three replicas: %v", len(seen), seen)
	}
	for name, n := range seen {
		if n != 3 {
			t.Errorf("%s answered %d of nine requests; round robin should give each three",
				name, n)
		}
	}
}

// A single-replica service proxies exactly as it always did.
func TestASingleReplicaStillProxies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "only")
	}))
	t.Cleanup(srv.Close)

	e, _ := newReplicaEngine(t)
	svc := &Service{
		ResourceID: "/subscriptions/x/app/web", Name: "web",
		Container: "nimbus-app-web-1", Port: portOf(t, srv.URL),
	}
	e.Put(svc)

	rec := httptest.NewRecorder()
	if err := e.Proxy(rec, httptest.NewRequest(http.MethodGet, "/", nil), svc, ""); err != nil {
		t.Fatalf("proxying: %v", err)
	}
	if rec.Body.String() != "only" {
		t.Errorf("single-replica proxy returned %q", rec.Body.String())
	}
}

// Logs come from every replica, not only the first.
//
// A workspace that holds one replica's console out of three is the kind of
// half-working feature this project would rather not have: nothing says
// the other two are missing, and their output is where the interesting
// thing usually is.
func TestLogsComeFromEveryReplica(t *testing.T) {
	e, f := newReplicaEngine(t)
	// The Fake only has logs for a container it knows about, so both
	// replicas are stood up first -- which is also the state a real
	// two-replica service is in.
	for _, name := range []string{"nimbus-app-web-1", "nimbus-app-web-2"} {
		if _, err := f.Run(context.Background(), docker.RunOptions{
			Name: name, Image: "nginx:alpine",
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.LogOutput["nimbus-app-web-1"] = []byte("from one\n")
	f.LogOutput["nimbus-app-web-2"] = []byte("from two\n")

	svc := &Service{
		ResourceID: "/subscriptions/x/app/web", Name: "web",
		Container: "nimbus-app-web-1", Port: 9300,
		Extra: []Replica{{Container: "nimbus-app-web-2", Port: 9301}},
	}

	out, err := e.Logs(context.Background(), svc, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"from one", "from two"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("%q is missing from the service's logs: %q", want, out)
		}
	}
}

func portOf(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A restart adopts every replica, with the right one as the primary.
//
// Reattach used to Put one Service per container it found, so the LAST
// container listed won and Extra was empty: a three-replica app came back
// serving from one container with the other two running and unreferenced.
// Not a leak -- removeGroup sweeps by label -- but a silent halving of
// what the configuration asked for, which is the shape this whole item
// exists to remove.
//
// The primary has to be identified rather than guessed, because Docker's
// listing order is not the start order and `svc.Port` is what the FQDN
// reaches.
func TestARestartAdoptsEveryReplica(t *testing.T) {
	e, f := newReplicaEngine(t)

	const id = "/subscriptions/x/app/web"
	svc := &Service{
		ResourceID: id, Name: "web", Image: "nginx:alpine", Replicas: 3,
		Sidecars: []Sidecar{{Name: "cache", Image: "redis:alpine"}},
	}
	_ = e.Start(context.Background(), svc)
	wantPrimary, wantPort := svc.Container, svc.Port

	// The restart: a fresh engine over the same containers.
	fresh := New(Config{NamePrefix: "nimbus-app", StartTimeout: 50 * time.Millisecond}, f)
	fresh.Reattach(context.Background(), func(string) bool { return true })

	got := fresh.Get(id)
	if got == nil {
		t.Fatal("the service was not adopted at all")
	}
	if got.Container != wantPrimary {
		t.Errorf("adopted %q as the primary, want %q -- the FQDN reaches svc.Port",
			got.Container, wantPrimary)
	}
	if got.Port != wantPort {
		t.Errorf("the primary's port came back %d, want %d", got.Port, wantPort)
	}
	if len(got.Extra) != 2 {
		t.Fatalf("expected two extra replicas, got %d: %+v", len(got.Extra), got.Extra)
	}
	for _, rep := range got.Extra {
		if rep.Port == 0 {
			t.Errorf("replica %s was adopted with no port, so nothing can reach it",
				rep.Container)
		}
		if rep.Container == wantPrimary {
			t.Errorf("the primary was also adopted as an extra replica")
		}
	}

	// And one Service, not three: three would mean two of them were
	// overwriting each other in the registry.
	if all := fresh.All(); len(all) != 1 {
		t.Errorf("expected one adopted service, got %d", len(all))
	}
}

// A single-replica service is adopted exactly as before.
func TestARestartAdoptsASingleReplicaUnchanged(t *testing.T) {
	e, f := newReplicaEngine(t)

	const id = "/subscriptions/x/app/web"
	svc := &Service{ResourceID: id, Name: "web", Image: "nginx:alpine"}
	_ = e.Start(context.Background(), svc)

	fresh := New(Config{NamePrefix: "nimbus-app", StartTimeout: 50 * time.Millisecond}, f)
	found := fresh.Reattach(context.Background(), func(string) bool { return true })
	if len(found) != 1 {
		t.Fatalf("expected one adopted service, got %d", len(found))
	}
	got := fresh.Get(id)
	if got.Container != svc.Container || got.Port != svc.Port {
		t.Errorf("adopted %q:%d, want %q:%d", got.Container, got.Port, svc.Container, svc.Port)
	}
	if len(got.Extra) != 0 {
		t.Errorf("a single-replica service came back with extras: %+v", got.Extra)
	}
}
