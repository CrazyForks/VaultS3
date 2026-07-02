package s3

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// TestRegressionListV2DelimiterCommonPrefixes verifies ListObjectsV2 collapses
// keys sharing a path segment into CommonPrefixes ("folders"). Previously the V2
// handler ignored the delimiter entirely and returned a flat key list, which
// breaks folder browsing for aws-cli, SDK paginators, and the dashboard.
func TestRegressionListV2DelimiterCommonPrefixes(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "delim-bucket"
	doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil).Body.Close()
	for _, k := range []string{"a/1", "a/2", "b/1", "top.txt"} {
		doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+k, []byte("x")).Body.Close()
	}

	resp := doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"?list-type=2&delimiter=/", nil)
	body := readBody(t, resp)
	var out struct {
		Contents       []struct{ Key string }    `xml:"Contents"`
		CommonPrefixes []struct{ Prefix string } `xml:"CommonPrefixes"`
	}
	if err := xml.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	cps := map[string]bool{}
	for _, cp := range out.CommonPrefixes {
		cps[cp.Prefix] = true
	}
	if !cps["a/"] || !cps["b/"] {
		t.Errorf("expected common prefixes a/ and b/, got %v", cps)
	}
	if len(out.Contents) != 1 || out.Contents[0].Key != "top.txt" {
		t.Errorf("expected only top.txt in contents, got %+v", out.Contents)
	}
}

// TestRegressionRangeGetOmitsChecksum verifies a 206 partial response does not
// carry a whole-object x-amz-checksum-* header. Modern SDKs (boto3 >= 1.36,
// aws-cli v2) validate that header against the bytes they receive, so a
// whole-object checksum on a range response makes every range download fail.
func TestRegressionRangeGetOmitsChecksum(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "ck-bucket"
	doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil).Body.Close()

	body := []byte("0123456789")
	sum := sha256.Sum256(body)
	ck := base64.StdEncoding.EncodeToString(sum[:])
	resp := doSignedWithHeaders(t, http.MethodPut, ts.URL+"/"+bucket+"/obj", body,
		map[string]string{"X-Amz-Checksum-Sha256": ck})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put with checksum: %d", resp.StatusCode)
	}

	full := doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/obj", nil)
	full.Body.Close()
	if full.Header.Get("X-Amz-Checksum-Sha256") == "" {
		t.Fatal("full GET should include the checksum header")
	}

	rng := doSignedWithHeaders(t, http.MethodGet, ts.URL+"/"+bucket+"/obj", nil,
		map[string]string{"Range": "bytes=2-5"})
	rng.Body.Close()
	if rng.StatusCode != http.StatusPartialContent {
		t.Fatalf("range GET: expected 206, got %d", rng.StatusCode)
	}
	if got := rng.Header.Get("X-Amz-Checksum-Sha256"); got != "" {
		t.Errorf("range GET must omit the whole-object checksum header, got %q", got)
	}
}

// TestRegressionMultipartMissingPartPreservesObject verifies a
// CompleteMultipartUpload referencing a missing part fails WITHOUT destroying a
// pre-existing object at the same key. Previously the non-encrypted path wrote
// directly to the final object path and os.Remove'd it on a missing part,
// deleting whatever was already stored there.
func TestRegressionMultipartMissingPartPreservesObject(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "mp-preserve"
	key := "important.txt"
	doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil).Body.Close()
	doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/"+key, []byte("original-data")).Body.Close()

	resp := doSigned(t, http.MethodPost, ts.URL+"/"+bucket+"/"+key+"?uploads", nil)
	var initResult initiateResult
	xml.NewDecoder(resp.Body).Decode(&initResult)
	resp.Body.Close()
	uploadID := initResult.UploadID

	resp = doSigned(t, http.MethodPut,
		fmt.Sprintf("%s/%s/%s?uploadId=%s&partNumber=1", ts.URL, bucket, key, uploadID),
		[]byte("newpartdata"))
	etag1 := resp.Header.Get("ETag")
	resp.Body.Close()

	// Reference part 1 AND a part 2 that was never uploaded.
	completeXML := fmt.Sprintf(`<CompleteMultipartUpload>
		<Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part>
		<Part><PartNumber>2</PartNumber><ETag>"deadbeef"</ETag></Part>
	</CompleteMultipartUpload>`, etag1)
	resp = doSigned(t, http.MethodPost,
		fmt.Sprintf("%s/%s/%s?uploadId=%s", ts.URL, bucket, key, uploadID),
		[]byte(completeXML))
	status := resp.StatusCode
	resp.Body.Close()
	if status == http.StatusOK {
		t.Fatal("complete with a missing part must not succeed")
	}

	get := doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/"+key, nil)
	got := readBody(t, get)
	if got != "original-data" {
		t.Errorf("pre-existing object corrupted by failed multipart complete: got %q, want %q", got, "original-data")
	}
}

// TestRegressionWORMBlocksNonVersionedDelete verifies object lock (retention and
// legal hold) is enforced on the non-versioned delete path. Previously that path
// skipped the lock check entirely, so a COMPLIANCE-locked object could be deleted,
// defeating WORM.
func TestRegressionWORMBlocksNonVersionedDelete(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "worm-bucket"
	doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil).Body.Close()
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	// Object under COMPLIANCE retention (set via the retention API): delete refused.
	doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/locked", []byte("immutable")).Body.Close()
	retXML := fmt.Sprintf("<Retention><Mode>COMPLIANCE</Mode><RetainUntilDate>%s</RetainUntilDate></Retention>", until)
	resp := doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/locked?retention", []byte(retXML))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put retention: %d", resp.StatusCode)
	}
	del := doSigned(t, http.MethodDelete, ts.URL+"/"+bucket+"/locked", nil)
	status := del.StatusCode
	del.Body.Close()
	if status != http.StatusForbidden {
		t.Fatalf("delete of COMPLIANCE-locked object: expected 403, got %d", status)
	}
	get := doSigned(t, http.MethodGet, ts.URL+"/"+bucket+"/locked", nil)
	if body := readBody(t, get); body != "immutable" {
		t.Errorf("locked object was removed despite retention: got %q", body)
	}

	// Object under legal hold: delete must be refused.
	doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/held", []byte("x")).Body.Close()
	lhXML := "<LegalHold><Status>ON</Status></LegalHold>"
	doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/held?legal-hold", []byte(lhXML)).Body.Close()
	del = doSigned(t, http.MethodDelete, ts.URL+"/"+bucket+"/held", nil)
	s2 := del.StatusCode
	del.Body.Close()
	if s2 != http.StatusForbidden {
		t.Fatalf("delete under legal hold: expected 403, got %d", s2)
	}

	// Retention set via inline PutObject headers (the common SDK pattern) on a
	// non-versioned bucket must also be honored.
	doSignedWithHeaders(t, http.MethodPut, ts.URL+"/"+bucket+"/inline", []byte("y"),
		map[string]string{
			"X-Amz-Object-Lock-Mode":              "COMPLIANCE",
			"X-Amz-Object-Lock-Retain-Until-Date": until,
		}).Body.Close()
	del = doSigned(t, http.MethodDelete, ts.URL+"/"+bucket+"/inline", nil)
	s3c := del.StatusCode
	del.Body.Close()
	if s3c != http.StatusForbidden {
		t.Fatalf("delete of inline-header-locked object: expected 403, got %d", s3c)
	}
}

// TestRegressionS3SelectEventStream verifies SelectObjectContent returns a valid
// AWS event stream (framed Records/Stats/End messages with correct CRCs) rather
// than raw output. Previously it wrote raw CSV/JSON, which no S3 SDK can parse
// (they fail with a checksum-mismatch on the event-stream prelude). Also checks
// that CAST(...) in the predicate resolves to the column.
func TestRegressionS3SelectEventStream(t *testing.T) {
	ts := newIntegrationServer(t)
	bucket := "select-bucket"
	doSigned(t, http.MethodPut, ts.URL+"/"+bucket, nil).Body.Close()
	doSigned(t, http.MethodPut, ts.URL+"/"+bucket+"/data.csv",
		[]byte("name,age\nalice,30\nbob,25\ncarol,40\n")).Body.Close()

	body := `<SelectObjectContentRequest><Expression>SELECT * FROM s3object s WHERE CAST(s.age AS INT) &gt; 28</Expression>` +
		`<ExpressionType>SQL</ExpressionType>` +
		`<InputSerialization><CSV><FileHeaderInfo>USE</FileHeaderInfo></CSV></InputSerialization>` +
		`<OutputSerialization><CSV></CSV></OutputSerialization></SelectObjectContentRequest>`
	resp := doSigned(t, http.MethodPost, ts.URL+"/"+bucket+"/data.csv?select&select-type=2", []byte(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("select status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/vnd.amazon.eventstream" {
		t.Fatalf("select content-type = %q, want application/vnd.amazon.eventstream", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	records, types := decodeEventStream(t, raw)
	hasType := func(s string) bool {
		for _, x := range types {
			if x == s {
				return true
			}
		}
		return false
	}
	if !hasType("Records") || !hasType("Stats") || !hasType("End") {
		t.Fatalf("event types = %v, want Records+Stats+End", types)
	}
	got := string(records)
	if !strings.Contains(got, "alice") || !strings.Contains(got, "carol") || strings.Contains(got, "bob") {
		t.Errorf("select result = %q, want alice+carol and not bob", got)
	}
}

// decodeEventStream parses an AWS event stream, validating the prelude and message
// CRC32 of each frame, and returns the concatenated Records payloads plus the list
// of event types seen.
func decodeEventStream(t *testing.T, b []byte) (records []byte, eventTypes []string) {
	t.Helper()
	for len(b) >= 16 {
		total := binary.BigEndian.Uint32(b[0:4])
		hlen := binary.BigEndian.Uint32(b[4:8])
		if int(total) > len(b) || total < 16 {
			t.Fatalf("event stream framing invalid: total=%d have=%d", total, len(b))
		}
		if crc32.ChecksumIEEE(b[0:8]) != binary.BigEndian.Uint32(b[8:12]) {
			t.Fatal("prelude CRC mismatch")
		}
		msg := b[:total]
		if crc32.ChecksumIEEE(msg[:total-4]) != binary.BigEndian.Uint32(msg[total-4:]) {
			t.Fatal("message CRC mismatch")
		}
		headers := msg[12 : 12+hlen]
		payload := msg[12+hlen : total-4]
		et := eventTypeFromHeaders(headers)
		eventTypes = append(eventTypes, et)
		if et == "Records" {
			records = append(records, payload...)
		}
		b = b[total:]
	}
	return records, eventTypes
}

func eventTypeFromHeaders(h []byte) string {
	for len(h) > 3 {
		nl := int(h[0])
		if 1+nl+3 > len(h) {
			break
		}
		name := string(h[1 : 1+nl])
		h = h[1+nl:]
		h = h[1:] // value type byte
		vl := int(binary.BigEndian.Uint16(h[0:2]))
		h = h[2:]
		if vl > len(h) {
			break
		}
		val := string(h[:vl])
		h = h[vl:]
		if name == ":event-type" {
			return val
		}
	}
	return ""
}

// TestRegressionObjectLockAutoVersioning verifies a bucket created with object
// lock enabled auto-enables versioning (required for object lock) and reports its
// true object-lock state, while a plain bucket reports none (404).
func TestRegressionObjectLockAutoVersioning(t *testing.T) {
	ts := newIntegrationServer(t)
	doSignedWithHeaders(t, http.MethodPut, ts.URL+"/lockbkt", nil,
		map[string]string{"X-Amz-Bucket-Object-Lock-Enabled": "true"}).Body.Close()

	vresp := doSigned(t, http.MethodGet, ts.URL+"/lockbkt?versioning", nil)
	vbody := readBody(t, vresp)
	if !strings.Contains(vbody, "<Status>Enabled</Status>") {
		t.Errorf("object-lock bucket should have versioning enabled, got: %s", vbody)
	}
	oresp := doSigned(t, http.MethodGet, ts.URL+"/lockbkt?object-lock", nil)
	oresp.Body.Close()
	if oresp.StatusCode != http.StatusOK {
		t.Errorf("object-lock config on lock bucket: got %d, want 200", oresp.StatusCode)
	}

	doSigned(t, http.MethodPut, ts.URL+"/plainbkt", nil).Body.Close()
	presp := doSigned(t, http.MethodGet, ts.URL+"/plainbkt?object-lock", nil)
	presp.Body.Close()
	if presp.StatusCode != http.StatusNotFound {
		t.Errorf("object-lock config on plain bucket: expected 404, got %d", presp.StatusCode)
	}
}

// TestRegressionUserMetadataLowercase verifies x-amz-meta-* keys are emitted
// lowercased (AWS parity) instead of Title-Cased. Checked at the handler level
// because Go's HTTP client canonicalizes response header keys on receive.
func TestRegressionUserMetadataLowercase(t *testing.T) {
	rec := httptest.NewRecorder()
	setUserMetadataHeaders(rec, &metadata.ObjectMeta{UserMetadata: map[string]string{"MyKey": "v"}})
	if _, ok := rec.Header()["x-amz-meta-mykey"]; !ok {
		t.Errorf("expected lowercase header key x-amz-meta-mykey, got: %v", rec.Header())
	}
	if _, ok := rec.Header()["X-Amz-Meta-Mykey"]; ok {
		t.Errorf("must not emit Title-Cased metadata key")
	}
}

// TestRegressionCanonicalQueryEncodeSpaces verifies the presigned canonical query
// uses %20 for spaces (RFC 3986), not Go's '+', so third-party-signed presigned
// URLs whose query carries a space verify correctly.
func TestRegressionCanonicalQueryEncodeSpaces(t *testing.T) {
	v := url.Values{}
	v.Set("response-content-disposition", "attachment; filename=a b.txt")
	v.Set("X-Amz-Date", "20260101T000000Z")
	got := canonicalQueryEncode(v)
	if strings.Contains(got, "+") {
		t.Errorf("canonical query must not use + for space: %q", got)
	}
	if !strings.Contains(got, "%20") {
		t.Errorf("canonical query must encode space as %%20: %q", got)
	}
	if !strings.HasPrefix(got, "X-Amz-Date=") {
		t.Errorf("canonical query params must be sorted, got: %q", got)
	}
}
