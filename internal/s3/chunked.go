package s3

import (
	"io"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
)

// maybeDecodeAwsChunked transparently decodes an `aws-chunked` request body.
//
// Modern AWS SDKs (boto3/botocore 1.36+, aws-sdk-js v3, aws-cli) default to
// "flexible checksums" and, when the transport supports it (notably HTTP/2, which
// Go negotiates for any TLS listener), stream the body using the AWS chunked
// content-encoding: the payload is framed as `<hex-size>\r\n<data>\r\n…0\r\n` with
// a trailing checksum, and `x-amz-content-sha256` is set to STREAMING-…-PAYLOAD.
//
// Without decoding, the chunk-size headers and trailer get stored verbatim as part
// of the object, corrupting every such upload (e.g. a 100-byte PUT is stored as
// 142 bytes). This strips the framing so handlers read the real object bytes.
//
// SigV4 verification is unaffected: the streaming modes sign the literal
// `STREAMING-…-PAYLOAD` string as the payload hash, so auth never reads the body
// and must run before this is called.
func maybeDecodeAwsChunked(r *http.Request) {
	if r.Body == nil {
		return
	}
	ce := r.Header.Get("Content-Encoding")
	sha := r.Header.Get("X-Amz-Content-Sha256")
	if !strings.Contains(ce, "aws-chunked") && !strings.HasPrefix(sha, "STREAMING-") {
		return
	}

	// httputil.NewChunkedReader decodes `<hex-size>[;ext]\r\n<data>\r\n` chunks and
	// stops at the terminating `0\r\n` — handling both the unsigned-trailer variant
	// (plain hex sizes) and the signed variant (`;chunk-signature=…` extensions),
	// and leaving the trailer unread (we discard it).
	r.Body = io.NopCloser(httputil.NewChunkedReader(r.Body))

	// The decoded object length lives in this header, not Content-Length (which
	// counts the framed bytes). Fix ContentLength so size-based logic — quota,
	// multipart/part-size caps, metrics — sees the true size.
	if dl := r.Header.Get("X-Amz-Decoded-Content-Length"); dl != "" {
		if n, err := strconv.ParseInt(dl, 10, 64); err == nil {
			r.ContentLength = n
			r.Header.Set("Content-Length", dl)
		}
	}
	// Drop the encoding markers so nothing downstream double-decodes.
	r.Header.Del("Content-Encoding")
}
