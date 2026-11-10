package aws

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// aws-chunked bodies.
//
// aws-cli **v2** asks for a checksum (`x-amz-checksum-algorithm: CRC32`)
// and then sends the body wrapped in chunk framing with that checksum in a
// trailer, rather than as plain bytes:
//
//	<hex-size>[;chunk-signature=…]\r\n<data>\r\n
//	…
//	0\r\n
//	x-amz-checksum-crc32:<base64>\r\n
//	\r\n
//
// Nimbus read `r.Body` straight through, so the framing was stored as
// content. Found by CI rather than here, which is what the Linux leg is
// for: the runners ship v2 and this machine has v1.40.33, which sends
// plain bytes. A 20,000,000-byte file uploaded in three parts came back as
// 20,000,162 — about fifty bytes of framing per part — and differed from
// byte 1, because the first thing in the object was a chunk-size header.
// Silent corruption, the same shape as the ranged-GET bug, at the other
// end of the same transfer.
//
// Two headers say a body is framed, and either may be the one present:
// `Content-Encoding: aws-chunked`, and an `x-amz-content-sha256` naming a
// STREAMING variant. `x-amz-decoded-content-length` then carries the real
// size, which is checked when it is there — a decode that silently came
// out short would be the very failure this fixes.
//
// Only a request that SAYS it is framed is decoded. A plain object whose
// bytes happen to look like a chunk header must be stored byte for byte.

// isChunkedBody reports whether this request's body carries chunk framing.
func isChunkedBody(r *http.Request) bool {
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Encoding")), "aws-chunked") {
		return true
	}
	return strings.HasPrefix(r.Header.Get("x-amz-content-sha256"), "STREAMING-")
}

// readS3Body reads a request body, decoding aws-chunked framing when the
// request says there is some. limit bounds the DECODED size.
func readS3Body(r *http.Request, limit int64) ([]byte, error) {
	if !isChunkedBody(r) {
		return io.ReadAll(io.LimitReader(r.Body, limit))
	}

	// The framed body is larger than its content, so the raw read is
	// allowed a little more room than the decoded result.
	data, err := decodeChunked(io.LimitReader(r.Body, limit+chunkOverheadAllowance), limit)
	if err != nil {
		return nil, err
	}
	// x-amz-decoded-content-length is the real size. When the client sends
	// it, it is free verification that the decode agrees with what was
	// meant -- and a short decode is exactly the bug being fixed here.
	if want := r.Header.Get("x-amz-decoded-content-length"); want != "" {
		n, convErr := strconv.ParseInt(want, 10, 64)
		if convErr == nil && n != int64(len(data)) {
			return nil, fmt.Errorf("the body decoded to %d bytes, but x-amz-decoded-content-length says %d",
				len(data), n)
		}
	}
	return data, nil
}

// chunkOverheadAllowance is how much framing a body may carry beyond its
// content: a header and a CRLF per chunk, plus the terminator and trailer.
// Generous on purpose -- it is a guard against an endless body, not a
// precise accounting.
const chunkOverheadAllowance = 1 << 20

var errChunkFormat = errors.New("the body is not valid aws-chunked data")

// decodeChunked unwraps the framing, returning the content alone.
func decodeChunked(r io.Reader, limit int64) ([]byte, error) {
	br := bufio.NewReader(r)
	var out []byte

	for {
		header, err := br.ReadString('\n')
		if err != nil {
			// A body that ends where a chunk header should start is
			// truncated, not finished: the terminator is an explicit
			// zero-length chunk.
			return nil, errChunkFormat
		}
		header = strings.TrimRight(header, "\r\n")
		if header == "" {
			continue // tolerate a stray blank line between chunks
		}
		// `<hex-size>` or `<hex-size>;chunk-signature=…`. Everything after
		// the semicolon is an extension and none of it is checked: a local
		// emulator verifying a signature could only ever report a clock
		// skew, as sigv4.go already says.
		size, err := strconv.ParseInt(strings.SplitN(header, ";", 2)[0], 16, 64)
		if err != nil || size < 0 {
			return nil, errChunkFormat
		}
		if size == 0 {
			// The trailer and the final CRLF follow. Nothing in them is
			// needed -- the checksum is the client's own -- and the body
			// ends here whether or not they arrive.
			return out, nil
		}
		if int64(len(out))+size > limit {
			return nil, fmt.Errorf("the body is larger than the %d bytes this emulator will store", limit)
		}

		chunk := make([]byte, size)
		if _, err := io.ReadFull(br, chunk); err != nil {
			// Claimed more than it carried.
			return nil, errChunkFormat
		}
		out = append(out, chunk...)

		// The CRLF that closes the chunk.
		if _, err := br.Discard(2); err != nil {
			return nil, errChunkFormat
		}
	}
}
