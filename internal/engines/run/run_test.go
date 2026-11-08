package run

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

func newTestEngine(t *testing.T) (*Engine, *docker.Fake) {
	t.Helper()
	f := docker.NewFake()
	f.Images = map[string]bool{"nginx:alpine": true}
	// Short, because the tests that reach the readiness probe are the ones
	// asserting that it gives up with a legible message.
	return New(Config{NamePrefix: "nimbus-run", PortBase: 19300,
		StartTimeout: 300 * time.Millisecond}, f), f
}

// PORT is the contract a Cloud Run container is written to: it is told
// where to listen rather than being assumed to have guessed.
func TestStartTellsTheContainerWhichPortToListenOn(t *testing.T) {
	e, f := newTestEngine(t)
	svc := &Service{ResourceID: "projects/p/locations/l/services/hello", Name: "hello",
		Image: "nginx:alpine", Env: map[string]string{"GREETING": "hi"}}

	// The readiness probe will fail -- nothing is listening behind a fake
	// container -- but the container was still described and started.
	_ = e.Start(context.Background(), svc)

	if len(f.Runs) != 1 {
		t.Fatalf("started %d containers, want 1", len(f.Runs))
	}
	opts := f.Runs[0]
	if opts.Env["PORT"] != fmt.Sprintf("%d", DefaultContainerPort) {
		t.Errorf("PORT = %q, want %d", opts.Env["PORT"], DefaultContainerPort)
	}
	if opts.Env["GREETING"] != "hi" {
		t.Errorf("the caller's own environment was dropped: %v", opts.Env)
	}
	if len(opts.Volumes) != 0 {
		t.Errorf("a service IS its image; nothing should be mounted: %v", opts.Volumes)
	}
	if opts.Labels[docker.LabelResource] != svc.ResourceID {
		t.Errorf("labels = %v, want the resource id so a restart can find it", opts.Labels)
	}
}

// A container that listens somewhere other than 8080 says so, and the
// publish has to follow it or nothing ever answers.
func TestStartHonoursAContainerPort(t *testing.T) {
	e, f := newTestEngine(t)
	svc := &Service{ResourceID: "id", Name: "hello", Image: "nginx:alpine", ContainerPort: 3000}
	_ = e.Start(context.Background(), svc)

	opts := f.Runs[0]
	published := false
	for host, container := range opts.Ports {
		if container == "3000" && strings.HasPrefix(host, "127.0.0.1:") {
			published = true
		}
	}
	if !published {
		t.Errorf("ports = %v, want the container's own 3000 published", opts.Ports)
	}
	if opts.Env["PORT"] != "3000" {
		t.Errorf("PORT = %q, want 3000", opts.Env["PORT"])
	}
}

// A container that never answers must produce a message that says what to
// look at, not a bare timeout.
func TestStartGivesUpWithSomethingToRead(t *testing.T) {
	e, _ := newTestEngine(t)
	err := e.Start(context.Background(), &Service{ResourceID: "id", Name: "hello", Image: "nginx:alpine"})
	if err == nil {
		t.Fatal("a container nothing is listening behind reported success")
	}
	if !strings.Contains(err.Error(), "docker logs") {
		t.Errorf("the error does not say where to look: %v", err)
	}
}

// Docker not running must say so, rather than hanging or reporting a
// transport error.
func TestStartSaysWhenDockerIsNotRunning(t *testing.T) {
	e, f := newTestEngine(t)
	f.Unavailable = true
	err := e.Start(context.Background(), &Service{ResourceID: "id", Name: "hello", Image: "nginx:alpine"})
	if err == nil || !strings.Contains(err.Error(), "Docker") {
		t.Fatalf("error = %v, want one naming Docker", err)
	}
}

func TestStartPullsAnImageItDoesNotHave(t *testing.T) {
	e, f := newTestEngine(t)
	_ = e.Start(context.Background(), &Service{ResourceID: "id", Name: "hello", Image: "example/absent:1"})
	pulled := false
	for _, c := range f.Calls {
		pulled = pulled || strings.Contains(c, "pull example/absent:1")
	}
	if !pulled {
		t.Errorf("the image was not pulled: %v", f.Calls)
	}
}

// --- the proxy ------------------------------------------------------------

// The proxy is the part a request really goes through, so it is tested
// against a real server rather than a fake: method, path, headers, body
// and status all have to survive the trip.
func TestProxyForwardsTheWholeRequestAndTheWholeResponse(t *testing.T) {
	type seen struct {
		method, path, body, header string
	}
	got := make(chan seen, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(body)
		}
		got <- seen{r.Method, r.URL.Path, string(body), r.Header.Get("X-Test")}
		w.Header().Set("X-From-Service", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	e, _ := newTestEngine(t)
	svc := serviceAt(t, backend.URL)
	e.Put(svc)

	req := httptest.NewRequest(http.MethodPost, "/_nimbus/run/hello/api/thing",
		strings.NewReader(`{"in":1}`))
	req.Header.Set("X-Test", "header")
	rec := httptest.NewRecorder()
	if err := e.Proxy(rec, req, svc, "/_nimbus/run/hello"); err != nil {
		t.Fatal(err)
	}

	s := <-got
	if s.method != http.MethodPost {
		t.Errorf("method = %s", s.method)
	}
	// The service sees the path it expects, with the proxy's prefix gone.
	if s.path != "/api/thing" {
		t.Errorf("path = %q, want /api/thing", s.path)
	}
	if s.body != `{"in":1}` {
		t.Errorf("body = %q", s.body)
	}
	if s.header != "header" {
		t.Errorf("the request's headers did not survive: %q", s.header)
	}

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want the service's own 201", rec.Code)
	}
	if rec.Header().Get("X-From-Service") != "yes" {
		t.Errorf("the response's headers did not survive: %v", rec.Header())
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// A dead container must not produce a blank 502: the message is the only
// thing the person has to go on.
func TestProxyToADeadContainerSaysWhichService(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := backend.URL
	backend.Close() // nothing is listening there any more

	e, _ := newTestEngine(t)
	svc := serviceAt(t, url)
	e.Put(svc)

	rec := httptest.NewRecorder()
	if err := e.Proxy(rec, httptest.NewRequest(http.MethodGet, "/_nimbus/run/hello", nil),
		svc, "/_nimbus/run/hello"); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "hello") {
		t.Errorf("the message does not name the service: %q", rec.Body.String())
	}
}

func TestProxyRefusesAServiceWithNoPort(t *testing.T) {
	e, _ := newTestEngine(t)
	svc := &Service{ResourceID: "id", Name: "hello"}
	err := e.Proxy(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/_nimbus/run/hello", nil), svc, "/_nimbus/run/hello")
	if err == nil {
		t.Fatal("proxying to a service that was never started reported success")
	}
}

// --- the registry ---------------------------------------------------------

func TestStopForgetsTheServiceAndRemovesItsContainer(t *testing.T) {
	e, f := newTestEngine(t)
	svc := &Service{ResourceID: "id", Name: "hello", Image: "nginx:alpine", Container: "nimbus-run-hello"}
	e.Put(svc)
	e.Stop(context.Background(), svc)

	if e.Get("id") != nil {
		t.Error("the service is still registered after Stop")
	}
	removed := false
	for _, c := range f.Calls {
		removed = removed || strings.Contains(c, "remove nimbus-run-hello")
	}
	if !removed {
		t.Errorf("the container was not removed: %v", f.Calls)
	}
}

// KeepRunning is what --persist means: the container outlives Nimbus so a
// restart finds it still there.
func TestStopLeavesTheContainerWhenItIsMeantToOutliveUs(t *testing.T) {
	f := docker.NewFake()
	e := New(Config{NamePrefix: "nimbus-run", KeepRunning: true}, f)
	svc := &Service{ResourceID: "id", Name: "hello", Container: "nimbus-run-hello"}
	e.Put(svc)
	e.Stop(context.Background(), svc)

	for _, c := range f.Calls {
		if strings.Contains(c, "remove") {
			t.Errorf("the container was removed despite KeepRunning: %v", f.Calls)
		}
	}
}

func TestAllIsOrderedSoShutdownIsDeterministic(t *testing.T) {
	e, _ := newTestEngine(t)
	e.Put(&Service{ResourceID: "b", Name: "b"})
	e.Put(&Service{ResourceID: "a", Name: "a"})
	e.Put(&Service{ResourceID: "c", Name: "c"})

	var ids []string
	for _, svc := range e.All() {
		ids = append(ids, svc.ResourceID)
	}
	if strings.Join(ids, ",") != "a,b,c" {
		t.Errorf("All() = %v, want resource-id order", ids)
	}
}

func TestStripPrefixAlwaysLeavesAnAbsolutePath(t *testing.T) {
	for _, c := range []struct{ path, prefix, want string }{
		{"/_nimbus/run/hello", "/_nimbus/run/hello", "/"},
		{"/_nimbus/run/hello/", "/_nimbus/run/hello", "/"},
		{"/_nimbus/run/hello/deep/path", "/_nimbus/run/hello", "/deep/path"},
		{"/unrelated", "/_nimbus/run/hello", "/unrelated"},
		{"/_nimbus/run/hello", "", "/_nimbus/run/hello"},
	} {
		if got := StripPrefix(c.path, c.prefix); got != c.want {
			t.Errorf("StripPrefix(%q, %q) = %q, want %q", c.path, c.prefix, got, c.want)
		}
	}
}

// serviceAt builds a Service pointing at an already-running HTTP server,
// which is how the proxy is tested without a container.
func serviceAt(t *testing.T, raw string) *Service {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port := 0
	if _, err := fmt.Sscanf(u.Port(), "%d", &port); err != nil {
		t.Fatalf("no port in %q: %v", raw, err)
	}
	return &Service{ResourceID: "projects/p/locations/l/services/hello", Name: "hello",
		Image: "nginx:alpine", Container: "nimbus-run-hello", Port: port}
}
