package aws

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Multipart upload.
//
// `aws s3 cp` switches to this on its own for a file over its
// multipart_threshold (8 MB by default), so a plain PUT is never attempted
// and a large file simply failed before this existed. Measured against
// aws-cli 1.40.33, the first call is:
//
//	POST /demo/big.bin?uploads
//	x-amz-checksum-algorithm: CRC32
//	Content-Length: 0
//
// which Nimbus answered `MethodNotAllowed: POST on an object`, because an
// object had only PUT, GET, HEAD and DELETE. An object path carries five
// more operations, all told apart by query parameters rather than by verb:
//
//	POST   ?uploads                         CreateMultipartUpload
//	PUT    ?partNumber=N&uploadId=X         UploadPart
//	POST   ?uploadId=X                      CompleteMultipartUpload
//	DELETE ?uploadId=X                      AbortMultipartUpload
//	GET    ?uploadId=X                      ListParts
//
// **The ETag of a multipart object is not the MD5 of its bytes.** It is the
// MD5 of the concatenated binary part MD5s, a dash, and the number of
// parts. That is how a client can tell a multipart object from a plain one,
// and why `md5sum` of a large file never matches what S3 reports. Getting
// it wrong is the quiet kind of mistake: everything works until something
// verifies.
//
// Parts live in memory and only in memory. An upload in flight is not an
// object -- nothing lists it and a GET of its key still 404s -- and losing
// one to a restart costs a client a retry, which it is already written to
// do. Real S3 keeps an incomplete upload until a lifecycle rule reaps it;
// that divergence is in docs/ROADMAP.md.

// maxUploadBytes bounds what one upload may buffer. A local emulator
// filling the machine's memory is a worse failure than a refused upload,
// and this is well past anything an example needs.
const maxUploadBytes = 512 << 20

type uploadPartData struct {
	number int
	etag   string
	data   []byte
}

type multipartUpload struct {
	id      string
	bucket  string
	key     string
	info    objectInfo // the content type and metadata the create carried
	parts   map[int]uploadPartData
	totalSz int
}

// multipartStore holds uploads in flight. Beside the object store rather
// than in it, the way DynamoDB's items sit beside its tables: an upload is
// not a resource and must not draw in the graph.
type multipartStore struct {
	mu      sync.Mutex
	uploads map[string]*multipartUpload
}

func newMultipartStore() *multipartStore {
	return &multipartStore{uploads: map[string]*multipartUpload{}}
}

func (m *multipartStore) create(bucket, key string, info objectInfo) *multipartUpload {
	m.mu.Lock()
	defer m.mu.Unlock()
	up := &multipartUpload{
		id: "nimbus-" + hexID(32), bucket: bucket, key: key, info: info,
		parts: map[int]uploadPartData{},
	}
	m.uploads[up.id] = up
	return up
}

// get returns the upload if the id names one for this exact bucket and key.
// S3 addresses an upload by all three, and a mismatch is NoSuchUpload.
func (m *multipartStore) get(id, bucket, key string) *multipartUpload {
	m.mu.Lock()
	defer m.mu.Unlock()
	up := m.uploads[id]
	if up == nil || up.bucket != bucket || up.key != key {
		return nil
	}
	return up
}

func (m *multipartStore) remove(id string) { m.mu.Lock(); delete(m.uploads, id); m.mu.Unlock() }

// listFor returns the uploads in flight for a bucket, oldest id first.
func (m *multipartStore) listFor(bucket string) []*multipartUpload {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*multipartUpload
	for _, up := range m.uploads {
		if up.bucket == bucket {
			out = append(out, up)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func (m *multipartStore) addPart(up *multipartUpload, number int, data []byte) (string, *apiError) {
	sum := md5.Sum(data)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`

	m.mu.Lock()
	defer m.mu.Unlock()
	grown := up.totalSz + len(data)
	if old, ok := up.parts[number]; ok {
		grown -= len(old.data) // re-uploading a part replaces it
	}
	if grown > maxUploadBytes {
		return "", errorf(400, "EntityTooLarge",
			"this upload would exceed the %d bytes Nimbus will buffer for one object", maxUploadBytes)
	}
	up.parts[number] = uploadPartData{number: number, etag: etag, data: append([]byte(nil), data...)}
	up.totalSz = grown
	return etag, nil
}

// ordered returns the parts by part number, which is the only thing that
// says what order they belong in -- a client uploads them concurrently, so
// arrival order says nothing.
func (m *multipartStore) ordered(up *multipartUpload) []uploadPartData {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]uploadPartData, 0, len(up.parts))
	for _, p := range up.parts {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].number < out[j].number })
	return out
}

// --- the handlers ----------------------------------------------------------

type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

func (s *Server) createMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) *apiError {
	info := objectInfo{ContentType: r.Header.Get("Content-Type"), Metadata: objectMetadataOf(r)}
	if info.ContentType == "" {
		info.ContentType = "binary/octet-stream"
	}
	up := s.uploads.create(bucket, key, info)
	writeXML(w, 200, initiateMultipartUploadResult{
		Xmlns: s3Namespace, Bucket: bucket, Key: key, UploadID: up.id,
	})
	return nil
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) *apiError {
	up := s.uploads.get(uploadID, bucket, key)
	if up == nil {
		return noSuchUpload(uploadID, bucket, key)
	}
	number, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if err != nil || number < 1 || number > 10000 {
		return errorf(400, "InvalidArgument", "Part number must be an integer between 1 and 10000")
	}
	data, rerr := io.ReadAll(io.LimitReader(r.Body, maxUploadBytes+1))
	if rerr != nil {
		return errorf(400, "IncompleteBody", "reading the part: %v", rerr)
	}
	etag, aerr := s.uploads.addPart(up, number, data)
	if aerr != nil {
		return aerr
	}
	// The client keeps this and sends it back in CompleteMultipartUpload.
	w.Header().Set("ETag", etag)
	w.WriteHeader(200)
	return nil
}

type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

func (s *Server) completeMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key, uploadID string) *apiError {
	up := s.uploads.get(uploadID, bucket, key)
	if up == nil {
		return noSuchUpload(uploadID, bucket, key)
	}
	parts := s.uploads.ordered(up)
	if len(parts) == 0 {
		return errorf(400, "InvalidRequest",
			"You must specify at least one part")
	}

	// Assembled in PART NUMBER order, which is the only order that means
	// anything: the client uploads parts concurrently.
	var data []byte
	var digests []byte
	for _, p := range parts {
		data = append(data, p.data...)
		raw, err := hex.DecodeString(strings.Trim(p.etag, `"`))
		if err != nil {
			return errorf(500, "InternalError", "a part's etag is not a digest")
		}
		digests = append(digests, raw...)
	}

	// The multipart ETag: MD5 of the concatenated part MD5s, a dash, and
	// the number of parts. NOT the MD5 of the assembled bytes, which is
	// what a single PUT would give and what a naive md5sum computes.
	outer := md5.Sum(digests)
	etag := fmt.Sprintf(`"%s-%d"`, hex.EncodeToString(outer[:]), len(parts))

	info := up.info
	info.ETag = etag
	s.objects.putWithETag(bucket, key, data, info)
	s.uploads.remove(uploadID)

	writeXML(w, 200, completeMultipartUploadResult{
		Xmlns:    s3Namespace,
		Location: s.cfg.PublicURL + "/" + bucket + "/" + key,
		Bucket:   bucket, Key: key, ETag: etag,
	})
	return nil
}

func (s *Server) abortMultipartUpload(w http.ResponseWriter, bucket, key, uploadID string) *apiError {
	if up := s.uploads.get(uploadID, bucket, key); up == nil {
		return noSuchUpload(uploadID, bucket, key)
	}
	s.uploads.remove(uploadID)
	w.WriteHeader(204)
	return nil
}

type listPartsResult struct {
	XMLName     xml.Name   `xml:"ListPartsResult"`
	Xmlns       string     `xml:"xmlns,attr"`
	Bucket      string     `xml:"Bucket"`
	Key         string     `xml:"Key"`
	UploadID    string     `xml:"UploadId"`
	IsTruncated bool       `xml:"IsTruncated"`
	Parts       []partInfo `xml:"Part"`
}

type partInfo struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
	Size       int64  `xml:"Size"`
}

func (s *Server) listParts(w http.ResponseWriter, bucket, key, uploadID string) *apiError {
	up := s.uploads.get(uploadID, bucket, key)
	if up == nil {
		return noSuchUpload(uploadID, bucket, key)
	}
	doc := listPartsResult{Xmlns: s3Namespace, Bucket: bucket, Key: key, UploadID: uploadID}
	for _, p := range s.uploads.ordered(up) {
		doc.Parts = append(doc.Parts, partInfo{
			PartNumber: p.number, ETag: p.etag, Size: int64(len(p.data)),
		})
	}
	writeXML(w, 200, doc)
	return nil
}

type listMultipartUploadsResult struct {
	XMLName     xml.Name     `xml:"ListMultipartUploadsResult"`
	Xmlns       string       `xml:"xmlns,attr"`
	Bucket      string       `xml:"Bucket"`
	IsTruncated bool         `xml:"IsTruncated"`
	Uploads     []uploadInfo `xml:"Upload"`
}

type uploadInfo struct {
	Key      string `xml:"Key"`
	UploadID string `xml:"UploadId"`
}

// listMultipartUploads answers `GET /{bucket}?uploads`. The provider reads
// it as one of a bucket's sub-resources, so it must answer an empty list
// rather than an error when nothing is in flight.
func (s *Server) listMultipartUploads(w http.ResponseWriter, bucket string) *apiError {
	doc := listMultipartUploadsResult{Xmlns: s3Namespace, Bucket: bucket}
	for _, up := range s.uploads.listFor(bucket) {
		doc.Uploads = append(doc.Uploads, uploadInfo{Key: up.key, UploadID: up.id})
	}
	writeXML(w, 200, doc)
	return nil
}

func noSuchUpload(id, bucket, key string) *apiError {
	return &apiError{
		Status: 404, Code: "NoSuchUpload",
		Message:  "The specified upload does not exist. The upload ID may be invalid, or the upload may have been aborted or completed.",
		Resource: "/" + bucket + "/" + key,
	}
}
