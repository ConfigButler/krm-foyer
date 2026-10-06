//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// browser is what the suite uses to reach krm-foyer: a cookie jar, no automatic
// redirects (each answer is inspected), and the fixture's transport. It signs in the
// way a person does, by walking Dex's login form.
type browser struct {
	jar    *cookiejar.Jar
	client *http.Client
	// transport is the client's, without the recorder, for responses that stream.
	transport http.RoundTripper
}

// seenResponse is one response krm-foyer sent the suite.
type seenResponse struct {
	target string
	header http.Header
	body   []byte
}

func (f *fixture) browser() *browser { return f.browserVia(f.client.Transport) }

// briefBrowser reaches the brief krm-foyer, whose sessions end within a minute.
func (f *fixture) briefBrowser() *browser { return f.browserVia(f.briefTransport) }

// frontDoorBrowser reaches the origin through Traefik, with the routes behind
// /auth/check (traefik-routes.yaml) as well as krm-foyer's own.
func (f *fixture) frontDoorBrowser() *browser { return f.browserVia(f.frontDoorTransport) }

// nginxBrowser reaches the origin through docs/ingress.md's nginx recipe.
func (f *fixture) nginxBrowser() *browser { return f.browserVia(f.nginxTransport) }

func (f *fixture) browserVia(transport http.RoundTripper) *browser {
	jar, err := cookiejar.New(nil)
	Expect(err).NotTo(HaveOccurred())
	return &browser{jar: jar, transport: transport, client: &http.Client{
		Jar:           jar,
		Transport:     recorder{f: f, next: transport},
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// recorder keeps every response krm-foyer sends, whoever asked.
type recorder struct {
	f    *fixture
	next http.RoundTripper
}

func (rec recorder) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := rec.next.RoundTrip(r)
	if err != nil || r.URL.Scheme+"://"+r.URL.Host != rec.f.foyerURL {
		return resp, err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	rec.f.seenMu.Lock()
	rec.f.seen = append(rec.f.seen, seenResponse{target: r.URL.String(), header: resp.Header.Clone(), body: body})
	rec.f.seenMu.Unlock()
	return resp, nil
}

// do sends a request; a target starting with / is on krm-foyer. Like fixture.direct,
// it marks the request with a unique User-Agent, unless header sets one.
func (b *browser) do(ctx context.Context, method, target string, body []byte, header http.Header) answer {
	if strings.HasPrefix(target, "/") {
		target = fx.foyerURL + target
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, r)
	Expect(err).NotTo(HaveOccurred())
	marker := "krm-foyer-e2e/" + randomID()
	req.Header.Set("User-Agent", marker)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := b.client.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	return answer{Code: resp.StatusCode, Header: resp.Header, Body: got, Marker: req.Header.Get("User-Agent")}
}

func (b *browser) get(ctx context.Context, target string) answer {
	return b.do(ctx, http.MethodGet, target, nil, nil)
}

// startLogin opens /auth/login and returns where krm-foyer sent the browser.
func (b *browser) startLogin(ctx context.Context, returnTo string) *url.URL {
	a := b.get(ctx, "/auth/login?"+url.Values{"return_to": {returnTo}}.Encode())
	Expect(a.Code).To(Equal(http.StatusFound), "%s", a.Body)
	u, err := url.Parse(a.Header.Get("Location"))
	Expect(err).NotTo(HaveOccurred())
	return u
}

// atDex walks Dex's login form as user, starting from the authorization request, and
// returns the callback URL Dex sends the browser back to. It does not open it.
func (b *browser) atDex(ctx context.Context, authorize *url.URL, user string) string {
	Expect(authorize.Scheme + "://" + authorize.Host).To(Equal(fx.dexIssuer))
	next := authorize
	for range 10 {
		a := b.get(ctx, next.String())
		if a.Code == http.StatusOK {
			// Dex's password form: post the credentials to its action.
			m := loginForm.FindSubmatch(a.Body)
			Expect(m).NotTo(BeNil(), "no login form at %s:\n%s", next, a.Body)
			action, err := next.Parse(html.UnescapeString(string(m[1])))
			Expect(err).NotTo(HaveOccurred())
			form := url.Values{"login": {user}, "password": {password}}.Encode()
			a = b.do(ctx, http.MethodPost, action.String(), []byte(form),
				http.Header{"Content-Type": {"application/x-www-form-urlencoded"}})
			next = action
		}
		Expect(a.Code).To(BeNumerically(">=", 300), "Dex answered %d at %s:\n%s", a.Code, next, a.Body)
		Expect(a.Code).To(BeNumerically("<", 400), "Dex answered %d at %s:\n%s", a.Code, next, a.Body)
		loc, err := next.Parse(a.Header.Get("Location"))
		Expect(err).NotTo(HaveOccurred())
		if loc.Scheme+"://"+loc.Host == fx.foyerURL {
			return loc.String()
		}
		next = loc
	}
	Fail("Dex did not send the browser back to krm-foyer")
	return ""
}

var loginForm = regexp.MustCompile(`<form[^>]*method="post"[^>]*action="([^"]*)"`)

// login signs in as user and returns krm-foyer's answer to the callback.
func (b *browser) login(ctx context.Context, user, returnTo string) answer {
	return b.get(ctx, b.atDex(ctx, b.startLogin(ctx, returnTo), user))
}

// signedIn logs in as user, checks it worked, and returns the session's CSRF token.
func (b *browser) signedIn(ctx context.Context, user string) string {
	a := b.login(ctx, user, "/")
	Expect(a.Code).To(Equal(http.StatusSeeOther), "%s", a.Body)
	s := b.session(ctx)
	Expect(s.Authenticated).To(BeTrue())
	Expect(s.Email).To(Equal(user))
	return s.CSRFToken
}

// sessionState is /auth/session's answer.
type sessionState struct {
	Authenticated bool   `json:"authenticated"`
	Issuer        string `json:"issuer"`
	Subject       string `json:"subject"`
	Email         string `json:"email"`
	ExpiresAt     string `json:"expiresAt"`
	CSRFToken     string `json:"csrfToken"`
	CSRFHeader    string `json:"csrfHeader"`
}

func (b *browser) session(ctx context.Context) sessionState {
	a := b.get(ctx, "/auth/session")
	var s sessionState
	Expect(a.decode(&s)).To(Succeed(), "%d %s", a.Code, a.Body)
	return s
}

// cookie returns the value of krm-foyer's cookie name in the jar.
func (b *browser) cookie(name string) string {
	u, err := url.Parse(fx.foyerURL)
	Expect(err).NotTo(HaveOccurred())
	for _, c := range b.jar.Cookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// setCookie puts a cookie in the jar, as an attacker who copied it would.
func (b *browser) setCookie(name, value string) {
	u, err := url.Parse(fx.foyerURL)
	Expect(err).NotTo(HaveOccurred())
	b.jar.SetCookies(u, []*http.Cookie{{Name: name, Value: value}}) //nolint:gosec // a request cookie has no attributes
}

func (a answer) decode(v any) error { return json.Unmarshal(a.Body, v) }
