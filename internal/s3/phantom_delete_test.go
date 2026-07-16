package s3

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

func newObjTestServer(t *testing.T) (*Handler, *metadata.Store, storage.Engine, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	engine, err := storage.NewFileSystem(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	auth := NewAuthenticator(testAccessKey, testSecretKey, store, nil, nil)
	handler := NewHandler(store, engine, auth, false, "", nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return handler, store, engine, ts
}

// TestNoPhantomHeadOrGetWhenMetadataGone is a regression test for issue #34:
// metadata is the source of truth. When an object's metadata is gone (a delete
// replicates cluster-wide via Raft) but a data file lingers on a node (an orphan
// left by a past ring/primary change), HEAD and GET must return 404 — not a
// phantom 200 with null Last-Modified/ETag served from the engine.
func TestNoPhantomHeadOrGetWhenMetadataGone(t *testing.T) {
	_, store, engine, ts := newObjTestServer(t)
	bucket, key := "b", "some/prefix/check.parquet"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	// Write an object, confirm it's healthy (HEAD has an ETag).
	if resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte("parquet-bytes")); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: status %d", resp.StatusCode)
	}
	resp := doSigned(t, http.MethodHead, ts.URL+"/"+bucket+"/"+key, nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("ETag") == "" {
		t.Fatalf("healthy HEAD: status %d etag %q", resp.StatusCode, resp.Header.Get("ETag"))
	}
	resp.Body.Close()

	// Simulate the cluster state after a delete: metadata gone (as Raft would
	// replicate) but the data file lingers on this node.
	if err := store.DeleteObjectMeta(bucket, key); err != nil {
		t.Fatalf("delete meta: %v", err)
	}
	if !engine.ObjectExists(bucket, key) {
		t.Fatal("precondition: the orphan data file should still be on disk")
	}

	// HEAD must be 404, not a phantom 200.
	resp = doSigned(t, http.MethodHead, ts.URL+"/"+bucket+"/"+key, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("phantom HEAD returned %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// GET must be 404, not phantom bytes.
	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("phantom GET returned %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestDeleteInvokesReplicaReaper covers issue #34 layer 2: a successful object
// delete triggers the cluster reaper so orphan copies on other nodes are removed.
func TestDeleteInvokesReplicaReaper(t *testing.T) {
	handler, store, _, ts := newObjTestServer(t)
	reaped := make(chan [2]string, 1)
	handler.SetReplicaReaper(func(b, k string) { reaped <- [2]string{b, k} })

	bucket, key := "b", "dir/obj.bin"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte("x")); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: status %d", resp.StatusCode)
	}
	if resp := doSigned(t, http.MethodDelete, ts.URL+"/"+bucket+"/"+key, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: status %d", resp.StatusCode)
	}
	select {
	case got := <-reaped:
		if got[0] != bucket || got[1] != key {
			t.Fatalf("reaper called with %v, want [%s %s]", got, bucket, key)
		}
	default:
		t.Fatal("delete did not invoke the replica reaper")
	}
}
