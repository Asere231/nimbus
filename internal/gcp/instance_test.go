package gcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

const zonePath = "/compute/v1/projects/nimbus-local/zones/us-central1-a"

// The exact body google provider 6.50.0 sends for a google_compute_instance,
// captured from a debug log. Four things in it are worth a test of their own
// and are asserted below: the body is NOT wrapped the way GKE's is, the
// metadata is an ARRAY of key/value items, the machineType and subnetwork
// arrive as partial URLs, and the image lives under
// disks[].initializeParams.sourceImage in another project.
const insertInstanceBody = `{
 "canIpForward": false,
 "deletionProtection": false,
 "disks": [
  {
   "autoDelete": true,
   "boot": true,
   "initializeParams": {
    "sourceImage": "projects/debian-cloud/global/images/debian-12"
   },
   "mode": "READ_WRITE"
  }
 ],
 "labels": {"goog-terraform-provisioned": "true"},
 "machineType": "projects/nimbus-local/zones/us-central1-a/machineTypes/e2-micro",
 "metadata": {
  "items": [
   {"key": "ssh-keys", "value": "nimbus:ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAImeasure nimbus"},
   {"key": "startup-script", "value": "echo hello > /var/log/nimbus-startup.log"}
  ]
 },
 "name": "web",
 "networkInterfaces": [
  {"subnetwork": "projects/nimbus-local/regions/us-central1/subnetworks/nodes"}
 ],
 "params": {},
 "scheduling": {"automaticRestart": true},
 "tags": {"items": ["web", "ssh"]}
}`

func instanceServer(t *testing.T) (*Server, *docker.Fake) {
	t.Helper()
	s := newTestServer(t)
	f := docker.NewFake()
	f.Images["debian:12"] = true
	f.Images[DefaultInstanceImage] = true
	f.ExecAny = true
	s.UseDocker(f)
	return s, f
}

// makeInstanceNetwork gives an instance the subnetwork it refers to.
func makeInstanceNetwork(t *testing.T, s *Server) {
	t.Helper()
	must(t, s, "POST", "/compute/v1/projects/nimbus-local/global/networks",
		`{"name":"vpc","autoCreateSubnetworks":false}`)
	must(t, s, "POST", "/compute/v1/projects/nimbus-local/regions/us-central1/subnetworks",
		`{"name":"nodes","ipCidrRange":"10.20.0.0/24","network":"projects/nimbus-local/global/networks/vpc"}`)
}

func runInstance(t *testing.T, s *Server) map[string]any {
	t.Helper()
	makeInstanceNetwork(t, s)
	op := must(t, s, "POST", zonePath+"/instances", insertInstanceBody)

	// The insert answers an operation that is NOT done: the provider polls
	// it and only then reads the instance. Every other Compute write here
	// answers DONE at once, because nothing else is slow -- starting a
	// container, which includes pulling its image, is.
	if op["status"] != "RUNNING" {
		t.Fatalf("insert operation status = %v, want RUNNING until the container is up", op["status"])
	}
	if op["zone"] == nil || op["region"] != nil {
		t.Errorf("a zonal operation must carry zone and not region: %v", op)
	}

	waitForInstanceStatus(t, s, "web", "RUNNING")
	return must(t, s, "GET", zonePath+"/instances/web", "")
}

func TestAnInstanceIsCreatedFromTheBodyTheProviderSends(t *testing.T) {
	s, _ := instanceServer(t)
	got := runInstance(t, s)

	if got["kind"] != "compute#instance" || got["name"] != "web" {
		t.Errorf("kind/name = %v/%v", got["kind"], got["name"])
	}
	// Partial URLs must come back as FULL self links, or the provider
	// compares them with what it sent and plans a change every run.
	for _, field := range []string{"machineType", "zone", "selfLink"} {
		if v := str(got[field]); !strings.HasPrefix(v, "http") {
			t.Errorf("%s = %q, want a full self link", field, v)
		}
	}
	nics, _ := got["networkInterfaces"].([]any)
	if len(nics) != 1 {
		t.Fatalf("networkInterfaces = %v", got["networkInterfaces"])
	}
	nic := nics[0].(map[string]any)
	if v := str(nic["subnetwork"]); !strings.HasPrefix(v, "http") {
		t.Errorf("subnetwork = %q, want a full self link", v)
	}
	// The network follows from the subnetwork even though the provider only
	// sent the subnetwork, because it reads the network back.
	if v := str(nic["network"]); !strings.Contains(v, "/networks/vpc") {
		t.Errorf("network = %q, want it derived from the subnetwork", v)
	}
	// Whatever the client sent is kept untouched.
	if got["canIpForward"] != false || got["deletionProtection"] != false {
		t.Errorf("the client's own fields were not kept: %v", got)
	}
	if tags, ok := got["tags"].(map[string]any); !ok || tags["fingerprint"] == nil {
		t.Errorf("tags = %v, want the items kept and a fingerprint added", got["tags"])
	}
}

// The image name decides the container image, and unlike an AMI id it can:
// `debian-12` says what it is. So a GCE instance usually needs no hint,
// where an EC2 one needs a nimbus:image tag.
func TestTheContainerImageComesFromTheGCEImageName(t *testing.T) {
	s, _ := instanceServer(t)
	runInstance(t, s)

	for _, m := range s.vms.All() {
		if m.Image != "debian:12" {
			t.Errorf("container image = %q, want debian:12 from debian-cloud/debian-12", m.Image)
		}
		if m.UserData != "echo hello > /var/log/nimbus-startup.log" {
			t.Errorf("the startup-script did not become the boot script: %q", m.UserData)
		}
		// ssh-keys metadata is "user:key" per line, because one instance
		// can carry keys for several users. Only the key is useful.
		if len(m.PublicKeys) != 1 || strings.HasPrefix(m.PublicKeys[0], "nimbus:") {
			t.Errorf("public keys = %v, want the user prefix stripped", m.PublicKeys)
		}
	}
}

// A few image references, all of which a provider or gcloud might send.
func TestImageNamesMapToRealBaseImages(t *testing.T) {
	for _, c := range []struct{ ref, want string }{
		{"projects/debian-cloud/global/images/debian-12", "debian:12"},
		{"projects/debian-cloud/global/images/family/debian-12", "debian:12"},
		{"debian-11", "debian:11"},
		{"ubuntu-2404-lts-amd64", "ubuntu:24.04"},
		{"ubuntu-os-cloud/ubuntu-2204-jammy-v20240927", "ubuntu:22.04"},
		{"projects/cos-cloud/global/images/cos-117-lts", "alpine:3.20"},
		{"something-nobody-has-heard-of", DefaultInstanceImage},
	} {
		if got := dockerImageFor(c.ref); got != c.want {
			t.Errorf("dockerImageFor(%q) = %q, want %q", c.ref, got, c.want)
		}
	}
}

// Public images live in projects nobody created -- debian-cloud,
// ubuntu-os-cloud -- and the provider resolves one BEFORE creating an
// instance, which the aws provider never does for an AMI. It asks by name
// and by family, because the reference is ambiguous between the two.
func TestPublicImagesAnswerForAProjectNobodyCreated(t *testing.T) {
	s, _ := instanceServer(t)
	for _, path := range []string{
		"/compute/v1/projects/debian-cloud/global/images/debian-12",
		"/compute/v1/projects/debian-cloud/global/images/family/debian-12",
		"/compute/v1/projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64",
	} {
		got := must(t, s, "GET", path, "")
		if got["kind"] != "compute#image" || got["status"] != "READY" {
			t.Errorf("%s = %v", path, got)
		}
		// The self link names the project it was ASKED for, because that is
		// what the provider puts in the disk's sourceImage and reads back.
		if v := str(got["selfLink"]); !strings.Contains(v, "/projects/debian-cloud/") &&
			!strings.Contains(v, "/projects/ubuntu-os-cloud/") {
			t.Errorf("%s selfLink = %q, want the project it was asked for", path, v)
		}
		if got["diskSizeGb"] != "10" {
			t.Errorf("diskSizeGb = %v, want the decimal STRING every Compute number is", got["diskSizeGb"])
		}
	}
}

// stop, start and reset are TRAILING PATH SEGMENTS, not colon verbs. GKE's
// setSize is a colon verb and Pub/Sub's publish is too, which makes this the
// third convention for "do a thing to a resource" in one cloud.
func TestTheLifecycleVerbsArePathSegmentsNotColonVerbs(t *testing.T) {
	s, f := instanceServer(t)
	runInstance(t, s)
	container := s.vms.All()[0].Container
	f.Calls = nil

	// GCE calls a STOPPED instance TERMINATED, which is the same word EC2
	// uses for one that has been destroyed. Getting them the same way round
	// is a mistake waiting to happen.
	must(t, s, "POST", zonePath+"/instances/web/stop", "")
	waitForInstanceStatus(t, s, "web", "TERMINATED")

	must(t, s, "POST", zonePath+"/instances/web/start", "")
	waitForInstanceStatus(t, s, "web", "RUNNING")

	// The same container throughout: a stop and start keeps the root disk.
	if got := s.vms.All()[0].Container; got != container {
		t.Errorf("the container changed across a stop/start: %q then %q", container, got)
	}
	calls := strings.Join(f.Calls, "; ")
	if !strings.Contains(calls, "start "+container) {
		t.Errorf("the instance was not resumed: %s", calls)
	}
	if strings.Contains(calls, "run ") {
		t.Errorf("a new container was created instead of resuming: %s", calls)
	}

	// A colon verb is NOT how Compute spells this, so it must not work --
	// answering it would hide the real convention from whoever reads next.
	if rec, _ := call(t, s, "POST", zonePath+"/instances/web:stop", ""); rec.Code == 200 {
		t.Error("a colon verb was accepted; Compute uses a path segment")
	}
}

// Deleting an instance removes it from the store at once, because the
// provider's delete waiter reads the OPERATION -- the opposite of EC2,
// where a terminated instance has to stay describable or the destroy
// times out.
func TestDeletingAnInstanceFinishesItsOperation(t *testing.T) {
	s, _ := instanceServer(t)
	runInstance(t, s)

	op := must(t, s, "DELETE", zonePath+"/instances/web", "")
	if op["operationType"] != "delete" {
		t.Errorf("operationType = %v", op["operationType"])
	}
	if rec, _ := call(t, s, "GET", zonePath+"/instances/web", ""); rec.Code != 404 {
		t.Errorf("after a delete the instance answered %d, want 404", rec.Code)
	}

	// The operation reaches DONE, which is what the provider waits for.
	link := str(op["selfLink"])
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.operations.get(link); got != nil && got.Status == "DONE" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the delete operation never reached DONE")
}

// An instance nests under its subnetwork, so the graph draws it inside the
// network it is on with no GCP knowledge in the visualizer.
func TestAnInstanceIsParentedToItsSubnetwork(t *testing.T) {
	s, _ := instanceServer(t)
	got := runInstance(t, s)

	res, err := s.store.Get(str(got["selfLink"]))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.ParentID, "/subnetworks/nodes") {
		t.Errorf("ParentID = %q, want the subnetwork", res.ParentID)
	}
	if res.Type != TypeInstance {
		t.Errorf("Type = %q", res.Type)
	}
	if res.Backing != "real" {
		t.Errorf("Backing = %q, want real -- a container runs it", res.Backing)
	}
}

// The internal IP and the fingerprints are DERIVED, so they are the same on
// every read. A value that differed between reads is a permanent diff.
func TestDerivedValuesDoNotChangeBetweenReads(t *testing.T) {
	s, _ := instanceServer(t)
	first := runInstance(t, s)

	ip := func(body map[string]any) string {
		nics, _ := body["networkInterfaces"].([]any)
		return str(nics[0].(map[string]any)["networkIP"])
	}
	if !strings.HasPrefix(ip(first), "10.128.") {
		t.Errorf("networkIP = %q, want it in GCE's default range", ip(first))
	}
	for i := 0; i < 3; i++ {
		again := must(t, s, "GET", zonePath+"/instances/web", "")
		if ip(again) != ip(first) {
			t.Fatalf("networkIP changed between reads: %q then %q", ip(first), ip(again))
		}
		if again["fingerprint"] != first["fingerprint"] {
			t.Fatalf("fingerprint changed between reads")
		}
	}
}

// The metadata server answers GCE's own paths, nests a recursive query the
// way real GCE does, and sends the Metadata-Flavor header clients check.
func TestTheMetadataServerAnswersGCEsShape(t *testing.T) {
	s, _ := instanceServer(t)
	got := runInstance(t, s)
	handler := s.instanceMetadata(str(got["selfLink"]))

	text := func(path string) string {
		t.Helper()
		rec, _ := serveMetadata(handler, path)
		return strings.TrimSpace(rec)
	}
	if v := text("/computeMetadata/v1/instance/name"); v != "web" {
		t.Errorf("instance/name = %q", v)
	}
	if v := text("/computeMetadata/v1/instance/attributes/startup-script"); !strings.Contains(v, "nimbus-startup.log") {
		t.Errorf("the startup script is not in the metadata: %q", v)
	}
	// A listing: the keys one level down, one per line.
	if v := text("/computeMetadata/v1/instance/attributes/"); !strings.Contains(v, "ssh-keys") {
		t.Errorf("attributes listing = %q", v)
	}
	// Real GCE NESTS a recursive answer. A flat map with slashes in the
	// keys would make a client walking instance.attributes find nothing.
	_, doc := serveMetadata(handler, "/computeMetadata/v1/?recursive=true")
	inst, ok := doc["instance"].(map[string]any)
	if !ok {
		t.Fatalf("recursive metadata is not nested: %v", doc)
	}
	attrs, ok := inst["attributes"].(map[string]any)
	if !ok || attrs["startup-script"] == nil {
		t.Errorf("instance.attributes = %v", inst["attributes"])
	}
}

// --- helpers ---------------------------------------------------------------

// waitForInstanceStatus polls as the provider does: the status moves in the
// background, because that is what PROVISIONING -> RUNNING is.
func waitForInstanceStatus(t *testing.T, s *Server, name, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		rec, doc := call(t, s, "GET", zonePath+"/instances/"+name, "")
		if rec.Code == 200 {
			last = str(doc["status"])
			if last == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("instance %s is %q, want %q", name, last, want)
}

// serveMetadata drives the metadata handler directly, which is simpler than
// finding the port it was published on and tells us the same thing.
func serveMetadata(h http.Handler, path string) (string, map[string]any) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	return rec.Body.String(), doc
}

// `POST .../operations/{name}/wait` is how GCLOUD waits for an operation:
// it blocks until the operation finishes rather than polling. The Terraform
// provider polls the same operation with GET instead -- two clients, two
// paths, one operation, the same split `gcloud run` and the Cloud Run
// provider have.
//
// Measured: `gcloud compute instances stop` failed with "Nimbus does not
// implement the Compute collection operations", naming the very collection
// it was already being served, because only the GET form existed.
func TestGcloudWaitsForAnOperationWithAPostNotAPoll(t *testing.T) {
	s, _ := instanceServer(t)
	makeInstanceNetwork(t, s)
	op := must(t, s, "POST", zonePath+"/instances", insertInstanceBody)

	name := lastSegment(str(op["selfLink"]))
	path := zonePath + "/operations/" + name + "/wait"

	// It blocks until done, so by the time it answers the status is DONE
	// and the instance is running -- no polling loop in the client.
	got := must(t, s, "POST", path, "")
	if got["status"] != "DONE" {
		t.Errorf("wait answered status %v, want DONE", got["status"])
	}
	if got["operationType"] != "insert" {
		t.Errorf("operationType = %v", got["operationType"])
	}

	// A GET on the same operation still works, because that is the path
	// the provider uses.
	polled := must(t, s, "GET", zonePath+"/operations/"+name, "")
	if polled["status"] != "DONE" {
		t.Errorf("GET answered status %v, want DONE", polled["status"])
	}

	// An operation nobody issued is a 404 either way: inventing a DONE
	// would turn a client's mistake into a silent success.
	if rec, _ := call(t, s, "POST", zonePath+"/operations/operation-nope/wait", ""); rec.Code != 404 {
		t.Errorf("waiting on an unknown operation = %d, want 404", rec.Code)
	}
}

// gcloud lists instances across every zone at once, through the aggregated
// path, where the provider uses the zonal collection. The answer is a MAP
// keyed by scope, not a list -- a client reading items as an array finds
// nothing.
func TestGcloudListsInstancesThroughTheAggregatedPath(t *testing.T) {
	s, _ := instanceServer(t)

	// Empty: a scope with nothing in it carries a warning rather than an
	// empty list, which is what gcloud prints as "Listed 0 items".
	empty := must(t, s, "GET", "/compute/v1/projects/nimbus-local/aggregated/instances?includeAllScopes=True", "")
	if empty["kind"] != "compute#instanceAggregatedList" {
		t.Errorf("kind = %v", empty["kind"])
	}
	items, ok := empty["items"].(map[string]any)
	if !ok || len(items) == 0 {
		t.Fatalf("items = %v, want a map keyed by scope", empty["items"])
	}
	for scope, v := range items {
		entry := v.(map[string]any)
		if entry["warning"] == nil {
			t.Errorf("%s = %v, want a NO_RESULTS_ON_PAGE warning", scope, entry)
		}
	}

	runInstance(t, s)
	got := must(t, s, "GET", "/compute/v1/projects/nimbus-local/aggregated/instances", "")
	items = got["items"].(map[string]any)
	zone, ok := items["zones/us-central1-a"].(map[string]any)
	if !ok {
		t.Fatalf("items = %v, want the instance's zone as a key", items)
	}
	list, _ := zone["instances"].([]any)
	if len(list) != 1 || str(list[0].(map[string]any)["name"]) != "web" {
		t.Errorf("zones/us-central1-a = %v", zone)
	}
}
