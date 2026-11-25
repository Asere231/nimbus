package aws

import (
	"testing"
)

// Everything put into a persisted bucket must come back after a restart,
// whatever it is called.
//
// The fsname package proves its encoding round-trips; this proves the store
// USES it, which is a different claim and the one that was false. Each name
// below was measured failing on Windows before internal/fsname existed, and
// they fail in three distinct ways, so a single "it works now" would not have
// covered them:
//
//   - "Readme.txt" and "readme.txt" are two keys that NTFS and APFS merged
//     into one file. After a restart the listing advertised "readme.txt",
//     which 404'd, while "Readme.txt" worked and was not listed -- because
//     the map key came from the path and the reported key came from the
//     metadata beside it, and the two disagreed.
//   - "a:b" opened an NTFS alternate data stream, so the bytes went into a
//     hidden stream of a zero-length file and the object came back empty
//     under the truncated name "a".
//   - "q?x" failed the write outright and writeThrough swallowed the error,
//     so the object served from memory until the restart and then was gone.
//
// "config.json" is here for a bug that was never about any platform: load
// skipped every path ending in ".json", which is the object's own file
// whenever the key ends that way, so it vanished on every OS.
func TestObjectsSurviveARestartWhateverTheyAreCalled(t *testing.T) {
	dir := t.TempDir()

	objects := []struct{ key, content string }{
		{"Readme.txt", "UPPER"},
		{"readme.txt", "lower"},
		{"README.TXT", "SHOUTING"},
		{"a:b", "colon"},
		{"q?x", "question"},
		{"star*x", "star"},
		{"pipe|x", "pipe"},
		{"trail.", "trailing dot"},
		{"con", "device name"},
		{"config.json", `{"a":1}`},
		{"img/logo.svg", "<svg/>"},
		{"normal.txt", "ordinary"},
	}

	before := newObjectStore(dir)
	for _, o := range objects {
		before.put("bucket", o.key, []byte(o.content), objectInfo{ContentType: "text/plain"}, false)
	}

	// A completely fresh store over the same directory: this is a restart.
	after := newObjectStore(dir)
	after.load()

	for _, o := range objects {
		data, info, ok := after.get("bucket", o.key)
		if !ok {
			t.Errorf("key %q did not survive the restart", o.key)
			continue
		}
		if string(data) != o.content {
			t.Errorf("key %q came back with %q, want %q", o.key, data, o.content)
		}
		// The reported key has to agree with the key it was found under, or
		// a listing advertises names that cannot be fetched -- which is
		// precisely what the case collision did.
		if info.Key != o.key {
			t.Errorf("key %q reports itself as %q", o.key, info.Key)
		}
		if info.ContentType != "text/plain" {
			t.Errorf("key %q lost its metadata: content type %q", o.key, info.ContentType)
		}
	}

	if got := after.count("bucket"); got != len(objects) {
		t.Errorf("bucket holds %d objects after the restart, want %d", got, len(objects))
	}

	// A listing must name every key exactly once, and name only keys that
	// can actually be fetched.
	listed := after.list("bucket", "")
	if len(listed) != len(objects) {
		t.Fatalf("listing returned %d objects, want %d", len(listed), len(objects))
	}
	for _, info := range listed {
		if _, _, ok := after.get("bucket", info.Key); !ok {
			t.Errorf("listing advertises key %q, which cannot be fetched", info.Key)
		}
	}
}

// A version HISTORY survives a restart, in order, delete markers included.
//
// The order is the part worth testing: it lives in the slice rather than
// being recovered from timestamps, and on disk it lives in the ".v{n}%"
// suffix -- so a restart has to put the versions back the way they were,
// not merely find them all.
func TestAnObjectsHistorySurvivesARestart(t *testing.T) {
	dir := t.TempDir()

	before := newObjectStore(dir)
	first := before.put("bucket", "a.txt", []byte("one"), objectInfo{ContentType: "text/plain"}, true)
	second := before.put("bucket", "a.txt", []byte("two"), objectInfo{ContentType: "text/plain"}, true)
	marker := before.putMarker("bucket", "a.txt", true)
	// An object with one version, to prove the ordinary case still writes
	// to the plain path and loads from it.
	before.put("bucket", "b.txt", []byte("only"), objectInfo{ContentType: "text/plain"}, true)

	after := newObjectStore(dir)
	after.load()

	got := after.versions("bucket", "")
	want := []string{marker.VersionID, second.VersionID, first.VersionID}
	if len(got) != 4 {
		t.Fatalf("after the restart: %d versions, want 4", len(got))
	}
	for i, id := range want {
		if got[i].Key != "a.txt" || got[i].VersionID != id {
			t.Errorf("version %d is %s/%s, want a.txt/%s", i, got[i].Key, got[i].VersionID, id)
		}
	}
	if !got[0].DeleteMarker {
		t.Error("the delete marker did not come back as a marker")
	}
	if got[1].DeleteMarker || got[2].DeleteMarker {
		t.Error("an ordinary version came back as a delete marker")
	}
	if got[3].Key != "b.txt" {
		t.Errorf("the single-version object is %q", got[3].Key)
	}

	// The bytes of a NONCURRENT version, which is the thing that was
	// simply not stored before.
	data, _, ok := after.getVersion("bucket", "a.txt", first.VersionID)
	if !ok || string(data) != "one" {
		t.Errorf("the oldest version reads %q (found=%v), want \"one\"", data, ok)
	}
	if _, _, ok := after.getVersion("bucket", "a.txt", second.VersionID); !ok {
		t.Error("the middle version did not survive")
	}
}

// Deleting a version in the MIDDLE renumbers the files on disk, and the
// history must still come back in order. This is why a whole key is
// rewritten on every change rather than appended to.
func TestDeletingAMiddleVersionKeepsTheRestInOrder(t *testing.T) {
	dir := t.TempDir()

	before := newObjectStore(dir)
	first := before.put("bucket", "a.txt", []byte("one"), objectInfo{}, true)
	second := before.put("bucket", "a.txt", []byte("two"), objectInfo{}, true)
	third := before.put("bucket", "a.txt", []byte("three"), objectInfo{}, true)
	if _, ok := before.deleteVersion("bucket", "a.txt", second.VersionID); !ok {
		t.Fatal("the middle version was not there to delete")
	}

	after := newObjectStore(dir)
	after.load()
	got := after.versions("bucket", "")
	if len(got) != 2 || got[0].VersionID != third.VersionID || got[1].VersionID != first.VersionID {
		t.Fatalf("after deleting the middle version and restarting: %v", got)
	}
	if data, _, _ := after.get("bucket", "a.txt"); string(data) != "three" {
		t.Errorf("the current version reads %q, want \"three\"", data)
	}
	if data, _, _ := after.getVersion("bucket", "a.txt", first.VersionID); string(data) != "one" {
		t.Errorf("the oldest version reads %q, want \"one\"", data)
	}
}
