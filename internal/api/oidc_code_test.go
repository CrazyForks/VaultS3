package api

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// codeFlowIdP is a provider that implements the authorization-code flow the way a
// real one does: it advertises the flow, enforces PKCE, echoes the nonce into the
// ID token, requires the client credential, and burns each code after one use.
type codeFlowIdP struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	issuer string

	clientID     string
	clientSecret string

	mu       sync.Mutex
	pending  map[string]pendingAuth // code → what the authorize request asked for
	redeemed map[string]bool

	// knobs
	noCodeSupport bool
	noPKCE        bool
}

type pendingAuth struct {
	challenge   string
	nonce       string
	redirectURI string
}

func newCodeFlowIdP(t *testing.T, cfg codeFlowIdP) *codeFlowIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	idp := &cfg
	idp.key = key
	idp.pending = map[string]pendingAuth{}
	idp.redeemed = map[string]bool{}
	if idp.clientID == "" {
		idp.clientID = "vaults3"
	}

	mux := http.NewServeMux()
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	idp.issuer = idp.srv.URL + "/application/o/my-app/"

	mux.HandleFunc("/application/o/my-app/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]any{
			"issuer":                 idp.issuer,
			"jwks_uri":               idp.srv.URL + "/application/o/my-app/jwks/",
			"authorization_endpoint": idp.srv.URL + "/application/o/authorize/",
			"token_endpoint":         idp.srv.URL + "/application/o/token/",
			"response_types_supported": func() []string {
				if idp.noCodeSupport {
					return []string{"id_token"}
				}
				return []string{"code", "id_token"}
			}(),
		}
		if !idp.noPKCE {
			doc["code_challenge_methods_supported"] = []string{"S256"}
		}
		if idp.noCodeSupport {
			delete(doc, "token_endpoint")
		}
		json.NewEncoder(w).Encode(doc)
	})

	mux.HandleFunc("/application/o/my-app/jwks/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
		}}})
	})

	// The user logs in here; we shortcut straight to issuing a code.
	mux.HandleFunc("/application/o/authorize/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("response_type") != "code" {
			http.Error(w, "unsupported_response_type", http.StatusBadRequest)
			return
		}
		if q.Get("client_id") != idp.clientID {
			http.Error(w, "unauthorized_client", http.StatusBadRequest)
			return
		}
		code := "code-" + q.Get("state")[:8]
		idp.mu.Lock()
		idp.pending[code] = pendingAuth{
			challenge:   q.Get("code_challenge"),
			nonce:       q.Get("nonce"),
			redirectURI: q.Get("redirect_uri"),
		}
		idp.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+url.QueryEscape(code)+
			"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})

	mux.HandleFunc("/application/o/token/", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		fail := func(code, desc string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
		}
		if r.Form.Get("grant_type") != "authorization_code" {
			fail("unsupported_grant_type", "")
			return
		}
		// Confidential clients must authenticate.
		if idp.clientSecret != "" {
			user, pass, ok := r.BasicAuth()
			if !ok || url.QueryEscape(idp.clientID) != user || url.QueryEscape(idp.clientSecret) != pass {
				fail("invalid_client", "bad client credentials")
				return
			}
		}
		code := r.Form.Get("code")
		idp.mu.Lock()
		auth, known := idp.pending[code]
		alreadyUsed := idp.redeemed[code]
		idp.redeemed[code] = true
		idp.mu.Unlock()
		if !known || alreadyUsed {
			fail("invalid_grant", "code is unknown or already redeemed")
			return
		}
		if auth.redirectURI != r.Form.Get("redirect_uri") {
			fail("invalid_grant", "redirect_uri mismatch")
			return
		}
		// PKCE: the verifier must hash to the challenge sent at authorize time.
		if auth.challenge != "" {
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != auth.challenge {
				fail("invalid_grant", "PKCE verification failed")
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"access_token": "at", "token_type": "Bearer",
			"id_token": idp.mintIDToken(auth.nonce, "michael@example.com"),
		})
	})

	return idp
}

func (i *codeFlowIdP) mintIDToken(nonce, email string) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	claims := map[string]any{
		"iss": i.issuer, "aud": i.clientID, "email": email, "sub": "u1",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	signing := enc(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "k1"}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, sum[:])
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (i *codeFlowIdP) configuredIssuer() string {
	return strings.TrimRight(i.issuer, "/")
}

// newCodeValidator wires a validator the way the server does at startup.
func newCodeValidator(t *testing.T, idp *codeFlowIdP) *OIDCValidator {
	t.Helper()
	v, err := NewOIDCValidator(idp.configuredIssuer(), idp.clientID, nil, 3600)
	if err != nil {
		t.Fatalf("NewOIDCValidator: %v", err)
	}
	v.SetClientSecret(idp.clientSecret)
	v.SetStateKey("test-admin-secret")
	return v
}

// followAuthorize plays the browser's part: open the authorize URL and read the
// code and state out of the redirect the provider answers with.
func followAuthorize(t *testing.T, authorizeURL string) (code, state string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(authorizeURL)
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize returned %d, want a redirect back to the callback", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	return loc.Query().Get("code"), loc.Query().Get("state")
}

// TestCodeFlowEndToEnd walks the whole authorization-code login against a provider
// that enforces PKCE and a client secret, which is how Authentik and Keycloak are
// configured out of the box.
func TestCodeFlowEndToEnd(t *testing.T) {
	idp := newCodeFlowIdP(t, codeFlowIdP{clientSecret: "s3cret"})
	v := newCodeValidator(t, idp)

	if !v.SupportsCodeFlow() {
		t.Fatal("provider advertises the code flow, so it must be detected")
	}

	authURL, err := v.StartLogin("https://s3.example.com/dashboard/oidc-callback")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	u, _ := url.Parse(authURL)
	if got := u.Query().Get("response_type"); got != "code" {
		t.Fatalf("response_type = %q, want code", got)
	}
	if u.Query().Get("code_challenge") == "" || u.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("PKCE challenge must be sent when the provider supports S256")
	}
	if u.Query().Get("state") == "" || u.Query().Get("nonce") == "" {
		t.Fatal("state and nonce are required")
	}
	// The secret and the verifier must never appear in a browser-visible URL.
	if strings.Contains(authURL, "s3cret") || strings.Contains(authURL, "code_verifier") {
		t.Fatal("SECURITY: the client secret and PKCE verifier must not leave the server")
	}

	code, state := followAuthorize(t, authURL)
	claims, err := v.CompleteLogin(code, state)
	if err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}
	if claims.Email != "michael@example.com" {
		t.Fatalf("email = %q", claims.Email)
	}
}

// TestCodeFlowRejectsTamperedState is the CSRF guard: the state is sealed, so a
// state the server did not mint cannot be used to complete a login.
func TestCodeFlowRejectsTamperedState(t *testing.T) {
	idp := newCodeFlowIdP(t, codeFlowIdP{})
	v := newCodeValidator(t, idp)

	authURL, err := v.StartLogin("https://s3.example.com/dashboard/oidc-callback")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	code, state := followAuthorize(t, authURL)

	for _, bad := range []string{
		"not-a-state",
		state[:len(state)-4] + "AAAA", // flipped ciphertext
		base64.RawURLEncoding.EncodeToString([]byte(`{"v":"x","n":"y","r":"z","e":9999999999}`)), // forged plaintext
	} {
		if _, err := v.CompleteLogin(code, bad); err == nil {
			t.Fatalf("SECURITY: state %q was accepted", bad)
		}
	}
}

// TestCodeFlowRejectsStateFromAnotherServer proves the seal is keyed: a state
// minted by a different deployment cannot complete a login here.
func TestCodeFlowRejectsStateFromAnotherServer(t *testing.T) {
	idp := newCodeFlowIdP(t, codeFlowIdP{})
	v := newCodeValidator(t, idp)

	other, err := NewOIDCValidator(idp.configuredIssuer(), idp.clientID, nil, 3600)
	if err != nil {
		t.Fatalf("NewOIDCValidator: %v", err)
	}
	other.SetStateKey("a-different-admin-secret")
	foreignURL, err := other.StartLogin("https://s3.example.com/dashboard/oidc-callback")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	code, foreignState := followAuthorize(t, foreignURL)

	if _, err := v.CompleteLogin(code, foreignState); err == nil {
		t.Fatal("SECURITY: a state sealed with another key must be rejected")
	}
}

// TestCodeFlowStateSurvivesAcrossNodes is the other half of that: two nodes
// sharing the admin secret must be able to finish each other's logins, which is
// what happens behind a load balancer.
func TestCodeFlowStateSurvivesAcrossNodes(t *testing.T) {
	idp := newCodeFlowIdP(t, codeFlowIdP{})
	nodeA := newCodeValidator(t, idp)
	nodeB := newCodeValidator(t, idp) // same seed

	authURL, err := nodeA.StartLogin("https://s3.example.com/dashboard/oidc-callback")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	code, state := followAuthorize(t, authURL)

	if _, err := nodeB.CompleteLogin(code, state); err != nil {
		t.Fatalf("a login started on one node must complete on another: %v", err)
	}
}

// TestCodeFlowRejectsReusedCode: the provider burns the code, and we surface that
// rather than issuing a second session from it.
func TestCodeFlowRejectsReusedCode(t *testing.T) {
	idp := newCodeFlowIdP(t, codeFlowIdP{})
	v := newCodeValidator(t, idp)

	authURL, _ := v.StartLogin("https://s3.example.com/dashboard/oidc-callback")
	code, state := followAuthorize(t, authURL)

	if _, err := v.CompleteLogin(code, state); err != nil {
		t.Fatalf("first exchange: %v", err)
	}
	if _, err := v.CompleteLogin(code, state); err == nil {
		t.Fatal("SECURITY: a replayed authorization code must be rejected")
	}
}

// TestCodeFlowRejectsWrongClientSecret checks we actually send the credential and
// surface the provider's complaint, which is the first thing an operator hits.
func TestCodeFlowRejectsWrongClientSecret(t *testing.T) {
	idp := newCodeFlowIdP(t, codeFlowIdP{clientSecret: "right-secret"})
	v := newCodeValidator(t, idp)
	v.SetClientSecret("wrong-secret")

	authURL, _ := v.StartLogin("https://s3.example.com/dashboard/oidc-callback")
	code, state := followAuthorize(t, authURL)

	_, err := v.CompleteLogin(code, state)
	if err == nil {
		t.Fatal("a wrong client secret must fail the exchange")
	}
	if !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("error should name the provider's reason, got: %v", err)
	}
}

// TestCodeFlowRejectsNonceMismatch stops an ID token minted for a different login
// from being accepted for this one.
func TestCodeFlowRejectsNonceMismatch(t *testing.T) {
	idp := newCodeFlowIdP(t, codeFlowIdP{})
	v := newCodeValidator(t, idp)

	authURL, _ := v.StartLogin("https://s3.example.com/dashboard/oidc-callback")
	code, state := followAuthorize(t, authURL)

	// The provider answers with a token carrying somebody else's nonce.
	idp.mu.Lock()
	auth := idp.pending[code]
	auth.nonce = "a-different-login"
	idp.pending[code] = auth
	idp.mu.Unlock()

	if _, err := v.CompleteLogin(code, state); err == nil {
		t.Fatal("SECURITY: an ID token whose nonce does not match this login must be rejected")
	}
}

// TestCodeFlowRejectsExpiredState bounds how long a started login stays usable.
func TestCodeFlowRejectsExpiredState(t *testing.T) {
	idp := newCodeFlowIdP(t, codeFlowIdP{})
	v := newCodeValidator(t, idp)

	expired, err := v.sealState(loginState{
		Verifier: "v", Nonce: "n",
		RedirectURI: "https://s3.example.com/dashboard/oidc-callback",
		ExpiresAt:   time.Now().Add(-time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("sealState: %v", err)
	}
	if _, err := v.CompleteLogin("some-code", expired); err == nil {
		t.Fatal("an expired login state must be rejected")
	}
}

// TestImplicitOnlyProviderStillWorks: a provider that advertises no code flow must
// keep using the implicit path rather than being broken by the new one.
func TestImplicitOnlyProviderStillWorks(t *testing.T) {
	idp := newCodeFlowIdP(t, codeFlowIdP{noCodeSupport: true})
	v := newCodeValidator(t, idp)

	if v.SupportsCodeFlow() {
		t.Fatal("a provider without a token endpoint does not support the code flow")
	}
	if _, err := v.StartLogin("https://s3.example.com/dashboard/oidc-callback"); err == nil {
		t.Fatal("StartLogin must refuse when the provider cannot do the code flow")
	}
	// The implicit path is unaffected.
	if _, err := v.ValidateToken(idp.mintIDToken("", "michael@example.com")); err != nil {
		t.Fatalf("implicit validation must still work: %v", err)
	}
}

// TestCodeFlowWithoutPKCESupport covers a provider that does not offer S256: the
// login still works, authenticated by the client secret alone.
func TestCodeFlowWithoutPKCESupport(t *testing.T) {
	idp := newCodeFlowIdP(t, codeFlowIdP{noPKCE: true, clientSecret: "s3cret"})
	v := newCodeValidator(t, idp)

	authURL, err := v.StartLogin("https://s3.example.com/dashboard/oidc-callback")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if strings.Contains(authURL, "code_challenge") {
		t.Fatal("PKCE must not be sent to a provider that does not advertise it")
	}
	code, state := followAuthorize(t, authURL)
	if _, err := v.CompleteLogin(code, state); err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}
}
