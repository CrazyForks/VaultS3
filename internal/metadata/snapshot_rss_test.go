package metadata

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// Issue #46 follow-up, measurement rig.
//
// Peak RSS is what decides whether a container gets OOM-killed, and it is the
// only honest way to tell whether batching the snapshot restore helped.
// runtime.HeapAlloc is not: it counts allocated-but-not-yet-collected objects,
// so garbage that GC has not swept yet inflates the peak and hides the
// difference. (Reading the wrong memory counter is exactly what made the
// reporter's own numbers misleading.)
//
// The restore must be measured in a process that does nothing else, otherwise
// seeding the fixture dominates the figure. So this is two env-gated steps:
//
//	VAULTS3_SNAP_OUT=/tmp/snap.bin VAULTS3_SNAP_OBJECTS=200000 \
//	  go test ./internal/metadata/ -run TestMakeSnapshotFixture
//	VAULTS3_SNAP_IN=/tmp/snap.bin \
//	  go test ./internal/metadata/ -run TestRestorePeakRSS -v

func TestMakeSnapshotFixture(t *testing.T) {
	out := os.Getenv("VAULTS3_SNAP_OUT")
	if out == "" {
		t.Skip("set VAULTS3_SNAP_OUT to build a snapshot fixture")
	}
	n := 200000
	if v := os.Getenv("VAULTS3_SNAP_OBJECTS"); v != "" {
		fmt.Sscanf(v, "%d", &n)
	}

	src, err := NewStore(filepath.Join(t.TempDir(), "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err := src.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}

	// Seed through one transaction per chunk. PutObjectMeta commits per call,
	// which would take many minutes at this size and is not what is being measured.
	const chunk = 20000
	for start := 0; start < n; start += chunk {
		end := start + chunk
		if end > n {
			end = n
		}
		if err := src.db.Update(func(tx *bolt.Tx) error {
			ob := tx.Bucket(objectsBucket)
			for i := start; i < end; i++ {
				m := ObjectMeta{
					Bucket:       "b",
					Key:          fmt.Sprintf("warehouse/dt=2026-08-08/part-%08d.snappy.parquet", i),
					ETag:         "\"d41d8cd98f00b204e9800998ecf8427e\"",
					Size:         1 << 20,
					ContentType:  "application/octet-stream",
					LastModified: 1754600000,
				}
				data, err := json.Marshal(m)
				if err != nil {
					return err
				}
				if err := ob.Put([]byte("b/"+m.Key), data); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := src.WriteSnapshot(f); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	fi, _ := f.Stat()
	t.Logf("fixture: %d objects, snapshot %d MiB -> %s", n, fi.Size()/(1<<20), out)
}

func TestRestorePeakRSS(t *testing.T) {
	in := os.Getenv("VAULTS3_SNAP_IN")
	if in == "" {
		t.Skip("set VAULTS3_SNAP_IN to measure restore peak RSS")
	}

	f, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()

	dst, err := NewStore(filepath.Join(t.TempDir(), "dst.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	baseline := maxRSSBytes()
	if err := dst.RestoreSnapshot(f); err != nil {
		t.Fatalf("restore: %v", err)
	}
	peak := maxRSSBytes()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	const MiB = 1 << 20
	t.Logf("snapshot %d MiB | peak RSS %d MiB (baseline before restore %d MiB) | HeapSys %d MiB",
		fi.Size()/MiB, peak/MiB, baseline/MiB, ms.HeapSys/MiB)
}

// maxRSSBytes returns the process's peak resident set size in bytes. Linux
// reports kilobytes, darwin reports bytes.
func maxRSSBytes() uint64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if runtime.GOOS == "linux" {
		return uint64(ru.Maxrss) * 1024
	}
	return uint64(ru.Maxrss)
}
