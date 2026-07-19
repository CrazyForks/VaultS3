package s3

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// TestAuthReverseProxySubpath covers issue #36: an S3 client pointed at a
// reverse-proxy subpath signs the URI WITH the prefix, but the proxy strips it
// before VaultS3 sees the request. SigV4 verification must add the prefix back
// (from base_path or X-Forwarded-Prefix) or every request is a 403 signature
// mismatch.
func TestAuthReverseProxySubpath(t *testing.T) {
	dir := t.TempDir()
	store, err := metadata.NewStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	// A request the client signed against https://host/sistemas/s3-nac, then the
	// proxy stripped down to /mybucket/key.txt before it reached us.
	newReq := func() *http.Request {
		r := httptest.NewRequest("GET", "http://example.com/sistemas/s3-nac/mybucket/key.txt", nil)
		r.Host = "example.com"
		signV4Request(r, testAccessKey, testSecretKey, []byte(""))
		r.URL.Path = "/mybucket/key.txt" // proxy strips the subpath
		return r
	}

	// Control: without the base path, the stripped path fails (signature was over
	// the full, prefixed path).
	plain := NewAuthenticator(testAccessKey, testSecretKey, store, nil, nil)
	if _, err := plain.Authenticate(newReq()); err == nil {
		t.Fatal("expected signature mismatch without base path (control)")
	}

	// Configured base_path restores the prefix → authenticates.
	cfgd := NewAuthenticator(testAccessKey, testSecretKey, store, nil, nil)
	cfgd.SetBasePath("/sistemas/s3-nac")
	if _, err := cfgd.Authenticate(newReq()); err != nil {
		t.Fatalf("configured base_path: auth failed: %v", err)
	}

	// The proxy's X-Forwarded-Prefix header works with no config too.
	hdr := NewAuthenticator(testAccessKey, testSecretKey, store, nil, nil)
	r := newReq()
	r.Header.Set("X-Forwarded-Prefix", "/sistemas/s3-nac")
	if _, err := hdr.Authenticate(r); err != nil {
		t.Fatalf("X-Forwarded-Prefix: auth failed: %v", err)
	}

	// A normal (non-proxied) request with no base path is unaffected.
	direct := NewAuthenticator(testAccessKey, testSecretKey, store, nil, nil)
	dr := httptest.NewRequest("GET", "http://example.com/mybucket/key.txt", nil)
	dr.Host = "example.com"
	signV4Request(dr, testAccessKey, testSecretKey, []byte(""))
	if _, err := direct.Authenticate(dr); err != nil {
		t.Fatalf("non-proxied auth regressed: %v", err)
	}
}
