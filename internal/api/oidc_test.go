package api

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// authentikIdP stands up a provider laid out the way Authentik, Keycloak and
// Auth0 lay themselves out: every application gets its own issuer path, while the
// authorization endpoint lives at ONE global path shared by all of them. That
// shape is what issue #44 is about — the authorize URL simply cannot be derived
// from the issuer.
type authentikIdP struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	issuer string // as the provider publishes it, with Authentik's trailing slash

	// knobs for the non-standard-provider cases
	omitAuthorizationEndpoint bool
	issuerOverride            string
}

func newAuthentikIdP(t *testing.T, cfg authentikIdP) *authentikIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	idp := &cfg
	idp.key = key

	mux := http.NewServeMux()
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)

	base := idp.srv.URL
	idp.issuer = base + "/application/o/my-app/" // note the trailing slash

	mux.HandleFunc("/application/o/my-app/.well-known/openid-configuration",
		func(w http.ResponseWriter, r *http.Request) {
			doc := map[string]any{
				"issuer":   idp.issuer,
				"jwks_uri": base + "/application/o/my-app/jwks/",
			}
			if idp.issuerOverride != "" {
				doc["issuer"] = idp.issuerOverride
			}
			if !idp.omitAuthorizationEndpoint {
				// The global endpoint — NOT under the application's issuer path.
				doc["authorization_endpoint"] = base + "/application/o/authorize/"
			}
			json.NewEncoder(w).Encode(doc)
		})

	mux.HandleFunc("/application/o/my-app/jwks/", func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
		json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{"kty": "RSA", "kid": "k1", "alg": "RS256", "n": n, "e": e}},
		})
	})

	// The application-scoped path the old code guessed at: it does not exist.
	mux.HandleFunc("/application/o/my-app/authorize", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/application/o/authorize/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	return idp
}

// configuredIssuer is what an operator naturally puts in vaults3.yaml: the issuer
// without Authentik's trailing slash.
func (i *authentikIdP) configuredIssuer() string {
	return strings.TrimRight(i.srv.URL+"/application/o/my-app/", "/")
}

// signIDToken mints an RS256 ID token with the given issuer claim.
func (i *authentikIdP) signIDToken(t *testing.T, issuer, clientID, email string) string {
	t.Helper()
	header := map[string]string{"alg": "RS256", "typ": "JWT", "kid": "k1"}
	claims := map[string]any{
		"iss": issuer, "aud": clientID, "email": email, "sub": "user-1",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signing := enc(header) + "." + enc(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// TestOIDCAuthorizeURLComesFromDiscovery is issue #44: the login URL must be the
// endpoint the provider publishes, not the issuer with "/authorize" glued on,
// which 404s on every provider that uses a global authorization endpoint.
func TestOIDCAuthorizeURLComesFromDiscovery(t *testing.T) {
	idp := newAuthentikIdP(t, authentikIdP{})

	v, err := NewOIDCValidator(idp.configuredIssuer(), "my-client-id", nil, 3600)
	if err != nil {
		t.Fatalf("NewOIDCValidator: %v", err)
	}

	want := idp.srv.URL + "/application/o/authorize/"
	if got := v.AuthorizeURL(); got != want {
		t.Fatalf("AuthorizeURL = %q, want the discovered global endpoint %q", got, want)
	}

	// The URL the old code built really is a 404 on this provider, which is the
	// symptom the reporter saw.
	guessed := idp.configuredIssuer() + "/authorize"
	resp, err := http.Get(guessed)
	if err != nil {
		t.Fatalf("GET %s: %v", guessed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected the guessed URL %s to 404 on an Authentik-style provider, got %d", guessed, resp.StatusCode)
	}
	// ...while the discovered one is live.
	resp2, err := http.Get(v.AuthorizeURL())
	if err != nil {
		t.Fatalf("GET %s: %v", v.AuthorizeURL(), err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("discovered authorize endpoint returned %d, want 200", resp2.StatusCode)
	}
}

// TestOIDCAcceptsIssuerDifferingByTrailingSlash covers the bug waiting behind the
// first one: Authentik publishes its issuer with a trailing slash, an operator
// configures it without, and every ID token was rejected as "invalid issuer".
func TestOIDCAcceptsIssuerDifferingByTrailingSlash(t *testing.T) {
	idp := newAuthentikIdP(t, authentikIdP{})

	v, err := NewOIDCValidator(idp.configuredIssuer(), "my-client-id", nil, 3600)
	if err != nil {
		t.Fatalf("NewOIDCValidator: %v", err)
	}

	// A token as the provider actually mints it: iss carries the trailing slash.
	token := idp.signIDToken(t, idp.issuer, "my-client-id", "user@example.com")
	claims, err := v.ValidateToken(token)
	if err != nil {
		t.Fatalf("ValidateToken: %v (a trailing slash on the issuer must not reject the token)", err)
	}
	if claims.Email != "user@example.com" {
		t.Fatalf("email = %q, want user@example.com", claims.Email)
	}

	// The same token without the slash is equally acceptable.
	if _, err := v.ValidateToken(idp.signIDToken(t, idp.configuredIssuer(), "my-client-id", "user@example.com")); err != nil {
		t.Fatalf("ValidateToken (no trailing slash): %v", err)
	}
}

// TestOIDCRejectsForeignIssuer keeps the check meaningful: tolerating a trailing
// slash must not turn into tolerating a different identity provider.
func TestOIDCRejectsForeignIssuer(t *testing.T) {
	idp := newAuthentikIdP(t, authentikIdP{})
	v, err := NewOIDCValidator(idp.configuredIssuer(), "my-client-id", nil, 3600)
	if err != nil {
		t.Fatalf("NewOIDCValidator: %v", err)
	}

	token := idp.signIDToken(t, "https://evil.example.com/application/o/my-app/", "my-client-id", "user@example.com")
	if _, err := v.ValidateToken(token); err == nil {
		t.Fatal("SECURITY: a token from a different issuer must be rejected")
	}
}

// TestOIDCFallsBackWhenProviderOmitsAuthorizationEndpoint preserves the behaviour
// of existing deployments whose provider publishes no authorization_endpoint.
func TestOIDCFallsBackWhenProviderOmitsAuthorizationEndpoint(t *testing.T) {
	idp := newAuthentikIdP(t, authentikIdP{omitAuthorizationEndpoint: true})

	v, err := NewOIDCValidator(idp.configuredIssuer(), "my-client-id", nil, 3600)
	if err != nil {
		t.Fatalf("NewOIDCValidator: %v", err)
	}
	want := idp.configuredIssuer() + "/authorize"
	if got := v.AuthorizeURL(); got != want {
		t.Fatalf("AuthorizeURL = %q, want the previous behaviour %q when discovery omits the endpoint", got, want)
	}
}

// TestOIDCIgnoresMismatchedDiscoveryIssuer: a discovery document claiming an
// unrelated issuer is out of spec, so tokens keep being validated against what the
// operator configured rather than whatever the document asserts.
func TestOIDCIgnoresMismatchedDiscoveryIssuer(t *testing.T) {
	idp := newAuthentikIdP(t, authentikIdP{issuerOverride: "https://somewhere-else.example.com/"})

	v, err := NewOIDCValidator(idp.configuredIssuer(), "my-client-id", nil, 3600)
	if err != nil {
		t.Fatalf("NewOIDCValidator: %v", err)
	}
	if _, err := v.ValidateToken(idp.signIDToken(t, "https://somewhere-else.example.com/", "my-client-id", "u@e.com")); err == nil {
		t.Fatal("SECURITY: an out-of-spec discovery issuer must not become the accepted issuer")
	}
	if _, err := v.ValidateToken(idp.signIDToken(t, idp.configuredIssuer(), "my-client-id", "u@e.com")); err != nil {
		t.Fatalf("the configured issuer must still be accepted: %v", err)
	}
}

// TestOIDCConfigEndpointPublishesAuthorizeURL checks the value actually reaches the
// dashboard, which is where the broken URL was being built.
func TestOIDCConfigEndpointPublishesAuthorizeURL(t *testing.T) {
	idp := newAuthentikIdP(t, authentikIdP{})
	h, _ := newTestAPI(t)
	v, err := NewOIDCValidator(idp.configuredIssuer(), "my-client-id", nil, 3600)
	if err != nil {
		t.Fatalf("NewOIDCValidator: %v", err)
	}
	h.SetOIDCValidator(v)
	h.cfg.OIDC.IssuerURL = idp.configuredIssuer()
	h.cfg.OIDC.ClientID = "my-client-id"

	rr := httptest.NewRecorder()
	h.handleOIDCConfig(rr, httptest.NewRequest(http.MethodGet, "/auth/oidc/config", nil))

	var out struct {
		Enabled      bool   `json:"enabled"`
		IssuerURL    string `json:"issuerUrl"`
		AuthorizeURL string `json:"authorizeUrl"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out.Enabled {
		t.Fatal("expected enabled=true")
	}
	want := idp.srv.URL + "/application/o/authorize/"
	if out.AuthorizeURL != want {
		t.Fatalf("authorizeUrl = %q, want %q", out.AuthorizeURL, want)
	}
	if strings.HasPrefix(out.AuthorizeURL, out.IssuerURL+"/authorize") {
		t.Fatal("the authorize URL must not be the issuer with /authorize appended")
	}
}
