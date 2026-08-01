package cluster

import (
	"testing"
)

// syncMembership derives each peer's API address from its Raft address by
// swapping in THIS node's API port, which is only right when every node serves
// on the same port. Where nodes share a host on different ports (a single-host
// cluster, or docker-compose with host networking) that made every peer resolve
// to this node: object forwarding, rebalance, and the cluster capacity rollup
// all silently addressed the wrong node, which is how the rollup reported one
// node's disk usage N times over (issue #43).
func TestSyncMembershipKeepsConfiguredPeerAPIs(t *testing.T) {
	nodes := newRaftCluster(t, 3)
	self := nodes[0].node

	ring := NewHashRing(64)
	p := NewProxy(ring, self, PlacementConfig{ReplicaCount: 3}, map[string]string{})
	p.SetPeerAPIs(map[string]string{
		"node-1": "127.0.0.1:9422",
		"node-2": "127.0.0.1:9423",
	})

	// 9421 is this node's API port, and the only one Raft membership can suggest.
	p.syncMembership(9421)
	addrs := p.NodeAddrs()

	if got := addrs["node-1"]; got != "127.0.0.1:9422" {
		t.Errorf("node-1 = %q, want the configured 127.0.0.1:9422", got)
	}
	if got := addrs["node-2"]; got != "127.0.0.1:9423" {
		t.Errorf("node-2 = %q, want the configured 127.0.0.1:9423", got)
	}
	if addrs["node-1"] == addrs["node-2"] {
		t.Errorf("peers collapsed onto one address (%q): every peer request would hit the same node", addrs["node-1"])
	}
}

// Without peer_apis the derived address is all there is, and it must still
// follow membership: that is what lets a restarted pod with a new IP self-heal.
func TestSyncMembershipDerivesUnconfiguredPeers(t *testing.T) {
	nodes := newRaftCluster(t, 3)
	p := NewProxy(NewHashRing(64), nodes[0].node, PlacementConfig{ReplicaCount: 3}, map[string]string{})

	p.syncMembership(9000)
	addrs := p.NodeAddrs()
	if len(addrs) != 3 {
		t.Fatalf("addrs = %v, want one entry per member", addrs)
	}
	for id, addr := range addrs {
		if addr == "" {
			t.Errorf("%s has no derived address", id)
		}
	}
}

// This node's own entry must stay derived from membership. The configured form
// is a bind address, which is commonly the 0.0.0.0 wildcard and useless as a
// destination.
func TestSyncMembershipDoesNotPinSelfToBindAddress(t *testing.T) {
	nodes := newRaftCluster(t, 3)
	self := nodes[0].node
	p := NewProxy(NewHashRing(64), self, PlacementConfig{ReplicaCount: 3}, map[string]string{})
	p.SetPeerAPIs(map[string]string{self.NodeID(): "0.0.0.0:9000"})

	p.syncMembership(9000)
	if got := p.NodeAddrs()[self.NodeID()]; got == "0.0.0.0:9000" {
		t.Errorf("self pinned to the wildcard bind address %q", got)
	}
}
