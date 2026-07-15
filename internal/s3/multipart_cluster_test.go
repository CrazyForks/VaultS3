package s3

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// TestMultipartUsesLocalMetadataStore is a regression test for issue #32:
// in-progress multipart upload metadata must live on the node-local store, not
// the Raft-replicated one, so a part uploaded right after create is found (the
// replicated read lagged the forwarded write and returned 404 NoSuchUpload under
// concurrency). The final assembled object must still land on the replicated
// store so it is visible cluster-wide.
func TestMultipartUsesLocalMetadataStore(t *testing.T) {
	dir := t.TempDir()
	// mainStore stands in for the Raft-replicated store; localStore is node-local.
	mainStore, err := metadata.NewStore(filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatalf("main store: %v", err)
	}
	localStore, err := metadata.NewStore(filepath.Join(dir, "local.db"))
	if err != nil {
		t.Fatalf("local store: %v", err)
	}
	t.Cleanup(func() { mainStore.Close(); localStore.Close() })

	engine, err := storage.NewFileSystem(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	auth := NewAuthenticator(testAccessKey, testSecretKey, mainStore, nil, nil)
	handler := NewHandler(mainStore, engine, auth, false, "", nil)
	handler.SetLocalMultipartStore(localStore)

	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	bucket, key := "mpbucket", "data/0001/part-0.parquet"
	if err := mainStore.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	// CreateMultipartUpload
	resp := doSigned(t, http.MethodPost, ts.URL+"/"+bucket+"/"+key+"?uploads", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CreateMultipartUpload: status %d", resp.StatusCode)
	}
	var init initiateResult
	if err := xml.NewDecoder(resp.Body).Decode(&init); err != nil {
		t.Fatalf("decode initiate: %v", err)
	}
	resp.Body.Close()
	uploadID := init.UploadID
	if uploadID == "" {
		t.Fatal("empty uploadId")
	}

	// The upload metadata must be on the LOCAL store, and must NOT have gone to the
	// replicated store (whose read lag caused the 404).
	if _, err := localStore.GetMultipartUpload(uploadID); err != nil {
		t.Fatalf("multipart upload not recorded on the local store: %v", err)
	}
	if _, err := mainStore.GetMultipartUpload(uploadID); err == nil {
		t.Fatal("multipart upload leaked onto the replicated store (the source of the #32 lag)")
	}

	// UploadPart must succeed — this is exactly where the bug returned 404.
	resp = doSigned(t, http.MethodPut,
		fmt.Sprintf("%s/%s/%s?uploadId=%s&partNumber=1", ts.URL, bucket, key, uploadID),
		[]byte("hello-parquet-part"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("UploadPart: status %d (404 here is the #32 bug)", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	resp.Body.Close()

	// CompleteMultipartUpload
	completeXML := fmt.Sprintf(
		`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, etag)
	resp = doSigned(t, http.MethodPost,
		fmt.Sprintf("%s/%s/%s?uploadId=%s", ts.URL, bucket, key, uploadID),
		[]byte(completeXML))
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CompleteMultipartUpload: status %d: %s", resp.StatusCode, body)
	}

	// The final assembled object must land on the replicated (main) store so the
	// whole cluster can see it.
	if _, err := mainStore.GetObjectMeta(bucket, key); err != nil {
		t.Fatalf("completed object not on the replicated store: %v", err)
	}
	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil)
	if got := readBody(t, resp); got != "hello-parquet-part" {
		t.Fatalf("object content = %q, want hello-parquet-part", got)
	}
}
