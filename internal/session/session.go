// Package session keeps krm-foyer's server-side sessions: the browser holds an opaque
// ID in a cookie, and everything else, the user's token included, stays here. It also
// decides which requests carry enough proof that the user's own page sent them (CSRF
// and same-origin checks). docs/design.md, "Login and sessions", is the contract.
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ConfigButler/krm-foyer/internal/proxy"
)

// CookieName is the session cookie. The __Host- prefix makes browsers refuse it
// unless it is Secure, host-only and for path /, so no other host can set it.
const CookieName = "__Host-krm-foyer-session"

// ErrNoSession means the request has no live session. It is a proxy.ErrNoCredential,
// which the proxy answers with 401.
var ErrNoSession = fmt.Errorf("no session: %w", proxy.ErrNoCredential)

// Session is what krm-foyer knows about a signed-in user.
type Session struct {
	// Issuer, Subject and Email identify the user as the issuer named them. They are
	// for display: the API server decides who the token belongs to.
	Issuer, Subject, Email string
	// IDToken is the credential sent to the API server. It never leaves krm-foyer.
	IDToken string
	// TokenExpiry is when the API server stops accepting IDToken. The session ends
	// then too: there is no refresh yet.
	TokenExpiry time.Time
	// CSRFToken is the proof a mutation must carry. Start sets it.
	CSRFToken string
	// Created and LastSeen drive absolute and idle expiry. Start sets them.
	Created, LastSeen time.Time
}

// Key is how a store knows a session: the SHA-256 of its ID, so reading the store
// does not yield IDs a browser could use.
type Key [32]byte

// ErrNotFound is what a Store answers for a key it does not hold.
var ErrNotFound = errors.New("session not found")

// Store keeps sessions. Expiry is the Manager's decision; a store may forget a
// session after the time it was given, and must not return one past it.
type Store interface {
	// Create stores s under key until expires. It fails if key is taken.
	Create(ctx context.Context, key Key, s Session, expires time.Time) error
	// Get returns the session under key, or ErrNotFound.
	Get(ctx context.Context, key Key) (Session, error)
	// Touch records activity on a session that still exists, or answers
	// ErrNotFound. It never creates one: a session deleted by a logout stays deleted.
	Touch(ctx context.Context, key Key, lastSeen, expires time.Time) error
	// Delete removes the session under key. Deleting a missing one is not an error.
	Delete(ctx context.Context, key Key) error
}

// Config is what a Manager needs.
type Config struct {
	Store Store
	// Origin is krm-foyer's public origin, such as https://foyer.example.com. A
	// mutation must come from it. It is configuration, never a request's Host.
	Origin string
	// IdleTimeout ends a session not used for this long; AbsoluteTimeout ends any
	// session this long after login.
	IdleTimeout, AbsoluteTimeout time.Duration
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Manager issues, finds and ends sessions.
type Manager struct {
	store          Store
	origin         string
	idle, absolute time.Duration
	now            func() time.Time
}

// New returns a Manager for cfg.
func New(cfg Config) (*Manager, error) {
	if cfg.Store == nil {
		return nil, errors.New("no session store")
	}
	origin, err := serializeOrigin(cfg.Origin)
	if err != nil {
		return nil, err
	}
	if cfg.IdleTimeout <= 0 || cfg.AbsoluteTimeout <= 0 || cfg.IdleTimeout > cfg.AbsoluteTimeout {
		return nil, fmt.Errorf("session timeouts must be positive with idle at most absolute, got idle %v and absolute %v",
			cfg.IdleTimeout, cfg.AbsoluteTimeout)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Manager{store: cfg.Store, origin: origin, idle: cfg.IdleTimeout, absolute: cfg.AbsoluteTimeout, now: now}, nil
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
// issues a new ID and ends any session r's cookies name: an ID the browser held
// before login, its own or one planted in it, never becomes the signed-in one.
func (m *Manager) Start(ctx context.Context, w http.ResponseWriter, r *http.Request, s Session) error {
	now := m.now()
	if s.IDToken == "" || !now.Before(s.TokenExpiry) {
		return errors.New("login produced no token the API server would accept")
	}
	for _, c := range r.CookiesNamed(CookieName) {
		if key, ok := keyOf(c.Value); ok {
			if err := m.store.Delete(ctx, key); err != nil {
				return fmt.Errorf("ending the previous session: %w", err)
			}
		}
	}

	id := randomString()
	key, _ := keyOf(id)
	s.CSRFToken = randomString()
	s.Created, s.LastSeen = now, now
	if err := m.store.Create(ctx, key, s, m.deadline(s)); err != nil {
		return fmt.Errorf("storing the session: %w", err)
	}
	http.SetCookie(w, m.cookie(id, int(m.end(s).Sub(now)/time.Second)))
	return nil
}

// Lookup returns the session r belongs to, and records the activity. It answers
// ErrNoSession unless r carries exactly one cookie naming a live session; any other
// error means the store could not say, and nothing may be decided from it.
func (m *Manager) Lookup(r *http.Request) (Session, error) {
	key, s, err := m.find(r)
	if err != nil {
		return Session{}, err
	}
	return m.touch(r.Context(), key, s)
}

// End deletes every session r's cookies name and clears the cookie. The session is
// gone from the store before End returns, so the answer to a logout is sent after it.
func (m *Manager) End(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	for _, c := range r.CookiesNamed(CookieName) {
		if key, ok := keyOf(c.Value); ok {
			if err := m.store.Delete(ctx, key); err != nil {
				return fmt.Errorf("deleting the session: %w", err)
			}
		}
	}
	http.SetCookie(w, m.cookie("", -1))
	return nil
}

// Token implements proxy.Credentials: the session's ID token, for a request that
// may use it. A mutation without CSRF proof is refused with an *proxy.Interruption
// before it counts as activity.
func (m *Manager) Token(r *http.Request) (string, error) {
	key, s, err := m.find(r)
	if err != nil {
		return "", err
	}
	if err := m.CheckMutation(r, s); err != nil {
		return "", err
	}
	s, err = m.touch(r.Context(), key, s)
	if err != nil {
		return "", err
	}
	return s.IDToken, nil
}

// find returns the live session named by r's one session cookie.
func (m *Manager) find(r *http.Request) (Key, Session, error) {
	// Exactly one cookie. Two may come from a browser that kept an old one beside a
	// new one, or from something that planted one; choosing between them is a guess.
	cookies := r.CookiesNamed(CookieName)
	if len(cookies) != 1 {
		return Key{}, Session{}, ErrNoSession
	}
	key, ok := keyOf(cookies[0].Value)
	if !ok {
		return Key{}, Session{}, ErrNoSession
	}
	s, err := m.store.Get(r.Context(), key)
	if errors.Is(err, ErrNotFound) {
		return Key{}, Session{}, ErrNoSession
	}
	if err != nil {
		return Key{}, Session{}, fmt.Errorf("reading the session: %w", err)
	}
	if !m.now().Before(m.deadline(s)) {
		if err := m.store.Delete(r.Context(), key); err != nil {
			return Key{}, Session{}, fmt.Errorf("deleting an expired session: %w", err)
		}
		return Key{}, Session{}, ErrNoSession
	}
	return key, s, nil
}

// touch records activity. A session deleted since find, by a logout, stays deleted.
func (m *Manager) touch(ctx context.Context, key Key, s Session) (Session, error) {
	s.LastSeen = m.now()
	err := m.store.Touch(ctx, key, s.LastSeen, m.deadline(s))
	if errors.Is(err, ErrNotFound) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, fmt.Errorf("recording session activity: %w", err)
	}
	return s, nil
}

// end is when s ends however busy it is: its absolute deadline or its token's
// expiry, whichever comes first.
func (m *Manager) end(s Session) time.Time {
	end := s.Created.Add(m.absolute)
	if s.TokenExpiry.Before(end) {
		end = s.TokenExpiry
	}
	return end
}

// deadline is when s ends if it is not used again.
func (m *Manager) deadline(s Session) time.Time {
	end := m.end(s)
	if idle := s.LastSeen.Add(m.idle); idle.Before(end) {
		end = idle
	}
	return end
}

func (m *Manager) cookie(value string, maxAge int) *http.Cookie {
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

// keyOf returns the store key for a session ID, if value is shaped like one.
func keyOf(value string) (Key, bool) {
	if len(value) != idLength {
		return Key{}, false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return Key{}, false
	}
	return sha256.Sum256(raw), true
}
