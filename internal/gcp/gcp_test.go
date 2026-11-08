package gcp

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/core"
)

// Every path and body replayed here was captured from the google provider
// 6.x against a logging server, not written from the documentation.

const base = "http://localhost:4703"

func newTestServer(t *testing.T) *Server {
	t.Helper()
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{PublicURL: base}, store, bus)
}

// call replays one request. The `alt=json&prettyPrint=false` the clients
// append is included, because it rides along on nearly every real call.
func call(t *testing.T, s *Server, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer nimbus")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var doc map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			// A download returns bytes, which is not an error here.
			return rec, nil
		}
	}
	return rec, doc
}

func must(t *testing.T, s *Server, method, path, body string) map[string]any {
	t.Helper()
	rec, doc := call(t, s, method, path, body)
	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("%s %s: status %d: %s", method, path, rec.Code, rec.Body)
	}
	return doc
}

// --- the envelope ---------------------------------------------------------

// A client decides "it is not there, create it" from the ERROR SHAPE, not
// from the status alone: it reads error.errors[].reason. A 404 whose reason
// is something else is not recognised, and the apply fails instead of
// creating anything.
func TestNotFoundCarriesTheReasonClientsMatchOn(t *testing.T) {
	s := newTestServer(t)
	rec, doc := call(t, s, http.MethodGet,
		"/compute/v1/projects/nimbus-local/global/networks/absent?alt=json", "")
	if rec.Code != 404 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	e, ok := doc["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error object in %v", doc)
	}
	if e["code"] != 404.0 || e["status"] != "NOT_FOUND" {
		t.Errorf("error = %v", e)
	}
	errs, ok := e["errors"].([]any)
	if !ok || len(errs) == 0 {
		t.Fatalf("no errors[] in %v", e)
	}
	if errs[0].(map[string]any)["reason"] != "notFound" {
		t.Errorf("reason = %v", errs[0])
	}
}

// --- the project ----------------------------------------------------------

// A fresh server must already have its project, for the same reason a fresh
// Nimbus must already list its Azure subscription: a CLI that lists none
// invents one, and everything lands somewhere nobody is looking.
func TestAFreshServerAlreadyHasItsProject(t *testing.T) {
	s := newTestServer(t)
	doc := must(t, s, http.MethodGet, "/v1/projects/nimbus-local", "")
	if doc["projectId"] != "nimbus-local" || doc["lifecycleState"] != "ACTIVE" {
		t.Errorf("v1 project = %v", doc)
	}
	// v3 is a different shape for the same thing, and clients use both.
	doc = must(t, s, http.MethodGet, "/v3/projects/nimbus-local", "")
	if doc["state"] != "ACTIVE" || doc["projectId"] != "nimbus-local" {
		t.Errorf("v3 project = %v", doc)
	}
	if doc["name"] != "projects/"+DefaultProjectNumber {
		t.Errorf("v3 name = %v; it is the NUMBER, not the id", doc["name"])
	}

	list := must(t, s, http.MethodGet, "/v1/projects", "")
	if len(list["projects"].([]any)) != 1 {
		t.Errorf("projects = %v", list["projects"])
	}
}

// Compute has its own view of the project, at a different URL, and the
// provider fetches it to turn a project NUMBER back into an id.
func TestComputeProjectAnswersForTheIdAndTheNumber(t *testing.T) {
	s := newTestServer(t)
	for _, ref := range []string{"nimbus-local", DefaultProjectNumber} {
		doc := must(t, s, http.MethodGet, "/compute/v1/projects/"+ref+"?alt=json", "")
		if doc["kind"] != "compute#project" || doc["name"] != "nimbus-local" {
			t.Errorf("%s: %v", ref, doc)
		}
	}
}

// --- compute --------------------------------------------------------------

// The body is the one the provider really sends, down to
// networkFirewallPolicyEnforcementOrder.
const networkBody = `{"autoCreateSubnetworks":false,"description":"measured",` +
	`"name":"nimbus-net","networkFirewallPolicyEnforcementOrder":"AFTER_CLASSIC_FIREWALL"}`

func TestCreatingANetworkAnswersADoneOperation(t *testing.T) {
	s := newTestServer(t)
	op := must(t, s, http.MethodPost,
		"/compute/v1/projects/nimbus-local/global/networks?alt=json", networkBody)

	// Measured: when the operation already says DONE the provider never
	// polls it. Anything else and it waits.
	if op["status"] != "DONE" || op["kind"] != "compute#operation" {
		t.Fatalf("operation = %v", op)
	}
	if op["operationType"] != "insert" {
		t.Errorf("operationType = %v", op["operationType"])
	}
	want := base + "/compute/v1/projects/nimbus-local/global/networks/nimbus-net"
	if op["targetLink"] != want {
		t.Errorf("targetLink = %v, want %v", op["targetLink"], want)
	}
	// It is still a real operation: a client that does poll must find it.
	op2 := must(t, s, http.MethodGet,
		strings.TrimPrefix(str(op["selfLink"]), base)+"?alt=json", "")
	if op2["status"] != "DONE" {
		t.Errorf("polling the operation gave %v", op2)
	}
}

func TestANetworkReadsBackTheSameTwice(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPost, "/compute/v1/projects/nimbus-local/global/networks?alt=json", networkBody)

	path := "/compute/v1/projects/nimbus-local/global/networks/nimbus-net?alt=json"
	first := must(t, s, http.MethodGet, path, "")
	for _, f := range []string{"id", "name", "selfLink", "creationTimestamp",
		"autoCreateSubnetworks", "routingConfig", "networkFirewallPolicyEnforcementOrder"} {
		if first[f] == nil {
			t.Errorf("a network is missing %q", f)
		}
	}
	// An id is a decimal STRING, not a number: a client unmarshals it into
	// a string field and gets nothing at all if it arrives as a number.
	if _, ok := first["id"].(string); !ok {
		t.Errorf("id = %T %v, want a string", first["id"], first["id"])
	}
	second := must(t, s, http.MethodGet, path, "")
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Errorf("a network changed between reads:\n%s\n%s", a, b)
	}
}

// A subnetwork's network reference arrives as a partial URL and has to come
// back as a full self link, and the network's own `subnetworks` list has to
// learn about it -- the provider reads that list off the PARENT on a later
// refresh, the same parent/child mirror AKS node pools and VNet subnets
// need.
func TestASubnetworkIsMirroredIntoItsNetwork(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPost, "/compute/v1/projects/nimbus-local/global/networks?alt=json", networkBody)
	must(t, s, http.MethodPost,
		"/compute/v1/projects/nimbus-local/regions/us-central1/subnetworks?alt=json",
		`{"ipCidrRange":"10.0.1.0/24","logConfig":{"enable":false},"name":"nimbus-a",`+
			`"network":"projects/nimbus-local/global/networks/nimbus-net",`+
			`"region":"projects/nimbus-local/global/regions/us-central1"}`)

	sub := must(t, s, http.MethodGet,
		"/compute/v1/projects/nimbus-local/regions/us-central1/subnetworks/nimbus-a?alt=json", "")
	wantNet := base + "/compute/v1/projects/nimbus-local/global/networks/nimbus-net"
	if sub["network"] != wantNet {
		t.Errorf("network = %v, want the full self link %v", sub["network"], wantNet)
	}
	if sub["gatewayAddress"] != "10.0.1.1" {
		t.Errorf("gatewayAddress = %v", sub["gatewayAddress"])
	}
	if !strings.HasSuffix(str(sub["region"]), "/regions/us-central1") {
		t.Errorf("region = %v, want a full self link", sub["region"])
	}

	net := must(t, s, http.MethodGet,
		"/compute/v1/projects/nimbus-local/global/networks/nimbus-net?alt=json", "")
	subs, _ := net["subnetworks"].([]any)
	if len(subs) != 1 || !strings.HasSuffix(str(subs[0]), "/subnetworks/nimbus-a") {
		t.Fatalf("the network does not list its subnetwork: %v", net["subnetworks"])
	}

	// And deleting it takes it back out again.
	must(t, s, http.MethodDelete,
		"/compute/v1/projects/nimbus-local/regions/us-central1/subnetworks/nimbus-a?alt=json", "")
	net = must(t, s, http.MethodGet,
		"/compute/v1/projects/nimbus-local/global/networks/nimbus-net?alt=json", "")
	if subs, _ := net["subnetworks"].([]any); len(subs) != 0 {
		t.Errorf("a deleted subnetwork is still listed: %v", subs)
	}
}

func TestFirewallRoundTrips(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPost, "/compute/v1/projects/nimbus-local/global/networks?alt=json", networkBody)
	must(t, s, http.MethodPost, "/compute/v1/projects/nimbus-local/global/firewalls?alt=json",
		`{"allowed":[{"IPProtocol":"tcp","ports":["22"]}],"logConfig":{"enable":false},`+
			`"name":"nimbus-ssh","network":"projects/nimbus-local/global/networks/nimbus-net",`+
			`"priority":1000,"sourceRanges":["0.0.0.0/0"]}`)

	fw := must(t, s, http.MethodGet,
		"/compute/v1/projects/nimbus-local/global/firewalls/nimbus-ssh?alt=json", "")
	allowed := fw["allowed"].([]any)[0].(map[string]any)
	// IPProtocol, with that capitalisation: a client reading "ipProtocol"
	// finds nothing and plans a change on every run.
	if allowed["IPProtocol"] != "tcp" {
		t.Errorf("allowed = %v", allowed)
	}
	if fw["direction"] != "INGRESS" || fw["priority"] != 1000.0 {
		t.Errorf("firewall = %v", fw)
	}
}

// A PATCH may only touch the fields its updateMask names. Applying the
// whole body would silently clear everything the client left out.
func TestPatchAppliesOnlyTheMaskedFields(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPost, "/compute/v1/projects/nimbus-local/global/networks?alt=json", networkBody)
	must(t, s, http.MethodPatch,
		"/compute/v1/projects/nimbus-local/global/networks/nimbus-net?alt=json&updateMask=description",
		`{"description":"changed","autoCreateSubnetworks":true,"name":"renamed"}`)

	net := must(t, s, http.MethodGet,
		"/compute/v1/projects/nimbus-local/global/networks/nimbus-net?alt=json", "")
	if net["description"] != "changed" {
		t.Errorf("the masked field was not applied: %v", net["description"])
	}
	if net["autoCreateSubnetworks"] != false {
		t.Errorf("an unmasked field was applied: autoCreateSubnetworks = %v",
			net["autoCreateSubnetworks"])
	}
	if net["name"] != "nimbus-net" {
		t.Errorf("the name is the resource's identity and must not be patchable: %v", net["name"])
	}
}

func TestRegionsAndZonesAreListedAndLookedUp(t *testing.T) {
	s := newTestServer(t)
	list := must(t, s, http.MethodGet, "/compute/v1/projects/nimbus-local/regions?alt=json", "")
	if len(list["items"].([]any)) == 0 {
		t.Fatal("no regions")
	}
	one := must(t, s, http.MethodGet, "/compute/v1/projects/nimbus-local/regions/us-central1?alt=json", "")
	if one["name"] != "us-central1" || one["status"] != "UP" {
		t.Errorf("region = %v", one)
	}
	zone := must(t, s, http.MethodGet, "/compute/v1/projects/nimbus-local/zones/us-central1-a?alt=json", "")
	if zone["name"] != "us-central1-a" {
		t.Errorf("zone = %v", zone)
	}
	rec, _ := call(t, s, http.MethodGet, "/compute/v1/projects/nimbus-local/regions/mars-west9?alt=json", "")
	if rec.Code != 404 {
		t.Errorf("an unknown region must 404, got %d", rec.Code)
	}
}

// --- IAM ------------------------------------------------------------------

func TestServiceAccountEmailIsDerivedAndStable(t *testing.T) {
	s := newTestServer(t)
	sa := must(t, s, http.MethodPost, "/v1/projects/nimbus-local/serviceAccounts?alt=json&prettyPrint=false",
		`{"accountId":"nimbus-app","serviceAccount":{"displayName":"Nimbus app"}}`)

	want := "nimbus-app@nimbus-local.iam.gserviceaccount.com"
	if sa["email"] != want {
		t.Fatalf("email = %v, want %v", sa["email"], want)
	}
	if sa["name"] != "projects/nimbus-local/serviceAccounts/"+want {
		t.Errorf("name = %v", sa["name"])
	}
	// The provider reads it straight back by email, twice, on every apply.
	for i := 0; i < 2; i++ {
		got := must(t, s, http.MethodGet,
			"/v1/projects/nimbus-local/serviceAccounts/"+want+"?alt=json&prettyPrint=false", "")
		if got["uniqueId"] != sa["uniqueId"] || got["email"] != sa["email"] {
			t.Errorf("read %d differs: %v vs %v", i, got, sa)
		}
	}
	// IAM puts its update mask in the BODY, not the query string.
	must(t, s, http.MethodPatch, "/v1/projects/nimbus-local/serviceAccounts/"+want+"?alt=json",
		`{"serviceAccount":{"displayName":"Renamed","description":"ignored"},"updateMask":"displayName"}`)
	got := must(t, s, http.MethodGet, "/v1/projects/nimbus-local/serviceAccounts/"+want+"?alt=json", "")
	if got["displayName"] != "Renamed" {
		t.Errorf("displayName = %v", got["displayName"])
	}
	if got["description"] != nil {
		t.Errorf("an unmasked field was applied: %v", got["description"])
	}
}

func TestDeletingAServiceAccountAnswersAnEmptyObject(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPost, "/v1/projects/nimbus-local/serviceAccounts?alt=json",
		`{"accountId":"gone","serviceAccount":{}}`)
	email := "gone@nimbus-local.iam.gserviceaccount.com"
	// Not an operation: IAM has none, and a client handed one would poll a
	// URL that does not exist.
	doc := must(t, s, http.MethodDelete, "/v1/projects/nimbus-local/serviceAccounts/"+email+"?alt=json", "")
	if len(doc) != 0 {
		t.Errorf("delete returned %v", doc)
	}
	rec, _ := call(t, s, http.MethodGet, "/v1/projects/nimbus-local/serviceAccounts/"+email+"?alt=json", "")
	if rec.Code != 404 {
		t.Errorf("the account is still there: %d", rec.Code)
	}
}

// --- Cloud Storage --------------------------------------------------------

const bucketBody = `{"iamConfiguration":{"uniformBucketLevelAccess":{"enabled":false}},` +
	`"labels":{"goog-terraform-provisioned":"true"},"lifecycle":{"rule":[]},` +
	`"location":"US","name":"nimbus-data","storageClass":"STANDARD"}`

func TestBucketRoundTripsAndRefusesASecondCreate(t *testing.T) {
	s := newTestServer(t)
	b := must(t, s, http.MethodPost, "/storage/v1/b?alt=json&prettyPrint=false&project=nimbus-local", bucketBody)
	if b["kind"] != "storage#bucket" || b["id"] != "nimbus-data" {
		t.Fatalf("bucket = %v", b)
	}
	// projectNumber and metageneration are strings on the wire.
	if _, ok := b["projectNumber"].(string); !ok {
		t.Errorf("projectNumber = %T", b["projectNumber"])
	}
	labels := b["labels"].(map[string]any)
	if labels["goog-terraform-provisioned"] != "true" {
		t.Errorf("labels = %v", labels)
	}

	rec, _ := call(t, s, http.MethodPost, "/storage/v1/b?project=nimbus-local", bucketBody)
	if rec.Code != 409 {
		t.Errorf("a second create gave %d, want 409", rec.Code)
	}

	got := must(t, s, http.MethodGet, "/storage/v1/b/nimbus-data?alt=json&prettyPrint=false", "")
	if got["selfLink"] != base+"/storage/v1/b/nimbus-data" {
		t.Errorf("selfLink = %v", got["selfLink"])
	}
}

func TestAnObjectUploadsAndComesBackWithItsDigests(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPost, "/storage/v1/b?project=nimbus-local", bucketBody)

	rec, doc := call(t, s, http.MethodPost,
		"/upload/storage/v1/b/nimbus-data/o?uploadType=media&name=hello.txt", "hello nimbus")
	if rec.Code != 200 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	// md5Hash is base64 of the RAW digest, not hex -- and crc32c is
	// Castagnoli, not the CRC-32 everything else uses. A client compares
	// both against its own and rejects an upload that disagrees.
	if doc["md5Hash"] != "KlsnAjju9Vlq3M+LwnpZYw==" { // md5("hello nimbus"), base64 of the raw digest
		t.Errorf("md5Hash = %v", doc["md5Hash"])
	}
	if doc["crc32c"] == "" || doc["crc32c"] == nil {
		t.Errorf("no crc32c: %v", doc)
	}
	if doc["size"] != "12" {
		t.Errorf("size = %v (%T), want the string \"12\"", doc["size"], doc["size"])
	}

	// alt=media returns the BYTES; anything else returns the metadata.
	rec, _ = call(t, s, http.MethodGet, "/storage/v1/b/nimbus-data/o/hello.txt?alt=media", "")
	if rec.Body.String() != "hello nimbus" {
		t.Errorf("download = %q", rec.Body.String())
	}
	meta := must(t, s, http.MethodGet, "/storage/v1/b/nimbus-data/o/hello.txt?alt=json", "")
	if meta["name"] != "hello.txt" || meta["bucket"] != "nimbus-data" {
		t.Errorf("metadata = %v", meta)
	}
}

// The provider uploads with multipart/related: metadata first, bytes
// second, told apart by ORDER because the parts carry no names.
func TestMultipartUploadReadsTheMetadataThenTheBytes(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPost, "/storage/v1/b?project=nimbus-local", bucketBody)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	meta, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json"}})
	_, _ = meta.Write([]byte(`{"name":"from-provider.txt","contentType":"text/plain","metadata":{"owner":"nimbus"}}`))
	data, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"text/plain"}})
	_, _ = data.Write([]byte("body bytes"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost,
		"/upload/storage/v1/b/nimbus-data/o?uploadType=multipart", &buf)
	req.Header.Set("Content-Type", "multipart/related; boundary="+mw.Boundary())
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	if doc["name"] != "from-provider.txt" || doc["contentType"] != "text/plain" {
		t.Fatalf("object = %v", doc)
	}
	if doc["metadata"].(map[string]any)["owner"] != "nimbus" {
		t.Errorf("custom metadata was lost: %v", doc["metadata"])
	}
	rec, _ = call(t, s, http.MethodGet, "/storage/v1/b/nimbus-data/o/from-provider.txt?alt=media", "")
	if rec.Body.String() != "body bytes" {
		t.Errorf("content = %q", rec.Body.String())
	}
}

// A resumable upload is a session URI and then chunks. An incomplete chunk
// must answer 308 with a Range header; a 200 too early and the client stops
// sending, leaving the object silently truncated.
func TestResumableUploadAnswers308UntilTheLastChunk(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPost, "/storage/v1/b?project=nimbus-local", bucketBody)

	req := httptest.NewRequest(http.MethodPost,
		"/upload/storage/v1/b/nimbus-data/o?uploadType=resumable",
		strings.NewReader(`{"name":"big.txt"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Upload-Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("starting the session: %d %s", rec.Code, rec.Body)
	}
	session := rec.Header().Get("Location")
	if session == "" {
		t.Fatal("no Location header: a resumable upload has nowhere to go")
	}
	path := strings.TrimPrefix(session, base)

	// First half, with a Content-Range that says more is coming.
	req = httptest.NewRequest(http.MethodPut, path, strings.NewReader("hello "))
	req.Header.Set("Content-Range", "bytes 0-5/12")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 308 {
		t.Fatalf("an incomplete chunk answered %d, want 308", rec.Code)
	}
	if got := rec.Header().Get("Range"); got != "bytes=0-5" {
		t.Errorf("Range = %q; it is inclusive of both ends", got)
	}

	// Second half completes it.
	req = httptest.NewRequest(http.MethodPut, path, strings.NewReader("nimbus"))
	req.Header.Set("Content-Range", "bytes 6-11/12")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("the final chunk answered %d: %s", rec.Code, rec.Body)
	}
	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	if doc["contentType"] != "text/plain" {
		t.Errorf("X-Upload-Content-Type was lost: %v", doc["contentType"])
	}
	rec, _ = call(t, s, http.MethodGet, "/storage/v1/b/nimbus-data/o/big.txt?alt=media", "")
	if rec.Body.String() != "hello nimbus" {
		t.Errorf("the object is %q, not the whole upload", rec.Body.String())
	}
}

// A bucket with contents is refused, as GCS refuses it: force_destroy works
// by emptying it first, and a silent cascade would destroy a bucket that
// was meant to be kept.
func TestABucketWithObjectsRefusesToBeDeleted(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPost, "/storage/v1/b?project=nimbus-local", bucketBody)
	must(t, s, http.MethodPost, "/upload/storage/v1/b/nimbus-data/o?uploadType=media&name=a.txt", "x")

	rec, _ := call(t, s, http.MethodDelete, "/storage/v1/b/nimbus-data?alt=json", "")
	if rec.Code != 409 {
		t.Fatalf("deleting a full bucket gave %d, want 409", rec.Code)
	}
	// This is the listing force_destroy makes before it deletes.
	list := must(t, s, http.MethodGet, "/storage/v1/b/nimbus-data/o?alt=json&versions=true", "")
	if len(list["items"].([]any)) != 1 {
		t.Fatalf("listing = %v", list)
	}

	rec, _ = call(t, s, http.MethodDelete, "/storage/v1/b/nimbus-data/o/a.txt?alt=json", "")
	if rec.Code != 204 {
		t.Errorf("deleting an object gave %d", rec.Code)
	}
	rec, _ = call(t, s, http.MethodDelete, "/storage/v1/b/nimbus-data?alt=json", "")
	if rec.Code != 204 {
		t.Errorf("deleting the emptied bucket gave %d", rec.Code)
	}
}

// An object name is a flat key that may contain slashes -- it is not a
// path, and splitting on one would turn "img/logo.svg" into two segments
// and a 404.
func TestAnObjectNameMayContainSlashes(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPost, "/storage/v1/b?project=nimbus-local", bucketBody)
	must(t, s, http.MethodPost,
		"/upload/storage/v1/b/nimbus-data/o?uploadType=media&name=img/logo.svg", "<svg/>")

	meta := must(t, s, http.MethodGet, "/storage/v1/b/nimbus-data/o/img/logo.svg?alt=json", "")
	if meta["name"] != "img/logo.svg" {
		t.Errorf("name = %v", meta["name"])
	}
	// And a delimiter listing collapses it into a prefix, which is how a
	// client draws a folder over a flat key space.
	list := must(t, s, http.MethodGet, "/storage/v1/b/nimbus-data/o?delimiter=/", "")
	if p, _ := list["prefixes"].([]any); len(p) != 1 || p[0] != "img/" {
		t.Errorf("prefixes = %v", list["prefixes"])
	}
}

// Everything lands in one store with the other clouds' resources, tagged
// with its own cloud so the visualizer files it under the right tab.
func TestResourcesAreStoredAsGCPWithTheProjectAsRoot(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPost, "/compute/v1/projects/nimbus-local/global/networks?alt=json", networkBody)
	must(t, s, http.MethodPost, "/storage/v1/b?project=nimbus-local", bucketBody)

	byType := map[string]*core.Resource{}
	for _, r := range s.store.List(func(r *core.Resource) bool { return r.Cloud == "gcp" }) {
		byType[r.Type] = r
	}
	for _, want := range []string{TypeProject, TypeNetwork, TypeBucket} {
		if byType[want] == nil {
			t.Fatalf("no %s in the store", want)
		}
	}
	if byType[TypeNetwork].ParentID != byType[TypeProject].ID {
		t.Errorf("a network's parent is %q, want the project", byType[TypeNetwork].ParentID)
	}
	// The id is the self link, which is what core.Resource's own comment
	// has promised since Milestone 1.
	if !strings.HasPrefix(byType[TypeNetwork].ID, base+"/compute/v1/") {
		t.Errorf("a network's id is %q, want a self link", byType[TypeNetwork].ID)
	}
}

// `gcloud storage cp` sends its boundary in SINGLE quotes, which RFC 2045
// does not allow and Go's own parser refuses outright. Measured against
// gcloud 592.0.0: the upload failed with "mime: invalid media parameter"
// and an error that blamed the request rather than the quoting.
func TestTheBoundaryIsReadHoweverTheClientQuotedIt(t *testing.T) {
	cases := map[string]string{
		`multipart/related; boundary='===============150545174933813382=='`: "===============150545174933813382==",
		`multipart/related; boundary="simple"`:                              "simple",
		`multipart/related; boundary=simple`:                                "simple",
		`multipart/related; boundary=abc==`:                                 "abc==",
		`multipart/related`:                                                 "",
	}
	for header, want := range cases {
		if got := boundaryOf(header); got != want {
			t.Errorf("boundaryOf(%q) = %q, want %q", header, got, want)
		}
	}
}

// The same, end to end: an upload whose Content-Type gcloud wrote.
func TestAGcloudStyleMultipartUploadIsAccepted(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPost, "/storage/v1/b?project=nimbus-local", bucketBody)

	boundary := "===============1505451749338133828=="
	body := "--" + boundary + "\r\n" +
		"Content-Type: application/json\r\n\r\n" +
		`{"name":"probe.txt"}` + "\r\n" +
		"--" + boundary + "\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"hello from gcloud\r\n" +
		"--" + boundary + "--\r\n"

	req := httptest.NewRequest(http.MethodPost,
		"/upload/storage/v1/b/nimbus-data/o?alt=json&uploadType=multipart",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "multipart/related; boundary='"+boundary+"'")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	rec, _ = call(t, s, http.MethodGet, "/storage/v1/b/nimbus-data/o/probe.txt?alt=media", "")
	if rec.Body.String() != "hello from gcloud" {
		t.Errorf("content = %q", rec.Body.String())
	}
}

// gcloud asks for the bucket's layout before it uploads. A 404 does not
// stop it, but it puts a red line in the activity feed for something that
// is working.
func TestStorageLayoutIsAnswered(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPost, "/storage/v1/b?project=nimbus-local", bucketBody)
	doc := must(t, s, http.MethodGet, "/storage/v1/b/nimbus-data/storageLayout?alt=json", "")
	if doc["bucket"] != "nimbus-data" {
		t.Errorf("layout = %v", doc)
	}
	if doc["hierarchicalNamespace"].(map[string]any)["enabled"] != false {
		t.Errorf("Nimbus does not emulate a hierarchical bucket and must not claim to: %v", doc)
	}
}

// Anywhere Caches matter far more than they look.
//
// The provider asks for them INSIDE its force_destroy loop and breaks out
// of that loop if the call errors -- so a 404 here is never seen as a 404.
// It silently skips deleting the bucket's objects, and the destroy then
// fails on "the bucket is not empty" with nothing connecting the two.
// Measured against google 6.50.0, which listed the object and then deleted
// the bucket without ever trying to delete the object.
func TestAnywhereCachesAnswersAnEmptyListSoForceDestroyWorks(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPost, "/storage/v1/b?project=nimbus-local", bucketBody)

	// The provider asks with a trailing slash.
	doc := must(t, s, http.MethodGet, "/storage/v1/b/nimbus-data/anywhereCaches/?alt=json", "")
	if doc["kind"] != "storage#anywhereCaches" {
		t.Fatalf("anywhereCaches = %v", doc)
	}
	items, ok := doc["items"].([]any)
	if !ok || len(items) != 0 {
		t.Errorf("items = %v; an empty list is what keeps force_destroy working", doc["items"])
	}
}

// waitFor polls a condition for a few seconds. Several things here finish
// in a background goroutine -- a cluster starting, an operation being
// marked done -- and a test that asserted immediately would be racing them.
func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
