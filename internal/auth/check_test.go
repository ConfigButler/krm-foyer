package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"

	"github.com/ConfigButler/krm-foyer/internal/interruption"
)

// fakeIdentify stands in for the API server's SelfSubjectReview: it records the
// request it was asked about, and answers as the API server would.
type fakeIdentify struct {
	mu     sync.Mutex
	asked  []*http.Request
	refuse bool
}

var aliceInfo = authenticationv1.UserInfo{
	Username: "oidc:alice@example.com", UID: "uid-alice",
	Groups: []string{"system:authenticated", "oidc:voters"},
	Extra:  map[string]authenticationv1.ExtraValue{"configbutler.ai/claims/email": {alice}},
}

func (f *fakeIdentify) identify(w http.ResponseWriter, r *http.Request) (authenticationv1.UserInfo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, r)
	if f.refuse {
		(&interruption.Interruption{Status: http.StatusServiceUnavailable, Reason: "ServiceUnavailable", Message: "no answer"}).Serve(w, r)
		return authenticationv1.UserInfo{}, false
	}
	return aliceInfo, true
}

func (f *fakeIdentify) calls() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.asked...)
}

// checkHarness serves /auth/check beside the login routes, as the binary does.
func checkHarness(t *testing.T, login LoginConfig) (*harness, *fakeIdentify) {
	t.Helper()
	h := newHarnessWith(t, login)
	f := &fakeIdentify{}
	mux := http.NewServeMux()
	mux.Handle("/auth/", h.auth.Handler())
	mux.Handle("GET /auth/check", h.auth.Check(f.identify))
	var handler http.Handler = mux
	h.handler.Store(&handler)
	return h, f
}

func decodeIdentity(t *testing.T, header http.Header) CheckIdentity {
	t.Helper()
	values := header.Values(IdentityHeader)
	if len(values) != 1 {
		t.Fatalf("%s = %q, want one value", IdentityHeader, values)
	}
	raw, err := base64.RawURLEncoding.DecodeString(values[0])
	if err != nil {
		t.Fatal(err)
	}
	var id CheckIdentity
	if err := json.Unmarshal(raw, &id); err != nil {
		t.Fatal(err)
	}
	return id
}

// The forged copy a browser may send of the identity header, which must never come
// back as if krm-foyer had said it.
var forgedIdentity = base64.RawURLEncoding.EncodeToString([]byte(`{"userInfo":{"username":"system:admin"},"connector":"room-pass"}`))

// Signed out, the check answers as /k8s does: a 401 Status for code, a page with a
// sign-in link back to the page the ingress names for a person, and, when the ingress
// asks, a redirect to the login for a page load only. It never names an identity,
// whatever the browser sent, and asks no one who the user is.
func TestCheckSignedOut(t *testing.T) {
	h, f := checkHarness(t, LoginConfig{})
	b := h.browser()
	forged := http.Header{IdentityHeader: {forgedIdentity}}

	code := b.do(http.MethodGet, "/auth/check?identity=true", forged)
	if code.code != http.StatusUnauthorized || !hasReason(code.body, "Unauthorized") ||
		code.header.Get(interruption.Header) != "Unauthorized" || code.header.Get("Cache-Control") != "no-store" {
		t.Errorf("a fetch: %d %v %s", code.code, code.header, code.body)
	}

	page := b.do(http.MethodGet, "/auth/check", http.Header{
		"Sec-Fetch-Dest": {"document"}, "Sec-Fetch-Mode": {"navigate"}, "X-Forwarded-Uri": {"/room?id=7"},
	})
	if page.code != http.StatusUnauthorized || !strings.Contains(page.body, `/auth/login?return_to=%2Froom%3Fid%3D7`) {
		t.Errorf("a page load: %d %s", page.code, page.body)
	}

	for name, tc := range map[string]struct {
		header   http.Header
		location string
	}{
		"Traefik's page":             {http.Header{"X-Forwarded-Uri": {"/room?id=7"}}, "/auth/login?return_to=%2Froom%3Fid%3D7"},
		"nginx's page":               {http.Header{"X-Original-Uri": {"/coffee"}}, "/auth/login?return_to=%2Fcoffee"},
		"a navigation":               {http.Header{"Sec-Fetch-Mode": {"navigate"}, "X-Forwarded-Uri": {"/a"}}, "/auth/login?return_to=%2Fa"},
		"no page":                    {nil, "/auth/login?return_to=%2F"},
		"another site":               {http.Header{"X-Forwarded-Uri": {"//evil.example/"}}, "/auth/login?return_to=%2F"},
		"a backslash":                {http.Header{"X-Forwarded-Uri": {`/\evil.example`}}, "/auth/login?return_to=%2F"},
		"an absolute URL":            {http.Header{"X-Forwarded-Uri": {"https://evil.example/"}}, "/auth/login?return_to=%2F"},
		"two pages":                  {http.Header{"X-Forwarded-Uri": {"/a", "/b"}}, "/auth/login?return_to=%2F"},
		"a forged one, then nginx's": {http.Header{"X-Forwarded-Uri": {"//evil.example"}, "X-Original-Uri": {"/b"}}, "/auth/login?return_to=%2Fb"},
	} {
		got := b.do(http.MethodGet, "/auth/check?redirect=true", tc.header)
		// Absolute, at the public URL: an ingress resolves a relative Location against
		// the check's own address.
		if want := h.foyer.URL + tc.location; got.code != http.StatusFound || got.header.Get("Location") != want || got.header.Get(IdentityHeader) != "" {
			t.Errorf("%s: %d to %q, want 302 to %q", name, got.code, got.header.Get("Location"), want)
		}
	}

	// A script gets the 401 it can act on, and a write is never sent to a login page.
	for name, header := range map[string]http.Header{
		"a fetch": {"Sec-Fetch-Mode": {"cors"}},
		"a POST":  {"X-Forwarded-Method": {http.MethodPost}},
	} {
		got := b.do(http.MethodGet, "/auth/check?redirect=true", header)
		if got.code != http.StatusUnauthorized || !hasReason(got.body, "Unauthorized") {
			t.Errorf("%s: %d %s, want the 401 Status", name, got.code, got.body)
		}
	}
	for _, resp := range []response{code, page} {
		if resp.header.Get(IdentityHeader) != "" {
			t.Errorf("a signed-out answer carries %s", IdentityHeader)
		}
	}
	if n := len(f.calls()); n != 0 {
		t.Errorf("asked who a signed-out browser is %d times", n)
	}
}

// Signed in, the gate's answer is 204 and nothing more; with identity=true it is the
// API server's answer, with the session's display name and connector, in one header.
// The identity is asked about the request the ingress forwards, and a browser's own
// copy of the header is never what comes back.
func TestCheckSignedIn(t *testing.T) {
	login := LoginConfig{SessionClaims: SessionClaims{Connector: "/federated_claims/connector_id"}}
	h, f := checkHarness(t, login)
	h.issuer.mu.Lock()
	h.issuer.tamper = func(claims map[string]any) {
		claims["name"] = "Alice Example"
		claims["federated_claims"] = map[string]any{"connector_id": "room-pass"}
	}
	h.issuer.mu.Unlock()
	b := h.browser()
	b.login(alice, "/")
	_, s := b.session()

	gate := b.do(http.MethodGet, "/auth/check", http.Header{IdentityHeader: {forgedIdentity}})
	if gate.code != http.StatusNoContent || gate.header.Get(IdentityHeader) != "" || gate.header.Get("Cache-Control") != "no-store" {
		t.Errorf("the page gate: %d %v", gate.code, gate.header)
	}
	if n := len(f.calls()); n != 0 {
		t.Errorf("the page gate asked who the user is %d times", n)
	}

	got := b.do(http.MethodGet, "/auth/check?identity=true", http.Header{
		IdentityHeader: {forgedIdentity}, "X-Forwarded-Uri": {"/public/vote"},
	})
	if got.code != http.StatusNoContent {
		t.Fatalf("identity: %d %s", got.code, got.body)
	}
	id := decodeIdentity(t, got.header)
	if id.UserInfo.Username != aliceInfo.Username || id.UserInfo.UID != aliceInfo.UID ||
		strings.Join(id.UserInfo.Groups, ",") != "system:authenticated,oidc:voters" ||
		id.UserInfo.Extra["configbutler.ai/claims/email"][0] != alice ||
		id.DisplayName != "Alice Example" || id.Connector != "room-pass" ||
		id.Issuer != h.issuer.URL || !id.ExpiresAt.Equal(*s.ExpiresAt) {
		t.Errorf("identity = %+v", id)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(got.header.Get(IdentityHeader))
	if strings.Contains(string(raw), h.issuer.idTokens[0]) || strings.Contains(string(raw), "system:admin") {
		t.Errorf("the identity holds a token or the browser's forgery: %s", raw)
	}
	asked := f.calls()
	if len(asked) != 1 || asked[0].Method != http.MethodGet || asked[0].RequestURI != "/public/vote" {
		t.Fatalf("asked about %v", asked)
	}
}

// A write to a domain backend needs what a write to /k8s needs: the session's CSRF
// proof, from krm-foyer's origin. The ingress names the browser's method; without
// the proof the answer is the 403 /k8s would give, and no one is asked who the user is.
func TestCheckHoldsWritesToTheCSRFRules(t *testing.T) {
	h, f := checkHarness(t, LoginConfig{})
	b := h.browser()
	b.login(alice, "/")
	_, s := b.session()
	post := func(header http.Header) response {
		header.Set("X-Forwarded-Method", http.MethodPost)
		return b.do(http.MethodGet, "/auth/check?identity=true", header)
	}

	for name, tc := range map[string]struct {
		header http.Header
		reason string
	}{
		"no proof":              {http.Header{"Origin": {h.foyer.URL}}, "CSRFProofRequired"},
		"another site's":        {http.Header{"Origin": {"https://evil.example"}, csrfKey: {s.CSRFToken}}, "CrossOriginRequest"},
		"a wrong token":         {http.Header{"Origin": {h.foyer.URL}, csrfKey: {"guess"}}, "CSRFProofRequired"},
		"no origin, no fetch":   {http.Header{csrfKey: {s.CSRFToken}}, "CrossOriginRequest"},
		"a cross-site fetch":    {http.Header{"Sec-Fetch-Site": {"same-site"}, csrfKey: {s.CSRFToken}}, "CrossOriginRequest"},
		"two tokens, one right": {http.Header{"Origin": {h.foyer.URL}, csrfKey: {s.CSRFToken, "x"}}, "CSRFProofRequired"},
	} {
		got := post(tc.header)
		if got.code != http.StatusForbidden || !hasReason(got.body, tc.reason) || got.header.Get(IdentityHeader) != "" {
			t.Errorf("%s: %d %s, want 403 %s", name, got.code, got.body, tc.reason)
		}
	}
	if n := len(f.calls()); n != 0 {
		t.Fatalf("asked who the user is for %d refused writes", n)
	}

	// A method spelt any other way is a write too, not a read.
	got := b.do(http.MethodGet, "/auth/check?identity=true", http.Header{"X-Forwarded-Method": {"get"}})
	if got.code != http.StatusForbidden {
		t.Errorf("a lower-case get: %d, want the 403 of a write", got.code)
	}

	ok := post(http.Header{"Origin": {h.foyer.URL}, csrfKey: {s.CSRFToken}})
	if ok.code != http.StatusNoContent || decodeIdentity(t, ok.header).UserInfo.Username != aliceInfo.Username {
		t.Errorf("a write with proof: %d %v", ok.code, ok.header)
	}
	if asked := f.calls(); len(asked) != 1 || asked[0].Method != http.MethodPost {
		t.Errorf("asked about %v, want the POST", asked)
	}
}

// When the API server cannot say who the user is, its answer is the check's, with no
// identity: never one guessed from the token.
func TestCheckWithoutAnAnswer(t *testing.T) {
	h, f := checkHarness(t, LoginConfig{})
	f.refuse = true
	b := h.browser()
	b.login(alice, "/")
	got := b.do(http.MethodGet, "/auth/check?identity=true", nil)
	if got.code != http.StatusServiceUnavailable || got.header.Get(IdentityHeader) != "" {
		t.Errorf("%d %v", got.code, got.header)
	}
}

// An ingress that asks wrongly is told so, rather than given a guess at what it meant:
// identity=1 must not quietly be a check without identity.
func TestCheckOptions(t *testing.T) {
	h, f := checkHarness(t, LoginConfig{})
	b := h.browser()
	b.login(alice, "/")
	for name, tc := range map[string]struct {
		query  string
		header http.Header
	}{
		"identity=1":            {"identity=1", nil},
		"identity twice":        {"identity=true&identity=true", nil},
		"redirect=yes":          {"redirect=yes", nil},
		"an unknown option":     {"identity=true&user=admin", nil},
		"a broken query":        {"identity=%zz", nil},
		"two methods":           {"", http.Header{"X-Forwarded-Method": {"GET", "POST"}}},
		"an empty method":       {"", http.Header{"X-Forwarded-Method": {""}}},
		"a method with a space": {"", http.Header{"X-Forwarded-Method": {"GET POST"}}},
	} {
		got := b.do(http.MethodGet, "/auth/check?"+tc.query, tc.header)
		if got.code != http.StatusBadRequest || !hasReason(got.body, "BadRequest") || got.header.Get(IdentityHeader) != "" {
			t.Errorf("%s: %d %s, want 400", name, got.code, got.body)
		}
	}
	if n := len(f.calls()); n != 0 {
		t.Errorf("asked who the user is for %d bad checks", n)
	}
	for _, query := range []string{"", "identity=true", "redirect=true", "redirect=true&identity=true"} {
		if got := b.do(http.MethodGet, "/auth/check?"+query, nil); got.code != http.StatusNoContent {
			t.Errorf("%q: %d %s", query, got.code, got.body)
		}
	}
}
