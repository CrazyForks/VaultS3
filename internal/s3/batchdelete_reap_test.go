package s3

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Issue #47 bug A. The multi-object delete (POST /{bucket}?delete) is a
// BUCKET-level request, so a cluster routes it by hash(bucket, "") to one node
// while the metadata it removes is Raft-replicated cluster-wide. That node's
// engine holds only its own share of the keys, so every key whose data lives
// elsewhere was orphaned: invisible to S3 (metadata is authoritative) and
// unreachable by any API. On an N-node cluster that leaked (N-1)/N of every
// bulk-deleted byte, which is how a delete-heavy Spark workload reached ~9x its
// logical size. These tests pin the reap broadcast that the single-object DELETE
// path always had and this one did not.

func batchDeleteBody(keys ...string) []byte {
	var b strings.Builder
	b.WriteString(`<Delete>`)
	for _, k := range keys {
		fmt.Fprintf(&b, "<Object><Key>%s</Key></Object>", k)
	}
	b.WriteString(`</Delete>`)
	return []byte(b.String())
}

func TestBatchDeleteReapsEveryDeletedKeyClusterWide(t *testing.T) {
	handler, store, _, ts := newObjTestServer(t)

	var batches [][]string
	handler.SetReplicaReaperBatch(func(_ string, keys []string) {
		batches = append(batches, keys)
	})

	bucket := "b"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	keys := []string{"a.parquet", "nested/b.parquet", "nested/deep/c.parquet"}
	for _, k := range keys {
		if resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+k, []byte("x")); resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT %s: status %d", k, resp.StatusCode)
		}
	}

	resp := doSigned(t, http.MethodPost, ts.URL+"/"+bucket+"?delete", batchDeleteBody(keys...))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch delete: status %d", resp.StatusCode)
	}
	var result deleteResult
	if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	resp.Body.Close()
	if len(result.Errors) != 0 {
		t.Fatalf("batch delete reported errors: %+v", result.Errors)
	}

	// One broadcast carrying every key, not one broadcast per key: a Spark job
	// deletes up to 1000 keys at a time and per-key fan-out would be peers*keys
	// separate requests.
	if len(batches) != 1 {
		t.Fatalf("got %d reap broadcasts, want exactly 1 batched call", len(batches))
	}
	got := map[string]bool{}
	for _, k := range batches[0] {
		got[k] = true
	}
	for _, k := range keys {
		if !got[k] {
			t.Errorf("key %q was deleted but never reaped on the other nodes, so its data leaks", k)
		}
	}
}

// A key that failed to delete must NOT be reaped: reaping it would destroy data
// on other nodes while its metadata is still live, turning a leak into data loss.
func TestBatchDeleteDoesNotReapKeysItRejected(t *testing.T) {
	handler, store, _, ts := newObjTestServer(t)

	var reaped []string
	handler.SetReplicaReaperBatch(func(_ string, keys []string) {
		reaped = append(reaped, keys...)
	})

	bucket := "b"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/ok.bin", []byte("x")); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: status %d", resp.StatusCode)
	}

	// "../escape" is rejected as path traversal before any delete happens.
	resp := doSigned(t, http.MethodPost, ts.URL+"/"+bucket+"?delete", batchDeleteBody("ok.bin", "../escape"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch delete: status %d", resp.StatusCode)
	}
	resp.Body.Close()

	for _, k := range reaped {
		if strings.Contains(k, "..") {
			t.Fatalf("reaped rejected key %q, which would delete data on other nodes", k)
		}
	}
	if len(reaped) != 1 || reaped[0] != "ok.bin" {
		t.Fatalf("reaped %v, want exactly [ok.bin]", reaped)
	}
}

// Without the batch hook wired (single-node, or an older wiring) the handler must
// still reap through the single-key reaper rather than silently skipping it.
func TestBatchDeleteFallsBackToSingleKeyReaper(t *testing.T) {
	handler, store, _, ts := newObjTestServer(t)

	var reaped []string
	handler.SetReplicaReaper(func(_, key, _ string) { reaped = append(reaped, key) })

	bucket := "b"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	for _, k := range []string{"x.bin", "y.bin"} {
		if resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+k, []byte("x")); resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT %s: status %d", k, resp.StatusCode)
		}
	}

	resp := doSigned(t, http.MethodPost, ts.URL+"/"+bucket+"?delete", batchDeleteBody("x.bin", "y.bin"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch delete: status %d", resp.StatusCode)
	}
	resp.Body.Close()

	if len(reaped) != 2 {
		t.Fatalf("single-key reaper called for %v, want both keys", reaped)
	}
}

// The versioned delete paths delete data too, so they carry the version through
// to the reaper; without it the peers would delete the wrong thing (the plain
// object) or nothing at all.
func TestVersionDeleteReapsThatVersion(t *testing.T) {
	handler, store, _, ts := newObjTestServer(t)

	type reap struct{ key, version string }
	var reaped []reap
	handler.SetReplicaReaper(func(_, key, version string) {
		reaped = append(reaped, reap{key, version})
	})

	bucket, key := "b", "v.bin"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := store.SetBucketVersioning(bucket, "Enabled"); err != nil {
		t.Fatalf("enable versioning: %v", err)
	}
	if resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte("x")); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: status %d", resp.StatusCode)
	}
	meta, err := store.GetObjectMeta(bucket, key)
	if err != nil {
		t.Fatalf("get meta: %v", err)
	}
	if meta.VersionID == "" {
		t.Skip("versioning did not assign a version id in this build")
	}

	resp := doSigned(t, http.MethodDelete, ts.URL+"/"+bucket+"/"+key+"?versionId="+meta.VersionID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("version delete: status %d", resp.StatusCode)
	}
	resp.Body.Close()

	if len(reaped) != 1 {
		t.Fatalf("version delete produced %d reaps, want 1", len(reaped))
	}
	if reaped[0].version != meta.VersionID {
		t.Fatalf("reaped version %q, want %q", reaped[0].version, meta.VersionID)
	}
}
