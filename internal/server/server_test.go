package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ConfigButler/krm-stream/gateway"
)

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	New(Config{Version: "v0.0.0-test"}).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	return rec
}

func TestProbes(t *testing.T) {
	for _, path := range []string{"/healthz", "/readyz"} {
		rec := get(t, path)
		if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
			t.Errorf("GET %s = %d %q, want 200 \"ok\\n\"", path, rec.Code, rec.Body.String())
		}
	}
}

func TestStartPage(t *testing.T) {
	rec := get(t, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "v0.0.0-test") {
		t.Error("start page does not show the version")
	}
}

func TestStylesheet(t *testing.T) {
	rec := get(t, "/_foyer/style.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /_foyer/style.css = %d, want 200", rec.Code)
	}
}

// The browser helper is a module script: browsers refuse to run one served with any
// type but JavaScript, and nosniff stops them guessing.
func TestBrowserHelper(t *testing.T) {
	rec := get(t, "/_foyer/foyer.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /_foyer/foyer.js = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("Content-Type = %q, want text/javascript", ct)
	}
	if !strings.Contains(rec.Body.String(), "export async function k8s(") {
		t.Errorf("the helper does not export k8s")
	}
}

// The start page answers "/" only. Anything else must not fall through to it,
// or a mistyped API path would get HTML instead of an error.
func TestUnknownPathIsNotFound(t *testing.T) {
	for _, path := range []string{"/nope", "/k8s/api/v1/pods", "/_foyer/"} {
		if rec := get(t, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	for _, path := range []string{"/", "/healthz", "/nope"} {
		rec := get(t, path)
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("GET %s: Content-Security-Policy = %q", path, csp)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s: missing X-Content-Type-Options", path)
		}
	}
}

// /k8s in any spelling reaches the API handler, which refuses what is not
// canonical. ServeMux must not get the chance to redirect it to a cleaned path.
func TestKubernetesRoutesReachTheProxy(t *testing.T) {
	reached := ""
	h := New(Config{Version: "test", Kubernetes: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = r.RequestURI
	})})
	for _, target := range []string{
		"/k8s", "/k8s/", "/k8s/api/v1/pods?watch=1", "//k8s/api", "/k8s/../healthz",
		"/k8s/api/../../healthz", "/%6B8s/api", "/x/../k8s/api", "/k8s%2Fapi",
	} {
		reached = ""
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil))
		if reached != target {
			t.Errorf("GET %s did not reach the API handler (status %d, Location %q)", target, rec.Code, rec.Header().Get("Location"))
		}
	}
	for _, target := range []string{"/k8sx", "/k8s-api/x", "/healthz", "/"} {
		reached = ""
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil))
		if reached != "" {
			t.Errorf("GET %s reached the API handler", target)
		}
	}
}

// Liveness never waits for the issuer; readiness does.
func TestReadiness(t *testing.T) {
	ready := false
	h := New(Config{Version: "test", Ready: func() bool { return ready }})
	for _, tc := range []struct {
		ready bool
		path  string
		code  int
	}{
		{false, "/healthz", http.StatusOK},
		{false, "/readyz", http.StatusServiceUnavailable},
		{true, "/readyz", http.StatusOK},
	} {
		ready = tc.ready
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.path, nil))
		if rec.Code != tc.code {
			t.Errorf("ready=%v GET %s = %d, want %d", tc.ready, tc.path, rec.Code, tc.code)
		}
	}
}

// /auth/ belongs to the login handler, every method and path under it; without
// one it does not exist.
func TestAuthRoutes(t *testing.T) {
	var reached []string
	h := New(Config{Version: "test", Auth: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.Method+" "+r.URL.Path)
	})})
	for _, target := range []string{"/auth/login", "/auth/callback?code=x", "/auth/session"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil))
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auth/logout", nil))
	if got := strings.Join(reached, ", "); got != "GET /auth/login, GET /auth/callback, GET /auth/session, POST /auth/logout" {
		t.Errorf("reached %s", got)
	}
	if rec := get(t, "/auth/login"); rec.Code != http.StatusNotFound {
		t.Errorf("without login, /auth/login = %d", rec.Code)
	}
	// The start page says whether sign-in is on, and nothing about how.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), `href="/auth/login"`) {
		t.Error("the start page does not offer sign-in")
	}
	if strings.Contains(get(t, "/").Body.String(), `href="/auth/login"`) {
		t.Error("the start page offers sign-in without login configured")
	}
}

// /stream/v1 reaches the stream with any method, so that it answers the ones it
// refuses itself; no other path under /stream does.
func TestStreamRoute(t *testing.T) {
	var reached []string
	h := New(Config{Version: "test", Stream: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.Method+" "+r.URL.Path)
	})})
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), method, "/stream/v1?resource=notes&version=v1", nil))
	}
	for _, target := range []string{"/stream", "/stream/", "/stream/v1/", "/stream/v2"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", target, rec.Code)
		}
	}
	if got := strings.Join(reached, ", "); got != "GET /stream/v1, POST /stream/v1" {
		t.Errorf("reached %s", got)
	}
	if rec := get(t, "/stream/v1"); rec.Code != http.StatusNotFound {
		t.Errorf("without streams, /stream/v1 = %d", rec.Code)
	}
}

// A stream gets a response writer it can flush and give write deadlines, over
// HTTP/1.1 and HTTP/2, through every route krm-foyer puts in front of it. The write
// timeout that bounds a stalled browser, and with it a shared stream's recheck,
// depends on both; a wrapper that hid them would end every stream at once.
func TestStreamsCanFlushAndSetWriteDeadlines(t *testing.T) {
	got := make(chan error, 1)
	h := New(Config{Version: "test", Kubernetes: http.NotFoundHandler(), Stream: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		got <- gateway.CheckHTTPStreaming(w)
	})})
	h1 := httptest.NewServer(h)
	t.Cleanup(h1.Close)
	h2 := httptest.NewUnstartedServer(h)
	h2.EnableHTTP2 = true
	h2.StartTLS()
	t.Cleanup(h2.Close)

	for name, srv := range map[string]*httptest.Server{"HTTP/1.1": h1, "HTTP/2": h2} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/stream/v1?resource=notes&version=v1", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if want := name[len("HTTP/")] - '0'; resp.ProtoMajor != int(want) {
			t.Errorf("%s: served over %s", name, resp.Proto)
		}
		if err := <-got; err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
