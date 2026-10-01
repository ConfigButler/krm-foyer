package auth

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
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	clientID     = "krm-foyer"
	clientSecret = "client-secret-91c3" //nolint:gosec // a marker to search for, not a credential
)

// fakeIssuer is an OIDC issuer that behaves like a strict one (PKCE enforced, codes
// single use, redirect URI checked) unless a test tells it to misbehave. It shares
// no code with what it tests: it signs its own JWTs.
type fakeIssuer struct {
	*httptest.Server
	t   *testing.T
	key *rsa.PrivateKey
	now func() time.Time

	mu    sync.Mutex
	codes map[string]grant
	// exchanges counts token requests that redeemed a code.
	exchanges int
	// idTokens is every ID token issued, for the leak scan.
	idTokens []string
	// verifiers is every PKCE verifier received.
	verifiers []string

	// Knobs for misbehaving.
	tamper      func(claims map[string]any)
	signWith    *rsa.PrivateKey
	omitIDToken bool
	refuseCodes bool
	// failDiscovery answers this many discovery requests with 503.
	failDiscovery int
}

type grant struct {
	challenge, nonce, redirectURI, email string
}

func newFakeIssuer(t *testing.T, now func() time.Time) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{t: t, key: key, now: now, codes: map[string]grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("GET /keys", f.keys)
	mux.HandleFunc("POST /token", f.token)
	f.Server = httptest.NewTLSServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeIssuer) discovery(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	failing := f.failDiscovery > 0
	if failing {
		f.failDiscovery--
	}
	f.mu.Unlock()
	if failing {
		http.Error(w, "starting", http.StatusServiceUnavailable)
		return
	}
	writeTestJSON(w, map[string]any{
		"issuer":                                f.URL,
		"authorization_endpoint":                f.URL + "/authorize",
		"token_endpoint":                        f.URL + "/token",
		"jwks_uri":                              f.URL + "/keys",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (f *fakeIssuer) keys(w http.ResponseWriter, _ *http.Request) {
	writeTestJSON(w, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "k1",
		"n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(f.key.E)).Bytes()),
	}}})
}

// authorize plays the issuer's login page for user: it checks the authorization
// request the browser was sent with, and returns the callback URL it would send the
// browser back to.
func (f *fakeIssuer) authorize(location, user string) string {
	f.t.Helper()
	u, err := url.Parse(location)
	if err != nil || !strings.HasPrefix(location, f.URL+"/authorize?") {
		f.t.Fatalf("login redirected to %q, not the issuer's authorization endpoint", location)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"response_type": "code", "client_id": clientID, "code_challenge_method": "S256",
	} {
		if got := q.Get(k); got != want {
			f.t.Fatalf("authorization request %s = %q, want %q", k, got, want)
		}
	}
	for _, k := range []string{"state", "nonce", "code_challenge", "redirect_uri", "scope"} {
		if len(q[k]) != 1 || q.Get(k) == "" {
			f.t.Fatalf("authorization request has %s = %q", k, q[k])
		}
	}
	code := random()
	f.mu.Lock()
	f.codes[code] = grant{challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), redirectURI: q.Get("redirect_uri"), email: user}
	f.mu.Unlock()
	return q.Get("redirect_uri") + "?" + url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
}

func (f *fakeIssuer) token(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostFormValue("client_id"), r.PostFormValue("client_secret")
	}
	if id != clientID || secret != clientSecret {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	code, verifier := r.PostFormValue("code"), r.PostFormValue("code_verifier")
	f.mu.Lock()
	g, found := f.codes[code]
	delete(f.codes, code) // single use, as RFC 6749 requires
	f.verifiers = append(f.verifiers, verifier)
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(verifier))
	if !found || f.refuseCodes || r.PostFormValue("grant_type") != "authorization_code" ||
		r.PostFormValue("redirect_uri") != g.redirectURI ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	now := f.now()
	claims := map[string]any{
		"iss": f.URL, "aud": clientID, "sub": "sub-" + g.email, "email": g.email, "email_verified": true,
		"nonce": g.nonce, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	if f.tamper != nil {
		f.tamper(claims)
	}
	key := f.key
	if f.signWith != nil {
		key = f.signWith
	}
	idToken := signJWT(f.t, key, claims)
	f.mu.Lock()
	f.exchanges++
	f.idTokens = append(f.idTokens, idToken)
	f.mu.Unlock()
	body := map[string]any{"access_token": "access-" + random(), "token_type": "Bearer", "expires_in": 3600}
	if !f.omitIDToken {
		body["id_token"] = idToken
	}
	writeTestJSON(w, body)
}

func (f *fakeIssuer) exchanged() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchanges
}

// signJWT signs claims with RS256 under key ID k1.
func signJWT(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signed := enc(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
