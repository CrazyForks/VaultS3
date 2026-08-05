package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// multipartPeerClient talks to peers over the cluster channel. These calls sit in
// the path of a client request, so the timeout is short: a slow or dead peer must
// degrade the answer, never hang the caller.
var multipartPeerClient = &http.Client{Timeout: 5 * time.Second}

// collectPeerUploads asks every other node for its in-progress uploads in a
// bucket. ListMultipartUploads is routed to a single node while uploads live on
// whichever node owns each object key, so without this the listing showed only
// the fraction of uploads that happened to hash to the listing node and hid the
// rest completely (issue #47 bug B).
//
// Peers are queried concurrently and a failing peer is skipped with a warning
// rather than failing the listing: a partial list is what S3 clients can act on,
// and the alternative (erroring) would make one sick node break every bucket
// listing in the cluster.
func collectPeerUploads(addrs map[string]string, selfID, scheme, secret, bucket string) []metadata.MultipartUpload {
	var (
		mu  sync.Mutex
		out []metadata.MultipartUpload
		wg  sync.WaitGroup
	)
	for id, addr := range addrs {
		if id == selfID || addr == "" {
			continue
		}
		wg.Add(1)
		go func(id, addr string) {
			defer wg.Done()
			u := scheme + "://" + addr + "/cluster/multipart-list?bucket=" + url.QueryEscape(bucket)
			req, err := http.NewRequest(http.MethodGet, u, nil)
			if err != nil {
				return
			}
			if secret != "" {
				req.Header.Set("X-Cluster-Secret", secret)
			}
			resp, err := multipartPeerClient.Do(req)
			if err != nil {
				slog.Warn("multipart listing: peer unreachable, its uploads are missing from this listing",
					"node", id, "bucket", bucket, "error", err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				slog.Warn("multipart listing: peer returned an error, its uploads are missing from this listing",
					"node", id, "bucket", bucket, "status", resp.StatusCode)
				return
			}
			var got []metadata.MultipartUpload
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				return
			}
			mu.Lock()
			out = append(out, got...)
			mu.Unlock()
		}(id, addr)
	}
	wg.Wait()
	return out
}

// findUploadHolder returns the node ID holding uploadID, or "" when no peer has
// it. Requests naming an upload route by object key, but the upload itself lives
// wherever it was created, so after a ring change the key's new owner has no
// record of it and would answer NoSuchUpload for an upload that is very much
// alive (issue #47 bug B).
//
// The first peer to claim it wins; an upload ID is unique so there is no ambiguity.
func findUploadHolder(addrs map[string]string, selfID, scheme, secret, uploadID string) string {
	var (
		mu     sync.Mutex
		holder string
		wg     sync.WaitGroup
	)
	for id, addr := range addrs {
		if id == selfID || addr == "" {
			continue
		}
		wg.Add(1)
		go func(id, addr string) {
			defer wg.Done()
			u := scheme + "://" + addr + "/cluster/multipart-find?uploadId=" + url.QueryEscape(uploadID)
			req, err := http.NewRequest(http.MethodGet, u, nil)
			if err != nil {
				return
			}
			if secret != "" {
				req.Header.Set("X-Cluster-Secret", secret)
			}
			resp, err := multipartPeerClient.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return
			}
			mu.Lock()
			if holder == "" {
				holder = id
			}
			mu.Unlock()
		}(id, addr)
	}
	wg.Wait()
	return holder
}
