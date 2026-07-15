package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// fakeCluster is a test ClusterController.
type fakeCluster struct {
	self, leader     string
	isLeader         bool
	members          []ClusterMember
	joined, left     []string
	joinErr, leaveEr error
}

func (f *fakeCluster) SelfID() string           { return f.self }
func (f *fakeCluster) IsLeader() bool           { return f.isLeader }
func (f *fakeCluster) LeaderID() string         { return f.leader }
func (f *fakeCluster) Members() []ClusterMember { return f.members }
func (f *fakeCluster) Join(id, addr string) error {
	f.joined = append(f.joined, id)
	return f.joinErr
}
func (f *fakeCluster) Leave(id string) error {
	f.left = append(f.left, id)
	return f.leaveEr
}

func TestClusterStatusNotClustered(t *testing.T) {
	h, _ := newTestAPI(t)
	rr := httptest.NewRecorder()
	h.handleClusterStatus(rr, httptest.NewRequest(http.MethodGet, "/x", nil))
	var out map[string]any
	json.Unmarshal(rr.Body.Bytes(), &out)
	if out["clustered"] != false {
		t.Fatalf("want clustered=false, got %v", out["clustered"])
	}
}

func TestClusterStatusAndMembership(t *testing.T) {
	h, _ := newTestAPI(t)
	fc := &fakeCluster{
		self: "node-0", leader: "node-0", isLeader: true,
		members: []ClusterMember{
			{NodeID: "node-0", Address: "10.0.0.1:7000", Suffrage: "Voter", Leader: true},
			{NodeID: "node-1", Address: "10.0.0.2:7000", Suffrage: "Voter"},
		},
	}
	writable := &atomic.Bool{}
	writable.Store(true)
	h.SetWritable(writable)
	rebalanced := false
	h.SetClusterController(fc, func() { rebalanced = true }, func() bool { return false })
	tok := getToken(t, h)

	// status (via full ServeHTTP so auth + routing are exercised)
	rr := doRequest(h, "GET", "/cluster/status", nil, tok)
	var st struct {
		Clustered bool            `json:"clustered"`
		Members   []ClusterMember `json:"members"`
		Writable  bool            `json:"writable"`
	}
	json.Unmarshal(rr.Body.Bytes(), &st)
	if !st.Clustered || len(st.Members) != 2 || !st.Writable {
		t.Fatalf("unexpected status: %+v (%s)", st, rr.Body.String())
	}

	// join
	if rr := doRequest(h, "POST", "/cluster/join", map[string]string{"nodeId": "node-2", "addr": "10.0.0.3:7000"}, tok); rr.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rr.Code, rr.Body.String())
	}
	if len(fc.joined) != 1 || fc.joined[0] != "node-2" {
		t.Fatalf("join not recorded: %v", fc.joined)
	}

	// join missing fields → 400
	if rr := doRequest(h, "POST", "/cluster/join", map[string]string{"nodeId": "x"}, tok); rr.Code != http.StatusBadRequest {
		t.Fatalf("join missing addr: want 400, got %d", rr.Code)
	}

	// leave
	if rr := doRequest(h, "POST", "/cluster/leave", map[string]string{"nodeId": "node-1"}, tok); rr.Code != http.StatusOK {
		t.Fatalf("leave: %d %s", rr.Code, rr.Body.String())
	}
	if len(fc.left) != 1 || fc.left[0] != "node-1" {
		t.Fatalf("leave not recorded: %v", fc.left)
	}

	// drain self (empty body defaults to this node) → flag flips false
	if rr := doRequest(h, "POST", "/cluster/drain", nil, tok); rr.Code != http.StatusOK {
		t.Fatalf("drain: %d %s", rr.Code, rr.Body.String())
	}
	if writable.Load() {
		t.Fatal("drain did not clear the writable flag")
	}
	// undrain → flag back to true
	if rr := doRequest(h, "POST", "/cluster/undrain", nil, tok); rr.Code != http.StatusOK {
		t.Fatalf("undrain: %d %s", rr.Code, rr.Body.String())
	}
	if !writable.Load() {
		t.Fatal("undrain did not set the writable flag")
	}

	// rebalance triggers the callback
	if rr := doRequest(h, "POST", "/cluster/rebalance", nil, tok); rr.Code != http.StatusAccepted {
		t.Fatalf("rebalance: %d %s", rr.Code, rr.Body.String())
	}
	if !rebalanced {
		t.Fatal("rebalance callback not invoked")
	}
}
