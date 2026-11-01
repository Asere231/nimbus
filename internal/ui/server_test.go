package ui

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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
	srv := httptest.NewServer(Handler(store, bus, Info{Version: "test", Clouds: map[string]string{"azure": "https://localhost:4701"}}, nil, nil))
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

// The visualizer is ES modules with no build step, so nothing resolves
// imports before a browser does. A typo in a module path is a blank page at
// runtime and nothing at all at compile time -- so the import graph is walked
// here, from index.html's entry point outwards, and every file it reaches must
// really be served.
func TestEveryModuleImportResolves(t *testing.T) {
	srv, _, _ := newUI(t)

	get := func(path string) string {
		t.Helper()
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 {
			t.Fatalf("GET %s = %d", path, res.StatusCode)
		}
		return string(body)
	}

	index := get("/")
	entry := regexp.MustCompile(`<script type="module" src="([^"]+)"`).FindStringSubmatch(index)
	if entry == nil {
		t.Fatal("index.html does not load a module entry point")
	}

	// Relative specifiers only: the visualizer has no CDN and no bare
	// package names, which is the rule this also enforces.
	imports := regexp.MustCompile(`(?m)^\s*(?:import|export)[^"']*from\s*["']([^"']+)["']`)
	seen := map[string]bool{}
	var walk func(file string)
	walk = func(file string) {
		if seen[file] {
			return
		}
		seen[file] = true
		src := get("/" + file)
		for _, m := range imports.FindAllStringSubmatch(src, -1) {
			spec := m[1]
			if !strings.HasPrefix(spec, "./") {
				t.Errorf("%s imports %q: only relative paths are allowed, no CDN and no bare specifiers", file, spec)
				continue
			}
			walk(strings.TrimPrefix(spec, "./"))
		}
	}
	walk(entry[1])

	if len(seen) < 5 {
		t.Errorf("walked only %d modules (%v); the app should be split into more than that", len(seen), seen)
	}
	// fixture.json is fetched by api.js rather than imported, so the walk
	// never reaches it, but ?fixture is broken without it.
	if get("/fixture.json") == "" {
		t.Error("fixture.json is not served")
	}
}

// fakeSecrets stands in for the Key Vault data plane.
type fakeSecrets struct{ reads int }

func (f *fakeSecrets) ListSecrets(vault string) []SecretInfo {
	if vault != "kv1" {
		return nil
	}
	return []SecretInfo{{Name: "db-password", Version: "abc", Versions: 2, Enabled: true, Updated: time.Now()}}
}

func (f *fakeSecrets) ReadSecret(vault, name string) (string, bool) {
	f.reads++
	if vault != "kv1" || name != "db-password" {
		return "", false
	}
	return "s3cret", true
}

// The secret list must carry no values: it is fetched whenever a vault is
// selected, so anything in it is on screen without being asked for. The value
// has its own endpoint, which is what the reveal button calls.
func TestSecretRoutesListNamesAndRevealOnDemand(t *testing.T) {
	bus := core.NewBus(10)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	secrets := &fakeSecrets{}
	srv := httptest.NewServer(Handler(store, bus, Info{Version: "test"}, nil, secrets))
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/api/keyvault/secrets?vault=kv1")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw := make([]byte, 4096)
	n, _ := res.Body.Read(raw)
	listed := string(raw[:n])
	if !strings.Contains(listed, "db-password") {
		t.Fatalf("secret list = %s", listed)
	}
	if strings.Contains(listed, "s3cret") {
		t.Error("the secret list leaked a value")
	}
	if secrets.reads != 0 {
		t.Errorf("listing read %d values; it must read none", secrets.reads)
	}

	res2, err := http.Get(srv.URL + "/api/keyvault/secret?vault=kv1&name=db-password")
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	var got map[string]string
	if err := json.NewDecoder(res2.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["value"] != "s3cret" {
		t.Errorf("reveal returned %v", got)
	}
	if res2.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("a revealed secret must not be cacheable: %q", res2.Header.Get("Cache-Control"))
	}

	res3, _ := http.Get(srv.URL + "/api/keyvault/secret?vault=kv1&name=nope")
	if res3 != nil {
		res3.Body.Close()
		if res3.StatusCode != 404 {
			t.Errorf("unknown secret = %d", res3.StatusCode)
		}
	}
}

// Without a secret browser the endpoints simply are not there, rather than
// answering with an empty list that looks like an empty vault.
func TestSecretRoutesAbsentWithoutABrowser(t *testing.T) {
	srv, _, _ := newUI(t)
	res, err := http.Get(srv.URL + "/api/keyvault/secrets?vault=kv1")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 404 {
		t.Errorf("secrets endpoint = %d with no browser, want 404", res.StatusCode)
	}
}

// The timeline fetches runs as summaries first, then one run with its events
// -- so the picker does not pull every event the journal holds.
func TestRunsAreListedAsSummariesAndFetchedWithEvents(t *testing.T) {
	srv, _, bus := newUI(t)
	now := time.Now().UTC()
	for i, e := range []core.Event{
		{Kind: "request", Action: "call", Method: "GET", Path: "/x", Status: 200},
		{Kind: "resource", Action: "create", Cloud: "azure", ResourceID: "/id/rg", ResourceType: "Microsoft.Resources/resourceGroups", Name: "rg"},
		{Kind: "resource", Action: "create", Cloud: "azure", ResourceID: "/id/v", ResourceType: "Microsoft.Network/virtualNetworks", Name: "v", ParentID: "/id/rg"},
	} {
		e.Time = now.Add(time.Duration(i) * time.Second)
		bus.Publish(e)
	}

	var runs []core.Run
	if code := getJSON(t, srv.URL+"/api/runs", &runs); code != 200 {
		t.Fatalf("GET /api/runs = %d", code)
	}
	if len(runs) != 1 {
		t.Fatalf("listed %d runs, want 1", len(runs))
	}
	if runs[0].Steps != 2 || runs[0].Created != 2 || runs[0].Requests != 1 {
		t.Errorf("summary = %+v", runs[0])
	}
	if runs[0].Events != nil {
		t.Error("the list must not carry every run's events")
	}

	var run core.Run
	if code := getJSON(t, srv.URL+"/api/run?id="+url.QueryEscape(runs[0].ID), &run); code != 200 {
		t.Fatalf("GET /api/run = %d", code)
	}
	if len(run.Events) != 3 {
		t.Fatalf("the run carries %d events, want 3", len(run.Events))
	}
	// Replay rebuilds the graph from these alone, so each one has to name a
	// resource fully: no second lookup is possible for something since
	// deleted.
	last := run.Events[2]
	if last.ResourceID == "" || last.ResourceType == "" || last.Name == "" || last.ParentID == "" {
		t.Errorf("a resource event is missing what a card needs to be drawn: %+v", last)
	}

	if code := getJSON(t, srv.URL+"/api/run?id=nope", nil); code != 404 {
		t.Errorf("GET /api/run for an unknown id = %d, want 404", code)
	}
}
