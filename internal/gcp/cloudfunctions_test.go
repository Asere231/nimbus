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

// --- what `gcloud functions deploy` needs ----------------------------------
//
// Four endpoints the Terraform provider never asks for, each found by running
// gcloud and reading --log-http. The provider working is not the same claim
// as the service working, which is the lesson Key Vault taught one milestone
// earlier -- there, `az` found three faults Terraform could not see.

// The runtime catalogue comes from the SAME table the deploy uses.
//
// gcloud reads it before deploying anything, and without it the deploy stops
// with a 404 and never sends a create. Built from the table rather than
// written out, because a catalogue advertising a runtime the deploy then
// refuses is worse than no catalogue at all.
func TestTheRuntimeCatalogueMatchesWhatWillRun(t *testing.T) {
	s := newTestServer(t)
	rec, body := call(t, s, http.MethodGet,
		"/v2/projects/"+s.cfg.ProjectID+"/locations/us-central1/runtimes", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("runtimes = %d %v", rec.Code, body)
	}
	items, _ := body["runtimes"].([]any)
	if len(items) == 0 {
		t.Fatal("the catalogue is empty")
	}
	for _, it := range items {
		m := mapOf(it)
		name := str(m["name"])
		// Every advertised runtime must actually deploy.
		if _, err := runtimeImage(name); err != nil {
			t.Errorf("the catalogue advertises %q and runtimeImage refuses it: %v", name, err)
		}
		if str(m["environment"]) != "GEN_2" {
			t.Errorf("%s is advertised as %q; Gen 1 is a different API", name, m["environment"])
		}
	}
	if len(items) != len(runtimes) {
		t.Errorf("the catalogue lists %d runtimes and the table has %d", len(items), len(runtimes))
	}
}

// gcloud asks the SERVICE where to put the source, then PUTs it there.
//
// A different route from the provider's, which uploads to a bucket its own
// configuration created and names it. Both end in a storageSource the create
// can read.
func TestGcloudUploadsItsSourceAndTheCreateFindsIt(t *testing.T) {
	s := newTestServer(t)

	rec, body := call(t, s, http.MethodPost,
		"/v2/projects/"+s.cfg.ProjectID+"/locations/us-central1/functions:generateUploadUrl", "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("generateUploadUrl = %d %v", rec.Code, body)
	}
	src := mapOf(body["storageSource"])
	bucket, object := str(src["bucket"]), str(src["object"])
	if bucket == "" || object == "" {
		t.Fatalf("no storageSource came back: %v", body)
	}
	uploadURL := str(body["uploadUrl"])
	if !strings.Contains(uploadURL, FunctionUploadPrefix) {
		t.Errorf("uploadUrl %q does not point at Nimbus", uploadURL)
	}

	// Two calls must not collide: a second deploy of the same function
	// would otherwise overwrite the first one's archive mid-flight.
	_, second := call(t, s, http.MethodPost,
		"/v2/projects/"+s.cfg.ProjectID+"/locations/us-central1/functions:generateUploadUrl", "{}")
	if str(mapOf(second["storageSource"])["object"]) == object {
		t.Error("two upload URLs named the same object")
	}

	// The PUT, as a client does with a signed URL.
	rec, _ = call(t, s, http.MethodPut, FunctionUploadPrefix+object, "pretend this is a zip")
	if rec.Code != http.StatusOK {
		t.Fatalf("the upload = %d", rec.Code)
	}
	// The archive is findable by the create path...
	if _, _, ok := s.objects.get(bucket, object); !ok {
		t.Error("the uploaded archive is not in the object store")
	}
	// ...and the BUCKET exists as a resource, or it is invisible in the
	// graph and answers NoSuchBucket to a listing. Measured: the PUT
	// answered 200 while the bucket 404'd.
	if !s.store.Exists(s.bucketLink(bucket)) {
		t.Errorf("the source bucket %q holds bytes but is not a resource", bucket)
	}
}

// The pre-flight APIs are answered LOCALLY, which is the whole reason they
// exist here.
//
// gcloud asks Service Usage whether run, cloudbuild and artifactregistry are
// enabled, and Cloud Build for a default service account. With no endpoint
// override those four requests go to the REAL Google and come back 403 and
// 401 -- measured with --log-http, in the middle of what looks like a local
// deploy. Neither product is emulated; what is served is exactly what the
// pre-flight reads.
func TestThePreflightAPIsAreAnsweredLocally(t *testing.T) {
	s := newTestServer(t)

	rec, body := call(t, s, http.MethodGet,
		"/v1/projects/"+s.cfg.ProjectID+"/services/run.googleapis.com", "")
	if rec.Code != http.StatusOK || str(body["state"]) != "ENABLED" {
		t.Errorf("service enablement = %d %v", rec.Code, body)
	}

	rec, body = call(t, s, http.MethodGet,
		"/v1/projects/"+s.cfg.ProjectID+"/locations/us-central1/defaultServiceAccount", "")
	if rec.Code != http.StatusOK || str(body["serviceAccountEmail"]) == "" {
		t.Errorf("default service account = %d %v", rec.Code, body)
	}

	// Anything else under Cloud Build says it is not emulated, rather than
	// inventing a build to poll.
	rec, body = call(t, s, http.MethodPost, "/v1/projects/"+s.cfg.ProjectID+"/builds", "{}")
	if rec.Code == http.StatusOK {
		t.Errorf("a Cloud Build submission was accepted: %v", body)
	}
}

// Gen 1 is a different API, and the refusal says so.
//
// `gcloud functions list` reads the v1 and v2 lists and MERGES them, so the
// v1 list must answer -- an empty list, truthfully, because Nimbus serves no
// Gen 1 functions. Without it gcloud printed a 404 error beside a listing
// that had worked, and an error next to a success is how people learn to
// ignore errors.
//
// But an empty list must not read as "Gen 1 works": a gen 1 DEPLOY is
// refused by name, and the colon verb it sends is why the check trims one.
func TestGen1IsAnEmptyListAndARefusalByName(t *testing.T) {
	s := newTestServer(t)
	p := "/v1/projects/" + s.cfg.ProjectID + "/locations/us-central1/functions"

	rec, body := call(t, s, http.MethodGet, p, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("the gen 1 list = %d %v", rec.Code, body)
	}
	if items, _ := body["functions"].([]any); len(items) != 0 {
		t.Errorf("the gen 1 list is not empty: %v", items)
	}

	// The deploy path, colon verb and all.
	rec, body = call(t, s, http.MethodPost, p+":generateUploadUrl", "{}")
	if rec.Code == http.StatusOK {
		t.Fatal("a gen 1 deploy was accepted")
	}
	msg := str(mapOf(body["error"])["message"])
	if !strings.Contains(msg, "GEN 2") || !strings.Contains(msg, "--gen2") {
		t.Errorf("the refusal does not name Gen 2 or what to do instead: %s", msg)
	}

	// And the bare locations list, which gcloud reads before either.
	rec, body = call(t, s, http.MethodGet, "/v1/projects/"+s.cfg.ProjectID+"/locations", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("the locations list = %d %v", rec.Code, body)
	}
	locs, _ := body["locations"].([]any)
	if len(locs) != 1 {
		t.Errorf("want exactly the one region Nimbus serves, got %d", len(locs))
	}
}

// /v1 now carries SIX services, and routing one to the wrong place answers a
// 404 naming the wrong API.
func TestV1RoutesSixServices(t *testing.T) {
	cases := []struct{ path, want string }{
		{"/v1/projects/p/services/run.googleapis.com", apiServiceUsage},
		{"/v1/projects/p/locations/l/defaultServiceAccount", apiCloudBuild},
		{"/v1/projects/p/locations", apiFunctions},
		{"/v1/projects/p/locations/l/functions", apiFunctions},
		{"/v1/projects/p/locations/l/functions:generateUploadUrl", apiFunctions},
		{"/v1/projects/p/locations/l/clusters", apiGKE},
		{"/v1/projects/p/serviceAccounts/sa", apiIAM},
		{"/v1/projects/p/topics/t", apiPubSub},
		{"/v1/projects/p", apiResourceManager},
	}
	for _, c := range cases {
		parts := strings.Split(strings.TrimPrefix(c.path, "/"), "/")
		if got := apiFor(parts); got != c.want {
			t.Errorf("apiFor(%s) = %q, want %q", c.path, got, c.want)
		}
	}
}
