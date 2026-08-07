package s3

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Issue #48. UploadPart used to write straight to the part path: os.Create
// truncated whatever was already there, and the copy-error path deleted it. So a
// RETRY of a part that had already succeeded destroyed the good data while the
// earlier success's part metadata survived. The upload was then permanently
// un-completable — ListParts still advertised the part, CompleteMultipartUpload
// could not open it and answered 400 InvalidPart, and no retry recovered.
//
// Any failed transfer was enough to trigger it (a dropped connection, a read
// timeout, a proxy or client retry), which is why the reporter saw it track
// dropped connections and memory pressure rather than data.

// startUpload creates a multipart upload and returns its ID.
func startUpload(t *testing.T, ts *httptest.Server, bucket, key string) string {
	t.Helper()
	resp := doSigned(t, http.MethodPost, ts.URL+"/"+bucket+"/"+key+"?uploads", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create upload: status %d", resp.StatusCode)
	}
	defer resp.Body.Close()
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&init); err != nil {
		t.Fatalf("decode upload id: %v", err)
	}
	return init.UploadID
}

func completeBody(partNum int, etag string) []byte {
	return []byte(fmt.Sprintf(
		"<CompleteMultipartUpload><Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>",
		partNum, etag))
}

// A part upload that dies mid-body must leave an earlier good copy of that part
// exactly as it was. This is the defect: before the fix the retry's os.Create
// truncated the good part and the error path removed it.
func TestFailedPartRetryDoesNotDestroyTheGoodPart(t *testing.T) {
	_, store, engine, ts := newObjTestServer(t)
	bucket, key := "b", "big.bin"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	uploadID := startUpload(t, ts, bucket, key)

	// 1. Upload part 1 successfully.
	payload := strings.Repeat("A", 64*1024)
	resp := doSigned(t, http.MethodPut,
		ts.URL+"/"+bucket+"/"+key+"?partNumber=1&uploadId="+uploadID, []byte(payload))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first UploadPart: status %d", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	resp.Body.Close()
	if etag == "" {
		t.Fatal("first UploadPart returned no ETag")
	}

	partPath := filepath.Join(engine.DataDir(), ".multipart", uploadID, "part-00001")
	if _, err := os.Stat(partPath); err != nil {
		t.Fatalf("precondition: part file should exist after a successful upload: %v", err)
	}

	// 2. Retry the same part, but the body dies partway through — a dropped
	//    connection, a proxy retry that is cut short, a client timeout.
	req, err := http.NewRequest(http.MethodPut,
		ts.URL+"/"+bucket+"/"+key+"?partNumber=1&uploadId="+uploadID,
		io.NopCloser(&failingReader{data: []byte(payload), failAfter: 1024}))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.ContentLength = int64(len(payload))
	// Sign for the FULL payload so the request authenticates, then swap in a body
	// that dies partway: signV4Request replaces r.Body, so the failing reader has
	// to be installed after it or the test silently sends the whole payload and
	// proves nothing.
	signV4Request(req, testAccessKey, testSecretKey, []byte(payload))
	req.Body = io.NopCloser(&failingReader{data: []byte(payload), failAfter: 1024})
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}

	// 3. The good part must still be there, and the upload must still complete.
	if _, err := os.Stat(partPath); err != nil {
		t.Fatalf("a failed retry destroyed a part that had already succeeded: %v", err)
	}

	resp = doSigned(t, http.MethodPost,
		ts.URL+"/"+bucket+"/"+key+"?uploadId="+uploadID, completeBody(1, etag))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("Complete after a failed part retry: status %d, body %s\n"+
			"this is issue #48: ListParts still advertises the part but its data was destroyed",
			resp.StatusCode, body)
	}
	resp.Body.Close()

	// And the object must hold the ORIGINAL bytes, not a truncated retry.
	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET completed object: status %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(got) != len(payload) {
		t.Fatalf("completed object is %d bytes, want %d — a failed retry corrupted the part",
			len(got), len(payload))
	}
}

// A part upload that dies mid-body when there is NO previous copy must leave
// nothing behind, so the part is genuinely absent rather than silently short.
func TestFailedFirstPartLeavesNoPartialFile(t *testing.T) {
	_, store, engine, ts := newObjTestServer(t)
	bucket, key := "b", "p.bin"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	uploadID := startUpload(t, ts, bucket, key)

	payload := strings.Repeat("B", 64*1024)
	req, err := http.NewRequest(http.MethodPut,
		ts.URL+"/"+bucket+"/"+key+"?partNumber=1&uploadId="+uploadID,
		io.NopCloser(&failingReader{data: []byte(payload), failAfter: 1024}))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.ContentLength = int64(len(payload))
	// Sign for the FULL payload so the request authenticates, then swap in a body
	// that dies partway: signV4Request replaces r.Body, so the failing reader has
	// to be installed after it or the test silently sends the whole payload and
	// proves nothing.
	signV4Request(req, testAccessKey, testSecretKey, []byte(payload))
	req.Body = io.NopCloser(&failingReader{data: []byte(payload), failAfter: 1024})
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}

	partPath := filepath.Join(engine.DataDir(), ".multipart", uploadID, "part-00001")
	if _, err := os.Stat(partPath); !os.IsNotExist(err) {
		t.Fatalf("a failed part upload left a partial file in place (err=%v)", err)
	}

	// And no temp file may be left behind. The client's call returns the moment it
	// aborts the body, while the server is still unwinding, so give the cleanup a
	// moment rather than racing it.
	dir := filepath.Join(engine.DataDir(), ".multipart", uploadID)
	var leftover []string
	for i := 0; i < 100; i++ {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read upload dir: %v", err)
		}
		leftover = leftover[:0]
		for _, e := range entries {
			leftover = append(leftover, e.Name())
		}
		if len(leftover) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("failed part upload left %v behind", leftover)
}

// Re-uploading a part with different content must replace it, so a legitimate
// retry after a genuine failure still works.
func TestSuccessfulPartRetryReplacesTheEarlierCopy(t *testing.T) {
	_, store, _, ts := newObjTestServer(t)
	bucket, key := "b", "r.bin"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	uploadID := startUpload(t, ts, bucket, key)

	first := strings.Repeat("X", 32*1024)
	resp := doSigned(t, http.MethodPut,
		ts.URL+"/"+bucket+"/"+key+"?partNumber=1&uploadId="+uploadID, []byte(first))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first UploadPart: status %d", resp.StatusCode)
	}
	resp.Body.Close()

	second := strings.Repeat("Y", 48*1024)
	resp = doSigned(t, http.MethodPut,
		ts.URL+"/"+bucket+"/"+key+"?partNumber=1&uploadId="+uploadID, []byte(second))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second UploadPart: status %d", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	resp.Body.Close()

	resp = doSigned(t, http.MethodPost,
		ts.URL+"/"+bucket+"/"+key+"?uploadId="+uploadID, completeBody(1, etag))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Complete: status %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != second {
		t.Fatalf("object holds %d bytes, want the re-uploaded part's %d", len(got), len(second))
	}
}

// A part whose data is genuinely gone must still report InvalidPart, and the
// completion must not half-write an object.
func TestGenuinelyMissingPartStillReportsInvalidPart(t *testing.T) {
	_, store, engine, ts := newObjTestServer(t)
	bucket, key := "b", "m.bin"
	if err := store.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	uploadID := startUpload(t, ts, bucket, key)

	resp := doSigned(t, http.MethodPut,
		ts.URL+"/"+bucket+"/"+key+"?partNumber=1&uploadId="+uploadID, []byte("data"))
	etag := resp.Header.Get("ETag")
	resp.Body.Close()

	// Exactly the state the reporter was left in.
	if err := os.Remove(filepath.Join(engine.DataDir(), ".multipart", uploadID, "part-00001")); err != nil {
		t.Fatalf("remove part: %v", err)
	}

	resp = doSigned(t, http.MethodPost,
		ts.URL+"/"+bucket+"/"+key+"?uploadId="+uploadID, completeBody(1, etag))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Complete with a genuinely missing part: status %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "InvalidPart") {
		t.Fatalf("expected InvalidPart, got %s", body)
	}
}

// failingReader yields data then fails, standing in for a connection that drops
// partway through a part upload.
type failingReader struct {
	data      []byte
	failAfter int
	read      int
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.read >= f.failAfter {
		return 0, io.ErrUnexpectedEOF
	}
	n := len(p)
	if remaining := f.failAfter - f.read; n > remaining {
		n = remaining
	}
	if n > len(f.data)-f.read {
		n = len(f.data) - f.read
	}
	if n <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	copy(p, f.data[f.read:f.read+n])
	f.read += n
	return n, nil
}
