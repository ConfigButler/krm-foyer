package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ConfigButler/krm-foyer/internal/interruption"
)

const userToken = "user-token-4f1c9a" //nolint:gosec // a marker to search for, not a credential

// credentials is the test's session: it hands the proxy a fixed answer. It exists
// only in this test file; the binary has no way to be given a token but a session.
type credentials struct {
	token   string
	refused *interruption.Interruption
}

func (c credentials) Token(*http.Request) (string, *interruption.Interruption) {
	return c.token, c.refused
}

// apiServer stands in for the API server and records what reached it.
type apiServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*http.Request
}

func (a *apiServer) received() []*http.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*http.Request(nil), a.requests...)
}

func newAPIServer(t *testing.T, handler http.HandlerFunc) *apiServer {
	t.Helper()
	return newAPIServerWith(t, false, handler)
}

// newAPIServerWith serves HTTP/2 as well when http2 is set, as real API servers do.
func newAPIServerWith(t *testing.T, http2 bool, handler http.HandlerFunc) *apiServer {
	t.Helper()
	a := &apiServer{}
	a.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.requests = append(a.requests, r.Clone(context.Background()))
		a.mu.Unlock()
		if handler == nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"kind":"ConfigMapList"}`)
			return
		}
		handler(w, r)
	}))
	a.EnableHTTP2 = http2
	a.StartTLS()
	t.Cleanup(a.Close)
	return a
}

// protocols are the ways krm-foyer may reach the API server.
var protocols = map[string]bool{"HTTP/1.1": false, "HTTP/2": true}

func assertProtocol(t *testing.T, api *apiServer, http2 bool) {
	t.Helper()
	for _, r := range api.received() {
		if (r.ProtoMajor == 2) != http2 {
			t.Fatalf("the API server was reached over %s", r.Proto)
		}
	}
}

type foyer struct {
	url  string
	logs *bytes.Buffer
}

// newFoyer serves a proxy to api with the given credentials, on a real listener so
// that request targets arrive as bytes on the wire.
func newFoyer(t *testing.T, api *apiServer, creds Credentials) foyer {
	t.Helper()
	server, err := url.Parse(api.URL)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(api.Certificate())
	logs := &bytes.Buffer{}
	p, err := New(Config{
		Server: server, RootCAs: roots, Credentials: creds,
		Logger: slog.New(slog.NewJSONHandler(&lockedWriter{w: logs}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	return foyer{url: front.URL, logs: logs}
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// answer is a response from krm-foyer, read to the end.
type answer struct {
	StatusCode int
	Header     http.Header
	Trailer    http.Header
	Body       string
}

// request sends target (path and query, exactly these bytes) to krm-foyer.
func (f foyer) request(t *testing.T, method, target string, body io.Reader, header http.Header) answer {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, f.url, body)
	if err != nil {
		t.Fatal(err)
	}
	pathPart, query, _ := strings.Cut(target, "?")
	req.URL.Opaque = pathPart // sent as is, never cleaned or re-encoded
	req.URL.RawQuery = query
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return answer{StatusCode: resp.StatusCode, Header: resp.Header, Trailer: resp.Trailer, Body: string(b)}
}

func (f foyer) get(t *testing.T, target string) answer {
	t.Helper()
	return f.request(t, http.MethodGet, target, nil, nil)
}

// status is a Kubernetes metav1.Status as a client reads it.
type status struct {
	Kind, APIVersion, Status, Message, Reason string
	Code                                      int
	Details                                   *struct {
		Causes []struct{ Reason, Message, Field string }
	}
}

// readStatus reads an interruption and checks that it is a Kubernetes Status with
// the response's own code.
func readStatus(t *testing.T, resp answer) status {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("interruption Content-Type = %q, want application/json", ct)
	}
	var s status
	if err := json.Unmarshal([]byte(resp.Body), &s); err != nil {
		t.Fatal(err)
	}
	if s.Kind != "Status" || s.APIVersion != "v1" || s.Status != "Failure" || s.Code != resp.StatusCode {
		t.Fatalf("interruption is not a Status for %d: %+v", resp.StatusCode, s)
	}
	if got := resp.Header.Values(interruption.Header); len(got) != 1 || got[0] != s.Reason {
		t.Fatalf("interruption %s = %q, want %q", interruption.Header, got, s.Reason)
	}
	return s
}

// The path and query reach the API server byte-for-byte as the browser sent them,
// minus /k8s.
func TestForwardsPathAndQueryUnchanged(t *testing.T) {
	for name, http2 := range protocols {
		t.Run(name, func(t *testing.T) { testForwardsPathAndQueryUnchanged(t, http2) })
	}
}

func testForwardsPathAndQueryUnchanged(t *testing.T, http2 bool) {
	api := newAPIServerWith(t, http2, nil)
	f := newFoyer(t, api, credentials{token: userToken})
	targets := []string{
		"/api/v1/namespaces/team-a/configmaps?labelSelector=app%3Dweb&limit=50",
		"/apis/rbac.authorization.k8s.io/v1/clusterroles/system:aggregate-to-admin",
		"/apis/rbac.authorization.k8s.io/v1/clusterroles/system%3Aaggregate-to-admin",
		"/api/v1/namespaces/team-a/configmaps/a%20b?fieldSelector=metadata.name%3Da%20b&x=1;y=2",
		"/api/v1/namespaces/team-a/configmaps?watch=1&resourceVersion=10&allowWatchBookmarks=true",
		"/version",
	}
	for _, target := range targets {
		if resp := f.get(t, "/k8s"+target); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /k8s%s = %d", target, resp.StatusCode)
		}
	}
	assertProtocol(t, api, http2)
	got := api.received()
	for i, target := range targets {
		if got[i].RequestURI != target {
			t.Errorf("API server received %q, want %q", got[i].RequestURI, target)
		}
	}
}

// The API server sees the user's token and nothing the browser sent to stand in
// for it: no Authorization of its own, no impersonation, no cookie, no forwarding
// headers. The headers Kubernetes needs to answer pass.
func TestSendsOnlyTheUsersToken(t *testing.T) {
	for name, http2 := range protocols {
		t.Run(name, func(t *testing.T) { testSendsOnlyTheUsersToken(t, http2) })
	}
}

func testSendsOnlyTheUsersToken(t *testing.T, http2 bool) {
	api := newAPIServerWith(t, http2, nil)
	f := newFoyer(t, api, credentials{token: userToken})
	browser := http.Header{
		"Authorization":         {"Bearer browser-supplied"},
		"Impersonate-User":      {"system:admin"},
		"Impersonate-Group":     {"system:masters"},
		"Impersonate-Uid":       {"0"},
		"Impersonate-Extra-Foo": {"bar"},
		"Cookie":                {"krm-foyer-session=s3cr3t"},
		"X-Forwarded-For":       {"10.0.0.1"},
		"X-Forwarded-Host":      {"evil.example"},
		"X-Forwarded-Proto":     {"http"},
		"X-Forwarded-User":      {"admin"},
		"Forwarded":             {"for=10.0.0.1"},
		"X-Real-Ip":             {"10.0.0.1"},
		"X-Remote-User":         {"admin"},
		"X-Remote-Group":        {"system:masters"},
		"Accept-Encoding":       {"br"},
		"Idempotency-Key":       {"1"},
		"Accept":                {"application/json;as=Table;g=meta.k8s.io;v=v1"},
		"Content-Type":          {"application/apply-patch+yaml"},
		"User-Agent":            {"krm-foyer-test/1"},
	}
	resp := f.request(t, http.MethodPatch, "/k8s/api/v1/namespaces/team-a/configmaps/c?fieldManager=web",
		strings.NewReader("data: {}"), browser)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d", resp.StatusCode)
	}

	assertProtocol(t, api, http2)
	got := api.received()[0].Header
	if v := got.Values("Authorization"); len(v) != 1 || v[0] != "Bearer "+userToken {
		t.Errorf("Authorization = %q, want only the user's token", v)
	}
	for k := range got {
		if strings.HasPrefix(k, "Impersonate-") || strings.HasPrefix(k, "X-Forwarded-") || strings.HasPrefix(k, "X-Remote-") {
			t.Errorf("browser header %s reached the API server", k)
		}
	}
	for _, k := range []string{"Cookie", "Forwarded", "X-Real-Ip", "Idempotency-Key"} {
		if v := got.Values(k); len(v) > 0 {
			t.Errorf("browser header %s reached the API server: %q", k, v)
		}
	}
	if v := got.Get("Accept-Encoding"); v != "gzip" {
		t.Errorf("Accept-Encoding = %q, want gzip from krm-foyer's transport, not the browser's", v)
	}
	for _, k := range []string{"Accept", "Content-Type", "User-Agent"} {
		if got.Get(k) != browser.Get(k) {
			t.Errorf("%s = %q, want %q", k, got.Get(k), browser.Get(k))
		}
	}
}

// Without a usable credential the request never reaches the API server: no
// anonymous request, and no other identity in its place. What the credential source
// answers instead is given as it is, even alongside a token.
var (
	csrfRefusal = &interruption.Interruption{Status: http.StatusForbidden, Reason: "CSRFProofRequired", Message: "no CSRF proof"}
	unavailable = &interruption.Interruption{Status: http.StatusServiceUnavailable, Reason: "ServiceUnavailable", Message: "the session could not be checked; try again later"}
)

func TestNoCredentialNeverReachesTheAPIServer(t *testing.T) {
	for name, tc := range map[string]struct {
		creds Credentials
		code  int
	}{
		"not signed in":           {credentials{refused: interruption.NotSignedIn()}, http.StatusUnauthorized},
		"empty token":             {credentials{}, http.StatusUnauthorized},
		"session unavailable":     {credentials{refused: unavailable}, http.StatusServiceUnavailable},
		"refused by the session":  {credentials{refused: csrfRefusal}, http.StatusForbidden},
		"token and a refusal":     {credentials{token: userToken, refused: csrfRefusal}, http.StatusForbidden},
		"token and not signed in": {credentials{token: userToken, refused: interruption.NotSignedIn()}, http.StatusUnauthorized},
	} {
		t.Run(name, func(t *testing.T) {
			api := newAPIServer(t, nil)
			f := newFoyer(t, api, tc.creds)
			resp := f.request(t, http.MethodGet, "/k8s/api/v1/namespaces", nil,
				http.Header{"Authorization": {"Bearer browser-supplied"}})
			if resp.StatusCode != tc.code {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.code)
			}
			readStatus(t, resp)
			if n := len(api.received()); n != 0 {
				t.Errorf("%d requests reached the API server", n)
			}
		})
	}
}

// Requests krm-foyer refuses are answered without contacting the API server.
func TestRefusedRequestsNeverReachTheAPIServer(t *testing.T) {
	for _, tc := range []struct {
		target string
		header http.Header
		code   int
	}{
		{"/k8s/api/v1/namespaces/team-a/configmaps/../secrets", nil, http.StatusBadRequest},
		{"/k8s/api/v1/namespaces/team-a%2Fsecrets", nil, http.StatusBadRequest},
		{"/k8s//api/v1/secrets", nil, http.StatusBadRequest},
		{"/k8s/healthz", nil, http.StatusNotFound},
		{"/k8s", nil, http.StatusNotFound},
		{"/k8s/api/v1/namespaces/team-a/pods/web-0/exec?command=sh", nil, http.StatusNotImplemented},
		{"/k8s/api/v1/namespaces/team-a/services/web/proxy", nil, http.StatusNotImplemented},
		{"/k8s/api/v1/namespaces/team-a/configmaps?watch=1",
			http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}}, http.StatusNotImplemented},
	} {
		api := newAPIServer(t, nil)
		f := newFoyer(t, api, credentials{token: userToken})
		resp := f.request(t, http.MethodGet, tc.target, nil, tc.header)
		if resp.StatusCode != tc.code {
			t.Errorf("GET %s = %d, want %d", tc.target, resp.StatusCode, tc.code)
		}
		readStatus(t, resp)
		if n := len(api.received()); n != 0 {
			t.Errorf("GET %s: %d requests reached the API server", tc.target, n)
		}
	}
}

// Answers from the API server, refusals included, reach the browser unchanged:
// status, body, content type and the Kubernetes headers.
func TestPassesTheAPIServersAnswer(t *testing.T) {
	for _, code := range []int{200, 201, 400, 403, 404, 409, 422, 429, 500} {
		body := `{"kind":"Status","apiVersion":"v1","code":` + http.StatusText(code) + `}`
		api := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Audit-Id", "a1")
			w.Header().Set("Warning", `299 - "deprecated"`)
			w.Header().Set("Retry-After", "1")
			w.Header().Set("X-Kubernetes-Pf-Flowschema-Uid", "fs")
			w.Header().Set("X-Kubernetes-Pf-Prioritylevel-Uid", "pl")
			w.WriteHeader(code)
			_, _ = io.WriteString(w, body)
		})
		f := newFoyer(t, api, credentials{token: userToken})
		resp := f.request(t, http.MethodPost, "/k8s/api/v1/namespaces/team-a/configmaps", strings.NewReader("{}"), nil)
		if resp.StatusCode != code {
			t.Errorf("status = %d, want %d", resp.StatusCode, code)
		}
		if got := resp.Body; got != body {
			t.Errorf("%d: body = %q, want %q", code, got, body)
		}
		for k, v := range map[string]string{
			"Content-Type": "application/json", "Audit-Id": "a1", "Warning": `299 - "deprecated"`, "Retry-After": "1",
			"X-Kubernetes-Pf-Flowschema-Uid": "fs", "X-Kubernetes-Pf-Prioritylevel-Uid": "pl",
		} {
			if got := resp.Header.Get(k); got != v {
				t.Errorf("%d: %s = %q, want %q", code, k, got, v)
			}
		}
	}
}

// Upstream headers outside the allowlist never reach the browser, and every
// proxied response carries krm-foyer's own security headers instead.
func TestDropsUpstreamHeaders(t *testing.T) {
	api := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Set-Cookie", "krm-foyer-session=planted; Path=/")
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Credentials", "true")
		h.Set("Cache-Control", "public, max-age=3600")
		h.Set("Content-Security-Policy", "script-src *")
		h.Set("X-Content-Type-Options", "")
		h.Set("Location", "https://evil.example/")
		h.Set("Refresh", "0; url=https://evil.example/")
		h.Set("Link", "<https://evil.example/x.js>; rel=preload")
		h.Set("Clear-Site-Data", `"cookies"`)
		_, _ = io.WriteString(w, "{}")
	})
	f := newFoyer(t, api, credentials{token: userToken})
	resp := f.get(t, "/k8s/apis/metrics.k8s.io/v1beta1/pods")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	allowed := map[string]bool{"Content-Type": true, "Content-Length": true, "Date": true,
		"X-Content-Type-Options": true, "Content-Security-Policy": true, "Cache-Control": true, "Referrer-Policy": true}
	for k := range resp.Header {
		if !allowed[k] {
			t.Errorf("upstream header %s reached the browser: %q", k, resp.Header.Values(k))
		}
	}
	assertSecurityHeaders(t, resp)
}

// An upstream cannot pass its answer off as krm-foyer's: the helper resends a change
// only when krm-foyer refused it, and knows that by the interruption header. An
// aggregated API answering exactly like a CSRF refusal still gets its answer through,
// as its own.
func TestUpstreamCannotClaimAnInterruption(t *testing.T) {
	body := `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"CSRFProofRequired","code":403}`
	api := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header()[interruption.Header] = []string{"CSRFProofRequired"}
		w.Header()["krm-foyer-interruption"] = []string{"CSRFProofRequired"}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, body)
	})
	f := newFoyer(t, api, credentials{token: userToken})
	resp := f.request(t, http.MethodPost, "/k8s/apis/aggregated.example.com/v1/things", strings.NewReader("{}"), nil)
	if resp.StatusCode != http.StatusForbidden || resp.Body != body {
		t.Fatalf("%d %q, want the API server's answer", resp.StatusCode, resp.Body)
	}
	if got := resp.Header.Values(interruption.Header); len(got) != 0 {
		t.Errorf("the API server's %s reached the browser: %q", interruption.Header, got)
	}
}

func assertSecurityHeaders(t *testing.T, resp answer) {
	t.Helper()
	for k, v := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; sandbox",
		"Cache-Control":           "no-store",
	} {
		if got := resp.Header.Values(k); len(got) != 1 || got[0] != v {
			t.Errorf("%s = %q, want exactly %q", k, got, v)
		}
	}
}

// A redirect is neither followed nor passed on: the browser gets a 502 Status
// naming the target, and no Location header.
func TestRedirectsAreNotFollowedOrPassedOn(t *testing.T) {
	for _, code := range []int{301, 302, 303, 307, 308} {
		api := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://evil.example/login")
			w.WriteHeader(code)
		})
		f := newFoyer(t, api, credentials{token: userToken})
		resp := f.get(t, "/k8s/apis/aggregated.example.com/v1/things")
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("%d: status = %d, want 502", code, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "" {
			t.Errorf("%d: Location %q reached the browser", code, loc)
		}
		assertSecurityHeaders(t, resp)
		s := readStatus(t, resp)
		if s.Details == nil || len(s.Details.Causes) != 1 || s.Details.Causes[0].Message != "https://evil.example/login" {
			t.Errorf("%d: Status does not name the target: %+v", code, s.Details)
		}
		if n := len(api.received()); n != 1 {
			t.Errorf("%d: the redirect was followed (%d upstream requests)", code, n)
		}
	}
}

// Content the browser could run or sniff is held back: only the API's own media
// types and text/plain for logs pass.
func TestHoldsBackOtherContent(t *testing.T) {
	const page = "<script>steal()</script>"
	for _, tc := range []struct {
		contentType string
		body        string
		code        int
	}{
		{"text/html; charset=utf-8", page, http.StatusBadGateway},
		{"application/xhtml+xml", page, http.StatusBadGateway},
		{"image/svg+xml", page, http.StatusBadGateway},
		{"application/javascript", page, http.StatusBadGateway},
		{"text/html, application/json", page, http.StatusBadGateway},
		{"", page, http.StatusBadGateway},
		{"application/json", "{}", http.StatusOK},
		{"application/json;stream=watch", "{}", http.StatusOK},
		{"application/vnd.kubernetes.protobuf;stream=watch", "k8s", http.StatusOK},
		{"application/yaml", "a: b", http.StatusOK},
		{"text/plain; charset=utf-8", "log line", http.StatusOK},
		{"", "", http.StatusOK},
	} {
		api := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header()["Content-Type"] = []string{tc.contentType}
			if tc.contentType == "" {
				w.Header()["Content-Type"] = nil // keep Go from sniffing one
			}
			_, _ = io.WriteString(w, tc.body)
		})
		f := newFoyer(t, api, credentials{token: userToken})
		resp := f.get(t, "/k8s/api/v1/namespaces/team-a/pods/web-0/log")
		if resp.StatusCode != tc.code {
			t.Errorf("Content-Type %q: status = %d, want %d", tc.contentType, resp.StatusCode, tc.code)
		}
		body := resp.Body
		if tc.code == http.StatusBadGateway && strings.Contains(body, page) {
			t.Errorf("Content-Type %q: the held-back body reached the browser", tc.contentType)
		}
		assertSecurityHeaders(t, resp)
	}
}

// The browser receives bodies decoded: krm-foyer asks for gzip itself and
// decodes it, and an encoding it did not ask for is held back.
func TestBodiesReachTheBrowserDecoded(t *testing.T) {
	const body = `{"kind":"ConfigMapList","items":[]}`
	api := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("encoding") == "br" {
			w.Header().Set("Content-Encoding", "br")
			_, _ = io.WriteString(w, "not really brotli")
			return
		}
		// Compressed whatever was asked: only krm-foyer's transport asking for gzip
		// makes it decode this.
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = io.WriteString(gz, body)
		_ = gz.Close()
	})
	f := newFoyer(t, api, credentials{token: userToken})

	resp := f.request(t, http.MethodGet, "/k8s/api/v1/configmaps", nil, http.Header{"Accept-Encoding": {"gzip, br"}})
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q, want none", enc)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Body; got != body {
		t.Errorf("body = %q, want it decoded: %q", got, body)
	}

	resp = f.get(t, "/k8s/api/v1/configmaps?encoding=br")
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("unrequested Content-Encoding: status = %d, want 502", resp.StatusCode)
	}
	readStatus(t, resp)
}

// A watch streams: each event reaches the browser as the API server sends it, and
// when the browser goes away the upstream request is cancelled.
func TestWatchStreamsAndCancels(t *testing.T) {
	sent := make(chan struct{})
	cancelled := make(chan struct{})
	api := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"ADDED"}`+"\n")
		_ = http.NewResponseController(w).Flush() // the test notices a missing flush
		close(sent)
		<-r.Context().Done()
		close(cancelled)
	})
	f := newFoyer(t, api, credentials{token: userToken})

	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url+"/k8s/api/v1/configmaps?watch=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	<-sent
	line := make([]byte, len(`{"type":"ADDED"}`+"\n"))
	if _, err := io.ReadFull(resp.Body, line); err != nil {
		t.Fatalf("the first event did not arrive while the watch was open: %v", err)
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the browser's request did not cancel the upstream watch")
	}
}

// A mutation is sent once. When the API server drops the connection, the write may
// have committed, so krm-foyer answers 502 and never sends it again.
func TestMutationsAreNotReplayed(t *testing.T) {
	var mu sync.Mutex
	posts := 0
	api := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{}")
			return
		}
		mu.Lock()
		posts++
		mu.Unlock()
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			panic(err)
		}
		_ = conn.Close()
	})
	f := newFoyer(t, api, credentials{token: userToken})
	// A kept-alive connection first: the transport only retries on a reused one.
	f.get(t, "/k8s/api/v1/namespaces/team-a/configmaps")

	resp := f.request(t, http.MethodPost, "/k8s/api/v1/namespaces/team-a/configmaps", strings.NewReader("{}"),
		http.Header{"Idempotency-Key": {"1"}})
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	readStatus(t, resp)
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Errorf("the API server received the POST %d times, want once", posts)
	}
}

// Informational responses carry headers too, and would pass the allowlist by
// arriving before the response it checks. They are dropped.
func TestDropsInformationalResponses(t *testing.T) {
	api := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", "<https://evil.example/x.js>; rel=preload")
		w.Header().Set("Set-Cookie", "planted=1")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		w.Header().Del("Set-Cookie")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
	})
	f := newFoyer(t, api, credentials{token: userToken})

	var informational []int
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
		informational = append(informational, code)
		return nil
	}}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace), http.MethodGet,
		f.url+"/k8s/api/v1/configmaps", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || len(informational) != 0 {
		t.Errorf("status %d after informational responses %v, want 200 and none", resp.StatusCode, informational)
	}
	if c := resp.Header.Get("Set-Cookie"); c != "" {
		t.Errorf("Set-Cookie from an informational response reached the browser: %q", c)
	}
}

// Trailers are headers sent after the body; none reaches the browser.
func TestDropsTrailers(t *testing.T) {
	api := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Trailer", "Set-Cookie, X-Secret")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
		w.Header().Set("Set-Cookie", "planted=1")
		w.Header().Set("X-Secret", "s")
	})
	f := newFoyer(t, api, credentials{token: userToken})
	resp := f.get(t, "/k8s/api/v1/configmaps")
	if len(resp.Trailer) != 0 {
		t.Errorf("trailers reached the browser: %v", resp.Trailer)
	}
}

// When the API server cannot be reached, the browser gets a 502 Status.
func TestUnreachableAPIServer(t *testing.T) {
	api := newAPIServer(t, nil)
	f := newFoyer(t, api, credentials{token: userToken})
	api.Close()
	resp := f.get(t, "/k8s/api/v1/configmaps")
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	readStatus(t, resp)
}

// The upstream is pinned: a proxy from the environment is never used, and the API
// server's certificate is verified.
func TestUpstreamIsPinnedAndVerified(t *testing.T) {
	api := newAPIServer(t, nil)
	// Go never proxies loopback addresses, so setting HTTPS_PROXY here would prove
	// nothing; look at the transport instead.
	server, _ := url.Parse(api.URL)
	pinned, err := New(Config{Server: server, Credentials: credentials{token: userToken}})
	if err != nil {
		t.Fatal(err)
	}
	if tr, ok := pinned.transport.(*http.Transport); !ok || tr.Proxy != nil || tr.TLSClientConfig.InsecureSkipVerify {
		t.Errorf("transport is not pinned and verified: %#v", pinned.transport)
	}

	p, err := New(Config{Server: server, RootCAs: x509.NewCertPool(), Credentials: credentials{token: userToken}})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(p)
	defer front.Close()
	before := len(api.received())
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, front.URL+"/k8s/api/v1/configmaps", nil)
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway || len(api.received()) != before {
		t.Errorf("an untrusted certificate: status = %d, want 502 and no request served", resp.StatusCode)
	}
}

// The proxy only talks to an https API server it was given.
func TestNewRejectsBadConfig(t *testing.T) {
	creds := credentials{token: userToken}
	for _, raw := range []string{"http://api.example", "https://", "https://api.example/prefix",
		"https://api.example?x=1", "https://u:p@api.example"} {
		u, _ := url.Parse(raw)
		if _, err := New(Config{Server: u, Credentials: creds}); err == nil {
			t.Errorf("New accepted server %q", raw)
		}
	}
	u, _ := url.Parse("https://api.example")
	if _, err := New(Config{Server: u}); err == nil {
		t.Error("New accepted no credential source")
	}
	if _, err := New(Config{Credentials: creds}); err == nil {
		t.Error("New accepted no server")
	}
}

// The token never appears in a response or a log line, whatever the outcome.
func TestTokenNeverLeaves(t *testing.T) {
	api := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("answer") {
		case "redirect":
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(http.StatusFound)
		case "html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<p>hi</p>")
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"kind":"Status"}`)
		}
	})
	f := newFoyer(t, api, credentials{token: userToken})
	for _, target := range []string{
		"/k8s/api/v1/secrets", "/k8s/api/v1/secrets?answer=redirect", "/k8s/api/v1/secrets?answer=html",
		"/k8s/api//v1", "/k8s/api/v1/namespaces/a/pods/b/exec", "/k8s/metrics",
	} {
		resp := f.get(t, target)
		var dump bytes.Buffer
		_ = resp.Header.Write(&dump)
		dump.WriteString(resp.Body)
		if strings.Contains(dump.String(), userToken) {
			t.Errorf("GET %s: the token is in the response", target)
		}
	}
	if strings.Contains(f.logs.String(), userToken) {
		t.Error("the token is in krm-foyer's logs")
	}
	if !strings.Contains(f.logs.String(), `"msg":"interruption"`) {
		t.Error("interruptions are not logged")
	}
}

// Content-Type sent as more than one header field is ambiguous: a browser takes
// the last usable value, a check of the first would see another. Held back, in
// either order and even when the values agree.
func TestHoldsBackRepeatedContentType(t *testing.T) {
	for name, http2 := range protocols {
		t.Run(name, func(t *testing.T) { testHoldsBackRepeatedContentType(t, http2) })
	}
}

func testHoldsBackRepeatedContentType(t *testing.T, http2 bool) {
	const page = "<script>steal()</script>"
	for _, values := range [][]string{
		{"application/json", "text/html"},
		{"text/html", "application/json"},
		{"application/json", "application/json"},
		{"application/json", ""},
		{"", "text/html"},
	} {
		api := newAPIServerWith(t, http2, func(w http.ResponseWriter, _ *http.Request) {
			w.Header()["Content-Type"] = values
			_, _ = io.WriteString(w, page)
		})
		f := newFoyer(t, api, credentials{token: userToken})
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			resp := f.request(t, method, "/k8s/apis/aggregated.example.com/v1/things", nil, nil)
			if resp.StatusCode != http.StatusBadGateway {
				t.Errorf("%s with Content-Type fields %q: status = %d, want 502", method, values, resp.StatusCode)
			}
			if strings.Contains(resp.Body, page) {
				t.Errorf("%s with Content-Type fields %q: the body reached the browser", method, values)
			}
			if got := resp.Header.Values("Content-Type"); len(got) > 1 {
				t.Errorf("%s with Content-Type fields %q: browser got %q", method, values, got)
			}
		}
		assertProtocol(t, api, http2)
	}
}

// A HEAD response has no body, whatever Content-Length says: that describes the
// GET. A missing content type is fine there; a disallowed one is still held back,
// so HEAD and GET agree on what is refused.
func TestHeadResponses(t *testing.T) {
	for _, tc := range []struct {
		contentType []string
		code        int
	}{
		{nil, http.StatusOK},
		{[]string{"application/json"}, http.StatusOK},
		{[]string{"text/html"}, http.StatusBadGateway},
		{[]string{"application/json", "text/html"}, http.StatusBadGateway},
	} {
		api := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header()["Content-Type"] = tc.contentType
			w.Header().Set("Content-Length", "12")
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusOK)
				return
			}
			_, _ = io.WriteString(w, `{"kind":"x"}`)
		})
		f := newFoyer(t, api, credentials{token: userToken})
		resp := f.request(t, http.MethodHead, "/k8s/api/v1/namespaces/team-a/configmaps/c", nil, nil)
		if resp.StatusCode != tc.code {
			t.Errorf("HEAD with Content-Type %q: status = %d, want %d", tc.contentType, resp.StatusCode, tc.code)
		}
		if tc.code == http.StatusOK && resp.Header.Get("Content-Length") != "12" {
			t.Errorf("HEAD with Content-Type %q: Content-Length = %q, want the API server's 12",
				tc.contentType, resp.Header.Get("Content-Length"))
		}
		assertSecurityHeaders(t, resp)
		// The same answer without a content type has a body on GET, and is held back.
		if tc.contentType == nil {
			if get := f.get(t, "/k8s/api/v1/namespaces/team-a/configmaps/c"); get.StatusCode != http.StatusBadGateway {
				t.Errorf("GET of a body with no content type: status = %d, want 502", get.StatusCode)
			}
		}
	}
}

// An encoding hidden behind an empty first header field is still an encoding.
func TestHoldsBackRepeatedContentEncoding(t *testing.T) {
	api := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header()["Content-Encoding"] = []string{"", "br"}
		_, _ = io.WriteString(w, "not really brotli")
	})
	f := newFoyer(t, api, credentials{token: userToken})
	resp := f.get(t, "/k8s/api/v1/configmaps")
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("Content-Encoding fields [\"\" br]: status = %d, want 502", resp.StatusCode)
	}
	readStatus(t, resp)
}

// An upgrade request is refused whatever its first Upgrade field says.
func TestRefusesUpgradeInAnyField(t *testing.T) {
	api := newAPIServer(t, nil)
	f := newFoyer(t, api, credentials{token: userToken})
	resp := f.request(t, http.MethodGet, "/k8s/api/v1/configmaps?watch=1", nil,
		http.Header{"Connection": {"Upgrade"}, "Upgrade": {"", "websocket"}})
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("Upgrade fields [\"\" websocket]: status = %d, want 501", resp.StatusCode)
	}
	if n := len(api.received()); n != 0 {
		t.Errorf("%d requests reached the API server", n)
	}
}
