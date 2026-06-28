package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
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

// TestMigrateRetriesTransient: a transient 503 on the first GET is retried and
// the object still copies successfully (issue #6).
func TestMigrateRetriesTransient(t *testing.T) {
	var mu sync.Mutex
	attempts := map[string]int{}
	data := []byte("payload that survives a flaky first fetch")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Path == "/" {
			io.WriteString(w, `<ListAllMyBucketsResult><Buckets><Bucket><Name>b</Name></Bucket></Buckets></ListAllMyBucketsResult>`)
			return
		}
		if r.URL.Query().Get("list-type") == "2" {
			fmt.Fprintf(w, `<ListBucketResult><Contents><Key>flaky.txt</Key><Size>%d</Size></Contents><IsTruncated>false</IsTruncated></ListBucketResult>`, len(data))
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/b/")
		mu.Lock()
		attempts[key]++
		n := attempts[key]
		mu.Unlock()
		if n == 1 {
			http.Error(w, "slow down", http.StatusServiceUnavailable) // transient
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write(data)
	}))
	defer srv.Close()

	store, eng := newLocal(t)
	m := NewManager(store, eng)
	id, err := m.Start(StartConfig{Endpoint: srv.URL, AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job := waitDone(t, m, id)

	if job.Status != "completed" || job.Copied != 1 || job.Failed != 0 {
		t.Fatalf("after retry: status=%s copied=%d failed=%d, want completed/1/0", job.Status, job.Copied, job.Failed)
	}
	mu.Lock()
	n := attempts["flaky.txt"]
	mu.Unlock()
	if n < 2 {
		t.Fatalf("transient 503 should have been retried (>=2 GETs), got %d", n)
	}
	if !eng.ObjectExists("b", "flaky.txt") {
		t.Fatal("object should exist after successful retry")
	}
}

// TestMigratePermanentErrorNotRetried: a 404 is permanent and must NOT be retried.
func TestMigratePermanentErrorNotRetried(t *testing.T) {
	var mu sync.Mutex
	gets := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Path == "/" {
			io.WriteString(w, `<ListAllMyBucketsResult><Buckets><Bucket><Name>b</Name></Bucket></Buckets></ListAllMyBucketsResult>`)
			return
		}
		if r.URL.Query().Get("list-type") == "2" {
			io.WriteString(w, `<ListBucketResult><Contents><Key>gone.txt</Key><Size>5</Size></Contents><IsTruncated>false</IsTruncated></ListBucketResult>`)
			return
		}
		mu.Lock()
		gets++
		mu.Unlock()
		http.Error(w, "not found", http.StatusNotFound) // permanent
	}))
	defer srv.Close()

	store, eng := newLocal(t)
	m := NewManager(store, eng)
	id, _ := m.Start(StartConfig{Endpoint: srv.URL, AccessKey: "k", SecretKey: "s"})
	job := waitDone(t, m, id)
	_ = eng

	if job.Failed != 1 {
		t.Fatalf("expected 1 failed object (404), got failed=%d", job.Failed)
	}
	mu.Lock()
	n := gets
	mu.Unlock()
	if n != 1 {
		t.Fatalf("404 must not be retried — expected 1 GET, got %d", n)
	}
}

func TestMigrateBadEndpoint(t *testing.T) {
	m := NewManager(nil, nil)
	if _, err := m.TestConnection(StartConfig{Endpoint: "http://127.0.0.1:1", AccessKey: "k", SecretKey: "s"}); err == nil {
		t.Fatal("expected error connecting to a dead endpoint")
	}
}

// TestGetObjectSpecialCharsSignature is a regression guard for issue #9: object
// keys containing '&', '$', or spaces produced a SigV4 SignatureDoesNotMatch
// because the path was escaped with Go's default rules (which leave sub-delims
// literal) instead of the strict AWS canonical encoding. The stub server here
// recomputes the signature the strict (AWS) way and rejects any mismatch.
func TestGetObjectSpecialCharsSignature(t *testing.T) {
	const access, secret, region = "AKIATEST", "secretkey1234567890", "us-east-1"
	const bucket = "bucket1"
	const key = "1027708 Artik & ASTI feat Artyom $ Kacher/soft-slidertick4.wav"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "Signature=" + strictSigV4(r, secret, region)
		if !strings.HasSuffix(r.Header.Get("Authorization"), want) {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<?xml version="1.0"?><Error><Code>SignatureDoesNotMatch</Code></Error>`)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		io.WriteString(w, "AUDIO")
	}))
	defer srv.Close()

	body, _, _, err := NewSource(srv.URL, access, secret, region, 10).GetObject(bucket, key)
	if err != nil {
		t.Fatalf("GetObject with special chars (&, $, space): %v", err)
	}
	defer body.Close()
	if b, _ := io.ReadAll(body); string(b) != "AUDIO" {
		t.Fatalf("body = %q, want AUDIO", b)
	}
}

// strictSigV4 recomputes the request's SigV4 signature using strict AWS canonical
// URI encoding — the encoding a real S3 server uses to validate the request.
func strictSigV4(r *http.Request, secret, region string) string {
	amzDate := r.Header.Get("X-Amz-Date")
	dateStr := amzDate[:8]
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", r.Host, payloadHash, amzDate)
	canonReq := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s", r.Method, uriEncodePath(r.URL.Path), "", canonHeaders, signedHeaders, payloadHash)
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStr, region)
	hash := sha256.Sum256([]byte(canonReq))
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s", amzDate, scope, hex.EncodeToString(hash[:]))
	return hex.EncodeToString(hmacSHA256(deriveKey(secret, dateStr, region, "s3"), []byte(stringToSign)))
}

// TestMigrateCancel verifies an in-progress migration can be cancelled: the job
// ends in "cancelled" status, stops copying (copied < total), and a second
// cancel on the now-finished job is a no-op. (issue #8)
func TestMigrateCancel(t *testing.T) {
	const n = 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Path == "/" {
			io.WriteString(w, `<ListAllMyBucketsResult><Buckets><Bucket><Name>b</Name></Bucket></Buckets></ListAllMyBucketsResult>`)
			return
		}
		if r.URL.Query().Get("list-type") == "2" {
			var sb strings.Builder
			sb.WriteString(`<ListBucketResult>`)
			for i := 0; i < n; i++ {
				fmt.Fprintf(&sb, `<Contents><Key>obj-%03d</Key><Size>1</Size></Contents>`, i)
			}
			sb.WriteString(`<IsTruncated>false</IsTruncated></ListBucketResult>`)
			io.WriteString(w, sb.String())
			return
		}
		time.Sleep(5 * time.Millisecond) // slow each GET so cancel can interleave
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("x"))
	}))
	defer srv.Close()

	store, eng := newLocal(t)
	m := NewManager(store, eng)
	id, err := m.Start(StartConfig{Endpoint: srv.URL, AccessKey: "k", SecretKey: "s"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	time.Sleep(40 * time.Millisecond) // let a few objects copy
	if !m.Cancel(id) {
		t.Fatal("Cancel returned false for a running job")
	}
	job := waitDone(t, m, id)

	if job.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", job.Status)
	}
	if job.Copied >= n {
		t.Fatalf("copied %d of %d — cancel did not stop the migration", job.Copied, n)
	}
	if m.Cancel(id) {
		t.Fatal("Cancel on a finished job should return false")
	}
}

// TestMigrateRejectsDuplicate verifies a second migration of the same source +
// buckets is rejected while the first is still running (issue #8 — double-click
// guard).
func TestMigrateRejectsDuplicate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Path == "/" {
			io.WriteString(w, `<ListAllMyBucketsResult><Buckets><Bucket><Name>b</Name></Bucket></Buckets></ListAllMyBucketsResult>`)
			return
		}
		if r.URL.Query().Get("list-type") == "2" {
			var sb strings.Builder
			sb.WriteString(`<ListBucketResult>`)
			for i := 0; i < 200; i++ {
				fmt.Fprintf(&sb, `<Contents><Key>obj-%03d</Key><Size>1</Size></Contents>`, i)
			}
			sb.WriteString(`<IsTruncated>false</IsTruncated></ListBucketResult>`)
			io.WriteString(w, sb.String())
			return
		}
		time.Sleep(5 * time.Millisecond)
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("x"))
	}))
	defer srv.Close()

	store, eng := newLocal(t)
	m := NewManager(store, eng)
	cfg := StartConfig{Endpoint: srv.URL, AccessKey: "k", SecretKey: "s", Buckets: []string{"b"}}

	id1, err := m.Start(cfg)
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if _, err := m.Start(cfg); err == nil {
		t.Fatal("expected duplicate migration to be rejected while the first is running")
	}
	// after cancelling the first, a new one is allowed again
	m.Cancel(id1)
	waitDone(t, m, id1)
	if _, err := m.Start(cfg); err != nil {
		t.Fatalf("Start after first finished should be allowed: %v", err)
	}
}
