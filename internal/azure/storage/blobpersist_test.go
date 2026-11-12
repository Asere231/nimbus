package storage

import (
	"testing"
)

// Every blob must come back after a restart, whatever it is called.
//
// The blob store fails DIFFERENTLY from S3's object store on the same
// filesystem, which is why this test exists beside the one in internal/aws
// rather than instead of it. S3 walks the directory and rebuilds each key
// from the path it finds; this store reads a manifest that already holds the
// real name. So where a case collision made S3 lose a key and misreport
// another, here it made both names resolve to the one surviving file:
// measured on Windows, "Notes.txt" and "notes.txt" both came back with the
// lower-case blob's content, which is a silent corruption rather than a
// silent loss, and arguably the worse of the two.
//
// The illegal-character names failed the write, which save() swallowed with
// a bare `continue`, so the blob served from memory until the restart.
func TestBlobsSurviveARestartWhateverTheyAreCalled(t *testing.T) {
	dir := t.TempDir()

	blobs := []struct{ name, content string }{
		{"Notes.txt", "UPPER"},
		{"notes.txt", "lower"},
		{"NOTES.TXT", "SHOUTING"},
		{"x:y", "colon"},
		{"star*", "star"},
		{"q?x", "question"},
		{"trail.", "trailing dot"},
		{"con", "device name"},
		{"img/logo.svg", "<svg/>"},
		{"readme.txt", "ordinary"},
	}

	s1 := NewStore()
	if err := s1.Persist(dir); err != nil {
		t.Fatal(err)
	}
	a := s1.CreateAccount("stnimbus")
	c, ok := a.CreateContainer("uploads", nil, "")
	if !ok {
		t.Fatal("could not create the container")
	}
	for _, b := range blobs {
		c.PutBlob(&Blob{Name: b.name, Content: []byte(b.content), ContentType: "text/plain"})
	}
	s1.Save(a)

	// A completely fresh store over the same directory: this is a restart.
	s2 := NewStore()
	if err := s2.Persist(dir); err != nil {
		t.Fatal(err)
	}
	got := s2.Account("stnimbus")
	if got == nil {
		t.Fatal("the account did not survive the restart")
	}
	restored := got.Container("uploads")
	if restored == nil {
		t.Fatal("the container did not survive the restart")
	}

	for _, want := range blobs {
		b := restored.Blob(want.name)
		if b == nil {
			t.Errorf("blob %q did not survive the restart", want.name)
			continue
		}
		if string(b.Content) != want.content {
			t.Errorf("blob %q came back with %q, want %q", want.name, b.Content, want.content)
		}
	}

	if n := len(restored.Blobs("")); n != len(blobs) {
		t.Errorf("container holds %d blobs after the restart, want %d", n, len(blobs))
	}
}
