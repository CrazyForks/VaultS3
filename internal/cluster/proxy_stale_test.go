package cluster

import (
	"net/http/httputil"
	"net/url"
	"testing"
)

// TestInvalidateStaleProxiesLocked verifies the reverse-proxy cache is dropped for
// nodes whose address changed (pod restart) or that left the cluster, while an
// unchanged node keeps its cached proxy. Without this, a node would keep routing to
// a dead address forever after a peer's IP changes.
func TestInvalidateStaleProxiesLocked(t *testing.T) {
	mk := func() *httputil.ReverseProxy {
		u, _ := url.Parse("http://x")
		return httputil.NewSingleHostReverseProxy(u)
	}
	p := &Proxy{
		nodeAddrs: map[string]string{
			"n1": "10.0.0.1:9000",
			"n2": "10.0.0.2:9000",
			"n3": "10.0.0.3:9000",
		},
		proxies: map[string]*httputil.ReverseProxy{
			"n1": mk(), "n2": mk(), "n3": mk(),
		},
	}

	// n1 unchanged, n2's address changed, n3 left the membership.
	members := map[string]string{
		"n1": "10.0.0.1:9000",
		"n2": "10.9.9.9:9000",
	}
	p.invalidateStaleProxiesLocked(members)

	if _, ok := p.proxies["n1"]; !ok {
		t.Error("n1 (unchanged) should keep its cached proxy")
	}
	if _, ok := p.proxies["n2"]; ok {
		t.Error("n2 (address changed) should have its cached proxy invalidated")
	}
	if _, ok := p.proxies["n3"]; ok {
		t.Error("n3 (departed) should have its cached proxy invalidated")
	}
}
