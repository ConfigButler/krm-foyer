package stream

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ConfigButler/krm-foyer/internal/gate"
	"github.com/ConfigButler/krm-foyer/internal/interruption"
	"github.com/ConfigButler/krm-foyer/internal/metrics"
)

const (
	userToken  = "user-token-4f1c9a"  //nolint:gosec // a marker to search for, not a credential
	otherToken = "other-token-7d2e0b" //nolint:gosec // a marker to search for, not a credential
	// within bounds how long a test waits for what must happen.
	within = 5 * time.Second
	// checkEvery is the session-check interval in these tests.
	checkEvery = 20 * time.Millisecond
)

// notes is the resource the tests watch, as the hello example does.
const notes = "/stream/v1?group=hello.krm-foyer.example&version=v1&resource=notes&namespace=hello"

// credentials is the test's session: a fixed token, a session name, and whether the
// session is still live.
type credentials struct {
	token   string
	session string
	refused *interruption.Interruption
	live    *atomic.Bool
}

func (c credentials) Token(*http.Request) (gate.Credential, *interruption.Interruption) {
	live := func(context.Context) bool { return c.live == nil || c.live.Load() }
	session := c.session
	if session == "" {
		session = "s1"
	}
	return gate.Credential{Token: c.token, Session: session, Live: live}, c.refused
}

// byHeader hands each request the token named by its Test-Token header, as if each
// came from a session of its own.
type byHeader struct{}

func (byHeader) Token(r *http.Request) (gate.Credential, *interruption.Interruption) {
	t := r.Header.Get("Test-Token")
	return credentials{token: t, session: t}.Token(r)
}

// apiServer stands in for the API server. It answers a watch of notes with a
// streaming list: each note as ADDED, the bookmark that ends the snapshot, then
// nothing until the request ends. Other answers come from respond.
type apiServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*http.Request
	ended    chan struct{}
}

func (a *apiServer) received() []*http.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*http.Request(nil), a.requests...)
}

func newAPIServer(t *testing.T, respond http.HandlerFunc) *apiServer {
	t.Helper()
	a := &apiServer{ended: make(chan struct{}, 100)}
	a.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.requests = append(a.requests, r.Clone(context.Background()))
		a.mu.Unlock()
		defer func() { a.ended <- struct{}{} }()
		if respond != nil {
			respond(w, r)
			return
		}
		snapshot(w, r)
	}))
	a.EnableHTTP2 = true
	a.StartTLS()
	t.Cleanup(a.Close)
	return a
}

// snapshot answers a streaming list of two notes, then holds the watch open.
func snapshot(w http.ResponseWriter, r *http.Request) {
	snapshotOf(w, 10, "note first")
	<-r.Context().Done()
}

// snapshotOf writes a streaming list of the notes first and second, first with the
// text first, at resourceVersions from rv, and the bookmark that ends it.
func snapshotOf(w http.ResponseWriter, rv int, first string) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	for i, name := range []string{"first", "second"} {
		text := "note " + name
		if name == "first" {
			text = first
		}
		_ = enc.Encode(map[string]any{"type": "ADDED", "object": map[string]any{
			"apiVersion": "hello.krm-foyer.example/v1", "kind": "Note",
			"metadata": map[string]any{
				"name": name, "namespace": "hello", "uid": "uid-" + name,
				"resourceVersion": fmt.Sprint(rv + i),
			},
			"spec": map[string]any{"text": text},
		}})
	}
	_ = enc.Encode(map[string]any{"type": "BOOKMARK", "object": map[string]any{
		"apiVersion": "hello.krm-foyer.example/v1", "kind": "Note",
		"metadata": map[string]any{
			"resourceVersion": fmt.Sprint(rv + 2),
			"annotations":     map[string]any{"k8s.io/initial-events-end": "true"},
		},
	}})
	_ = http.NewResponseController(w).Flush()
}

// status answers with a Kubernetes Status.
func status(code int, reason, message string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"kind": "Status", "apiVersion": "v1", "status": "Failure",
			"reason": reason, "message": message, "code": code,
		})
	}
}

type foyer struct {
	url  string
	logs *lockedLog
}

// lockedLog keeps what krm-foyer logs, for a test to read while it may still be
// writing.
type lockedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// newFoyer serves streams from api behind a gate with creds; adjust changes the
// streams' configuration and the gate's.
func newFoyer(t *testing.T, api *apiServer, creds gate.Credentials, adjust func(*Config, *gate.Config)) foyer {
	t.Helper()
	logs := &lockedLog{}
	gcfg := gate.Config{
		Credentials: creds, SessionCheckInterval: checkEvery,
		Logger: slog.New(slog.NewJSONHandler(logs, nil)),
	}
	server, _ := url.Parse(api.URL)
	roots := x509.NewCertPool()
	roots.AddCert(api.Certificate())
	cfg := Config{Server: server, RootCAs: roots}
	if adjust != nil {
		adjust(&cfg, &gcfg)
	}
	g, err := gate.New(gcfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Gate = g
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(s)
	t.Cleanup(front.Close)
	return foyer{url: front.URL, logs: logs}
}

// event is one krm-stream event, as the browser receives it.
type event struct {
	Type       string          `json:"type"`
	Code       string          `json:"code"`
	Message    string          `json:"message"`
	Terminal   bool            `json:"terminal"`
	RetryAfter *int            `json:"retryAfterMs"`
	Projection string          `json:"projection"`
	Object     json.RawMessage `json:"object"`
}

// sse is a stream as the browser reads it.
type sse struct {
	resp *http.Response
	r    *bufio.Reader
	raw  strings.Builder
}

// open sends a GET for target with header, and returns once its head arrived.
func (f foyer) open(ctx context.Context, t *testing.T, target string, header http.Header) *sse {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return &sse{resp: resp, r: bufio.NewReader(resp.Body)}
}

// next reads the next event, or returns the error that ended the stream first.
func (s *sse) next() (event, error) {
	for {
		line, err := s.r.ReadString('\n')
		s.raw.WriteString(line)
		if err != nil {
			return event{}, err
		}
		data, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "data: ")
		if !ok {
			continue // a comment (heartbeat), an event name or a blank line
		}
		var e event
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return event{}, fmt.Errorf("event %q: %w", data, err)
		}
		return e, nil
	}
}

// until reads events until one of type typ, and returns every event read.
func (s *sse) until(t *testing.T, typ string) []event {
	t.Helper()
	var seen []event
	for {
		e, err := s.next()
		if err != nil {
			t.Fatalf("the stream ended (%v) before a %q event; read %+v", err, typ, seen)
		}
		seen = append(seen, e)
		if e.Type == typ {
			return seen
		}
	}
}

// rest reads the stream to its end, and returns how it ended: nil for a clean end.
func (s *sse) rest(t *testing.T) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		var b strings.Builder
		_, err := io.Copy(&b, s.r)
		s.raw.WriteString(b.String())
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(within):
		_ = s.resp.Body.Close()
		<-done
		t.Fatal("the stream was still open after " + within.String())
		return nil
	}
}

func types(events []event) string {
	var ts []string
	for _, e := range events {
		ts = append(ts, e.Type)
	}
	return strings.Join(ts, ",")
}

// Without a session there is no stream: krm-foyer's 401, before anything is sent
// to the API server.
func TestAStreamNeedsASession(t *testing.T) {
	api := newAPIServer(t, nil)
	f := newFoyer(t, api, credentials{refused: interruption.NotSignedIn()}, nil)
	s := f.open(t.Context(), t, notes, nil)
	if s.resp.StatusCode != http.StatusUnauthorized || s.resp.Header.Get(interruption.Header) == "" {
		t.Errorf("status %d, %s %q; want krm-foyer's 401", s.resp.StatusCode, interruption.Header, s.resp.Header.Get(interruption.Header))
	}
	if n := len(api.received()); n != 0 {
		t.Errorf("%d requests reached the API server", n)
	}
}

// A stream is a GET. Anything else is refused before it reaches the gate or the API
// server, as krm-foyer's own 405.
func TestAStreamIsAGet(t *testing.T) {
	api := newAPIServer(t, nil)
	f := newFoyer(t, api, credentials{token: userToken}, nil)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, _ := http.NewRequestWithContext(t.Context(), method, f.url+notes, nil)
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get(interruption.Header) != "MethodNotAllowed" {
			t.Errorf("%s: status %d, %s %q; want krm-foyer's 405", method, resp.StatusCode,
				interruption.Header, resp.Header.Get(interruption.Header))
		}
	}
	if n := len(api.received()); n != 0 {
		t.Errorf("%d requests reached the API server", n)
	}
}

// A stream is the user's own watch: the API server is asked for the scope, with
// the session's token and nothing the browser sent but its User-Agent, and the
// browser gets the snapshot, then live updates.
func TestAStreamWatchesAsTheUser(t *testing.T) {
	api := newAPIServer(t, nil)
	f := newFoyer(t, api, credentials{token: userToken}, nil)
	s := f.open(t.Context(), t, notes, http.Header{
		"Authorization":    {"Bearer browser-supplied"},
		"Impersonate-User": {"system:admin"},
		"Cookie":           {"__Host-krm-foyer-session=abc"},
		"User-Agent":       {"the-browser/1"},
	})
	if s.resp.StatusCode != http.StatusOK || !strings.HasPrefix(s.resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, Content-Type %q", s.resp.StatusCode, s.resp.Header.Get("Content-Type"))
	}
	events := s.until(t, "synced")
	if got := types(events); got != "reset,added,added,synced" {
		t.Errorf("events %s, want reset,added,added,synced", got)
	}
	reqs := api.received()
	if len(reqs) != 1 {
		t.Fatalf("%d requests reached the API server, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodGet || r.URL.Path != "/apis/hello.krm-foyer.example/v1/namespaces/hello/notes" ||
		r.URL.Query().Get("watch") != "true" {
		t.Errorf("the API server was asked %s %s", r.Method, r.URL)
	}
	if got := r.Header.Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+userToken {
		t.Errorf("Authorization %q, want the session's token alone", got)
	}
	for k := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "impersonate-") || k == "Cookie" {
			t.Errorf("the browser's %s reached the API server", k)
		}
	}
	if ua := r.Header.Get("User-Agent"); ua != "the-browser/1" {
		t.Errorf("User-Agent %q, want the browser's", ua)
	}
}

// Streams of different sessions, open at once, each reach the API server with
// their own session's token and no other.
func TestEachStreamCarriesItsOwnToken(t *testing.T) {
	api := newAPIServer(t, nil)
	f := newFoyer(t, api, byHeader{}, nil)
	var wg sync.WaitGroup
	for i := range 20 {
		token := userToken
		if i%2 == 1 {
			token = otherToken
		}
		ns := map[string]string{userToken: "ns-user", otherToken: "ns-other"}[token]
		wg.Go(func() {
			s := f.open(t.Context(), t, strings.Replace(notes, "namespace=hello", "namespace="+ns, 1),
				http.Header{"Test-Token": {token}})
			s.until(t, "synced")
		})
	}
	wg.Wait()
	reqs := api.received()
	if len(reqs) != 20 {
		t.Fatalf("%d requests reached the API server, want 20", len(reqs))
	}
	for _, r := range reqs {
		want := map[string]string{"ns-user": userToken, "ns-other": otherToken}[strings.Split(r.URL.Path, "/")[5]]
		if got := r.Header.Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+want {
			t.Errorf("%s was watched with %q, want its own session's token", r.URL.Path, got)
		}
	}
}

// The API server is pinned: an untrusted certificate ends the stream with no
// request served, and no proxy from the environment is ever used.
func TestTheUpstreamIsPinnedAndVerified(t *testing.T) {
	api := newAPIServer(t, nil)
	f := newFoyer(t, api, credentials{token: userToken}, func(c *Config, _ *gate.Config) { c.RootCAs = x509.NewCertPool() })
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	s := f.open(ctx, t, notes, nil)
	for {
		e, err := s.next()
		if err != nil {
			break
		}
		if e.Type == "reset" || e.Type == "added" {
			t.Fatalf("a stream opened against an untrusted certificate: %+v", e)
		}
	}
	if n := len(api.received()); n != 0 {
		t.Errorf("%d requests were served past an untrusted certificate", n)
	}
	if strings.Contains(s.raw.String(), strings.TrimPrefix(api.URL, "https://")) {
		t.Errorf("the API server's address reached the browser: %s", s.raw.String())
	}

	server, _ := url.Parse(api.URL)
	g, _ := gate.New(gate.Config{Credentials: credentials{token: userToken}})
	streams, err := New(Config{Server: server, Gate: g})
	if err != nil {
		t.Fatal(err)
	}
	if tr, ok := streams.transport.(*http.Transport); !ok || tr.Proxy != nil || tr.TLSClientConfig.InsecureSkipVerify {
		t.Errorf("transport is not pinned and verified: %#v", streams.transport)
	}
}

// What the browser asks for is checked by krm-stream's gateway, which answers a
// scope it will not serve with a terminal SCOPE_INVALID, before anything reaches the
// API server. krm-foyer serves one cluster, the unnamed target.
func TestScopesTheGatewayRefuses(t *testing.T) {
	api := newAPIServer(t, nil)
	f := newFoyer(t, api, credentials{token: userToken}, nil)
	for name, target := range map[string]string{
		"an API server address": notes + "&server=https://evil.example",
		"a token":               notes + "&token=abc",
		"another target":        notes + "&target=prod",
		"no version":            "/stream/v1?group=hello.krm-foyer.example&resource=notes&namespace=hello",
		"a Kind for a resource": "/stream/v1?group=hello.krm-foyer.example&version=v1&resource=Note&namespace=hello",
		"a path for a group":    "/stream/v1?group=../../api&version=v1&resource=notes",
		"an unknown projection": notes + "&projection=krm-everything/v1",
	} {
		s := f.open(t.Context(), t, target, nil)
		e, err := s.next()
		if err != nil || e.Type != "error" || !e.Terminal || (e.Code != "SCOPE_INVALID" && e.Code != "FORBIDDEN") {
			t.Errorf("%s: %+v, %v; want a terminal refusal", name, e, err)
		}
	}
	if n := len(api.received()); n != 0 {
		t.Errorf("%d requests reached the API server", n)
	}
}

// Each built-in projection may be asked for: a projection hides nothing from a
// user who may read the whole object through /k8s.
func TestEveryBuiltInProjection(t *testing.T) {
	api := newAPIServer(t, nil)
	f := newFoyer(t, api, credentials{token: userToken}, nil)
	for requested, want := range map[string]string{
		"": "krm-full/v1", "krm-raw/v1": "krm-raw/v1", "krm-full/v1": "krm-full/v1", "krm-spec/v1": "krm-spec/v1",
	} {
		target := notes
		if requested != "" {
			target += "&projection=" + requested
		}
		events := f.open(t.Context(), t, target, nil).until(t, "reset")
		if got := events[len(events)-1].Projection; got != want {
			t.Errorf("asked for %q: projection %q, want %q", requested, got, want)
		}
	}
}

// Kubernetes' refusals keep their meaning on the stream: a 403 is FORBIDDEN with
// Kubernetes' own message, a 401 UNAUTHENTICATED, and a resource the API server
// does not serve SCOPE_INVALID. Each is terminal, and none says where the API
// server is.
func TestKubernetesRefusalsKeepTheirMeaning(t *testing.T) {
	const forbidden = `notes.hello.krm-foyer.example is forbidden: User "oidc:carol@example.com" cannot watch resource "notes"`
	for name, tc := range map[string]struct {
		respond http.HandlerFunc
		code    string
		message string
	}{
		"403": {status(http.StatusForbidden, "Forbidden", forbidden), "FORBIDDEN", forbidden},
		"401": {status(http.StatusUnauthorized, "Unauthorized", "Unauthorized"), "UNAUTHENTICATED", ""},
		"404": {status(http.StatusNotFound, "NotFound", "the server could not find the requested resource"), "SCOPE_INVALID", ""},
	} {
		t.Run(name, func(t *testing.T) {
			api := newAPIServer(t, tc.respond)
			f := newFoyer(t, api, credentials{token: userToken}, nil)
			s := f.open(t.Context(), t, notes, nil)
			e, err := s.next()
			if err != nil || e.Type != "error" || e.Code != tc.code || !e.Terminal {
				t.Fatalf("%+v, %v; want a terminal %s", e, err, tc.code)
			}
			if tc.message != "" && e.Message != tc.message {
				t.Errorf("message %q, want Kubernetes' own %q", e.Message, tc.message)
			}
			_ = s.rest(t)
			if host := strings.TrimPrefix(api.URL, "https://"); strings.Contains(s.raw.String(), host) {
				t.Errorf("the API server's address reached the browser: %s", s.raw.String())
			}
		})
	}
}

// A stream open when its session ends is cut short within the session-check
// interval: aborted, never ended cleanly, and its watch cancelled at the API server.
func TestAStreamEndsWithItsSession(t *testing.T) {
	api := newAPIServer(t, nil)
	live := &atomic.Bool{}
	live.Store(true)
	f := newFoyer(t, api, credentials{token: userToken, live: live}, nil)
	s := f.open(t.Context(), t, notes, nil)
	s.until(t, "synced")
	live.Store(false)
	if err := s.rest(t); err == nil || errors.Is(err, io.EOF) {
		t.Errorf("the stream ended cleanly (%v); it must be aborted", err)
	}
	select {
	case <-api.ended:
	case <-time.After(within):
		t.Error("the watch was not cancelled at the API server")
	}
}

// When the gate cuts a stream short, the handler aborts the response even where the
// write deadline cannot: the gateway returns quietly when its context ends, and
// returning would end the response cleanly.
func TestACutStreamAbortsWithoutTheWriteDeadline(t *testing.T) {
	api := newAPIServer(t, nil)
	live := &atomic.Bool{}
	live.Store(true)
	server, _ := url.Parse(api.URL)
	roots := x509.NewCertPool()
	roots.AddCert(api.Certificate())
	g, err := gate.New(gate.Config{Credentials: credentials{token: userToken, live: live}, SessionCheckInterval: checkEvery})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Server: server, RootCAs: roots, Gate: g})
	if err != nil {
		t.Fatal(err)
	}
	// A ResponseRecorder has no write deadline to set.
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, notes, nil)
	time.AfterFunc(5*checkEvery, func() { live.Store(false) })
	defer func() {
		if v := recover(); v != http.ErrAbortHandler { //nolint:errorlint // ErrAbortHandler is panicked as is
			t.Fatalf("recovered %v; want http.ErrAbortHandler", v)
		}
	}()
	s.ServeHTTP(w, r)
}

// gauge reads one unlabelled gauge from m, as scraped.
func gauge(t *testing.T, m *metrics.Metrics, name string) string {
	t.Helper()
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	for line := range strings.Lines(w.Body.String()) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), name+" "); ok {
			return v
		}
	}
	t.Fatalf("no %s in the scrape", name)
	return ""
}

// A stream counts against the stream limits, not the request limits: past a
// session's limit it is krm-foyer's 429, and nothing reaches the API server.
func TestStreamsHaveLimitsOfTheirOwn(t *testing.T) {
	api := newAPIServer(t, nil)
	f := newFoyer(t, api, credentials{token: userToken}, func(_ *Config, g *gate.Config) {
		g.MaxSessionStreams, g.MaxSessionConcurrentRequests = 1, 1
	})
	f.open(t.Context(), t, notes, nil).until(t, "synced")
	second := f.open(t.Context(), t, notes, nil)
	if second.resp.StatusCode != http.StatusTooManyRequests || second.resp.Header.Get(interruption.Header) != "TooManyStreams" {
		t.Errorf("second stream: %d %s, want krm-foyer's 429 TooManyStreams", second.resp.StatusCode,
			second.resp.Header.Get(interruption.Header))
	}
	if n := len(api.received()); n != 1 {
		t.Errorf("%d requests reached the API server, want the first stream's alone", n)
	}
}

// The streams open, and the watches they hold at the API server, are counted apart:
// one each while a stream is open, none once it has ended.
func TestStreamsAndTheirWatchesAreCounted(t *testing.T) {
	api := newAPIServer(t, nil)
	m := metrics.New()
	f := newFoyer(t, api, credentials{token: userToken}, func(_ *Config, g *gate.Config) { g.Metrics = m })
	ctx, cancel := context.WithCancel(t.Context())
	f.open(ctx, t, notes, nil).until(t, "synced")
	if s, w := gauge(t, m, "krm_foyer_streams_open"), gauge(t, m, "krm_foyer_upstream_watches_open"); s != "1" || w != "1" {
		t.Errorf("while open: %s streams and %s watches, want 1 and 1", s, w)
	}
	cancel()
	deadline := time.Now().Add(within)
	for gauge(t, m, "krm_foyer_streams_open") != "0" || gauge(t, m, "krm_foyer_upstream_watches_open") != "0" {
		if time.Now().After(deadline) {
			t.Fatalf("after the browser left: %s streams and %s watches, want none",
				gauge(t, m, "krm_foyer_streams_open"), gauge(t, m, "krm_foyer_upstream_watches_open"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := gauge(t, m, "krm_foyer_requests_in_flight"); n != "0" {
		t.Errorf("a stream counted as %s requests in flight", n)
	}
}

// texts returns the text of each note in events, by name, as the last event said.
func texts(t *testing.T, events []event) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, e := range events {
		if e.Type != "added" && e.Type != "modified" {
			continue
		}
		var o struct {
			Metadata struct{ Name string } `json:"metadata"`
			Spec     struct{ Text string } `json:"spec"`
		}
		if err := json.Unmarshal(e.Object, &o); err != nil {
			t.Fatal(err)
		}
		out[o.Metadata.Name] = o.Spec.Text
	}
	return out
}

// A watch the API server ends, as it does after its timeout, or ends with 410 Gone,
// is opened again on the same stream: the browser is told continuity was lost, gets a
// fresh snapshot with what changed in between, and the stream goes on, as the user.
func TestAStreamRecoversWhenItsWatchEnds(t *testing.T) {
	for name, end := range map[string]func(http.ResponseWriter){
		"ended": func(http.ResponseWriter) {},
		"410 Gone": func(w http.ResponseWriter) {
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "ERROR", "object": map[string]any{
				"kind": "Status", "apiVersion": "v1", "status": "Failure",
				"reason": "Expired", "code": 410, "message": "too old resource version: 10 (300)",
			}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			var watches atomic.Int32
			api := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
				if watches.Add(1) == 1 {
					snapshotOf(w, 10, "before")
					// A real watch lives for minutes. krm-stream takes one that ends
					// within a second of its snapshot for a failing one.
					time.Sleep(1200 * time.Millisecond)
					end(w)
					return
				}
				snapshotOf(w, 20, "changed in between")
				<-r.Context().Done()
			})
			f := newFoyer(t, api, credentials{token: userToken}, nil)
			s := f.open(t.Context(), t, notes, nil)
			first := s.until(t, "synced")
			second := s.until(t, "synced")
			if got := types(first); got != "reset,added,added,synced" {
				t.Errorf("first cycle %s", got)
			}
			if got := types(second); got != "error,reset,added,added,synced" ||
				second[0].Code != "RESYNC_REQUIRED" || second[0].Terminal {
				t.Errorf("second cycle %s, %+v; want a RESYNC_REQUIRED, then a fresh snapshot", got, second[0])
			}
			if got := texts(t, second)["first"]; got != "changed in between" {
				t.Errorf("after recovery, first reads %q", got)
			}
			for _, r := range api.received() {
				if r.Header.Get("Authorization") != "Bearer "+userToken {
					t.Errorf("a watch was opened again as %q", r.Header.Get("Authorization"))
				}
			}
			// A routine end is not a failure: no wait before the watch is opened again,
			// and nothing logged as one.
			if logs := f.logs.String(); strings.Contains(logs, "could not serve") {
				t.Errorf("a routine end of a watch was logged as a failure: %s", logs)
			}
		})
	}
}

// recorder is a server that should never be reached, and records what reached it.
type recorder struct {
	mu   sync.Mutex
	auth []string
}

func (rc *recorder) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.auth = append(rc.auth, r.Header.Get("Authorization"))
}

func (rc *recorder) reached() []string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]string(nil), rc.auth...)
}

// A redirect from the API server is not followed, as /k8s does not follow one: not to
// another host over https, and not to plain http. The token never leaves for anywhere
// but the configured API server, and the stream ends saying why.
func TestARedirectIsNotFollowed(t *testing.T) {
	elsewhereTLS := &recorder{}
	tlsServer := httptest.NewTLSServer(elsewhereTLS) // the same certificate as the API server's
	t.Cleanup(tlsServer.Close)
	elsewherePlain := &recorder{}
	plainServer := httptest.NewServer(elsewherePlain)
	t.Cleanup(plainServer.Close)

	for name, target := range map[string]struct {
		url string
		rc  *recorder
	}{
		"to another https server": {tlsServer.URL, elsewhereTLS},
		"to plain http":           {plainServer.URL, elsewherePlain},
	} {
		t.Run(name, func(t *testing.T) {
			api := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.url+r.URL.RequestURI(), http.StatusTemporaryRedirect) //nolint:gosec // the hostile API server under test redirects on purpose
			})
			f := newFoyer(t, api, credentials{token: userToken}, nil)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			s := f.open(ctx, t, notes, nil)
			e, err := s.next()
			if err != nil || e.Type != "error" || e.Code != "INTERNAL" || !e.Terminal {
				t.Errorf("%+v, %v; want a terminal INTERNAL", e, err)
			}
			if got := target.rc.reached(); len(got) != 0 {
				t.Errorf("the redirect was followed, with Authorization %q", got)
			}
			if n := len(api.received()); n != 1 {
				t.Errorf("the watch was opened %d times; a redirect is final", n)
			}
			if logs := f.logs.String(); !strings.Contains(logs, `"cause":"redirect"`) {
				t.Errorf("the redirect was not logged as one: %s", logs)
			}
		})
	}
}

// watchThen answers a streaming list of the two notes, then whatever then writes.
func watchThen(then func(w http.ResponseWriter)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snapshotOf(w, 10, "note first")
		then(w)
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
	}
}

// watchError writes an ERROR event carrying a Kubernetes Status, as the API server
// sends one on an open watch.
func watchError(code int, reason, message string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "ERROR", "object": map[string]any{
			"kind": "Status", "apiVersion": "v1", "status": "Failure",
			"reason": reason, "code": code, "message": message,
		}})
	}
}

// Kubernetes' refusals on an open watch end the stream with their meaning, as at
// opening: a 403 is a terminal FORBIDDEN with Kubernetes' message, a 401 a terminal
// UNAUTHENTICATED, and the watch is not opened again.
func TestARefusalOnAnOpenWatchEndsTheStream(t *testing.T) {
	const forbidden = `notes.hello.krm-foyer.example is forbidden: User "oidc:carol@example.com" cannot watch resource "notes"`
	for name, tc := range map[string]struct {
		then    func(http.ResponseWriter)
		code    string
		message string
	}{
		"403": {watchError(http.StatusForbidden, "Forbidden", forbidden), "FORBIDDEN", forbidden},
		"401": {watchError(http.StatusUnauthorized, "Unauthorized", "Unauthorized"), "UNAUTHENTICATED", ""},
	} {
		t.Run(name, func(t *testing.T) {
			api := newAPIServer(t, watchThen(tc.then))
			f := newFoyer(t, api, credentials{token: userToken}, nil)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			s := f.open(ctx, t, notes, nil)
			s.until(t, "synced")
			e, err := s.next()
			if err != nil || e.Type != "error" || e.Code != tc.code || !e.Terminal {
				t.Fatalf("%+v, %v; want a terminal %s", e, err, tc.code)
			}
			if tc.message != "" && e.Message != tc.message {
				t.Errorf("message %q, want Kubernetes' own %q", e.Message, tc.message)
			}
			if n := len(api.received()); n != 1 {
				t.Errorf("the watch was opened %d times; a refusal is final", n)
			}
		})
	}
}

// What the API server says when it fails is never logged as it is: it is not
// krm-foyer's text, and it could hold anything, the token it was sent among it.
func TestFailuresAreLoggedWithoutTheirText(t *testing.T) {
	echo := func(w http.ResponseWriter, r *http.Request) {
		status(http.StatusServiceUnavailable, "ServiceUnavailable", "you sent "+r.Header.Get("Authorization"))(w, r)
	}
	for name, respond := range map[string]http.HandlerFunc{
		"a Status at opening": echo,
		"a Status on the open watch": func(w http.ResponseWriter, r *http.Request) {
			watchThen(watchError(http.StatusInternalServerError, "InternalError", "you sent "+r.Header.Get("Authorization")))(w, r)
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := newAPIServer(t, respond)
			f := newFoyer(t, api, credentials{token: userToken}, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			s := f.open(ctx, t, notes, nil)
			for {
				if _, err := s.next(); err != nil {
					break
				}
			}
			if strings.Contains(f.logs.String(), userToken) {
				t.Errorf("the token reached krm-foyer's log: %s", f.logs.String())
			}
			if strings.Contains(s.raw.String(), userToken) {
				t.Errorf("the token reached the browser: %s", s.raw.String())
			}
		})
	}
}

// A stream leaves nothing behind when it ends, even while the API server is still
// sending: every goroutine it started is gone once its browser is.
func TestAnEndedStreamLeavesNoGoroutines(t *testing.T) {
	api := newAPIServer(t, watchThen(func(w http.ResponseWriter) {
		// Bookmarks, as the API server sends them, faster than anyone reads them.
		for range 10000 {
			if json.NewEncoder(w).Encode(map[string]any{"type": "BOOKMARK", "object": map[string]any{
				"apiVersion": "hello.krm-foyer.example/v1", "kind": "Note",
				"metadata": map[string]any{"resourceVersion": "99"},
			}}) != nil {
				return
			}
		}
	}))
	f := newFoyer(t, api, credentials{token: userToken}, nil)
	f.open(t.Context(), t, notes, nil).until(t, "synced") // warm up, so pools exist
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()
	for range 20 {
		ctx, cancel := context.WithCancel(t.Context())
		f.open(ctx, t, notes, nil).until(t, "synced")
		cancel()
	}
	deadline := time.Now().Add(within)
	for runtime.NumGoroutine() > before+5 {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("%d goroutines, %d before 20 streams opened and ended:\n%s",
				runtime.NumGoroutine(), before, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A failure that may pass is left to the browser's client to retry: the stream sends a
// non-terminal UPSTREAM_UNAVAILABLE, with the API server's Retry-After as its hint,
// and closes after that one attempt. Each retry is then a request of its own, through
// the gate and its request rate. A watch that ends before its snapshot, or hardly
// after it, is such a failure too: krm-stream opens it once more on the stream, as
// it would after a routine end, and the second such end closes it.
func TestARetryableFailureIsLeftToTheBrowser(t *testing.T) {
	// The hint in the Status alone: with a Retry-After header, client-go waits and
	// asks again itself, up to ten times, before the stream hears of it.
	retryAfter := func(code int, reason string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": reason,
				"message": "slow down", "code": code, "details": map[string]any{"retryAfterSeconds": 3},
			})
		}
	}
	for name, tc := range map[string]struct {
		respond http.HandlerFunc
		hint    int
		// attempts is how many watches the stream opens; zero means one.
		attempts int
	}{
		"503 at opening":        {respond: status(http.StatusServiceUnavailable, "ServiceUnavailable", "etcd is down at 10.0.0.7")},
		"429 at opening":        {respond: retryAfter(http.StatusTooManyRequests, "TooManyRequests"), hint: 3000},
		"500 on the open watch": {respond: watchThen(watchError(http.StatusInternalServerError, "InternalError", "etcd is down"))},
		"429 on the open watch": {respond: watchThen(watchError(http.StatusTooManyRequests, "TooManyRequests", "slow down"))},
		"ended before its snapshot": {respond: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
		}, attempts: 2},
		"ended right after its snapshot": {respond: func(w http.ResponseWriter, _ *http.Request) { snapshotOf(w, 10, "x") }, attempts: 2},
		"410 right after its snapshot": {respond: func(w http.ResponseWriter, r *http.Request) {
			snapshotOf(w, 10, "x")
			watchError(http.StatusGone, "Expired", "too old resource version")(w)
		}, attempts: 2},
	} {
		t.Run(name, func(t *testing.T) {
			api := newAPIServer(t, tc.respond)
			f := newFoyer(t, api, credentials{token: userToken}, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			s := f.open(ctx, t, notes, nil)
			var last event
			for {
				e, err := s.next()
				if err != nil {
					break
				}
				last = e
			}
			if last.Type != "error" || last.Code != "UPSTREAM_UNAVAILABLE" || last.Terminal {
				t.Errorf("the stream ended with %+v; want a non-terminal UPSTREAM_UNAVAILABLE", last)
			}
			if tc.hint != 0 && (last.RetryAfter == nil || *last.RetryAfter != tc.hint) {
				t.Errorf("retryAfterMs %v, want the API server's %d", last.RetryAfter, tc.hint)
			}
			want := max(tc.attempts, 1)
			if n := len(api.received()); n != want {
				t.Errorf("%d attempts on one connection, want %d; the browser's client retries, not the stream", n, want)
			}
			if strings.Contains(s.raw.String(), "10.0.0.7") {
				t.Errorf("what the API server said reached the browser: %s", s.raw.String())
			}
		})
	}
}
