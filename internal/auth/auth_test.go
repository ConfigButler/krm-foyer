package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ConfigButler/krm-foyer/internal/session"
)

const alice = "alice@example.com"

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// harness is krm-foyer's login routes on a TLS server, an issuer, and a recorder
// of every response a browser received.
type harness struct {
	t        *testing.T
	clock    *clock
	issuer   *fakeIssuer
	auth     *Auth
	sessions *session.Manager
	store    *flakyStore
	foyer    *httptest.Server
	// logs is everything krm-foyer logged, for the leak scan.
	logs syncBuffer
	mu   sync.Mutex
	seen []seenResponse
}

// syncBuffer is a buffer handlers on several goroutines can write to.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type seenResponse struct {
	target string
	header http.Header
	body   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := newUndiscovered(t)
	if err := h.auth.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	return h
}

func newUndiscovered(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, clock: &clock{t: time.Now().Truncate(time.Second)}}
	h.issuer = newFakeIssuer(t, h.clock.Now)
	var handler http.Handler
	h.foyer = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	h.foyer.StartTLS()
	t.Cleanup(h.foyer.Close)

	h.store = &flakyStore{Memory: session.NewMemory(h.clock.Now)}
	var err error
	h.sessions, err = session.New(session.Config{
		Store: h.store, Origin: h.foyer.URL, IdleTimeout: time.Hour, AbsoluteTimeout: 8 * time.Hour, Now: h.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	issuerCA := x509.NewCertPool()
	issuerCA.AddCert(h.issuer.Certificate())
	h.auth, err = New(Config{
		PublicURL: h.foyer.URL, Issuer: h.issuer.URL, ClientID: clientID, ClientSecret: clientSecret,
		RootCAs: issuerCA, Sessions: h.sessions, Now: h.clock.Now,
		Logger: slog.New(slog.NewJSONHandler(&h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	handler = h.auth.Handler()
	t.Cleanup(h.scan)
	return h
}

// browser is a cookie jar over TLS that does not follow redirects, so each test
// sees every answer.
type browser struct {
	h      *harness
	jar    *cookiejar.Jar
	client *http.Client
}

func (h *harness) browser() *browser {
	jar, err := cookiejar.New(nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return &browser{h: h, jar: jar, client: &http.Client{
		Jar:           jar,
		Transport:     recorder{h: h, next: h.foyer.Client().Transport},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// recorder keeps a copy of every response krm-foyer sent, for the leak scan.
type recorder struct {
	h    *harness
	next http.RoundTripper
}

func (rec recorder) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := rec.next.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	rec.h.mu.Lock()
	rec.h.seen = append(rec.h.seen, seenResponse{r.URL.String(), resp.Header.Clone(), string(body)})
	rec.h.mu.Unlock()
	return resp, nil
}

type response struct {
	code   int
	header http.Header
	body   string
}

func (b *browser) do(method, target string, header http.Header) response {
	b.h.t.Helper()
	if strings.HasPrefix(target, "/") {
		target = b.h.foyer.URL + target
	}
	r, err := http.NewRequestWithContext(context.Background(), method, target, nil)
	if err != nil {
		b.h.t.Fatal(err)
	}
	for k, v := range header {
		r.Header[k] = v
	}
	resp, err := b.client.Do(r)
	if err != nil {
		b.h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return response{resp.StatusCode, resp.Header, string(body)}
}

func (b *browser) get(target string) response { return b.do(http.MethodGet, target, nil) }

// startLogin opens /auth/login and returns where it sent the browser.
func (b *browser) startLogin(returnTo string) string {
	b.h.t.Helper()
	target := "/auth/login"
	if returnTo != "" {
		target += "?" + url.Values{"return_to": {returnTo}}.Encode()
	}
	resp := b.get(target)
	if resp.code != http.StatusFound {
		b.h.t.Fatalf("GET %s = %d %s", target, resp.code, resp.body)
	}
	return resp.header.Get("Location")
}

// login walks the whole flow as user and returns the callback's answer.
func (b *browser) login(user, returnTo string) response {
	b.h.t.Helper()
	return b.get(b.h.issuer.authorize(b.startLogin(returnTo), user))
}

func (b *browser) cookie(name string) string {
	u, _ := url.Parse(b.h.foyer.URL)
	for _, c := range b.jar.Cookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func (b *browser) session() (int, sessionState) {
	b.h.t.Helper()
	resp := b.get("/auth/session")
	var s sessionState
	if err := json.Unmarshal([]byte(resp.body), &s); err != nil {
		b.h.t.Fatalf("/auth/session answered %d %q", resp.code, resp.body)
	}
	return resp.code, s
}

func assertLoginError(t *testing.T, resp response, status int, reason string) {
	t.Helper()
	if resp.code != status || !strings.Contains(resp.body, "<code>"+reason+"</code>") {
		t.Fatalf("answered %d, want %d with reason %s:\n%s", resp.code, status, reason, resp.body)
	}
	if !strings.HasPrefix(resp.header.Get("Content-Type"), "text/html") || resp.header.Get("Cache-Control") != "no-store" ||
		!strings.Contains(resp.header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("error page headers: %v", resp.header)
	}
	for _, c := range resp.header.Values("Set-Cookie") {
		if strings.HasPrefix(c, session.CookieName+"=") && !strings.HasPrefix(c, session.CookieName+"=;") {
			t.Errorf("a refused login set a session cookie: %s", c)
		}
	}
}

// scan runs after every test: no response a browser received and no line krm-foyer
// logged holds an ID token, an access token, the client secret, an authorization
// code or a PKCE verifier anywhere, and a session ID appears only in the Set-Cookie
// that issues it.
func (h *harness) scan() {
	h.issuer.mu.Lock()
	secrets := map[string]string{clientSecret: "the client secret"}
	for _, tok := range h.issuer.idTokens {
		secrets[tok] = "an ID token"
		// The signature alone identifies a token, and survives truncation.
		secrets[tok[strings.LastIndexByte(tok, '.')+1:]] = "an ID token's signature"
	}
	for _, tok := range h.issuer.accessTokens {
		secrets[tok] = "an access token"
	}
	for _, v := range h.issuer.verifiers {
		if v != "" {
			secrets[v] = "a PKCE verifier"
		}
	}
	for _, c := range h.issuer.issuedCodes {
		secrets[c] = "an authorization code"
	}
	h.issuer.mu.Unlock()

	h.mu.Lock()
	defer h.mu.Unlock()
	var sessionIDs []string
	for _, r := range h.seen {
		for _, c := range r.header.Values("Set-Cookie") {
			if v, ok := strings.CutPrefix(c, session.CookieName+"="); ok {
				if id, _, _ := strings.Cut(v, ";"); id != "" {
					sessionIDs = append(sessionIDs, id)
				}
			}
		}
	}
	for _, r := range h.seen {
		for secret, what := range secrets {
			if strings.Contains(r.body, secret) || headerContains(r.header, secret, "") {
				h.t.Errorf("%s reached the browser in the answer to %s", what, r.target)
			}
		}
		for _, id := range sessionIDs {
			if strings.Contains(r.body, id) || headerContains(r.header, id, session.CookieName+"="+id+";") {
				h.t.Errorf("a session ID reached the browser outside its Set-Cookie, in the answer to %s", r.target)
			}
		}
	}
	logs := h.logs.String()
	for secret, what := range secrets {
		if strings.Contains(logs, secret) {
			h.t.Errorf("%s was logged:\n%s", what, logs)
		}
	}
	for _, id := range sessionIDs {
		if strings.Contains(logs, id) {
			h.t.Errorf("a session ID was logged:\n%s", logs)
		}
	}
}

// headerContains reports whether s appears in any header field, apart from a
// Set-Cookie field starting with allowed.
func headerContains(h http.Header, s, allowed string) bool {
	for k, vs := range h {
		for _, v := range vs {
			if allowed != "" && k == "Set-Cookie" && strings.HasPrefix(v, allowed) {
				continue
			}
			if strings.Contains(k, s) || strings.Contains(v, s) {
				return true
			}
		}
	}
	return false
}

// A login: the browser goes to the issuer with PKCE, state and nonce, comes back,
// gets an opaque session cookie and lands on the path it asked for.
func TestLogin(t *testing.T) {
	h := newHarness(t)
	b := h.browser()

	location := b.startLogin("/apps/coffee?view=list")
	q, _ := url.Parse(location)
	if got := q.Query().Get("redirect_uri"); got != h.foyer.URL+"/auth/callback" {
		t.Errorf("redirect_uri = %q", got)
	}
	if got := q.Query().Get("scope"); got != "openid email profile" {
		t.Errorf("scope = %q; no offline_access while there is no refresh", got)
	}
	if q.Query().Get("code_challenge") == "" || q.Query().Get("state") == q.Query().Get("nonce") {
		t.Errorf("authorization request %v", q.Query())
	}

	resp := b.get(h.issuer.authorize(location, alice))
	if resp.code != http.StatusSeeOther || resp.header.Get("Location") != "/apps/coffee?view=list" {
		t.Fatalf("callback answered %d, Location %q:\n%s", resp.code, resp.header.Get("Location"), resp.body)
	}
	if b.cookie(transactionCookieName) != "" {
		t.Error("the login cookie outlived the callback")
	}
	if b.cookie(session.CookieName) == "" {
		t.Fatal("no session cookie")
	}

	code, s := b.session()
	if code != http.StatusOK || !s.Authenticated || s.Email != alice || s.Issuer != h.issuer.URL ||
		s.Subject != "sub-"+alice || s.CSRFToken == "" || s.CSRFHeader != session.CSRFHeader || s.ExpiresAt == nil {
		t.Fatalf("/auth/session = %d %+v", code, s)
	}
	// The token's expiry ends the session before the absolute timeout does.
	if !s.ExpiresAt.Equal(h.clock.Now().Add(time.Hour)) {
		t.Errorf("expiresAt = %v, want the token's expiry", s.ExpiresAt)
	}

	// The session holds the ID token the issuer issued, for the proxy.
	stored, err := h.sessions.Lookup(cookieRequest(b))
	if err != nil || stored.IDToken != h.issuer.idTokens[0] {
		t.Fatalf("stored session %+v, %v", stored, err)
	}
}

func cookieRequest(b *browser) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, b.h.foyer.URL+"/k8s/api", nil)
	r.AddCookie(&http.Cookie{Name: session.CookieName, Value: b.cookie(session.CookieName)}) //nolint:gosec // a request cookie has no attributes
	return r
}

// The login cookie is host-only, Secure, HttpOnly and Lax, and lasts as long as a
// login may take.
func TestTransactionCookie(t *testing.T) {
	h := newHarness(t)
	resp := h.browser().get("/auth/login")
	cookies := (&http.Response{Header: resp.header}).Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login set %v", cookies)
	}
	c := cookies[0]
	if c.Name != transactionCookieName || !strings.HasPrefix(c.Name, "__Host-") || !c.Secure || !c.HttpOnly ||
		c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" || c.MaxAge != 600 {
		t.Errorf("login cookie %+v", c)
	}
	if resp.header.Get("Cache-Control") != "no-store" {
		t.Error("the redirect to the issuer may be cached")
	}
}

// Login CSRF: an attacker starts a login, and sends the victim's browser to the
// callback with the attacker's code and state. The victim must not end up signed in
// as the attacker, with or without a login of their own in progress.
func TestCallbackIsBoundToTheBrowser(t *testing.T) {
	h := newHarness(t)
	attacker, victim := h.browser(), h.browser()
	callback := h.issuer.authorize(attacker.startLogin("/"), "mallory@example.com")

	assertLoginError(t, victim.get(callback), http.StatusBadRequest, "login-not-in-progress")
	if victim.cookie(session.CookieName) != "" {
		t.Fatal("the victim got a session")
	}

	victim.startLogin("/")
	assertLoginError(t, victim.get(callback), http.StatusBadRequest, "state-mismatch")
	if victim.cookie(session.CookieName) != "" {
		t.Fatal("the victim got a session")
	}
	if n := h.issuer.exchanged(); n != 0 {
		t.Errorf("%d codes were redeemed for a callback that did not belong", n)
	}
}

// A callback is used once: replaying it, with the login cookie put back, finds no
// login in progress, and the code is not offered to the issuer again.
func TestCallbackIsSingleUse(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	location := b.startLogin("/")
	txn := b.cookie(transactionCookieName)
	callback := h.issuer.authorize(location, alice)
	if resp := b.get(callback); resp.code != http.StatusSeeOther {
		t.Fatalf("first callback: %d", resp.code)
	}

	replay := h.browser()
	u, _ := url.Parse(h.foyer.URL)
	replay.jar.SetCookies(u, []*http.Cookie{{Name: transactionCookieName, Value: txn}}) //nolint:gosec // a request cookie has no attributes
	assertLoginError(t, replay.get(callback), http.StatusBadRequest, "login-not-in-progress")
	if n := h.issuer.exchanged(); n != 1 {
		t.Errorf("the code was redeemed %d times", n)
	}
}

// A refused callback uses the transaction up too: guessing states is one guess.
func TestRefusedCallbackEndsTheTransaction(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	callback := h.issuer.authorize(b.startLogin("/"), alice)
	u, _ := url.Parse(callback)
	q := u.Query()
	q.Set("state", "guess")
	u.RawQuery = q.Encode()
	assertLoginError(t, b.get(u.String()), http.StatusBadRequest, "state-mismatch")
	assertLoginError(t, b.get(callback), http.StatusBadRequest, "login-not-in-progress")
}

// Every malformed callback is refused before a code is redeemed.
func TestMalformedCallbacks(t *testing.T) {
	for name, edit := range map[string]func(q url.Values){
		"no state":    func(q url.Values) { q.Del("state") },
		"empty state": func(q url.Values) { q.Set("state", "") },
		"two states":  func(q url.Values) { q.Add("state", q.Get("state")) },
		"no code":     func(q url.Values) { q.Del("code") },
		"empty code":  func(q url.Values) { q.Set("code", "") },
		"two codes":   func(q url.Values) { q.Add("code", "other") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			b := h.browser()
			u, _ := url.Parse(h.issuer.authorize(b.startLogin("/"), alice))
			q := u.Query()
			edit(q)
			u.RawQuery = q.Encode()
			resp := b.get(u.String())
			if resp.code != http.StatusBadRequest || h.issuer.exchanged() != 0 {
				t.Fatalf("answered %d after %d exchanges", resp.code, h.issuer.exchanged())
			}
		})
	}
}

// Two login cookies: krm-foyer cannot tell which login this callback belongs to.
func TestTwoLoginCookies(t *testing.T) {
	h := newHarness(t)
	a, b := h.browser(), h.browser()
	a.startLogin("/")
	callback := h.issuer.authorize(b.startLogin("/"), alice)
	header := http.Header{"Cookie": {transactionCookieName + "=" + a.cookie(transactionCookieName) + "; " +
		transactionCookieName + "=" + b.cookie(transactionCookieName)}}
	assertLoginError(t, h.browser().do(http.MethodGet, callback, header), http.StatusBadRequest, "login-not-in-progress")
	if h.issuer.exchanged() != 0 {
		t.Error("a code was redeemed")
	}
}

// A login left at the issuer for longer than ten minutes is over.
func TestTransactionExpires(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	callback := h.issuer.authorize(b.startLogin("/"), alice)
	h.clock.Advance(transactionLifetime)
	assertLoginError(t, b.get(callback), http.StatusBadRequest, "login-not-in-progress")
}

// The issuer's answer is checked: whatever comes back from the token endpoint, a
// session starts only with an ID token from the configured issuer, for this client,
// unexpired, signed by the issuer's key and carrying this login's nonce.
func TestTokenResponseIsVerified(t *testing.T) {
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		misbehave func(*fakeIssuer)
		status    int
		reason    string
	}{
		"code refused":         {func(f *fakeIssuer) { f.refuseCodes = true }, http.StatusBadGateway, "token-exchange-failed"},
		"no ID token":          {func(f *fakeIssuer) { f.omitIDToken = true }, http.StatusBadGateway, "id-token-invalid"},
		"another nonce":        {tamper(func(c map[string]any) { c["nonce"] = "other" }), http.StatusBadGateway, "nonce-mismatch"},
		"no nonce":             {tamper(func(c map[string]any) { delete(c, "nonce") }), http.StatusBadGateway, "nonce-mismatch"},
		"another audience":     {tamper(func(c map[string]any) { c["aud"] = "other-app" }), http.StatusBadGateway, "id-token-invalid"},
		"audience list":        {tamper(func(c map[string]any) { c["aud"] = []string{"other-app"} }), http.StatusBadGateway, "id-token-invalid"},
		"another issuer":       {tamper(func(c map[string]any) { c["iss"] = "https://evil.example" }), http.StatusBadGateway, "id-token-invalid"},
		"expired":              {tamper(func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }), http.StatusBadGateway, "id-token-invalid"},
		"no expiry":            {tamper(func(c map[string]any) { delete(c, "exp") }), http.StatusBadGateway, "id-token-invalid"},
		"signed by a stranger": {func(f *fakeIssuer) { f.signWith = otherKey }, http.StatusBadGateway, "id-token-invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			tc.misbehave(h.issuer)
			b := h.browser()
			assertLoginError(t, b.login(alice, "/"), tc.status, tc.reason)
			if b.cookie(session.CookieName) != "" {
				t.Fatal("a session cookie was set")
			}
			if code, _ := b.session(); code != http.StatusUnauthorized {
				t.Fatalf("/auth/session = %d", code)
			}
		})
	}
}

// An issuer that refuses the token request by repeating it, in any part of its
// error, has its refusal logged by status and error code only. The request held the
// client secret, the code and the PKCE verifier; the scan after each case looks for
// them in the log.
func TestTokenErrorIsNotLogged(t *testing.T) {
	jsonError := func(status int, body map[string]string) func(http.ResponseWriter, string) {
		return func(w http.ResponseWriter, _ string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
		}
	}
	for name, tc := range map[string]struct {
		echo func(w http.ResponseWriter, request string)
		// logged are fields the log line must still carry, as JSON.
		logged []string
	}{
		"in error_description": {
			func(w http.ResponseWriter, req string) {
				jsonError(http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": req})(w, req)
			},
			[]string{`"issuer_status":400`, `"issuer_error":"invalid_grant"`},
		},
		"in error_uri": {
			func(w http.ResponseWriter, req string) {
				jsonError(http.StatusBadRequest, map[string]string{"error": "invalid_client", "error_uri": "https://issuer.example/e?" + url.QueryEscape(req)})(w, req)
			},
			[]string{`"issuer_status":400`, `"issuer_error":"invalid_client"`},
		},
		"as the error code": {
			func(w http.ResponseWriter, req string) {
				jsonError(http.StatusBadRequest, map[string]string{"error": req})(w, req)
			},
			[]string{`"issuer_status":400`, `"issuer_error":"unknown"`},
		},
		"in a body with no error code": {
			func(w http.ResponseWriter, req string) {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, req) //nolint:gosec // the fake issuer echoing its request is the point
			},
			[]string{`"issuer_status":500`, `"issuer_error":"none"`},
		},
		"in a success that is not one": {
			func(w http.ResponseWriter, req string) {
				jsonError(http.StatusOK, map[string]string{"token_type": req})(w, req)
			},
			[]string{`"cause":"invalid-response"`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.issuer.echo = tc.echo
			assertLoginError(t, h.browser().login(alice, "/"), http.StatusBadGateway, "token-exchange-failed")
			logs := h.logs.String()
			for _, field := range append(tc.logged, `"msg":"token exchange failed"`) {
				if !strings.Contains(logs, field) {
					t.Errorf("the log has no %s:\n%s", field, logs)
				}
			}
		})
	}
}

// An issuer that cannot be reached for the token request is logged as such.
func TestTokenEndpointUnreachable(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	callback := h.issuer.authorize(b.startLogin("/"), alice)
	h.issuer.Close()
	assertLoginError(t, b.get(callback), http.StatusBadGateway, "token-exchange-failed")
	if logs := h.logs.String(); !strings.Contains(logs, `"cause":"unreachable"`) {
		t.Errorf("the log does not say the issuer was unreachable:\n%s", logs)
	}
}

func tamper(f func(map[string]any)) func(*fakeIssuer) {
	return func(i *fakeIssuer) { i.tamper = f }
}

// A login that does not end at a local path is refused before the issuer is
// involved, and an accepted one comes back to exactly that path.
func TestReturnPath(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{
		"https://evil.example/", "//evil.example", "///evil.example", "////evil.example", "//", "/\\evil.example", "\\\\evil.example", "/\t/evil.example",
		"/\n/evil.example", " /x", "/x y", "evil.example", "javascript:alert(1)", "http:/evil.example",
		"/caf\u00e9", "/" + strings.Repeat("a", maxReturnTo),
	} {
		resp := h.browser().get("/auth/login?" + url.Values{"return_to": {p}}.Encode())
		assertLoginError(t, resp, http.StatusBadRequest, "return-path-not-local")
		if resp.header.Get("Set-Cookie") != "" {
			t.Errorf("return_to %q started a login", p)
		}
	}
	// Two return_to parameters: which one would be checked, and which one used?
	assertLoginError(t, h.browser().get("/auth/login?return_to=/a&return_to=//evil.example"),
		http.StatusBadRequest, "return-path-not-local")

	for _, p := range []string{"/", "/a/b?c=d&e=%2F%2Fx", "/%2F%2Fevil.example", "/a#frag", "/a/../b", "/a//b/", "/a/./b"} {
		b := h.browser()
		if loc := b.login(alice, p).header.Get("Location"); loc != p {
			t.Errorf("return_to %q came back as %q", p, loc)
		}
	}
	if loc := h.browser().login(alice, "").header.Get("Location"); loc != "/" {
		t.Errorf("no return_to came back as %q", loc)
	}
}

// An error from the issuer is shown by its code only. The rest of the callback is
// anyone's text, and is not repeated on krm-foyer's origin.
func TestIssuerError(t *testing.T) {
	h := newHarness(t)
	for code, shown := range map[string]string{"access_denied": "access_denied", "<b>made-up</b>": "unknown"} {
		b := h.browser()
		u, _ := url.Parse(b.startLogin("/back"))
		callback := u.Query().Get("redirect_uri") + "?" + url.Values{
			"error": {code}, "error_description": {"<a href=//evil.example>Call 555-0100</a>"},
			"state": {u.Query().Get("state")},
		}.Encode()
		resp := b.get(callback)
		assertLoginError(t, resp, http.StatusBadRequest, "issuer-refused")
		if !strings.Contains(resp.body, "<code>"+shown+"</code>") || strings.Contains(resp.body, "555-0100") ||
			strings.Contains(resp.body, "made-up") {
			t.Errorf("error page for %q:\n%s", code, resp.body)
		}
		if !strings.Contains(resp.body, `href="/auth/login?return_to=%2Fback"`) {
			t.Errorf("no way to try again:\n%s", resp.body)
		}
	}
}

// Logging in again rotates the session: the previous ID is worthless.
func TestLoginAgainRotates(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice, "/")
	first := b.cookie(session.CookieName)
	b.login(alice, "/")
	if second := b.cookie(session.CookieName); second == first || second == "" {
		t.Fatal("the session ID did not change")
	}
	old := h.browser()
	u, _ := url.Parse(h.foyer.URL)
	old.jar.SetCookies(u, []*http.Cookie{{Name: session.CookieName, Value: first}}) //nolint:gosec // a request cookie has no attributes
	if code, _ := old.session(); code != http.StatusUnauthorized {
		t.Fatalf("the previous session still answers %d", code)
	}
}

// /auth/session without a session: 401, with nothing but that, and never cached.
func TestSessionWithoutOne(t *testing.T) {
	h := newHarness(t)
	resp := h.browser().get("/auth/session")
	if resp.code != http.StatusUnauthorized || strings.TrimSpace(resp.body) != `{"authenticated":false}` ||
		resp.header.Get("Cache-Control") != "no-store" || resp.header.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %v %s", resp.code, resp.header, resp.body)
	}
}

// Logout needs the same proof as any mutation, so another site cannot sign the user
// out; with it, the session is gone before the answer, and the cookie is cleared.
func TestLogout(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice, "/")
	_, s := b.session()
	id := b.cookie(session.CookieName)
	proof := http.Header{"Origin": {h.foyer.URL}, session.CSRFHeader: {s.CSRFToken}}

	for name, header := range map[string]http.Header{
		"no proof":       {"Origin": {h.foyer.URL}},
		"wrong proof":    {"Origin": {h.foyer.URL}, session.CSRFHeader: {"guess"}},
		"another origin": {"Origin": {"https://evil.example"}, session.CSRFHeader: {s.CSRFToken}},
		"no origin":      {session.CSRFHeader: {s.CSRFToken}},
	} {
		resp := b.do(http.MethodPost, "/auth/logout", header)
		if resp.code != http.StatusForbidden || !strings.Contains(resp.body, `"kind":"Status"`) {
			t.Errorf("%s: %d %s", name, resp.code, resp.body)
		}
		if code, _ := b.session(); code != http.StatusOK {
			t.Fatalf("%s: a refused logout ended the session", name)
		}
	}
	if resp := b.get("/auth/logout"); resp.code != http.StatusMethodNotAllowed {
		t.Errorf("GET /auth/logout = %d", resp.code)
	}

	resp := b.do(http.MethodPost, "/auth/logout", proof)
	if resp.code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", resp.code, resp.body)
	}
	if b.cookie(session.CookieName) != "" {
		t.Error("the cookie was not cleared")
	}
	// A copy of the cookie is worthless now.
	replay := h.browser()
	u, _ := url.Parse(h.foyer.URL)
	replay.jar.SetCookies(u, []*http.Cookie{{Name: session.CookieName, Value: id}}) //nolint:gosec // a request cookie has no attributes
	if code, _ := replay.session(); code != http.StatusUnauthorized {
		t.Fatalf("a replayed cookie answers %d after logout", code)
	}
	// Signing out when not signed in is not an error.
	if resp := h.browser().do(http.MethodPost, "/auth/logout", http.Header{"Origin": {h.foyer.URL}}); resp.code != http.StatusNoContent {
		t.Errorf("logout without a session: %d", resp.code)
	}
	if resp := h.browser().get("/auth/logged-out"); resp.code != http.StatusOK || !strings.Contains(resp.body, `href="/auth/login"`) {
		t.Errorf("logged-out page: %d", resp.code)
	}
}

// Until the issuer has been discovered, there is no login, and readiness says so.
func TestNotReadyUntilDiscovered(t *testing.T) {
	h := newUndiscovered(t)
	if h.auth.Ready() {
		t.Fatal("ready before discovery")
	}
	assertLoginError(t, h.browser().get("/auth/login"), http.StatusServiceUnavailable, "issuer-unavailable")
	if err := h.auth.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !h.auth.Ready() {
		t.Fatal("not ready after discovery")
	}
}

// Run keeps trying until the issuer answers.
func TestRunRetries(t *testing.T) {
	h := newUndiscovered(t)
	h.issuer.mu.Lock()
	h.issuer.failDiscovery = 1
	h.issuer.mu.Unlock()
	done := make(chan struct{})
	go func() { h.auth.Run(t.Context()); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if !h.auth.Ready() {
		t.Fatal("not ready after Run")
	}
	h.issuer.mu.Lock()
	defer h.issuer.mu.Unlock()
	if h.issuer.failDiscovery != 0 {
		t.Fatal("Run never met the failing issuer")
	}
}

// Logins in progress are bounded: anyone can start one.
func TestTransactionsAreBounded(t *testing.T) {
	h := newHarness(t)
	for range maxTransactions {
		if _, _, ok := h.auth.transactions.begin("/"); !ok {
			t.Fatal("refused below the bound")
		}
	}
	assertLoginError(t, h.browser().get("/auth/login"), http.StatusServiceUnavailable, "too-many-logins")
	// Expired ones make room again.
	h.clock.Advance(transactionLifetime)
	if resp := h.browser().get("/auth/login"); resp.code != http.StatusFound {
		t.Fatalf("after expiry: %d", resp.code)
	}
}

// flakyStore is a memory store that can be made to fail.
type flakyStore struct {
	*session.Memory
	down atomic.Bool
	// deleteFails makes only Delete fail: the session can be read, not ended.
	deleteFails atomic.Bool
}

var errStoreDown = errors.New("store: connection refused")

func (f *flakyStore) Create(ctx context.Context, key session.Key, s session.Session, expires time.Time) error {
	if f.down.Load() {
		return errStoreDown
	}
	return f.Memory.Create(ctx, key, s, expires)
}

func (f *flakyStore) Get(ctx context.Context, key session.Key) (session.Session, error) {
	if f.down.Load() {
		return session.Session{}, errStoreDown
	}
	return f.Memory.Get(ctx, key)
}

func (f *flakyStore) Delete(ctx context.Context, key session.Key) error {
	if f.down.Load() || f.deleteFails.Load() {
		return errStoreDown
	}
	return f.Memory.Delete(ctx, key)
}

// With the store down, nothing pretends: login does not claim success, the session
// is not reported missing, and logout does not report a session ended that is not.
func TestStoreDown(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice, "/")
	_, s := b.session()
	h.store.down.Store(true)

	if code, _ := b.session(); code != http.StatusServiceUnavailable {
		t.Errorf("/auth/session = %d, want 503", code)
	}
	resp := b.do(http.MethodPost, "/auth/logout", http.Header{"Origin": {h.foyer.URL}, session.CSRFHeader: {s.CSRFToken}})
	if resp.code != http.StatusServiceUnavailable {
		t.Errorf("logout = %d, want 503", resp.code)
	}
	if b.cookie(session.CookieName) == "" {
		t.Error("logout cleared the cookie of a session it could not end")
	}

	other := h.browser()
	assertLoginError(t, other.login(alice, "/"), http.StatusServiceUnavailable, "session-store-unavailable")

	h.store.down.Store(false)
	if code, _ := b.session(); code != http.StatusOK {
		t.Errorf("the session did not survive the outage: %d", code)
	}

	// The session can be read but not deleted: logout must not answer as if it
	// had ended it.
	h.store.deleteFails.Store(true)
	resp = b.do(http.MethodPost, "/auth/logout", http.Header{"Origin": {h.foyer.URL}, session.CSRFHeader: {s.CSRFToken}})
	if resp.code != http.StatusServiceUnavailable || b.cookie(session.CookieName) == "" {
		t.Errorf("logout that could not delete = %d, cookie kept %v", resp.code, b.cookie(session.CookieName) != "")
	}
}

// The issuer is reached only over verified TLS and never through a proxy from the
// environment: without its CA, discovery fails, and the transport has no proxy
// function. That is read from the transport, because Go never proxies the loopback
// address the fake issuer listens on, so an environment variable would prove nothing.
func TestIssuerTransport(t *testing.T) {
	h := newUndiscovered(t)
	if tr, ok := h.auth.client.Transport.(*http.Transport); !ok || tr.Proxy != nil {
		t.Fatal("the issuer transport may use a proxy from the environment")
	}
	if err := h.auth.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}

	untrusting, err := New(Config{
		PublicURL: h.foyer.URL, Issuer: h.issuer.URL, ClientID: clientID, ClientSecret: clientSecret,
		Sessions: h.sessions,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := untrusting.Discover(t.Context()); err == nil {
		t.Fatal("discovered an issuer whose certificate nothing vouches for")
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	sessions, err := session.New(session.Config{Store: session.NewMemory(nil), Origin: "https://foyer.example.test", IdleTimeout: time.Hour, AbsoluteTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	good := Config{PublicURL: "https://foyer.example.test", Issuer: "https://dex.example.test", ClientID: clientID, ClientSecret: clientSecret, Sessions: sessions}
	for name, mutate := range map[string]func(*Config){
		"http public URL":      func(c *Config) { c.PublicURL = "http://foyer.example.test" },
		"public URL with path": func(c *Config) { c.PublicURL = "https://foyer.example.test/app" },
		"http issuer":          func(c *Config) { c.Issuer = "http://dex.example.test" },
		"no client ID":         func(c *Config) { c.ClientID = "" },
		"no client secret":     func(c *Config) { c.ClientSecret = "" },
		"no sessions":          func(c *Config) { c.Sessions = nil },
		"no openid scope":      func(c *Config) { c.Scopes = []string{"email"} },
		"offline_access scope": func(c *Config) { c.Scopes = []string{"openid", "offline_access"} },
		"empty scope list":     func(c *Config) { c.Scopes = []string{} },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := good
			mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("New accepted it")
			}
		})
	}
	if _, err := New(good); err != nil {
		t.Fatal(err)
	}
}
