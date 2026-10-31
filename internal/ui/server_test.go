package ui

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/core"
)

func newUI(t *testing.T) (*httptest.Server, *core.Store, *core.Bus) {
	t.Helper()
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(store, bus, Info{Version: "test", Clouds: map[string]string{"azure": "https://localhost:4701"}}, nil))
	t.Cleanup(srv.Close)
	return srv, store, bus
}

func seed(store *core.Store) {
	store.Put(&core.Resource{Cloud: "azure", ID: "/subscriptions/s/resourceGroups/rg", Type: "Microsoft.Resources/resourceGroups", Name: "rg", Backing: "state", Body: []byte(`{"name":"rg"}`)})
	store.Put(&core.Resource{Cloud: "azure", ID: "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/v", Type: "Microsoft.Network/virtualNetworks", Name: "v", ParentID: "/subscriptions/s/resourceGroups/rg", Backing: "state", Body: []byte(`{"name":"v"}`)})
	store.Put(&core.Resource{Cloud: "aws", ID: "arn:aws:s3:::bucket", Type: "AWS::S3::Bucket", Name: "bucket", Backing: "state", Body: []byte(`{"name":"bucket"}`)})
}

func getJSON(t *testing.T, url string, into any) int {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if into != nil && res.StatusCode < 300 {
		if err := json.NewDecoder(res.Body).Decode(into); err != nil {
			t.Fatalf("%s: decoding response: %v", url, err)
		}
	}
	return res.StatusCode
}

func TestInfoReportsEndpoints(t *testing.T) {
	srv, _, _ := newUI(t)
	var info Info
	if code := getJSON(t, srv.URL+"/api/info", &info); code != 200 {
		t.Fatalf("GET /api/info = %d", code)
	}
	if info.Version != "test" || info.Clouds["azure"] == "" {
		t.Errorf("info = %+v", info)
	}
}

func TestResourcesListAndCloudFilter(t *testing.T) {
	srv, store, _ := newUI(t)
	seed(store)

	var all []core.Resource
	if code := getJSON(t, srv.URL+"/api/resources", &all); code != 200 {
		t.Fatalf("GET /api/resources = %d", code)
	}
	if len(all) != 3 {
		t.Errorf("got %d resources, want 3", len(all))
	}

	var az []core.Resource
	getJSON(t, srv.URL+"/api/resources?cloud=azure", &az)
	if len(az) != 2 {
		t.Errorf("cloud=azure returned %d, want 2", len(az))
	}
	for _, r := range az {
		if r.Cloud != "azure" {
			t.Errorf("cloud filter leaked %s", r.Cloud)
		}
	}

	var none []core.Resource
	getJSON(t, srv.URL+"/api/resources?cloud=gcp", &none)
	if len(none) != 0 {
		t.Errorf("cloud=gcp returned %d, want 0", len(none))
	}
}

func TestResourceLookup(t *testing.T) {
	srv, store, _ := newUI(t)
	seed(store)

	var r core.Resource
	if code := getJSON(t, srv.URL+"/api/resource?id=/subscriptions/s/resourceGroups/rg", &r); code != 200 {
		t.Fatalf("GET /api/resource = %d", code)
	}
	if r.Name != "rg" {
		t.Errorf("name = %q", r.Name)
	}

	// IDs are case-insensitive here too: the inspector is reached from links
	// that may carry any casing.
	if code := getJSON(t, srv.URL+"/api/resource?id=/SUBSCRIPTIONS/s/RESOURCEGROUPS/rg", nil); code != 200 {
		t.Errorf("lookup with different casing = %d, want 200", code)
	}
	if code := getJSON(t, srv.URL+"/api/resource?id=/nope", nil); code != 404 {
		t.Errorf("missing resource = %d, want 404", code)
	}
	if code := getJSON(t, srv.URL+"/api/resource", nil); code != 404 {
		t.Errorf("no id parameter = %d, want 404", code)
	}
}

func TestResetIsPostOnly(t *testing.T) {
	srv, store, _ := newUI(t)
	seed(store)

	if code := getJSON(t, srv.URL+"/api/reset", nil); code != 405 {
		t.Errorf("GET /api/reset = %d, want 405", code)
	}
	if len(store.List(nil)) != 3 {
		t.Error("a GET must not have reset anything")
	}

	res, err := http.Post(srv.URL+"/api/reset", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("POST /api/reset = %d", res.StatusCode)
	}
	if n := len(store.List(nil)); n != 0 {
		t.Errorf("%d resources survived the reset", n)
	}
}

// A browser that connects mid-apply must still see what already happened, so
// the stream replays history before it starts pushing live events.
func TestEventStreamReplaysHistoryThenStreamsLive(t *testing.T) {
	srv, store, bus := newUI(t)
	seed(store) // three create events are now in history

	req, _ := http.NewRequest("GET", srv.URL+"/api/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	type sseEvent struct {
		Kind       string `json:"kind"`
		Action     string `json:"action"`
		ResourceID string `json:"resourceId"`
	}
	events := make(chan sseEvent, 16)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var e sseEvent
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e) == nil {
				events <- e
			}
		}
	}()

	// The first message must be the first historical event, not a live one.
	select {
	case e := <-events:
		if e.Action != "create" || e.ResourceID != "/subscriptions/s/resourceGroups/rg" {
			t.Errorf("first streamed event = %+v, want the first create from history", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no history replayed on connect")
	}

	// Drain the rest of the history, then prove live events arrive.
	for i := 0; i < 2; i++ {
		select {
		case <-events:
		case <-time.After(3 * time.Second):
			t.Fatal("history was truncated")
		}
	}

	bus.Publish(core.Event{Kind: "resource", Action: "delete", ResourceID: "/live"})
	select {
	case e := <-events:
		if e.ResourceID != "/live" || e.Action != "delete" {
			t.Errorf("live event = %+v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("live event never arrived")
	}
}

// The web app is embedded, so it must be served without a build step or a CDN.
func TestStaticAppIsEmbedded(t *testing.T) {
	srv, _, _ := newUI(t)
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body := make([]byte, 512)
		n, _ := res.Body.Read(body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Errorf("GET %s = %d", path, res.StatusCode)
		}
		if n == 0 {
			t.Errorf("GET %s served an empty file", path)
		}
	}
}
