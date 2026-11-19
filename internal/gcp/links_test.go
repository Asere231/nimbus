package gcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// callFrom replays a request as if it arrived on another host, which is
// what a client inside a container looks like from here.
func callFrom(t *testing.T, s *Server, host, method, path, body, ctype string) map[string]any {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "http://"+host+path, r)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("Authorization", "Bearer nimbus")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("%s %s from %s: status %d: %s", method, path, host, rec.Code, rec.Body)
	}
	var doc map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	}
	return doc
}

const containerHost = "host.docker.internal:4703"

func uploadTo(t *testing.T, s *Server, host, bucket, name, content string) map[string]any {
	t.Helper()
	return callFrom(t, s, host, http.MethodPost,
		"/upload/storage/v1/b/"+bucket+"/o?uploadType=media&name="+name,
		content, "text/plain")
}

// A FOLLOWED link has to name the host the request came in on.
//
// This is the one cross-cloud call that genuinely failed before it.
// Measured from a Kubernetes pod, with client_options(api_endpoint=...)
// set correctly: google-cloud-storage uploaded fine, because it composes
// that URL itself, and then the download followed
//
//	"mediaLink": "http://localhost:4703/storage/v1/b/{b}/o/{o}?alt=media"
//
// and failed, because inside a container localhost is the container:
//
//	HTTPConnectionPool(host='localhost', port=4703): Connection refused
//
// An endpoint override cannot help: the SDK is not composing this URL, it
// is obeying one Nimbus sent.
func TestAFollowedLinkNamesTheHostTheRequestCameFrom(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPost, "/storage/v1/b?project=nimbus-local", `{"name":"b1"}`)

	doc := uploadTo(t, s, containerHost, "b1", "o1.txt", "hello")

	media, _ := doc["mediaLink"].(string)
	if !strings.HasPrefix(media, "http://"+containerHost+"/") {
		t.Errorf("mediaLink = %q, want it on %s", media, containerHost)
	}
	// selfLink is a followed link too -- a client given one may GET it --
	// and an object's identity is "bucket/name/generation", not a URL, so
	// there is nothing here that rewriting it could break.
	self, _ := doc["selfLink"].(string)
	if !strings.HasPrefix(self, "http://"+containerHost+"/") {
		t.Errorf("selfLink = %q, want it on %s", self, containerHost)
	}
}

// Every path that hands back an object document has to do it, not just the
// upload: a client reads metadata, lists a bucket and patches an object,
// and a link that is right on one path and wrong on another is worse than
// one that is wrong everywhere, because it works until it does not.
func TestEveryObjectResponseRelinks(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPost, "/storage/v1/b?project=nimbus-local", `{"name":"b1"}`)
	uploadTo(t, s, "localhost:4703", "b1", "o1.txt", "hello")

	t.Run("get", func(t *testing.T) {
		doc := callFrom(t, s, containerHost, http.MethodGet, "/storage/v1/b/b1/o/o1.txt", "", "")
		wantHost(t, "get", doc)
	})

	t.Run("patch", func(t *testing.T) {
		doc := callFrom(t, s, containerHost, http.MethodPatch, "/storage/v1/b/b1/o/o1.txt",
			`{"contentType":"text/plain"}`, "application/json")
		wantHost(t, "patch", doc)
	})

	t.Run("list", func(t *testing.T) {
		doc := callFrom(t, s, containerHost, http.MethodGet, "/storage/v1/b/b1/o", "", "")
		items, _ := doc["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("list returned %d items, want 1", len(items))
		}
		first, _ := items[0].(map[string]any)
		wantHost(t, "list", first)
	})
}

func wantHost(t *testing.T, where string, doc map[string]any) {
	t.Helper()
	for _, field := range []string{"selfLink", "mediaLink"} {
		link, _ := doc[field].(string)
		if !strings.HasPrefix(link, "http://"+containerHost+"/") {
			t.Errorf("%s: %s = %q, want it on %s", where, field, link, containerHost)
		}
	}
}

// Terraform always reaches Nimbus on the configured address, so it must
// keep reading exactly what it read before. media_link and self_link are
// COMPUTED attributes it stores, and a value that differed between reads
// would be a permanent diff -- the rule that has cost this project a day
// per cloud.
func TestTheConfiguredHostIsUnchanged(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPost, "/storage/v1/b?project=nimbus-local", `{"name":"b1"}`)

	// NOTE: the package's own `must` helper cannot be used here.
	// httptest.NewRequest given a bare PATH sets Host to "example.com",
	// so a request made that way is not "the configured host" -- it is a
	// third one, and the relinking would rightly rewrite to it. The host
	// has to be named for this test to be about what it says.
	up := uploadTo(t, s, "localhost:4703", "b1", "o1.txt", "hello")
	got := callFrom(t, s, "localhost:4703", http.MethodGet, "/storage/v1/b/b1/o/o1.txt", "", "")

	for _, field := range []string{"selfLink", "mediaLink"} {
		if up[field] != got[field] {
			t.Errorf("%s differs between the upload and the read: %q then %q",
				field, up[field], got[field])
		}
		if link, _ := got[field].(string); !strings.HasPrefix(link, base+"/") {
			t.Errorf("%s = %q, want it on the configured %s", field, link, base)
		}
	}
}

// The stored document is not mutated, because the object store hands it
// out by reference: rewriting in place would persist one client's hostname
// for every later reader. This project has found five races of exactly
// that shape, and the rule it wrote down is that a registry of mutable
// things hands out COPIES.
func TestRelinkingDoesNotMutateTheStoredDocument(t *testing.T) {
	s := newTestServer(t)
	must(t, s, http.MethodPost, "/storage/v1/b?project=nimbus-local", `{"name":"b1"}`)
	uploadTo(t, s, "localhost:4703", "b1", "o1.txt", "hello")

	// One client arrives from a container...
	callFrom(t, s, containerHost, http.MethodGet, "/storage/v1/b/b1/o/o1.txt", "", "")
	// ...and the next, on the configured address, must be unaffected.
	got := callFrom(t, s, "localhost:4703", http.MethodGet, "/storage/v1/b/b1/o/o1.txt", "", "")

	if link, _ := got["mediaLink"].(string); !strings.HasPrefix(link, base+"/") {
		t.Errorf("mediaLink = %q after a container read it, want it back on %s", link, base)
	}
}

// A request with no Host at all -- which HTTP/1.0 allows -- falls back to
// the configured address rather than producing a link to nowhere.
func TestNoHostFallsBackToTheConfiguredAddress(t *testing.T) {
	s := newTestServer(t)
	if got := s.requestBase(nil); got != base {
		t.Errorf("requestBase(nil) = %q, want %q", got, base)
	}
	req := httptest.NewRequest(http.MethodGet, "/storage/v1/b/b1/o/o1.txt", nil)
	req.Host = ""
	if got := s.requestBase(req); got != base {
		t.Errorf("requestBase(no host) = %q, want %q", got, base)
	}
}
