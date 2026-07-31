package api

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The authorization-code flow with PKCE, which is what current identity providers
// expect and what OAuth 2.1 requires. The implicit flow VaultS3 used before
// (response_type=id_token) returns the token through the browser's URL fragment
// and is disabled by default on Authentik and Keycloak, so SSO could not be
// completed there at all.
//
// How the pieces fit:
//
//  1. The dashboard asks /auth/oidc/start for a login URL. This node generates the
//     PKCE verifier, the nonce and the CSRF state, and hands back only the URL.
//  2. The provider authenticates the user and redirects back with a code.
//  3. The dashboard posts that code to /auth/oidc/callback, which redeems it for an
//     ID token over a back channel (server to server) and validates it.
//
// The verifier, the nonce and the client secret never reach the browser, and the
// ID token never travels through a URL. What does travel through the browser is
// the state, sealed with AES-GCM so it cannot be read or forged, and self
// contained so any node in a cluster can complete a login another node started.

// loginStateTTL bounds how long a started login may take to come back. Long enough
// for a user to type a password and satisfy MFA, short enough to bound replay.
const loginStateTTL = 15 * time.Minute

// loginState is what has to survive the round trip through the provider.
type loginState struct {
	Verifier    string `json:"v"`
	Nonce       string `json:"n"`
	RedirectURI string `json:"r"`
	ExpiresAt   int64  `json:"e"`
}

// SupportsCodeFlow reports whether the provider advertises the authorization-code
// flow, which decides the flow the dashboard uses when none is pinned in config.
func (v *OIDCValidator) SupportsCodeFlow() bool { return v.supportsCode }

// SetClientSecret supplies the credential for a confidential client. Public
// clients (no secret) authenticate the exchange with PKCE alone.
func (v *OIDCValidator) SetClientSecret(secret string) { v.clientSecret = secret }

// SetScopes pins the scopes to request, overriding what would be negotiated from
// the provider's discovery document.
func (v *OIDCValidator) SetScopes(scopes []string) { v.configuredScopes = scopes }

// wantedScopes are the scopes VaultS3 would like: identity, a display name, and
// group membership for role mapping.
var wantedScopes = []string{"openid", "email", "profile", "groups"}

// Scopes returns the scope string to send with an authorization request.
//
// A scope the provider does not recognise is fatal, not ignorable: a stock
// Keycloak answers a request including "groups" with
// "error=invalid_scope ... Invalid scopes: openid email profile groups" and never
// shows a login page, because Keycloak has no "groups" client scope by default
// while Authentik does. So ask only for what the provider advertises, and keep
// the full list for providers that publish no scopes_supported at all.
func (v *OIDCValidator) Scopes() string {
	if len(v.configuredScopes) > 0 {
		return strings.Join(v.configuredScopes, " ")
	}
	if len(v.scopesSupported) == 0 {
		return strings.Join(wantedScopes, " ")
	}
	out := []string{"openid"} // required by OpenID Connect, always sent
	for _, s := range wantedScopes[1:] {
		if containsFold(v.scopesSupported, s) {
			out = append(out, s)
		}
	}
	return strings.Join(out, " ")
}

// SetStateKey sets the key that seals login state. Deriving it from a value shared
// by every node (the admin secret) means a login started on one node can be
// completed on another, which is what happens behind a load balancer.
func (v *OIDCValidator) SetStateKey(seed string) {
	sum := sha256.Sum256([]byte("vaults3-oidc-state\x00" + seed))
	v.stateKey = sum[:]
}

// StartLogin builds the URL that begins an authorization-code login, together with
// the sealed state the callback will need.
func (v *OIDCValidator) StartLogin(redirectURI string) (string, error) {
	if !v.supportsCode {
		return "", fmt.Errorf("provider does not advertise the authorization code flow")
	}
	verifier, err := randomURLSafe(64)
	if err != nil {
		return "", err
	}
	nonce, err := randomURLSafe(24)
	if err != nil {
		return "", err
	}
	state, err := v.sealState(loginState{
		Verifier:    verifier,
		Nonce:       nonce,
		RedirectURI: redirectURI,
		ExpiresAt:   time.Now().Add(loginStateTTL).Unix(),
	})
	if err != nil {
		return "", err
	}

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", v.clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", v.Scopes())
	q.Set("state", state)
	q.Set("nonce", nonce)
	// PKCE binds the code to this browser session, so a stolen code is useless
	// without the verifier. Sent whenever the provider supports it, which is the
	// only way a public client can be safe.
	if v.supportsPKCE {
		challenge := sha256.Sum256([]byte(verifier))
		q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
		q.Set("code_challenge_method", "S256")
	}

	base := v.AuthorizeURL()
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + q.Encode(), nil
}

// CompleteLogin redeems an authorization code and returns the validated claims of
// the ID token it yields.
func (v *OIDCValidator) CompleteLogin(code, state string) (*OIDCClaims, error) {
	st, err := v.openState(state)
	if err != nil {
		return nil, err
	}
	if time.Now().Unix() > st.ExpiresAt {
		return nil, fmt.Errorf("login expired, please try again")
	}

	idToken, err := v.exchangeCode(code, st)
	if err != nil {
		return nil, err
	}

	claims, err := v.ValidateToken(idToken)
	if err != nil {
		return nil, err
	}
	// The nonce ties this ID token to the login WE started, so one obtained
	// elsewhere cannot be injected here. A provider that omits it cannot offer
	// that protection, but the exchange is still bound by PKCE and the client
	// credential.
	if claims.Nonce != "" && claims.Nonce != st.Nonce {
		return nil, fmt.Errorf("nonce mismatch")
	}
	return claims, nil
}

// exchangeCode performs the back-channel token request.
func (v *OIDCValidator) exchangeCode(code string, st *loginState) (string, error) {
	if v.tokenURL == "" {
		return "", fmt.Errorf("provider published no token endpoint")
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", st.RedirectURI)
	form.Set("client_id", v.clientID)
	if v.supportsPKCE {
		form.Set("code_verifier", st.Verifier)
	}

	req, err := http.NewRequest(http.MethodPost, v.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if v.clientSecret != "" {
		// HTTP Basic is the method every provider must support, per OAuth 2.0.
		req.SetBasicAuth(url.QueryEscape(v.clientID), url.QueryEscape(v.clientSecret))
	}

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		// Surface the provider's own error code, which is what tells an operator
		// whether the client secret, the redirect URI or the flow is wrong.
		var oauthErr struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &oauthErr)
		if oauthErr.Error != "" {
			return "", fmt.Errorf("token exchange rejected (%s): %s", oauthErr.Error, oauthErr.Description)
		}
		return "", fmt.Errorf("token exchange returned %d", resp.StatusCode)
	}

	var out struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}
	if out.IDToken == "" {
		return "", fmt.Errorf("token response contained no id_token (is the openid scope granted?)")
	}
	return out.IDToken, nil
}

// sealState encrypts the login state so it can be parked in the browser without
// being readable or forgeable.
func (v *OIDCValidator) sealState(st loginState) (string, error) {
	gcm, err := v.stateCipher()
	if err != nil {
		return "", err
	}
	plain, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(gcm.Seal(nonce, nonce, plain, nil)), nil
}

func (v *OIDCValidator) openState(state string) (*loginState, error) {
	gcm, err := v.stateCipher()
	if err != nil {
		return nil, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(state)
	if err != nil || len(raw) < gcm.NonceSize() {
		return nil, fmt.Errorf("invalid login state")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return nil, fmt.Errorf("invalid login state")
	}
	var st loginState
	if err := json.Unmarshal(plain, &st); err != nil {
		return nil, fmt.Errorf("invalid login state")
	}
	return &st, nil
}

func (v *OIDCValidator) stateCipher() (cipher.AEAD, error) {
	if len(v.stateKey) == 0 {
		return nil, fmt.Errorf("oidc login state key not configured")
	}
	block, err := aes.NewCipher(v.stateKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// containsFold reports whether list holds want, case-insensitively. Providers are
// inconsistent about the casing of things like "S256".
func containsFold(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(strings.TrimSpace(s), want) {
			return true
		}
	}
	return false
}
