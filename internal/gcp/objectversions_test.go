package gcp

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"
)

// Cloud Storage object versioning, which `versioning` round-tripped as a
// bucket setting without ever being: every write bumped the generation and
// the previous generation was gone.
//
// GCS's model is NOT S3's, and the difference is the interesting part. S3
// lays a delete MARKER over the history as a new version. GCS has no
// marker: a delete ARCHIVES the live generation by stamping timeDeleted on
// it, so there is no new generation and nothing to "undelete" by removing.

func makeVersionedBucket(t *testing.T, s *Server, name string, versioned bool) {
	t.Helper()
	body := `{"name":"` + name + `"}`
	if versioned {
		body = `{"name":"` + name + `","versioning":{"enabled":true}}`
	}
	must(t, s, http.MethodPost, "/storage/v1/b?project="+s.cfg.ProjectID, body)
}

// putObjectBytes writes an object through the media upload path and
// returns its generation.
func putObjectBytes(t *testing.T, s *Server, bucket, name, content string) string {
	t.Helper()
	path := "/upload/storage/v1/b/" + bucket + "/o?uploadType=media&name=" + url.QueryEscape(name)
	doc := must(t, s, http.MethodPost, path, content)
	gen := str(doc["generation"])
	if gen == "" {
		t.Fatalf("a write of %q reported no generation: %v", name, doc)
	}
	return gen
}

func listVersions(t *testing.T, s *Server, bucket string) []map[string]any {
	t.Helper()
	doc := must(t, s, http.MethodGet, "/storage/v1/b/"+bucket+"/o?versions=true", "")
	items, _ := doc["items"].([]any)
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		m, _ := it.(map[string]any)
		out = append(out, m)
	}
	return out
}

// The central claim: an overwrite against a versioned bucket keeps the
// generation it replaced, and that generation is still readable by number.
func TestAnOverwriteKeepsThePreviousGeneration(t *testing.T) {
	s := newTestServer(t)
	makeVersionedBucket(t, s, "vers", true)

	first := putObjectBytes(t, s, "vers", "a.txt", "one")
	second := putObjectBytes(t, s, "vers", "a.txt", "two")
	if first == second {
		t.Fatalf("two writes got the same generation: %s", first)
	}

	// The live generation is the second.
	rec, _ := call(t, s, http.MethodGet, "/storage/v1/b/vers/o/a.txt?alt=media", "")
	if rec.Body.String() != "two" {
		t.Errorf("the live generation reads %q, want \"two\"", rec.Body)
	}
	// And the first is still there, by number.
	rec, _ = call(t, s, http.MethodGet,
		"/storage/v1/b/vers/o/a.txt?alt=media&generation="+first, "")
	if rec.Code != 200 || rec.Body.String() != "one" {
		t.Errorf("reading generation %s: %d %q, want 200 \"one\"", first, rec.Code, rec.Body)
	}

	// An ordinary listing shows the object ONCE.
	doc := must(t, s, http.MethodGet, "/storage/v1/b/vers/o", "")
	if items, _ := doc["items"].([]any); len(items) != 1 {
		t.Errorf("the plain listing reports %d objects, want 1", len(items))
	}
	// versions=true shows both, oldest first, and only the archived one
	// carries timeDeleted.
	vs := listVersions(t, s, "vers")
	if len(vs) != 2 {
		t.Fatalf("versions=true reports %d generations, want 2", len(vs))
	}
	if str(vs[0]["generation"]) != first || str(vs[1]["generation"]) != second {
		t.Errorf("generations came back as %s then %s, want %s then %s",
			str(vs[0]["generation"]), str(vs[1]["generation"]), first, second)
	}
	if _, archived := vs[0]["timeDeleted"]; !archived {
		t.Error("the replaced generation is not marked archived with timeDeleted")
	}
	if _, archived := vs[1]["timeDeleted"]; archived {
		t.Error("the live generation is marked archived")
	}
}

// A delete archives the live generation. There is no delete marker and no
// new generation -- the object simply has none that is live.
func TestADeleteArchivesTheLiveGeneration(t *testing.T) {
	s := newTestServer(t)
	makeVersionedBucket(t, s, "vers", true)
	gen := putObjectBytes(t, s, "vers", "a.txt", "one")

	if rec, _ := call(t, s, http.MethodDelete, "/storage/v1/b/vers/o/a.txt", ""); rec.Code != 204 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec, _ := call(t, s, http.MethodGet, "/storage/v1/b/vers/o/a.txt", ""); rec.Code != 404 {
		t.Errorf("reading the object after the delete = %d, want 404", rec.Code)
	}
	// But the generation is still there, and still readable by number.
	rec, _ := call(t, s, http.MethodGet,
		"/storage/v1/b/vers/o/a.txt?alt=media&generation="+gen, "")
	if rec.Code != 200 || rec.Body.String() != "one" {
		t.Errorf("reading the archived generation: %d %q", rec.Code, rec.Body)
	}
	vs := listVersions(t, s, "vers")
	if len(vs) != 1 {
		t.Fatalf("versions=true reports %d generations, want 1", len(vs))
	}
	if _, archived := vs[0]["timeDeleted"]; !archived {
		t.Error("the deleted generation does not carry timeDeleted")
	}
	// And the object is absent from an ordinary listing.
	doc := must(t, s, http.MethodGet, "/storage/v1/b/vers/o", "")
	if items, _ := doc["items"].([]any); len(items) != 0 {
		t.Errorf("a deleted object still appears in the plain listing: %v", items)
	}
}

// A bucket holding only archived generations is NOT empty, and GCS's
// refusal to delete it is what force_destroy exists to work around.
func TestABucketOfArchivedGenerationsIsNotEmpty(t *testing.T) {
	s := newTestServer(t)
	makeVersionedBucket(t, s, "vers", true)
	putObjectBytes(t, s, "vers", "a.txt", "one")
	putObjectBytes(t, s, "vers", "a.txt", "two")
	call(t, s, http.MethodDelete, "/storage/v1/b/vers/o/a.txt", "")

	// Nothing is live now, and the bucket still refuses.
	if rec, _ := call(t, s, http.MethodGet, "/storage/v1/b/vers/o/a.txt", ""); rec.Code != 404 {
		t.Fatalf("the object should read 404: %d", rec.Code)
	}
	if rec, _ := call(t, s, http.MethodDelete, "/storage/v1/b/vers", ""); rec.Code != 409 {
		t.Errorf("deleting a bucket whose archive survives = %d, want 409", rec.Code)
	}

	// Emptying it means deleting each generation by number, which is what
	// the provider does after its versions=true listing.
	for _, v := range listVersions(t, s, "vers") {
		path := "/storage/v1/b/vers/o/" + str(v["name"]) + "?generation=" + str(v["generation"])
		if rec, _ := call(t, s, http.MethodDelete, path, ""); rec.Code != 204 {
			t.Fatalf("deleting generation %s: %d", str(v["generation"]), rec.Code)
		}
	}
	if rec, _ := call(t, s, http.MethodDelete, "/storage/v1/b/vers", ""); rec.Code != 204 {
		t.Errorf("the bucket is empty now: %d %s", rec.Code, rec.Body)
	}
}

// A bucket without versioning behaves exactly as it did, which is what
// keeps every existing example and test true.
func TestAnUnversionedBucketKeepsOneGeneration(t *testing.T) {
	s := newTestServer(t)
	makeVersionedBucket(t, s, "plain", false)

	first := putObjectBytes(t, s, "plain", "a.txt", "one")
	putObjectBytes(t, s, "plain", "a.txt", "two")

	if vs := listVersions(t, s, "plain"); len(vs) != 1 {
		t.Errorf("an overwrite in an unversioned bucket left %d generations, want 1", len(vs))
	}
	if rec, _ := call(t, s, http.MethodGet,
		"/storage/v1/b/plain/o/a.txt?alt=media&generation="+first, ""); rec.Code != 404 {
		t.Errorf("the replaced generation is still readable: %d %s", rec.Code, rec.Body)
	}
	// A delete empties the bucket outright.
	call(t, s, http.MethodDelete, "/storage/v1/b/plain/o/a.txt", "")
	if rec, _ := call(t, s, http.MethodDelete, "/storage/v1/b/plain", ""); rec.Code != 204 {
		t.Errorf("the bucket should be empty: %d %s", rec.Code, rec.Body)
	}
}

// A generation number must never be REUSED, even after the newest
// generation is deleted -- a client uses it for if-generation-match, so a
// reused number would make two different objects compare equal.
func TestAGenerationNumberIsNeverReused(t *testing.T) {
	s := newTestServer(t)
	makeVersionedBucket(t, s, "vers", true)

	first := putObjectBytes(t, s, "vers", "a.txt", "one")
	second := putObjectBytes(t, s, "vers", "a.txt", "two")
	call(t, s, http.MethodDelete, "/storage/v1/b/vers/o/a.txt?generation="+second, "")

	third := putObjectBytes(t, s, "vers", "a.txt", "three")
	if third == second || third == first {
		t.Errorf("the write after deleting generation %s reused a number: %s", second, third)
	}
}

// A generation HISTORY survives a restart, in order, with the archived
// ones still archived -- and the generation counter comes back with it, so
// a write after the restart cannot reuse a number.
func TestAGenerationHistorySurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t)

	before := newObjectStore(dir)
	first, _ := before.put(s, "vers", "a.txt", []byte("one"), nil, true)
	second, _ := before.put(s, "vers", "a.txt", []byte("two"), nil, true)
	before.delete("vers", "a.txt", true)
	// An object with one generation, to prove the ordinary case still
	// writes to the unsuffixed path and loads from it.
	before.put(s, "vers", "b.txt", []byte("only"), nil, true)

	after := newObjectStore(dir)
	after.load()

	gens := after.generations("vers")
	if len(gens) != 3 {
		t.Fatalf("after the restart: %d generations, want 3", len(gens))
	}
	if str(gens[0]["generation"]) != str(first["generation"]) ||
		str(gens[1]["generation"]) != str(second["generation"]) {
		t.Errorf("a.txt's generations came back as %s then %s, want %s then %s",
			str(gens[0]["generation"]), str(gens[1]["generation"]),
			str(first["generation"]), str(second["generation"]))
	}
	for i := range 2 {
		if _, archived := gens[i]["timeDeleted"]; !archived {
			t.Errorf("generation %s came back live, want archived", str(gens[i]["generation"]))
		}
	}
	// Nothing of a.txt is live, which is what the delete meant.
	if _, _, ok := after.get("vers", "a.txt"); ok {
		t.Error("a deleted object came back live after the restart")
	}
	// The ARCHIVED bytes, which is the thing that simply was not stored
	// before.
	data, _, ok := after.getGeneration("vers", "a.txt", str(first["generation"]))
	if !ok || string(data) != "one" {
		t.Errorf("the oldest generation reads %q (found=%v), want \"one\"", data, ok)
	}
	if data, _, _ := after.get("vers", "b.txt"); string(data) != "only" {
		t.Errorf("the single-generation object reads %q", data)
	}

	// And the counter survived: a write after the restart issues a number
	// higher than anything on disk.
	third, _ := after.put(s, "vers", "a.txt", []byte("three"), nil, true)
	newest, _ := strconv.ParseInt(str(third["generation"]), 10, 64)
	previous, _ := strconv.ParseInt(str(second["generation"]), 10, 64)
	if newest <= previous {
		t.Errorf("the write after the restart issued generation %d, which is not above %d",
			newest, previous)
	}
}
