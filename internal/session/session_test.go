package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	origin  = "https://foyer.example.test"
	idToken = "id-token-7d2e91" //nolint:gosec // a marker to search for, not a credential
	maxAge  = 8 * time.Hour
)

// clock is a settable time source.
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

// randomKey is a fresh session key, as a deployment writes it.
func randomKey() string {
	b := make([]byte, KeySize)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// keyA and keyB are two keys the tests rotate between.
var keyA, keyB = randomKey(), randomKey()

func parseKeys(t testing.TB, lines ...string) *Keys {
	t.Helper()
	ks, err := ParseKeys([]byte(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	return ks
}

func testKeys() *Keys {
	ks, err := ParseKeys([]byte(keyA))
	if err != nil {
		panic(err)
	}
	return ks
}

type harness struct {
	m     *Manager
	clock *clock
}

func newHarness(t *testing.T) harness {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	return newHarnessWith(t, c, testKeys(), maxAge)
}

func newHarnessWith(t *testing.T, c *clock, ks *Keys, absolute time.Duration) harness {
	t.Helper()
	m, err := New(Config{Keys: ks, Origin: origin, AbsoluteTimeout: absolute, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	return harness{m: m, clock: c}
}

// restarted is another process with the same clock and these keys: nothing of h's
// survives in it but what the browser holds.
func (h harness) restarted(t *testing.T, ks *Keys) harness {
	t.Helper()
	return newHarnessWith(t, h.clock, ks, maxAge)
}

// user is a session as login hands it over, with a token valid for a day.
func (h harness) user() Session {
	return Session{
		Issuer: "https://dex.example.test", Subject: "alice-sub", Email: "alice@example.com",
		IDToken: idToken, TokenExpiry: h.clock.Now().Add(24 * time.Hour),
	}
}

// start logs in on a request carrying cookies (none, or planted ones) and returns
// the cookie the browser was given.
func (h harness) start(t *testing.T, s Session, cookies ...*http.Cookie) *http.Cookie {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, origin+"/auth/callback", nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	if err := h.m.Start(w, r, s); err != nil {
		t.Fatalf("Start: %v", err)
	}
	set := w.Result().Cookies()
	if len(set) != 1 || set[0].Name != CookieName {
		t.Fatalf("Start set cookies %v, want one %s", set, CookieName)
	}
	return set[0]
}

func request(method string, cookies ...*http.Cookie) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), method, origin+"/k8s/api/v1/namespaces", nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	return r
}

func (h harness) lookup(cookies ...*http.Cookie) (Session, error) {
	return h.m.Lookup(request(http.MethodGet, cookies...))
}

func (h harness) mustLookup(t *testing.T, cookies ...*http.Cookie) Session {
	t.Helper()
	s, err := h.lookup(cookies...)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	return s
}

func assertNoSession(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrNoSession) {
		t.Fatalf("err = %v, want ErrNoSession", err)
	}
}

// presented is a session cookie as a request carries it: only a name and a value
// travel, so attributes have no meaning here.
func presented(value string) *http.Cookie {
	return &http.Cookie{Name: CookieName, Value: value} //nolint:gosec // a request cookie has no attributes
}

// The cookie is host-only, Secure, HttpOnly and SameSite=Lax, lasts as long as the
// session can, and holds nothing readable: not the token, the CSRF token, the ID or
// who the user is.
func TestStartSetsASealedHostOnlyCookie(t *testing.T) {
	h := newHarness(t)
	c := h.start(t, h.user())

	if !strings.HasPrefix(CookieName, "__Host-") {
		t.Errorf("cookie name %q lacks the __Host- prefix, which keeps other hosts from setting it", CookieName)
	}
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" {
		t.Errorf("cookie attributes: Secure=%v HttpOnly=%v SameSite=%v Path=%q Domain=%q",
			c.Secure, c.HttpOnly, c.SameSite, c.Path, c.Domain)
	}
	if c.MaxAge != int(maxAge/time.Second) {
		t.Errorf("Max-Age = %d, want %d", c.MaxAge, int(maxAge/time.Second))
	}

	s := h.mustLookup(t, c)
	if s.IDToken != idToken || s.Email != "alice@example.com" || s.Subject != "alice-sub" ||
		s.Issuer != "https://dex.example.test" {
		t.Errorf("Lookup returned %+v", s)
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		t.Fatalf("cookie value is not unpadded base64url: %v", err)
	}
	for _, secret := range []string{idToken, s.CSRFToken, s.ID, "alice", "dex.example.test"} {
		if strings.Contains(c.Value, secret) || bytes.Contains(raw, []byte(secret)) {
			t.Errorf("the cookie holds %q in the clear", secret)
		}
	}
}

// Two logins never share an ID or a CSRF token, and Start ignores whatever the
// caller put in those, or in the times.
func TestStartIssuesFreshSecrets(t *testing.T) {
	h := newHarness(t)
	s := h.user()
	s.ID, s.CSRFToken = "chosen-by-the-caller", "chosen-by-the-caller"
	s.Issued, s.Expires = h.clock.Now().Add(-time.Hour), h.clock.Now().Add(48*time.Hour)
	a, b := h.mustLookup(t, h.start(t, s)), h.mustLookup(t, h.start(t, s))
	if a.ID == b.ID || a.CSRFToken == b.CSRFToken || a.ID == a.CSRFToken {
		t.Errorf("IDs %q, %q and CSRF tokens %q, %q", a.ID, b.ID, a.CSRFToken, b.CSRFToken)
	}
	for _, v := range []string{a.ID, a.CSRFToken} {
		if v == "chosen-by-the-caller" || len(v) != idLength {
			t.Errorf("Start kept or made %q", v)
		}
	}
	if !a.Issued.Equal(h.clock.Now()) || !a.Expires.Equal(h.clock.Now().Add(maxAge)) {
		t.Errorf("Issued %v, Expires %v, want the login time and %v after it", a.Issued, a.Expires, maxAge)
	}
	if a.Handle() == b.Handle() || strings.Contains(a.Handle(), a.ID) {
		t.Errorf("handles %q and %q for IDs %q and %q", a.Handle(), b.Handle(), a.ID, b.ID)
	}
}

// A session cannot start without a token the API server will accept.
func TestStartRefusesAnUnusableToken(t *testing.T) {
	h := newHarness(t)
	for name, mutate := range map[string]func(*Session){
		"no token":                     func(s *Session) { s.IDToken = "" },
		"no expiry":                    func(s *Session) { s.TokenExpiry = time.Time{} },
		"expired token":                func(s *Session) { s.TokenExpiry = h.clock.Now() },
		"token expiring in the second": func(s *Session) { s.TokenExpiry = h.clock.Now().Add(time.Second / 2) },
	} {
		t.Run(name, func(t *testing.T) {
			s := h.user()
			mutate(&s)
			w := httptest.NewRecorder()
			if err := h.m.Start(w, request(http.MethodGet), s); err == nil {
				t.Fatal("Start accepted it")
			}
			if len(w.Result().Cookies()) != 0 {
				t.Error("a cookie was set anyway")
			}
		})
	}
}

// A process started later with the same keys knows the session as it was: the same
// user, token, ID, CSRF token and deadlines. Nothing about it was kept anywhere else.
func TestSessionSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	c := h.start(t, h.user())
	before := h.mustLookup(t, c)
	h.clock.Advance(time.Hour)

	after := h.restarted(t, testKeys()).mustLookup(t, c)
	if after != before {
		t.Fatalf("after a restart the session is %+v, was %+v", after, before)
	}
	if after.Handle() != before.Handle() {
		t.Error("the session's handle changed with the process")
	}
}

// Login always issues a new ID, so a cookie an attacker planted never becomes the
// signed-in session (session fixation). The old cookie is not revoked: a copy of it
// stays the session it was until that expires. That is the limit of keeping sessions
// in cookies, and this test records it.
func TestStartRotatesTheIDButRevokesNothing(t *testing.T) {
	h := newHarness(t)
	planted := h.start(t, h.user())
	c := h.start(t, h.user(), planted)
	if h.mustLookup(t, c).ID == h.mustLookup(t, planted).ID {
		t.Fatal("login adopted the planted session")
	}
	if _, err := h.lookup(planted); err != nil {
		t.Fatalf("the planted cookie stopped being a session (%v): this test records that it does not", err)
	}
}

// Every way of not having exactly one cookie holding a session is no session, and
// none of them is told apart from another.
func TestLookupWithoutAValidCookie(t *testing.T) {
	h := newHarness(t)
	valid := h.start(t, h.user())
	other := h.start(t, h.user())
	raw, _ := base64.RawURLEncoding.DecodeString(valid.Value)
	reencode := func(b []byte) *http.Cookie { return presented(base64.RawURLEncoding.EncodeToString(b)) }
	for name, cookies := range map[string][]*http.Cookie{
		"no cookie":        nil,
		"empty":            {presented("")},
		"garbage":          {presented("not-a-session")},
		"cut short":        {presented(valid.Value[:len(valid.Value)-1])},
		"a byte short":     {reencode(raw[:len(raw)-1])},
		"a byte more":      {reencode(append(append([]byte{}, raw...), 0))},
		"header only":      {reencode(raw[:1+keyIDSize])},
		"header and nonce": {reencode(raw[:1+keyIDSize+12])},
		"padded":           {presented(valid.Value + "=")},
		"standard base64":  {presented(base64.RawStdEncoding.EncodeToString(raw))},
		"other version":    {reencode(append([]byte{cookieVersion + 1}, raw[1:]...))},
		"other key ID":     {reencode(append(append([]byte{raw[0]}, bytes.Repeat([]byte{0}, keyIDSize)...), raw[1+keyIDSize:]...))},
		"other name":       {{Name: "krm-foyer-session", Value: valid.Value}}, //nolint:gosec // a request cookie has no attributes
		"two cookies":      {valid, other},
		"valid and forged": {valid, presented("forged")},
		"forged and valid": {presented("forged"), valid},
		"the same twice":   {valid, valid},
		"too long":         {presented(valid.Value + strings.Repeat("A", MaxCookieValue))},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.lookup(cookies...)
			assertNoSession(t, err)
		})
	}
}

// The value has one spelling: setting a spare bit of its last character, which
// decodes to the same bytes in a lenient decoder, is no session.
func TestCookieHasOneSpelling(t *testing.T) {
	h := newHarness(t)
	for range 8 {
		c := h.start(t, h.user())
		if len(c.Value)%4 == 0 {
			continue // no spare bits at this length
		}
		_, err := h.lookup(presented(respell(c.Value)))
		assertNoSession(t, err)
	}
}

// respell sets a spare bit in the last character of an unpadded base64url string.
func respell(v string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	i := strings.IndexByte(alphabet, v[len(v)-1])
	return v[:len(v)-1] + string(alphabet[i|1])
}

// Changing any bit of a cookie makes it no session: the version, the key's ID, the
// nonce and the ciphertext are all authenticated.
func TestEveryBitIsAuthenticated(t *testing.T) {
	h := newHarness(t)
	c := h.start(t, h.user())
	raw, _ := base64.RawURLEncoding.DecodeString(c.Value)
	for i := range raw {
		for _, bit := range []byte{0x01, 0x80} {
			changed := append([]byte{}, raw...)
			changed[i] ^= bit
			if _, err := h.lookup(presented(base64.RawURLEncoding.EncodeToString(changed))); !errors.Is(err, ErrNoSession) {
				t.Fatalf("byte %d with bit %#x flipped: err = %v, want ErrNoSession", i, bit, err)
			}
		}
	}
}

// seal seals any plaintext as a cookie of this version with the active key, as
// krm-foyer would: what a key holder could make, to check what unseal accepts.
func seal(m *Manager, version byte, plain []byte) *http.Cookie {
	k := m.keys.sealing()
	header := append([]byte{version}, k.id[:]...)
	nonce := make([]byte, k.aead.NonceSize())
	_, _ = rand.Read(nonce)
	return presented(base64.RawURLEncoding.EncodeToString(k.aead.Seal(append(header, nonce...), nonce, plain, additionalData(header))))
}

// Even sealed with the right key, a cookie must hold a whole session of this
// version, and nothing else.
func TestSealedPayloadIsValidated(t *testing.T) {
	h := newHarness(t)
	s := h.mustLookup(t, h.start(t, h.user()))
	good := payload{
		Version: cookieVersion, ID: s.ID, CSRFToken: s.CSRFToken, Issued: s.Issued.Unix(), Expires: s.Expires.Unix(),
		Issuer: s.Issuer, Subject: s.Subject, Email: s.Email, IDToken: s.IDToken,
	}
	marshal := func(p any) []byte {
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if _, err := h.lookup(seal(h.m, cookieVersion, marshal(good))); err != nil {
		t.Fatalf("the payload as krm-foyer seals it: %v", err)
	}
	for name, plain := range map[string][]byte{
		"not JSON":          []byte("not json"),
		"two documents":     append(marshal(good), marshal(good)...),
		"an unknown field":  bytes.Replace(marshal(good), []byte(`{`), []byte(`{"admin":true,`), 1),
		"no token":          marshal(with(good, func(p *payload) { p.IDToken = "" })),
		"no CSRF token":     marshal(with(good, func(p *payload) { p.CSRFToken = "" })),
		"a short ID":        marshal(with(good, func(p *payload) { p.ID = "short" })),
		"another version":   marshal(with(good, func(p *payload) { p.Version = cookieVersion + 1 })),
		"ends as it starts": marshal(with(good, func(p *payload) { p.Expires = p.Issued })),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.lookup(seal(h.m, cookieVersion, plain))
			assertNoSession(t, err)
		})
	}
	// A good payload under another version's header is no session either.
	_, err := h.lookup(seal(h.m, cookieVersion+1, marshal(good)))
	assertNoSession(t, err)
}

func with(p payload, edit func(*payload)) payload {
	edit(&p)
	return p
}

// A cookie sealed for another purpose, here a login in progress sealed with the same
// key under another name, is no session: the cookie's name is authenticated.
func TestCookieIsBoundToItsName(t *testing.T) {
	h := newHarness(t)
	s := h.mustLookup(t, h.start(t, h.user()))
	plain, _ := json.Marshal(payload{
		Version: cookieVersion, ID: s.ID, CSRFToken: s.CSRFToken, Issued: s.Issued.Unix(), Expires: s.Expires.Unix(),
		IDToken: s.IDToken,
	})
	k := h.m.keys.sealing()
	header := append([]byte{cookieVersion}, k.id[:]...)
	nonce := make([]byte, k.aead.NonceSize())
	other := append([]byte("krm-foyer session cookie\x00__Host-krm-foyer-login-x\x00"), header...)
	value := base64.RawURLEncoding.EncodeToString(k.aead.Seal(append(header, nonce...), nonce, plain, other))
	_, err := h.lookup(presented(value))
	assertNoSession(t, err)
}

// Rotation: the first key seals, every key opens. With the new key added in front,
// cookies of the old one still open and new ones are sealed with the new one; once
// the old key is removed, its cookies are no session, and the new key's still are.
func TestKeyRotation(t *testing.T) {
	h := newHarness(t) // keyA
	old := h.start(t, h.user())

	overlap := h.restarted(t, parseKeys(t, keyB, keyA))
	if _, err := overlap.lookup(old); err != nil {
		t.Fatalf("during the overlap, a cookie of the previous key: %v", err)
	}
	renewed := overlap.start(t, overlap.user())
	if _, err := h.lookup(renewed); !errors.Is(err, ErrNoSession) {
		t.Fatal("a new cookie was sealed with the previous key, not the first one")
	}

	after := h.restarted(t, parseKeys(t, keyB))
	_, err := after.lookup(old)
	assertNoSession(t, err)
	if _, err := after.lookup(renewed); err != nil {
		t.Fatalf("a cookie of the remaining key: %v", err)
	}

	// Putting the old key back makes its unexpired cookies sessions again: rolling
	// back to a removed key is a decision, and docs/design.md says so.
	if _, err := h.restarted(t, parseKeys(t, keyB, keyA)).lookup(old); err != nil {
		t.Fatalf("with the old key restored: %v", err)
	}
}

// Expiry: a session ends at its absolute timeout or its token's expiry, whichever is
// first, by krm-foyer's clock, however the cookie is used and whether or not
// krm-foyer ran in between. Nothing renews it, and there is no idle timeout.
func TestExpiry(t *testing.T) {
	for name, tc := range map[string]struct {
		tokenLife, end time.Duration
	}{
		"the absolute timeout first": {24 * time.Hour, maxAge},
		"the token's expiry first":   {time.Hour, time.Hour},
		// Seconds: a cookie holds whole seconds, and never more than the token had.
		"a token expiring mid-second": {time.Hour + 999*time.Millisecond, time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			s := h.user()
			s.TokenExpiry = h.clock.Now().Add(tc.tokenLife)
			c := h.start(t, s)
			if c.MaxAge != int(tc.end/time.Second) {
				t.Errorf("Max-Age = %d, want %d", c.MaxAge, int(tc.end/time.Second))
			}
			// Unused for almost all of it, then used: still a session.
			h.clock.Advance(tc.end - time.Second)
			if _, err := h.lookup(c); err != nil {
				t.Fatalf("a second before the end: %v", err)
			}
			// A restart in the last second changes nothing.
			h.clock.Advance(time.Second)
			_, err := h.restarted(t, testKeys()).lookup(c)
			assertNoSession(t, err)
			_, err = h.lookup(c)
			assertNoSession(t, err)
		})
	}
}

// Lowering the absolute timeout shortens the sessions already issued too: the cookie
// says when it was issued, and the configured timeout is measured from that.
func TestLowerTimeoutAppliesToIssuedCookies(t *testing.T) {
	h := newHarness(t)
	c := h.start(t, h.user())
	h.clock.Advance(2 * time.Hour)
	if _, err := newHarnessWith(t, h.clock, testKeys(), 3*time.Hour).lookup(c); err != nil {
		t.Fatalf("within the lower timeout: %v", err)
	}
	_, err := newHarnessWith(t, h.clock, testKeys(), 2*time.Hour).lookup(c)
	assertNoSession(t, err)
	// Raising it does not lengthen one: the cookie keeps the end it was issued with.
	h.clock.Advance(maxAge)
	_, err = newHarnessWith(t, h.clock, testKeys(), 24*time.Hour).lookup(c)
	assertNoSession(t, err)
}

// realisticToken is an ID token shaped like Dex's: a JWS of a header, claims with
// this many groups of this length each, and an RS256 signature.
func realisticToken(groups, groupLen int) string {
	enc := base64.RawURLEncoding.EncodeToString
	names := make([]string, groups)
	for i := range names {
		names[i] = strings.Repeat("g", groupLen)
	}
	claims, _ := json.Marshal(map[string]any{
		"iss": "https://dex.example.test/dex", "sub": "CiQ0ZjZiNDA4Mi1hYjFiLTRiZjMtODZkNy0wNjQwMGQ5NzM2ZjkSBWxvY2Fs",
		"aud": "krm-foyer", "exp": 1790000000, "iat": 1789913600, "nonce": strings.Repeat("n", 43),
		"at_hash": strings.Repeat("h", 22), "email": "alice.longname@example.com", "email_verified": true,
		"name": "Alice Longname", "groups": names,
		"federated_claims": map[string]string{"connector_id": "audience", "user_id": strings.Repeat("u", 36)},
	})
	return enc([]byte(`{"alg":"RS256","kid":"`+strings.Repeat("k", 40)+`"}`)) + "." + enc(claims) + "." +
		enc(bytes.Repeat([]byte{1}, 256))
}

// Size: a session with a realistic token fits, with room to spare, and the whole
// Set-Cookie header stays inside what every browser keeps. One that does not fit is
// refused with ErrTooLarge and sets no cookie: nothing is cut short or kept aside.
func TestCookieSize(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		groups, groupLen int
		fits             bool
	}{
		{0, 0, true},
		{10, 20, true},
		{30, 24, true},
		{200, 30, false},
	} {
		s := h.user()
		s.IDToken = realisticToken(tc.groups, tc.groupLen)
		w := httptest.NewRecorder()
		err := h.m.Start(w, request(http.MethodGet), s)
		header := w.Result().Header.Values("Set-Cookie")
		t.Logf("%d groups of %d: token %d bytes, cookie %d bytes", tc.groups, tc.groupLen, len(s.IDToken),
			len(strings.Join(header, "")))
		if !tc.fits {
			if !errors.Is(err, ErrTooLarge) || len(header) != 0 {
				t.Errorf("%d groups of %d: err = %v, Set-Cookie %d, want ErrTooLarge and no cookie", tc.groups, tc.groupLen, err, len(header))
			}
			continue
		}
		if err != nil || len(header) != 1 {
			t.Fatalf("%d groups of %d: err = %v, %d cookies", tc.groups, tc.groupLen, err, len(header))
		}
		// Browsers keep a cookie whose name and value are 4,096 bytes at most, and the
		// attributes have a limit of their own (1,024 bytes each).
		name, _, _ := strings.Cut(header[0], ";")
		if len(name) > 4096 {
			t.Errorf("name and value are %d bytes", len(name))
		}
		if _, err := h.lookup(presented(strings.TrimPrefix(name, CookieName+"="))); err != nil {
			t.Errorf("the cookie as set: %v", err)
		}
	}

	// The bound is on the encoded value, and is checked before a cookie is set.
	s := h.user()
	s.IDToken = strings.Repeat("t", MaxCookieValue)
	if err := h.m.Start(httptest.NewRecorder(), request(http.MethodGet), s); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a token as long as the bound: err = %v", err)
	}
	if err := h.m.Start(httptest.NewRecorder(), request(http.MethodGet), s); strings.Contains(err.Error(), s.IDToken[:64]) {
		t.Error("the error quotes the token")
	}
}

// End tells the browser to drop the cookie, and ends the responses its session has
// open in this process. It revokes nothing: a copy of the cookie is still the
// session until it expires. That is the deliberate limit this test records.
func TestEnd(t *testing.T) {
	h := newHarness(t)
	c := h.start(t, h.user())
	w := httptest.NewRecorder()
	h.m.End(w, request(http.MethodPost, c))

	cleared := w.Result().Cookies()
	if len(cleared) != 1 || cleared[0].Name != CookieName || cleared[0].MaxAge >= 0 || cleared[0].Value != "" ||
		!cleared[0].Secure || !cleared[0].HttpOnly || cleared[0].Path != "/" {
		t.Errorf("End set %v, want the session cookie cleared", cleared)
	}
	if _, err := h.lookup(c); err != nil {
		t.Fatalf("a copy of the cookie stopped being a session after End (%v): this test records that it does not", err)
	}
	// Ending without a session, or with garbage, still clears the cookie.
	for _, r := range []*http.Request{request(http.MethodPost), request(http.MethodPost, presented("garbage"))} {
		w := httptest.NewRecorder()
		h.m.End(w, r)
		if len(w.Result().Cookies()) != 1 {
			t.Error("End without a session did not clear the cookie")
		}
	}
}

// Watch: a response is live until its session expires, or until its session is
// ended in this process, by a logout or by a login in the same browser. Responses of
// other sessions, and responses a copy of the cookie opens later, are not affected.
func TestWatch(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, other := h.start(t, h.user()), h.start(t, h.user())
	s, so := h.mustLookup(t, c), h.mustLookup(t, other)

	open, openOther := h.m.Watch(ctx, s), h.m.Watch(ctx, so)
	if !open() || !openOther() {
		t.Fatal("a fresh response is not live")
	}
	h.m.End(httptest.NewRecorder(), request(http.MethodPost, c))
	if open() {
		t.Error("a response stayed live after its session's logout")
	}
	if !openOther() {
		t.Error("another session's response ended with this logout")
	}
	if !h.m.Watch(ctx, s)() {
		t.Error("a response opened after the logout, by a copy of the cookie, is not live: this test records that it is")
	}

	// A login in the same browser ends the responses of the session it replaces.
	h.start(t, h.user(), other)
	if openOther() {
		t.Error("a response stayed live after a new login in its browser")
	}

	// Expiry ends a response by itself.
	late := h.m.Watch(ctx, h.mustLookup(t, h.start(t, h.user())))
	h.clock.Advance(maxAge)
	if late() {
		t.Error("a response stayed live past its session's expiry")
	}
}

// A logout in another process does not reach this one: there is nothing shared to
// carry it. This test records that limit.
func TestEndIsLocalToTheProcess(t *testing.T) {
	h := newHarness(t)
	c := h.start(t, h.user())
	elsewhere := h.restarted(t, testKeys())
	open := elsewhere.m.Watch(context.Background(), elsewhere.mustLookup(t, c))
	h.m.End(httptest.NewRecorder(), request(http.MethodPost, c))
	if !open() {
		t.Error("a logout in one process ended a response in another: this test records that it does not")
	}
}

// The registry of open responses forgets each one when its request ends, so it holds
// nothing but what is open.
func TestWatchForgetsEndedResponses(t *testing.T) {
	h := newHarness(t)
	s := h.mustLookup(t, h.start(t, h.user()))
	var cancels []context.CancelFunc
	for range 100 {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		h.m.Watch(ctx, s)
	}
	if n := h.m.open.count(); n != 100 {
		t.Fatalf("%d registered, want 100", n)
	}
	for _, cancel := range cancels {
		cancel()
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.m.open.count() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d still registered after their requests ended", h.m.open.count())
		}
		time.Sleep(time.Millisecond)
	}
	h.m.End(httptest.NewRecorder(), request(http.MethodPost, presented("garbage")))
	if len(h.m.open.open) != 0 {
		t.Error("End of no session left an entry")
	}
}

func TestParseKeys(t *testing.T) {
	for name, text := range map[string]string{
		"one key":                  keyA,
		"with a trailing newline":  keyA + "\n",
		"two keys and blank lines": "\n" + keyA + "\n\n  " + keyB + "  \r\n",
		"as many as allowed":       strings.Join([]string{randomKey(), randomKey(), randomKey(), randomKey()}, "\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseKeys([]byte(text)); err != nil {
				t.Fatal(err)
			}
		})
	}
	short := base64.StdEncoding.EncodeToString(make([]byte, KeySize-1))
	long := base64.StdEncoding.EncodeToString(make([]byte, KeySize+1))
	urlSafe := base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, KeySize))
	for name, text := range map[string]string{
		"empty":            "",
		"blank":            "\n \n",
		"too short":        short,
		"too long":         long,
		"unpadded":         strings.TrimRight(keyA, "="),
		"URL alphabet":     urlSafe,
		"not base64":       "this is not a key at all, though it is long enough",
		"a repeated key":   keyA + "\n" + keyB + "\n" + keyA,
		"too many keys":    strings.Join([]string{randomKey(), randomKey(), randomKey(), randomKey(), randomKey()}, "\n"),
		"one good one bad": keyA + "\n" + short,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseKeys([]byte(text))
			if err == nil {
				t.Fatal("ParseKeys accepted it")
			}
			// The error says which line, never what is on it.
			for _, line := range strings.Split(text, "\n") {
				if len(line) > 8 && strings.Contains(err.Error(), strings.TrimSpace(line)) {
					t.Errorf("the error quotes a line: %v", err)
				}
			}
		})
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	good := Config{Keys: testKeys(), Origin: origin, AbsoluteTimeout: maxAge}
	for name, mutate := range map[string]func(*Config){
		"no keys":            func(c *Config) { c.Keys = nil },
		"empty keys":         func(c *Config) { c.Keys = &Keys{} },
		"no origin":          func(c *Config) { c.Origin = "" },
		"http origin":        func(c *Config) { c.Origin = "http://foyer.example.test" },
		"origin with a path": func(c *Config) { c.Origin = origin + "/app" },
		"origin with query":  func(c *Config) { c.Origin = origin + "?x" },
		"origin with user":   func(c *Config) { c.Origin = "https://u@foyer.example.test" },
		"no host":            func(c *Config) { c.Origin = "https://" },
		"zero timeout":       func(c *Config) { c.AbsoluteTimeout = 0 },
		"negative timeout":   func(c *Config) { c.AbsoluteTimeout = -time.Hour },
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
		t.Fatalf("good config: %v", err)
	}
}

// BenchmarkSealAndOpen is what a session costs krm-foyer: sealing one at login, and
// opening one, which every request does, with a token shaped like Dex's.
func BenchmarkSealAndOpen(b *testing.B) {
	c := &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	m, err := New(Config{Keys: parseKeys(b, keyB, keyA), Origin: origin, AbsoluteTimeout: maxAge, Now: c.Now})
	if err != nil {
		b.Fatal(err)
	}
	s := Session{
		Issuer: "https://dex.example.test", Subject: "alice-sub", Email: "alice@example.com",
		IDToken: realisticToken(10, 20), TokenExpiry: c.Now().Add(time.Hour),
	}
	w := httptest.NewRecorder()
	if err := m.Start(w, request(http.MethodGet), s); err != nil {
		b.Fatal(err)
	}
	cookie := w.Result().Cookies()[0]
	b.Run("seal", func(b *testing.B) {
		for b.Loop() {
			if err := m.Start(httptest.NewRecorder(), request(http.MethodGet), s); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("open", func(b *testing.B) {
		r := request(http.MethodGet, cookie)
		for b.Loop() {
			if _, err := m.Lookup(r); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// FuzzForgedCookie states the boundary from the attacker's side, without reading the
// code: whatever value a browser presents, it is a session only if it is exactly the
// value krm-foyer set at a login, and then that login's session.
func FuzzForgedCookie(f *testing.F) {
	c := &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	m, err := New(Config{Keys: parseKeys(f, keyA, keyB), Origin: origin, AbsoluteTimeout: maxAge, Now: c.Now})
	if err != nil {
		f.Fatal(err)
	}
	issued := map[string]string{}
	for _, email := range []string{"alice@example.com", "bob@example.com"} {
		w := httptest.NewRecorder()
		s := Session{Subject: email, Email: email, IDToken: idToken + email, TokenExpiry: c.Now().Add(time.Hour)}
		if err := m.Start(w, request(http.MethodGet), s); err != nil {
			f.Fatal(err)
		}
		value := w.Result().Cookies()[0].Value
		issued[value] = email
		f.Add(value)
		f.Add(value[:len(value)/2])
		f.Add(strings.ToUpper(value))
	}
	f.Add("")
	f.Add("AQ")
	f.Fuzz(func(t *testing.T, value string) {
		s, err := m.Lookup(request(http.MethodGet, presented(value)))
		email, ok := issued[value]
		if ok != (err == nil) {
			t.Fatalf("Lookup(%q) = %v; issued by krm-foyer: %v", value, err, ok)
		}
		if ok && (s.Email != email || s.IDToken != idToken+email) {
			t.Fatalf("Lookup of %s's cookie returned %+v", email, s)
		}
	})
}
