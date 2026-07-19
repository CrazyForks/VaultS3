package api

import (
	"crypto/hmac"
	"fmt"
	"net/http"
	"sync/atomic"
)

// ClusterController is the subset of cluster.Node the admin API needs for
// membership operations. An adapter in server.go implements it so this package
// does not import internal/cluster.
type ClusterController interface {
	SelfID() string
	IsLeader() bool
	LeaderID() string
	Members() []ClusterMember
	Join(nodeID, addr string) error
	Leave(nodeID string) error
}

// ClusterMember is one Raft member as reported by the cluster status endpoint.
type ClusterMember struct {
	NodeID   string `json:"nodeId"`
	Address  string `json:"address"`  // raft address
	Suffrage string `json:"suffrage"` // Voter / Nonvoter
	Leader   bool   `json:"leader"`
}

// SetWritable wires the node-local write gate (shared with the S3 handler). When
// it holds false the node is "drained": S3 object writes are rejected while reads
// continue. nil ⇒ always writable. Enables the drain/undrain admin endpoints.
func (h *APIHandler) SetWritable(w *atomic.Bool) { h.writable = w }

// SetClusterController wires the cluster-membership operations and the rebalance
// trigger for the admin cluster endpoints (not-clustered if unset).
func (h *APIHandler) SetClusterController(ctl ClusterController, triggerRebalance func(), rebalanceRunning func() bool) {
	h.clusterCtl = ctl
	h.triggerRebalance = triggerRebalance
	h.rebalanceRunning = rebalanceRunning
}

// isWritable reports whether this node currently accepts writes.
func (h *APIHandler) isWritable() bool { return h.writable == nil || h.writable.Load() }

// handleClusterStatus handles GET /api/v1/cluster/status: Raft membership, the
// current leader, and this node's write (drain) state.
func (h *APIHandler) handleClusterStatus(w http.ResponseWriter, _ *http.Request) {
	if h.clusterCtl == nil {
		writeJSON(w, http.StatusOK, map[string]any{"clustered": false, "writable": h.isWritable()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"clustered": true,
		"selfId":    h.clusterCtl.SelfID(),
		"leaderId":  h.clusterCtl.LeaderID(),
		"isLeader":  h.clusterCtl.IsLeader(),
		"writable":  h.isWritable(),
		"members":   h.clusterCtl.Members(),
	})
}

// handleClusterJoin handles POST /api/v1/cluster/join {nodeId, addr}: add a Raft
// member. Must be run against the leader (the node method redirects otherwise).
func (h *APIHandler) handleClusterJoin(w http.ResponseWriter, r *http.Request) {
	if h.clusterCtl == nil {
		writeError(w, http.StatusBadRequest, "this node is not running in cluster mode")
		return
	}
	var req struct {
		NodeID string `json:"nodeId"`
		Addr   string `json:"addr"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.NodeID == "" || req.Addr == "" {
		writeError(w, http.StatusBadRequest, "nodeId and addr are required")
		return
	}
	if err := h.clusterCtl.Join(req.NodeID, req.Addr); err != nil {
		writeError(w, http.StatusInternalServerError, "join failed: "+err.Error()+" (run join against the leader node)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "message": "node " + req.NodeID + " joined"})
}

// handleClusterLeave handles POST /api/v1/cluster/leave {nodeId}: remove a Raft
// member. Removing a node that still holds the only copy of data loses it — drain
// and rebalance first (see docs/SCALING.md).
func (h *APIHandler) handleClusterLeave(w http.ResponseWriter, r *http.Request) {
	if h.clusterCtl == nil {
		writeError(w, http.StatusBadRequest, "this node is not running in cluster mode")
		return
	}
	var req struct {
		NodeID string `json:"nodeId"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.NodeID == "" {
		writeError(w, http.StatusBadRequest, "nodeId is required")
		return
	}
	if err := h.clusterCtl.Leave(req.NodeID); err != nil {
		writeError(w, http.StatusInternalServerError, "leave failed: "+err.Error()+" (run leave against the leader node)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "message": "node " + req.NodeID + " removed"})
}

// handleClusterDrain handles POST /api/v1/cluster/drain and /undrain. Body
// {nodeId} is optional: empty or this node's ID drains the node serving the
// request; another node's ID is forwarded over the cluster channel. Draining
// makes a node reject S3 object writes (503) while still serving reads, so it can
// be evacuated for replacement or maintenance.
func (h *APIHandler) handleClusterDrain(w http.ResponseWriter, r *http.Request, drain bool) {
	if h.writable == nil {
		writeError(w, http.StatusBadRequest, "drain is unavailable on this node")
		return
	}
	var req struct {
		NodeID string `json:"nodeId"`
	}
	_ = readJSON(r, &req) // body optional (defaults to this node)

	self := ""
	if h.clusterCtl != nil {
		self = h.clusterCtl.SelfID()
	}
	if req.NodeID != "" && req.NodeID != self {
		if err := h.forwardDrain(req.NodeID, drain); err != nil {
			writeError(w, http.StatusBadGateway, "forward drain to "+req.NodeID+" failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"nodeId": req.NodeID, "writable": !drain})
		return
	}

	h.writable.Store(!drain)
	writeJSON(w, http.StatusOK, map[string]any{"nodeId": self, "writable": !drain})
}

// forwardDrain sets the drain state on another node over the cluster channel
// (POST /cluster/drain?state=, cluster-secret authed) using the peer address the
// placement proxy already reaches.
func (h *APIHandler) forwardDrain(nodeID string, drain bool) error {
	if h.clusterNodeAddrs == nil {
		return fmt.Errorf("no peer address map")
	}
	addr := h.clusterNodeAddrs()[nodeID]
	if addr == "" {
		return fmt.Errorf("unknown node %q", nodeID)
	}
	scheme := "http"
	if h.cfg != nil && h.cfg.Server.TLS.Enabled {
		scheme = "https"
	}
	state := "false"
	if !drain {
		state = "true"
	}
	req, err := http.NewRequest(http.MethodPost, scheme+"://"+addr+"/cluster/drain?state="+state, nil)
	if err != nil {
		return err
	}
	if h.clusterSecret != "" {
		req.Header.Set(clusterSecretHeader, h.clusterSecret)
	}
	resp, err := clusterInfoClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// ClusterDrainHandler serves POST /cluster/drain?state=true|false on the cluster
// channel (cluster-secret authed), letting the coordinator set this node's write
// gate. state=true ⇒ writable, state=false ⇒ drained.
func (h *APIHandler) ClusterDrainHandler(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if secret != "" && !hmac.Equal([]byte(r.Header.Get(clusterSecretHeader)), []byte(secret)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if h.writable != nil {
			h.writable.Store(r.URL.Query().Get("state") != "false")
		}
		w.WriteHeader(http.StatusOK)
	}
}

// ClusterObjectDeleteHandler removes a single object's data file from THIS node's
// local engine only (no metadata, no proxy). The delete coordinator broadcasts it
// to every node to reap replica/orphan copies left after a delete, so deleted
// data doesn't linger on disk (issue #34 layer 2). Cluster-secret authed;
// best-effort — a missing file is not an error.
func (h *APIHandler) ClusterObjectDeleteHandler(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if secret != "" && !hmac.Equal([]byte(r.Header.Get(clusterSecretHeader)), []byte(secret)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		bucket := r.URL.Query().Get("bucket")
		key := r.URL.Query().Get("key")
		if bucket != "" && key != "" && h.engine != nil {
			_ = h.engine.DeleteObject(bucket, key) // best-effort; missing file is fine
		}
		w.WriteHeader(http.StatusOK)
	}
}

// ClusterReplicaPutHandler stores an object's data on THIS node's local engine
// (no metadata write — that arrives via Raft — and no re-fan-out). The primary
// broadcasts here to keep replica copies so a node loss doesn't make an object
// unavailable (issue #37, replica_count > 1). Cluster-secret authed.
func (h *APIHandler) ClusterReplicaPutHandler(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if secret != "" && !hmac.Equal([]byte(r.Header.Get(clusterSecretHeader)), []byte(secret)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		bucket := r.URL.Query().Get("bucket")
		key := r.URL.Query().Get("key")
		if bucket == "" || key == "" || h.engine == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, _, err := h.engine.PutObject(bucket, key, r.Body, r.ContentLength); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// handleClusterRebalance handles POST /api/v1/cluster/rebalance: trigger a
// background pass that moves objects to their correct hash-ring owner (used after
// membership changes to evacuate or absorb a node's data).
func (h *APIHandler) handleClusterRebalance(w http.ResponseWriter, _ *http.Request) {
	if h.triggerRebalance == nil {
		writeError(w, http.StatusBadRequest, "rebalance is unavailable (node not clustered)")
		return
	}
	h.triggerRebalance()
	running := false
	if h.rebalanceRunning != nil {
		running = h.rebalanceRunning()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "triggered", "running": running})
}
