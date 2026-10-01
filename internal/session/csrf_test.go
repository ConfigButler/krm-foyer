package session

import (
	"errors"
	"net/http"
	"testing"
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

func assertRefused(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
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
				edit func(http.Header)
				want error
			}{
				"no proof":                {func(h http.Header) { h.Del(CSRFHeader) }, ErrNoCSRFProof},
				"empty proof":             {func(h http.Header) { h.Set(CSRFHeader, "") }, ErrNoCSRFProof},
				"another session's proof": {func(h http.Header) { h.Set(CSRFHeader, os.CSRFToken) }, ErrNoCSRFProof},
				"proof with a suffix":     {func(h http.Header) { h.Set(CSRFHeader, s.CSRFToken+"x") }, ErrNoCSRFProof},
				"proof cut short":         {func(h http.Header) { h.Set(CSRFHeader, s.CSRFToken[1:]) }, ErrNoCSRFProof},
				"the session ID as proof": {func(h http.Header) { h.Set(CSRFHeader, c.Value) }, ErrNoCSRFProof},
				// Two fields: Get would read only the first.
				"wrong proof, then right": {func(h http.Header) { h[csrfKey] = []string{"x", s.CSRFToken} }, ErrNoCSRFProof},
				"right proof, then wrong": {func(h http.Header) { h[csrfKey] = []string{s.CSRFToken, "x"} }, ErrNoCSRFProof},
				"right proof twice":       {func(h http.Header) { h[csrfKey] = []string{s.CSRFToken, s.CSRFToken} }, ErrNoCSRFProof},
				"proofs joined by comma":  {func(h http.Header) { h.Set(CSRFHeader, s.CSRFToken+", "+s.CSRFToken) }, ErrNoCSRFProof},

				"cross-site origin":       {func(h http.Header) { h.Set("Origin", "https://evil.example") }, ErrCrossOrigin},
				"sibling subdomain":       {func(h http.Header) { h.Set("Origin", "https://app.example.test") }, ErrCrossOrigin},
				"http origin":             {func(h http.Header) { h.Set("Origin", "http://foyer.example.test") }, ErrCrossOrigin},
				"other port":              {func(h http.Header) { h.Set("Origin", origin+":8443") }, ErrCrossOrigin},
				"origin as a prefix":      {func(h http.Header) { h.Set("Origin", origin+".evil.example") }, ErrCrossOrigin},
				"origin with a path":      {func(h http.Header) { h.Set("Origin", origin+"/") }, ErrCrossOrigin},
				"null origin":             {func(h http.Header) { h.Set("Origin", "null") }, ErrCrossOrigin},
				"empty origin":            {func(h http.Header) { h.Set("Origin", "") }, ErrCrossOrigin},
				"two origins, ours first": {func(h http.Header) { h["Origin"] = []string{origin, "https://evil.example"} }, ErrCrossOrigin},
				"ours twice":              {func(h http.Header) { h["Origin"] = []string{origin, origin} }, ErrCrossOrigin},
				// Origin decides when present; Sec-Fetch-Site cannot overrule it.
				"cross-site origin, same-origin fetch": {func(h http.Header) {
					h.Set("Origin", "https://evil.example")
					h.Set("Sec-Fetch-Site", "same-origin")
				}, ErrCrossOrigin},

				"no origin, cross-site":  {func(h http.Header) { h.Del("Origin"); h.Set("Sec-Fetch-Site", "cross-site") }, ErrCrossOrigin},
				"no origin, same-site":   {func(h http.Header) { h.Del("Origin"); h.Set("Sec-Fetch-Site", "same-site") }, ErrCrossOrigin},
				"no origin, typed in":    {func(h http.Header) { h.Del("Origin"); h.Set("Sec-Fetch-Site", "none") }, ErrCrossOrigin},
				"no origin, two values":  {func(h http.Header) { h.Del("Origin"); h["Sec-Fetch-Site"] = []string{"same-origin", "cross-site"} }, ErrCrossOrigin},
				"no origin, no metadata": {func(h http.Header) { h.Del("Origin"); h.Del("Sec-Fetch-Site") }, ErrCrossOrigin},
			} {
				t.Run(name, func(t *testing.T) {
					r := sameOrigin(request(method, c), s.CSRFToken)
					tc.edit(r.Header)
					assertRefused(t, h.m.CheckMutation(r, s), tc.want)
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
	assertRefused(t, h.m.CheckMutation(sameOrigin(request(http.MethodPost), ""), Session{}), ErrNoCSRFProof)
}

// A refused mutation leaves no trace on the session: it does not count as activity.
func TestRefusedMutationDoesNotTouch(t *testing.T) {
	h := newHarness(t)
	c := h.start(t, h.user())
	h.clock.Advance(idle - 1)
	r := request(http.MethodPost, c)
	r.Header.Set("Origin", "https://evil.example")
	if _, err := h.m.Use(r); err == nil {
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
		if err != nil && !errors.Is(err, ErrCrossOrigin) && !errors.Is(err, ErrNoCSRFProof) {
			t.Fatalf("refusal is neither ErrCrossOrigin nor ErrNoCSRFProof: %v", err)
		}
	})
}
