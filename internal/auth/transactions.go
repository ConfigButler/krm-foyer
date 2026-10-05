package auth

import (
	"cmp"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// transactionCookiePrefix names the cookies that bind logins in progress to the
// browser that started them, one cookie per login. The rest of the name is a hash of
// the login's state, so a callback finds its own login and leaves any other in
// progress in the same browser alone. Like the session cookie, they are host-only,
// Secure and HttpOnly. SameSite=Lax lets them ride along on the issuer's redirect
// back, a cross-site top-level GET.
const transactionCookiePrefix = "__Host-krm-foyer-login-"

// transactionLifetime is how long a login may take at the issuer.
const transactionLifetime = 10 * time.Minute

// maxBrowserTransactions bounds the logins one browser may have in progress. Each is
// a cookie sent with every request to the origin, so a page that keeps starting
// logins would otherwise grow the Cookie header without end.
const maxBrowserTransactions = 3

// transaction is a login in progress: what the callback must match, and where to
// go afterwards.
type transaction struct {
	State    string    `json:"s"`
	Nonce    string    `json:"n"`
	Verifier string    `json:"v"`
	ReturnTo string    `json:"r"`
	Expires  time.Time `json:"e"`
	// Params are the values the login request gave for the issuer's parameters, so a
	// failed login can offer the same choice again.
	Params map[string]string `json:"p,omitempty"`
}

// transactions keeps logins in progress in the browser that started them, sealed
// with a key that lives only in this process: the browser can neither read nor
// change them, and krm-foyer holds nothing until the callback. So anyone may start
// as many logins as they like without crowding out anyone else's.
type transactions struct {
	now  func() time.Time
	aead cipher.AEAD
}

func newTransactions(now func() time.Time) *transactions {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err) // cannot happen: the key is 32 bytes
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err) // cannot happen: AES has a 16-byte block
	}
	return &transactions{now: now, aead: aead}
}

func random() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails; it panics if the system has no randomness
	return base64.RawURLEncoding.EncodeToString(b)
}

// transactionCookieName is the name of the cookie that holds the login with this
// state.
func transactionCookieName(state string) string {
	sum := sha256.Sum256([]byte(state))
	return transactionCookiePrefix + base64.RawURLEncoding.EncodeToString(sum[:12])
}

// maxTransactionCookie bounds a login cookie's name and value together. Browsers keep
// a cookie of at most 4,096 bytes there and drop a longer one silently, which would
// fail the login at its callback; this leaves room for the attributes too.
const maxTransactionCookie = 4000

// errTransactionTooLarge means a login's cookie would be too large to keep.
var errTransactionTooLarge = errors.New("the login does not fit in its cookie")

// begin starts a transaction and returns the cookie that holds it, or
// errTransactionTooLarge when that cookie, as the browser would hold it, is too large:
// a long return path with long parameters, after JSON escaping, sealing and base64.
func (ts *transactions) begin(returnTo string, params map[string]string) (*http.Cookie, transaction, error) {
	t := transaction{
		State: random(), Nonce: random(), Verifier: oauth2.GenerateVerifier(),
		ReturnTo: returnTo, Expires: ts.now().Add(transactionLifetime), Params: params,
	}
	plain, err := json.Marshal(t)
	if err != nil {
		panic(err) // cannot happen: strings, a map of strings and a time
	}
	name := transactionCookieName(t.State)
	nonce := make([]byte, ts.aead.NonceSize())
	_, _ = rand.Read(nonce)
	// The name is sealed in too: a cookie cannot be moved to another login's name.
	sealed := ts.aead.Seal(nonce, nonce, plain, []byte(name))
	value := base64.RawURLEncoding.EncodeToString(sealed)
	if len(name)+1+len(value) > maxTransactionCookie {
		return nil, transaction{}, errTransactionTooLarge
	}
	return transactionCookie(name, value, int(transactionLifetime/time.Second)), t, nil
}

// open returns the live transaction the cookie called name holds, or false.
func (ts *transactions) open(name, value string) (transaction, bool) {
	sealed, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(sealed) < ts.aead.NonceSize() {
		return transaction{}, false
	}
	n := ts.aead.NonceSize()
	plain, err := ts.aead.Open(nil, sealed[:n], sealed[n:], []byte(name))
	if err != nil {
		return transaction{}, false
	}
	var t transaction
	if json.Unmarshal(plain, &t) != nil || !ts.now().Before(t.Expires) {
		return transaction{}, false
	}
	return t, true
}

// excess names the login cookies r carries that a new login should end: any that
// holds no live login, and the oldest beyond maxBrowserTransactions-1 others, so that
// with the new one the browser has at most maxBrowserTransactions.
func (ts *transactions) excess(r *http.Request) []string {
	type live struct {
		name    string
		expires time.Time
	}
	var keep []live
	var end []string
	for _, c := range r.Cookies() {
		if !strings.HasPrefix(c.Name, transactionCookiePrefix) {
			continue
		}
		if t, ok := ts.open(c.Name, c.Value); ok {
			keep = append(keep, live{c.Name, t.Expires})
		} else {
			end = append(end, c.Name)
		}
	}
	slices.SortFunc(keep, func(a, b live) int { return b.expires.Compare(a.expires) })
	for i, l := range keep {
		if i >= maxBrowserTransactions-1 {
			end = append(end, l.name)
		}
	}
	slices.SortFunc(end, cmp.Compare)
	return slices.Compact(end)
}

func transactionCookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: maxAge,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	}
}
