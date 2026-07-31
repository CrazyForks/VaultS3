package metadata

import (
	"path/filepath"
	"testing"
)

func newDurabilityStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateBucket("b"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	return s
}

func ptrBool(v bool) *bool { return &v }
func ptrInt(v int) *int    { return &v }

// TestBucketDurabilityInheritsDefaults: a bucket that expresses no preference
// gets the server settings, and says so, so the dashboard can show "inherited"
// rather than implying somebody chose it (issue #39).
func TestBucketDurabilityInheritsDefaults(t *testing.T) {
	s := newDurabilityStore(t)

	d := s.BucketDurability("b", true, 3)
	if !d.ErasureEnabled || d.ReplicaCount != 3 {
		t.Fatalf("got erasure=%v replicas=%d, want the server defaults", d.ErasureEnabled, d.ReplicaCount)
	}
	if d.ErasureExplicit || d.ReplicaExplicit {
		t.Fatal("an untouched bucket must not report its settings as explicit")
	}

	// A different server default flows straight through.
	if d := s.BucketDurability("b", false, 1); d.ErasureEnabled || d.ReplicaCount != 1 {
		t.Fatalf("got erasure=%v replicas=%d, want erasure=false replicas=1", d.ErasureEnabled, d.ReplicaCount)
	}
}

// TestBucketDurabilityOverrides is the feature itself: a bucket can be cheaper or
// safer than the server default, independently on each axis.
func TestBucketDurabilityOverrides(t *testing.T) {
	cases := []struct {
		name         string
		erasure      *bool
		replicas     *int
		wantErasure  bool
		wantReplicas int
	}{
		{"erasure off only", ptrBool(false), nil, false, 3},
		{"replicas down only", nil, ptrInt(1), true, 1},
		{"both down (scratch data)", ptrBool(false), ptrInt(1), false, 1},
		{"replicas above the default", nil, ptrInt(5), true, 5},
		{"erasure on against an off default", ptrBool(true), nil, true, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newDurabilityStore(t)
			if err := s.SetBucketDurability("b", tc.erasure, tc.replicas); err != nil {
				t.Fatalf("SetBucketDurability: %v", err)
			}
			d := s.BucketDurability("b", true, 3)
			if d.ErasureEnabled != tc.wantErasure || d.ReplicaCount != tc.wantReplicas {
				t.Fatalf("got erasure=%v replicas=%d, want erasure=%v replicas=%d",
					d.ErasureEnabled, d.ReplicaCount, tc.wantErasure, tc.wantReplicas)
			}
			if (tc.erasure != nil) != d.ErasureExplicit {
				t.Errorf("erasureExplicit = %v, want %v", d.ErasureExplicit, tc.erasure != nil)
			}
			if (tc.replicas != nil) != d.ReplicaExplicit {
				t.Errorf("replicaExplicit = %v, want %v", d.ReplicaExplicit, tc.replicas != nil)
			}
		})
	}
}

// TestBucketDurabilityClearsBackToDefault: clearing an override has to be
// distinguishable from setting it to off, which is why the fields are pointers.
func TestBucketDurabilityClearsBackToDefault(t *testing.T) {
	s := newDurabilityStore(t)

	if err := s.SetBucketDurability("b", ptrBool(false), ptrInt(1)); err != nil {
		t.Fatalf("SetBucketDurability: %v", err)
	}
	if d := s.BucketDurability("b", true, 3); d.ErasureEnabled || d.ReplicaCount != 1 {
		t.Fatal("precondition: the override should be in force")
	}

	if err := s.SetBucketDurability("b", nil, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	d := s.BucketDurability("b", true, 3)
	if !d.ErasureEnabled || d.ReplicaCount != 3 {
		t.Fatalf("after clearing, got erasure=%v replicas=%d, want the defaults back",
			d.ErasureEnabled, d.ReplicaCount)
	}
	if d.ErasureExplicit || d.ReplicaExplicit {
		t.Error("a cleared override must report as inherited again")
	}
}

// TestBucketDurabilityIgnoresNonsenseReplicaCount guards the write path: a stored
// zero or negative must never collapse placement to "no copies".
func TestBucketDurabilityIgnoresNonsenseReplicaCount(t *testing.T) {
	s := newDurabilityStore(t)
	for _, bad := range []int{0, -1} {
		if err := s.SetBucketDurability("b", nil, ptrInt(bad)); err != nil {
			t.Fatalf("SetBucketDurability(%d): %v", bad, err)
		}
		if d := s.BucketDurability("b", true, 3); d.ReplicaCount != 3 {
			t.Fatalf("replica_count %d should fall back to the default, got %d", bad, d.ReplicaCount)
		}
	}
}

// TestBucketDurabilityUnknownBucket keeps the write path safe: resolving a bucket
// that does not exist yet must not panic or report zero copies.
func TestBucketDurabilityUnknownBucket(t *testing.T) {
	s := newDurabilityStore(t)
	d := s.BucketDurability("no-such-bucket", true, 2)
	if !d.ErasureEnabled || d.ReplicaCount != 2 {
		t.Fatalf("got erasure=%v replicas=%d, want the defaults", d.ErasureEnabled, d.ReplicaCount)
	}
}

// TestBucketDurabilitySurvivesOtherBucketEdits makes sure the setting is not
// clobbered by an unrelated bucket update.
func TestBucketDurabilitySurvivesOtherBucketEdits(t *testing.T) {
	s := newDurabilityStore(t)
	if err := s.SetBucketDurability("b", ptrBool(false), ptrInt(1)); err != nil {
		t.Fatalf("SetBucketDurability: %v", err)
	}
	if err := s.UpdateBucketQuota("b", 1<<30, 100); err != nil {
		t.Fatalf("UpdateBucketQuota: %v", err)
	}
	d := s.BucketDurability("b", true, 3)
	if d.ErasureEnabled || d.ReplicaCount != 1 {
		t.Fatalf("a quota change must not reset durability: erasure=%v replicas=%d",
			d.ErasureEnabled, d.ReplicaCount)
	}
}
