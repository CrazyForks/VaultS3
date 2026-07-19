package cluster

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/hashicorp/raft"
)

// These tests run a real multi-node Raft cluster in a single process using
// HashiCorp Raft's in-memory transport (which supports true network
// partitioning via Connect/Disconnect) and in-memory log/snapshot stores.
// They cover the consensus core: leader election, log replication, the
// no-split-brain safety property under partition, and membership changes.

type testNode struct {
	node  *Node
	trans *raft.InmemTransport
	addr  raft.ServerAddress
	store *metadata.Store
}

// eventually polls cond until it returns true or the timeout elapses.
func eventually(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, desc)
}

func leaderCount(nodes []*testNode) int {
	c := 0
	for _, n := range nodes {
		if n.node.IsLeader() {
			c++
		}
	}
	return c
}

func leaderOf(nodes []*testNode) *testNode {
	for _, n := range nodes {
		if n.node.IsLeader() {
			return n
		}
	}
	return nil
}

func mustLeader(t *testing.T, nodes []*testNode) *testNode {
	t.Helper()
	var ld *testNode
	eventually(t, 15*time.Second, "a leader exists", func() bool {
		ld = leaderOf(nodes)
		return ld != nil
	})
	return ld
}

func createBucketOn(t *testing.T, n *Node, name string) error {
	t.Helper()
	cmd, err := marshalCommand(CmdCreateBucket, struct{ Name string }{Name: name})
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}
	return n.Apply(cmd)
}

// newRaftCluster spins up an n-node cluster: node-0 bootstraps, then adds the
// rest as voters. Returns once every node sees the full membership.
func newRaftCluster(t *testing.T, n int) []*testNode {
	t.Helper()
	nodes := make([]*testNode, n)

	for i := 0; i < n; i++ {
		id := fmt.Sprintf("node-%d", i)
		addr, trans := raft.NewInmemTransport(raft.ServerAddress(id))

		store, err := metadata.NewStore(filepath.Join(t.TempDir(), "meta.db"))
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		t.Cleanup(func() { store.Close() })

		rstore := raft.NewInmemStore()
		node, err := newNodeWithDeps(
			ClusterConfig{NodeID: id, Bootstrap: i == 0},
			store,
			raftDeps{
				transport: trans,
				logStore:  rstore,
				stable:    rstore,
				snapshots: raft.NewInmemSnapshotStore(),
			},
		)
		if err != nil {
			t.Fatalf("newNodeWithDeps(%s): %v", id, err)
		}
		t.Cleanup(func() { node.Shutdown() })

		nodes[i] = &testNode{node: node, trans: trans, addr: addr, store: store}
	}

	// Fully connect the in-memory transports.
	for i := range nodes {
		for j := range nodes {
			if i != j {
				nodes[i].trans.Connect(nodes[j].addr, nodes[j].trans)
			}
		}
	}

	// node-0 bootstrapped as a single-server cluster; wait for it to win.
	eventually(t, 15*time.Second, "node-0 becomes initial leader", func() bool {
		return nodes[0].node.IsLeader()
	})

	// Leader adds the remaining nodes as voters.
	for i := 1; i < n; i++ {
		if err := nodes[0].node.Join(string(nodes[i].addr), string(nodes[i].addr)); err != nil {
			t.Fatalf("join %s: %v", nodes[i].addr, err)
		}
	}

	// Wait until every node has the full configuration replicated.
	eventually(t, 15*time.Second, "all nodes see full membership", func() bool {
		for _, nd := range nodes {
			servers, err := nd.node.Servers()
			if err != nil || len(servers) != n {
				return false
			}
		}
		return true
	})

	return nodes
}

// TestRaftLeaderElection: a 3-node cluster elects exactly one leader and all
// nodes agree on its identity.
func TestRaftLeaderElection(t *testing.T) {
	nodes := newRaftCluster(t, 3)

	eventually(t, 15*time.Second, "exactly one leader", func() bool {
		return leaderCount(nodes) == 1
	})

	leadID := leaderOf(nodes).node.NodeID()
	eventually(t, 10*time.Second, "all nodes agree on the leader", func() bool {
		for _, n := range nodes {
			if n.node.LeaderID() != leadID {
				return false
			}
		}
		return true
	})
}

// TestRaftLogReplication: a write applied on the leader lands in every
// follower's FSM (metadata store).
func TestRaftLogReplication(t *testing.T) {
	nodes := newRaftCluster(t, 3)
	leader := mustLeader(t, nodes)

	if err := createBucketOn(t, leader.node, "replicated"); err != nil {
		t.Fatalf("apply on leader: %v", err)
	}

	eventually(t, 15*time.Second, "bucket replicated to every store", func() bool {
		for _, n := range nodes {
			if !n.store.BucketExists("replicated") {
				return false
			}
		}
		return true
	})
}

// TestRaftFollowerRejectsWrites: writes must go through the leader; a follower
// returns ErrNotLeader rather than applying locally.
func TestRaftFollowerRejectsWrites(t *testing.T) {
	nodes := newRaftCluster(t, 3)
	leader := mustLeader(t, nodes)

	var follower *testNode
	for _, n := range nodes {
		if n != leader {
			follower = n
			break
		}
	}

	err := createBucketOn(t, follower.node, "should-fail")
	if err != ErrNotLeader {
		t.Fatalf("follower write: got %v, want ErrNotLeader", err)
	}
	if follower.store.BucketExists("should-fail") {
		t.Fatal("follower applied a write locally — bypassed consensus")
	}
}

// TestRaftPartitionNoSplitBrain: partitioning the leader into a minority of one
// must force it to step down and reject writes, while the majority elects a new
// leader and keeps serving. After healing, the old leader converges.
func TestRaftPartitionNoSplitBrain(t *testing.T) {
	nodes := newRaftCluster(t, 3)
	leader := mustLeader(t, nodes)

	if err := createBucketOn(t, leader.node, "baseline"); err != nil {
		t.Fatalf("apply baseline: %v", err)
	}
	eventually(t, 15*time.Second, "baseline replicated", func() bool {
		for _, n := range nodes {
			if !n.store.BucketExists("baseline") {
				return false
			}
		}
		return true
	})

	// Isolate the leader from the other two (minority of 1).
	var majority []*testNode
	for _, n := range nodes {
		if n != leader {
			majority = append(majority, n)
			leader.trans.Disconnect(n.addr)
			n.trans.Disconnect(leader.addr)
		}
	}

	// The isolated leader loses quorum and must step down.
	eventually(t, 15*time.Second, "isolated leader steps down", func() bool {
		return !leader.node.IsLeader()
	})

	// The majority side elects a fresh leader.
	var newLeader *testNode
	eventually(t, 15*time.Second, "majority elects a new leader", func() bool {
		for _, n := range majority {
			if n.node.IsLeader() {
				newLeader = n
				return true
			}
		}
		return false
	})

	// Safety: the isolated minority node cannot commit a write.
	if err := createBucketOn(t, leader.node, "orphan"); err == nil {
		t.Fatal("isolated minority leader accepted a write — split-brain!")
	}

	// Liveness: the majority side keeps accepting writes.
	if err := createBucketOn(t, newLeader.node, "majority-write"); err != nil {
		t.Fatalf("majority write failed: %v", err)
	}
	eventually(t, 15*time.Second, "majority write commits within the partition", func() bool {
		for _, n := range majority {
			if !n.store.BucketExists("majority-write") {
				return false
			}
		}
		return true
	})

	// The write must not have leaked to the isolated node.
	if leader.store.BucketExists("majority-write") {
		t.Fatal("write crossed the partition")
	}
	if leader.store.BucketExists("orphan") {
		t.Fatal("orphan write must never have committed")
	}

	// Heal the partition — the old leader rejoins and catches up.
	for _, n := range majority {
		leader.trans.Connect(n.addr, n.trans)
		n.trans.Connect(leader.addr, leader.trans)
	}
	eventually(t, 20*time.Second, "old leader converges after heal", func() bool {
		return leader.store.BucketExists("majority-write")
	})
	eventually(t, 15*time.Second, "exactly one leader after heal", func() bool {
		return leaderCount(nodes) == 1
	})
}

// TestRaftMembershipChange: removing a node mid-operation keeps prior data and
// continues to replicate new writes to the surviving members.
func TestRaftMembershipChange(t *testing.T) {
	nodes := newRaftCluster(t, 3)
	leader := mustLeader(t, nodes)

	if err := createBucketOn(t, leader.node, "before"); err != nil {
		t.Fatalf("apply before: %v", err)
	}
	eventually(t, 15*time.Second, "pre-change write replicated", func() bool {
		for _, n := range nodes {
			if !n.store.BucketExists("before") {
				return false
			}
		}
		return true
	})

	// Remove a follower from the cluster.
	var victim, remaining *testNode
	for _, n := range nodes {
		if n == leader {
			continue
		}
		if victim == nil {
			victim = n
		} else {
			remaining = n
		}
	}
	if err := leader.node.Leave(victim.node.NodeID()); err != nil {
		t.Fatalf("leave: %v", err)
	}
	eventually(t, 15*time.Second, "membership shrinks to 2", func() bool {
		servers, err := leader.node.Servers()
		return err == nil && len(servers) == 2
	})

	// New writes must still replicate to the remaining follower (no data loss).
	if err := createBucketOn(t, leader.node, "after"); err != nil {
		t.Fatalf("apply after: %v", err)
	}
	eventually(t, 15*time.Second, "post-change write replicated to survivor", func() bool {
		return remaining.store.BucketExists("after")
	})

	// And the pre-change data is still intact on the surviving members.
	if !leader.store.BucketExists("before") || !remaining.store.BucketExists("before") {
		t.Fatal("pre-membership-change data lost")
	}
}

// TestReadYourWritesApplyBarrier covers issue #37: after the leader commits a
// write, a follower's WaitForApply lets it block until its own FSM has applied
// that index, so a read-after-write on the follower reflects the write.
func TestReadYourWritesApplyBarrier(t *testing.T) {
	nodes := newRaftCluster(t, 3)
	leader := mustLeader(t, nodes)

	var follower *testNode
	for _, n := range nodes {
		if !n.node.IsLeader() {
			follower = n
			break
		}
	}
	if follower == nil {
		t.Fatal("no follower found")
	}

	cmd, err := marshalCommand(CmdCreateBucket, struct{ Name string }{Name: "ryw"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	idx, err := leader.node.ApplyIndexed(cmd)
	if err != nil {
		t.Fatalf("ApplyIndexed on leader: %v", err)
	}
	if idx == 0 {
		t.Fatal("expected a non-zero committed index")
	}

	// The follower blocks until its FSM reaches the committed index, then MUST see
	// the write — this is the read-your-writes guarantee.
	if err := follower.node.WaitForApply(idx, 3*time.Second); err != nil {
		t.Fatalf("follower WaitForApply: %v", err)
	}
	if !follower.store.BucketExists("ryw") {
		t.Fatal("follower did not reflect the write after the apply barrier")
	}

	// ApplyIndexed is leader-only.
	if _, err := follower.node.ApplyIndexed(cmd); err != ErrNotLeader {
		t.Fatalf("follower ApplyIndexed: want ErrNotLeader, got %v", err)
	}
}
