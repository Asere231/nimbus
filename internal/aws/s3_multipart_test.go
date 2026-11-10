package aws

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Multipart upload: what `aws s3 cp` does on its own for a file over 8 MB.
//
// Measured against aws-cli 1.40.33 before any of this existed. The CLI
// switches protocol by itself at its multipart_threshold, so a plain PUT is
// never attempted and the first call is:
//
//	POST /demo/big.bin?uploads
//	x-amz-checksum-algorithm: CRC32
//	Content-Length: 0
//
// which Nimbus answered `MethodNotAllowed: POST on an object`, because an
// object had only PUT, GET, HEAD and DELETE.

func initiateUpload(t *testing.T, s *Server, bucket, key string) string {
	t.Helper()
	rec := s3req(t, s, http.MethodPost, "/"+bucket+"/"+key+"?uploads", "")
	if rec.Code != 200 {
		t.Fatalf("CreateMultipartUpload = %d:\n%s", rec.Code, rec.Body)
	}
	var got struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		UploadID string   `xml:"UploadId"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v\n%s", err, rec.Body)
	}
	if got.Bucket != bucket || got.Key != key {
		t.Errorf("initiate echoed bucket/key %q/%q, want %q/%q", got.Bucket, got.Key, bucket, key)
	}
	if got.UploadID == "" {
		t.Fatal("no UploadId; every later call is addressed by it")
	}
	return got.UploadID
}

func uploadPart(t *testing.T, s *Server, bucket, key, uploadID string, n int, data string) string {
	t.Helper()
	path := fmt.Sprintf("/%s/%s?partNumber=%d&uploadId=%s", bucket, key, n, uploadID)
	rec := s3req(t, s, http.MethodPut, path, data)
	if rec.Code != 200 {
		t.Fatalf("UploadPart %d = %d:\n%s", n, rec.Code, rec.Body)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("part %d came back with no ETag; the client sends it back on complete", n)
	}
	return etag
}

func completeUpload(t *testing.T, s *Server, bucket, key, uploadID string, etags []string) *http.Response {
	t.Helper()
	var b strings.Builder
	b.WriteString("<CompleteMultipartUpload>")
	for i, e := range etags {
		fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", i+1, e)
	}
	b.WriteString("</CompleteMultipartUpload>")

	rec := s3req(t, s, http.MethodPost, "/"+bucket+"/"+key+"?uploadId="+uploadID, b.String())
	if rec.Code != 200 {
		t.Fatalf("CompleteMultipartUpload = %d:\n%s", rec.Code, rec.Body)
	}
	return rec.Result()
}

func TestAMultipartUploadAssemblesItsPartsInOrder(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")

	// Deliberately out of order on the wire: parts arrive concurrently and
	// the order they are UPLOADED in says nothing about the order they
	// belong in. Only PartNumber does.
	id := initiateUpload(t, s, "demo", "big.bin")
	second := uploadPart(t, s, "demo", "big.bin", id, 2, "world")
	first := uploadPart(t, s, "demo", "big.bin", id, 1, "hello ")

	completeUpload(t, s, "demo", "big.bin", id, []string{first, second})

	rec := s3req(t, s, http.MethodGet, "/demo/big.bin", "")
	if rec.Code != 200 {
		t.Fatalf("GET after complete = %d:\n%s", rec.Code, rec.Body)
	}
	if got := rec.Body.String(); got != "hello world" {
		t.Errorf("assembled object = %q, want %q", got, "hello world")
	}
}

// The ETag of a multipart object is NOT the MD5 of its bytes. It is the MD5
// of the concatenated binary part MD5s, then a dash and the number of parts
// — which is how a client can tell a multipart object from a plain one, and
// why `aws s3 cp` of a large file never matches a naive md5sum.
func TestAMultipartETagIsTheDigestOfTheParts(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")

	parts := []string{"hello ", "world"}
	id := initiateUpload(t, s, "demo", "big.bin")
	var etags []string
	for i, p := range parts {
		etags = append(etags, uploadPart(t, s, "demo", "big.bin", id, i+1, p))
	}
	resp := completeUpload(t, s, "demo", "big.bin", id, etags)
	_ = resp

	// Work out what it should be, the way S3 does.
	var concat []byte
	for _, p := range parts {
		sum := md5.Sum([]byte(p))
		concat = append(concat, sum[:]...)
	}
	outer := md5.Sum(concat)
	want := fmt.Sprintf(`"%s-%d"`, hex.EncodeToString(outer[:]), len(parts))

	rec := s3req(t, s, http.MethodHead, "/demo/big.bin", "")
	if got := rec.Header().Get("ETag"); got != want {
		t.Errorf("ETag = %s, want %s", got, want)
	}
	// And it must NOT be the plain MD5 of the whole object, which is what a
	// single-part PUT would give.
	whole := md5.Sum([]byte(strings.Join(parts, "")))
	if rec.Header().Get("ETag") == `"`+hex.EncodeToString(whole[:])+`"` {
		t.Error("the ETag is the MD5 of the bytes; a multipart object's is not")
	}
}

// A part's own ETag is an ordinary MD5, because each part is a plain PUT.
func TestAPartsETagIsItsOwnMD5(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	id := initiateUpload(t, s, "demo", "big.bin")

	got := uploadPart(t, s, "demo", "big.bin", id, 1, "hello ")
	sum := md5.Sum([]byte("hello "))
	if want := `"` + hex.EncodeToString(sum[:]) + `"`; got != want {
		t.Errorf("part ETag = %s, want %s", got, want)
	}
}

// Nothing is visible until the upload is completed: an in-flight upload is
// not a half-written object, and a client listing the bucket must not see
// one.
func TestAnIncompleteUploadIsNotAnObject(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	id := initiateUpload(t, s, "demo", "big.bin")
	uploadPart(t, s, "demo", "big.bin", id, 1, "hello ")

	if rec := s3req(t, s, http.MethodGet, "/demo/big.bin", ""); rec.Code != 404 {
		t.Errorf("GET of an unfinished upload = %d, want 404", rec.Code)
	}
	if rec := s3req(t, s, http.MethodGet, "/demo", ""); strings.Contains(rec.Body.String(), "big.bin") {
		t.Error("an unfinished upload appeared in the bucket listing")
	}
}

// ListParts is how a client checks what the server actually received.
func TestListPartsReportsWhatArrived(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	id := initiateUpload(t, s, "demo", "big.bin")
	uploadPart(t, s, "demo", "big.bin", id, 1, "hello ")
	uploadPart(t, s, "demo", "big.bin", id, 2, "world")

	rec := s3req(t, s, http.MethodGet, "/demo/big.bin?uploadId="+id, "")
	if rec.Code != 200 {
		t.Fatalf("ListParts = %d:\n%s", rec.Code, rec.Body)
	}
	var got struct {
		XMLName  xml.Name `xml:"ListPartsResult"`
		Bucket   string   `xml:"Bucket"`
		UploadID string   `xml:"UploadId"`
		Parts    []struct {
			PartNumber int    `xml:"PartNumber"`
			Size       int64  `xml:"Size"`
			ETag       string `xml:"ETag"`
		} `xml:"Part"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v\n%s", err, rec.Body)
	}
	if len(got.Parts) != 2 {
		t.Fatalf("ListParts returned %d parts:\n%s", len(got.Parts), rec.Body)
	}
	// In part-number order, whatever order they arrived in.
	if got.Parts[0].PartNumber != 1 || got.Parts[1].PartNumber != 2 {
		t.Errorf("parts are not in order: %+v", got.Parts)
	}
	if got.Parts[0].Size != 6 {
		t.Errorf("part 1 size = %d, want 6", got.Parts[0].Size)
	}
}

// Aborting throws the parts away. A client does this when an upload fails
// halfway, and an emulator that kept them would leak every abandoned copy.
func TestAbortingAnUploadDiscardsItsParts(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	id := initiateUpload(t, s, "demo", "big.bin")
	uploadPart(t, s, "demo", "big.bin", id, 1, "hello ")

	rec := s3req(t, s, http.MethodDelete, "/demo/big.bin?uploadId="+id, "")
	if rec.Code != 204 {
		t.Errorf("AbortMultipartUpload = %d, want 204:\n%s", rec.Code, rec.Body)
	}
	// Everything addressed by that id is gone, with the code S3 uses.
	if rec := s3req(t, s, http.MethodGet, "/demo/big.bin?uploadId="+id, ""); rec.Code != 404 {
		t.Errorf("ListParts after abort = %d, want 404", rec.Code)
	}
	if rec := s3req(t, s, http.MethodPut,
		"/demo/big.bin?partNumber=2&uploadId="+id, "x"); rec.Code != 404 {
		t.Errorf("UploadPart after abort = %d, want 404", rec.Code)
	}
}

// An upload id nobody issued is NoSuchUpload, not a generic failure: a
// client retrying after an abort matches on that code.
func TestAnUnknownUploadIdIsNoSuchUpload(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")

	for _, c := range []struct {
		method, path string
	}{
		{http.MethodGet, "/demo/big.bin?uploadId=nope"},
		{http.MethodPut, "/demo/big.bin?partNumber=1&uploadId=nope"},
		{http.MethodPost, "/demo/big.bin?uploadId=nope"},
	} {
		rec := s3req(t, s, c.method, c.path, "<CompleteMultipartUpload></CompleteMultipartUpload>")
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), "NoSuchUpload") {
			t.Errorf("%s %s = %d:\n%s", c.method, c.path, rec.Code, rec.Body)
		}
	}
}

// The completed object keeps the bytes exactly, including every byte value
// — the point of the whole exercise is a file that round-trips.
func TestAssembledBytesAreExact(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")

	// Every byte value, split across parts at an awkward boundary.
	all := make([]byte, 0, 256*4)
	for i := 0; i < 4; i++ {
		for b := 0; b < 256; b++ {
			all = append(all, byte(b))
		}
	}
	chunks := [][]byte{all[:300], all[300:700], all[700:]}

	id := initiateUpload(t, s, "demo", "bytes.bin")
	var etags []string
	for i, c := range chunks {
		etags = append(etags, uploadPart(t, s, "demo", "bytes.bin", id, i+1, string(c)))
	}
	completeUpload(t, s, "demo", "bytes.bin", id, etags)

	rec := s3req(t, s, http.MethodGet, "/demo/bytes.bin", "")
	if !bytes.Equal(rec.Body.Bytes(), all) {
		t.Errorf("round-tripped %d bytes, want %d, and they differ", rec.Body.Len(), len(all))
	}
}

// Ranged GET, which is how `aws s3 cp` DOWNLOADS a large object: several
// concurrent ranged requests, written at their own offsets.
//
// Nimbus advertised `Accept-Ranges: bytes` and then ignored the Range
// header, so every one of those requests returned the WHOLE object and the
// CLI assembled overlapping copies. Measured: a 20,000,000-byte file came
// back as 36,777,216 bytes. Nothing errored -- it is the silent kind of
// wrong, and worse than refusing, because an emulator that corrupts a file
// teaches you to distrust your own code.
//
// Advertising the capability is the promise; this is keeping it.
func TestARangedGetReturnsOnlyThatRange(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	s3req(t, s, http.MethodPut, "/demo/abc.txt", "abcdefghij")

	for _, c := range []struct {
		header, want, contentRange string
	}{
		{"bytes=0-4", "abcde", "bytes 0-4/10"},
		{"bytes=5-9", "fghij", "bytes 5-9/10"},
		{"bytes=3-", "defghij", "bytes 3-9/10"},       // open-ended: to the end
		{"bytes=-3", "hij", "bytes 7-9/10"},           // suffix: the last N bytes
		{"bytes=0-100", "abcdefghij", "bytes 0-9/10"}, // past the end is clamped
	} {
		rec := s3req(t, s, http.MethodGet, "/demo/abc.txt", "", "Range", c.header)
		if rec.Code != 206 {
			t.Errorf("%s = %d, want 206 Partial Content", c.header, rec.Code)
			continue
		}
		if got := rec.Body.String(); got != c.want {
			t.Errorf("%s returned %q, want %q", c.header, got, c.want)
		}
		if got := rec.Header().Get("Content-Range"); got != c.contentRange {
			t.Errorf("%s Content-Range = %q, want %q", c.header, got, c.contentRange)
		}
		if got := rec.Header().Get("Content-Length"); got != fmt.Sprint(len(c.want)) {
			t.Errorf("%s Content-Length = %q, want %d", c.header, got, len(c.want))
		}
	}

	// No Range at all is still the whole object, with a 200.
	rec := s3req(t, s, http.MethodGet, "/demo/abc.txt", "")
	if rec.Code != 200 || rec.Body.String() != "abcdefghij" {
		t.Errorf("unranged GET = %d %q", rec.Code, rec.Body)
	}

	// A range that starts past the end is 416, with the object's size --
	// which is how a client learns what it should have asked for.
	rec = s3req(t, s, http.MethodGet, "/demo/abc.txt", "", "Range", "bytes=50-60")
	if rec.Code != 416 {
		t.Errorf("an unsatisfiable range = %d, want 416", rec.Code)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes */10" {
		t.Errorf("416 Content-Range = %q, want \"bytes */10\"", got)
	}
}

// A range Nimbus cannot parse is IGNORED and the whole object returned,
// which is what RFC 9110 says to do and what S3 does. Refusing would fail a
// download over a header the client could have sent for any reason.
func TestAnUnparseableRangeIsIgnoredNotRefused(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	s3req(t, s, http.MethodPut, "/demo/abc.txt", "abcdefghij")

	for _, header := range []string{"rows=1-2", "bytes=", "bytes=abc-def", "nonsense"} {
		rec := s3req(t, s, http.MethodGet, "/demo/abc.txt", "", "Range", header)
		if rec.Code != 200 || rec.Body.String() != "abcdefghij" {
			t.Errorf("Range %q = %d %q, want the whole object with 200", header, rec.Code, rec.Body)
		}
	}
}

// A HEAD carries the size and never a body, ranged or not.
func TestAHeadIsNeverRanged(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	s3req(t, s, http.MethodPut, "/demo/abc.txt", "abcdefghij")

	rec := s3req(t, s, http.MethodHead, "/demo/abc.txt", "", "Range", "bytes=0-4")
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD returned a body of %d bytes", rec.Body.Len())
	}
	if got := rec.Header().Get("Content-Length"); got != "10" {
		t.Errorf("HEAD Content-Length = %q, want the whole size 10", got)
	}
}
