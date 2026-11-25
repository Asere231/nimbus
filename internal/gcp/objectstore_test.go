package gcp

import (
	"testing"
)

// Objects survive a restart with their metadata intact.
//
// The Cloud Storage objectStore had no persistence test at all, which is
// the gap Milestone 19 exists to close. S3's equivalent and Azure's both
// gained one in Milestone 18 — after a reload lost every key ending in
// ".json" on every platform, and after a case-insensitive filesystem
// merged two keys into one file. Neither was found by a test.
//
// This store is the one that was NEVER affected by those, because it
// hashes a name into a filename rather than joining it onto a directory,
// and that is worth pinning rather than assuming: an object name is a flat
// key that may contain slashes, may differ from another only in case, and
// may contain characters a filesystem cannot hold.
//
// The metadata matters as much as the bytes. md5Hash and crc32c are
// digests GCS always reports, and a client compares them; generation is a
// decimal string a client unmarshals into a string field. A reload that
// dropped or re-typed any of them would be a permanent diff.
func TestObjectsSurviveARestartWithTheirMetadata(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t)

	names := []string{
		"readme.txt",
		"img/logo.svg",          // a slash is part of the flat key
		"Readme.txt",            // differs from the first only in case
		"weird:name*with?chars", // legal in GCS, illegal on NTFS
		"config.json",           // the suffix that broke S3's loader
	}

	before := newObjectStore(dir)
	for _, name := range names {
		if _, err := before.put(s, "nimbus-data", name,
			[]byte("contents of "+name), map[string]any{
				"contentType": "text/plain",
			}, false); err != nil {
			t.Fatalf("put %q: %v", name, err)
		}
	}

	// A completely fresh store over the same directory: this is a restart.
	after := newObjectStore(dir)
	after.load()

	if got := after.count("nimbus-data"); got != len(names) {
		t.Errorf("bucket holds %d objects after the restart, want %d", got, len(names))
	}

	for _, name := range names {
		data, meta, ok := after.get("nimbus-data", name)
		if !ok {
			t.Errorf("object %q did not survive the restart", name)
			continue
		}
		if want := "contents of " + name; string(data) != want {
			t.Errorf("object %q came back with %q, want %q", name, data, want)
		}
		// The name it reports must be the name it was stored under, or a
		// listing advertises objects that cannot be fetched.
		if meta["name"] != name {
			t.Errorf("object %q reports itself as %v", name, meta["name"])
		}
		for _, field := range []string{"md5Hash", "crc32c", "generation", "contentType"} {
			if meta[field] == nil || meta[field] == "" {
				t.Errorf("object %q lost %s across the restart", name, field)
			}
		}
	}
}
