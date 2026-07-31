package cluster

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// newTestProxy builds a Proxy with a fixed membership and no raft, so the
// forwarding behaviour can be exercised against real HTTP peers.
func newTestProxy(selfID string, addrs map[string]string, ring *HashRing, replicas int) *FailoverProxy {
	if ring == nil {
		ring = NewHashRing(64)
		for id := range addrs {
			ring.AddNode(id)
		}
	}
	return &FailoverProxy{
		Proxy: &Proxy{
			ring:      ring,
			node:      &Node{cfg: ClusterConfig{NodeID: selfID}},
			placement: PlacementConfig{ReplicaCount: replicas},
			nodeAddrs: addrs,
			proxies:   map[string]*httputil.ReverseProxy{},
		},
	}
}

// hostOf strips the scheme from an httptest server URL, leaving "host:port".
func hostOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	return strings.TrimPrefix(srv.URL, "http://")
}

// TestForwardRetriesConnectionFailure is the core of issue #42: a hop that dies at
// the connection level, before the peer ever saw the request, must be retried
// rather than surfacing to the client as 502 Bad Gateway. The reporter saw these
// clear on retry, which is the retry we should have been doing ourselves.
func TestForwardRetriesConnectionFailure(t *testing.T) {
	var attempts int64
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&attempts, 1) == 1 {
			// Kill the connection the way a peer with a full accept queue or a
			// just-closed keep-alive connection does: no response at all.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("hijack unsupported")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "served")
	}))
	defer peer.Close()

	p := newTestProxy("self", map[string]string{"peer": hostOf(t, peer)}, nil, 1)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/b/k", nil)
	p.ForwardRequest(rec, req, "peer")

	if rec.Code != http.StatusOK {
		t.Fatalf("client saw %d, want 200: a connection-level failure must be retried, not returned as 502", rec.Code)
	}
	if body := rec.Body.String(); body != "served" {
		t.Fatalf("body = %q, want %q", body, "served")
	}
	if got := atomic.LoadInt64(&attempts); got != 2 {
		t.Fatalf("peer saw %d attempts, want 2 (one failed, one retried)", got)
	}
}

// TestForwardDoesNotRetryAfterResponseStarted guards the other side of the retry:
// once bytes are on their way to the client, a mid-stream failure must NOT be
// replayed onto the same response.
func TestForwardDoesNotRetryAfterResponseStarted(t *testing.T) {
	var attempts int64
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&attempts, 1)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		// Then die mid-body.
		hj, _ := w.(http.Hijacker)
		conn, _, err := hj.Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer peer.Close()

	p := newTestProxy("self", map[string]string{"peer": hostOf(t, peer)}, nil, 1)
	rec := httptest.NewRecorder()
	p.ForwardRequest(rec, httptest.NewRequest(http.MethodGet, "/b/k", nil), "peer")

	if got := atomic.LoadInt64(&attempts); got != 1 {
		t.Fatalf("peer saw %d attempts, want 1: a failure after the response started must not be replayed", got)
	}
}

// TestForwardedUploadIsNotCutOffByHeaderTimeout covers the 502-on-PUT half of
// issue #42. A forwarded upload gets its response headers only after the whole body
// has been received and stored, so the read path's 10s ResponseHeaderTimeout was in
// effect a rule that no forwarded PUT may take longer than ten seconds. Anything
// slower — a large object, a slow client, a busy peer — was cut off and became a
// 502 at the client.
func TestForwardedUploadIsNotCutOffByHeaderTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("takes longer than the 10s header timeout by design")
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		// Respond only after the old ResponseHeaderTimeout would have fired.
		time.Sleep(11 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer peer.Close()

	p := newTestProxy("self", map[string]string{"peer": hostOf(t, peer)}, nil, 1)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/b/k", strings.NewReader("payload"))
	p.ForwardRequest(rec, req, "peer")

	if rec.Code != http.StatusOK {
		t.Fatalf("slow forwarded upload got %d, want 200: uploads must not be bounded by the read header timeout", rec.Code)
	}
}

// TestForwardedReadStillFailsFastOnHungPeer is the counterpart: reads keep the
// header timeout, so a hung or OOM-looping owner does not park the client (the
// protection added for issue #37).
func TestForwardedReadStillFailsFastOnHungPeer(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the 10s read header timeout")
	}
	block := make(chan struct{})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // never responds
	}))
	defer func() { close(block); peer.Close() }()

	p := newTestProxy("self", map[string]string{"peer": hostOf(t, peer)}, nil, 1)
	rec := httptest.NewRecorder()
	start := time.Now()
	p.ForwardRequest(rec, httptest.NewRequest(http.MethodGet, "/b/k", nil), "peer")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("hung peer gave %d, want a bounded 503", rec.Code)
	}
	if elapsed := time.Since(start); elapsed > 25*time.Second {
		t.Fatalf("read waited %v on a hung peer, want a bounded fail-fast", elapsed)
	}
}

// TestForwardWithRetryFallsBackToAnotherHolder proves a request is not lost when
// the node it was routed to is unreachable (a restarting pod, in the reporter's
// Kubernetes cluster) and another holder can serve it.
func TestForwardWithRetryFallsBackToAnotherHolder(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "from-holder")
	}))
	defer good.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadAddr := hostOf(t, dead)
	dead.Close() // nothing is listening: every connect is refused

	ring := NewHashRing(64)
	ring.AddNode("a")
	ring.AddNode("b")
	ring.AddNode("c")
	bucket, key := "bkt", "obj"
	holders := ring.GetNodes(bucket, key, 2)

	// Self is the one node that holds nothing; the primary holder is down.
	var self string
	for _, id := range []string{"a", "b", "c"} {
		if id != holders[0] && id != holders[1] {
			self = id
		}
	}
	if self == "" {
		t.Fatal("expected one non-holder among three nodes at replica_count=2")
	}
	addrs := map[string]string{holders[0]: deadAddr, holders[1]: hostOf(t, good)}
	p := newTestProxy(self, addrs, ring, 2)

	rec := httptest.NewRecorder()
	if !p.ForwardWithRetry(rec, httptest.NewRequest(http.MethodGet, "/bkt/obj", nil), bucket, key) {
		t.Fatal("ForwardWithRetry should have handled the request")
	}
	if rec.Code != http.StatusOK || rec.Body.String() != "from-holder" {
		t.Fatalf("got %d %q, want 200 from the surviving holder", rec.Code, rec.Body.String())
	}
}

// TestForwardWithRetryReportsRetryableErrorWhenNoHolderAnswers keeps the failure
// honest: when nothing can serve the request the client must still get an error,
// not a hang or an empty 200. It is reported as a retryable 503 with an S3 error
// document, because the condition is temporary and a bare text 502 reached the
// user as an unexplained "Bad Gateway" (issue #42).
func TestForwardWithRetryReportsRetryableErrorWhenNoHolderAnswers(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadAddr := hostOf(t, dead)
	dead.Close()

	ring := NewHashRing(64)
	ring.AddNode("a")
	ring.AddNode("b")
	holders := ring.GetNodes("bkt", "obj", 2)
	self := "a"
	if holders[0] == self && holders[1] == self {
		t.Fatal("unexpected ring")
	}
	addrs := map[string]string{holders[0]: deadAddr, holders[1]: deadAddr}
	p := newTestProxy("nobody", addrs, ring, 2)

	rec := httptest.NewRecorder()
	p.ForwardWithRetry(rec, httptest.NewRequest(http.MethodGet, "/bkt/obj", nil), "bkt", "obj")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 when no holder can serve", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "<Code>SlowDown</Code>") {
		t.Fatalf("body = %q, want an S3 error document the client can parse and retry", body)
	}
}

// TestRetryableForwardError pins down which failures may be replayed. Getting this
// wrong in either direction is costly: too narrow and the 502s stay, too wide and a
// request the peer is already acting on gets sent twice.
func TestRetryableForwardError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"connection refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{"connection reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, true},
		{"peer closed pooled connection", io.EOF, true},
		{"dial timeout", &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}, true},
		{"response header timeout", os.ErrDeadlineExceeded, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableForwardError(tc.err); got != tc.want {
				t.Fatalf("retryableForwardError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
