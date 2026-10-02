package proxy

import (
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/ConfigButler/krm-foyer/internal/interruption"
)

// navigation is the header a browser sends when a person opens a URL in a tab.
// Page scripts cannot set it: Sec- headers are forbidden to fetch and XHR.
var navigation = http.Header{"Sec-Fetch-Dest": {"document"}, "Sec-Fetch-Mode": {"navigate"}}

// interruptionCase triggers one row of the interruptions table in docs/design.md.
type interruptionCase struct {
	name     string
	target   string
	creds    Credentials
	upstream http.HandlerFunc
	header   http.Header
	// occupy holds one request of the session open first, with a concurrency limit
	// of one per session.
	occupy bool
	status int
	// page must contain these, as text a person reads.
	page []string
}

func interruptionCases() []interruptionCase {
	return []interruptionCase{
		{name: "no session", target: "/k8s/api/v1/namespaces?labelSelector=team%3Da",
			creds: credentials{refused: interruption.NotSignedIn()}, status: http.StatusUnauthorized,
			page: []string{"Sign in", `href="/auth/login?return_to=%2Fk8s%2Fapi%2Fv1%2Fnamespaces%3FlabelSelector%3Dteam%253Da"`}},
		{name: "session store unavailable", target: "/k8s/api/v1/namespaces",
			creds: credentials{refused: unavailable}, status: http.StatusServiceUnavailable,
			page: []string{"session could not be checked"}},
		{name: "refused by the session", target: "/k8s/api/v1/namespaces",
			creds: credentials{refused: csrfRefusal}, status: http.StatusForbidden,
			page: []string{"CSRFProofRequired"}},
		{name: "non-canonical path", target: "/k8s/api/v1//namespaces",
			status: http.StatusBadRequest, page: []string{"canonical"}},
		{name: "not an API route", target: "/k8s/healthz",
			status: http.StatusNotFound, page: []string{"NotFound"}},
		{name: "unsupported subresource", target: "/k8s/api/v1/namespaces/a/pods/b/exec",
			status: http.StatusNotImplemented, page: []string{"not supported"}},
		{name: "upgrade", target: "/k8s/api/v1/namespaces", header: http.Header{"Upgrade": {"websocket"}, "Connection": {"Upgrade"}},
			status: http.StatusNotImplemented, page: []string{"not supported"}},
		{name: "upstream redirect", target: "/k8s/apis/aggregated.example.com/v1/things",
			upstream: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "https://elsewhere.example/a?b=c")
				w.WriteHeader(http.StatusFound)
			}, status: http.StatusBadGateway,
			page: []string{"https://elsewhere.example/a?b=c", `href="https://elsewhere.example/a?b=c"`}},
		{name: "upstream content held back", target: "/k8s/apis/aggregated.example.com/v1/things",
			upstream: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(w, "<script>steal()</script>")
			}, status: http.StatusBadGateway, page: []string{"text/html"}},
		{name: "a bound reached", target: "/k8s/api/v1/namespaces",
			upstream: holdingHandler, occupy: true, status: http.StatusTooManyRequests,
			page: []string{"Too many requests at once", "requests in flight"}},
	}
}

// foyer serves the case's upstream behind a proxy, with the session's one slot
// already taken when the case occupies it.
func (tc interruptionCase) foyer(t *testing.T) foyer {
	t.Helper()
	api := newAPIServer(t, tc.upstream)
	creds := tc.creds
	if creds == nil {
		creds = credentials{token: userToken}
	}
	o := frontOptions{}
	if tc.occupy {
		o.config = func(c *Config) { c.MaxSessionConcurrentRequests = 1 }
	}
	f := newFoyerWith(t, api, creds, o)
	if tc.occupy {
		f.open(t, "/k8s/api/v1/configmaps?watch=1")
	}
	return f
}

func (tc interruptionCase) send(t *testing.T, extra http.Header) answer {
	t.Helper()
	f := tc.foyer(t)
	header := http.Header{}
	for k, v := range tc.header {
		header[k] = v
	}
	for k, v := range extra {
		header[k] = v
	}
	return f.request(t, http.MethodGet, tc.target, nil, header)
}

// Every interruption has two forms with one status code: a Status for code, and a
// page for a person who opened the URL in a tab.
func TestInterruptionsHaveAPageForNavigations(t *testing.T) {
	for _, tc := range interruptionCases() {
		t.Run(tc.name, func(t *testing.T) {
			code := tc.send(t, nil)
			if code.StatusCode != tc.status {
				t.Fatalf("for code: %d, want %d", code.StatusCode, tc.status)
			}
			reason := readStatus(t, code).Reason

			page := tc.send(t, navigation)
			if page.StatusCode != tc.status {
				t.Fatalf("for a navigation: %d, want %d, the same as for code", page.StatusCode, tc.status)
			}
			assertPage(t, page)
			if got := page.Header.Values(interruption.Header); len(got) != 1 || got[0] != reason {
				t.Errorf("page %s = %q, want %q, the same as for code", interruption.Header, got, reason)
			}
			for _, want := range tc.page {
				if !strings.Contains(page.Body, want) {
					t.Errorf("the page does not say %q:\n%s", want, page.Body)
				}
			}
		})
	}
}

func assertPage(t *testing.T, resp answer) {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("page Content-Type = %q", ct)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src") ||
		!strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("page Content-Security-Policy = %q", csp)
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("page headers %v", resp.Header)
	}
	if strings.Contains(resp.Body, "<script") {
		t.Errorf("the page has a script:\n%s", resp.Body)
	}
	if strings.Contains(resp.Body, userToken) {
		t.Error("the page holds the token")
	}
}

// Only a browser navigation gets a page: a GET with exactly one Sec-Fetch-Dest
// saying document. A script's fetch, an iframe, any other method, or anything
// ambiguous gets the Status, so code always reads JSON.
func TestOnlyNavigationsGetAPage(t *testing.T) {
	noSession := interruptionCases()[0]
	for name, header := range map[string]http.Header{
		"fetch":              {"Sec-Fetch-Dest": {"empty"}, "Sec-Fetch-Mode": {"cors"}},
		"iframe":             {"Sec-Fetch-Dest": {"iframe"}, "Sec-Fetch-Mode": {"navigate"}},
		"script":             {"Sec-Fetch-Dest": {"script"}},
		"capitalized":        {"Sec-Fetch-Dest": {"Document"}},
		"document twice":     {"Sec-Fetch-Dest": {"document", "document"}},
		"document and empty": {"Sec-Fetch-Dest": {"document", "empty"}},
		"empty and document": {"Sec-Fetch-Dest": {"empty", "document"}},
		"joined":             {"Sec-Fetch-Dest": {"empty, document"}},
		"accept html only":   {"Accept": {"text/html"}},
		"no fetch metadata":  nil,
	} {
		t.Run(name, func(t *testing.T) {
			resp := noSession.send(t, header)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status %d", resp.StatusCode)
			}
			readStatus(t, resp)
		})
	}
	for _, method := range []string{http.MethodHead, http.MethodPost, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			api := newAPIServer(t, nil)
			f := newFoyer(t, api, credentials{refused: interruption.NotSignedIn()})
			resp := f.request(t, method, "/k8s/api/v1/namespaces", nil, navigation)
			if resp.StatusCode != http.StatusUnauthorized || strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
				t.Fatalf("%s navigation: %d %s", method, resp.StatusCode, resp.Header.Get("Content-Type"))
			}
		})
	}
}

// What the API server answers reaches the tab as the API server sent it. Only
// krm-foyer's own decisions become pages.
func TestKubernetesAnswersAreNeverReplaced(t *testing.T) {
	for _, code := range []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict,
		http.StatusUnprocessableEntity, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	} {
		body := `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"FromTheAPIServer","code":` + strconv.Itoa(code) + `}`
		api := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = io.WriteString(w, body)
		})
		f := newFoyer(t, api, credentials{token: userToken})
		resp := f.request(t, http.MethodGet, "/k8s/api/v1/namespaces/team-a/configmaps", nil, navigation)
		if resp.StatusCode != code || resp.Body != body || resp.Header.Get("Content-Type") != "application/json" {
			t.Errorf("API server's %d reached the tab as %d %q %q", code, resp.StatusCode, resp.Header.Get("Content-Type"), resp.Body)
		}
	}
}

// A redirect target is shown in full, and is a link only when it is an absolute
// http(s) URL: anything else is text, never something the page makes clickable.
func TestRedirectPageLinksOnlyToWebTargets(t *testing.T) {
	for target, link := range map[string]bool{
		"https://elsewhere.example/x":         true,
		"http://elsewhere.example/x":          true,
		"javascript:alert(document.cookie)":   false,
		"data:text/html,<script>x()</script>": false,
		"/apis/other/v1":                      false,
		"//elsewhere.example/x":               false,
		"https:elsewhere.example":             false,
	} {
		api := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", target)
			w.WriteHeader(http.StatusTemporaryRedirect)
		})
		f := newFoyer(t, api, credentials{token: userToken})
		resp := f.request(t, http.MethodGet, "/k8s/apis/aggregated.example.com/v1/things", nil, navigation)
		assertPage(t, resp)
		hrefs := strings.Count(resp.Body, "href=")
		// The stylesheet is the page's one other link.
		if got := hrefs > 1; got != link {
			t.Errorf("target %q: linked = %v, want %v:\n%s", target, got, link, resp.Body)
		}
		if strings.Contains(resp.Body, "javascript:") && strings.Contains(resp.Body, `href="javascript:`) {
			t.Errorf("target %q became a javascript link", target)
		}
		if !strings.Contains(resp.Body, "<code>"+escapeForCheck(target)+"</code>") {
			t.Errorf("target %q is not shown in full:\n%s", target, resp.Body)
		}
	}
}

// escapeForCheck is what html/template makes of text, for the characters these
// targets use.
func escapeForCheck(s string) string {
	return strings.NewReplacer("<", "&lt;", ">", "&gt;", "&", "&amp;", `"`, "&#34;", "'", "&#39;", "+", "&#43;").Replace(s)
}

// The sign-in link brings the person back to the URL they opened.
func TestSignInReturnsToTheURL(t *testing.T) {
	api := newAPIServer(t, nil)
	f := newFoyer(t, api, credentials{refused: interruption.NotSignedIn()})
	target := "/k8s/api/v1/namespaces/team-a/configmaps?watch=1&labelSelector=a%3Db"
	resp := f.request(t, http.MethodGet, target, nil, navigation)
	i := strings.Index(resp.Body, `href="/auth/login?return_to=`)
	if i < 0 {
		t.Fatalf("no sign-in link:\n%s", resp.Body)
	}
	href := resp.Body[i+len(`href="`):]
	href = strings.ReplaceAll(href[:strings.IndexByte(href, '"')], "&amp;", "&")
	u, err := url.Parse(href)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("return_to"); got != target {
		t.Errorf("return_to = %q, want %q", got, target)
	}
}

// Every interruption is logged with the path that was asked for, including the
// ones decided after the API server answered.
func TestInterruptionsAreLoggedWithThePath(t *testing.T) {
	for _, tc := range interruptionCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.foyer(t)
			f.request(t, http.MethodGet, tc.target, nil, tc.header)
			path, _, _ := strings.Cut(tc.target, "?")
			if logs := f.logs.String(); !strings.Contains(logs, `"msg":"interruption"`) || !strings.Contains(logs, `"path":"`+path+`"`) {
				t.Errorf("logged:\n%s", logs)
			}
		})
	}
}
