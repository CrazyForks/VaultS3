package metadata

import "testing"

// TestBucketStatsCounter verifies the per-bucket counters are maintained
// incrementally through put / overwrite / delete-marker / delete — the fix for
// issue #16 (stats pages re-walking every object on every request).
func TestBucketStatsCounter(t *testing.T) {
	s := newTestStore(t)

	// Before backfill there is no baseline, so writes don't fabricate a partial
	// counter (which would undercount pre-existing data).
	if err := s.PutObjectMeta(ObjectMeta{Bucket: "b", Key: "k0", Size: 100}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.BucketStats("b"); ok {
		t.Fatal("no counter should exist before backfill")
	}

	// Backfill seeds the baseline (as a one-time filesystem walk would).
	if err := s.SetBucketStats("b", BucketStat{Size: 100, Count: 1}); err != nil {
		t.Fatal(err)
	}

	check := func(when string, wantCount, wantSize int64) {
		t.Helper()
		st, ok, err := s.BucketStats("b")
		if err != nil || !ok {
			t.Fatalf("%s: BucketStats ok=%v err=%v", when, ok, err)
		}
		if st.Count != wantCount || st.Size != wantSize {
			t.Fatalf("%s: got count=%d size=%d, want count=%d size=%d", when, st.Count, st.Size, wantCount, wantSize)
		}
	}

	s.PutObjectMeta(ObjectMeta{Bucket: "b", Key: "k1", Size: 50}) // new object
	check("after put", 2, 150)

	s.PutObjectMeta(ObjectMeta{Bucket: "b", Key: "k1", Size: 70}) // overwrite 50->70
	check("after overwrite", 2, 170)

	s.PutObjectMeta(ObjectMeta{Bucket: "b", Key: "k1", DeleteMarker: true}) // delete marker
	check("after delete marker", 1, 100)

	s.DeleteObjectMeta("b", "k0") // hard delete
	check("after delete", 0, 0)

	// A second bucket is independent.
	s.SetBucketStats("c", BucketStat{})
	s.PutObjectMeta(ObjectMeta{Bucket: "c", Key: "x", Size: 9})
	if st, _, _ := s.BucketStats("c"); st.Count != 1 || st.Size != 9 {
		t.Fatalf("bucket c isolation: %+v", st)
	}
	check("bucket b unchanged", 0, 0)
}

// TestBackfillBucketStatsFromMetadata verifies the atomic backfill computes size
// and count from the metadata index and skips delete markers.
func TestBackfillBucketStatsFromMetadata(t *testing.T) {
	s := newTestStore(t)
	s.PutObjectMeta(ObjectMeta{Bucket: "vb", Key: "a", Size: 100})
	s.PutObjectMeta(ObjectMeta{Bucket: "vb", Key: "b", Size: 250})
	s.PutObjectMeta(ObjectMeta{Bucket: "vb", Key: "c", DeleteMarker: true}) // not counted

	st, err := s.BackfillBucketStats("vb")
	if err != nil {
		t.Fatal(err)
	}
	if st.Count != 2 || st.Size != 350 {
		t.Fatalf("backfill: got count=%d size=%d, want 2/350", st.Count, st.Size)
	}
	// And it seeded the cached counter.
	if got, ok, _ := s.BucketStats("vb"); !ok || got.Size != 350 || got.Count != 2 {
		t.Fatalf("seeded counter: ok=%v %+v", ok, got)
	}
}

// TestVersionPointerStatsDelta verifies that repointing the latest version (via
// SetLatestVersion and UpdateObjectVersionMeta) adjusts the cached bucket counter,
// which previously it bypassed, causing permanent drift on version delete/promote.
func TestVersionPointerStatsDelta(t *testing.T) {
	s := newTestStore(t)
	s.PutObjectMeta(ObjectMeta{Bucket: "vb", Key: "a", Size: 100})
	s.SetBucketStats("vb", BucketStat{Size: 100, Count: 1})

	// Two versions of "a" recorded in the versions bucket (no counter effect).
	s.PutObjectVersion(ObjectMeta{Bucket: "vb", Key: "a", VersionID: "v2", Size: 500})
	s.PutObjectVersion(ObjectMeta{Bucket: "vb", Key: "a", VersionID: "v1", Size: 100})

	// Repoint latest to v2 (500 bytes): counter must reflect +400.
	if err := s.SetLatestVersion("vb", "a", "v2"); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := s.BucketStats("vb"); st.Size != 500 || st.Count != 1 {
		t.Fatalf("after SetLatestVersion v2: got size=%d count=%d, want 500/1", st.Size, st.Count)
	}

	// Promote v1 (100 bytes) as latest: counter must go back to 100.
	if err := s.UpdateObjectVersionMeta(ObjectMeta{Bucket: "vb", Key: "a", VersionID: "v1", Size: 100, IsLatest: true}); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := s.BucketStats("vb"); st.Size != 100 {
		t.Fatalf("after promote v1: got size=%d, want 100", st.Size)
	}
}
