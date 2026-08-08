package metadata

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Issue #46 follow-up: a node OOM-killed during its own startup/join phase,
// before any load. Restoring a Raft snapshot is the heavy allocation on that
// path — a joining or restarting node replaces its whole metadata DB from the
// leader's snapshot — and RestoreSnapshot does it in a SINGLE BoltDB write
// transaction. Bolt holds every dirty page in memory until commit, so peak
// memory scales with the total size of the metadata rather than staying flat.
//
// This measures that scaling. It is a diagnostic rather than a pass/fail guard:
// run with -v to see the numbers.
func TestSnapshotRestoreMemoryScaling(t *testing.T) {
	// Opt-in: seeding and restoring enough objects to show the trend takes
	// minutes, which does not belong in every `go test ./...`.
	if os.Getenv("VAULTS3_MEM_SCALING") == "" {
		t.Skip("set VAULTS3_MEM_SCALING=1 to run the snapshot-restore memory scaling measurement")
	}
	for _, objects := range []int{5_000, 20_000, 80_000} {
		t.Run(fmt.Sprintf("objects=%d", objects), func(t *testing.T) {
			src := newStoreAt(t, filepath.Join(t.TempDir(), "src.db"))
			if err := src.CreateBucket("b"); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < objects; i++ {
				if err := src.PutObjectMeta(ObjectMeta{
					Bucket:       "b",
					Key:          fmt.Sprintf("prefix/%08d/part-of-a-realistic-length-key.parquet", i),
					ETag:         "\"d41d8cd98f00b204e9800998ecf8427e\"",
					Size:         1 << 20,
					LastModified: time.Now().Unix(),
					ContentType:  "application/octet-stream",
				}); err != nil {
					t.Fatal(err)
				}
			}

			var snap bytes.Buffer
			if err := src.WriteSnapshot(&snap); err != nil {
				t.Fatalf("write snapshot: %v", err)
			}
			src.Close()
			snapBytes := snap.Len()

			dst := newStoreAt(t, filepath.Join(t.TempDir(), "dst.db"))
			defer dst.Close()

			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)

			done := make(chan struct{})
			var peak uint64
			go func() {
				var m runtime.MemStats
				for {
					select {
					case <-done:
						return
					default:
					}
					runtime.ReadMemStats(&m)
					if m.HeapAlloc > peak {
						peak = m.HeapAlloc
					}
					time.Sleep(time.Millisecond)
				}
			}()

			if err := dst.RestoreSnapshot(bytes.NewReader(snap.Bytes())); err != nil {
				close(done)
				t.Fatalf("restore: %v", err)
			}
			close(done)

			const MiB = 1 << 20
			t.Logf("objects=%d snapshot=%d MiB peakHeapDuringRestore=%d MiB (%.1f bytes/object)",
				objects, snapBytes/MiB, peak/MiB, float64(peak)/float64(objects))
		})
	}
}

func newStoreAt(t *testing.T, path string) *Store {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}
