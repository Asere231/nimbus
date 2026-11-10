package aws

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// aws-chunked: how aws-cli v2 sends a body.
//
// Found by CI, not locally, and that is the point of the Linux leg. The
// runners ship aws-cli v2; this machine has v1.40.33. v2 asks for a CRC32
// checksum (`x-amz-checksum-algorithm: CRC32` was in the very first
// measured CreateMultipartUpload) and then sends each body wrapped in
// CHUNK FRAMING with the checksum in a trailer:
//
//	<hex-size>\r\n<data>\r\n0\r\nx-amz-checksum-crc32:<base64>\r\n\r\n
//
// Nimbus stored those framing bytes as content. A 20,000,000-byte file
// uploaded in three parts came back as 20,000,162 -- about fifty bytes of
// framing per part -- differing from byte 1, because the first thing in
// the object was a chunk-size header. The same silent corruption as the
// ranged-GET bug, at the other end of the same transfer.
//
// `Content-Length` counts the FRAMED body; `x-amz-decoded-content-length`
// is the real size, which is how a server knows the difference.

// chunked frames data the way the CLI does, with a trailing checksum.
func chunked(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		fmt.Fprintf(&b, "%x\r\n%s\r\n", len(p), p)
	}
	b.WriteString("0\r\n")
	b.WriteString("x-amz-checksum-crc32:AAAAAA==\r\n")
	b.WriteString("\r\n")
	return b.String()
}

func TestAnAwsChunkedBodyIsDecodedNotStored(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")

	body := chunked("hello ", "world")
	rec := s3req(t, s, http.MethodPut, "/demo/hi.txt", body,
		"Content-Encoding", "aws-chunked",
		"x-amz-content-sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER",
		"x-amz-decoded-content-length", "11")
	if rec.Code != 200 {
		t.Fatalf("PUT = %d:\n%s", rec.Code, rec.Body)
	}

	got := s3req(t, s, http.MethodGet, "/demo/hi.txt", "")
	if got.Body.String() != "hello world" {
		t.Errorf("stored %q, want %q", got.Body.String(), "hello world")
	}
	// And the ETag is the digest of the CONTENT, not of the framing.
	sum := md5.Sum([]byte("hello world"))
	if want := `"` + hex.EncodeToString(sum[:]) + `"`; got.Header().Get("ETag") != want {
		t.Errorf("ETag = %s, want %s", got.Header().Get("ETag"), want)
	}
}

// The same framing on an UploadPart body, which is where CI actually hit
// it: three framed parts, three lots of framing stored as content.
func TestChunkedMultipartPartsAssembleToTheRealBytes(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")
	id := initiateUpload(t, s, "demo", "big.bin")

	headers := []string{
		"Content-Encoding", "aws-chunked",
		"x-amz-content-sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER",
	}
	var etags []string
	for i, text := range []string{"hello ", "world"} {
		path := fmt.Sprintf("/demo/big.bin?partNumber=%d&uploadId=%s", i+1, id)
		h := append(append([]string{}, headers...), "x-amz-decoded-content-length", fmt.Sprint(len(text)))
		rec := s3req(t, s, http.MethodPut, path, chunked(text), h...)
		if rec.Code != 200 {
			t.Fatalf("UploadPart %d = %d:\n%s", i+1, rec.Code, rec.Body)
		}
		etags = append(etags, rec.Header().Get("ETag"))
		// A part's ETag is the MD5 of its CONTENT; framing in it would
		// also break CompleteMultipartUpload's digest-of-digests.
		sum := md5.Sum([]byte(text))
		if want := `"` + hex.EncodeToString(sum[:]) + `"`; etags[i] != want {
			t.Errorf("part %d ETag = %s, want %s", i+1, etags[i], want)
		}
	}
	completeUpload(t, s, "demo", "big.bin", id, etags)

	rec := s3req(t, s, http.MethodGet, "/demo/big.bin", "")
	if rec.Body.String() != "hello world" {
		t.Errorf("assembled %q, want %q", rec.Body.String(), "hello world")
	}
}

// A chunk header may carry a signature, which is what the SIGNED streaming
// variant sends. The size is everything before the semicolon.
func TestASignedChunkHeaderIsReadForItsSizeOnly(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")

	body := "6;chunk-signature=0a1b2c3d\r\nhello \r\n" +
		"5;chunk-signature=4e5f6a7b\r\nworld\r\n" +
		"0;chunk-signature=8c9d0e1f\r\n\r\n"
	rec := s3req(t, s, http.MethodPut, "/demo/signed.txt", body,
		"Content-Encoding", "aws-chunked",
		"x-amz-content-sha256", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD",
		"x-amz-decoded-content-length", "11")
	if rec.Code != 200 {
		t.Fatalf("PUT = %d:\n%s", rec.Code, rec.Body)
	}
	if got := s3req(t, s, http.MethodGet, "/demo/signed.txt", ""); got.Body.String() != "hello world" {
		t.Errorf("stored %q, want %q", got.Body.String(), "hello world")
	}
}

// An ordinary body must be untouched. Only a request that SAYS it is
// chunked is decoded -- a plain object whose bytes happen to begin with
// something hex-looking must not be mangled.
func TestAPlainBodyIsNeverDecoded(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")

	// Deliberately chunk-shaped content, sent as an ordinary body.
	body := "5\r\nhello\r\n0\r\n\r\n"
	s3req(t, s, http.MethodPut, "/demo/plain.txt", body)
	if got := s3req(t, s, http.MethodGet, "/demo/plain.txt", ""); got.Body.String() != body {
		t.Errorf("stored %q, want it byte for byte", got.Body.String())
	}
}

// A truncated or malformed chunked body is REFUSED rather than stored
// half-decoded: a silently short object is the failure this whole bug was.
func TestAMalformedChunkedBodyIsRefused(t *testing.T) {
	s := newTestServer(t)
	makeBucket(t, s, "demo")

	for _, body := range []string{
		"zz\r\nhello\r\n0\r\n\r\n", // size is not hex
		"20\r\nhello\r\n",          // claims more than it carries
	} {
		rec := s3req(t, s, http.MethodPut, "/demo/bad.txt", body,
			"Content-Encoding", "aws-chunked",
			"x-amz-content-sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
		if rec.Code != 400 {
			t.Errorf("a malformed chunked body = %d, want 400:\n%s", rec.Code, rec.Body)
		}
		var e struct {
			XMLName xml.Name `xml:"Error"`
			Code    string   `xml:"Code"`
		}
		if xml.Unmarshal(rec.Body.Bytes(), &e) == nil && e.Code != "IncompleteBody" {
			t.Errorf("error code = %q, want IncompleteBody", e.Code)
		}
	}
}
