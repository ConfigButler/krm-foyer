package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// transactionCookieName binds a login in progress to the browser that started it.
// Like the session cookie, it is host-only, Secure and HttpOnly. SameSite=Lax lets
// it ride along on the issuer's redirect back, a cross-site top-level GET.
const transactionCookieName = "__Host-krm-foyer-login"

// transactionLifetime is how long a login may take at the issuer.
const transactionLifetime = 10 * time.Minute

// maxTransactions bounds logins in progress. Anyone can start one, so without a
// bound /auth/login would hold memory for every request.
const maxTransactions = 10000

// transaction is a login in progress: what the callback must match, and where to
// go afterwards. The browser holds only an opaque ID for it.
type transaction struct {
	state, nonce, verifier, returnTo string
	expires                          time.Time
}

// transactions keeps logins in progress, keyed by the SHA-256 of their ID. Each
// can be taken once.
type transactions struct {
	now func() time.Time
	mu  sync.Mutex
	m   map[[32]byte]transaction
}

func newTransactions(now func() time.Time) *transactions {
	return &transactions{now: now, m: map[[32]byte]transaction{}}
}

func random() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails; it panics if the system has no randomness
	return base64.RawURLEncoding.EncodeToString(b)
}

// begin starts a transaction and returns its ID, or false when too many are open.
func (ts *transactions) begin(returnTo string) (string, transaction, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	now := ts.now()
	if len(ts.m) >= maxTransactions {
		for k, t := range ts.m {
			if !now.Before(t.expires) {
				delete(ts.m, k)
			}
		}
		if len(ts.m) >= maxTransactions {
			return "", transaction{}, false
		}
	}
	id := random()
	t := transaction{
		state: random(), nonce: random(), verifier: oauth2.GenerateVerifier(),
		returnTo: returnTo, expires: now.Add(transactionLifetime),
	}
	ts.m[sha256.Sum256([]byte(id))] = t
	return id, t, true
}

// take removes and returns the live transaction with this ID.
func (ts *transactions) take(id string) (transaction, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	key := sha256.Sum256([]byte(id))
	t, ok := ts.m[key]
	delete(ts.m, key)
	if !ok || !ts.now().Before(t.expires) {
		return transaction{}, false
	}
	return t, true
}

func transactionCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: transactionCookieName, Value: value, Path: "/", MaxAge: maxAge,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	}
}
