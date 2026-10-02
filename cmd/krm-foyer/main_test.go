package main

import (
	"encoding/pem"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ConfigButler/krm-foyer/internal/metrics"
)

// files is a fake file system for parseConfig.
func files(m map[string]string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		if v, ok := m[name]; ok {
			return []byte(v), nil
		}
		return nil, fs.ErrNotExist
	}
}

var loginArgs = []string{
	"-public-url", "https://foyer.example.test", "-oidc-issuer", "https://dex.example.test",
	"-oidc-client-id", "krm-foyer", "-oidc-client-secret-file", "/secret",
	"-kubernetes-server", "https://kubernetes.default.svc",
}

func TestParseConfigWithoutLogin(t *testing.T) {
	cfg, err := parseConfig(nil, files(nil), io.Discard)
	if err != nil || cfg.login != nil || cfg.listen != ":8080" || cfg.metricsListen != ":9090" {
		t.Fatalf("%+v, %v", cfg, err)
	}
}

func TestParseConfigWithLogin(t *testing.T) {
	cfg, err := parseConfig(append(loginArgs, "-oidc-scopes", "openid, email,groups"),
		files(map[string]string{"/secret": "s3cret\n"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	l := cfg.login
	if l == nil || l.auth.ClientSecret != "s3cret" || l.kubernetes.Server.Host != "kubernetes.default.svc" ||
		strings.Join(l.auth.Scopes, " ") != "openid email groups" || l.sessions.Origin != "https://foyer.example.test" ||
		l.sessions.IdleTimeout != time.Hour || l.sessions.AbsoluteTimeout != 8*time.Hour {
		t.Fatalf("%+v", l)
	}
}

// Sign-in and the proxy come together or not at all, and nothing half-configured
// starts.
func TestParseConfigRefuses(t *testing.T) {
	without := func(flag string) []string {
		var out []string
		for i := 0; i < len(loginArgs); i += 2 {
			if loginArgs[i] != flag {
				out = append(out, loginArgs[i], loginArgs[i+1])
			}
		}
		return out
	}
	secret := map[string]string{"/secret": "s3cret"}
	for name, tc := range map[string]struct {
		args  []string
		files map[string]string
		want  string
	}{
		"no public URL":       {without("-public-url"), secret, "-public-url"},
		"no issuer":           {without("-oidc-issuer"), secret, "-oidc-issuer"},
		"no client ID":        {without("-oidc-client-id"), secret, "-oidc-client-id"},
		"no secret file":      {without("-oidc-client-secret-file"), secret, "-oidc-client-secret-file"},
		"no API server":       {without("-kubernetes-server"), secret, "-kubernetes-server"},
		"secret file missing": {loginArgs, nil, "client secret"},
		"secret file empty":   {loginArgs, map[string]string{"/secret": "\n"}, "empty"},
		"only a cert":         {[]string{"-tls-cert-file", "/c"}, nil, "-tls-key-file"},
		"only a key":          {[]string{"-tls-key-file", "/k"}, nil, "-tls-cert-file"},
		"CA without login":    {[]string{"-kubernetes-ca-file", "/ca"}, nil, "sign-in flags"},
		"CA not PEM":          {append(loginArgs, "-kubernetes-ca-file", "/ca"), map[string]string{"/secret": "s", "/ca": "nope"}, "no PEM"},
		"stray argument":      {[]string{"serve"}, nil, "unexpected"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseConfig(tc.args, files(tc.files), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// Without login there is no /k8s and no /auth: the start page and probes only.
func TestHandlerWithoutLogin(t *testing.T) {
	h, _, err := handler(config{}, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]int{"/": 200, "/healthz": 200, "/readyz": 200, "/auth/login": 404, "/k8s/api": 404} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if w.Code != want {
			t.Errorf("GET %s = %d, want %d", path, w.Code, want)
		}
	}
}

// With login configured, the binary's /k8s has one credential source, the session.
// Without one it answers 401 and the API server is never contacted, whatever the
// browser sends. The issuer here is unreachable, so the pod is not ready either.
func TestHandlerWithLoginNeedsASession(t *testing.T) {
	var reached atomic.Int32
	api := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	t.Cleanup(api.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw})
	args := append([]string{}, loginArgs...)
	args[len(args)-1] = api.URL
	args = append(args, "-kubernetes-ca-file", "/ca", "-oidc-issuer", "https://127.0.0.1:1")
	cfg, err := parseConfig(args, files(map[string]string{"/secret": "s", "/ca": string(ca)}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	h, _, err := handler(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, header := range []http.Header{
		nil,
		{"Authorization": {"Bearer from-the-browser"}},
		{"Cookie": {"__Host-krm-foyer-session=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}},
		{"Impersonate-User": {"system:admin"}},
	} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/k8s/api/v1/namespaces", nil)
		r.Header = header.Clone()
		if r.Header == nil {
			r.Header = http.Header{}
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("with %v: %d, want 401", header, w.Code)
		}
	}
	if n := reached.Load(); n != 0 {
		t.Fatalf("the API server was reached %d times without a session", n)
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d before the issuer was discovered", w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/login", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("/auth/login = %d before the issuer was discovered", w.Code)
	}
}

// The 401 page's sign-in link is one login accepts: the proxy writes it, login
// checks it, and the two must agree on what a return path is.
func TestSignInLinkIsAcceptedByLogin(t *testing.T) {
	cfg, err := parseConfig(loginArgs, files(map[string]string{"/secret": "s"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	h, _, err := handler(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"/k8s/api/v1/namespaces",
		"/k8s/apis/apps/v1/namespaces/team-a/deployments?labelSelector=app%3Dweb&limit=10",
		"/k8s/api/v1/namespaces/team-a/pods/web-0/log?container=app&follow=true",
	} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		r.Header.Set("Sec-Fetch-Dest", "document")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		body := w.Body.String()
		i := strings.Index(body, `href="/auth/login?`)
		if w.Code != http.StatusUnauthorized || i < 0 {
			t.Fatalf("%s: %d without a sign-in link:\n%s", target, w.Code, body)
		}
		link := body[i+len(`href="`):]
		link = strings.ReplaceAll(link[:strings.IndexByte(link, '"')], "&amp;", "&")

		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, link, nil))
		// Not yet discovered, so 503; a return path login refused would be 400.
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "issuer-unavailable") {
			t.Errorf("%s: login answered %d to its sign-in link %s", target, w.Code, link)
		}
	}
}

// Metrics are served on their own listener and nowhere on the origin, where any page
// could read them, and an interruption in the wired binary is counted there.
func TestMetricsAreServedApartAndCount(t *testing.T) {
	cfg, err := parseConfig(loginArgs, files(map[string]string{"/secret": "s"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	h, _, err := handler(cfg, slog.New(slog.DiscardHandler), m)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/k8s/api/v1/namespaces", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("/k8s without a session = %d", w.Code)
	}
	for _, path := range []string{"/metrics", "/k8s/metrics", "/_foyer/metrics"} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if strings.Contains(w.Body.String(), "krm_foyer_") {
			t.Errorf("the origin serves metrics at %s", path)
		}
	}

	mh := metricsHandler(m)
	w = httptest.NewRecorder()
	mh.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if want := `krm_foyer_interruptions_total{reason="Unauthorized"} 1`; !strings.Contains(w.Body.String(), want) {
		t.Errorf("metrics lack %q:\n%s", want, w.Body.String())
	}
	w = httptest.NewRecorder()
	mh.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("the metrics listener serves / with %d", w.Code)
	}
}
