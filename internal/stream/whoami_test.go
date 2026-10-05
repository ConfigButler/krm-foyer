package stream

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ConfigButler/krm-foyer/internal/gate"
	"github.com/ConfigButler/krm-foyer/internal/interruption"
)

// whoamiFoyer serves /auth/whoami from api behind a gate with creds.
func whoamiFoyer(t *testing.T, api *apiServer, creds gate.Credentials) *httptest.Server {
	t.Helper()
	g, err := gate.New(gate.Config{Credentials: creds})
	if err != nil {
		t.Fatal(err)
	}
	server, _ := url.Parse(api.URL)
	roots := x509.NewCertPool()
	roots.AddCert(api.Certificate())
	s, err := New(Config{Server: server, RootCAs: roots, Gate: g})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(s.WhoAmI())
	t.Cleanup(front.Close)
	return front
}

// expiring is a session's credential, with its issuer and end.
type expiring struct{ credentials }

var sessionEnd = time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC)

func (c expiring) Token(r *http.Request) (gate.Credential, *interruption.Interruption) {
	cred, refused := c.credentials.Token(r)
	cred.Issuer, cred.Expires = "https://dex.example.test", sessionEnd
	return cred, refused
}

func getWhoami(t *testing.T, front *httptest.Server, header http.Header) (int, http.Header, string) {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, front.URL+"/auth/whoami", nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		r.Header[k] = v
	}
	resp, err := front.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(body)
}

// /auth/whoami is the API server's own answer to a SelfSubjectReview sent with the
// user's token: username, UID, groups and extra, with the session's issuer and end.
// Only that request reaches the API server, with that token alone; whatever identity
// headers the browser sent go nowhere.
func TestWhoAmI(t *testing.T) {
	api := newSharedAPI(t)
	front := whoamiFoyer(t, api.apiServer, expiring{credentials{token: userToken}})
	code, header, body := getWhoami(t, front, http.Header{
		"Impersonate-User":  {"admin"},
		"Impersonate-Group": {"system:masters"},
		"Impersonate-Extra-Configbutler.ai%2fclaims%2femail": {"mallory@example.com"},
		"X-Remote-User": {"admin"},
		"X-Remote-Extra-Configbutler.ai%2fclaims%2femail": {"mallory@example.com"},
		"Authorization": {"Bearer " + otherToken},
	})
	if code != http.StatusOK || header.Get("Cache-Control") != "no-store" || header.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %v %s", code, header, body)
	}
	var got whoami
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.UserInfo.Username != "oidc:alice@example.com" || got.UserInfo.UID != "uid-oidc:alice@example.com" ||
		strings.Join(got.UserInfo.Groups, ",") != "system:authenticated,readers" ||
		strings.Join(got.UserInfo.Extra["scopes"], ",") != "openid" ||
		got.Issuer != "https://dex.example.test" || !got.ExpiresAt.Equal(sessionEnd) {
		t.Errorf("whoami = %+v", got)
	}
	if strings.Contains(body, userToken) || strings.Contains(body, "mallory") {
		t.Errorf("the answer holds a token or a header the browser sent: %s", body)
	}
	reqs := api.received()
	if len(reqs) != 1 || reqs[0].Method != http.MethodPost || reqs[0].URL.Path != "/apis/authentication.k8s.io/v1/selfsubjectreviews" ||
		reqs[0].Header.Get("Authorization") != "Bearer "+userToken {
		t.Fatalf("the API server received %d requests: %v", len(reqs), reqs)
	}
	for k := range reqs[0].Header {
		if lower := strings.ToLower(k); strings.HasPrefix(lower, "impersonate-") || strings.HasPrefix(lower, "x-remote-") {
			t.Errorf("the browser's %s header reached the API server", k)
		}
	}
}

// No session is the 401 interruption, and nothing reaches the API server. A refusal
// from the API server is its own answer, status and Status, passed on. A failure, an
// answer naming no one, and a redirect, which is never followed, are 503s: never an
// identity guessed from the token, and never another credential.
func TestWhoAmIFailsClosed(t *testing.T) {
	t.Run("no session", func(t *testing.T) {
		api := newSharedAPI(t)
		front := whoamiFoyer(t, api.apiServer, credentials{refused: interruption.NotSignedIn()})
		code, header, _ := getWhoami(t, front, nil)
		if code != http.StatusUnauthorized || header.Get(interruption.Header) != "Unauthorized" {
			t.Fatalf("%d %v", code, header)
		}
		if n := len(api.received()); n != 0 {
			t.Fatalf("%d requests reached the API server without a session", n)
		}
	})

	var redirected atomic.Int32
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	t.Cleanup(elsewhere.Close)
	for name, tc := range map[string]struct {
		token  string
		ssr    http.HandlerFunc
		code   int
		reason string
	}{
		"a token the API server does not accept": {"unknown-token", nil, http.StatusUnauthorized, `"reason":"Unauthorized"`},
		"a review refused": {userToken, status(http.StatusForbidden, "Forbidden", "selfsubjectreviews is forbidden"),
			http.StatusForbidden, `"message":"selfsubjectreviews is forbidden"`},
		"the API server failing": {userToken, status(http.StatusInternalServerError, "InternalError", "etcd"), http.StatusServiceUnavailable, `"reason":"ServiceUnavailable"`},
		"an answer naming no one": {userToken, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview","status":{"userInfo":{}}}`)
		}, http.StatusServiceUnavailable, `"reason":"ServiceUnavailable"`},
		"a redirect": {userToken, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.URL+"/apis/authentication.k8s.io/v1/selfsubjectreviews", http.StatusTemporaryRedirect)
		}, http.StatusServiceUnavailable, `"reason":"ServiceUnavailable"`},
	} {
		t.Run(name, func(t *testing.T) {
			api := newSharedAPI(t)
			api.ssr = tc.ssr
			front := whoamiFoyer(t, api.apiServer, credentials{token: tc.token})
			code, header, body := getWhoami(t, front, nil)
			if code != tc.code || !strings.Contains(body, tc.reason) || header.Get("Cache-Control") != "no-store" {
				t.Fatalf("%d %s, want %d with %s", code, body, tc.code, tc.reason)
			}
			if strings.Contains(body, "userInfo") {
				t.Error("a failure answered with an identity")
			}
		})
	}
	if n := redirected.Load(); n != 0 {
		t.Fatalf("the redirect was followed %d times", n)
	}
}
