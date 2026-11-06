package aws

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Asere231/nimbus/internal/core"
)

// s3req replays an S3 request: path-style, with the SigV4 scope naming s3.
func s3req(t *testing.T, s *Server, method, path string, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=nimbus/20261106/us-east-1/s3/aws4_request, "+
			"SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=2c90a61f")
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	return rec
}

func makeBucket(t *testing.T, s *Server, name string) {
	t.Helper()
	if rec := s3req(t, s, http.MethodPut, "/"+name, ""); rec.Code != 200 {
		t.Fatalf("creating %s: %d %s", name, rec.Code, rec.Body)
	}
}

func TestBucketLifecycle(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")

	if rec := s3req(t, s, http.MethodHead, "/demo", ""); rec.Code != 200 {
		t.Errorf("HEAD bucket = %d", rec.Code)
	}
	if rec := s3req(t, s, http.MethodHead, "/nope", ""); rec.Code != 404 {
		t.Errorf("HEAD of a missing bucket = %d, want 404", rec.Code)
	}

	var listed struct {
		Buckets []struct {
			Name string `xml:"Name"`
		} `xml:"Buckets>Bucket"`
	}
	rec := s3req(t, s, http.MethodGet, "/", "")
	if err := xml.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("%v\n%s", err, rec.Body)
	}
	if len(listed.Buckets) != 1 || listed.Buckets[0].Name != "demo" {
		t.Errorf("buckets = %+v", listed.Buckets)
	}

	// A bucket is a graph node under the account.
	res, err := s.store.Get("arn:aws:s3:::demo")
	if err != nil {
		t.Fatal(err)
	}
	if res.Type != typeBucket || res.ParentID != s.accountARN() || res.Backing != "real" {
		t.Errorf("bucket resource = %+v", res)
	}

	if rec := s3req(t, s, http.MethodDelete, "/demo", ""); rec.Code != 204 {
		t.Errorf("DELETE bucket = %d %s", rec.Code, rec.Body)
	}
}

// S3 refuses to delete a bucket with anything in it, and the provider
// depends on that: force_destroy works by emptying it first.
func TestDeletingANonEmptyBucketIsRefused(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	s3req(t, s, http.MethodPut, "/demo/a.txt", "hello")

	rec := s3req(t, s, http.MethodDelete, "/demo", "")
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "BucketNotEmpty") {
		t.Fatalf("deleting a full bucket = %d:\n%s", rec.Code, rec.Body)
	}
	s3req(t, s, http.MethodDelete, "/demo/a.txt", "")
	if rec := s3req(t, s, http.MethodDelete, "/demo", ""); rec.Code != 204 {
		t.Errorf("after emptying it, delete = %d", rec.Code)
	}
}

// THE lesson of this checkpoint, and of Milestone 4's Key Vault before it.
//
// After creating a bucket the provider reads every sub-configuration. Each
// must answer the way AWS answers when nothing has been set, with its OWN
// error code -- the provider is written to tolerate exactly those. A 501,
// or the wrong code, fails an apply on a bucket created a moment earlier.
func TestUnsetSubConfigurationsAnswerWithTheirOwnCode(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")

	for sub, code := range map[string]string{
		"tagging":           "NoSuchTagSet",
		"policy":            "NoSuchBucketPolicy",
		"cors":              "NoSuchCORSConfiguration",
		"website":           "NoSuchWebsiteConfiguration",
		"lifecycle":         "NoSuchLifecycleConfiguration",
		"replication":       "ReplicationConfigurationNotFoundError",
		"publicAccessBlock": "NoSuchPublicAccessBlockConfiguration",
		"encryption":        "ServerSideEncryptionConfigurationNotFoundError",
		"ownershipControls": "OwnershipControlsNotFoundError",
		"object-lock":       "ObjectLockConfigurationNotFoundError",
	} {
		rec := s3req(t, s, http.MethodGet, "/demo?"+sub, "")
		if rec.Code != 404 {
			t.Errorf("GET ?%s = %d, want 404", sub, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), code) {
			t.Errorf("GET ?%s should answer %s:\n%s", sub, code, rec.Body)
		}
	}
}

// The other half: the ones that are never an error, because every bucket
// has one whether you set it or not.
func TestTheAlwaysPresentSubConfigurations(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	for _, sub := range []string{"location", "versioning", "acl", "requestPayment", "accelerate", "logging", "notification"} {
		if rec := s3req(t, s, http.MethodGet, "/demo?"+sub, ""); rec.Code != 200 {
			t.Errorf("GET ?%s = %d, want 200:\n%s", sub, rec.Code, rec.Body)
		}
	}
	// us-east-1 is reported as an EMPTY LocationConstraint, which is S3's
	// oldest wart and still how every client reads it.
	rec := s3req(t, s, http.MethodGet, "/demo?location", "")
	var loc struct {
		Value string `xml:",chardata"`
	}
	_ = xml.Unmarshal(rec.Body.Bytes(), &loc)
	if strings.TrimSpace(loc.Value) != "" {
		t.Errorf("location = %q, want empty for us-east-1", loc.Value)
	}
}

func TestVersioningAndTaggingRoundTrip(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")

	s3req(t, s, http.MethodPut, "/demo?versioning",
		`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`)
	rec := s3req(t, s, http.MethodGet, "/demo?versioning", "")
	if !strings.Contains(rec.Body.String(), "<Status>Enabled</Status>") {
		t.Errorf("versioning did not round-trip:\n%s", rec.Body)
	}

	s3req(t, s, http.MethodPut, "/demo?tagging",
		`<Tagging><TagSet><Tag><Key>env</Key><Value>dev</Value></Tag></TagSet></Tagging>`)
	rec = s3req(t, s, http.MethodGet, "/demo?tagging", "")
	if !strings.Contains(rec.Body.String(), "<Key>env</Key>") {
		t.Errorf("tagging did not round-trip:\n%s", rec.Body)
	}
	// Deleting it brings the "not set" error back, rather than an empty
	// document -- which is what the provider checks to notice a removal.
	s3req(t, s, http.MethodDelete, "/demo?tagging", "")
	if rec := s3req(t, s, http.MethodGet, "/demo?tagging", ""); rec.Code != 404 {
		t.Errorf("after deleting the tags, GET ?tagging = %d, want 404", rec.Code)
	}
}

// A bucket policy is JSON, not XML: the one S3 call that is.
func TestBucketPolicyIsJSON(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	policy := `{"Version":"2012-10-17","Statement":[]}`
	s3req(t, s, http.MethodPut, "/demo?policy", policy)

	rec := s3req(t, s, http.MethodGet, "/demo?policy", "")
	if got := rec.Body.String(); got != policy {
		t.Errorf("policy = %q, want it byte for byte", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q", ct)
	}
}

func TestObjectRoundTrip(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")

	content := "hello from nimbus\n"
	rec := s3req(t, s, http.MethodPut, "/demo/notes/hello.txt", content,
		"Content-Type", "text/plain", "x-amz-meta-Owner", "bryan")
	if rec.Code != 200 {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	// The ETag of a single-part upload is the MD5 of the content, and
	// clients compare it to decide whether to upload again. Unlike Azure's
	// Content-MD5 -- a content setting that must not be invented -- this
	// one is always the digest.
	sum := md5.Sum([]byte(content))
	want := `"` + hex.EncodeToString(sum[:]) + `"`
	if got := rec.Header().Get("ETag"); got != want {
		t.Errorf("ETag = %s, want the md5 %s", got, want)
	}

	rec = s3req(t, s, http.MethodGet, "/demo/notes/hello.txt", "")
	if rec.Code != 200 || rec.Body.String() != content {
		t.Fatalf("GET = %d %q", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain" {
		t.Errorf("content type = %q", ct)
	}
	// Metadata comes back lower-cased and written verbatim: a client
	// strips the prefix and uses the rest as a literal key, so "Owner"
	// would never be found. The same trap Azure's blob metadata had.
	if got := rec.Header()["x-amz-meta-owner"]; len(got) != 1 || got[0] != "bryan" {
		t.Errorf("metadata headers = %v", rec.Header())
	}

	rec = s3req(t, s, http.MethodHead, "/demo/notes/hello.txt", "")
	if rec.Code != 200 || rec.Body.Len() != 0 {
		t.Errorf("HEAD = %d with %d bytes, want 200 and none", rec.Code, rec.Body.Len())
	}
	if rec.Header().Get("Content-Length") != "18" {
		t.Errorf("HEAD content length = %q", rec.Header().Get("Content-Length"))
	}

	if rec := s3req(t, s, http.MethodGet, "/demo/missing", ""); rec.Code != 404 ||
		!strings.Contains(rec.Body.String(), "NoSuchKey") {
		t.Errorf("GET of a missing key = %d:\n%s", rec.Code, rec.Body)
	}
}

// Deleting something that is not there succeeds, which is what makes a
// destroy idempotent.
func TestDeletingAMissingObjectSucceeds(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	if rec := s3req(t, s, http.MethodDelete, "/demo/never-existed", ""); rec.Code != 204 {
		t.Errorf("= %d, want 204", rec.Code)
	}
}

func TestListObjectsWithPrefixAndDelimiter(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	for _, key := range []string{"a.txt", "img/one.png", "img/two.png", "docs/x.md"} {
		s3req(t, s, http.MethodPut, "/demo/"+key, "x")
	}

	list := func(query string) (keys []string, prefixes []string) {
		rec := s3req(t, s, http.MethodGet, "/demo?list-type=2"+query, "")
		var doc struct {
			Contents []struct {
				Key  string `xml:"Key"`
				Size int64  `xml:"Size"`
			} `xml:"Contents"`
			CommonPrefixes []struct {
				Prefix string `xml:"Prefix"`
			} `xml:"CommonPrefixes"`
		}
		if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("%v\n%s", err, rec.Body)
		}
		for _, c := range doc.Contents {
			keys = append(keys, c.Key)
		}
		for _, p := range doc.CommonPrefixes {
			prefixes = append(prefixes, p.Prefix)
		}
		return
	}

	keys, _ := list("")
	// Sorted by key, which is the order S3 guarantees.
	if len(keys) != 4 || keys[0] != "a.txt" || keys[1] != "docs/x.md" {
		t.Errorf("keys = %v", keys)
	}
	keys, _ = list("&prefix=img/")
	if len(keys) != 2 {
		t.Errorf("prefixed keys = %v", keys)
	}
	// A delimiter rolls everything below it into a common prefix, which is
	// how a client shows folders for a store that has none.
	keys, prefixes := list("&delimiter=/")
	if len(keys) != 1 || keys[0] != "a.txt" {
		t.Errorf("top-level keys = %v", keys)
	}
	if len(prefixes) != 2 {
		t.Errorf("common prefixes = %v, want docs/ and img/", prefixes)
	}
}

func TestBulkDelete(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	s3req(t, s, http.MethodPut, "/demo/a", "1")
	s3req(t, s, http.MethodPut, "/demo/b", "2")

	rec := s3req(t, s, http.MethodPost, "/demo?delete",
		`<Delete><Object><Key>a</Key></Object><Object><Key>b</Key></Object></Delete>`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<Key>a</Key>") {
		t.Fatalf("bulk delete = %d:\n%s", rec.Code, rec.Body)
	}
	if n := s.objects.count("demo"); n != 0 {
		t.Errorf("%d objects left", n)
	}
}

// Nothing a client sends is trusted with a path. "../.." is a legal S3 key
// -- S3 stores it flat and it means nothing -- but written through a
// filepath.Join it would leave the data directory entirely.
func TestKeysCannotEscapeTheBucketDirectory(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	for _, key := range []string{"../escape", "a/../../escape", "x/../../../etc/passwd"} {
		rec := s3req(t, s, http.MethodPut, "/demo/"+key, "x")
		if rec.Code != 400 {
			t.Errorf("PUT %q = %d, want 400", key, rec.Code)
		}
	}
	if _, ok := cleanKey("img/logo.svg"); !ok {
		t.Error("an ordinary nested key must still be allowed")
	}
}

// Objects are bytes on disk with --persist, and they have to come back.
func TestObjectsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, dir)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{DataDir: dir}, store, bus)
	makeBucket(t, s, "demo")
	s3req(t, s, http.MethodPut, "/demo/notes/hello.txt", "hello from nimbus",
		"Content-Type", "text/plain", "x-amz-meta-owner", "bryan")

	// A new process over the same directory.
	bus2 := core.NewBus(100)
	store2, err := core.NewStore(bus2, dir)
	if err != nil {
		t.Fatal(err)
	}
	s2 := New(Config{DataDir: dir}, store2, bus2)

	rec := s3req(t, s2, http.MethodGet, "/demo/notes/hello.txt", "")
	if rec.Code != 200 || rec.Body.String() != "hello from nimbus" {
		t.Fatalf("after a restart, GET = %d %q", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain" {
		t.Errorf("content type did not survive: %q", ct)
	}
	if got := rec.Header()["x-amz-meta-owner"]; len(got) != 1 {
		t.Errorf("metadata did not survive: %v", rec.Header())
	}

	// The key keeps its own shape on disk, so a nested key is a file in a
	// directory rather than a file with a slash in its name.
	if _, err := os.Stat(dir + "/s3/demo/notes/hello.txt"); err != nil {
		t.Errorf("the object is not where it should be on disk: %v", err)
	}
}

// Without --persist nothing is written, which is the other half of the
// promise: a local emulator should not litter the disk unasked.
func TestWithoutPersistNothingIsWritten(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	s3req(t, s, http.MethodPut, "/demo/a.txt", "hello")
	if s.objects.dir != "" {
		t.Errorf("the object store has a directory (%q) with no DataDir set", s.objects.dir)
	}
	if _, _, ok := s.objects.get("demo", "a.txt"); !ok {
		t.Error("the object should still be readable from memory")
	}
}

// An operation on a bucket nobody created is NoSuchBucket, not a crash or
// a silent create.
func TestOperationsOnAMissingBucket(t *testing.T) {
	s := newTestServer(t)
	for _, path := range []string{"/nope?versioning", "/nope/key.txt", "/nope?list-type=2"} {
		rec := s3req(t, s, http.MethodGet, path, "")
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), "NoSuchBucket") {
			t.Errorf("GET %s = %d:\n%s", path, rec.Code, rec.Body)
		}
	}
}

// A clean refresh reads a dozen unset sub-resources and gets a dozen 404s.
// They are correct answers, so the activity feed must not paint them as
// failures -- a feed that cries wolf on every plan teaches people to stop
// reading it.
func TestExpected404sAreMarkedSoInTheFeed(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")

	before := len(s.bus.History())
	s3req(t, s, http.MethodGet, "/demo?tagging", "")
	s3req(t, s, http.MethodGet, "/demo?policy", "")
	// A genuine mistake, which must still be flagged.
	s3req(t, s, http.MethodGet, "/no-such-bucket?tagging", "")

	events := s.bus.History()[before:]
	if len(events) != 3 {
		t.Fatalf("published %d events, want 3", len(events))
	}
	for i, e := range events[:2] {
		if !strings.HasPrefix(e.Note, "expected:") {
			t.Errorf("event %d note = %q, want it marked expected", i, e.Note)
		}
		if e.Status != 404 {
			t.Errorf("event %d status = %d, want the real 404 to still be reported", i, e.Status)
		}
	}
	if strings.HasPrefix(events[2].Note, "expected:") {
		t.Errorf("a missing bucket is a real error, not an expected one: %q", events[2].Note)
	}
}
