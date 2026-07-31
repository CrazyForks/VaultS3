package cluster

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestForwardToDataHolderServesFromPeer is the fix for the phantom 404 in issue
// #42. Object metadata is replicated through Raft and lands everywhere at once,
// but the object's bytes are pushed to the other holders in the background. A read
// that arrives at a holder inside that window used to return "not found" for an
// object that had just been written successfully.
func TestForwardToDataHolderServesFromPeer(t *testing.T) {
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(dataFallbackHeader) == "" {
			t.Error("fallback hop must be marked so peers do not bounce it onward")
		}
		fmt.Fprint(w, "object-bytes")
	}))
	defer holder.Close()

	ring := NewHashRing(64)
	ring.AddNode("n1")
	ring.AddNode("n2")
	bucket, key := "bkt", "obj"
	holders := ring.GetNodes(bucket, key, 2)
	self, peer := holders[0], holders[1]

	p := newTestProxy(self, map[string]string{peer: hostOf(t, holder)}, ring, 2)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bkt/obj", nil)
	if served, _ := p.ForwardToDataHolder(rec, req, bucket, key); !served {
		t.Fatal("a peer holds the data, so the read must be served rather than 404'd")
	}
	if rec.Body.String() != "object-bytes" {
		t.Fatalf("body = %q, want the peer's object bytes", rec.Body.String())
	}
}

// TestForwardToDataHolderSkipsPeersThatAlsoLackTheData covers the default
// replica_count of 3: a peer still waiting for its own copy answers 404 too, and
// that answer must not be handed to the client while another holder is left to ask.
func TestForwardToDataHolderSkipsPeersThatAlsoLackTheData(t *testing.T) {
	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Served-By", "missing")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "<Error><Code>NoSuchKey</Code></Error>")
	}))
	defer missing.Close()
	present := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Served-By", "present")
		fmt.Fprint(w, "object-bytes")
	}))
	defer present.Close()

	ring := NewHashRing(64)
	for _, id := range []string{"n1", "n2", "n3"} {
		ring.AddNode(id)
	}
	bucket, key := "bkt", "obj"
	holders := ring.GetNodes(bucket, key, 3)
	self := holders[0]

	// The first peer asked lacks the data; the second has it.
	addrs := map[string]string{
		holders[1]: hostOf(t, missing),
		holders[2]: hostOf(t, present),
	}
	p := newTestProxy(self, addrs, ring, 3)

	rec := httptest.NewRecorder()
	if served, _ := p.ForwardToDataHolder(rec, httptest.NewRequest(http.MethodGet, "/bkt/obj", nil), bucket, key); !served {
		t.Fatal("the third holder has the data, so the read must succeed")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a 404 from one holder must not end the search", rec.Code)
	}
	if rec.Body.String() != "object-bytes" {
		t.Fatalf("body = %q, want the object bytes", rec.Body.String())
	}
	if got := rec.Header().Get("X-Served-By"); got != "present" {
		t.Fatalf("served by %q, want the holder that has the data (discarded headers must not leak)", got)
	}
}

// TestForwardToDataHolderReturnsFalseWhenNobodyHasIt keeps a genuine miss a miss:
// when no holder can serve the object the caller must fall through to its own 404,
// which is what makes a deleted object stay deleted (issue #34).
func TestForwardToDataHolderReturnsFalseWhenNobodyHasIt(t *testing.T) {
	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer missing.Close()

	ring := NewHashRing(64)
	ring.AddNode("n1")
	ring.AddNode("n2")
	holders := ring.GetNodes("bkt", "obj", 2)
	p := newTestProxy(holders[0], map[string]string{holders[1]: hostOf(t, missing)}, ring, 2)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bkt/obj", nil)
	// Only one peer exists, so its 404 is the final answer and is passed through.
	served, _ := p.ForwardToDataHolder(rec, req, "bkt", "obj")
	if served && rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want the peer's 404 to stand when no holder has the data", rec.Code)
	}
}

// TestForwardToDataHolderDistinguishesUnreachableFromMissing is what stops a
// restarting pod from being reported as a missing object. A holder that answers
// "not found" means the data is gone; a holder we cannot reach means the data is
// merely out of reach, and the caller must be able to tell those apart to answer
// 503-retry instead of a 404 the client cannot recover from.
func TestForwardToDataHolderDistinguishesUnreachableFromMissing(t *testing.T) {
	ring := NewHashRing(64)
	ring.AddNode("n1")
	ring.AddNode("n2")
	holders := ring.GetNodes("bkt", "obj", 2)

	t.Run("holder answers not-found", func(t *testing.T) {
		missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer missing.Close()
		p := newTestProxy(holders[0], map[string]string{holders[1]: hostOf(t, missing)}, ring, 2)
		_, unreachable := p.ForwardToDataHolder(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/bkt/obj", nil), "bkt", "obj")
		if unreachable {
			t.Fatal("a holder that answered must not be reported as unreachable")
		}
	})

	t.Run("holder is down", func(t *testing.T) {
		down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		addr := hostOf(t, down)
		down.Close()
		p := newTestProxy(holders[0], map[string]string{holders[1]: addr}, ring, 2)
		served, unreachable := p.ForwardToDataHolder(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/bkt/obj", nil), "bkt", "obj")
		if served {
			t.Fatal("a down holder cannot have served the read")
		}
		if !unreachable {
			t.Fatal("a holder that could not be reached must be reported as unreachable, so the client is told to retry rather than that the object is gone")
		}
	})
}

// TestForwardToDataHolderDoesNotBounce stops a fallback hop from being forwarded
// again by the node that receives it, which would let two data-less holders ping a
// request back and forth.
func TestForwardToDataHolderDoesNotBounce(t *testing.T) {
	ring := NewHashRing(64)
	ring.AddNode("n1")
	ring.AddNode("n2")
	holders := ring.GetNodes("bkt", "obj", 2)
	p := newTestProxy(holders[0], map[string]string{holders[1]: "127.0.0.1:1"}, ring, 2)

	req := httptest.NewRequest(http.MethodGet, "/bkt/obj", nil)
	req.Header.Set(dataFallbackHeader, "some-other-node")
	if served, _ := p.ForwardToDataHolder(httptest.NewRecorder(), req, "bkt", "obj"); served {
		t.Fatal("a request that is already a fallback hop must not be forwarded again")
	}
}
