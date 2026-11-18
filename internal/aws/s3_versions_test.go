package aws

import (
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
)

// GET /{bucket}?versions lists what is in the bucket.
//
// It used to fall through to the generic "empty document" answer for an
// unimplemented sub-resource -- under a comment calling that "the safer of
// the two wrong answers", which for this one it was not. The aws provider
// empties a VERSIONED bucket through ListObjectVersions, so an empty list
// means force_destroy deletes nothing, and then the bucket delete fails on
// BucketNotEmpty for ever:
//
//	aws_s3_bucket.assets: Still destroying... [10m0s elapsed]
//
// Measured against aws provider 5.100.0: a destroy that cannot finish, on a
// bucket Terraform created, with nothing connecting the two. This is exactly
// the Cloud Storage anywhereCaches lesson -- it is not enough for an
// unimplemented sub-resource to fail POLITELY, because a client may be
// reading the failure as something else entirely -- and the second time a
// bucket's own force_destroy has been the thing it broke.
//
// Nimbus keeps ONE version of an object, so every object is reported as a
// single version with id "null", which is real S3's id for an object written
// before versioning was enabled. Enabling versioning therefore does not
// actually retain anything here, which is a documented divergence rather
// than a hidden one; what it must not do is make the bucket undeletable.
func TestListObjectVersionsListsEveryObject(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	mustPutVersioning(t, s, "demo")

	for _, key := range []string{"pages/a.html", "pages/b.html", "notes.txt"} {
		if rec := s3req(t, s, http.MethodPut, "/demo/"+key, "hello"); rec.Code != 200 {
			t.Fatalf("putting %s: %d %s", key, rec.Code, rec.Body)
		}
	}

	doc := mustListVersions(t, s, "/demo?versions")
	if len(doc.Versions) != 3 {
		t.Fatalf("expected all three objects as versions, got %d: %+v",
			len(doc.Versions), doc.Versions)
	}
	for _, v := range doc.Versions {
		if v.VersionID == "" {
			t.Errorf("%s has no VersionId; a client deletes by version id", v.Key)
		}
		if !v.IsLatest {
			t.Errorf("%s is the only version there is, so it is the latest", v.Key)
		}
		if v.ETag == "" || v.Size == 0 {
			t.Errorf("%s came back without an etag or a size: %+v", v.Key, v)
		}
	}
	if doc.Name != "demo" {
		t.Errorf("the result does not name the bucket: %q", doc.Name)
	}
}

// A prefix narrows it, because `aws s3 rm` and some tooling pass one.
func TestListObjectVersionsHonoursAPrefix(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	mustPutVersioning(t, s, "demo")
	for _, key := range []string{"pages/a.html", "other/b.html"} {
		s3req(t, s, http.MethodPut, "/demo/"+key, "x")
	}

	doc := mustListVersions(t, s, "/demo?versions&prefix=pages/")
	if len(doc.Versions) != 1 || doc.Versions[0].Key != "pages/a.html" {
		t.Errorf("prefix was not honoured: %+v", doc.Versions)
	}
	if doc.Prefix != "pages/" {
		t.Errorf("the result does not echo the prefix: %q", doc.Prefix)
	}
}

// An empty bucket lists nothing, and is not an error.
func TestListObjectVersionsOnAnEmptyBucketIsEmpty(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")

	doc := mustListVersions(t, s, "/demo?versions")
	if len(doc.Versions) != 0 {
		t.Errorf("an empty bucket has no versions: %+v", doc.Versions)
	}
}

// Deleting by version id deletes the object, which is what emptying a
// versioned bucket actually consists of.
//
// Without this the list is answered, the client deletes each version, every
// delete reports success, and the bucket is still not empty -- a worse
// failure than the one it replaced, because now it looks like it worked.
func TestDeletingByVersionIdEmptiesTheBucket(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	mustPutVersioning(t, s, "demo")
	s3req(t, s, http.MethodPut, "/demo/a.txt", "hello")
	s3req(t, s, http.MethodPut, "/demo/b.txt", "hello")

	// The bucket will not go while it holds anything: that refusal is what
	// force_destroy exists to work around, and the provider depends on it.
	if rec := s3req(t, s, http.MethodDelete, "/demo", ""); rec.Code != 409 {
		t.Fatalf("a bucket with objects should refuse to be deleted, got %d", rec.Code)
	}

	for _, v := range mustListVersions(t, s, "/demo?versions").Versions {
		rec := s3req(t, s, http.MethodDelete,
			"/demo/"+v.Key+"?versionId="+v.VersionID, "")
		if rec.Code != 204 {
			t.Fatalf("deleting %s version %s: %d %s", v.Key, v.VersionID, rec.Code, rec.Body)
		}
	}

	if rec := s3req(t, s, http.MethodDelete, "/demo", ""); rec.Code != 204 {
		t.Fatalf("the bucket is empty now and should delete: %d %s", rec.Code, rec.Body)
	}
}

// The bulk delete the provider really uses carries version ids too.
func TestBulkDeleteWithVersionIdsEmptiesTheBucket(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	mustPutVersioning(t, s, "demo")
	s3req(t, s, http.MethodPut, "/demo/a.txt", "hello")

	body := `<?xml version="1.0" encoding="UTF-8"?>
<Delete><Quiet>false</Quiet>
  <Object><Key>a.txt</Key><VersionId>null</VersionId></Object>
</Delete>`
	rec := s3req(t, s, http.MethodPost, "/demo?delete", body)
	if rec.Code != 200 {
		t.Fatalf("bulk delete: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "a.txt") {
		t.Errorf("the result does not report the key it deleted: %s", rec.Body)
	}
	if rec := s3req(t, s, http.MethodDelete, "/demo", ""); rec.Code != 204 {
		t.Errorf("the bucket should be empty after the bulk delete: %d %s", rec.Code, rec.Body)
	}
}

// --- helpers ---------------------------------------------------------------

type objectVersionsResult struct {
	XMLName  xml.Name `xml:"ListVersionsResult"`
	Name     string   `xml:"Name"`
	Prefix   string   `xml:"Prefix"`
	Versions []struct {
		Key          string `xml:"Key"`
		VersionID    string `xml:"VersionId"`
		IsLatest     bool   `xml:"IsLatest"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int64  `xml:"Size"`
		StorageClass string `xml:"StorageClass"`
	} `xml:"Version"`
}

func mustPutVersioning(t *testing.T, s *Server, bucket string) {
	t.Helper()
	body := `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`
	if rec := s3req(t, s, http.MethodPut, "/"+bucket+"?versioning", body); rec.Code != 200 {
		t.Fatalf("enabling versioning: %d %s", rec.Code, rec.Body)
	}
}

func mustListVersions(t *testing.T, s *Server, path string) objectVersionsResult {
	t.Helper()
	rec := s3req(t, s, http.MethodGet, path, "")
	if rec.Code != 200 {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
	}
	var doc objectVersionsResult
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("GET %s is not a ListVersionsResult: %v\n%s", path, err, rec.Body)
	}
	return doc
}
