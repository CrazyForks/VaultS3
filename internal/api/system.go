package api

import (
	"crypto/hmac"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/sysinfo"
)

// clusterSecretHeader authenticates the inter-node /cluster/sysinfo request,
// matching the cluster package's convention.
const clusterSecretHeader = "X-Cluster-Secret"

// NodeSystemInfo is one node's version, capacity, and object usage. The cluster
// fields (NodeID/Address/Reachable) are omitted from the single-node
// /api/v1/system response and populated only in the cluster rollup.
type NodeSystemInfo struct {
	NodeID      string       `json:"nodeId,omitempty"`
	Address     string       `json:"address,omitempty"`
	Reachable   bool         `json:"reachable,omitempty"`
	Error       string       `json:"error,omitempty"` // why a peer is unreachable
	Version     string       `json:"version"`
	OS          string       `json:"os"`
	Arch        string       `json:"arch"`
	DataDirs    []string     `json:"dataDirs"`
	Disk        sysinfo.Disk `json:"disk"`
	ObjectBytes int64        `json:"objectBytes"`
	ObjectCount int64        `json:"objectCount"`
	BucketCount int          `json:"bucketCount"`
}

// clusterInfoClient fetches peers' /api/v1/system for the cluster rollup. Inter
// -node TLS is commonly self-signed, so certificate verification is skipped for
// these internal, admin-authenticated calls.
var clusterInfoClient = &http.Client{
	Timeout:   5 * time.Second,
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
}

// localSystemInfo gathers this node's version, on-disk capacity, and logical
// object usage.
func (h *APIHandler) localSystemInfo() NodeSystemInfo {
	var dirs []string
	if h.cfg != nil {
		dirs = append(dirs, h.cfg.Storage.DataDir, h.cfg.Storage.MetadataDir)
		if h.cfg.Tiering.Enabled && h.cfg.Tiering.ColdDataDir != "" {
			dirs = append(dirs, h.cfg.Tiering.ColdDataDir)
		}
		if h.cfg.Erasure.Enabled {
			dirs = append(dirs, h.cfg.Erasure.DataDirs...)
		}
	}

	var objectBytes, objectCount int64
	var bucketCount int
	if buckets, err := h.store.ListBuckets(); err == nil {
		bucketCount = len(buckets)
		for _, b := range buckets {
			size, count := h.bucketStatCounter(b.Name)
			objectBytes += size
			objectCount += count
		}
	}

	version := "dev"
	if h.updater != nil {
		if v := h.updater.LastStatus().Current; v != "" {
			version = v
		}
	}

	return NodeSystemInfo{
		Version:     version,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		DataDirs:    uniqueNonEmpty(dirs),
		Disk:        sysinfo.DiskUsage(dirs),
		ObjectBytes: objectBytes,
		ObjectCount: objectCount,
		BucketCount: bucketCount,
	}
}

// handleSystemInfo handles GET /api/v1/system: this node's version, data
// directories, on-disk capacity (total/used/free), and logical object usage.
func (h *APIHandler) handleSystemInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.localSystemInfo())
}

// ClusterSysInfoHandler serves this node's system info on the cluster channel
// (registered next to /cluster/status). The coordinator calls it peer-to-peer to
// build the capacity rollup, so it does not depend on the dashboard /api/v1 port.
// It is authenticated by the shared cluster secret when one is configured.
func (h *APIHandler) ClusterSysInfoHandler(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if secret != "" && !hmac.Equal([]byte(r.Header.Get(clusterSecretHeader)), []byte(secret)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, h.localSystemInfo())
	}
}

// handleClusterInfo handles GET /api/v1/cluster/info: the version and capacity of
// every node in the cluster, plus aggregate totals — a cluster-wide equivalent of
// `mc admin info`. On a single node it returns just this node.
func (h *APIHandler) handleClusterInfo(w http.ResponseWriter, _ *http.Request) {
	self := h.localSystemInfo()
	self.NodeID = h.clusterSelfID
	self.Reachable = true
	nodes := []NodeSystemInfo{self}

	if h.clusterNodeAddrs != nil {
		for id, addr := range h.clusterNodeAddrs() {
			if id == h.clusterSelfID || addr == "" {
				continue
			}
			nodes = append(nodes, h.fetchPeerSystemInfo(id, addr))
		}
	}

	// Aggregate physical disk across reachable nodes (replicas legitimately use
	// disk on multiple nodes, so this is the true "how full is the cluster").
	var totalDisk sysinfo.Disk
	var objectBytes, objectCount int64
	reachable := 0
	for _, n := range nodes {
		if !n.Reachable {
			continue
		}
		reachable++
		totalDisk.TotalBytes += n.Disk.TotalBytes
		totalDisk.UsedBytes += n.Disk.UsedBytes
		totalDisk.FreeBytes += n.Disk.FreeBytes
		objectBytes += n.ObjectBytes
		objectCount += n.ObjectCount
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"clustered":      h.clusterSelfID != "",
		"nodeCount":      len(nodes),
		"reachableNodes": reachable,
		"nodes":          nodes,
		"totals": map[string]any{
			"disk":        totalDisk,
			"objectBytes": objectBytes,
			"objectCount": objectCount,
		},
	})
}

// fetchPeerSystemInfo reads a peer's capacity over the cluster channel
// (/cluster/sysinfo, the same address the object-placement proxy already reaches
// for S3 forwarding). This avoids the dashboard /api/v1 port and admin login,
// which are not reachable peer-to-peer in split-port or proxied deployments. An
// unreachable peer is returned with Reachable=false rather than failing the
// whole rollup.
func (h *APIHandler) fetchPeerSystemInfo(id, addr string) NodeSystemInfo {
	ni := NodeSystemInfo{NodeID: id, Address: addr}
	scheme := "http"
	if h.cfg != nil && h.cfg.Server.TLS.Enabled {
		scheme = "https"
	}

	req, err := http.NewRequest(http.MethodGet, scheme+"://"+addr+"/cluster/sysinfo", nil)
	if err != nil {
		ni.Error = err.Error()
		return ni
	}
	if h.clusterSecret != "" {
		req.Header.Set(clusterSecretHeader, h.clusterSecret)
	}
	resp, err := clusterInfoClient.Do(req)
	if err != nil {
		ni.Error = err.Error()
		return ni
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		ni.Error = fmt.Sprintf("cluster/sysinfo returned HTTP %d", resp.StatusCode)
		return ni
	}
	if err := json.NewDecoder(resp.Body).Decode(&ni); err != nil {
		ni.Error = err.Error()
		return ni
	}
	ni.NodeID, ni.Address, ni.Reachable, ni.Error = id, addr, true, ""
	return ni
}

func uniqueNonEmpty(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
