package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ConfigButler/krm-foyer/internal/gate"
	"github.com/ConfigButler/krm-foyer/internal/interruption"
	"github.com/ConfigButler/krm-foyer/internal/proxy"
	"github.com/ConfigButler/krm-foyer/internal/session"
)

// csrfKey is session.CSRFHeader as a key of a parsed request's header map.
var csrfKey = http.CanonicalHeaderKey(session.CSRFHeader)

// Through the proxy, wired as the binary wires it: Auth is the proxy's only
// credential source, a refused request never reaches the API server, and an allowed
// one carries the session's ID token.
func TestThroughTheProxy(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice, "/")
	_, s := b.session()

	var mu sync.Mutex
	var auths []string
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(api.Close)
	u, _ := url.Parse(api.URL)
	g, err := gate.New(gate.Config{Credentials: h.auth})
	if err != nil {
		t.Fatal(err)
	}
	p, err := proxy.New(proxy.Config{
		Server: u, Gate: g,
		RootCAs: api.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs,
	})
	if err != nil {
		t.Fatal(err)
	}
	send := func(method string, signedIn bool, header http.Header) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), method, h.foyer.URL+"/k8s/api/v1/namespaces/team-a/configmaps", nil)
		if signedIn {
			r = cookieRequest(b)
			r.Method = method
		}
		for k, v := range header {
			r.Header[k] = v
		}
		r.RequestURI = "/k8s/api/v1/namespaces/team-a/configmaps"
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w
	}
	reached := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(auths)
	}
	proof := http.Header{"Origin": {h.foyer.URL}, csrfKey: {s.CSRFToken}}

	for _, tc := range []struct {
		name     string
		method   string
		signedIn bool
		header   http.Header
		code     int
		reason   string
	}{
		{"read without a session", http.MethodGet, false, nil, http.StatusUnauthorized, "Unauthorized"},
		{"write without a session", http.MethodPost, false, proof, http.StatusUnauthorized, "Unauthorized"},
		{"write without proof", http.MethodPost, true, http.Header{"Origin": {h.foyer.URL}}, http.StatusForbidden, "CSRFProofRequired"},
		{"cross-site write", http.MethodPost, true, http.Header{"Origin": {"https://evil.example"}, csrfKey: {s.CSRFToken}},
			http.StatusForbidden, "CrossOriginRequest"},
	} {
		w := send(tc.method, tc.signedIn, tc.header)
		if w.Code != tc.code || !hasReason(w.Body.String(), tc.reason) {
			t.Errorf("%s: %d %s, want %d %s", tc.name, w.Code, w.Body, tc.code, tc.reason)
		}
		if got := w.Header().Values(interruption.Header); !slices.Equal(got, []string{tc.reason}) {
			t.Errorf("%s: %s = %q, want %q", tc.name, interruption.Header, got, tc.reason)
		}
	}
	if n := len(reached()); n != 0 {
		t.Fatalf("%d refused requests reached the API server", n)
	}

	if w := send(http.MethodGet, true, nil); w.Code != http.StatusOK {
		t.Errorf("signed-in read: %d", w.Code)
	}
	if w := send(http.MethodPost, true, proof); w.Code != http.StatusOK {
		t.Errorf("signed-in write with proof: %d", w.Code)
	}
	want := "Bearer " + h.issuer.idTokens[0]
	if got := reached(); !slices.Equal(got, []string{want, want}) {
		t.Errorf("the API server received Authorization %q", got)
	}
}

func hasReason(body, reason string) bool {
	return reason != "" && strings.Contains(body, `"reason":"`+reason+`"`)
}

// Every session error becomes one answer, however it is wrapped. A refusal is never
// RBAC's Forbidden, and an error the session manager does not give is a 503, logged,
// never a guess that the user is or is not signed in.
func TestRefusal(t *testing.T) {
	h := newHarness(t)
	unexpected := errors.New("something unexpected")
	for name, tc := range map[string]struct {
		err    error
		status int
		reason string
	}{
		"no session":           {session.ErrNoSession, http.StatusUnauthorized, "Unauthorized"},
		"wrapped no session":   {fmt.Errorf("expired: %w", session.ErrNoSession), http.StatusUnauthorized, "Unauthorized"},
		"cross origin":         {session.ErrCrossOrigin, http.StatusForbidden, "CrossOriginRequest"},
		"no CSRF proof":        {session.ErrNoCSRFProof, http.StatusForbidden, "CSRFProofRequired"},
		"wrapped CSRF refusal": {fmt.Errorf("checking: %w", session.ErrNoCSRFProof), http.StatusForbidden, "CSRFProofRequired"},
		"no session and proof": {errors.Join(session.ErrNoCSRFProof, session.ErrNoSession), http.StatusUnauthorized, "Unauthorized"},
		"unexpected":           {unexpected, http.StatusServiceUnavailable, "ServiceUnavailable"},
		"unexpected, joined":   {errors.Join(unexpected, errors.New("other")), http.StatusServiceUnavailable, "ServiceUnavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			i := h.auth.refusal(tc.err)
			if i.Status != tc.status || i.Reason != tc.reason {
				t.Errorf("%d %s, want %d %s", i.Status, i.Reason, tc.status, tc.reason)
			}
			if i.Reason == "Forbidden" {
				t.Error("a session refusal reads as an RBAC refusal")
			}
		})
	}
	if logs := h.logs.String(); !strings.Contains(logs, "something unexpected") {
		t.Errorf("an unexpected error was not logged:\n%s", logs)
	}
}

// Live follows the session the credential came from: true while it lasts, false
// after logout in this process or once it expires, and false when the check ran out
// of time. A credential a copy of the cookie gets after the logout is live again:
// logout ends what is open, and revokes nothing.
func TestLiveFollowsTheSession(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	b.login(alice, "/")
	_, s := b.session()
	cred, refused := h.auth.Token(cookieRequest(b))
	if refused != nil || cred.Token == "" || cred.Live == nil {
		t.Fatalf("Token = %+v, %v", cred, refused)
	}
	if !cred.Live(t.Context()) {
		t.Fatal("a live session's credential is not live")
	}
	// Log lines name the user as the issuer did.
	if cred.User != alice {
		t.Errorf("User = %q, want %q", cred.User, alice)
	}
	// The session's name for bounds is stable, its own, and not its ID.
	again, _ := h.auth.Token(cookieRequest(b))
	other := h.browser()
	other.login(alice, "/")
	theirs, _ := h.auth.Token(cookieRequest(other))
	if id := b.cookie(session.CookieName); cred.Session == "" || cred.Session != again.Session ||
		cred.Session == theirs.Session || strings.Contains(cred.Session, id) {
		t.Errorf("Session %q (again %q, another session %q) is not an opaque name of its own", cred.Session, again.Session, theirs.Session)
	}

	id := b.cookie(session.CookieName)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if cred.Live(ctx) {
		t.Error("live after the check ran out of time")
	}

	if resp := b.do(http.MethodPost, "/auth/logout", http.Header{"Origin": {h.foyer.URL}, session.CSRFHeader: {s.CSRFToken}}); resp.code != http.StatusNoContent {
		t.Fatalf("logout: %d", resp.code)
	}
	if cred.Live(t.Context()) {
		t.Error("live after logout")
	}
	if !theirs.Live(t.Context()) {
		t.Error("another session's credential ended with this logout")
	}

	copied := h.browser()
	copied.setSession(id)
	later, refused := h.auth.Token(cookieRequest(copied))
	if refused != nil || !later.Live(t.Context()) || later.Session != cred.Session {
		t.Fatalf("a copy of the cookie after logout: %+v, %v; this test records that it is still the session", later, refused)
	}
	h.clock.Advance(time.Hour)
	if later.Live(t.Context()) || theirs.Live(t.Context()) {
		t.Error("live past the session's expiry")
	}
}
