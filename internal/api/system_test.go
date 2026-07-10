package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
