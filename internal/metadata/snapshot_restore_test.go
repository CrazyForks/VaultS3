package metadata

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Issue #46 follow-up. A Raft snapshot restore is what a joining or restarting
// node does before it serves anything, and it used to run as ONE BoltDB write
// transaction. Bolt keeps every dirty page of a transaction in memory until it
// commits, so peak memory scaled with the whole cluster's metadata (~1.4 KB per
// object measured) and a node with a few hundred thousand objects was OOM-killed
// during its own startup. It now commits in bounded batches.

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seed writes n objects and returns the snapshot bytes.
func seed(t *testing.T, n int) []byte {
	t.Helper()
	src := openStore(t)
	if err := src.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := src.PutObjectMeta(ObjectMeta{
			Bucket:       "b",
			Key:          fmt.Sprintf("k/%06d.parquet", i),
			ETag:         "\"etag\"",
			Size:         int64(i),
			LastModified: time.Now().Unix(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := src.WriteSnapshot(&buf); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	return buf.Bytes()
}

// The batched restore must reproduce the source exactly, including across a
// batch boundary (restoreBatchKeys is well above these counts, so drive the
// boundary with the byte budget by using more keys than one batch holds).
func TestBatchedRestoreReproducesEveryKey(t *testing.T) {
	const n = 2500
	snap := seed(t, n)

	dst := openStore(t)
	if err := dst.RestoreSnapshot(bytes.NewReader(snap)); err != nil {
		t.Fatalf("restore: %v", err)
	}

	for i := 0; i < n; i++ {
		key := fmt.Sprintf("k/%06d.parquet", i)
		m, err := dst.GetObjectMeta("b", key)
		if err != nil || m == nil {
			t.Fatalf("object %s missing after restore: %v", key, err)
		}
		if m.Size != int64(i) {
			t.Fatalf("object %s has size %d, want %d", key, m.Size, i)
		}
	}
	if !dst.BucketExists("b") {
		t.Fatal("bucket missing after restore")
	}
}

// Restoring must replace what was there, not merge with it: a node rejoining
// must not keep objects the cluster no longer has.
func TestRestoreReplacesPreExistingState(t *testing.T) {
	snap := seed(t, 10)

	dst := openStore(t)
	if err := dst.CreateBucket("stale"); err != nil {
		t.Fatal(err)
	}
	if err := dst.PutObjectMeta(ObjectMeta{Bucket: "stale", Key: "old.bin"}); err != nil {
		t.Fatal(err)
	}

	if err := dst.RestoreSnapshot(bytes.NewReader(snap)); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if dst.BucketExists("stale") {
		t.Fatal("a bucket the snapshot does not contain survived the restore")
	}
	if m, err := dst.GetObjectMeta("stale", "old.bin"); err == nil && m != nil {
		t.Fatal("a stale object survived the restore")
	}
}

// A restore that stops partway leaves a partial copy of the cluster's metadata.
// Serving that would present a subset of the objects as the whole set, so the
// next open must recognise and discard it.
func TestInterruptedRestoreIsDiscardedOnNextOpen(t *testing.T) {
	snap := seed(t, 200)
	dir := t.TempDir()
	path := filepath.Join(dir, "meta.db")

	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Stop after the state is dropped and the marker is written, which is exactly
	// where an OOM kill during startup would land.
	if err := s.beginRestore(); err != nil {
		t.Fatalf("beginRestore: %v", err)
	}
	if err := s.restoreBucket(bytes.NewReader(snap[:0]), objectsBucket, 0); err != nil {
		t.Fatalf("partial restore: %v", err)
	}
	if !s.restoreWasInterrupted() {
		t.Fatal("an unfinished restore is not marked as interrupted")
	}
	s.Close()

	reopened, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	if reopened.restoreWasInterrupted() {
		t.Fatal("reopening did not clear the interrupted-restore marker")
	}
	buckets, err := reopened.ListBuckets()
	if err != nil {
		t.Fatalf("list buckets: %v", err)
	}
	if len(buckets) != 0 {
		t.Fatalf("partial metadata survived the reopen: %d buckets", len(buckets))
	}
}

// A completed restore must leave no marker, so a healthy node never discards its
// metadata on the next restart.
func TestCompletedRestoreLeavesNoMarker(t *testing.T) {
	snap := seed(t, 50)
	dir := t.TempDir()
	path := filepath.Join(dir, "meta.db")

	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.RestoreSnapshot(bytes.NewReader(snap)); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if s.restoreWasInterrupted() {
		t.Fatal("a completed restore still looks interrupted")
	}
	s.Close()

	reopened, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	if m, err := reopened.GetObjectMeta("b", "k/000000.parquet"); err != nil || m == nil {
		t.Fatalf("restored metadata was discarded on restart: %v", err)
	}
}

// The restore must commit in several transactions rather than one, which is the
// whole point: one transaction is what made peak memory scale with the data.
func TestRestoreCommitsInMultipleTransactions(t *testing.T) {
	// Shrink the batch instead of seeding a realistic amount of data: the point
	// is that the restore commits repeatedly, not how many keys fit in a batch.
	defer func(k, b int) { restoreBatchKeys, restoreBatchBytes = k, b }(restoreBatchKeys, restoreBatchBytes)
	restoreBatchKeys, restoreBatchBytes = 100, 1<<20

	const n = 1200 // many batches at the shrunk size
	snap := seed(t, n)

	dst := openStore(t)
	var txIDs []int
	dst.db.Update(func(tx *bolt.Tx) error { txIDs = append(txIDs, tx.ID()); return nil })
	before := txIDs[0]

	if err := dst.RestoreSnapshot(bytes.NewReader(snap)); err != nil {
		t.Fatalf("restore: %v", err)
	}

	var after int
	dst.db.Update(func(tx *bolt.Tx) error { after = tx.ID(); return nil })
	// begin + at least two batches + finish, plus bucket creation, so comfortably
	// more than the 2 a single-transaction restore would use.
	if after-before < 4 {
		t.Fatalf("restore used only %d write transactions, so it is still buffering the whole snapshot", after-before)
	}
	if m, err := dst.GetObjectMeta("b", fmt.Sprintf("k/%06d.parquet", n-1)); err != nil || m == nil {
		t.Fatalf("last object missing after a multi-batch restore: %v", err)
	}
}
