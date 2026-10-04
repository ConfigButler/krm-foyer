package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ConfigButler/krm-foyer/internal/gate"
	"github.com/ConfigButler/krm-foyer/internal/interruption"
)

// checkEvery is the session-check interval in these tests: short, so they are fast,
// and long enough that a response is seen open before it is checked.
const checkEvery = 20 * time.Millisecond

// within is how long a test waits for something the proxy should do within an
// interval or two.
const within = 5 * time.Second

// session is a session the test ends when it likes.
type session struct{ ended atomic.Bool }

func (s *session) credentials() credentials {
	return credentials{token: userToken, live: func(context.Context) bool { return !s.ended.Load() }}
}

// streamingAPIServer sends one event, then holds the response open until the
// request is cancelled. sent is closed when the event is out, cancelled when the
// request's context ends.
func streamingAPIServer(t *testing.T, http2 bool) (api *apiServer, sent, cancelled chan struct{}) {
	t.Helper()
	sent, cancelled = make(chan struct{}), make(chan struct{})
	api = newAPIServerWith(t, http2, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"ADDED"}`+"\n")
		_ = http.NewResponseController(w).Flush()
		close(sent)
		<-r.Context().Done()
		close(cancelled)
	})
	return api, sent, cancelled
}

// open starts a GET for target and returns its body once the head arrived. The body
// is closed when the test ends.
func (f foyer) open(t *testing.T, target string) io.Reader {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.url+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.client.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp.Body
}

// readsAborted reads body to its end and fails the test unless it ends in an error:
// an aborted response, never a clean end that would pass for a complete one.
func readsAborted(t *testing.T, body io.Reader) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, body)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the response ended cleanly; a response cut short must be aborted")
		}
	case <-time.After(within):
		t.Fatal("the response stayed open")
	}
}

func closedWithin(t *testing.T, c chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(within):
		t.Fatal(what)
	}
}

// Every open response ends when its session ends, at logout or expiry: the browser's
// response is aborted, and the request to the API server is cancelled, so the API
// server releases the watch at once. Over every combination of protocols.
func TestOpenResponseEndsWithItsSession(t *testing.T) {
	for upName, upstream := range protocols {
		for frontName, front := range protocols {
			t.Run(frontName+" to krm-foyer, "+upName+" to the API server", func(t *testing.T) {
				api, sent, cancelled := streamingAPIServer(t, upstream)
				s := &session{}
				f := newFoyerWith(t, api, s.credentials(), frontOptions{
					http2: front, config: func(c *testConfig) { c.SessionCheckInterval = checkEvery },
				})
				body := f.open(t, "/k8s/api/v1/configmaps?watch=1")
				<-sent
				line := make([]byte, len(`{"type":"ADDED"}`+"\n"))
				if _, err := io.ReadFull(body, line); err != nil {
					t.Fatalf("the first event did not arrive: %v", err)
				}
				// Open for several intervals with a live session: nothing is cut.
				time.Sleep(5 * checkEvery)
				select {
				case <-cancelled:
					t.Fatal("a response was cut short while its session was live")
				default:
				}

				s.ended.Store(true)
				readsAborted(t, body)
				closedWithin(t, cancelled, "the request to the API server was not cancelled when the session ended")
				assertProtocol(t, api, upstream)
				if !strings.Contains(f.logs.String(), `"cause":"session_ended"`) {
					t.Errorf("no log line names why the response was cut short:\n%s", f.logs)
				}
			})
		}
	}
}

// A response that finishes while its session is live is not touched, however long
// it took.
func TestALiveSessionsResponseIsNotCut(t *testing.T) {
	api := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		for range 10 {
			_, _ = io.WriteString(w, "line\n")
			_ = http.NewResponseController(w).Flush()
			time.Sleep(checkEvery)
		}
	})
	s := &session{}
	f := newFoyerWith(t, api, s.credentials(), frontOptions{config: func(c *testConfig) { c.SessionCheckInterval = checkEvery }})
	got, err := io.ReadAll(f.open(t, "/k8s/api/v1/namespaces/a/pods/b/log?follow=true"))
	if err != nil || strings.Count(string(got), "line\n") != 10 {
		t.Fatalf("read %q, %v; want ten lines and a clean end", got, err)
	}
}

// A session that ends before the API server has answered gets no answer at all:
// not a 200 with nothing in it, and not an interruption claiming the request never
// reached Kubernetes, which it did.
func TestSessionEndingBeforeTheAnswer(t *testing.T) {
	cancelled := make(chan struct{})
	api := newAPIServer(t, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(cancelled)
	})
	s := &session{}
	s.ended.Store(true)
	f := newFoyerWith(t, api, s.credentials(), frontOptions{config: func(c *testConfig) { c.SessionCheckInterval = checkEvery }})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.url+"/k8s/api/v1/configmaps?watch=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.client.RoundTrip(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("got a %d; want the request aborted", resp.StatusCode)
	}
	closedWithin(t, cancelled, "the request to the API server was not cancelled")
}

// A session check that does not answer in time counts as a session that has ended:
// fail closed, even if the answer, when it comes, says the session is live.
func TestAHangingSessionCheckCountsAsEnded(t *testing.T) {
	api, sent, cancelled := streamingAPIServer(t, true)
	hanging := credentials{token: userToken, live: func(ctx context.Context) bool {
		<-ctx.Done()
		return true // too late to count
	}}
	f := newFoyerWith(t, api, hanging, frontOptions{config: func(c *testConfig) { c.SessionCheckInterval = checkEvery }})
	body := f.open(t, "/k8s/api/v1/configmaps?watch=1")
	<-sent
	readsAborted(t, body)
	closedWithin(t, cancelled, "the request to the API server was not cancelled")
}

// A browser that stops reading cannot keep a response open past the end of its
// session: the write blocked on it is given a deadline, so the proxy's handler
// returns, and the request to the API server is cancelled.
func TestAStalledBrowserIsCutShortToo(t *testing.T) {
	for frontName, front := range protocols {
		t.Run(frontName, func(t *testing.T) {
			cancelled := make(chan struct{})
			api := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
				defer close(cancelled)
				w.Header().Set("Content-Type", "application/json")
				chunk := []byte(strings.Repeat(" ", 64<<10))
				for r.Context().Err() == nil {
					if _, err := w.Write(chunk); err != nil {
						return
					}
				}
			})
			s := &session{}
			returned := make(chan struct{})
			f := newFoyerWith(t, api, s.credentials(), frontOptions{
				http2:  front,
				config: func(c *testConfig) { c.SessionCheckInterval = checkEvery },
				wrap: func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						defer close(returned)
						defer func() {
							// The abort is how a cut-short response ends; it is not a failure here.
							if v := recover(); v != nil && !errors.Is(asError(v), http.ErrAbortHandler) {
								panic(v)
							}
						}()
						next.ServeHTTP(w, r)
					})
				},
			})
			_ = f.open(t, "/k8s/api/v1/configmaps?watch=1") // and never read
			time.Sleep(10 * checkEvery)                     // until every buffer between is full
			s.ended.Store(true)
			closedWithin(t, returned, "the proxy stayed blocked writing to a browser that stopped reading")
			closedWithin(t, cancelled, "the request to the API server was not cancelled")
		})
	}
}

func asError(v any) error {
	err, _ := v.(error)
	return err
}

// noLive is a credential source that says nothing about whether its token may
// still be used.
type noLive struct{}

func (noLive) Token(*http.Request) (gate.Credential, *interruption.Interruption) {
	return gate.Credential{Token: userToken}, nil
}

// A credential that cannot say whether its session is live counts as one that has
// ended: fail closed.
func TestACredentialWithoutLiveIsCutShort(t *testing.T) {
	api, sent, cancelled := streamingAPIServer(t, false)
	f := newFoyerWith(t, api, noLive{}, frontOptions{config: func(c *testConfig) { c.SessionCheckInterval = checkEvery }})
	body := f.open(t, "/k8s/api/v1/configmaps?watch=1")
	<-sent
	readsAborted(t, body)
	closedWithin(t, cancelled, "the request to the API server was not cancelled")
}

// When krm-foyer cut the request short before the API server answered, the answer
// is an abort, whether or not the write deadline already broke the connection: the
// request may have reached Kubernetes, so no interruption may answer for it, and
// writing nothing would send an empty 200.
func TestUpstreamErrorAfterACutAborts(t *testing.T) {
	p, err := newProxy(testConfig{
		Config:     Config{Server: &url.URL{Scheme: "https", Host: "kubernetes.example.test"}},
		gateConfig: gateConfig{Credentials: noLive{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cut := context.WithCancelCause(t.Context())
	cut(gate.CauseSessionEnded)
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/k8s/api/v1/configmaps?watch=1", nil)
	w := httptest.NewRecorder()
	defer func() {
		if v := recover(); !errors.Is(asError(v), http.ErrAbortHandler) {
			t.Fatalf("recovered %v; want http.ErrAbortHandler", v)
		}
		if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Flushed {
			t.Errorf("something was written before the abort: %d %q", w.Code, w.Body)
		}
	}()
	_ = p.upstreamError(r, context.Canceled)
}
