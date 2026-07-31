package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/sysinfo"
)

// TestClusterInfoAggregation covers the Tier-2 cluster rollup: the coordinator
// fetches each peer's /cluster/sysinfo over the cluster channel and aggregates
// capacity, marking unreachable peers without failing the whole call.
func TestClusterInfoAggregation(t *testing.T) {
	h, store := newTestAPI(t)
	if err := store.CreateBucket("b"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	// A reachable peer that serves its system info on the cluster channel.
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cluster/sysinfo") {
			json.NewEncoder(w).Encode(NodeSystemInfo{
				Version: "v9", OS: "linux", Arch: "amd64",
				Disk:        sysinfo.Disk{TotalBytes: 1000, UsedBytes: 400, FreeBytes: 600},
				ObjectBytes: 50, ObjectCount: 5, BucketCount: 2,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer peer.Close()
	peerAddr := strings.TrimPrefix(peer.URL, "http://")

	// A peer that returns 403 on the cluster channel (e.g. secret mismatch) — must
	// be reported unreachable WITH a reason, not silently.
	peer403 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"access denied"}`))
	}))
	defer peer403.Close()
	peer403Addr := strings.TrimPrefix(peer403.URL, "http://")

	h.SetClusterInfo("node-a", func() map[string]string {
		return map[string]string{
			"node-a": "self:9000",   // self — computed locally, not fetched
			"node-b": peerAddr,      // reachable peer
			"node-c": "127.0.0.1:1", // connection refused
			"node-d": peer403Addr,   // cluster/sysinfo 403
		}
	}, "")

	rr := httptest.NewRecorder()
	h.handleClusterInfo(rr, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}

	var out struct {
		Clustered      bool             `json:"clustered"`
		NodeCount      int              `json:"nodeCount"`
		ReachableNodes int              `json:"reachableNodes"`
		Nodes          []NodeSystemInfo `json:"nodes"`
		Totals         struct {
			Disk sysinfo.Disk `json:"disk"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !out.Clustered {
		t.Error("expected clustered=true")
	}
	if out.NodeCount != 4 {
		t.Errorf("nodeCount=%d, want 4", out.NodeCount)
	}
	if out.ReachableNodes != 2 {
		t.Errorf("reachableNodes=%d, want 2 (self + node-b)", out.ReachableNodes)
	}

	var b, c, d *NodeSystemInfo
	for i := range out.Nodes {
		switch out.Nodes[i].NodeID {
		case "node-b":
			b = &out.Nodes[i]
		case "node-c":
			c = &out.Nodes[i]
		case "node-d":
			d = &out.Nodes[i]
		}
	}
	if b == nil || !b.Reachable || b.Version != "v9" || b.Disk.TotalBytes != 1000 {
		t.Fatalf("reachable peer node-b not aggregated correctly: %+v", b)
	}
	if c == nil || c.Reachable || c.Error == "" {
		t.Fatalf("connection-refused peer node-c should be down with a reason: %+v", c)
	}
	if d == nil || d.Reachable || !strings.Contains(d.Error, "403") {
		t.Fatalf("403-login peer node-d should be down with a 403 reason: %+v", d)
	}
	// Totals include the peer's 1000 plus this node's real disk.
	if out.Totals.Disk.TotalBytes < 1000 {
		t.Errorf("totals should include peer's 1000, got %d", out.Totals.Disk.TotalBytes)
	}
}

// TestClusterInfoDoesNotMultiplyLogicalUsage is issue #43. Object metadata is
// replicated by Raft, so every node reports the SAME cluster-wide logical totals.
// Adding them up multiplied the number by the node count, which is why a 12-node
// cluster whose dashboard card read 82.2 GB in 395,999 objects showed 986.4 GB in
// 4,751,988 objects — exactly 12x — in the capacity panel right below it.
func TestClusterInfoDoesNotMultiplyLogicalUsage(t *testing.T) {
	h, store := newTestAPI(t)
	if err := store.CreateBucket("b"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	for i, size := range []int64{1024, 2048, 4096} {
		if err := store.PutObjectMeta(metadata.ObjectMeta{
			Bucket: "b", Key: fmt.Sprintf("obj-%d", i), Size: size, ETag: "e",
		}); err != nil {
			t.Fatalf("PutObjectMeta: %v", err)
		}
	}

	// In a real cluster every peer answers with the SAME replicated totals, since
	// Raft puts the whole metadata index on every node. Mirror that here.
	self := h.localSystemInfo()
	if self.ObjectCount != 3 {
		t.Fatalf("precondition: this node should see 3 objects, got %d", self.ObjectCount)
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(NodeSystemInfo{
			Version:     "v9",
			Disk:        sysinfo.Disk{TotalBytes: 1000, UsedBytes: 400, FreeBytes: 600},
			ObjectBytes: self.ObjectBytes,
			ObjectCount: self.ObjectCount,
		})
	}))
	defer peer.Close()
	peerAddr := strings.TrimPrefix(peer.URL, "http://")

	addrs := map[string]string{"node-a": "self:9000"}
	for _, id := range []string{"node-b", "node-c", "node-d", "node-e"} {
		addrs[id] = peerAddr
	}
	h.SetClusterInfo("node-a", func() map[string]string { return addrs }, "")

	rr := httptest.NewRecorder()
	h.handleClusterInfo(rr, httptest.NewRequest(http.MethodGet, "/x", nil))

	var out struct {
		Totals struct {
			Disk        sysinfo.Disk `json:"disk"`
			ObjectBytes int64        `json:"objectBytes"`
			ObjectCount int64        `json:"objectCount"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if out.Totals.ObjectBytes != self.ObjectBytes {
		t.Errorf("objectBytes = %d, want %d: logical usage is already cluster-wide, so summing it over %d nodes multiplies it by the node count",
			out.Totals.ObjectBytes, self.ObjectBytes, len(addrs))
	}
	if out.Totals.ObjectCount != self.ObjectCount {
		t.Errorf("objectCount = %d, want %d (same double-count)", out.Totals.ObjectCount, self.ObjectCount)
	}
	// Physical disk is genuinely per-node and must still add up across the cluster.
	if out.Totals.Disk.TotalBytes <= 1000 {
		t.Errorf("disk total = %d, want the sum across nodes: replicas really do occupy disk on every node",
			out.Totals.Disk.TotalBytes)
	}
}
