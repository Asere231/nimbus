package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Everything a client put into the emulator must come back after a restart,
// including the account KEYS -- lose those and every client fails to
// authenticate against data that is otherwise intact.
func TestStorePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()

	s1 := NewStore()
	if err := s1.Persist(dir); err != nil {
		t.Fatal(err)
	}
	a := s1.CreateAccount("stnimbus")
	keys := a.Keys

	c, _ := a.CreateContainer("uploads", map[string]string{"owner": "nimbus"}, "blob")
	c.PutBlob(&Blob{Name: "dir/hello.txt", Content: []byte("hello nimbus"),
		ContentType: "text/plain", Metadata: map[string]string{"k": "v"}})
	q, _ := a.CreateQueue("jobs", nil)
	q.Put("job-1", time.Hour, 0)
	tbl, _ := a.CreateTable("people")
	tbl.Put(&Entity{PartitionKey: "eng", RowKey: "ada", Properties: map[string]any{"role": "cto"}})
	s1.Save(a)

	// A completely fresh store over the same directory: this is a restart.
	s2 := NewStore()
	if err := s2.Persist(dir); err != nil {
		t.Fatal(err)
	}
	got := s2.Account("stnimbus")
	if got == nil {
		t.Fatal("account did not survive the restart")
	}
	if got.Keys != keys {
		t.Error("account keys changed across a restart; every client would fail to authenticate")
	}

	gc := got.Container("uploads")
	if gc == nil {
		t.Fatal("container did not survive")
	}
	if gc.PublicAccess != "blob" || gc.Metadata["owner"] != "nimbus" {
		t.Errorf("container properties lost: access=%q meta=%v", gc.PublicAccess, gc.Metadata)
	}
	b := gc.Blob("dir/hello.txt")
	if b == nil {
		t.Fatal("blob did not survive")
	}
	if string(b.Content) != "hello nimbus" {
		t.Errorf("blob content = %q", b.Content)
	}
	if b.ContentType != "text/plain" || b.Metadata["k"] != "v" {
		t.Errorf("blob properties lost: type=%q meta=%v", b.ContentType, b.Metadata)
	}

	if gq := got.Queue("jobs"); gq == nil || gq.Len() != 1 {
		t.Error("queue or its messages did not survive")
	}
	gt := got.Table("people")
	if gt == nil || gt.Entity("eng", "ada") == nil {
		t.Fatal("table or entity did not survive")
	}
	if gt.Entity("eng", "ada").Properties["role"] != "cto" {
		t.Error("entity properties lost")
	}
}

// Blobs are stored as plain files, so you can look at what an emulated upload
// produced without a tool.
func TestBlobIsReadableOnDiskAsAPlainFile(t *testing.T) {
	dir := t.TempDir()
	s := NewStore()
	if err := s.Persist(dir); err != nil {
		t.Fatal(err)
	}
	a := s.CreateAccount("stnimbus")
	c, _ := a.CreateContainer("c", nil, "")
	c.PutBlob(&Blob{Name: "notes/readme.txt", Content: []byte("plain bytes")})
	s.Save(a)

	path := filepath.Join(dir, "stnimbus", "blobs", "c", "notes", "readme.txt")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("blob is not a plain file at %s: %v", path, err)
	}
	if string(data) != "plain bytes" {
		t.Errorf("on-disk content = %q", data)
	}
}

// Deleting an account must take its data with it, not leave bytes behind.
func TestDeletingAnAccountRemovesItsData(t *testing.T) {
	dir := t.TempDir()
	s := NewStore()
	if err := s.Persist(dir); err != nil {
		t.Fatal(err)
	}
	a := s.CreateAccount("stnimbus")
	c, _ := a.CreateContainer("c", nil, "")
	c.PutBlob(&Blob{Name: "f.txt", Content: []byte("x")})
	s.Save(a)

	s.DeleteAccount("stnimbus")
	if _, err := os.Stat(filepath.Join(dir, "stnimbus")); !os.IsNotExist(err) {
		t.Error("account directory survived deletion")
	}
	s2 := NewStore()
	_ = s2.Persist(dir)
	if s2.Account("stnimbus") != nil {
		t.Error("deleted account came back after a restart")
	}
}
