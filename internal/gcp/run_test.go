package gcp

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
	"github.com/Asere231/nimbus/internal/engines/run"
)

// Captured from the google provider 6.50.0 against a logging server.

const runPath = "/v2/projects/nimbus-local/locations/us-central1/services"

// The create body carries no name: the name is in `?serviceId=`.
const runBody = `{
	"labels": {"goog-terraform-provisioned": "true"},
	"template": {"containers": [{"image": "nginx:alpine"}]}
}`

func newRunTestServer(t *testing.T) *Server {
	t.Helper()
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{PublicURL: base, DataDir: t.TempDir()}, store, bus)
	f := docker.NewFake()
	f.Unavailable = true
	s.UseDocker(f)
	return s
}

func TestCreateServiceTakesItsNameFromTheQueryParameter(t *testing.T) {
	s := newRunTestServer(t)
	doc := must(t, s, http.MethodPost, runPath+"?alt=json&serviceId=hello", runBody)

	// The answer is a longrunning Operation, with a resource NAME rather
	// than a URL -- a different document from Compute's and GKE's.
	name, _ := doc["name"].(string)
	if !strings.HasPrefix(name, "projects/nimbus-local/locations/us-central1/operations/") {
		t.Fatalf("operation name = %q, want a resource name", name)
	}

	svc := must(t, s, http.MethodGet, runPath+"/hello", "")
	if svc["name"] != "projects/nimbus-local/locations/us-central1/services/hello" {
		t.Fatalf("service name = %v, want the one from serviceId", svc["name"])
	}
}

// THE asymmetry of this milestone. GKE's provider polls the resource and
// ignores the operation; Cloud Run's polls the operation and ignores the
// resource's own terminalCondition. Measured both ways. So a create's
// operation must NOT be done until the container is up, or Terraform
// reports a service as ready while nothing is listening.
func TestACreatesOperationIsNotDoneBeforeTheContainerIs(t *testing.T) {
	s := newRunTestServer(t)
	doc := must(t, s, http.MethodPost, runPath+"?serviceId=hello", runBody)
	if doc["done"] == true {
		t.Fatal("the create's operation said done immediately: the provider polls " +
			"THIS, so it would report a service as ready before anything answered")
	}
}

// With Docker unavailable the container cannot start, and the failure has
// to reach the client through the operation -- that is what makes a bad
// image a readable error instead of a timeout.
func TestAFailedStartFinishesTheOperationWithTheReason(t *testing.T) {
	s := newRunTestServer(t)
	doc := must(t, s, http.MethodPost, runPath+"?serviceId=hello", runBody)
	opName := doc["name"].(string)

	if !waitFor(func() bool {
		op := s.lros.get(opName)
		return op != nil && op.Done
	}) {
		t.Fatal("the operation never finished, so a client would poll for ever")
	}
	op := s.lros.get(opName)
	if op.Error == nil || op.Error.Message == "" {
		t.Fatalf("a failed start left no error on the operation: %+v", op)
	}
	if !strings.Contains(strings.ToLower(op.Error.Message), "docker") {
		t.Errorf("the message does not say what went wrong: %q", op.Error.Message)
	}
}

// uri has to be stable: Terraform stores it, a push subscription stores
// it, a person pastes it. A container's port changes on every start, so
// the address points at Nimbus, which proxies.
func TestServiceURIIsStableAcrossARedeploy(t *testing.T) {
	s := newRunTestServer(t)
	must(t, s, http.MethodPost, runPath+"?serviceId=hello", runBody)
	first := must(t, s, http.MethodGet, runPath+"/hello", "")

	must(t, s, http.MethodPatch, runPath+"/hello",
		`{"template": {"containers": [{"image": "nginx:1.27-alpine"}]}}`)
	second := must(t, s, http.MethodGet, runPath+"/hello", "")

	if first["uri"] != second["uri"] {
		t.Errorf("uri moved on redeploy: %v -> %v", first["uri"], second["uri"])
	}
	if first["uri"] != base+RunProxyPrefix+"hello" {
		t.Errorf("uri = %v, want the stable proxy address", first["uri"])
	}
}

// Numbers travel as decimal STRINGS. A JSON number lands in a client's
// string field as the zero value, silently.
func TestGenerationIsAStringAndRisesOnRedeploy(t *testing.T) {
	s := newRunTestServer(t)
	must(t, s, http.MethodPost, runPath+"?serviceId=hello", runBody)
	first := must(t, s, http.MethodGet, runPath+"/hello", "")
	if _, ok := first["generation"].(string); !ok {
		t.Fatalf("generation = %#v, want a decimal string", first["generation"])
	}
	if first["generation"] != "1" {
		t.Errorf("generation = %v, want \"1\"", first["generation"])
	}

	must(t, s, http.MethodPatch, runPath+"/hello",
		`{"template": {"containers": [{"image": "nginx:1.27-alpine"}]}}`)
	second := must(t, s, http.MethodGet, runPath+"/hello", "")
	if second["generation"] != "2" {
		t.Errorf("generation after a redeploy = %v, want \"2\"", second["generation"])
	}
}

// A redeploy leaves a revision behind, so the graph has the history.
func TestARedeployAddsARevision(t *testing.T) {
	s := newRunTestServer(t)
	must(t, s, http.MethodPost, runPath+"?serviceId=hello", runBody)
	must(t, s, http.MethodPatch, runPath+"/hello",
		`{"template": {"containers": [{"image": "nginx:1.27-alpine"}]}}`)

	doc := must(t, s, http.MethodGet, runPath+"/hello/revisions", "")
	revisions, _ := doc["revisions"].([]any)
	if len(revisions) != 2 {
		t.Fatalf("got %d revisions after one redeploy, want 2", len(revisions))
	}
}

func TestCreateWithoutAnImageIsRefused(t *testing.T) {
	s := newRunTestServer(t)
	rec, doc := call(t, s, http.MethodPost, runPath+"?serviceId=hello", `{"template": {}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	e, _ := doc["error"].(map[string]any)
	if e == nil || e["status"] != "INVALID_ARGUMENT" {
		t.Errorf("error = %v", doc["error"])
	}
}

// Recorded, never enforced: nothing here refuses a request for lacking a
// role, exactly as Azure RBAC and AWS IAM do not. But it has to round-trip,
// because the provider reads it back.
func TestServiceIamPolicyRoundTrips(t *testing.T) {
	s := newRunTestServer(t)
	must(t, s, http.MethodPost, runPath+"?serviceId=hello", runBody)
	must(t, s, http.MethodPost, runPath+"/hello:setIamPolicy", `{"policy": {
		"bindings": [{"role": "roles/run.invoker", "members": ["allUsers"]}]
	}}`)

	doc := must(t, s, http.MethodPost, runPath+"/hello:getIamPolicy", `{}`)
	bindings, _ := doc["bindings"].([]any)
	if len(bindings) != 1 {
		t.Fatalf("bindings = %v, want the one that was set", doc["bindings"])
	}
	b := bindings[0].(map[string]any)
	if b["role"] != "roles/run.invoker" {
		t.Errorf("role = %v", b["role"])
	}
}

// --- the proxy ------------------------------------------------------------

// The two failures are different and the difference is the whole answer:
// a service nobody created, or one whose container is not up.
func TestTheProxyTellsMissingApartFromNotRunning(t *testing.T) {
	s := newRunTestServer(t)

	rec, _ := call(t, s, http.MethodGet, RunProxyPrefix+"nope", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("an unknown service gave %d, want 404", rec.Code)
	}

	must(t, s, http.MethodPost, runPath+"?serviceId=hello", runBody)
	rec, doc := call(t, s, http.MethodGet, RunProxyPrefix+"hello", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a stopped service gave %d, want 503", rec.Code)
	}
	e, _ := doc["error"].(map[string]any)
	msg, _ := e["message"].(string)
	if !strings.Contains(msg, "not running") {
		t.Errorf("the message does not distinguish the case: %q", msg)
	}
}

// The service sees the path it expects, not the proxy prefix.
func TestTheProxyStripsItsOwnPrefix(t *testing.T) {
	for _, c := range []struct{ path, prefix, want string }{
		{"/_nimbus/run/hello", "/_nimbus/run/hello", "/"},
		{"/_nimbus/run/hello/", "/_nimbus/run/hello", "/"},
		{"/_nimbus/run/hello/api/v1", "/_nimbus/run/hello", "/api/v1"},
	} {
		if got := run.StripPrefix(c.path, c.prefix); got != c.want {
			t.Errorf("stripPrefix(%q, %q) = %q, want %q", c.path, c.prefix, got, c.want)
		}
	}
}

// --- the Knative projection gcloud uses -----------------------------------

// `gcloud run` does not speak the Admin API at all. Measured: it asks for
// /apis/serving.knative.dev/v1/namespaces/{project}/services.
func TestKnativeProjectionAnswersGcloudsOwnPath(t *testing.T) {
	s := newRunTestServer(t)
	must(t, s, http.MethodPost, runPath+"?serviceId=hello", runBody)

	doc := must(t, s, http.MethodGet,
		"/apis/serving.knative.dev/v1/namespaces/nimbus-local/services?alt=json", "")
	if doc["kind"] != "ServiceList" {
		t.Fatalf("kind = %v, want ServiceList", doc["kind"])
	}
	items, _ := doc["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v, want one service", doc["items"])
	}
	item := items[0].(map[string]any)
	meta := item["metadata"].(map[string]any)
	if meta["name"] != "hello" || meta["namespace"] != "nimbus-local" {
		t.Errorf("metadata = %v", item["metadata"])
	}
	status := item["status"].(map[string]any)
	if status["url"] != base+RunProxyPrefix+"hello" {
		t.Errorf("status.url = %v, want the service's uri", status["url"])
	}
	// The container spec is the same either way, which is what makes this
	// a projection rather than a second model.
	spec := item["spec"].(map[string]any)
	tmpl := spec["template"].(map[string]any)
	containers := tmpl["spec"].(map[string]any)["containers"].([]any)
	if containers[0].(map[string]any)["image"] != "nginx:alpine" {
		t.Errorf("containers = %v", containers)
	}
}

// A collection gcloud asks for that Nimbus does not model answers an empty
// list, not a 404: Milestone 9's anywhereCaches showed what a client can
// do with an error it was not expecting.
func TestKnativeAnswersAnEmptyListForCollectionsNimbusDoesNotModel(t *testing.T) {
	s := newRunTestServer(t)
	for _, collection := range []string{"configurations", "routes"} {
		doc := must(t, s, http.MethodGet,
			"/apis/serving.knative.dev/v1/namespaces/nimbus-local/"+collection, "")
		if _, ok := doc["items"]; !ok {
			t.Errorf("%s did not answer an items list: %v", collection, doc)
		}
	}
}

func TestKnativeWritesSayTheyAreNotImplemented(t *testing.T) {
	s := newRunTestServer(t)
	rec, _ := call(t, s, http.MethodPost,
		"/apis/serving.knative.dev/v1/namespaces/nimbus-local/services", `{}`)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rec.Code)
	}
}
