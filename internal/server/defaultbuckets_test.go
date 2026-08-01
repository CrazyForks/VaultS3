package server

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// newTestServer builds a real single-node server on temp dirs, so the bucket
// bootstrap runs against the actual metadata store and storage engine.
func newTestServer(t *testing.T, buckets ...string) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Server.Address = "127.0.0.1"
	cfg.Server.Port = 0
	cfg.Storage.DataDir = filepath.Join(dir, "data")
	cfg.Storage.MetadataDir = filepath.Join(dir, "metadata")
	cfg.Storage.DefaultBuckets = buckets
	cfg.Auth.AdminAccessKey = "vaults3-admin"
	cfg.Auth.AdminSecretKey = "vaults3-secret-change-me"
	cfg.Memory.MaxSearchEntries = 100

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func TestDefaultBucketsCreatedOnStartup(t *testing.T) {
	srv := newTestServer(t, "app-data", "backups")

	if err := srv.startDefaultBuckets(make(chan error, 1)); err != nil {
		t.Fatalf("startDefaultBuckets: %v", err)
	}

	for _, name := range []string{"app-data", "backups"} {
		if !srv.metaStore.BucketExists(name) {
			t.Errorf("bucket %q was not created in metadata", name)
		}
		dir := filepath.Join(srv.cfg.Storage.DataDir, name)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Errorf("bucket %q has no storage directory: %v", name, err)
		}
	}
}

// The whole point of "create if missing": a restart must not disturb a bucket
// that already holds data.
func TestDefaultBucketsLeaveExistingUntouched(t *testing.T) {
	srv := newTestServer(t, "app-data")

	if err := srv.startDefaultBuckets(make(chan error, 1)); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if _, _, err := srv.engine.PutObject("app-data", "keep.txt", strings.NewReader("payload"), 7); err != nil {
		t.Fatalf("put object: %v", err)
	}
	before, err := srv.metaStore.GetBucket("app-data")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}

	// Second startup with the same config.
	if err := srv.startDefaultBuckets(make(chan error, 1)); err != nil {
		t.Fatalf("second start: %v", err)
	}

	after, err := srv.metaStore.GetBucket("app-data")
	if err != nil {
		t.Fatalf("get bucket after restart: %v", err)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("bucket was re-created: CreatedAt %v -> %v", before.CreatedAt, after.CreatedAt)
	}
	data, err := os.ReadFile(filepath.Join(srv.cfg.Storage.DataDir, "app-data", "keep.txt"))
	if err != nil || string(data) != "payload" {
		t.Errorf("existing object did not survive: %q %v", data, err)
	}
}

// A bucket missing its data directory (metadata restored from a snapshot, or a
// cluster node that learned the bucket over Raft) gets the directory back.
func TestDefaultBucketsRestoreMissingDirectory(t *testing.T) {
	srv := newTestServer(t, "app-data")

	if err := srv.startDefaultBuckets(make(chan error, 1)); err != nil {
		t.Fatalf("first start: %v", err)
	}
	dir := filepath.Join(srv.cfg.Storage.DataDir, "app-data")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove dir: %v", err)
	}

	if err := srv.startDefaultBuckets(make(chan error, 1)); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("storage directory was not restored: %v", err)
	}
}

func TestDefaultBucketsRejectInvalidNames(t *testing.T) {
	invalid := []string{"ab", "UPPER", "has_underscore", "-leading", "trailing-", "a..b", strings.Repeat("x", 64)}
	for _, name := range invalid {
		if err := validateDefaultBuckets([]string{name}); err == nil {
			t.Errorf("name %q was accepted, want rejected", name)
		} else if !strings.Contains(err.Error(), "VAULTS3_DEFAULT_BUCKETS") {
			t.Errorf("name %q: error should point at the setting, got %q", name, err)
		}
	}
	for _, name := range []string{"app-data", "backups", "my.bucket.name", "abc", "0k9"} {
		if err := validateDefaultBuckets([]string{name}); err != nil {
			t.Errorf("name %q was rejected: %v", name, err)
		}
	}
}

// An invalid name must stop startup before anything is created, so a typo does
// not leave the deployment half-provisioned.
func TestDefaultBucketsInvalidNameCreatesNothing(t *testing.T) {
	srv := newTestServer(t, "app-data", "Bad_Name")

	err := srv.startDefaultBuckets(make(chan error, 1))
	if err == nil {
		t.Fatal("expected startup to fail on an invalid bucket name")
	}
	if !strings.Contains(err.Error(), "Bad_Name") {
		t.Errorf("error should name the offending bucket, got %q", err)
	}
	if srv.metaStore.BucketExists("app-data") {
		t.Error("a valid bucket was created despite the startup failure")
	}
}

func TestDefaultBucketsNoneConfigured(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.startDefaultBuckets(make(chan error, 1)); err != nil {
		t.Fatalf("startDefaultBuckets with no buckets: %v", err)
	}
	buckets, err := srv.metaStore.ListBuckets()
	if err != nil {
		t.Fatalf("list buckets: %v", err)
	}
	if len(buckets) != 0 {
		t.Errorf("expected no buckets, got %d", len(buckets))
	}
}

// raceStore simulates the cluster case where another node committed the bucket
// between our BucketExists check and our own create: BucketExists says no, the
// create comes back with the leader's "already exists". That must count as
// success, not kill the process.
type raceStore struct {
	metadata.StoreAPI
	createErr error
	creates   int
}

func (r *raceStore) BucketExists(string) bool { return false }
func (r *raceStore) CreateBucket(string) error {
	r.creates++
	return r.createErr
}

func TestDefaultBucketsToleratesConcurrentCreate(t *testing.T) {
	srv := newTestServer(t, "app-data")
	// Exactly the string a forwarded write returns through the leader.
	rs := &raceStore{StoreAPI: srv.metaStore,
		createErr: errors.New("cluster: leader rejected forwarded write (500): bucket already exists: app-data")}
	srv.metaStore = rs

	if err := srv.startDefaultBuckets(make(chan error, 1)); err != nil {
		t.Fatalf("a concurrent create by another node must not fail startup, got: %v", err)
	}
	if rs.creates != 1 {
		t.Errorf("CreateBucket called %d times, want 1", rs.creates)
	}
}

// Any other create failure is real and must stop startup.
func TestDefaultBucketsFailOnRealCreateError(t *testing.T) {
	srv := newTestServer(t, "app-data")
	srv.metaStore = &raceStore{StoreAPI: srv.metaStore, createErr: errors.New("disk I/O error")}

	err := srv.startDefaultBuckets(make(chan error, 1))
	if err == nil {
		t.Fatal("expected startup to fail when the bucket cannot be created")
	}
	if !strings.Contains(err.Error(), "app-data") || !strings.Contains(err.Error(), "disk I/O error") {
		t.Errorf("error should name the bucket and the cause, got %q", err)
	}
}

// A name repeated in the list is created once and does not trip the exists check.
func TestDefaultBucketsDuplicateNames(t *testing.T) {
	srv := newTestServer(t, "app-data", "app-data", "backups")
	if err := srv.startDefaultBuckets(make(chan error, 1)); err != nil {
		t.Fatalf("duplicate names should be harmless, got: %v", err)
	}
	buckets, err := srv.metaStore.ListBuckets()
	if err != nil {
		t.Fatalf("list buckets: %v", err)
	}
	if len(buckets) != 2 {
		t.Errorf("got %d buckets, want 2 (duplicate collapsed)", len(buckets))
	}
}

// If the storage directory cannot be created, the metadata entry is rolled back
// so a retry (or the next boot) is not blocked by a half-made bucket.
func TestDefaultBucketsRollbackOnStorageFailure(t *testing.T) {
	srv := newTestServer(t, "app-data")
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only directory would not stop mkdir")
	}
	if err := os.Chmod(srv.cfg.Storage.DataDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(srv.cfg.Storage.DataDir, 0o755) })

	err := srv.startDefaultBuckets(make(chan error, 1))
	if err == nil {
		t.Fatal("expected startup to fail when the data directory is not writable")
	}
	if srv.metaStore.BucketExists("app-data") {
		t.Error("bucket was left in metadata without its storage directory")
	}
}
