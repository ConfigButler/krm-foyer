package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
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
	idle    = 30 * time.Minute
	maxAge  = 8 * time.Hour
)

// clock is a settable time source shared by the manager and its store.
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

type harness struct {
	m     *Manager
	store Store
	clock *clock
}

func newHarness(t *testing.T) harness {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	return newHarnessWith(t, c, NewMemory(c.Now))
}

func newHarnessWith(t *testing.T, c *clock, store Store) harness {
	t.Helper()
	m, err := New(Config{Store: store, Origin: origin, IdleTimeout: idle, AbsoluteTimeout: maxAge, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	return harness{m: m, store: store, clock: c}
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
	if err := h.m.Start(context.Background(), w, r, s); err != nil {
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

func assertNoSession(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrNoSession) {
		t.Fatalf("err = %v, want ErrNoSession", err)
	}
}

// The cookie is the only thing the browser holds: opaque, host-only, Secure,
// HttpOnly and SameSite=Lax, and its value is long enough not to be guessed.
func TestStartSetsAHostOnlyOpaqueCookie(t *testing.T) {
	h := newHarness(t)
	c := h.start(t, h.user())

	if !strings.HasPrefix(CookieName, "__Host-") {
		t.Errorf("cookie name %q lacks the __Host- prefix, which keeps other hosts from setting it", CookieName)
	}
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" {
		t.Errorf("cookie attributes: Secure=%v HttpOnly=%v SameSite=%v Path=%q Domain=%q",
			c.Secure, c.HttpOnly, c.SameSite, c.Path, c.Domain)
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil || len(raw) != 32 {
		t.Errorf("cookie value %q is not 32 random bytes in base64url", c.Value)
	}
	if strings.Contains(c.Value, idToken) {
		t.Error("the cookie carries the token")
	}
	// It lasts as long as the session can, and no longer.
	if c.MaxAge != int(maxAge/time.Second) {
		t.Errorf("Max-Age = %d, want %d", c.MaxAge, int(maxAge/time.Second))
	}

	got, err := h.lookup(c)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.IDToken != idToken || got.Email != "alice@example.com" {
		t.Errorf("Lookup returned %+v", got)
	}
}

// Two logins never share an ID or a CSRF token, and Start ignores whatever CSRF token
// and timestamps the caller put in.
func TestStartIssuesFreshSecrets(t *testing.T) {
	h := newHarness(t)
	s := h.user()
	s.CSRFToken = "chosen-by-the-caller"
	s.Created = h.clock.Now().Add(-time.Hour)
	a, b := h.start(t, s), h.start(t, s)
	if a.Value == b.Value {
		t.Fatal("two logins got the same session ID")
	}
	sa, _ := h.lookup(a)
	sb, _ := h.lookup(b)
	if sa.CSRFToken == "" || sa.CSRFToken == sb.CSRFToken || sa.CSRFToken == "chosen-by-the-caller" {
		t.Errorf("CSRF tokens %q and %q", sa.CSRFToken, sb.CSRFToken)
	}
	if !sa.Created.Equal(h.clock.Now()) {
		t.Errorf("Created = %v, want the time of login", sa.Created)
	}
}

// A session cannot start without a token the API server will accept.
func TestStartRefusesAnUnusableToken(t *testing.T) {
	h := newHarness(t)
	for name, mutate := range map[string]func(*Session){
		"no token":      func(s *Session) { s.IDToken = "" },
		"no expiry":     func(s *Session) { s.TokenExpiry = time.Time{} },
		"expired token": func(s *Session) { s.TokenExpiry = h.clock.Now() },
	} {
		t.Run(name, func(t *testing.T) {
			s := h.user()
			mutate(&s)
			w := httptest.NewRecorder()
			if err := h.m.Start(context.Background(), w, request(http.MethodGet), s); err == nil {
				t.Fatal("Start accepted it")
			}
			if len(w.Result().Cookies()) != 0 {
				t.Error("a cookie was set anyway")
			}
		})
	}
}

// Login rotates the ID: a cookie the browser already had, its own or one an attacker
// planted, never becomes the signed-in session (session fixation), and a session it
// named is ended.
func TestStartRotatesTheID(t *testing.T) {
	h := newHarness(t)
	planted := presented(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	c := h.start(t, h.user(), planted)
	if c.Value == planted.Value {
		t.Fatal("login adopted the planted ID")
	}
	_, err := h.lookup(planted)
	assertNoSession(t, err)

	old := h.start(t, h.user())
	renewed := h.start(t, h.user(), old)
	if renewed.Value == old.Value {
		t.Fatal("login kept the previous ID")
	}
	_, err = h.lookup(old)
	assertNoSession(t, err)
	if _, err := h.lookup(renewed); err != nil {
		t.Fatalf("new session: %v", err)
	}
}

// Every way of not having exactly one valid cookie is no session.
func TestLookupWithoutAValidCookie(t *testing.T) {
	h := newHarness(t)
	valid := h.start(t, h.user())
	other := h.start(t, h.user())
	unknown := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	for name, cookies := range map[string][]*http.Cookie{
		"no cookie":  nil,
		"unknown ID": {presented(unknown)},
		"empty":      {presented("")},
		"too short":  {presented(valid.Value[:42])},
		"too long":   {presented(valid.Value + "A")},
		"padded":     {presented(valid.Value + "=")},
		// 43 characters carry 258 bits; the last two must be zero, so an ID has one
		// spelling and not four.
		"same bytes, other spelling": {presented(respell(valid.Value))},
		// The right length, with a character from standard base64's alphabet.
		"standard base64":  {presented(valid.Value[:42] + "+")},
		"other name":       {{Name: "krm-foyer-session", Value: valid.Value}}, //nolint:gosec // a request cookie has no attributes
		"two cookies":      {valid, other},
		"valid and forged": {valid, presented(unknown)},
		"forged and valid": {presented(unknown), valid},
		"the same twice":   {valid, valid},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.lookup(cookies...)
			assertNoSession(t, err)
		})
	}
}

// A session ID has exactly one spelling: the same 32 bytes in standard base64, or
// with the spare bits set, or padded, name no session.
func TestIDHasOneSpelling(t *testing.T) {
	raw := bytes.Repeat([]byte{0xfb, 0xff, 0xbf}, 11)[:32] // encodes to '-' and '_'
	id := base64.RawURLEncoding.EncodeToString(raw)
	if !strings.ContainsAny(id, "-_") {
		t.Fatalf("%q does not exercise the URL alphabet", id)
	}
	key, ok := keyOf(id)
	if !ok || key != sha256.Sum256(raw) {
		t.Fatalf("keyOf(%q) = %x, %v", id, key, ok)
	}
	for _, other := range []string{
		base64.RawStdEncoding.EncodeToString(raw),
		base64.URLEncoding.EncodeToString(raw),
		respell(id),
	} {
		if _, ok := keyOf(other); ok {
			t.Errorf("keyOf accepted %q, another spelling of %q", other, id)
		}
	}
}

// presented is a session cookie as a request carries it: only a name and a value
// travel, so attributes have no meaning here.
func presented(value string) *http.Cookie {
	return &http.Cookie{Name: CookieName, Value: value} //nolint:gosec // a request cookie has no attributes
}

// respell sets a spare bit in the last character of an unpadded base64url string.
func respell(v string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	i := strings.IndexByte(alphabet, v[len(v)-1])
	return v[:len(v)-1] + string(alphabet[i|1])
}

// The store holds a hash of the ID, never the ID: reading it yields nothing a
// browser could present.
func TestStoreNeverSeesTheID(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	rec := &recordingStore{Store: NewMemory(c.Now)}
	h := newHarnessWith(t, c, rec)
	cookie := h.start(t, h.user())
	if _, err := h.lookup(cookie); err != nil {
		t.Fatal(err)
	}
	if err := h.m.End(context.Background(), httptest.NewRecorder(), request(http.MethodPost, cookie)); err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(cookie.Value)
	seen := rec.String()
	for _, secret := range []string{cookie.Value, string(raw), fmt.Sprintf("%v", raw), fmt.Sprintf("%x", raw)} {
		if strings.Contains(seen, secret) {
			t.Fatalf("the store was given the session ID: %s", seen)
		}
	}
	if !strings.Contains(seen, fmt.Sprintf("%v", Key(sha256.Sum256(raw)))) {
		t.Fatalf("the store was not keyed by the ID's SHA-256: %s", seen)
	}
}

// recordingStore writes down every argument a store is given.
type recordingStore struct {
	Store
	mu  sync.Mutex
	log strings.Builder
}

func (r *recordingStore) record(args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintln(&r.log, args...)
}

func (r *recordingStore) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.log.String()
}

func (r *recordingStore) Create(ctx context.Context, key Key, s Session, expires time.Time) error {
	r.record("create", key, fmt.Sprintf("%+v", s), expires)
	return r.Store.Create(ctx, key, s, expires)
}

func (r *recordingStore) Get(ctx context.Context, key Key) (Session, error) {
	r.record("get", key)
	return r.Store.Get(ctx, key)
}

func (r *recordingStore) Touch(ctx context.Context, key Key, lastSeen, expires time.Time) error {
	r.record("touch", key, lastSeen, expires)
	return r.Store.Touch(ctx, key, lastSeen, expires)
}

func (r *recordingStore) Delete(ctx context.Context, key Key) error {
	r.record("delete", key)
	return r.Store.Delete(ctx, key)
}

// Expiry: a session ends at the first of its idle deadline, its absolute deadline
// and its token's expiry, and is gone from the store after that.
func TestExpiry(t *testing.T) {
	for name, tc := range map[string]struct {
		tokenLife time.Duration
		// steps of inactivity, each followed by a request
		steps []time.Duration
		// alive after each step
		alive []bool
	}{
		"used within the idle timeout": {24 * time.Hour, []time.Duration{idle - time.Second, idle - time.Second}, []bool{true, true}},
		"idle exactly the timeout":     {24 * time.Hour, []time.Duration{idle}, []bool{false}},
		"idle longer":                  {24 * time.Hour, []time.Duration{idle + time.Second}, []bool{false}},
		// Activity extends the idle deadline but never the absolute one.
		"busy up to the absolute timeout": {24 * time.Hour, nil, nil},
		"token expires first": {time.Hour,
			[]time.Duration{20 * time.Minute, 20 * time.Minute, 20*time.Minute - time.Second, time.Second},
			[]bool{true, true, true, false}},
	} {
		for store, harness := range map[string]func(*testing.T) harness{
			"memory": newHarness,
			// A store that keeps everything (as one with coarse TTLs might, for a
			// while): the Manager has to decide expiry on its own.
			"keeping": newKeepingHarness,
		} {
			t.Run(name+"/"+store, func(t *testing.T) {
				h := harness(t)
				testExpiry(t, h, tc.tokenLife, tc.steps, tc.alive)
			})
		}
	}
}

// testExpiry makes a request after each step of inactivity and checks the session
// is alive exactly when alive says. With no steps, it makes one every 15 minutes
// until the absolute deadline.
func testExpiry(t *testing.T, h harness, tokenLife time.Duration, steps []time.Duration, alive []bool) {
	t.Helper()
	s := h.user()
	s.TokenExpiry = h.clock.Now().Add(tokenLife)
	c := h.start(t, s)
	if steps == nil {
		for elapsed := 15 * time.Minute; elapsed <= maxAge; elapsed += 15 * time.Minute {
			steps = append(steps, 15*time.Minute)
			alive = append(alive, elapsed < maxAge)
		}
	}
	for i, d := range steps {
		h.clock.Advance(d)
		_, err := h.lookup(c)
		if alive[i] {
			if err != nil {
				t.Fatalf("step %d: session ended early: %v", i, err)
			}
			continue
		}
		assertNoSession(t, err)
		// Gone, not only refused: a clock moving back cannot revive it.
		key, _ := keyOf(c.Value)
		if _, err := h.store.Get(context.Background(), key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expired session still in the store: %v", err)
		}
		return
	}
}

// newKeepingHarness gives the store a clock that never moves, so it never expires
// a session by itself.
func newKeepingHarness(t *testing.T) harness {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	frozen := c.Now()
	return newHarnessWith(t, c, NewMemory(func() time.Time { return frozen }))
}

// The cookie lasts no longer than the token, when the token ends first.
func TestCookieLastsNoLongerThanTheToken(t *testing.T) {
	h := newHarness(t)
	s := h.user()
	s.TokenExpiry = h.clock.Now().Add(time.Hour)
	if c := h.start(t, s); c.MaxAge != 3600 {
		t.Errorf("Max-Age = %d, want 3600", c.MaxAge)
	}
}

// A request that only reads touches the session; the idle deadline moves, the
// absolute one does not.
func TestLookupMovesTheIdleDeadline(t *testing.T) {
	h := newHarness(t)
	c := h.start(t, h.user())
	h.clock.Advance(idle / 2)
	if _, err := h.lookup(c); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(idle/2 + time.Second)
	s, err := h.lookup(c)
	if err != nil {
		t.Fatalf("a used session expired on its first idle deadline: %v", err)
	}
	if !s.LastSeen.Equal(h.clock.Now()) {
		t.Errorf("LastSeen = %v, want %v", s.LastSeen, h.clock.Now())
	}
}

// End deletes the session before it answers and tells the browser to drop the
// cookie. A copy of the cookie is worthless afterwards.
func TestEnd(t *testing.T) {
	h := newHarness(t)
	c := h.start(t, h.user())
	w := httptest.NewRecorder()
	if err := h.m.End(context.Background(), w, request(http.MethodPost, c)); err != nil {
		t.Fatal(err)
	}
	_, err := h.lookup(c)
	assertNoSession(t, err)

	cleared := w.Result().Cookies()
	if len(cleared) != 1 || cleared[0].Name != CookieName || cleared[0].MaxAge >= 0 || cleared[0].Value != "" ||
		!cleared[0].Secure || cleared[0].Path != "/" {
		t.Errorf("End set %v, want the session cookie cleared", cleared)
	}

	// Ending without a session, or twice, is not an error: the user is signed out.
	if err := h.m.End(context.Background(), httptest.NewRecorder(), request(http.MethodPost, c)); err != nil {
		t.Errorf("second End: %v", err)
	}
	if err := h.m.End(context.Background(), httptest.NewRecorder(), request(http.MethodPost)); err != nil {
		t.Errorf("End without a cookie: %v", err)
	}
}

// End removes every session the request names. With two cookies it cannot know
// which one the user means, and signing out must not leave one behind.
func TestEndEndsEverySessionNamed(t *testing.T) {
	h := newHarness(t)
	a, b := h.start(t, h.user()), h.start(t, h.user())
	if err := h.m.End(context.Background(), httptest.NewRecorder(), request(http.MethodPost, a, b)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*http.Cookie{a, b} {
		_, err := h.lookup(c)
		assertNoSession(t, err)
	}
}

// Logout during a request: the request read the session, logout deleted it, and
// then the request recorded its activity. Logout wins.
func TestLogoutWinsOverATouch(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	racing := &racingStore{Store: NewMemory(c.Now)}
	h := newHarnessWith(t, c, racing)
	cookie := h.start(t, h.user())
	racing.beforeTouch = func() {
		if err := h.m.End(context.Background(), httptest.NewRecorder(), request(http.MethodPost, cookie)); err != nil {
			t.Error(err)
		}
	}
	_, err := h.lookup(cookie)
	assertNoSession(t, err)
	racing.beforeTouch = nil
	_, err = h.lookup(cookie)
	assertNoSession(t, err)
}

type racingStore struct {
	Store
	beforeTouch func()
}

func (r *racingStore) Touch(ctx context.Context, key Key, lastSeen, expires time.Time) error {
	if r.beforeTouch != nil {
		r.beforeTouch()
	}
	return r.Store.Touch(ctx, key, lastSeen, expires)
}

// When the store fails, nothing is decided from a stale or empty answer: the error
// is not "no session" (a 401 would send the user to log in again), and no session
// is returned.
func TestStoreFailureIsNotNoSession(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	failing := &failingStore{Store: NewMemory(c.Now)}
	h := newHarnessWith(t, c, failing)
	cookie := h.start(t, h.user())
	for _, op := range []string{"get", "touch"} {
		t.Run(op, func(t *testing.T) {
			failing.op = op
			_, err := h.lookup(cookie)
			if err == nil || errors.Is(err, ErrNoSession) {
				t.Fatalf("err = %v, want a store error", err)
			}
			s, err := h.m.Use(request(http.MethodGet, cookie))
			if err == nil || errors.Is(err, ErrNoSession) || s.IDToken != "" {
				t.Fatalf("Use = %+v, %v with the store down", s, err)
			}
			if strings.Contains(err.Error(), cookie.Value) {
				t.Errorf("the error names the session ID: %v", err)
			}
		})
	}
	failing.op = "delete"
	if err := h.m.End(context.Background(), httptest.NewRecorder(), request(http.MethodPost, cookie)); err == nil {
		t.Error("End reported success while the session stayed in the store")
	}
}

var errStoreDown = errors.New("store: connection refused")

type failingStore struct {
	Store
	op string
}

func (f *failingStore) Get(ctx context.Context, key Key) (Session, error) {
	if f.op == "get" {
		return Session{}, errStoreDown
	}
	return f.Store.Get(ctx, key)
}

func (f *failingStore) Touch(ctx context.Context, key Key, lastSeen, expires time.Time) error {
	if f.op == "touch" {
		return errStoreDown
	}
	return f.Store.Touch(ctx, key, lastSeen, expires)
}

func (f *failingStore) Delete(ctx context.Context, key Key) error {
	if f.op == "delete" {
		return errStoreDown
	}
	return f.Store.Delete(ctx, key)
}

func TestNewRejectsBadConfig(t *testing.T) {
	store := NewMemory(nil)
	good := Config{Store: store, Origin: origin, IdleTimeout: idle, AbsoluteTimeout: maxAge}
	for name, mutate := range map[string]func(*Config){
		"no store":           func(c *Config) { c.Store = nil },
		"no origin":          func(c *Config) { c.Origin = "" },
		"http origin":        func(c *Config) { c.Origin = "http://foyer.example.test" },
		"origin with a path": func(c *Config) { c.Origin = origin + "/app" },
		"origin with query":  func(c *Config) { c.Origin = origin + "?x" },
		"origin with user":   func(c *Config) { c.Origin = "https://u@foyer.example.test" },
		"no host":            func(c *Config) { c.Origin = "https://" },
		"zero idle":          func(c *Config) { c.IdleTimeout = 0 },
		"zero absolute":      func(c *Config) { c.AbsoluteTimeout = 0 },
		"idle past absolute": func(c *Config) { c.IdleTimeout = maxAge + time.Second },
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
