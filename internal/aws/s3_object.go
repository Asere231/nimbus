package aws

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// S3 objects: the bytes, and the listing that finds them.

const maxObject = 256 << 20 // 256 MB, far past anything a local emulator needs

func (s *Server) serveObject(w http.ResponseWriter, r *http.Request, bucketName, key string) *apiError {
	key, ok := cleanKey(key)
	if !ok {
		return &apiError{Status: 400, Code: "InvalidArgument",
			Message: "the key is not one this emulator will store", Resource: "/" + bucketName}
	}
	if _, aerr := s.loadBucket(bucketName); aerr != nil {
		return aerr
	}

	// Multipart upload: five more operations on an object's path, told
	// apart by query parameters rather than by verb. See s3_multipart.go.
	q := r.URL.Query()
	uploadID := q.Get("uploadId")
	switch {
	case r.Method == http.MethodPost && hasFlag(r, "uploads"):
		return s.createMultipartUpload(w, r, bucketName, key)
	case r.Method == http.MethodPut && uploadID != "" && q.Get("partNumber") != "":
		return s.uploadPart(w, r, bucketName, key, uploadID)
	case r.Method == http.MethodPost && uploadID != "":
		return s.completeMultipartUpload(w, r, bucketName, key, uploadID)
	case r.Method == http.MethodDelete && uploadID != "":
		return s.abortMultipartUpload(w, bucketName, key, uploadID)
	case r.Method == http.MethodGet && uploadID != "":
		return s.listParts(w, bucketName, key, uploadID)
	}

	switch r.Method {
	case http.MethodPut:
		return s.putObject(w, r, bucketName, key)
	case http.MethodGet:
		return s.getObject(w, r, bucketName, key, true)
	case http.MethodHead:
		return s.getObject(w, r, bucketName, key, false)
	case http.MethodDelete:
		if s.objects.delete(bucketName, key) {
			w.WriteHeader(204)
			return nil
		}
		// Deleting an object that is not there succeeds in S3, which is
		// what makes a destroy idempotent.
		w.WriteHeader(204)
		return nil
	}
	return errorf(405, "MethodNotAllowed", "%s on an object", r.Method)
}

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, bucketName, key string) *apiError {
	// readS3Body, not io.ReadAll: aws-cli v2 wraps a body in aws-chunked
	// framing, and storing that framing as content is how a 20 MB file
	// came back 162 bytes too long. See s3_chunked.go.
	data, err := readS3Body(r, maxObject)
	if err != nil {
		return errorf(400, "IncompleteBody", "reading the body: %v", err)
	}
	info := objectInfo{ContentType: r.Header.Get("Content-Type")}
	if info.ContentType == "" {
		info.ContentType = "binary/octet-stream"
	}
	info.Metadata = objectMetadataOf(r)
	stored := s.objects.put(bucketName, key, data, info)
	w.Header().Set("ETag", stored.ETag)
	w.WriteHeader(200)
	return nil
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, bucketName, key string, withBody bool) *apiError {
	data, info, ok := s.objects.get(bucketName, key)
	if !ok {
		return &apiError{Status: 404, Code: "NoSuchKey",
			Message: "The specified key does not exist.", Resource: "/" + bucketName + "/" + key}
	}
	w.Header().Set("Content-Type", info.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.Header().Set("ETag", info.ETag)
	w.Header().Set("Last-Modified", info.LastModified.Format(http.TimeFormat))
	w.Header().Set("Accept-Ranges", "bytes")
	for k, v := range info.Metadata {
		// Assigned directly rather than through Set, so the name stays
		// exactly as it is written: Go would canonicalise it otherwise and
		// a client doing a literal lookup would miss it.
		w.Header()["x-amz-meta-"+k] = []string{v}
	}
	if !withBody {
		// A HEAD carries the whole object's size and no body, ranged or
		// not: it answers "how big is this", which a range does not change.
		w.WriteHeader(200)
		return nil
	}

	// A ranged GET is how `aws s3 cp` DOWNLOADS a large object: several
	// concurrent requests, each written at its own offset. Ignoring the
	// header while advertising Accept-Ranges meant every one of them got
	// the whole object and the client assembled overlapping copies --
	// measured, a 20,000,000-byte file came back as 36,777,216 bytes, with
	// nothing erroring. Advertising the capability is the promise; this
	// keeps it.
	if spec := r.Header.Get("Range"); spec != "" {
		start, end, status := byteRange(spec, info.Size)
		switch status {
		case rangeUnsatisfiable:
			// The size is in the answer, which is how a client learns what
			// it should have asked for.
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", info.Size))
			w.Header().Del("Content-Length")
			return &apiError{Status: 416, Code: "InvalidRange",
				Message:  "The requested range is not satisfiable",
				Resource: "/" + bucketName + "/" + key}
		case rangeOK:
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, info.Size))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(206)
			_, _ = w.Write(data[start : end+1])
			return nil
		}
		// rangeIgnore: a header nothing could parse. RFC 9110 says to
		// ignore it and send the whole object, which is also what S3 does
		// -- refusing would fail a download over a header the client may
		// have sent for any reason.
	}

	w.WriteHeader(200)
	_, _ = w.Write(data)
	return nil
}

// How a Range header was read.
const (
	rangeIgnore = iota // unparseable: send the whole object
	rangeOK
	rangeUnsatisfiable
)

// byteRange reads a single-range `bytes=` header against an object of the
// given size, returning an inclusive start and end.
//
// Only one range is honoured. A multi-range request is answered with the
// whole object rather than a multipart/byteranges body: no client Nimbus
// serves sends one, and half-implementing it would be worse than not
// claiming it.
func byteRange(spec string, size int64) (start, end int64, status int) {
	value, ok := strings.CutPrefix(strings.TrimSpace(spec), "bytes=")
	if !ok || strings.Contains(value, ",") {
		return 0, 0, rangeIgnore
	}
	first, last, ok := strings.Cut(value, "-")
	if !ok {
		return 0, 0, rangeIgnore
	}

	switch {
	case first == "" && last == "":
		return 0, 0, rangeIgnore
	case first == "":
		// A SUFFIX range: the last N bytes, not a range starting at -N.
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, rangeIgnore
		}
		if n > size {
			n = size
		}
		start, end = size-n, size-1
	default:
		var err error
		if start, err = strconv.ParseInt(first, 10, 64); err != nil || start < 0 {
			return 0, 0, rangeIgnore
		}
		if last == "" {
			end = size - 1
		} else if end, err = strconv.ParseInt(last, 10, 64); err != nil {
			return 0, 0, rangeIgnore
		}
		// Asking past the end is CLAMPED, not refused -- a client that does
		// not know the size asks for more than there is on the last chunk.
		if end > size-1 {
			end = size - 1
		}
	}

	if size == 0 || start > size-1 || end < start {
		return 0, 0, rangeUnsatisfiable
	}
	return start, end, rangeOK
}

// --- listing --------------------------------------------------------------

type listObjectsV2Result struct {
	XMLName        xml.Name       `xml:"ListBucketResult"`
	Xmlns          string         `xml:"xmlns,attr"`
	Name           string         `xml:"Name"`
	Prefix         string         `xml:"Prefix"`
	Delimiter      string         `xml:"Delimiter,omitempty"`
	KeyCount       int            `xml:"KeyCount"`
	MaxKeys        int            `xml:"MaxKeys"`
	IsTruncated    bool           `xml:"IsTruncated"`
	Contents       []listedObject `xml:"Contents"`
	CommonPrefixes []commonPrefix `xml:"CommonPrefixes"`
}

type listedObject struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type commonPrefix struct {
	Prefix string `xml:"Prefix"`
}

// listObjects answers both ListObjects (v1) and ListObjectsV2. They differ
// in what the response is called and in how paging works; the body is the
// same shape otherwise, and nothing local needs paging.
func (s *Server) listObjects(w http.ResponseWriter, r *http.Request, bucketName string) *apiError {
	if _, aerr := s.loadBucket(bucketName); aerr != nil {
		return aerr
	}
	q := r.URL.Query()
	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")
	maxKeys := 1000
	if v, err := strconv.Atoi(q.Get("max-keys")); err == nil && v > 0 {
		maxKeys = v
	}

	doc := listObjectsV2Result{
		Xmlns: s3Namespace, Name: bucketName, Prefix: prefix,
		Delimiter: delimiter, MaxKeys: maxKeys,
		Contents: []listedObject{}, CommonPrefixes: []commonPrefix{},
	}
	seenPrefix := map[string]bool{}
	for _, info := range s.objects.list(bucketName, prefix) {
		// A delimiter rolls everything below it into a common prefix,
		// which is how a client shows "folders" for a store that has none.
		if delimiter != "" {
			rest := strings.TrimPrefix(info.Key, prefix)
			if i := strings.Index(rest, delimiter); i >= 0 {
				p := prefix + rest[:i+len(delimiter)]
				if !seenPrefix[p] {
					seenPrefix[p] = true
					doc.CommonPrefixes = append(doc.CommonPrefixes, commonPrefix{Prefix: p})
				}
				continue
			}
		}
		if len(doc.Contents) >= maxKeys {
			doc.IsTruncated = true
			break
		}
		doc.Contents = append(doc.Contents, listedObject{
			Key:          info.Key,
			LastModified: info.LastModified.Format(time.RFC3339),
			ETag:         info.ETag,
			Size:         info.Size,
			StorageClass: "STANDARD",
		})
	}
	doc.KeyCount = len(doc.Contents)
	writeXML(w, 200, doc)
	return nil
}

// deleteObjects is the bulk delete a `terraform destroy` with
// force_destroy uses, and `aws s3 rm --recursive` after it.
type deleteObjectsResult struct {
	XMLName xml.Name `xml:"DeleteResult"`
	Xmlns   string   `xml:"xmlns,attr"`
	Deleted []struct {
		Key string `xml:"Key"`
	} `xml:"Deleted"`
}

func (s *Server) deleteObjects(w http.ResponseWriter, r *http.Request, b *bucket) *apiError {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return errorf(400, "MalformedXML", "reading the request: %v", err)
	}
	var req struct {
		Objects []struct {
			Key string `xml:"Key"`
		} `xml:"Object"`
	}
	if err := xml.Unmarshal(body, &req); err != nil {
		return errorf(400, "MalformedXML", "parsing the request: %v", err)
	}
	doc := deleteObjectsResult{Xmlns: s3Namespace}
	for _, o := range req.Objects {
		s.objects.delete(b.Name, o.Key)
		doc.Deleted = append(doc.Deleted, struct {
			Key string `xml:"Key"`
		}{o.Key})
	}
	writeXML(w, 200, doc)
	return nil
}

// objectMetadataOf reads the x-amz-meta-* headers.
//
// The part after the prefix is the key a client will look for, and it is
// lower-cased deliberately: Go canonicalises header names on BOTH sides,
// so a client doing a literal lookup of "owner" would miss "Owner". The
// same trap Azure's blob metadata had in Milestone 2.
func objectMetadataOf(r *http.Request) map[string]string {
	out := map[string]string{}
	for name, values := range r.Header {
		if lower := strings.ToLower(name); strings.HasPrefix(lower, "x-amz-meta-") && len(values) > 0 {
			out[strings.TrimPrefix(lower, "x-amz-meta-")] = values[0]
		}
	}
	return out
}

// hasFlag reports whether a valueless query parameter is present. S3's
// sub-resources carry no value, so Query().Get returns "" for all of them
// and the raw query has to be inspected -- the same reason subResource
// exists.
func hasFlag(r *http.Request, name string) bool {
	_, ok := r.URL.Query()[name]
	return ok
}
