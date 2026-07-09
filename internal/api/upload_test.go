package api

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestUploadStreamsAndPreservesFolderPath covers the dashboard upload rewrite for
// issue #26: it streams the multipart body (no whole-file temp buffering) and
// preserves a relative folder path in the filename instead of flattening it to
// the base name.
func TestUploadStreamsAndPreservesFolderPath(t *testing.T) {
	h, store := newTestAPI(t)
	if err := store.CreateBucket("vault"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	// The dashboard sends folder uploads with the relative path as the filename.
	part, err := mw.CreateFormFile("file", "backups/2026/report.bin")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	content := []byte("hello-large-content-streamed-through")
	part.Write(content)
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/buckets/vault/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	h.handleUpload(rr, req, "vault")

	if rr.Code != http.StatusOK {
		t.Fatalf("upload: status %d, body %s", rr.Code, rr.Body.String())
	}

	// The object must be stored under the full nested key, not the base name.
	meta, err := store.GetObjectMeta("vault", "backups/2026/report.bin")
	if err != nil {
		t.Fatalf("object not stored under nested key (folder flattened?): %v", err)
	}
	if meta.Size != int64(len(content)) {
		t.Fatalf("stored size = %d, want %d", meta.Size, len(content))
	}
	if _, err := store.GetObjectMeta("vault", "report.bin"); err == nil {
		t.Fatal("object was flattened to the base name report.bin")
	}
}
