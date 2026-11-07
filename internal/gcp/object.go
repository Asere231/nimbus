package gcp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
)

// Objects, and the three ways a client uploads one.
//
// GCS has one download path and three upload protocols, and a client picks
// between them by size and by whether it is streaming:
//
//   - `uploadType=media`: the body IS the bytes, the name is a query
//     parameter. The simplest, and what a small `gcloud storage cp` uses.
//   - `uploadType=multipart`: a multipart/related body whose first part is
//     the object's JSON metadata and whose second is the bytes. This is
//     what the Terraform provider sends.
//   - `uploadType=resumable`: a POST with the metadata that answers with a
//     session URI in `Location`, then one or more PUTs to that URI carrying
//     `Content-Range`. Each incomplete PUT answers **308** with a `Range`
//     header saying how much arrived. Anything large goes this way.
//
// All three end in the same place: bytes in the object store, one metadata
// document, and `md5Hash` and `crc32c` computed from the content. Those two
// are DIGESTS, not settings -- unlike Azure's Content-MD5, which must never
// be invented, GCS always reports them and a client compares them to decide
// whether an upload needs repeating.

func (s *Server) serveObject(w http.ResponseWriter, r *http.Request, bucket, object string) string {
	if _, aerr := s.loadBucket(bucket); aerr != nil {
		writeError(w, aerr)
		return "storage object: no bucket"
	}
	q := r.URL.Query()

	switch r.Method {
	case http.MethodGet:
		if object == "" {
			return s.listObjects(w, r, bucket)
		}
		// One path, two answers: `alt=media` returns the BYTES, anything
		// else returns the metadata. A client that wanted metadata and got
		// bytes fails to parse them with no clue why.
		if q.Get("alt") == "media" {
			return s.downloadObject(w, bucket, object)
		}
		return s.getObjectMeta(w, bucket, object)

	case http.MethodPost:
		if object != "" {
			writeError(w, badRequest("POST to an object name is not an upload"))
			return "storage object: bad post"
		}
		return s.uploadObject(w, r, bucket)

	case http.MethodPut:
		// A PUT lands here only as a resumable session's data leg.
		return s.resumableChunk(w, r, bucket)

	case http.MethodPatch:
		return s.patchObject(w, r, bucket, object)

	case http.MethodDelete:
		// Deleting an object that is not there SUCCEEDS, which is what
		// makes a destroy idempotent -- the bucket is the thing that
		// refuses, not the object.
		s.objects.delete(bucket, object)
		w.WriteHeader(http.StatusNoContent)
		return ""
	}
	writeError(w, notFound("Nimbus does not implement %s on an object yet", r.Method))
	return "storage object " + r.Method
}

// uploadObject handles POST .../o, which is all three upload types.
func (s *Server) uploadObject(w http.ResponseWriter, r *http.Request, bucket string) string {
	q := r.URL.Query()
	switch q.Get("uploadType") {
	case "resumable":
		return s.startResumable(w, r, bucket)
	case "multipart":
		return s.uploadMultipart(w, r, bucket)
	default:
		// `media`, and an upload with no uploadType at all, which some
		// clients send.
		name := q.Get("name")
		if name == "" {
			writeError(w, badRequest("Required parameter 'name' not specified"))
			return "storage upload: no name"
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, maxObject))
		if err != nil {
			writeError(w, badRequest("%v", err))
			return "storage upload: unreadable"
		}
		meta := map[string]any{"contentType": r.Header.Get("Content-Type")}
		return s.storeObject(w, bucket, name, data, meta)
	}
}

// uploadMultipart reads multipart/related: metadata first, then bytes.
func (s *Server) uploadMultipart(w http.ResponseWriter, r *http.Request, bucket string) string {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || params["boundary"] == "" {
		writeError(w, badRequest("a multipart upload needs a boundary: %v", err))
		return "storage upload: no boundary"
	}
	mr := multipart.NewReader(io.LimitReader(r.Body, maxObject), params["boundary"])

	meta := map[string]any{}
	var data []byte
	for i := 0; ; i++ {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeError(w, badRequest("reading the upload: %v", err))
			return "storage upload: bad multipart"
		}
		body, err := io.ReadAll(part)
		part.Close()
		if err != nil {
			writeError(w, badRequest("reading the upload: %v", err))
			return "storage upload: bad part"
		}
		// The FIRST part is the metadata and the second is the content --
		// by order, not by name: the parts of a multipart/related upload
		// carry no names at all.
		if i == 0 {
			_ = json.Unmarshal(body, &meta)
			continue
		}
		data = body
		if ct := part.Header.Get("Content-Type"); ct != "" && meta["contentType"] == nil {
			meta["contentType"] = ct
		}
	}
	name := str(meta["name"])
	if name == "" {
		name = r.URL.Query().Get("name")
	}
	if name == "" {
		writeError(w, badRequest("Required parameter 'name' not specified"))
		return "storage upload: no name"
	}
	return s.storeObject(w, bucket, name, data, meta)
}

// startResumable answers with the session URI a client then PUTs to.
//
// The URI goes in the `Location` header and carries an `upload_id`; the
// client treats it as opaque, which is what lets Nimbus point it back at
// its own port.
func (s *Server) startResumable(w http.ResponseWriter, r *http.Request, bucket string) string {
	var meta map[string]any
	if err := readJSON(r, &meta); err != nil {
		writeError(w, badRequest("%v", err))
		return "storage resumable: bad metadata"
	}
	if meta == nil {
		meta = map[string]any{}
	}
	name := str(meta["name"])
	if name == "" {
		name = r.URL.Query().Get("name")
	}
	if name == "" {
		writeError(w, badRequest("Required parameter 'name' not specified"))
		return "storage resumable: no name"
	}
	if ct := r.Header.Get("X-Upload-Content-Type"); ct != "" && meta["contentType"] == nil {
		meta["contentType"] = ct
	}
	id := s.objects.newUpload(bucket, name, meta)
	w.Header().Set("Location", fmt.Sprintf("%s/upload/storage/v1/b/%s/o?uploadType=resumable&upload_id=%s",
		s.base(), bucket, id))
	writeJSON(w, http.StatusOK, map[string]any{})
	return ""
}

// resumableChunk takes one PUT of a session.
//
// An incomplete chunk answers **308 Resume Incomplete** with a `Range`
// header of the bytes held so far; the final one answers 200 with the
// object. A client that is told 200 too early stops sending and the object
// is silently truncated.
func (s *Server) resumableChunk(w http.ResponseWriter, r *http.Request, bucket string) string {
	id := r.URL.Query().Get("upload_id")
	up := s.objects.upload(id)
	if up == nil {
		writeError(w, notFound("No such upload session: %s", id))
		return "storage resumable: no session"
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxObject))
	if err != nil {
		writeError(w, badRequest("%v", err))
		return "storage resumable: unreadable"
	}

	total, complete := parseContentRange(r.Header.Get("Content-Range"), len(data))
	up.data = append(up.data, data...)
	if !complete && total >= 0 && int64(len(up.data)) < total {
		// 308, and the Range header is INCLUSIVE of both ends: bytes 0-99
		// means a hundred bytes.
		w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(up.data)-1))
		w.Header().Set("X-Range-MD5", "")
		w.WriteHeader(308)
		return ""
	}
	s.objects.finishUpload(id)
	return s.storeObject(w, bucket, up.name, up.data, up.meta)
}

// parseContentRange reads `bytes {start}-{end}/{total}`, where the total
// may be `*` while the client still does not know it.
func parseContentRange(header string, chunk int) (total int64, complete bool) {
	if header == "" {
		// No Content-Range at all means the whole object in one PUT.
		return int64(chunk), true
	}
	spec := strings.TrimPrefix(header, "bytes ")
	_, size, ok := strings.Cut(spec, "/")
	if !ok || size == "*" {
		return -1, false
	}
	n, err := strconv.ParseInt(size, 10, 64)
	if err != nil {
		return -1, false
	}
	return n, false
}

// storeObject is where all three upload paths end.
func (s *Server) storeObject(w http.ResponseWriter, bucket, name string, data []byte, meta map[string]any) string {
	doc, aerr := s.objects.put(s, bucket, name, data, meta)
	if aerr != nil {
		writeError(w, aerr)
		return "storage upload: " + aerr.Message
	}
	writeJSON(w, http.StatusOK, doc)
	return ""
}

func (s *Server) getObjectMeta(w http.ResponseWriter, bucket, name string) string {
	doc, ok := s.objects.meta(bucket, name)
	if !ok {
		writeError(w, errorf(http.StatusNotFound, "notFound", "NOT_FOUND",
			"No such object: %s/%s", bucket, name))
		return "storage object: not found"
	}
	writeJSON(w, http.StatusOK, doc)
	return ""
}

func (s *Server) downloadObject(w http.ResponseWriter, bucket, name string) string {
	data, doc, ok := s.objects.get(bucket, name)
	if !ok {
		writeError(w, errorf(http.StatusNotFound, "notFound", "NOT_FOUND",
			"No such object: %s/%s", bucket, name))
		return "storage object: not found"
	}
	ct := str(doc["contentType"])
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("ETag", str(doc["etag"]))
	// The hashes travel on the download too, in the header a client checks
	// against what it read.
	w.Header().Set("X-Goog-Hash", "crc32c="+str(doc["crc32c"])+",md5="+str(doc["md5Hash"]))
	w.Header().Set("X-Goog-Generation", str(doc["generation"]))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	return ""
}

func (s *Server) listObjects(w http.ResponseWriter, r *http.Request, bucket string) string {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")

	var items []map[string]any
	prefixes := map[string]bool{}
	for _, name := range sortedNames(s.objects.names(bucket)) {
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		// A delimiter turns a flat key space into a listing with folders:
		// everything up to the next delimiter after the prefix collapses
		// into prefixes[]. An object name is not a path, but this is how a
		// client draws one.
		if delimiter != "" {
			rest := strings.TrimPrefix(name, prefix)
			if i := strings.Index(rest, delimiter); i >= 0 {
				prefixes[prefix+rest[:i+len(delimiter)]] = true
				continue
			}
		}
		if doc, ok := s.objects.meta(bucket, name); ok {
			items = append(items, doc)
		}
	}
	out := map[string]any{"kind": "storage#objects"}
	if len(items) > 0 {
		out["items"] = items
	}
	if len(prefixes) > 0 {
		var list []string
		for p := range prefixes {
			list = append(list, p)
		}
		out["prefixes"] = sortedNames(list)
	}
	writeJSON(w, http.StatusOK, out)
	return ""
}

func (s *Server) patchObject(w http.ResponseWriter, r *http.Request, bucket, name string) string {
	var patch map[string]any
	if err := readJSON(r, &patch); err != nil {
		writeError(w, badRequest("%v", err))
		return "storage object patch: bad body"
	}
	doc, ok := s.objects.patchMeta(bucket, name, patch)
	if !ok {
		writeError(w, errorf(http.StatusNotFound, "notFound", "NOT_FOUND",
			"No such object: %s/%s", bucket, name))
		return "storage object patch: not found"
	}
	writeJSON(w, http.StatusOK, doc)
	return ""
}

// base64MD5 is how GCS reports md5Hash: base64 of the raw digest, not hex.
func base64MD5(sum []byte) string { return base64.StdEncoding.EncodeToString(sum) }
