package storage

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Metadata names must come back lower-cased and un-canonicalised.
//
// Clients compare them literally: the Python SDK strips "x-ms-meta-" off
// whatever it receives and uses the rest as a dictionary key, so a response
// carrying "X-Ms-Meta-Owner" yields {"Owner": ...} and a lookup of "owner"
// misses. Go canonicalises header names on BOTH sides -- inbound while parsing
// the request, and outbound on Header.Set -- so the read path lower-cases and
// the write path assigns into the map directly.
func TestMetadataNamesAreLowerCasedAndWrittenVerbatim(t *testing.T) {
	// Go has already canonicalised inbound names by the time a handler runs.
	h := http.Header{}
	h.Set("x-ms-meta-owner", "nimbus")
	h.Set("x-ms-meta-camelCase", "kept")

	meta := metadataFrom(h)
	if meta["owner"] != "nimbus" || meta["camelcase"] != "kept" {
		t.Errorf("metadata names were not lower-cased: %v", meta)
	}
	if _, bad := meta["Owner"]; bad {
		t.Error("a capitalised metadata name leaked through")
	}

	rec := httptest.NewRecorder()
	setMetadata(rec, meta)
	for _, want := range []string{"x-ms-meta-owner", "x-ms-meta-camelcase"} {
		if _, ok := rec.Header()[want]; !ok {
			t.Errorf("header %q was not written verbatim; got %v", want, rec.Header())
		}
	}
	if _, bad := rec.Header()["X-Ms-Meta-Owner"]; bad {
		t.Error("Header.Set canonicalisation crept back in")
	}
}
