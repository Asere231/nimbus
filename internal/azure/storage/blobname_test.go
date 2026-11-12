package storage

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A blob name is a NAME, not a path.
//
// persist.go decides where a blob's bytes go with
// filepath.Join(acctDir, "blobs", container, name), so until this check
// existed a name carrying ".." escaped its container. Measured on Windows
// against an account `terraform apply` had just created: a blob PUT as
// "../../nimbus-traversal-probe.txt" was written two levels up, into the
// account directory, landing beside the account.json manifest that a blob of
// that name would have overwritten -- and the container went on listing it
// under its original name as though nothing odd had happened.
//
// S3's cleanKey has refused exactly this since Milestone 8. The blob store
// never learned it, and nothing on a case-sensitive Linux CI runner was ever
// going to say so.
//
// The name is judged RAW and slash-separated rather than handed to filepath,
// which is the shape the three platform bugs before this one were all fixed
// into: a backslash is a separator on Windows only, so `a\..\..\x` is one
// harmless segment to a "/"-split check and three levels up to filepath.Join
// there; and "C:/x" is an ordinary name on Unix and absolute on Windows.
// Judging the name itself refuses the same request on every machine.
func TestABlobNameCannotEscapeItsContainer(t *testing.T) {
	refused := []struct{ name, why string }{
		{"../../account.json", "climbs out of the container and overwrites the manifest"},
		{"../sibling.txt", "climbs one level"},
		{"a/../../../etc/passwd", "climbs out from inside a legitimate-looking prefix"},
		{"/absolute.txt", "is absolute, and filepath.IsAbs is false for it on Windows"},
		{`a\..\..\x`, "is one segment to a /-split check and three levels up on Windows"},
		{"C:/windows.txt", "is an ordinary name on Unix and absolute on Windows"},
		{"", "is empty"},
	}
	for _, c := range refused {
		if ok := blobNameIsSafe(c.name); ok {
			t.Errorf("blob name %q was allowed; it %s", c.name, c.why)
		}
	}

	// The refusal must stay narrow. These are ordinary names that clients
	// really send, and a check that caught any of them would be worse than
	// the bug: a dot is not a traversal, and a colon is how every timestamp
	// is spelled.
	allowed := []string{
		"readme.txt",
		"img/logo.svg",
		"deep/nested/path/to/file.bin",
		"2026-01-01T00:00:00Z.log",
		"a:b",
		"...leading-dots.txt",
		"file..name.txt",
		"..hidden",
		"name.",
	}
	for _, name := range allowed {
		if !blobNameIsSafe(name) {
			t.Errorf("ordinary blob name %q was refused", name)
		}
	}
}

// The check has to sit on the request path, not only in the helper, or it is
// a rule nothing enforces. serveBlob is the single entry point for every blob
// operation, which is where AWS's "one enforcement point" lesson says to put
// it: a check inside each operation is a silent grant whenever one forgets to
// ask.
func TestTheBlobHandlerRefusesATraversingName(t *testing.T) {
	dir := t.TempDir()
	store := NewStore()
	if err := store.Persist(dir); err != nil {
		t.Fatal(err)
	}
	a := store.CreateAccount("stnimbus")
	if _, ok := a.CreateContainer("uploads", nil, ""); !ok {
		t.Fatal("could not create the container")
	}
	h := NewHandler(store)

	req := httptest.NewRequest(http.MethodPut,
		"/uploads/../../nimbus-traversal-probe.txt", strings.NewReader("ESCAPED"))
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	rec := httptest.NewRecorder()
	h.serveBlob(rec, req, a)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("traversing PUT answered %d, want 400", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "InvalidResourceName") {
		t.Errorf("error body does not name InvalidResourceName: %s", body)
	}

	// The point of the whole exercise: nothing outside the container.
	escaped := filepath.Join(dir, "stnimbus", "nimbus-traversal-probe.txt")
	if _, err := os.Stat(escaped); !os.IsNotExist(err) {
		t.Errorf("a file escaped the container: %s", escaped)
	}
}
