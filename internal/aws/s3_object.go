package aws

import (
	"encoding/xml"
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
	data, err := io.ReadAll(io.LimitReader(r.Body, maxObject))
	if err != nil {
		return errorf(400, "IncompleteBody", "reading the body: %v", err)
	}
	info := objectInfo{
		ContentType: r.Header.Get("Content-Type"),
		Metadata:    map[string]string{},
	}
	if info.ContentType == "" {
		info.ContentType = "binary/octet-stream"
	}
	// User metadata arrives as x-amz-meta-*, and the part after the prefix
	// is the key the client will look for. Go canonicalises header names
	// on both sides, so it is lower-cased here deliberately -- the same
	// trap Azure's blob metadata had in Milestone 2.
	for name, values := range r.Header {
		if lower := strings.ToLower(name); strings.HasPrefix(lower, "x-amz-meta-") && len(values) > 0 {
			info.Metadata[strings.TrimPrefix(lower, "x-amz-meta-")] = values[0]
		}
	}
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
		w.WriteHeader(200)
		return nil
	}
	w.WriteHeader(200)
	_, _ = w.Write(data)
	return nil
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
