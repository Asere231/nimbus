package gcp

import (
	"net/http"

	"github.com/Asere231/nimbus/internal/engines/docker"
	"strings"
	"testing"
)

// Cloud Functions Gen 2, replaying what google 6.50.0 actually sends.
//
// The create body here is verbatim from a logged apply, down to the explicit
// JSON nulls the provider sends for the two update policies -- present and
// nil is not absent, and anything reading a field has to survive both.

const gcfBody = `{"buildConfig":{"automaticUpdatePolicy":null,"entryPoint":"hello",` +
	`"onDeployUpdatePolicy":null,"runtime":"python311",` +
	`"source":{"storageSource":{"bucket":"code","object":"src.zip"}}},` +
	`"labels":{"goog-terraform-provisioned":"true"},` +
	`"name":"projects/nimbus-local/locations/us-central1/functions/hello-fn",` +
	`"serviceConfig":{"allTrafficOnLatestRevision":true,"availableMemory":"256M",` +
	`"ingressSettings":"ALLOW_ALL","maxInstanceCount":1,"timeoutSeconds":60}}`

func gcfPath(s *Server, rest string) string {
	return "/v2/projects/" + s.cfg.ProjectID + "/locations/us-central1/functions" + rest
}

// putSource puts a source archive in the object store, which is where a
// function's code really comes from: the provider uploads a zip to Cloud
// Storage and names the bucket and object. Nimbus reads its OWN storage.
func putSource(t *testing.T, s *Server, bucket, object string) {
	t.Helper()
	if _, aerr := s.objects.put(s, bucket, object, []byte("not a real zip"), nil); aerr != nil {
		t.Fatalf("seeding the source archive: %v", aerr)
	}
}

// The resource round-trips, and the URL is filled by the CREATE.
//
// That last part is the one that matters. The provider writes state from a
// read taken while the function is still deploying and never looks again, so
// an address filled when the container becomes ready would be empty in state
// for ever -- the GKE endpoint lesson, now in its second service.
func TestAFunctionIsCreatedWithItsURLAlreadyFilled(t *testing.T) {
	s := newTestServer(t)
	putSource(t, s, "code", "src.zip")

	rec, body := call(t, s, http.MethodPost, gcfPath(s, "?functionId=hello-fn"), gcfBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d %v", rec.Code, body)
	}
	// Create answers an OPERATION, not the function: Cloud Functions is
	// Cloud Run underneath and the provider polls the operation, the
	// opposite of GKE, which polls the resource.
	if body["name"] == nil || !strings.Contains(str(body["name"]), "/operations/") {
		t.Errorf("create did not answer an operation: %v", body)
	}
	if body["done"] == true {
		t.Error("the operation was already done; the container has not started yet")
	}

	rec, fn := call(t, s, http.MethodGet, gcfPath(s, "/hello-fn"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d %v", rec.Code, fn)
	}
	url := str(fn["url"])
	if url == "" {
		t.Fatal("url is empty; the provider stores it from a read taken while still deploying")
	}
	if !strings.Contains(url, FunctionProxyPrefix) {
		t.Errorf("url = %q, want it to go through the function proxy", url)
	}
	svc := mapOf(fn["serviceConfig"])
	if str(svc["uri"]) != url {
		t.Errorf("serviceConfig.uri = %q and url = %q; a client reads either", svc["uri"], url)
	}
	// A Gen 2 function really runs on a Cloud Run service, and the provider
	// stores the name. Reported, not created: inventing a second resource
	// would put a Cloud Run service in the graph no configuration asked for.
	if !strings.Contains(str(svc["service"]), "/services/hello-fn") {
		t.Errorf("serviceConfig.service = %q", svc["service"])
	}
	if str(fn["environment"]) != "GEN_2" {
		t.Errorf("environment = %q, want GEN_2", fn["environment"])
	}
}

// Everything a client reads back twice must be identical, or the plan never
// settles. Two reads, compared whole.
func TestAFunctionReadsBackIdentically(t *testing.T) {
	s := newTestServer(t)
	putSource(t, s, "code", "src.zip")
	call(t, s, http.MethodPost, gcfPath(s, "?functionId=hello-fn"), gcfBody)

	_, first := call(t, s, http.MethodGet, gcfPath(s, "/hello-fn"), "")
	_, second := call(t, s, http.MethodGet, gcfPath(s, "/hello-fn"), "")
	for _, field := range []string{"url", "name", "environment"} {
		if str(first[field]) != str(second[field]) {
			t.Errorf("%s changed between reads: %q then %q", field, first[field], second[field])
		}
	}
	b1, b2 := mapOf(first["buildConfig"]), mapOf(second["buildConfig"])
	if str(b1["build"]) != str(b2["build"]) {
		t.Errorf("buildConfig.build changed between reads: %q then %q", b1["build"], b2["build"])
	}
}

// A function lists, patches and deletes.
func TestAFunctionListsPatchesAndDeletes(t *testing.T) {
	s := newTestServer(t)
	putSource(t, s, "code", "src.zip")
	call(t, s, http.MethodPost, gcfPath(s, "?functionId=hello-fn"), gcfBody)

	_, list := call(t, s, http.MethodGet, gcfPath(s, ""), "")
	items, _ := list["functions"].([]any)
	if len(items) != 1 {
		t.Fatalf("list returned %d functions: %v", len(items), list)
	}

	// updateMask is a WHITELIST: a field the mask does not name must not be
	// touched, or a partial update silently clears everything left out.
	_, _ = call(t, s, http.MethodPatch, gcfPath(s, "/hello-fn")+"?updateMask=description",
		`{"description":"updated","labels":{"must":"not apply"}}`)
	_, after := call(t, s, http.MethodGet, gcfPath(s, "/hello-fn"), "")
	if str(after["description"]) != "updated" {
		t.Errorf("the masked field was not applied: %v", after["description"])
	}
	if labels := mapOf(after["labels"]); str(labels["must"]) == "not apply" {
		t.Error("a field the updateMask did not name was applied anyway")
	}

	if rec, body := call(t, s, http.MethodDelete, gcfPath(s, "/hello-fn"), ""); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %v", rec.Code, body)
	}
	if rec, _ := call(t, s, http.MethodGet, gcfPath(s, "/hello-fn"), ""); rec.Code != http.StatusNotFound {
		t.Errorf("a deleted function still reads: %d", rec.Code)
	}
}

// What Nimbus cannot do is refused BY NAME, before anything is stored.
//
// A runtime served by the nearest interpreter would run the code and could
// not be trusted to mean anything about the runtime that was asked for; a
// function with no source archive would build an image out of nothing.
func TestAnUnsupportedFunctionRequestIsRefusedByName(t *testing.T) {
	s := newTestServer(t)
	putSource(t, s, "code", "src.zip")

	cases := []struct{ name, body, named string }{
		{"unknown runtime",
			`{"buildConfig":{"runtime":"ruby33","entryPoint":"h","source":{"storageSource":{"bucket":"code","object":"src.zip"}}}}`,
			"ruby33"},
		{"no runtime",
			`{"buildConfig":{"entryPoint":"h","source":{"storageSource":{"bucket":"code","object":"src.zip"}}}}`,
			"runtime"},
		{"no entry point",
			`{"buildConfig":{"runtime":"python311","source":{"storageSource":{"bucket":"code","object":"src.zip"}}}}`,
			"entryPoint"},
		{"no source",
			`{"buildConfig":{"runtime":"python311","entryPoint":"h"}}`,
			"storageSource"},
		{"source that is not there",
			`{"buildConfig":{"runtime":"python311","entryPoint":"h","source":{"storageSource":{"bucket":"code","object":"missing.zip"}}}}`,
			"does not exist"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, body := call(t, s, http.MethodPost, gcfPath(s, "?functionId=refused"), c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("= %d, want 400: %v", rec.Code, body)
			}
			msg := str(mapOf(body["error"])["message"])
			if !strings.Contains(msg, c.named) {
				t.Errorf("the refusal does not name %q: %s", c.named, msg)
			}
			// And nothing was stored: a refused create must not leave a
			// resource behind for the retry to collide with.
			if s.store.Exists(s.functionName("us-central1", "refused")) {
				t.Error("a refused create stored the function anyway")
			}
		})
	}

	// The name is required, and arrives as a QUERY parameter -- Cloud Run's
	// ?serviceId= convention again, even though the body carries it too.
	if rec, _ := call(t, s, http.MethodPost, gcfPath(s, ""), gcfBody); rec.Code != http.StatusBadRequest {
		t.Errorf("a create with no functionId = %d, want 400", rec.Code)
	}
}

// /v2 no longer means one thing, so it is disambiguated by the collection.
//
// Cloud Run and Cloud Functions both answer under /v2/projects/{p}/
// locations/{l}/..., and routing a path to the wrong service answers a 404
// naming the wrong API -- which sends the next person reading the activity
// feed in entirely the wrong direction.
func TestV2IsSplitBetweenRunAndFunctions(t *testing.T) {
	cases := []struct{ path, want string }{
		{"/v2/projects/p/locations/l/functions", apiFunctions},
		{"/v2/projects/p/locations/l/functions/f", apiFunctions},
		{"/v2/projects/p/locations/l/services", apiRun},
		{"/v2/projects/p/locations/l/services/s", apiRun},
		// An operations path belongs to neither in particular, and that is
		// fine: one registry holds every long-running operation, so either
		// handler answers a poll identically.
		{"/v2/projects/p/locations/l/operations/op-1", apiRun},
	}
	for _, c := range cases {
		parts := strings.Split(strings.TrimPrefix(c.path, "/"), "/")
		if got := apiFor(parts); got != c.want {
			t.Errorf("apiFor(%s) = %q, want %q", c.path, got, c.want)
		}
	}
}

// The permission is derived, with no table entry, and it is Google's real
// name for it.
func TestAFunctionRequestDerivesItsPermission(t *testing.T) {
	cases := []struct{ method, path, want string }{
		{"POST", "/v2/projects/p/locations/l/functions", "cloudfunctions.functions.create"},
		{"GET", "/v2/projects/p/locations/l/functions", "cloudfunctions.functions.list"},
		{"GET", "/v2/projects/p/locations/l/functions/f", "cloudfunctions.functions.get"},
		{"PATCH", "/v2/projects/p/locations/l/functions/f", "cloudfunctions.functions.update"},
		{"DELETE", "/v2/projects/p/locations/l/functions/f", "cloudfunctions.functions.delete"},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(c.method, c.path, nil)
		parts := strings.Split(strings.TrimPrefix(c.path, "/"), "/")
		got, _ := permissionFor(req, parts)
		if got != c.want {
			t.Errorf("%s %s needs %q, classifier says %q", c.method, c.path, c.want, got)
		}
	}
}

// A failed deploy is recorded on the RESOURCE as well as on the operation.
//
// Both, because they are read by different things: the operation's error is
// what reaches the terraform output, and the resource's state is what a later
// read or `gcloud functions describe` shows. The Docker Fake is unavailable
// in these tests, so the start fails immediately rather than waiting out the
// readiness probe -- which against a Fake can never succeed, because the
// probe is a real HTTP GET against a published port.
func TestAFailedDeployIsRecordedOnTheFunctionItself(t *testing.T) {
	s := newTestServer(t)
	putSource(t, s, "code", "src.zip")
	call(t, s, http.MethodPost, gcfPath(s, "?functionId=hello-fn"), gcfBody)

	// provisionFunction runs in a goroutine; drive it directly so the test
	// asserts about a state it created rather than racing one.
	s.provisionFunction("us-central1", "hello-fn", "op-test")

	_, fn := call(t, s, http.MethodGet, gcfPath(s, "/hello-fn"), "")
	if str(fn["state"]) != "FAILED" {
		t.Errorf("state = %q, want FAILED after the start failed", fn["state"])
	}
	msgs, _ := fn["stateMessages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("a failed deploy left no stateMessages; a later read says nothing about why")
	}
	if str(mapOf(msgs[0])["message"]) == "" {
		t.Error("the state message is empty")
	}
}

// Deleting a function removes its IMAGE, even after a restart.
//
// This is the quietest leak in the project and it was measured rather than
// reasoned about: a destroy straight after an apply left no image, and a
// destroy after a RESTART left one behind. Nothing in `docker ps` shows it.
//
// The cause is not a bug in the engine. Reattach adopts a container and
// cannot tell whether its image was built or pulled, so an adopted Service
// has no BuildDir and removeBuiltImage declines -- which is correct, because
// removing a shared PULLED image because one function used it would break
// everything else on the machine. The consequence is that the plugin must
// remove its own built image by its derived tag, which is what Azure's
// function app does for exactly the same reason.
//
// Forgetting the service is what a restart does to the engine's memory, so
// that is how this test reaches the state that leaked.
func TestDeletingAFunctionRemovesItsImageEvenAfterARestart(t *testing.T) {
	s := newTestServer(t)
	putSource(t, s, "code", "src.zip")
	call(t, s, http.MethodPost, gcfPath(s, "?functionId=hello-fn"), gcfBody)

	id := s.functionName("us-central1", "hello-fn")
	tag := functionImageTag("hello-fn", id)

	f, ok := s.functions.Docker().(*docker.Fake)
	if !ok {
		t.Fatal("the test server is not using the Docker Fake")
	}
	// Available for this one. newTestServer marks the Fake unavailable so a
	// handler cannot start anything, and an unavailable daemon makes every
	// docker call a no-op -- including the removal under test, which would
	// make this pass or fail for the wrong reason either way.
	f.Unavailable = false
	f.Images = map[string]bool{tag: true}

	// The post-restart state: the container is adopted, the built-image
	// memory is gone.
	s.functions.Forget(id)

	if rec, body := call(t, s, http.MethodDelete, gcfPath(s, "/hello-fn"), ""); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %v", rec.Code, body)
	}
	if f.Images[tag] {
		t.Errorf("the image %s survived the delete; nothing in `docker ps` would show it", tag)
	}
}

// The image tag is a legal one BY CONSTRUCTION, because it comes from the
// shared helper rather than from a digest invented here.
func TestAFunctionsImageTagComesFromTheSharedHelper(t *testing.T) {
	id := "http://localhost:4703/v2/projects/P/locations/us-central1/functions/Hello-FN"
	tag := functionImageTag("Hello-FN", id)
	if tag != docker.ImageTag("nimbus-gcf", id, "Hello-FN") {
		t.Errorf("the tag does not come from docker.ImageTag: %s", tag)
	}
	// Even given an id and a name with uppercase in them.
	if tag != strings.ToLower(tag) {
		t.Errorf("tag %q has uppercase letters; docker refuses them", tag)
	}
	repo, _, _ := strings.Cut(tag, ":")
	if strings.ContainsAny(repo, "/+= ") {
		t.Errorf("tag %q contains a character docker will not take there", tag)
	}
}
