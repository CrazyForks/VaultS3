package cluster

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// clusterSecretHeader carries the shared secret that authenticates inter-node
// requests (join/leave/apply). Empty secret = auth disabled (single-tenant/dev).
const clusterSecretHeader = "X-Cluster-Secret"

// authOK reports whether an inter-node request is authorized. When no secret is
// configured it allows everything (backward compatible); otherwise it requires a
// constant-time match.
func (n *Node) authOK(r *http.Request) bool {
	if n.cfg.Secret == "" {
		return true
	}
	return hmac.Equal([]byte(r.Header.Get(clusterSecretHeader)), []byte(n.cfg.Secret))
}

// ClusterStatus is the response for the /cluster/status endpoint.
type ClusterStatus struct {
	NodeID   string            `json:"node_id"`
	State    string            `json:"state"` // Leader, Follower, Candidate
	Leader   string            `json:"leader"`
	LeaderID string            `json:"leader_id"`
	Servers  []ServerInfo      `json:"servers"`
	Stats    map[string]string `json:"stats"`
}

// ServerInfo describes a single cluster member.
type ServerInfo struct {
	ID       string `json:"id"`
	Address  string `json:"address"`
	Suffrage string `json:"suffrage"` // Voter, Nonvoter
}

// MembersInfo returns the current Raft members (id, raft address, suffrage) as
// plain structs, so callers outside this package don't need the raft types.
func (n *Node) MembersInfo() []ServerInfo {
	servers, err := n.Servers()
	if err != nil {
		return nil
	}
	out := make([]ServerInfo, 0, len(servers))
	for _, s := range servers {
		out = append(out, ServerInfo{
			ID:       string(s.ID),
			Address:  string(s.Address),
			Suffrage: s.Suffrage.String(),
		})
	}
	return out
}

// StatusHandler returns an HTTP handler for /cluster/status.
func (n *Node) StatusHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := ClusterStatus{
			NodeID:   n.cfg.NodeID,
			State:    n.raft.State().String(),
			Leader:   n.LeaderAddr(),
			LeaderID: n.LeaderID(),
			Stats:    n.Stats(),
		}

		if servers, err := n.Servers(); err == nil {
			for _, s := range servers {
				status.Servers = append(status.Servers, ServerInfo{
					ID:       string(s.ID),
					Address:  string(s.Address),
					Suffrage: s.Suffrage.String(),
				})
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	}
}

// JoinHandler returns an HTTP handler for POST /cluster/join.
// Body: {"node_id": "...", "addr": "host:port"}
func (n *Node) JoinHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !n.authOK(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var req struct {
			NodeID string `json:"node_id"`
			Addr   string `json:"addr"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.NodeID == "" || req.Addr == "" {
			http.Error(w, "node_id and addr are required", http.StatusBadRequest)
			return
		}

		if err := n.Join(req.NodeID, req.Addr); err != nil {
			if err == ErrNotLeader {
				// Redirect to leader
				leaderAddr := n.LeaderAddr()
				if leaderAddr == "" {
					http.Error(w, "no leader available", http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Location", fmt.Sprintf("http://%s/cluster/join", apiAddrFromRaft(leaderAddr)))
				http.Error(w, "not leader, redirect to: "+leaderAddr, http.StatusTemporaryRedirect)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"message": fmt.Sprintf("node %s joined at %s", req.NodeID, req.Addr),
		})
	}
}

// LeaveHandler returns an HTTP handler for POST /cluster/leave.
// Body: {"node_id": "..."}
func (n *Node) LeaveHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !n.authOK(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var req struct {
			NodeID string `json:"node_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.NodeID == "" {
			http.Error(w, "node_id is required", http.StatusBadRequest)
			return
		}

		if err := n.Leave(req.NodeID); err != nil {
			if err == ErrNotLeader {
				http.Error(w, "not leader", http.StatusTemporaryRedirect)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"message": fmt.Sprintf("node %s removed", req.NodeID),
		})
	}
}

// AutoJoin makes this node repeatedly ask an existing member (joinAddr, an API
// host:port) to add it to the cluster, until it succeeds or ctx is cancelled.
// It is the node-initiated counterpart to the JoinHandler: a fresh node POSTs
// its own {node_id, raft_addr}, retrying with backoff so it tolerates the leader
// not being ready yet (e.g. a Kubernetes StatefulSet where all pods start at
// once). If the target is a follower it answers 307 with the leader's address,
// which we follow. A node that is already a cluster member is a no-op.
func (n *Node) AutoJoin(ctx context.Context, joinAddr string) {
	selfAddr := fmt.Sprintf("%s:%d", n.cfg.BindAddr, n.cfg.RaftPort)
	body, _ := json.Marshal(map[string]string{"node_id": n.cfg.NodeID, "addr": selfAddr})
	client := &http.Client{Timeout: 5 * time.Second}
	backoff := time.Second

	// Announce our current address to the leader exactly once-successfully. We do
	// this even if we already appear to be a member: on a restart the pod IP may
	// have changed, and re-announcing (AddVoter with the same server ID, new
	// address) heals the cluster's record for this node.
	for {
		if err := postJoin(ctx, client, joinAddr, body, n.cfg.Secret); err == nil {
			slog.Info("cluster: auto-join announced", "node_id", n.cfg.NodeID, "addr", selfAddr, "via", joinAddr)
			return
		} else {
			slog.Debug("cluster: auto-join retrying", "node_id", n.cfg.NodeID, "via", joinAddr, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

// postJoin POSTs a join request to addr, following a single leader redirect.
func postJoin(ctx context.Context, client *http.Client, addr string, body []byte, secret string) error {
	url := fmt.Sprintf("http://%s/cluster/join", addr)
	for redirects := 0; redirects < 2; redirects++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if secret != "" {
			req.Header.Set(clusterSecretHeader, secret)
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK:
			return nil
		case resp.StatusCode == http.StatusTemporaryRedirect:
			if loc := resp.Header.Get("Location"); loc != "" {
				url = loc
				continue
			}
			return fmt.Errorf("redirect without Location")
		default:
			return fmt.Errorf("join returned %d", resp.StatusCode)
		}
	}
	return fmt.Errorf("too many redirects")
}

// ForwardToLeader sends an already-serialized metadata command to the current
// leader's /cluster/apply endpoint to be committed. Used by the DistributedStore
// when a write lands on a follower, so clients can write to any node.
func (n *Node) ForwardToLeader(data []byte) error {
	leaderRaft := n.LeaderAddr()
	if leaderRaft == "" {
		return fmt.Errorf("cluster: no leader to forward write to")
	}
	url := fmt.Sprintf("http://%s/cluster/apply", apiAddrFromRaft(leaderRaft))
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if n.cfg.Secret != "" {
		req.Header.Set(clusterSecretHeader, n.cfg.Secret)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("cluster: forward to leader: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("cluster: leader rejected forwarded write (%d): %s", resp.StatusCode, string(body))
	}
	// The leader replies with the committed log index; wait for our own FSM to
	// apply it so a read-after-write on THIS node sees the write (issue #37).
	var out struct {
		Index uint64 `json:"index"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 256)).Decode(&out) == nil && out.Index > 0 {
		if werr := n.WaitForApply(out.Index, 5*time.Second); werr != nil {
			slog.Warn("cluster: read-your-writes barrier timed out", "error", werr)
		}
	}
	return nil
}

// ApplyHandler returns an HTTP handler for POST /cluster/apply: a follower
// forwards a serialized metadata command here and the leader commits it to Raft.
// Inter-node use only.
func (n *Node) ApplyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !n.authOK(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		idx, err := n.ApplyIndexed(data)
		if err != nil {
			if err == ErrNotLeader {
				// Leadership moved between forward and apply — tell the caller to retry.
				http.Error(w, "not leader", http.StatusServiceUnavailable)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Return the committed log index so the forwarding follower can wait for its
		// own FSM to apply it (read-your-writes, issue #37).
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]uint64{"index": idx})
	}
}

// apiAddrFromRaft converts a Raft address (host:raftPort) to an API address (host:apiPort).
// Since we can't know the API port from the Raft port, we use a convention:
// Raft port 9001 → API port 9000 (raftPort - 1).
func apiAddrFromRaft(raftAddr string) string {
	parts := strings.Split(raftAddr, ":")
	if len(parts) != 2 {
		return raftAddr
	}
	return parts[0] + ":9000"
}
