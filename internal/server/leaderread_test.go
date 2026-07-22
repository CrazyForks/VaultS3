package server

import (
	"net/http"
	"testing"
)

// TestRequiresLeaderRead pins the routing rule behind the issue #37 fix: a bucket
// listing must be answered by the Raft leader (which has every committed write
// applied) so a list right after a PUT cannot miss the just-written key on a lagging
// follower. Object reads keep their per-key barrier + owner routing, and in-progress
// multipart listing (?uploads) stays node-local (issue #32), so both are excluded.
func TestRequiresLeaderRead(t *testing.T) {
	req := func(method, target string) *http.Request {
		r, err := http.NewRequest(method, "http://host"+target, nil)
		if err != nil {
			t.Fatalf("NewRequest(%s %s): %v", method, target, err)
		}
		return r
	}

	cases := []struct {
		name        string
		method      string
		target      string
		bucket, key string
		wantLeader  bool
	}{
		{"list objects v2", "GET", "/b?list-type=2&prefix=k", "b", "", true},
		{"list objects v1", "GET", "/b", "b", "", true},
		{"list versions", "GET", "/b?versions", "b", "", true},
		{"get bucket location", "GET", "/b?location=", "b", "", true},
		{"list multipart uploads stays node-local", "GET", "/b?uploads", "b", "", false},
		{"object GET keeps owner routing", "GET", "/b/obj", "b", "obj", false},
		{"object HEAD keeps owner routing", "HEAD", "/b/obj", "b", "obj", false},
		{"bucket write is not a read", "PUT", "/b", "b", "", false},
		{"service level list buckets", "GET", "/", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := requiresLeaderRead(req(c.method, c.target), c.bucket, c.key)
			if got != c.wantLeader {
				t.Errorf("requiresLeaderRead(%s %s) = %v, want %v", c.method, c.target, got, c.wantLeader)
			}
		})
	}
}
