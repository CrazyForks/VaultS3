package server

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/s3"
)

// A clustered node cannot create a bucket until the cluster has a leader to
// commit the Raft write, and a pod that starts before its peers may wait a while
// for one. Retry instead of killing a container that is simply early.
const (
	defaultBucketAttempts   = 10
	defaultBucketRetryDelay = 3 * time.Second
)

// startDefaultBuckets creates the buckets named by storage.default_buckets
// (VAULTS3_DEFAULT_BUCKETS) so a container deployment gets its first buckets
// without an init container or a separate S3 client (issue #45).
//
// Names are checked synchronously, so a typo fails the process immediately.
// Creation itself is synchronous on a single node; clustered, it moves to a
// goroutine that waits for a leader and reports failure on errCh, which Run
// treats exactly like a listener error.
func (s *Server) startDefaultBuckets(errCh chan<- error) error {
	if len(s.cfg.Storage.DefaultBuckets) == 0 {
		return nil
	}
	if err := validateDefaultBuckets(s.cfg.Storage.DefaultBuckets); err != nil {
		return err
	}

	if s.clusterNode == nil {
		return s.ensureDefaultBuckets()
	}

	go func() {
		var err error
		for attempt := 1; attempt <= defaultBucketAttempts; attempt++ {
			if err = s.clusterNode.WaitForLeader(); err == nil {
				if err = s.ensureDefaultBuckets(); err == nil {
					return
				}
			}
			slog.Warn("default buckets: cluster not ready, retrying",
				"attempt", attempt, "of", defaultBucketAttempts, "error", err)
			time.Sleep(defaultBucketRetryDelay)
		}
		errCh <- fmt.Errorf("default buckets: gave up after %d attempts: %w", defaultBucketAttempts, err)
	}()
	return nil
}

// validateDefaultBuckets rejects a bad name up front, so a typo in
// VAULTS3_DEFAULT_BUCKETS stops startup with a message naming the bucket and the
// rule it broke.
func validateDefaultBuckets(names []string) error {
	for _, name := range names {
		if err := s3.ValidateBucketName(name); err != nil {
			return fmt.Errorf("default bucket %q: %w (from storage.default_buckets / VAULTS3_DEFAULT_BUCKETS)", name, err)
		}
	}
	return nil
}

// ensureDefaultBuckets creates each configured bucket that does not exist yet,
// through the same metadata + storage path as PUT /{bucket}, so they are ordinary
// buckets in every respect. A bucket that already exists is left exactly as it
// is: nothing is reset, re-created, or reconfigured.
func (s *Server) ensureDefaultBuckets() error {
	created, existing := 0, 0
	for _, name := range s.cfg.Storage.DefaultBuckets {
		if s.metaStore.BucketExists(name) {
			// Still make sure the data directory is there: a metadata store restored
			// without its data dir, or a node that learned the bucket over Raft rather
			// than creating it, has the bucket but not the folder.
			if err := s.engine.CreateBucketDir(name); err != nil {
				return fmt.Errorf("default bucket %q: create storage directory: %w", name, err)
			}
			existing++
			continue
		}

		if err := s.metaStore.CreateBucket(name); err != nil {
			// Another node in the cluster (or a racing restart) got there first — that
			// is the desired end state, not a failure.
			if !strings.Contains(err.Error(), "already exists") {
				return fmt.Errorf("default bucket %q: %w", name, err)
			}
			existing++
			continue
		}
		if err := s.engine.CreateBucketDir(name); err != nil {
			s.metaStore.DeleteBucket(name) // rollback, same as the S3 handler
			return fmt.Errorf("default bucket %q: create storage directory: %w", name, err)
		}
		slog.Info("default bucket created", "bucket", name)
		created++
	}

	slog.Info("default buckets ready",
		"configured", len(s.cfg.Storage.DefaultBuckets),
		"created", created,
		"already_existed", existing,
	)
	return nil
}
