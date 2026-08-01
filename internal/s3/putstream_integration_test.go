package s3

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"hash/crc32"
	"io"
	"net/http"
	"testing"
)

// A streamed upload validates after the bytes are written, so the rejection path
// has to leave the store exactly as it found it: the client must still see the
// same error, and the object must not become readable (issue #46).
func TestRejectedUploadIsNotServed(t *testing.T) {
	ts := newIntegrationServer(t)
	body := []byte("the payload that will actually be sent")
	wrong := sha256.Sum256([]byte("a completely different payload"))

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/rejects", nil)
	signV4Request(req, testAccessKey, testSecretKey, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	resp.Body.Close()

	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/rejects/bad", bytes.NewReader(body))
	req.Header.Set("X-Amz-Checksum-Sha256", base64.StdEncoding.EncodeToString(wrong[:]))
	req.ContentLength = int64(len(body))
	signV4Request(req, testAccessKey, testSecretKey, body)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, out)
	}
	if !bytes.Contains(out, []byte("BadDigest")) {
		t.Errorf("expected a BadDigest error, got %s", out)
	}

	// The object must not be retrievable, listed, or headable.
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/rejects/bad", nil)
	signV4Request(req, testAccessKey, testSecretKey, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("rejected upload is readable: GET returned %d, want 404", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/rejects", nil)
	signV4Request(req, testAccessKey, testSecretKey, nil)
	resp, _ = http.DefaultClient.Do(req)
	listing, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if bytes.Contains(listing, []byte("<Key>bad</Key>")) {
		t.Errorf("rejected upload appears in the listing: %s", listing)
	}
}

// The happy path has to survive streaming too: a correct checksum is accepted,
// the bytes come back intact, and the recorded checksum is returned on GET.
func TestStreamedUploadRoundTrips(t *testing.T) {
	ts := newIntegrationServer(t)
	// Large enough to cross many Read boundaries rather than land in one buffer.
	body := bytes.Repeat([]byte("vaults3-streaming-payload!"), 200000) // ~5 MB
	sum := sha256.Sum256(body)
	sumB64 := base64.StdEncoding.EncodeToString(sum[:])
	md5sum := md5.Sum(body)

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/streamed", nil)
	signV4Request(req, testAccessKey, testSecretKey, nil)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/streamed/big", bytes.NewReader(body))
	req.Header.Set("X-Amz-Checksum-Sha256", sumB64)
	req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(md5sum[:]))
	req.Header.Set("X-Amz-Checksum-Crc32", crcB64(crc32.ChecksumIEEE(body)))
	req.ContentLength = int64(len(body))
	signV4Request(req, testAccessKey, testSecretKey, body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, out)
	}

	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/streamed/big", nil)
	signV4Request(req, testAccessKey, testSecretKey, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !bytes.Equal(got, body) {
		t.Fatalf("round trip corrupted the object: got %d bytes, want %d", len(got), len(body))
	}
	if h := resp.Header.Get("X-Amz-Checksum-Sha256"); h != sumB64 {
		t.Errorf("checksum header = %q, want %q", h, sumB64)
	}
}
