package metadata

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func newListStore(t testing.TB) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func putObj(t *testing.T, s *Store, bucket, key string) {
	t.Helper()
	if err := s.PutObjectMeta(ObjectMeta{Bucket: bucket, Key: key, ETag: "e", Size: 1}); err != nil {
		t.Fatalf("PutObjectMeta %s: %v", key, err)
	}
}

// TestListLatestObjectsPagination verifies seek-based continuation: pages are
// returned in key order, truncation is flagged correctly, and the continuation
// marker resumes exactly where the previous page ended.
func TestListLatestObjectsPagination(t *testing.T) {
	s := newListStore(t)
	s.CreateBucket("b")
	for i := 0; i < 25; i++ {
		putObj(t, s, "b", fmt.Sprintf("k-%02d", i))
	}

	p1, trunc1, err := s.ListLatestObjects("b", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1) != 10 || !trunc1 || p1[0].Key != "k-00" || p1[9].Key != "k-09" {
		t.Fatalf("page1: len=%d trunc=%v first=%s last=%s", len(p1), trunc1, p1[0].Key, p1[9].Key)
	}

	p2, trunc2, _ := s.ListLatestObjects("b", "", p1[9].Key, 10)
	if len(p2) != 10 || !trunc2 || p2[0].Key != "k-10" {
		t.Fatalf("page2: len=%d trunc=%v first=%s", len(p2), trunc2, p2[0].Key)
	}

	p3, trunc3, _ := s.ListLatestObjects("b", "", p2[9].Key, 10)
	if len(p3) != 5 || trunc3 || p3[4].Key != "k-24" {
		t.Fatalf("page3 (last): len=%d trunc=%v last=%s", len(p3), trunc3, p3[len(p3)-1].Key)
	}
}

func TestListLatestObjectsPrefix(t *testing.T) {
	s := newListStore(t)
	s.CreateBucket("b")
	for _, k := range []string{"a/1", "a/2", "b/1", "c/1"} {
		putObj(t, s, "b", k)
	}
	got, _, _ := s.ListLatestObjects("b", "a/", "", 100)
	if len(got) != 2 || got[0].Key != "a/1" || got[1].Key != "a/2" {
		t.Fatalf("prefix filter: %+v", got)
	}
}

func TestListLatestObjectsSkipsDeleteMarkers(t *testing.T) {
	s := newListStore(t)
	s.CreateBucket("b")
	putObj(t, s, "b", "live")
	if err := s.PutObjectMeta(ObjectMeta{Bucket: "b", Key: "gone", DeleteMarker: true}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.ListLatestObjects("b", "", "", 100)
	if len(got) != 1 || got[0].Key != "live" {
		t.Fatalf("delete marker not skipped: %+v", got)
	}
}

// TestListLatestObjectsBucketIsolation guards the shared key space: bucket "foo"
// must not leak objects from "foobar" (which shares the "foo" string prefix).
func TestListLatestObjectsBucketIsolation(t *testing.T) {
	s := newListStore(t)
	s.CreateBucket("foo")
	s.CreateBucket("foobar")
	putObj(t, s, "foo", "x")
	putObj(t, s, "foobar", "y")
	got, _, _ := s.ListLatestObjects("foo", "", "", 100)
	if len(got) != 1 || got[0].Key != "x" {
		t.Fatalf("bucket isolation broken: %+v", got)
	}
}

// TestListLatestObjectsDeepOffset confirms a page deep in a large bucket is
// fetched correctly (seek lands on the right key, not the front).
func TestListLatestObjectsDeepOffset(t *testing.T) {
	s := newListStore(t)
	s.CreateBucket("b")
	for i := 0; i < 1000; i++ {
		putObj(t, s, "b", fmt.Sprintf("obj-%04d", i))
	}
	got, trunc, _ := s.ListLatestObjects("b", "", "obj-0500", 5)
	if len(got) != 5 || !trunc || got[0].Key != "obj-0501" || got[4].Key != "obj-0505" {
		t.Fatalf("deep offset: len=%d trunc=%v first=%s last=%s", len(got), trunc, got[0].Key, got[len(got)-1].Key)
	}
}

// BenchmarkListLatestObjectsPage measures one page (1000 keys) fetched mid-bucket
// at increasing total object counts. With seek-based pagination the per-page cost
// is ~flat (O(log n + pageSize)); the old read-all-then-sort grew with N.
func BenchmarkListLatestObjectsPage(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000, 1_000_000} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			s := newListStore(b)
			s.CreateBucket("bench")
			seedBench(b, s, "bench", n)

			startAfter := fmt.Sprintf("obj-%09d", n/2) // page from the middle
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				objs, _, err := s.ListLatestObjects("bench", "", startAfter, 1000)
				if err != nil || len(objs) == 0 {
					b.Fatalf("list: err=%v len=%d", err, len(objs))
				}
			}
		})
	}
}

// seedBench bulk-inserts n object-meta rows in bounded transactions.
func seedBench(b *testing.B, s *Store, bucket string, n int) {
	b.Helper()
	const chunk = 50_000
	for off := 0; off < n; off += chunk {
		end := off + chunk
		if end > n {
			end = n
		}
		err := s.db.Update(func(tx *bolt.Tx) error {
			ob := tx.Bucket(objectsBucket)
			for i := off; i < end; i++ {
				key := fmt.Sprintf("obj-%09d", i)
				data, _ := json.Marshal(ObjectMeta{Bucket: bucket, Key: key, ETag: "e", Size: 1})
				if err := ob.Put(objectMetaKey(bucket, key), data); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			b.Fatalf("seed: %v", err)
		}
	}
}
