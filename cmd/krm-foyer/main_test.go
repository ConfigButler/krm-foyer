package main

import (
	"context"
	"encoding/pem"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	"-session-keys-file", "/keys", "-kubernetes-server", "https://kubernetes.default.svc",
}

// testSessionKeys is a session keys file with one key.
const testSessionKeys = "c2Vzc2lvbi1rZXktZm9yLWtybS1mb3llci10ZXN0cyE=\n" //nolint:gosec // a test key

func TestParseConfigWithoutLogin(t *testing.T) {
	cfg, err := parseConfig(nil, files(nil), io.Discard)
	if err != nil || cfg.login != nil || cfg.listen != ":8080" || cfg.metricsListen != ":9090" {
		t.Fatalf("%+v, %v", cfg, err)
	}
}

func TestParseConfigWithLogin(t *testing.T) {
	cfg, err := parseConfig(append(loginArgs, "-oidc-scopes", "openid, email,groups"),
		files(map[string]string{"/secret": "s3cret\n", "/keys": testSessionKeys}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	l := cfg.login
	if l == nil || l.auth.ClientSecret != "s3cret" || l.kubernetes.Server.Host != "kubernetes.default.svc" ||
		strings.Join(l.auth.Scopes, " ") != "openid email groups" || l.sessions.Origin != "https://foyer.example.test" ||
		l.sessions.Keys == nil || l.sessions.AbsoluteTimeout != 8*time.Hour ||
		l.gate.SessionCheckInterval != 5*time.Second || l.gate.MaxResponseDuration != 30*time.Minute ||
		l.gate.MaxSessionConcurrentRequests != 64 || l.gate.MaxConcurrentRequests != 2000 ||
		l.gate.MaxSessionStreams != 32 || l.gate.MaxStreams != 2000 ||
		l.gate.SessionRequestRate != 20 || l.gate.SessionRequestBurst != 100 ||
		l.kubernetes.MaxResponseBytes != 32<<20 || l.streamWrites != 10*time.Second {
		t.Fatalf("%+v", l)
	}
}

// The login configuration is optional, and read from its file when given.
func TestParseConfigLogin(t *testing.T) {
	cfg, err := parseConfig(loginArgs, files(map[string]string{"/secret": "s", "/keys": testSessionKeys}), io.Discard)
	if err != nil || cfg.login.auth.Login.AuthorizationParameters != nil {
		t.Fatalf("without the flag: %+v, %v", cfg.login.auth.Login, err)
	}
	cfg, err = parseConfig(append(loginArgs, "-login-config-file", "/login"), files(map[string]string{
		"/secret": "s", "/keys": testSessionKeys,
		"/login": `{"authorizationParameters":{"connector_id":{"default":"audience","allowFromRequest":true}},` +
			`"sessionClaims":{"connector":"/federated_claims/connector_id"}}`,
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if l := cfg.login.auth.Login; l.AuthorizationParameters["connector_id"].Default != "audience" ||
		l.SessionClaims.Connector != "/federated_claims/connector_id" {
		t.Fatalf("%+v", l)
	}
}

// Shared watches are off unless asked for, and take the resources and the token
// file they are given, with the recheck defaults.
func TestParseConfigSharedWatches(t *testing.T) {
	secret := files(map[string]string{"/secret": "s3cret", "/keys": testSessionKeys})
	cfg, err := parseConfig(loginArgs, secret, io.Discard)
	if err != nil || cfg.login.shared != nil {
		t.Fatalf("without the flags: %+v, %v; want no shared watches", cfg.login.shared, err)
	}
	cfg, err = parseConfig(append(loginArgs, "-shared-watch-resources", "notes.hello.krm-foyer.example, configmaps",
		"-shared-watch-token-file", "/token"), secret, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	sh := cfg.login.shared
	if sh == nil || sh.TokenFile != "/token" || len(sh.Resources) != 2 ||
		sh.Resources[0].Group != "hello.krm-foyer.example" || sh.Resources[0].Resource != "notes" ||
		sh.Resources[1].Group != "" || sh.Resources[1].Resource != "configmaps" ||
		sh.RecheckInterval != 30*time.Second || sh.DecisionTTL != 10*time.Second || sh.QPS != 100 {
		t.Fatalf("%+v", sh)
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
	secret := map[string]string{"/secret": "s3cret", "/keys": testSessionKeys}
	for name, tc := range map[string]struct {
		args  []string
		files map[string]string
		want  string
	}{
		"no public URL":           {without("-public-url"), secret, "-public-url"},
		"no issuer":               {without("-oidc-issuer"), secret, "-oidc-issuer"},
		"no client ID":            {without("-oidc-client-id"), secret, "-oidc-client-id"},
		"no secret file":          {without("-oidc-client-secret-file"), secret, "-oidc-client-secret-file"},
		"no API server":           {without("-kubernetes-server"), secret, "-kubernetes-server"},
		"no session keys file":    {without("-session-keys-file"), secret, "-session-keys-file"},
		"session keys missing":    {loginArgs, map[string]string{"/secret": "s"}, "reading the session keys"},
		"session keys empty":      {loginArgs, map[string]string{"/secret": "s", "/keys": "\n"}, "no session key"},
		"a session key too short": {loginArgs, map[string]string{"/secret": "s", "/keys": "c2hvcnQ="}, "-session-keys-file: line 1"},
		"the idle timeout of 0.1": {append(loginArgs, "-session-idle-timeout", "1h"), secret, "session-idle-timeout"},
		"login config missing":    {append(loginArgs, "-login-config-file", "/login"), secret, "reading the login configuration"},
		"login config misspelt": {append(loginArgs, "-login-config-file", "/login"),
			map[string]string{"/secret": "s", "/keys": testSessionKeys, "/login": "authorizationParameter: {}"}, "-login-config-file"},
		"login config reserved": {append(loginArgs, "-login-config-file", "/login"),
			map[string]string{"/secret": "s", "/keys": testSessionKeys, "/login": "authorizationParameters: {state: {allowFromRequest: true}}"}, "krm-foyer's own"},
		"login config, bad pointer": {append(loginArgs, "-login-config-file", "/login"),
			map[string]string{"/secret": "s", "/keys": testSessionKeys, "/login": "sessionClaims: {groups: groups}"}, "sessionClaims.groups"},
		"login config without login": {[]string{"-login-config-file", "/login"}, nil, "need the sign-in flags"},
		"no session timeout":         {append(loginArgs, "-session-absolute-timeout", "0s"), secret, "-session-absolute-timeout"},
		"secret file missing":        {loginArgs, nil, "client secret"},
		"secret file empty":          {loginArgs, map[string]string{"/secret": "\n", "/keys": testSessionKeys}, "empty"},
		"only a cert":                {[]string{"-tls-cert-file", "/c"}, nil, "-tls-key-file"},
		"only a key":                 {[]string{"-tls-key-file", "/k"}, nil, "-tls-cert-file"},
		"CA without login":           {[]string{"-kubernetes-ca-file", "/ca"}, nil, "sign-in flags"},
		"CA not PEM":                 {append(loginArgs, "-kubernetes-ca-file", "/ca"), map[string]string{"/secret": "s", "/keys": testSessionKeys, "/ca": "nope"}, "no PEM"},
		"stray argument":             {[]string{"serve"}, nil, "unexpected"},
		"no session checks":          {append(loginArgs, "-session-check-interval", "0s"), secret, "-session-check-interval"},
		"no response duration":       {append(loginArgs, "-max-response-duration", "0s"), secret, "-max-response-duration"},
		"no concurrency":             {append(loginArgs, "-max-concurrent-requests", "0"), secret, "-max-concurrent-requests"},
		"no session concurrency":     {append(loginArgs, "-max-session-concurrent-requests", "-1"), secret, "-max-session-concurrent-requests"},
		"no session streams":         {append(loginArgs, "-max-session-streams", "0"), secret, "-max-session-streams"},
		"no streams":                 {append(loginArgs, "-max-streams", "-1"), secret, "-max-streams"},
		"no request rate":            {append(loginArgs, "-session-request-rate", "0"), secret, "-session-request-rate"},
		"no request burst":           {append(loginArgs, "-session-request-burst", "0"), secret, "-session-request-burst"},
		"shared without token":       {append(loginArgs, "-shared-watch-resources", "configmaps"), secret, "go together"},
		"shared token alone":         {append(loginArgs, "-shared-watch-token-file", "/token"), secret, "go together"},
		"shared resource bad":        {append(loginArgs, "-shared-watch-resources", "apps/deployments", "-shared-watch-token-file", "/token"), secret, "-shared-watch-resources"},
		"shared names none":          {append(loginArgs, "-shared-watch-resources", ",", "-shared-watch-token-file", "/token"), secret, "names no resource"},
		"decisions outlive check": {append(loginArgs, "-shared-watch-resources", "configmaps", "-shared-watch-token-file", "/token",
			"-shared-watch-decision-ttl", "1m"), secret, "-shared-watch-decision-ttl"},
		"no stream write timeout": {append(loginArgs, "-stream-write-timeout", "0s"), secret, "-stream-write-timeout"},
		"no shared QPS": {append(loginArgs, "-shared-watch-resources", "configmaps", "-shared-watch-token-file", "/token",
			"-shared-watch-qps", "0"), secret, "-shared-watch-qps"},
		"shared without login": {[]string{"-shared-watch-resources", "configmaps"}, nil, "need the sign-in flags"},
		"no byte limit":        {append(loginArgs, "-max-response-bytes", "0"), secret, "-max-response-bytes"},
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
	cfg, err := parseConfig(args, files(map[string]string{"/secret": "s", "/keys": testSessionKeys, "/ca": string(ca)}), io.Discard)
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
	cfg, err := parseConfig(loginArgs, files(map[string]string{"/secret": "s", "/keys": testSessionKeys}), io.Discard)
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
	cfg, err := parseConfig(loginArgs, files(map[string]string{"/secret": "s", "/keys": testSessionKeys}), io.Discard)
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

// A metrics listener that cannot start stops krm-foyer, rather than leaving it
// running without the metrics it was configured to serve.
func TestRunFailsWhenMetricsCannotListen(t *testing.T) {
	taken, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	done := make(chan error, 1)
	go func() {
		done <- run(config{listen: "127.0.0.1:0", metricsListen: taken.Addr().String()}, slog.New(slog.DiscardHandler))
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "metrics") {
			t.Fatalf("run = %v, want an error naming the metrics listener", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("krm-foyer kept running without its metrics listener")
	}
}

// shutdownFixture is a server as run starts it, on a local port, and what it logs.
type shutdownFixture struct {
	url         string
	srv         *http.Server
	endRequests context.CancelFunc
	logger      *slog.Logger
	logs        logBuffer
}

func serveForShutdown(t *testing.T, h http.Handler) *shutdownFixture {
	t.Helper()
	f := &shutdownFixture{}
	f.logger = slog.New(slog.NewTextHandler(&f.logs, nil))
	f.srv, f.endRequests = newServer("", h, f.logger)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = f.srv.Serve(ln) }()
	t.Cleanup(func() { _ = f.srv.Close() })
	f.url = "http://" + ln.Addr().String()
	return f
}

func (f *shutdownFixture) shutdown(drain, timeout time.Duration) {
	shutdown(f.srv, f.endRequests, drain, timeout, f.logger)
}

// logBuffer is a strings.Builder that handlers on several goroutines may write to.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func get(url string) (*http.Response, error) {
	// Not the test's context: the server, not the client, is what ends these requests.
	r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(r)
}

// A stream never finishes by itself, so shutting down must end it: a rollout with
// a browser connected stops after the drain, cleanly, rather than waiting out the
// whole timeout and cutting the connection.
func TestShutdownEndsOpenStreams(t *testing.T) {
	opened, ended := make(chan struct{}), make(chan struct{})
	f := serveForShutdown(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(opened)
		<-r.Context().Done()
		close(ended)
	}))
	resp, err := get(f.url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	<-opened

	start := time.Now()
	f.shutdown(100*time.Millisecond, 5*time.Second)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("shutdown took %v with a stream open", took)
	}
	select {
	case <-ended:
	default:
		t.Error("the stream's request context was not ended")
	}
	if !strings.Contains(f.logs.String(), "shut down cleanly") {
		t.Errorf("shutdown was not clean:\n%s", f.logs.String())
	}
}

// A request in flight that finishes within the drain is not cut short: only what
// is still open after it is ended.
func TestShutdownLetsRequestsInFlightFinish(t *testing.T) {
	started := make(chan struct{})
	f := serveForShutdown(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-time.After(300 * time.Millisecond):
			_, _ = io.WriteString(w, "done")
		case <-r.Context().Done():
		}
	}))
	got := make(chan string, 1)
	go func() {
		resp, err := get(f.url)
		if err != nil {
			got <- err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		got <- string(b)
	}()
	<-started
	f.shutdown(2*time.Second, 5*time.Second)
	if body := <-got; body != "done" {
		t.Errorf("the request in flight got %q, want it to finish", body)
	}
}
