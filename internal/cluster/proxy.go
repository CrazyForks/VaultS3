package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Proxy handles forwarding S3 requests to the correct node in the cluster
// based on the hash ring placement.
type Proxy struct {
	ring      *HashRing
	node      *Node
	placement PlacementConfig
	nodeAddrs map[string]string // nodeID → "host:apiPort"
	mu        sync.RWMutex
	proxies   map[string]*httputil.ReverseProxy // cached per-node proxies
}

// NewProxy creates a new cluster proxy.
func NewProxy(ring *HashRing, node *Node, placement PlacementConfig, nodeAddrs map[string]string) *Proxy {
	applyPlacementDefaults(&placement)
	return &Proxy{
		ring:      ring,
		node:      node,
		placement: placement,
		nodeAddrs: nodeAddrs,
		proxies:   make(map[string]*httputil.ReverseProxy),
	}
}

// RunMembershipSync keeps the hash ring and the node-address map in step with the
// live Raft membership, so data placement is identical on every node. This is
// what makes auto-clustering work: nodes join dynamically (not via static config),
// so the ring must follow the cluster, not a fixed peer list. apiPort is the API
// port every node serves on (Raft addresses carry the raft port, which we swap).
func (p *Proxy) RunMembershipSync(ctx context.Context, apiPort int) {
	p.syncMembership(apiPort)
	// Reconcile the ring to live Raft membership frequently so the window where two
	// pods disagree about a key's owner (and a read is routed to a node that lacks
	// it) stays small during membership churn (issue #37).
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.syncMembership(apiPort)
		}
	}
}

func (p *Proxy) syncMembership(apiPort int) {
	servers, err := p.node.Servers()
	if err != nil {
		return
	}
	members := make(map[string]string, len(servers))
	for _, s := range servers {
		host := string(s.Address)
		if i := strings.LastIndex(host, ":"); i >= 0 {
			host = host[:i]
		}
		members[string(s.ID)] = fmt.Sprintf("%s:%d", host, apiPort)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// Drop cached reverse proxies whose address changed or whose node left, so the
	// next request rebuilds them against the current address. Pods get a new IP on
	// restart; without this a node would route to a dead address forever.
	p.invalidateStaleProxiesLocked(members)
	p.nodeAddrs = members
	// Reconcile the ring to exactly the live member set.
	want := make(map[string]bool, len(members))
	for id := range members {
		want[id] = true
	}
	for _, id := range p.ring.Nodes() {
		if !want[id] {
			p.ring.RemoveNode(id)
		}
	}
	for id := range members {
		if !p.ring.HasNode(id) {
			p.ring.AddNode(id)
		}
	}
}

// OwnerAPIAddr returns the API address of the node that owns (bucket, key) when
// that is NOT this node, so callers can place or fetch the object there. Returns
// ("", false) when this node is the owner.
func (p *Proxy) OwnerAPIAddr(bucket, key string) (string, bool) {
	target := p.ShouldProxy(bucket, key)
	if target == "" {
		return "", false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	addr, ok := p.nodeAddrs[target]
	if !ok {
		return "", false
	}
	return addr, true
}

// ShouldProxy checks if a request for the given bucket/key should be proxied
// to another node. Returns the target node ID if proxying is needed,
// or empty string if this node should handle it.
func (p *Proxy) ShouldProxy(bucket, key string) string {
	if bucket == "" {
		// Service-level operations (ListBuckets) — handle locally
		return ""
	}

	// For bucket-level operations (key == ""), hash on just the bucket
	hashKey := key
	if hashKey == "" {
		hashKey = ""
	}

	primaryNode := p.ring.GetNode(bucket, hashKey)
	if primaryNode == "" || primaryNode == p.node.NodeID() {
		return ""
	}

	return primaryNode
}

// ForwardRequest proxies an HTTP request to the specified target node.
func (p *Proxy) ForwardRequest(w http.ResponseWriter, r *http.Request, targetNodeID string) {
	p.mu.RLock()
	addr, ok := p.nodeAddrs[targetNodeID]
	p.mu.RUnlock()

	if !ok {
		slog.Warn("proxy: unknown target node", "node_id", targetNodeID)
		http.Error(w, "cluster node not found", http.StatusBadGateway)
		return
	}

	proxy := p.getOrCreateProxy(targetNodeID, addr)

	// Mark as internal cluster proxy to prevent infinite proxy loops
	r.Header.Set("X-VaultS3-Proxy", p.node.NodeID())

	slog.Debug("proxy: forwarding request",
		"method", r.Method,
		"path", r.URL.Path,
		"from", p.node.NodeID(),
		"to", targetNodeID,
		"addr", addr,
	)

	proxy.ServeHTTP(w, r)
}

// IsProxied checks if a request was already proxied from another node.
func IsProxied(r *http.Request) bool {
	return r.Header.Get("X-VaultS3-Proxy") != ""
}

// UpdateNodeAddr updates the API address for a node.
func (p *Proxy) UpdateNodeAddr(nodeID, addr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nodeAddrs[nodeID] = addr
	// Invalidate cached proxy
	delete(p.proxies, nodeID)
}

// RemoveNodeAddr removes the address mapping for a node.
func (p *Proxy) RemoveNodeAddr(nodeID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.nodeAddrs, nodeID)
	delete(p.proxies, nodeID)
}

// invalidateStaleProxiesLocked deletes cached reverse proxies for nodes whose
// address in the new membership differs from the currently recorded one (a changed
// address or a departed node). The caller must hold p.mu.
func (p *Proxy) invalidateStaleProxiesLocked(members map[string]string) {
	for id, oldAddr := range p.nodeAddrs {
		if members[id] != oldAddr {
			delete(p.proxies, id)
		}
	}
}

func (p *Proxy) getOrCreateProxy(nodeID, addr string) *httputil.ReverseProxy {
	p.mu.RLock()
	if proxy, ok := p.proxies[nodeID]; ok {
		p.mu.RUnlock()
		return proxy
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check after acquiring write lock
	if proxy, ok := p.proxies[nodeID]; ok {
		return proxy
	}

	target, err := url.Parse(fmt.Sprintf("http://%s", addr))
	if err != nil {
		slog.Error("proxy: invalid target URL", "addr", addr, "error", err)
		target = &url.URL{Scheme: "http", Host: addr}
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	// Fail fast when a shard owner is down or OOM-looping instead of hanging on the
	// default (timeout-less) transport as "upstream node unavailable" (issue #37).
	// A short dial timeout trips quickly on a dead node; ResponseHeaderTimeout
	// bounds a hung one — but only time-to-first-byte, so large-object streaming
	// after the headers is unaffected.
	proxy.Transport = &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Error("proxy: upstream error", "target", nodeID, "addr", addr, "error", err)
		http.Error(w, "upstream node unavailable", http.StatusBadGateway)
	}

	p.proxies[nodeID] = proxy
	return proxy
}

// NodeAddrs returns a copy of the node address map.
func (p *Proxy) NodeAddrs() map[string]string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	addrs := make(map[string]string, len(p.nodeAddrs))
	for k, v := range p.nodeAddrs {
		addrs[k] = v
	}
	return addrs
}

// Ring returns the underlying hash ring.
func (p *Proxy) Ring() *HashRing {
	return p.ring
}

// Placement returns the placement config.
func (p *Proxy) Placement() PlacementConfig {
	return p.placement
}
