package gcp

import (
	"net/http"
	"strings"
)

// A GCP response is full of absolute URLs, and clients treat them in two
// very different ways. Some are IDENTITY -- a resource's self link is its
// id, in the store and in Terraform state -- and must never change. Others
// are instructions to GO SOMEWHERE, and a client follows them verbatim.
//
// The second kind cannot be built from a fixed address. Measured from a
// Kubernetes pod with the endpoint override set correctly:
// google-cloud-storage UPLOADED fine, because it composes that URL itself,
// and then the DOWNLOAD failed, because it followed
//
//	"mediaLink": "http://localhost:4703/storage/v1/b/{b}/o/{o}?alt=media"
//
// and inside a container localhost is the container:
//
//	HTTPConnectionPool(host='localhost', port=4703): Connection refused
//
// So a followed link is rebuilt from the host the request actually arrived
// on. A client that reached Nimbus on host.docker.internal is told to come
// back there; Terraform, which always uses localhost, keeps reading
// localhost and sees no diff -- which matters, because media_link and
// self_link are computed attributes it stores.
//
// What is deliberately NOT rewritten is anything a resource is identified
// by: Compute self links are store ids, so a per-request value would make
// the same resource two resources. An object is safe because its id is
// "bucket/name/generation", not a URL.

// requestBase is the origin a client reached this server on.
//
// Falls back to the configured PublicURL when the request carries no Host,
// which HTTP/1.0 allows. The scheme is taken from the listener rather than
// the request because GCP is served over plain HTTP here -- there is no
// bucket hostname to certify, which is the whole reason it needs no TLS.
func (s *Server) requestBase(r *http.Request) string {
	if r == nil || r.Host == "" {
		return s.base()
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// relinked copies doc with every FOLLOWED link rebuilt for this request.
//
// A copy, not a mutation: the document belongs to the object store and is
// handed out by reference, so rewriting in place would persist one
// client's hostname for every later reader -- the "a registry hands out
// copies" rule this project has already found five races under.
func (s *Server) relinked(r *http.Request, doc map[string]any) map[string]any {
	if doc == nil {
		return nil
	}
	base := s.requestBase(r)
	stored := s.base()
	if base == stored {
		return doc
	}
	out := make(map[string]any, len(doc))
	for k, v := range doc {
		out[k] = v
	}
	for _, field := range []string{"selfLink", "mediaLink"} {
		if link, ok := out[field].(string); ok && strings.HasPrefix(link, stored) {
			out[field] = base + strings.TrimPrefix(link, stored)
		}
	}
	return out
}

// relinkedAll is relinked over a listing.
func (s *Server) relinkedAll(r *http.Request, docs []map[string]any) []map[string]any {
	if len(docs) == 0 {
		return docs
	}
	out := make([]map[string]any, 0, len(docs))
	for _, d := range docs {
		out = append(out, s.relinked(r, d))
	}
	return out
}
