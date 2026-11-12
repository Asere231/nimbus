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
		before.put("bucket", o.key, []byte(o.content), objectInfo{ContentType: "text/plain"})
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
