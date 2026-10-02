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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ConfigButler/krm-foyer/internal/gate"
	"github.com/ConfigButler/krm-foyer/internal/interruption"
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
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	for i, name := range []string{"first", "second"} {
		_ = enc.Encode(map[string]any{"type": "ADDED", "object": map[string]any{
			"apiVersion": "hello.krm-foyer.example/v1", "kind": "Note",
			"metadata": map[string]any{
				"name": name, "namespace": "hello", "uid": "uid-" + name,
				"resourceVersion": fmt.Sprint(10 + i),
			},
			"spec": map[string]any{"text": "note " + name},
		}})
	}
	_ = enc.Encode(map[string]any{"type": "BOOKMARK", "object": map[string]any{
		"apiVersion": "hello.krm-foyer.example/v1", "kind": "Note",
		"metadata": map[string]any{
			"resourceVersion": "12",
			"annotations":     map[string]any{"k8s.io/initial-events-end": "true"},
		},
	}})
	_ = http.NewResponseController(w).Flush()
	<-r.Context().Done()
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
	logs *strings.Builder
}

// newFoyer serves streams from api behind a gate with creds, configured by adjust.
func newFoyer(t *testing.T, api *apiServer, creds gate.Credentials, adjust func(*Config)) foyer {
	t.Helper()
	logs := &strings.Builder{}
	var mu sync.Mutex
	g, err := gate.New(gate.Config{
		Credentials: creds, SessionCheckInterval: checkEvery,
		Logger: slog.New(slog.NewJSONHandler(writerFunc(func(p []byte) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			return logs.Write(p)
		}), nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	server, _ := url.Parse(api.URL)
	roots := x509.NewCertPool()
	roots.AddCert(api.Certificate())
	cfg := Config{Server: server, RootCAs: roots, Gate: g, RetryDelay: 50 * time.Millisecond}
	if adjust != nil {
		adjust(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(s)
	t.Cleanup(front.Close)
	return foyer{url: front.URL, logs: logs}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// event is one krm-stream event, as the browser receives it.
type event struct {
	Type       string          `json:"type"`
	Code       string          `json:"code"`
	Message    string          `json:"message"`
	Terminal   bool            `json:"terminal"`
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
	f := newFoyer(t, api, credentials{token: userToken}, func(c *Config) { c.RootCAs = x509.NewCertPool() })
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

// An API server that cannot serve the watch right now is asked again, with a wait
// that grows, and the browser is never told to give up: no terminal event, and no
// INTERNAL with what went wrong inside.
func TestAnUnavailableAPIServerIsRetriedWithBackoff(t *testing.T) {
	api := newAPIServer(t, status(http.StatusServiceUnavailable, "ServiceUnavailable", "etcd is down at 10.0.0.7"))
	f := newFoyer(t, api, credentials{token: userToken}, nil) // first wait 50ms
	ctx, cancel := context.WithTimeout(t.Context(), 1200*time.Millisecond)
	defer cancel()
	s := f.open(ctx, t, notes, nil)
	for {
		e, err := s.next()
		if err != nil {
			break
		}
		if e.Terminal || e.Code == "INTERNAL" {
			t.Fatalf("%+v; an unavailable API server is not a reason to give up", e)
		}
	}
	// 50ms doubling: attempts at about 0, 50, 150, 350 and 750ms. Without a wait
	// there would be hundreds.
	if n := len(api.received()); n < 3 || n > 7 {
		t.Errorf("%d attempts in 1.2s, want about 5", n)
	}
	if strings.Contains(s.raw.String(), "10.0.0.7") {
		t.Errorf("what the API server said inside reached the browser: %s", s.raw.String())
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
