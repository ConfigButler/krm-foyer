// Package session keeps krm-foyer's sessions in the browser: the session, the user's
// token included, is sealed (AES-GCM) into an HttpOnly cookie with keys only krm-foyer
// holds, so a session survives a restart and krm-foyer stores none. It also decides
// which requests carry enough proof that the user's own page sent them (CSRF and
// same-origin checks), and ends the responses a logout leaves open in this process.
// docs/design.md, "Login and sessions", is the contract.
package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CookieName is the session cookie. The __Host- prefix makes browsers refuse it
// unless it is Secure, host-only and for path /, so no other host can set it.
const CookieName = "__Host-krm-foyer-session"

// MaxCookieValue is the longest session cookie krm-foyer sets, encoded. Browsers keep
// a cookie whose name and value together are at most 4,096 bytes and drop a longer one
// without a word; this leaves room for the name, and is what voter allows. A login
// whose session does not fit is refused: a token is never cut short, split across
// cookies or kept on the server instead.
const MaxCookieValue = 3800

// cookieVersion is the format of the cookie's value. A cookie of another version is
// no session.
const cookieVersion = 1

// ErrNoSession means the request has no live session: no cookie, more than one, or
// one that is not a session sealed with a configured key, has another version, or has
// expired.
var ErrNoSession = errors.New("no session")

// ErrTooLarge means a login's session does not fit in its cookie.
var ErrTooLarge = errors.New("the session does not fit in a cookie")

// Session is what krm-foyer knows about a signed-in user, all of it in their cookie.
type Session struct {
	// Issuer, Subject and Email identify the user as the issuer named them. They are
	// for display: the API server decides who the token belongs to.
	Issuer, Subject, Email string
	// IDToken is the credential sent to the API server. It leaves krm-foyer only
	// sealed in the cookie, which page scripts cannot read and the browser cannot open.
	IDToken string
	// TokenExpiry is when the API server stops accepting IDToken. The session ends
	// then too: there is no refresh yet. Start reads it; the cookie keeps only Expires.
	TokenExpiry time.Time
	// ID names the session, CSRFToken is the proof a mutation must carry, and Issued
	// and Expires are when it began and when it ends, however busy it is. Start sets
	// them, and nothing changes them after.
	ID, CSRFToken   string
	Issued, Expires time.Time
}

// Handle names s for the bounds kept per session: an opaque string, the same for every
// request of that session, wherever its cookie was sealed, and for no other. It is
// derived from the session's ID, and is neither the ID nor the cookie.
func (s Session) Handle() string {
	h := sha256.Sum256([]byte("krm-foyer session handle\x00" + s.ID))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// Config is what a Manager needs.
type Config struct {
	// Keys seal and open session cookies.
	Keys *Keys
	// Origin is krm-foyer's public origin, such as https://foyer.example.com. A
	// mutation must come from it. It is configuration, never a request's Host.
	Origin string
	// AbsoluteTimeout ends any session this long after login. Lowering it ends
	// sessions already issued sooner too.
	AbsoluteTimeout time.Duration
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Manager issues, opens and ends sessions.
type Manager struct {
	keys     *Keys
	origin   string
	absolute time.Duration
	now      func() time.Time
	open     openResponses
}

// New returns a Manager for cfg.
func New(cfg Config) (*Manager, error) {
	if cfg.Keys == nil || len(cfg.Keys.keys) == 0 {
		return nil, errors.New("no session keys")
	}
	origin, err := serializeOrigin(cfg.Origin)
	if err != nil {
		return nil, err
	}
	if cfg.AbsoluteTimeout <= 0 {
		return nil, fmt.Errorf("the session timeout must be positive, got %v", cfg.AbsoluteTimeout)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Manager{keys: cfg.Keys, origin: origin, absolute: cfg.AbsoluteTimeout, now: now}, nil
}

// serializeOrigin turns the configured public URL into the string a browser sends
// as Origin: lower-case scheme and host, the port only when it is not the default.
func serializeOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("public origin must be an https URL with no path, query or user info, got %q", raw)
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && port != "443" {
		host += ":" + port
	}
	return "https://" + host, nil
}

// Start begins a session for s after a login, and sets its cookie on w. It always
// issues a new ID and CSRF token, so a cookie the browser held before login, its own
// or one planted in it, never becomes the signed-in one; the responses that cookie's
// session has open in this process are ended. It answers ErrTooLarge, and sets no
// cookie, when the session does not fit in one.
func (m *Manager) Start(w http.ResponseWriter, r *http.Request, s Session) error {
	now := m.now().Truncate(time.Second)
	if s.IDToken == "" || !now.Before(s.TokenExpiry) {
		return errors.New("login produced no token the API server would accept")
	}
	s.ID, s.CSRFToken = randomString(), randomString()
	s.Issued = now
	s.Expires = now.Add(m.absolute)
	if expiry := s.TokenExpiry.Truncate(time.Second); expiry.Before(s.Expires) {
		s.Expires = expiry
	}
	if !now.Before(s.Expires) {
		return errors.New("login produced a token that expires within the second")
	}
	value, err := m.seal(s)
	if err != nil {
		return err
	}
	m.endOpen(r)
	http.SetCookie(w, cookie(value, int(s.Expires.Sub(now)/time.Second)))
	return nil
}

// Lookup returns the session r belongs to. It answers ErrNoSession unless r carries
// exactly one cookie holding a live session.
func (m *Manager) Lookup(r *http.Request) (Session, error) {
	// Exactly one cookie. Two may come from a browser that kept an old one beside a
	// new one, or from something that planted one; choosing between them is a guess.
	cookies := r.CookiesNamed(CookieName)
	if len(cookies) != 1 {
		return Session{}, ErrNoSession
	}
	s, ok := m.unseal(cookies[0].Value)
	if !ok || !m.now().Before(s.Expires) {
		return Session{}, ErrNoSession
	}
	return s, nil
}

// Use returns the session r belongs to if r may act on it: Lookup, then
// CheckMutation.
func (m *Manager) Use(r *http.Request) (Session, error) {
	s, err := m.Lookup(r)
	if err != nil {
		return Session{}, err
	}
	if err := m.CheckMutation(r, s); err != nil {
		return Session{}, err
	}
	return s, nil
}

// End clears the browser's session cookie, and ends the responses that r's session
// has open in this process. It revokes nothing: a copy of the cookie stays a session
// until it expires, here or on any process with its key.
func (m *Manager) End(w http.ResponseWriter, r *http.Request) {
	m.endOpen(r)
	http.SetCookie(w, cookie("", -1))
}

// Watch registers a response to s as open until ctx ends, and returns whether s may
// still be used for it: false once s has expired, or once s has been ended in this
// process since, by a logout or a login in the same browser. An open response asks
// it, and is cut short when it says no.
func (m *Manager) Watch(ctx context.Context, s Session) (live func() bool) {
	ended := m.open.add(ctx, s.Handle())
	return func() bool { return !ended.Load() && m.now().Before(s.Expires) }
}

// endOpen ends the open responses of every session r's cookies hold.
func (m *Manager) endOpen(r *http.Request) {
	for _, c := range r.CookiesNamed(CookieName) {
		if s, ok := m.unseal(c.Value); ok {
			m.open.end(s.Handle())
		}
	}
}

// payload is a session as its cookie holds it. Times are Unix seconds.
type payload struct {
	Version   int    `json:"v"`
	ID        string `json:"id"`
	CSRFToken string `json:"csrf"`
	Issued    int64  `json:"iat"`
	Expires   int64  `json:"exp"`
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	Email     string `json:"email,omitempty"`
	IDToken   string `json:"idt"`
}

// seal turns s into a cookie value: its version, the ID of the key that sealed it, a
// random nonce, and the session, encrypted and authenticated together with the
// version, the key's ID and the cookie's name.
func (m *Manager) seal(s Session) (string, error) {
	plain, err := json.Marshal(payload{
		Version: cookieVersion, ID: s.ID, CSRFToken: s.CSRFToken,
		Issued: s.Issued.Unix(), Expires: s.Expires.Unix(),
		Issuer: s.Issuer, Subject: s.Subject, Email: s.Email, IDToken: s.IDToken,
	})
	if err != nil {
		return "", err
	}
	k := m.keys.sealing()
	header := append([]byte{cookieVersion}, k.id[:]...)
	nonce := make([]byte, k.aead.NonceSize())
	_, _ = rand.Read(nonce)
	sealed := k.aead.Seal(append(header, nonce...), nonce, plain, additionalData(header))
	value := base64.RawURLEncoding.EncodeToString(sealed)
	if len(value) > MaxCookieValue {
		return "", fmt.Errorf("%w: %d bytes, at most %d", ErrTooLarge, len(value), MaxCookieValue)
	}
	return value, nil
}

// unseal returns the session a cookie value holds, if a configured key sealed it, in
// this version, for this cookie. It does not check expiry.
func (m *Manager) unseal(value string) (Session, bool) {
	if len(value) > MaxCookieValue {
		return Session{}, false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(raw) < 1+keyIDSize || raw[0] != cookieVersion {
		return Session{}, false
	}
	header := raw[:1+keyIDSize]
	k, ok := m.keys.byID(header[1:])
	if !ok || len(raw) < len(header)+k.aead.NonceSize() {
		return Session{}, false
	}
	nonce := raw[len(header) : len(header)+k.aead.NonceSize()]
	plain, err := k.aead.Open(nil, nonce, raw[len(header)+len(nonce):], additionalData(header))
	if err != nil {
		return Session{}, false
	}
	var p payload
	dec := json.NewDecoder(bytes.NewReader(plain))
	dec.DisallowUnknownFields()
	if dec.Decode(&p) != nil || dec.More() || p.Version != cookieVersion ||
		len(p.ID) != idLength || len(p.CSRFToken) != idLength || p.IDToken == "" || p.Expires <= p.Issued {
		return Session{}, false
	}
	s := Session{
		Issuer: p.Issuer, Subject: p.Subject, Email: p.Email, IDToken: p.IDToken,
		ID: p.ID, CSRFToken: p.CSRFToken, Issued: time.Unix(p.Issued, 0), Expires: time.Unix(p.Expires, 0),
	}
	// A lower timeout than the one the cookie was sealed under applies to it too.
	if end := s.Issued.Add(m.absolute); end.Before(s.Expires) {
		s.Expires = end
	}
	return s, true
}

// additionalData binds a cookie's ciphertext to what it is: a krm-foyer session
// cookie of this name, version and key.
func additionalData(header []byte) []byte {
	return append([]byte("krm-foyer session cookie\x00"+CookieName+"\x00"), header...)
}

func cookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: CookieName, Value: value, Path: "/", MaxAge: maxAge,
		Secure: true, HttpOnly: true,
		// Lax, not Strict: the browser arrives from the issuer by a cross-site
		// navigation, and a link to a /k8s URL from elsewhere should open signed in.
		// Every request that changes state needs CSRF proof regardless.
		SameSite: http.SameSiteLaxMode,
	}
}

// idLength is the length of a session ID or CSRF token: 32 random bytes in
// unpadded base64url.
var idLength = base64.RawURLEncoding.EncodedLen(32)

func randomString() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails; it panics if the system has no randomness
	return base64.RawURLEncoding.EncodeToString(b)
}

// openResponses are the responses open in this process, by the handle of their
// session, so that a logout can end them. It holds nothing once they have ended: it
// is not a list of sessions, nor of revoked ones.
type openResponses struct {
	mu   sync.Mutex
	open map[string]map[*atomic.Bool]struct{}
}

// add registers a response of the session handle names until ctx ends, and returns
// the flag end sets.
func (o *openResponses) add(ctx context.Context, handle string) *atomic.Bool {
	ended := new(atomic.Bool)
	o.mu.Lock()
	if o.open == nil {
		o.open = map[string]map[*atomic.Bool]struct{}{}
	}
	if o.open[handle] == nil {
		o.open[handle] = map[*atomic.Bool]struct{}{}
	}
	o.open[handle][ended] = struct{}{}
	o.mu.Unlock()
	context.AfterFunc(ctx, func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		if set := o.open[handle]; set != nil {
			delete(set, ended)
			if len(set) == 0 {
				delete(o.open, handle)
			}
		}
	})
	return ended
}

// end ends every response registered for handle. Responses registered later, by a
// copy of the same cookie, are not affected.
func (o *openResponses) end(handle string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for ended := range o.open[handle] {
		ended.Store(true)
	}
	delete(o.open, handle)
}

// count is how many responses are registered, for tests.
func (o *openResponses) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, set := range o.open {
		n += len(set)
	}
	return n
}
