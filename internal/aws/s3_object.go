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
	b, aerr := s.loadBucket(bucketName)
	if aerr != nil {
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
		return s.putObject(w, r, b, key)
	case http.MethodGet:
		return s.getObject(w, r, b, key, true)
	case http.MethodHead:
		return s.getObject(w, r, b, key, false)
	case http.MethodDelete:
		return s.deleteObject(w, r, b, key)
	}
	return errorf(405, "MethodNotAllowed", "%s on an object", r.Method)
}

// versioned reports whether a write to this bucket should keep the version
// it replaces.
//
// Only "Enabled" does. "Suspended" is NOT the same as never having had
// versioning: the history already written stays, and new writes carry the
// id "null" and replace each other -- which is the whole purpose of
// suspending rather than there being no way back.
func (b *bucket) versioned() bool { return b.Versioning == "Enabled" }

// writeVersionID sets x-amz-version-id, which a client reads to learn
// which version it just made or read.
//
// Not sent for a bucket that has never had versioning configured: there
// the id is "null" and real S3 omits the header entirely, so sending it
// would tell a client its unversioned bucket has versions.
func writeVersionID(w http.ResponseWriter, b *bucket, versionID string) {
	if b.Versioning == "" && versionID == nullVersionID {
		return
	}
	if versionID != "" {
		w.Header().Set("x-amz-version-id", versionID)
	}
}

// deleteObject is a delete, which does three different things depending on
// what it is told.
//
//   - With a versionId, it removes that version PERMANENTLY. Removing a
//     delete marker is how an object comes back.
//   - Without one, against a bucket with versioning configured, it lays a
//     DELETE MARKER over the history: the object then reads 404 and every
//     earlier version is still there. This is the piece that makes a
//     bucket's history a history.
//   - Without one, against a bucket that has never had versioning, it
//     removes the object, as it always did.
//
// Deleting something that is not there succeeds in every case, which is
// what makes a destroy idempotent.
func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request, b *bucket, key string) *apiError {
	if id := r.URL.Query().Get("versionId"); id != "" {
		if info, ok := s.objects.deleteVersion(b.Name, key, id); ok {
			if info.DeleteMarker {
				w.Header().Set("x-amz-delete-marker", "true")
			}
			w.Header().Set("x-amz-version-id", id)
		}
		w.WriteHeader(204)
		return nil
	}
	if b.Versioning == "" {
		s.objects.deleteKey(b.Name, key)
		w.WriteHeader(204)
		return nil
	}
	// A marker over nothing at all is not laid: real S3 does create one
	// for a key that never existed, but it also then reports the bucket as
	// non-empty for ever, and a destroy that deletes an absent key would
	// make the bucket undeletable. Only a key with a history gets one.
	if _, _, ok := s.objects.get(b.Name, key); !ok {
		w.WriteHeader(204)
		return nil
	}
	marker := s.objects.putMarker(b.Name, key, b.versioned())
	w.Header().Set("x-amz-delete-marker", "true")
	w.Header().Set("x-amz-version-id", marker.VersionID)
	w.WriteHeader(204)
	return nil
}

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, b *bucket, key string) *apiError {
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
	stored := s.objects.put(b.Name, key, data, info, b.versioned())
	w.Header().Set("ETag", stored.ETag)
	writeVersionID(w, b, stored.VersionID)
	w.WriteHeader(200)
	return nil
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, b *bucket, key string, withBody bool) *apiError {
	bucketName := b.Name
	data, info, ok := readVersion(s.objects, bucketName, key, r.URL.Query().Get("versionId"))
	if !ok {
		return &apiError{Status: 404, Code: "NoSuchKey",
			Message: "The specified key does not exist.", Resource: "/" + bucketName + "/" + key}
	}
	if info.DeleteMarker {
		// A delete marker is not an object, and S3 says so twice: the
		// header tells a client WHY the key is gone, which it cannot
		// otherwise tell from a 404, and the status differs depending on
		// whether the marker was asked for by id. Asking for it by id is
		// 405, because a marker has no bytes to GET.
		w.Header().Set("x-amz-delete-marker", "true")
		w.Header().Set("x-amz-version-id", info.VersionID)
		if r.URL.Query().Get("versionId") != "" {
			return &apiError{Status: 405, Code: "MethodNotAllowed",
				Message:  "The specified method is not allowed against this resource.",
				Resource: "/" + bucketName + "/" + key}
		}
		return &apiError{Status: 404, Code: "NoSuchKey",
			Message: "The specified key does not exist.", Resource: "/" + bucketName + "/" + key}
	}
	writeVersionID(w, b, info.VersionID)
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

type listVersionsResult struct {
	XMLName        xml.Name `xml:"ListVersionsResult"`
	Xmlns          string   `xml:"xmlns,attr"`
	Name           string   `xml:"Name"`
	Prefix         string   `xml:"Prefix"`
	Delimiter      string   `xml:"Delimiter,omitempty"`
	MaxKeys        int      `xml:"MaxKeys"`
	IsTruncated    bool     `xml:"IsTruncated"`
	Versions       []objectVersion
	DeleteMarkers  []deleteMarkerEntry
	CommonPrefixes []commonPrefix `xml:"CommonPrefixes"`
}

type objectVersion struct {
	XMLName      xml.Name `xml:"Version"`
	Key          string   `xml:"Key"`
	VersionID    string   `xml:"VersionId"`
	IsLatest     bool     `xml:"IsLatest"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
	Size         int64    `xml:"Size"`
	StorageClass string   `xml:"StorageClass"`
}

// deleteMarkerEntry is a DIFFERENT element from a version, not a version
// with a flag: it carries no ETag, no size and no storage class, because a
// marker has no bytes. Its own type, because one Go type with XMLName
// "Version" cannot also be serialised as "DeleteMarker" -- the XMLName
// wins over the field tag, so a marker would have come out looking like a
// zero-sized object.
type deleteMarkerEntry struct {
	XMLName      xml.Name `xml:"DeleteMarker"`
	Key          string   `xml:"Key"`
	VersionID    string   `xml:"VersionId"`
	IsLatest     bool     `xml:"IsLatest"`
	LastModified string   `xml:"LastModified"`
}

// listObjectVersions answers GET /{bucket}?versions.
//
// This is how the aws provider empties a VERSIONED bucket for
// force_destroy, and until it existed the call fell through to the generic
// empty document for an unimplemented sub-resource. The provider then
// deleted nothing and asked for the bucket, which refused because it was
// not empty, and the destroy retried for ever:
//
//	aws_s3_bucket.assets: Still destroying... [10m0s elapsed]
//
// The same shape as Cloud Storage's anywhereCaches 404: an unimplemented
// sub-resource failing politely is not safe when a client is reading the
// failure as an answer.
//
// Every version of every key is reported, newest first within a key, with
// IsLatest on the first of each -- which the store's own ordering gives,
// rather than being recovered by comparing timestamps that two concurrent
// writes can share. Delete markers are a separate element, because they
// have no bytes.
//
// A bucket that has never had versioning enabled reports each object once,
// as version "null": real S3's id for an object written before versioning,
// and a real id rather than an absent field, because emptying a bucket is
// a list followed by a delete per version.
func (s *Server) listObjectVersions(w http.ResponseWriter, r *http.Request, bucketName string) *apiError {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")
	maxKeys := 1000
	if v, err := strconv.Atoi(q.Get("max-keys")); err == nil && v > 0 {
		maxKeys = v
	}

	doc := listVersionsResult{
		Xmlns: s3Namespace, Name: bucketName, Prefix: prefix,
		Delimiter: delimiter, MaxKeys: maxKeys,
		Versions: []objectVersion{}, DeleteMarkers: []deleteMarkerEntry{},
		CommonPrefixes: []commonPrefix{},
	}
	seenPrefix := map[string]bool{}
	latestSeen := map[string]bool{}
	reported := 0
	for _, info := range s.objects.versions(bucketName, prefix) {
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
		if reported >= maxKeys {
			doc.IsTruncated = true
			break
		}
		reported++
		// The store hands versions back newest first within a key, so the
		// first one seen for a key is the current one.
		isLatest := !latestSeen[info.Key]
		latestSeen[info.Key] = true

		if info.DeleteMarker {
			doc.DeleteMarkers = append(doc.DeleteMarkers, deleteMarkerEntry{
				Key:          info.Key,
				VersionID:    info.VersionID,
				IsLatest:     isLatest,
				LastModified: info.LastModified.Format(time.RFC3339),
			})
			continue
		}
		doc.Versions = append(doc.Versions, objectVersion{
			Key:          info.Key,
			VersionID:    info.VersionID,
			IsLatest:     isLatest,
			LastModified: info.LastModified.Format(time.RFC3339),
			ETag:         info.ETag,
			Size:         info.Size,
			StorageClass: "STANDARD",
		})
	}
	writeXML(w, 200, doc)
	return nil
}

// deleteObjects is the bulk delete a `terraform destroy` with
// force_destroy uses, and `aws s3 rm --recursive` after it.
type deleteObjectsResult struct {
	XMLName xml.Name  `xml:"DeleteResult"`
	Xmlns   string    `xml:"xmlns,attr"`
	Deleted []deleted `xml:"Deleted"`
}

// deleted is one entry in a batch delete's answer. The version fields are
// what a client emptying a versioned bucket reads back, and they are
// omitted for an unversioned one so the document stays what it was.
type deleted struct {
	Key                   string `xml:"Key"`
	VersionID             string `xml:"VersionId,omitempty"`
	DeleteMarker          bool   `xml:"DeleteMarker,omitempty"`
	DeleteMarkerVersionID string `xml:"DeleteMarkerVersionId,omitempty"`
}

// deleteObjects is the batch delete, and it is how the aws provider empties
// a VERSIONED bucket for force_destroy: it lists with ?versions and then
// sends each Key with its VersionId back here. Reading only the keys --
// which is what this did -- laid a delete marker over each one instead of
// removing anything, so the bucket stayed non-empty and the destroy
// retried for ever. The same failure the ?versions listing itself used to
// cause, one call later.
func (s *Server) deleteObjects(w http.ResponseWriter, r *http.Request, b *bucket) *apiError {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return errorf(400, "MalformedXML", "reading the request: %v", err)
	}
	var req struct {
		Objects []struct {
			Key       string `xml:"Key"`
			VersionID string `xml:"VersionId"`
		} `xml:"Object"`
	}
	if err := xml.Unmarshal(body, &req); err != nil {
		return errorf(400, "MalformedXML", "parsing the request: %v", err)
	}
	doc := deleteObjectsResult{Xmlns: s3Namespace}
	for _, o := range req.Objects {
		entry := deleted{Key: o.Key}
		switch {
		case o.VersionID != "":
			info, ok := s.objects.deleteVersion(b.Name, o.Key, o.VersionID)
			entry.VersionID = o.VersionID
			entry.DeleteMarker = ok && info.DeleteMarker
		case b.Versioning == "":
			s.objects.deleteKey(b.Name, o.Key)
		default:
			if _, _, ok := s.objects.get(b.Name, o.Key); ok {
				marker := s.objects.putMarker(b.Name, o.Key, b.versioned())
				entry.DeleteMarker = true
				entry.DeleteMarkerVersionID = marker.VersionID
			}
		}
		doc.Deleted = append(doc.Deleted, entry)
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

// readVersion is the current version, or a named one. One helper rather
// than a branch at each call site, because a GET and a HEAD answer the same
// question and a versionId is legal on both.
func readVersion(store *objectStore, bucket, key, versionID string) ([]byte, objectInfo, bool) {
	if versionID == "" {
		return store.get(bucket, key)
	}
	return store.getVersion(bucket, key, versionID)
}
