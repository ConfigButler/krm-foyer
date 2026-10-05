package stream

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/ConfigButler/krm-foyer/internal/gate"
	"github.com/ConfigButler/krm-foyer/internal/interruption"
)

// whoami is /auth/whoami's answer: who Kubernetes takes the user to be, and the
// session's issuer and end. The groups here are the API server's mapping, which
// differ from the issuer's groups /auth/session shows. It never holds a token.
type whoami struct {
	UserInfo  authenticationv1.UserInfo `json:"userInfo"`
	Issuer    string                    `json:"issuer"`
	ExpiresAt time.Time                 `json:"expiresAt"`
}

// WhoAmI serves GET /auth/whoami: a fresh SelfSubjectReview, sent with the user's own
// token through the gate, so its bounds hold, and answered exactly as the API server
// answered. It lives here because the stream's shared watches ask the same question
// the same way (selfSubjectReview), and there is one implementation of it. Nothing is
// inferred from the token: a refusal is passed on, a failure is a 503, and there is no
// other credential to try.
func (s *Streams) WhoAmI() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info, cred, ok := s.identify(w, r, nil)
		if ok {
			writeWhoami(w, cred, info)
		}
	})
}

// Identify is /auth/check's question, asked as /auth/whoami asks it, through the gate
// with the user's own token, and answered on failure as /auth/whoami answers. One
// difference: an answer is reused for IdentityTTL, as a domain backend may check on
// every request.
func (s *Streams) Identify(w http.ResponseWriter, r *http.Request) (authenticationv1.UserInfo, bool) {
	info, _, ok := s.identify(w, r, s.identities)
	return info, ok
}

// identify asks the API server who r's user is, or answers r itself and returns false.
// With reuse, a recent answer for the same token stands in for the review.
func (s *Streams) identify(w http.ResponseWriter, r *http.Request, reuse *identities) (authenticationv1.UserInfo, gate.Credential, bool) {
	a := s.gate.Admit(w, r)
	if a == nil {
		return authenticationv1.UserInfo{}, gate.Credential{}, false
	}
	defer a.Close()
	if info, ok := reuse.get(a.Token); ok {
		return info, a.Credential, true
	}
	u := &upstream{streams: s, userAgent: r.UserAgent()}
	info, err := u.selfSubjectReview(a.Request.Context(), a.Credential)
	var api apierrors.APIStatus
	switch {
	case err == nil && info.Username != "":
		reuse.put(a.Token, info, a.Expires)
		return info, a.Credential, true
	case err == nil:
		s.logger.Warn("the API server named no user", "route", route(r))
		a.Interrupt(w, unknownIdentity)
	case errors.As(err, &api) && api.Status().Code >= 400 && api.Status().Code < 500:
		// The API server's own refusal, as it gave it: a token it does not accept
		// is its 401, not krm-foyer's.
		a.Refused(gate.ByKubernetes, "status", api.Status().Code, "reason", string(api.Status().Reason))
		st := api.Status()
		body, _ := json.Marshal(&st)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(int(st.Code))
		_, _ = w.Write(body)
	default:
		s.logger.Warn("could not find who the user is", "route", route(r), "cause", class(err))
		a.Interrupt(w, unknownIdentity)
	}
	return authenticationv1.UserInfo{}, gate.Credential{}, false
}

// route names the route r arrived on, for a log line.
func route(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/auth/check") {
		return "/auth/check"
	}
	return "/auth/whoami"
}

// IdentityTTL is how long /auth/check reuses the API server's answer to who a token
// belongs to. The answer depends only on the token and the cluster's authentication
// configuration, so this is how long a change to that configuration may take to reach
// a domain backend; access decisions are not reused here, and are never krm-foyer's.
const IdentityTTL = 30 * time.Second

// maxIdentities bounds the answers kept: one per signed-in token, beyond which a check
// asks the API server again rather than keep more.
const maxIdentities = 10000

// identities reuses SelfSubjectReview answers by token, for at most IdentityTTL and
// never past the session's end. The key is the token's SHA-256, so no token is kept.
// A nil *identities reuses nothing.
type identities struct {
	ttl time.Duration
	max int
	now func() time.Time

	mu      sync.Mutex
	entries map[[sha256.Size]byte]knownIdentity
}

type knownIdentity struct {
	info  authenticationv1.UserInfo
	until time.Time
}

func newIdentities(now func() time.Time) *identities {
	if now == nil {
		now = time.Now
	}
	return &identities{ttl: IdentityTTL, max: maxIdentities, now: now, entries: map[[sha256.Size]byte]knownIdentity{}}
}

func (c *identities) get(token string) (authenticationv1.UserInfo, bool) {
	if c == nil {
		return authenticationv1.UserInfo{}, false
	}
	key := sha256.Sum256([]byte(token))
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || !c.now().Before(e.until) {
		delete(c.entries, key)
		return authenticationv1.UserInfo{}, false
	}
	return *e.info.DeepCopy(), true
}

func (c *identities) put(token string, info authenticationv1.UserInfo, sessionEnd time.Time) {
	if c == nil {
		return
	}
	now := c.now()
	until := now.Add(c.ttl)
	if sessionEnd.Before(until) {
		until = sessionEnd
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		for k, e := range c.entries {
			if !now.Before(e.until) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= c.max {
			return
		}
	}
	c.entries[sha256.Sum256([]byte(token))] = knownIdentity{info: *info.DeepCopy(), until: until}
}

// unknownIdentity is the answer when the API server could not say who the user is.
var unknownIdentity = &interruption.Interruption{
	Status: http.StatusServiceUnavailable, Reason: "ServiceUnavailable",
	Message: "the API server could not say who you are; try again later",
}

func writeWhoami(w http.ResponseWriter, cred gate.Credential, info authenticationv1.UserInfo) {
	body, err := json.Marshal(whoami{UserInfo: info, Issuer: cred.Issuer, ExpiresAt: cred.Expires})
	if err != nil {
		panic(err) // strings, lists and a time: cannot fail
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
