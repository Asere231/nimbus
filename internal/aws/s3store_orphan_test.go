package aws

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A file in the data directory that Nimbus did not write is REPORTED.
//
// The file is still ignored, and that is right: refusing to start over a
// stray file nobody asked about would be far worse, and a data directory
// somebody has copied, edited or half-migrated is an ordinary thing to
// find. What was wrong is that the skip was silent.
//
// Measured in Milestone 19 against a real server: a junk file at the top
// level, a pre-fsname uppercase name and a name fsname.Decode refuses all
// left it starting normally with nothing said anywhere. So an object that is
// genuinely not being served reads exactly like one that was never uploaded.
//
// The uppercase directory here is NOT one of them, and that is the
// interesting part: a name written before internal/fsname existed decodes to
// itself, because Decode is a percent-decoder and a safe name has nothing to
// decode. So a pre-fsname store still loads, and must not be reported as
// something Nimbus is refusing to serve. Writing it off as an orphan was the
// first thing this test asserted, and it was wrong.
func TestAnUnrecognisedFileInTheDataDirectoryIsReported(t *testing.T) {
	root := t.TempDir()

	// One real object, so the test can tell "counted everything" from
	// "counted nothing".
	s := newObjectStore(root)
	s.put("demo", "notes/hello.txt", []byte("hi"), objectInfo{}, false)

	// A stray at the top level: no bucket directory, so it cannot name a
	// bucket and a key.
	if err := os.WriteFile(filepath.Join(s.dir, "junk.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A pre-fsname bucket directory, which still LOADS: see the note above.
	legacy := filepath.Join(s.dir, "OldBucket")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "thing"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A name Decode refuses outright: a truncated escape.
	if err := os.WriteFile(filepath.Join(s.dir, "demo", "a%2"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	out, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(out)
		log.SetFlags(flags)
	}()

	reloaded := newObjectStore(root)
	reloaded.load()

	said := buf.String()
	if said == "" {
		t.Fatalf("unrecognised files loaded in silence; nothing says the data "+
			"directory holds anything Nimbus is not serving (dir %s)", s.dir)
	}
	if !strings.Contains(said, "2 files") {
		t.Errorf("expected the two unreadable strays counted, got %q", said)
	}
	if !strings.Contains(said, s.dir) {
		t.Errorf("the report does not name the directory: %q", said)
	}

	// And the real object is still there, which is what makes the count a
	// count of strays rather than of everything.
	if _, _, ok := reloaded.get("demo", "notes/hello.txt"); !ok {
		t.Error("the object that WAS written by Nimbus did not load")
	}
	// The legacy name still serves, and is not what the line is about.
	if _, _, ok := reloaded.get("OldBucket", "thing"); !ok {
		t.Error("a pre-fsname object stopped loading; that would be a regression " +
			"for anyone upgrading, not an orphan")
	}
}

// A data directory holding only what Nimbus wrote says nothing at all.
//
// A diagnostic printed on every start is one people learn to scroll past --
// the same argument this repo already made about the activity feed crying
// wolf over an unimplemented action that was working.
func TestACleanDataDirectoryReportsNothing(t *testing.T) {
	root := t.TempDir()
	s := newObjectStore(root)
	// Keys that exercise the encoding: uppercase, a slash, a percent and a
	// dot-suffix that used to collide with the metadata sidecar.
	s.put("demo", "Notes/Hello World.txt", []byte("hi"), objectInfo{}, false)
	s.put("demo", "config.json", []byte("{}"), objectInfo{}, false)
	s.put("demo", "weird%name", []byte("x"), objectInfo{}, false)

	var buf bytes.Buffer
	out, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(out)
		log.SetFlags(flags)
	}()

	reloaded := newObjectStore(root)
	reloaded.load()

	if said := buf.String(); said != "" {
		t.Errorf("a clean reload should say nothing, got %q", said)
	}
	for _, key := range []string{"Notes/Hello World.txt", "config.json", "weird%name"} {
		if _, _, ok := reloaded.get("demo", key); !ok {
			t.Errorf("%q did not survive the reload", key)
		}
	}
}
