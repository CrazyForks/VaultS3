package metadata

import (
	"testing"
	"time"
)

// fakeRaft is a RaftApplier for the DistributedStore consistent-read tests.
type fakeRaft struct{ leader bool }

func (f *fakeRaft) Apply([]byte) error           { return nil }
func (f *fakeRaft) IsLeader() bool               { return f.leader }
func (f *fakeRaft) ForwardToLeader([]byte) error { return nil }

// TestGetObjectMetaConsistentWaitsForReplication covers the issue #37 read-side
// fix: on a follower, a consistent read of a not-yet-replicated object waits
// (polling the local store) for it to arrive rather than returning a premature
// 404 — and it does so with no inter-node RPC, so it's robust to network/proxy
// topology. A genuine miss still returns not-found after the timeout, and the
// leader (authoritative) never waits.
func TestGetObjectMetaConsistentWaitsForReplication(t *testing.T) {
	old := ReadYourWritesTimeout
	ReadYourWritesTimeout = 800 * time.Millisecond
	defer func() { ReadYourWritesTimeout = old }()

	store := newTestStore(t)
	if err := store.CreateBucket("b"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	// Follower: the object "replicates" ~120ms after the read starts.
	ds := NewDistributedStore(store, &fakeRaft{leader: false})
	go func() {
		time.Sleep(120 * time.Millisecond)
		store.PutObjectMeta(ObjectMeta{Bucket: "b", Key: "k", Size: 5})
	}()
	start := time.Now()
	if meta, _ := ds.GetObjectMetaConsistent("b", "k"); meta == nil {
		t.Fatal("consistent read returned a premature miss instead of waiting for replication")
	}
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Fatalf("read returned in %s — it should have polled until the write landed", waited)
	}

	// A genuine miss returns nil once the timeout elapses (no false hit).
	if meta, _ := ds.GetObjectMetaConsistent("b", "gone"); meta != nil {
		t.Fatalf("genuine miss should return nil, got %+v", meta)
	}

	// The leader is authoritative — it never waits on a miss.
	dsLeader := NewDistributedStore(store, &fakeRaft{leader: true})
	start = time.Now()
	if meta, _ := dsLeader.GetObjectMetaConsistent("b", "absent"); meta != nil {
		t.Fatal("leader miss should be nil")
	}
	if waited := time.Since(start); waited > 100*time.Millisecond {
		t.Fatalf("leader waited %s on a miss — it should return immediately", waited)
	}

	// BucketExists uses the same wait-on-miss.
	if !ds.BucketExists("b") {
		t.Fatal("existing bucket should be found without waiting")
	}
}
