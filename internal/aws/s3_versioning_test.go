package aws

import (
	"encoding/xml"
	"net/http"
	"testing"
)

// Object versioning as a HISTORY, which is what `?versioning` round-tripped
// as a setting without ever being.
//
// Before this, every PUT overwrote: a bucket with versioning Enabled held
// one version of each object, reported under the id "null". The setting
// applied cleanly and did nothing, which is the shape of divergence this
// project is most careful about -- a client that enables versioning to see
// an overwrite kept gets no error and no version.

func mustSuspendVersioning(t *testing.T, s *Server, bucket string) {
	t.Helper()
	body := `<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>`
	if rec := s3req(t, s, http.MethodPut, "/"+bucket+"?versioning", body); rec.Code != 200 {
		t.Fatalf("suspending versioning: %d %s", rec.Code, rec.Body)
	}
}

// putVersion writes an object and returns the version id the response
// reports, which is the id a client would then use.
func putVersion(t *testing.T, s *Server, path, body string) string {
	t.Helper()
	rec := s3req(t, s, http.MethodPut, path, body)
	if rec.Code != 200 {
		t.Fatalf("PUT %s: %d %s", path, rec.Code, rec.Body)
	}
	return rec.Header().Get("x-amz-version-id")
}

// The central claim: an overwrite against a versioned bucket keeps what it
// replaced, and both versions are readable by id.
func TestAnOverwriteKeepsThePreviousVersion(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	mustPutVersioning(t, s, "demo")

	first := putVersion(t, s, "/demo/a.txt", "one")
	second := putVersion(t, s, "/demo/a.txt", "two")

	if first == "" || second == "" {
		t.Fatalf("a versioned PUT must report x-amz-version-id: %q then %q", first, second)
	}
	if first == second {
		t.Fatalf("two writes got the same version id: %q", first)
	}

	// The current version is the second.
	if rec := s3req(t, s, http.MethodGet, "/demo/a.txt", ""); rec.Body.String() != "two" {
		t.Errorf("the current version is %q, want \"two\"", rec.Body)
	}
	// And the first is still there, by id.
	rec := s3req(t, s, http.MethodGet, "/demo/a.txt?versionId="+first, "")
	if rec.Code != 200 || rec.Body.String() != "one" {
		t.Errorf("reading the previous version: %d %q, want 200 \"one\"", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("x-amz-version-id"); got != first {
		t.Errorf("the read reports version %q, want %q", got, first)
	}

	// A plain listing shows the key ONCE, because a history is not a
	// bucket full of objects.
	var listed struct {
		Contents []struct {
			Key string `xml:"Key"`
		} `xml:"Contents"`
	}
	lr := s3req(t, s, http.MethodGet, "/demo?list-type=2", "")
	if err := xml.Unmarshal(lr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("listing: %v\n%s", err, lr.Body)
	}
	if len(listed.Contents) != 1 {
		t.Errorf("ListObjectsV2 reports %d objects, want 1", len(listed.Contents))
	}
	// The version listing shows both, newest first, with IsLatest on the
	// first of them.
	vs := mustListVersions(t, s, "/demo?versions").Versions
	if len(vs) != 2 {
		t.Fatalf("?versions reports %d versions, want 2", len(vs))
	}
	if vs[0].VersionID != second || !vs[0].IsLatest {
		t.Errorf("the first entry is %q IsLatest=%v, want %q and true", vs[0].VersionID, vs[0].IsLatest, second)
	}
	if vs[1].VersionID != first || vs[1].IsLatest {
		t.Errorf("the second entry is %q IsLatest=%v, want %q and false", vs[1].VersionID, vs[1].IsLatest, first)
	}
}

// A delete against a versioned bucket lays a MARKER: the object reads 404
// and its history survives. This is the piece that makes versioning a
// history rather than a label -- and removing the marker undeletes it.
func TestADeleteLaysAMarkerAndRemovingItUndeletes(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	mustPutVersioning(t, s, "demo")
	written := putVersion(t, s, "/demo/a.txt", "one")

	del := s3req(t, s, http.MethodDelete, "/demo/a.txt", "")
	if del.Code != 204 {
		t.Fatalf("delete: %d %s", del.Code, del.Body)
	}
	if del.Header().Get("x-amz-delete-marker") != "true" {
		t.Error("the delete does not report that it laid a marker")
	}
	marker := del.Header().Get("x-amz-version-id")
	if marker == "" || marker == written {
		t.Fatalf("the marker's version id is %q", marker)
	}

	// The object is gone, and the 404 says WHY -- which a client cannot
	// otherwise tell from a key that never existed.
	get := s3req(t, s, http.MethodGet, "/demo/a.txt", "")
	if get.Code != 404 {
		t.Errorf("GET after the delete = %d, want 404", get.Code)
	}
	if get.Header().Get("x-amz-delete-marker") != "true" {
		t.Error("the 404 does not say a delete marker is in the way")
	}
	// The earlier version is still readable by id.
	if rec := s3req(t, s, http.MethodGet, "/demo/a.txt?versionId="+written, ""); rec.Body.String() != "one" {
		t.Errorf("the version under the marker reads %q, want \"one\"", rec.Body)
	}
	// Asking for the MARKER by id is 405, not 404: it exists, it just has
	// no bytes to GET.
	if rec := s3req(t, s, http.MethodGet, "/demo/a.txt?versionId="+marker, ""); rec.Code != 405 {
		t.Errorf("GET of the marker itself = %d, want 405", rec.Code)
	}

	// The listing separates them, and the marker is the latest.
	doc := mustListVersions(t, s, "/demo?versions")
	if len(doc.Versions) != 1 || len(doc.DeleteMarkers) != 1 {
		t.Fatalf("?versions: %d versions and %d markers, want 1 and 1",
			len(doc.Versions), len(doc.DeleteMarkers))
	}
	if !doc.DeleteMarkers[0].IsLatest || doc.Versions[0].IsLatest {
		t.Error("the marker should be the latest version, not the object")
	}

	// Removing the marker brings the object back. This is the behaviour
	// that makes a delete marker worth having rather than a slower delete.
	if rec := s3req(t, s, http.MethodDelete, "/demo/a.txt?versionId="+marker, ""); rec.Code != 204 {
		t.Fatalf("removing the marker: %d %s", rec.Code, rec.Body)
	}
	if rec := s3req(t, s, http.MethodGet, "/demo/a.txt", ""); rec.Code != 200 || rec.Body.String() != "one" {
		t.Errorf("after removing the marker: %d %q, want 200 \"one\"", rec.Code, rec.Body)
	}
}

// A bucket holding only delete markers is NOT empty, and S3's refusal to
// delete it is what force_destroy exists to work around. Reporting it as
// empty would make the provider stop after one pass.
func TestABucketOfDeleteMarkersIsNotEmpty(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	mustPutVersioning(t, s, "demo")
	putVersion(t, s, "/demo/a.txt", "one")
	s3req(t, s, http.MethodDelete, "/demo/a.txt", "")

	if rec := s3req(t, s, http.MethodDelete, "/demo", ""); rec.Code != 409 {
		t.Errorf("deleting a bucket whose history survives = %d, want 409", rec.Code)
	}

	// And a bucket holding ONLY a marker is still not empty, which is the
	// case the count has to get right: removing the object's one version
	// leaves nothing readable, and real S3 still refuses the bucket.
	// Without this, a count that skipped markers would pass everything
	// above -- it did, when the bug was injected to check.
	for _, v := range mustListVersions(t, s, "/demo?versions").Versions {
		s3req(t, s, http.MethodDelete, "/demo/"+v.Key+"?versionId="+v.VersionID, "")
	}
	if doc := mustListVersions(t, s, "/demo?versions"); len(doc.Versions) != 0 || len(doc.DeleteMarkers) != 1 {
		t.Fatalf("expected a bucket holding one marker and nothing else, got %d and %d",
			len(doc.Versions), len(doc.DeleteMarkers))
	}
	if rec := s3req(t, s, http.MethodDelete, "/demo", ""); rec.Code != 409 {
		t.Errorf("deleting a bucket holding only a delete marker = %d, want 409", rec.Code)
	}

	// Emptying it means deleting every version AND every marker by id,
	// which is exactly what the provider does.
	doc := mustListVersions(t, s, "/demo?versions")
	for _, v := range doc.Versions {
		s3req(t, s, http.MethodDelete, "/demo/"+v.Key+"?versionId="+v.VersionID, "")
	}
	for _, m := range doc.DeleteMarkers {
		s3req(t, s, http.MethodDelete, "/demo/"+m.Key+"?versionId="+m.VersionID, "")
	}
	if rec := s3req(t, s, http.MethodDelete, "/demo", ""); rec.Code != 204 {
		t.Errorf("the bucket is empty now: %d %s", rec.Code, rec.Body)
	}
}

// Suspending versioning is NOT the same as never having had it. The
// history already written stays; new writes carry the id "null" and
// replace each other. Getting this wrong either way loses data or grows
// the bucket for ever.
func TestSuspendingVersioningKeepsTheHistoryAndStopsAddingToIt(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	mustPutVersioning(t, s, "demo")
	kept := putVersion(t, s, "/demo/a.txt", "one")

	mustSuspendVersioning(t, s, "demo")
	if id := putVersion(t, s, "/demo/a.txt", "two"); id != nullVersionID {
		t.Errorf("a suspended write reports version %q, want %q", id, nullVersionID)
	}
	if id := putVersion(t, s, "/demo/a.txt", "three"); id != nullVersionID {
		t.Errorf("a second suspended write reports version %q", id)
	}

	vs := mustListVersions(t, s, "/demo?versions").Versions
	if len(vs) != 2 {
		t.Fatalf("after two suspended writes over one version: %d versions, want 2", len(vs))
	}
	if rec := s3req(t, s, http.MethodGet, "/demo/a.txt?versionId="+kept, ""); rec.Body.String() != "one" {
		t.Errorf("the version from before suspension reads %q, want \"one\"", rec.Body)
	}
	if rec := s3req(t, s, http.MethodGet, "/demo/a.txt", ""); rec.Body.String() != "three" {
		t.Errorf("the current version reads %q, want \"three\"", rec.Body)
	}
}

// A bucket that has never had versioning behaves exactly as it did, which
// is what keeps every existing example and test true. The id is "null" and
// the header is ABSENT -- sending it would tell a client an unversioned
// bucket has versions.
func TestAnUnversionedBucketIsUnchanged(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")

	if id := putVersion(t, s, "/demo/a.txt", "one"); id != "" {
		t.Errorf("an unversioned PUT reported x-amz-version-id %q", id)
	}
	putVersion(t, s, "/demo/a.txt", "two")
	if vs := mustListVersions(t, s, "/demo?versions").Versions; len(vs) != 1 {
		t.Errorf("an overwrite in an unversioned bucket left %d versions, want 1", len(vs))
	}
	if rec := s3req(t, s, http.MethodGet, "/demo/a.txt", ""); rec.Body.String() != "two" {
		t.Errorf("the object reads %q, want \"two\"", rec.Body)
	}
	// A delete removes it outright: no marker, and the bucket is empty.
	if rec := s3req(t, s, http.MethodDelete, "/demo/a.txt", ""); rec.Header().Get("x-amz-delete-marker") != "" {
		t.Error("an unversioned delete reported a delete marker")
	}
	if rec := s3req(t, s, http.MethodDelete, "/demo", ""); rec.Code != 204 {
		t.Errorf("the bucket should be empty: %d %s", rec.Code, rec.Body)
	}
}
