package storage

import (
	"encoding/xml"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// blobOp handles everything addressed at "/{container}/{blob}".
func (h *Handler) blobOp(w http.ResponseWriter, r *http.Request, a *Account, containerName, blobName, comp string) string {
	c := a.Container(containerName)
	if c == nil {
		WriteError(w, 404, "ContainerNotFound", "The specified container does not exist.")
		return ""
	}

	switch r.Method {
	case http.MethodPut:
		switch comp {
		case "block":
			return h.putBlock(w, r, c, blobName)
		case "blocklist":
			return h.putBlockList(w, r, c, blobName)
		case "metadata":
			b := c.Blob(blobName)
			if b == nil {
				notFoundBlob(w)
				return ""
			}
			b.Metadata = metadataFrom(r.Header)
			c.PutBlob(b)
			writeBlobWriteResponse(w, b, 200)
			return ""
		case "properties":
			b := c.Blob(blobName)
			if b == nil {
				notFoundBlob(w)
				return ""
			}
			applyBlobHeaders(b, r.Header)
			c.PutBlob(b)
			writeBlobWriteResponse(w, b, 200)
			return ""
		case "":
			return h.putBlob(w, r, a, c, blobName)
		}

	case http.MethodGet, http.MethodHead:
		b := c.Blob(blobName)
		if b == nil {
			notFoundBlob(w)
			return ""
		}
		if comp == "blocklist" {
			writeXML(w, 200, struct {
				XMLName   xml.Name `xml:"BlockList"`
				Committed struct{} `xml:"CommittedBlocks"`
			}{})
			return ""
		}
		return h.getBlob(w, r, b)

	case http.MethodDelete:
		if !c.DeleteBlob(blobName) {
			notFoundBlob(w)
			return ""
		}
		h.changed(a.Name, containerName, "update")
		h.persist(a)
		setCommonHeaders(w)
		w.WriteHeader(202)
		return ""
	}

	WriteError(w, 400, "InvalidUri", "Unsupported blob operation.")
	return "blob: unsupported blob op " + r.Method + " comp=" + comp
}

func notFoundBlob(w http.ResponseWriter) {
	WriteError(w, 404, "BlobNotFound", "The specified blob does not exist.")
}

func (h *Handler) putBlob(w http.ResponseWriter, r *http.Request, a *Account, c *Container, name string) string {
	if t := r.Header.Get("x-ms-blob-type"); t != "" && t != "BlockBlob" {
		WriteError(w, 400, "UnsupportedBlobType", "Nimbus emulates block blobs only; got "+t+".")
		return "blob: unsupported blob type " + t
	}
	content, err := io.ReadAll(r.Body)
	if err != nil {
		WriteError(w, 400, "InvalidInput", "The request body could not be read.")
		return ""
	}
	b := &Blob{Name: name, Content: content, BlobType: "BlockBlob", Metadata: metadataFrom(r.Header)}
	applyBlobHeaders(b, r.Header)
	c.PutBlob(b)
	h.changed(a.Name, c.Name, "update")
	h.persist(a)
	writeBlobWriteResponse(w, b, 201)
	return ""
}

// applyBlobHeaders copies the x-ms-blob-* content headers a client can set.
func applyBlobHeaders(b *Blob, h http.Header) {
	if v := h.Get("x-ms-blob-content-type"); v != "" {
		b.ContentType = v
	} else if v := h.Get("Content-Type"); v != "" && b.ContentType == "" {
		b.ContentType = v
	}
	if v := h.Get("x-ms-blob-cache-control"); v != "" {
		b.CacheControl = v
	}
	if v := h.Get("x-ms-blob-content-encoding"); v != "" {
		b.Encoding = v
	}
	if v := h.Get("x-ms-blob-content-language"); v != "" {
		b.Language = v
	}
	if v := h.Get("x-ms-blob-content-disposition"); v != "" {
		b.Disposition = v
	}
	// Only the explicit content setting; a request's own Content-MD5 is an
	// integrity check on that upload, not a property of the blob.
	if v := h.Get("x-ms-blob-content-md5"); v != "" {
		b.ContentMD5 = v
	}
}

func writeBlobWriteResponse(w http.ResponseWriter, b *Blob, status int) {
	w.Header().Set("ETag", b.ETag)
	w.Header().Set("Last-Modified", httpTime(b.Modified))
	if md5 := b.MD5(); md5 != "" {
		w.Header().Set("Content-MD5", md5)
	}
	w.Header().Set("x-ms-request-server-encrypted", "true")
	setCommonHeaders(w)
	w.WriteHeader(status)
}

// getBlob serves content, honouring a single Range header. Clients use ranges
// for large downloads, and the SDK asks for one even on small blobs when it is
// resuming.
func (h *Handler) getBlob(w http.ResponseWriter, r *http.Request, b *Blob) string {
	content := b.Content
	status := 200

	if rng := r.Header.Get("x-ms-range"); rng != "" {
		r.Header.Set("Range", rng)
	}
	if rng := r.Header.Get("Range"); rng != "" {
		start, end, ok := parseRange(rng, int64(len(b.Content)))
		if !ok {
			w.Header().Set("Content-Range", "bytes */"+strconv.Itoa(len(b.Content)))
			WriteError(w, 416, "InvalidRange", "The range specified is invalid for the current size of the resource.")
			return ""
		}
		content = b.Content[start : end+1]
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+"/"+strconv.Itoa(len(b.Content)))
		status = 206
	}

	w.Header().Set("Content-Type", contentTypeOf(b))
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.Header().Set("ETag", b.ETag)
	w.Header().Set("Last-Modified", httpTime(b.Modified))
	w.Header().Set("x-ms-blob-type", b.BlobType)
	w.Header().Set("x-ms-lease-status", "unlocked")
	w.Header().Set("x-ms-lease-state", "available")
	w.Header().Set("x-ms-server-encrypted", "true")
	w.Header().Set("Accept-Ranges", "bytes")
	if md5 := b.MD5(); status == 200 && md5 != "" {
		// Content-MD5 on a partial response would describe the whole blob, so
		// it is only sent for a full one.
		w.Header().Set("Content-MD5", md5)
	}
	if b.CacheControl != "" {
		w.Header().Set("Cache-Control", b.CacheControl)
	}
	if b.Disposition != "" {
		w.Header().Set("Content-Disposition", b.Disposition)
	}
	setMetadata(w, b.Metadata)
	setCommonHeaders(w)
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(content)
	}
	return ""
}

// parseRange handles "bytes=start-end", "bytes=start-" and "bytes=-suffix".
func parseRange(h string, size int64) (start, end int64, ok bool) {
	if !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(h, "bytes=")
	if i := strings.Index(spec, ","); i >= 0 {
		spec = spec[:i] // only the first range is honoured
	}
	dash := strings.Index(spec, "-")
	if dash < 0 {
		return 0, 0, false
	}
	lo, hi := spec[:dash], spec[dash+1:]

	switch {
	case lo == "" && hi == "":
		return 0, 0, false
	case lo == "": // suffix: the last N bytes
		n, err := strconv.ParseInt(hi, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, size > 0
	default:
		start, err := strconv.ParseInt(lo, 10, 64)
		if err != nil || start < 0 || start >= size {
			return 0, 0, false
		}
		end = size - 1
		if hi != "" {
			if e, err := strconv.ParseInt(hi, 10, 64); err == nil && e < end {
				end = e
			}
		}
		return start, end, end >= start
	}
}

// --- block upload ----------------------------------------------------------

// putBlock stages one block. Large uploads arrive as many of these, then a
// Put Block List assembles them.
func (h *Handler) putBlock(w http.ResponseWriter, r *http.Request, c *Container, name string) string {
	id := r.URL.Query().Get("blockid")
	if id == "" {
		WriteError(w, 400, "InvalidQueryParameterValue", "blockid is required.")
		return ""
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		WriteError(w, 400, "InvalidInput", "The request body could not be read.")
		return ""
	}
	c.StageBlock(name, id, data)
	setCommonHeaders(w)
	w.WriteHeader(201)
	return ""
}

type blockListRequest struct {
	XMLName     xml.Name `xml:"BlockList"`
	Committed   []string `xml:"Committed"`
	Uncommitted []string `xml:"Uncommitted"`
	Latest      []string `xml:"Latest"`
}

func (h *Handler) putBlockList(w http.ResponseWriter, r *http.Request, c *Container, name string) string {
	var req blockListRequest
	if err := xml.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		WriteError(w, 400, "InvalidXmlDocument", "The block list could not be parsed.")
		return ""
	}
	// Order matters and is taken from the document; Latest is the common case.
	ids := append(append(append([]string{}, req.Committed...), req.Uncommitted...), req.Latest...)
	b, ok := c.CommitBlocks(name, ids)
	if !ok {
		WriteError(w, 400, "InvalidBlockList", "The specified block list is invalid.")
		return ""
	}
	applyBlobHeaders(b, r.Header)
	if m := metadataFrom(r.Header); len(m) > 0 {
		b.Metadata = m
	}
	writeBlobWriteResponse(w, b, 201)
	return ""
}
