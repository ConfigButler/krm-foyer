package session

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ConfigButler/krm-foyer/internal/proxy"
)

// csrfKey is CSRFHeader as a key of a parsed request's header map. A test that
// writes the map directly must use it, or it adds a field no server would produce.
var csrfKey = http.CanonicalHeaderKey(CSRFHeader)

// sameOrigin is what a page on krm-foyer's origin sends with a fetch that writes.
func sameOrigin(r *http.Request, proof string) *http.Request {
	r.Header.Set("Origin", origin)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set(CSRFHeader, proof)
	return r
}

func assertRefused(t *testing.T, err error, reason string) {
	t.Helper()
	var refused *proxy.Interruption
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if refused.Status != http.StatusForbidden || refused.Reason != reason {
		t.Fatalf("refused with %d %s, want 403 %s", refused.Status, refused.Reason, reason)
	}
	// RBAC's refusal is 403 Forbidden; this one must not be mistaken for it.
	if refused.Reason == "Forbidden" {
		t.Fatal("a CSRF refusal reads as an RBAC refusal")
	}
}

// Reading needs no proof. Kubernetes' GETs are safe by API convention, and a tab
// must be able to open /k8s.
func TestReadsNeedNoProof(t *testing.T) {
	h := newHarness(t)
	c := h.start(t, h.user())
	s, _ := h.lookup(c)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		r := request(method, c)
		r.Header.Set("Origin", "https://evil.example")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		if err := h.m.CheckMutation(r, s); err != nil {
			t.Errorf("%s: %v", method, err)
		}
	}
}

// Every other method needs CSRF proof from the session and a request from
// krm-foyer's own origin. Each case starts from a request that passes and changes
// one thing.
func TestMutationsNeedProofAndTheSameOrigin(t *testing.T) {
	h := newHarness(t)
	c := h.start(t, h.user())
	s, _ := h.lookup(c)
	other := h.start(t, h.user())
	os, _ := h.lookup(other)

	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
		http.MethodOptions, "PROPFIND", "get", "head",
	} {
		t.Run(method, func(t *testing.T) {
			if err := h.m.CheckMutation(sameOrigin(request(method, c), s.CSRFToken), s); err != nil {
				t.Fatalf("a same-origin request with proof was refused: %v", err)
			}
			for name, tc := range map[string]struct {
				edit   func(http.Header)
				reason string
			}{
				"no proof":                {func(h http.Header) { h.Del(CSRFHeader) }, "CSRFProofRequired"},
				"empty proof":             {func(h http.Header) { h.Set(CSRFHeader, "") }, "CSRFProofRequired"},
				"another session's proof": {func(h http.Header) { h.Set(CSRFHeader, os.CSRFToken) }, "CSRFProofRequired"},
				"proof with a suffix":     {func(h http.Header) { h.Set(CSRFHeader, s.CSRFToken+"x") }, "CSRFProofRequired"},
				"proof cut short":         {func(h http.Header) { h.Set(CSRFHeader, s.CSRFToken[1:]) }, "CSRFProofRequired"},
				"the session ID as proof": {func(h http.Header) { h.Set(CSRFHeader, c.Value) }, "CSRFProofRequired"},
				// Two fields: Get would read only the first.
				"wrong proof, then right": {func(h http.Header) { h[csrfKey] = []string{"x", s.CSRFToken} }, "CSRFProofRequired"},
				"right proof, then wrong": {func(h http.Header) { h[csrfKey] = []string{s.CSRFToken, "x"} }, "CSRFProofRequired"},
				"right proof twice":       {func(h http.Header) { h[csrfKey] = []string{s.CSRFToken, s.CSRFToken} }, "CSRFProofRequired"},
				"proofs joined by comma":  {func(h http.Header) { h.Set(CSRFHeader, s.CSRFToken+", "+s.CSRFToken) }, "CSRFProofRequired"},

				"cross-site origin":       {func(h http.Header) { h.Set("Origin", "https://evil.example") }, "CrossOriginRequest"},
				"sibling subdomain":       {func(h http.Header) { h.Set("Origin", "https://app.example.test") }, "CrossOriginRequest"},
				"http origin":             {func(h http.Header) { h.Set("Origin", "http://foyer.example.test") }, "CrossOriginRequest"},
				"other port":              {func(h http.Header) { h.Set("Origin", origin+":8443") }, "CrossOriginRequest"},
				"origin as a prefix":      {func(h http.Header) { h.Set("Origin", origin+".evil.example") }, "CrossOriginRequest"},
				"origin with a path":      {func(h http.Header) { h.Set("Origin", origin+"/") }, "CrossOriginRequest"},
				"null origin":             {func(h http.Header) { h.Set("Origin", "null") }, "CrossOriginRequest"},
				"empty origin":            {func(h http.Header) { h.Set("Origin", "") }, "CrossOriginRequest"},
				"two origins, ours first": {func(h http.Header) { h["Origin"] = []string{origin, "https://evil.example"} }, "CrossOriginRequest"},
				"ours twice":              {func(h http.Header) { h["Origin"] = []string{origin, origin} }, "CrossOriginRequest"},
				// Origin decides when present; Sec-Fetch-Site cannot overrule it.
				"cross-site origin, same-origin fetch": {func(h http.Header) {
					h.Set("Origin", "https://evil.example")
					h.Set("Sec-Fetch-Site", "same-origin")
				}, "CrossOriginRequest"},

				"no origin, cross-site":  {func(h http.Header) { h.Del("Origin"); h.Set("Sec-Fetch-Site", "cross-site") }, "CrossOriginRequest"},
				"no origin, same-site":   {func(h http.Header) { h.Del("Origin"); h.Set("Sec-Fetch-Site", "same-site") }, "CrossOriginRequest"},
				"no origin, typed in":    {func(h http.Header) { h.Del("Origin"); h.Set("Sec-Fetch-Site", "none") }, "CrossOriginRequest"},
				"no origin, two values":  {func(h http.Header) { h.Del("Origin"); h["Sec-Fetch-Site"] = []string{"same-origin", "cross-site"} }, "CrossOriginRequest"},
				"no origin, no metadata": {func(h http.Header) { h.Del("Origin"); h.Del("Sec-Fetch-Site") }, "CrossOriginRequest"},
			} {
				t.Run(name, func(t *testing.T) {
					r := sameOrigin(request(method, c), s.CSRFToken)
					tc.edit(r.Header)
					assertRefused(t, h.m.CheckMutation(r, s), tc.reason)
				})
			}
		})
	}
}

// Without Origin, a browser's Sec-Fetch-Site: same-origin is enough provenance.
// Some browsers omit Origin on same-origin requests they consider safe to send.
func TestSecFetchSiteStandsInForOrigin(t *testing.T) {
	h := newHarness(t)
	c := h.start(t, h.user())
	s, _ := h.lookup(c)
	r := sameOrigin(request(http.MethodDelete, c), s.CSRFToken)
	r.Header.Del("Origin")
	if err := h.m.CheckMutation(r, s); err != nil {
		t.Fatal(err)
	}
}

// The configured origin is compared the way a browser serializes one: a default
// port and upper-case letters in the configuration do not lock everyone out.
func TestOriginIsNormalized(t *testing.T) {
	for _, configured := range []string{origin, origin + "/", "https://FOYER.example.test", origin + ":443"} {
		t.Run(configured, func(t *testing.T) {
			m, err := New(Config{Store: NewMemory(nil), Origin: configured, IdleTimeout: idle, AbsoluteTimeout: maxAge})
			if err != nil {
				t.Fatal(err)
			}
			s := Session{CSRFToken: "proof"}
			if err := m.CheckMutation(sameOrigin(request(http.MethodPost), "proof"), s); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A session without a CSRF token (a store bug, say) accepts no proof at all, not
// the empty one.
func TestEmptySessionTokenAcceptsNothing(t *testing.T) {
	h := newHarness(t)
	assertRefused(t, h.m.CheckMutation(sameOrigin(request(http.MethodPost), ""), Session{}), "CSRFProofRequired")
}

// Through the proxy: Manager is the proxy's only credential source, and a refused
// request never reaches the API server.
func TestThroughTheProxy(t *testing.T) {
	h := newHarness(t)
	c := h.start(t, h.user())
	s, _ := h.lookup(c)

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
	p, err := proxy.New(proxy.Config{
		Server: u, Credentials: h.m,
		RootCAs: api.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs,
	})
	if err != nil {
		t.Fatal(err)
	}
	send := func(r *http.Request) int {
		r.RequestURI = "/k8s/api/v1/namespaces/team-a/configmaps"
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w.Code
	}
	reached := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(auths)
	}

	for _, tc := range []struct {
		name string
		r    *http.Request
		code int
	}{
		{"read without a session", request(http.MethodGet), http.StatusUnauthorized},
		{"write without a session", sameOrigin(request(http.MethodPost), s.CSRFToken), http.StatusUnauthorized},
		{"write without proof", func() *http.Request {
			r := sameOrigin(request(http.MethodPost, c), s.CSRFToken)
			r.Header.Del(CSRFHeader)
			return r
		}(), http.StatusForbidden},
		{"cross-site write", func() *http.Request {
			r := sameOrigin(request(http.MethodPost, c), s.CSRFToken)
			r.Header.Set("Origin", "https://evil.example")
			return r
		}(), http.StatusForbidden},
	} {
		if code := send(tc.r); code != tc.code {
			t.Errorf("%s: status %d, want %d", tc.name, code, tc.code)
		}
	}
	if n := len(reached()); n != 0 {
		t.Fatalf("%d refused requests reached the API server", n)
	}

	if code := send(request(http.MethodGet, c)); code != http.StatusOK {
		t.Errorf("signed-in read: %d", code)
	}
	if code := send(sameOrigin(request(http.MethodPost, c), s.CSRFToken)); code != http.StatusOK {
		t.Errorf("signed-in write with proof: %d", code)
	}
	if got := reached(); !slices.Equal(got, []string{"Bearer " + idToken, "Bearer " + idToken}) {
		t.Errorf("the API server received Authorization %q", got)
	}
}

// A refused mutation leaves no trace on the session: it does not count as activity.
func TestRefusedMutationDoesNotTouch(t *testing.T) {
	h := newHarness(t)
	c := h.start(t, h.user())
	h.clock.Advance(idle - 1)
	r := request(http.MethodPost, c)
	r.Header.Set("Origin", "https://evil.example")
	if _, err := h.m.Token(r); err == nil {
		t.Fatal("refused nothing")
	}
	h.clock.Advance(1)
	_, err := h.lookup(c)
	assertNoSession(t, err)
}

// FuzzCheckMutation states the rule from the outside, without reading the code:
// a request is let through exactly when it is a GET or HEAD, or when it carries one
// proof field equal to the session's token and comes from krm-foyer's origin, shown
// by a single Origin field equal to it, or, with no Origin at all, by a single
// Sec-Fetch-Site field saying same-origin.
func FuzzCheckMutation(f *testing.F) {
	const proof = "the-session-proof"
	f.Add("POST", origin, "", "same-origin", "", proof, "", uint8(0b1011))
	f.Add("POST", origin, "", "same-origin", "", proof, "", uint8(0b1001))
	f.Add("DELETE", "", "", "same-origin", "", proof, "", uint8(0b1010))
	f.Add("PUT", origin, origin, "", "", proof, proof, uint8(0b1111_1111))
	f.Add("GET", "https://evil.example", "", "cross-site", "", "", "", uint8(0b0011))
	f.Add("PATCH", "null", "", "same-origin", "", proof, "", uint8(0b1011))
	f.Fuzz(func(t *testing.T, method, origin1, origin2, site1, site2, proof1, proof2 string, fields uint8) {
		m, err := New(Config{Store: NewMemory(nil), Origin: origin, IdleTimeout: idle, AbsoluteTimeout: maxAge})
		if err != nil {
			t.Fatal(err)
		}
		r, err := http.NewRequest(http.MethodGet, origin+"/k8s/api", nil) //nolint:noctx // never sent
		if err != nil {
			t.Fatal(err)
		}
		r.Method = method
		// fields picks how many of each header the request carries: 0, 1 or 2.
		put := func(name string, n uint8, a, b string) {
			switch n % 3 {
			case 1:
				r.Header[name] = []string{a}
			case 2:
				r.Header[name] = []string{a, b}
			}
		}
		put("Origin", fields, origin1, origin2)
		put("Sec-Fetch-Site", fields>>2, site1, site2)
		put(csrfKey, fields>>4, proof1, proof2)

		single := func(name, want string) bool {
			v, ok := r.Header[name]
			return ok && len(v) == 1 && v[0] == want
		}
		_, hasOrigin := r.Header["Origin"]
		want := method == "GET" || method == "HEAD" ||
			single(csrfKey, proof) &&
				(single("Origin", origin) || !hasOrigin && single("Sec-Fetch-Site", "same-origin"))

		err = m.CheckMutation(r, Session{CSRFToken: proof})
		if (err == nil) != want {
			t.Fatalf("method %q, headers %q: CheckMutation = %v, want allowed = %v", method, r.Header, err, want)
		}
		if err != nil && !strings.Contains(err.Error(), "403") {
			t.Fatalf("refusal is not a 403: %v", err)
		}
	})
}
