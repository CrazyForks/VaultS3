package migrate

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// stubS3 mimics an S3 source: ListBuckets, paginated ListObjectsV2, and GetObject.
// Objects are an in-memory map keyed by "bucket/key".
func stubS3(t *testing.T, objects map[string][]byte) string {
	t.Helper()

	// Group keys by bucket.
	byBucket := map[string][]string{}
	for path := range objects {
		parts := strings.SplitN(path, "/", 2)
		byBucket[parts[0]] = append(byBucket[parts[0]], parts[1])
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")

		// ListBuckets: GET /
		if r.URL.Path == "/" {
			var b strings.Builder
			b.WriteString(`<ListAllMyBucketsResult><Buckets>`)
			for bucket := range byBucket {
				fmt.Fprintf(&b, `<Bucket><Name>%s</Name></Bucket>`, bucket)
			}
			b.WriteString(`</Buckets></ListAllMyBucketsResult>`)
			io.WriteString(w, b.String())
			return
		}

		trimmed := strings.TrimPrefix(r.URL.Path, "/")

		// ListObjectsV2: GET /{bucket}?list-type=2  (one key per page to exercise paging)
		if r.URL.Query().Get("list-type") == "2" {
			bucket := trimmed
			keys := byBucket[bucket]
			start := 0
			if tok := r.URL.Query().Get("continuation-token"); tok != "" {
				fmt.Sscanf(tok, "%d", &start)
			}
			var b strings.Builder
			b.WriteString(`<ListBucketResult>`)
			if start < len(keys) {
				k := keys[start]
				fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>%d</Size><ETag>"x"</ETag></Contents>`, k, len(objects[bucket+"/"+k]))
			}
			if start+1 < len(keys) {
				fmt.Fprintf(&b, `<IsTruncated>true</IsTruncated><NextContinuationToken>%d</NextContinuationToken>`, start+1)
			} else {
				b.WriteString(`<IsTruncated>false</IsTruncated>`)
			}
			b.WriteString(`</ListBucketResult>`)
			io.WriteString(w, b.String())
			return
		}

		// GetObject: GET /{bucket}/{key}
		if data, ok := objects[trimmed]; ok {
			w.Header().Set("Content-Type", "text/plain")
			w.Write(data)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func newLocal(t *testing.T) (*metadata.Store, storage.Engine) {
	t.Helper()
	base := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(base, "meta.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	eng, err := storage.NewFileSystem(filepath.Join(base, "data"))
	if err != nil {
		t.Fatalf("fs: %v", err)
	}
	return store, eng
}

func waitDone(t *testing.T, m *Manager, id string) *Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		j := m.GetJob(id)
		if j != nil && j.Status != "running" {
			return j
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("migration did not finish in time")
	return nil
}

func TestMigrateCopiesAllObjects(t *testing.T) {
	objects := map[string][]byte{
		"docs/a.txt":     []byte("alpha"),
		"docs/sub/b.txt": []byte("bravo"),
		"media/c.txt":    []byte("charlie"),
	}
	endpoint := stubS3(t, objects)
	store, eng := newLocal(t)
	m := NewManager(store, eng)

	id, err := m.Start(StartConfig{Endpoint: endpoint, AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitDone(t, m, id)

	if job.Status != "completed" {
		t.Fatalf("status=%s err=%s", job.Status, job.Error)
	}
	if job.Copied != 3 || job.Failed != 0 {
		t.Fatalf("copied=%d failed=%d, want 3/0", job.Copied, job.Failed)
	}

	// Every object must exist locally with identical bytes.
	for path, want := range objects {
		parts := strings.SplitN(path, "/", 2)
		bucket, key := parts[0], parts[1]
		if !store.BucketExists(bucket) {
			t.Fatalf("bucket %s not created locally", bucket)
		}
		rc, _, err := eng.GetObject(bucket, key)
		if err != nil {
			t.Fatalf("local GetObject %s/%s: %v", bucket, key, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != string(want) {
			t.Fatalf("object %s mismatch: got %q want %q", path, got, want)
		}
	}
}

func TestMigrateSelectedBucketOnly(t *testing.T) {
	objects := map[string][]byte{
		"keep/a.txt": []byte("a"),
		"skip/b.txt": []byte("b"),
	}
	endpoint := stubS3(t, objects)
	store, eng := newLocal(t)
	m := NewManager(store, eng)

	id, err := m.Start(StartConfig{Endpoint: endpoint, AccessKey: "k", SecretKey: "s", Buckets: []string{"keep"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitDone(t, m, id)

	if job.Copied != 1 {
		t.Fatalf("copied=%d, want 1 (selected bucket only)", job.Copied)
	}
	if !eng.ObjectExists("keep", "a.txt") {
		t.Fatal("selected bucket object missing")
	}
	if store.BucketExists("skip") {
		t.Fatal("non-selected bucket should not have been created")
	}
}

func TestMigrateTestConnection(t *testing.T) {
	endpoint := stubS3(t, map[string][]byte{"b1/x": []byte("1"), "b2/y": []byte("2")})
	m := NewManager(nil, nil) // TestConnection doesn't touch store/engine
	buckets, err := m.TestConnection(StartConfig{Endpoint: endpoint, AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("got %d buckets, want 2: %v", len(buckets), buckets)
	}
}

func TestMigrateBadEndpoint(t *testing.T) {
	m := NewManager(nil, nil)
	if _, err := m.TestConnection(StartConfig{Endpoint: "http://127.0.0.1:1", AccessKey: "k", SecretKey: "s"}); err == nil {
		t.Fatal("expected error connecting to a dead endpoint")
	}
}
