package proxy

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ConfigButler/krm-foyer/internal/gate"
	"github.com/ConfigButler/krm-foyer/internal/interruption"
	"github.com/ConfigButler/krm-foyer/internal/metrics"
)

// bySession is a credential source for several sessions at once: the session is
// named by a header only these tests send, and stays live.
type bySession struct{}

const sessionHeader = "Test-Session"

func (bySession) Token(r *http.Request) (gate.Credential, *interruption.Interruption) {
	return credentials{token: userToken, session: r.Header.Get(sessionHeader)}.Token(r)
}

// holdingAPIServer sends one event, then holds the response open until the request
// is cancelled.
func holdingAPIServer(t *testing.T) *apiServer {
	t.Helper()
	return newAPIServerWith(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"ADDED"}`+"\n")
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
	})
}

// opened is a response whose head has arrived. Its body is closed when the test ends.
type opened struct {
	StatusCode int
	Header     http.Header
	Body       io.Reader
}

// openAs sends a GET for target as session, and returns once the head arrived. The
// request ends when ctx does.
func (f foyer) openAs(ctx context.Context, t *testing.T, session, target string) opened {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(sessionHeader, session)
	resp, err := f.client.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return opened{StatusCode: resp.StatusCode, Header: resp.Header, Body: resp.Body}
}

const watchTarget = "/k8s/api/v1/configmaps?watch=1"

// endsAborted opens target as session and checks that the response is aborted:
// before its head, when it was cut short before the API server answered, or while
// its body is read. A response that ends cleanly fails.
func (f foyer) endsAborted(t *testing.T, session, target string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.url+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(sessionHeader, session)
	resp, err := f.client.RoundTrip(req)
	if err != nil {
		return // aborted before the head
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	readsAborted(t, resp.Body)
}

// assertTooMany checks a refusal for concurrency: a 429 interruption naming the
// bound and its limit, with no Retry-After, since nobody knows when a slot frees.
func assertTooMany(t *testing.T, resp opened, bound string, limit int) {
	t.Helper()
	// Before reading: a request let through by mistake may stay open.
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	a := answer{StatusCode: resp.StatusCode, Header: resp.Header, Body: string(body)}
	s := readStatus(t, a)
	if s.Reason != "TooManyConcurrentRequests" || s.Details == nil || len(s.Details.Causes) != 1 ||
		s.Details.Causes[0].Reason != "BoundReached" || s.Details.Causes[0].Field != bound ||
		s.Details.Causes[0].Message != strconv.Itoa(limit) {
		t.Errorf("Status %+v does not name the bound %s and its limit %d", s, bound, limit)
	}
	if ra := a.Header.Values("Retry-After"); len(ra) != 0 {
		t.Errorf("Retry-After %q on a concurrency refusal", ra)
	}
}

// eventuallyLetThrough retries a request as session until the concurrency bound
// lets it through, whatever the API server then answers: a slot frees when the
// proxy's handler has returned, a moment after the browser left.
func (f foyer) eventuallyLetThrough(t *testing.T, session string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		// The head is enough; the API server may hold the body open.
		ctx, cancel := context.WithCancel(t.Context())
		resp := f.openAs(ctx, t, session, "/k8s/api/v1/configmaps")
		cancel()
		if resp.StatusCode != http.StatusTooManyRequests {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still %d: a slot was never released", resp.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// One session may have so many requests in flight, and no more. The one refused
// never reaches the API server, another session is not affected, and the slot frees
// when a request ends.
func TestConcurrentRequestsPerSession(t *testing.T) {
	api := holdingAPIServer(t)
	m := metrics.New()
	f := newFoyerWith(t, api, bySession{}, frontOptions{config: func(c *testConfig) {
		c.MaxSessionConcurrentRequests, c.Metrics = 2, m
	}})
	first, leave := context.WithCancel(t.Context())
	defer leave()
	for range 2 {
		if resp := f.openAs(first, t, "a", watchTarget); resp.StatusCode != http.StatusOK {
			t.Fatalf("let through: %d", resp.StatusCode)
		}
	}
	assertTooMany(t, f.openAs(t.Context(), t, "a", watchTarget), metrics.BoundSessionConcurrentRequests, 2)
	if n := len(api.received()); n != 2 {
		t.Errorf("the API server received %d requests; the refused one reached it", n)
	}
	if resp := f.openAs(t.Context(), t, "b", watchTarget); resp.StatusCode != http.StatusOK {
		t.Errorf("another session was refused: %d", resp.StatusCode)
	}
	assertMetric(t, m, `krm_foyer_bound_reached_total{bound="session_concurrent_requests"} 1`)
	assertMetric(t, m, `krm_foyer_bound_usage_ratio_bucket{bound="session_concurrent_requests",le="0.5"} 2`)
	assertMetric(t, m, `krm_foyer_bound_usage_ratio_bucket{bound="session_concurrent_requests",le="1"} 3`)
	assertMetric(t, m, `krm_foyer_bound_limit{bound="session_concurrent_requests"} 2`)
	assertMetric(t, m, `krm_foyer_requests_in_flight 3`)

	leave()
	f.eventuallyLetThrough(t, "a")
}

// The replica may have so many requests in flight, whatever their sessions.
func TestConcurrentRequestsPerReplica(t *testing.T) {
	api := holdingAPIServer(t)
	m := metrics.New()
	f := newFoyerWith(t, api, bySession{}, frontOptions{config: func(c *testConfig) {
		c.MaxConcurrentRequests, c.Metrics = 2, m
	}})
	open, leave := context.WithCancel(t.Context())
	defer leave()
	for _, s := range []string{"a", "b"} {
		if resp := f.openAs(open, t, s, watchTarget); resp.StatusCode != http.StatusOK {
			t.Fatalf("let through: %d", resp.StatusCode)
		}
	}
	assertTooMany(t, f.openAs(t.Context(), t, "c", watchTarget), metrics.BoundConcurrentRequests, 2)
	if n := len(api.received()); n != 2 {
		t.Errorf("the API server received %d requests; the refused one reached it", n)
	}
	assertMetric(t, m, `krm_foyer_bound_reached_total{bound="concurrent_requests"} 1`)
	assertMetric(t, m, `krm_foyer_bound_limit{bound="concurrent_requests"} 2`)
	leave()
	f.eventuallyLetThrough(t, "c")
}

// However a request ends, its slot is released. With a limit of one, each way of
// ending is followed by a request that must be let through, and nothing stays in
// flight.
func TestEverySlotIsReleased(t *testing.T) {
	for name, tc := range map[string]struct {
		upstream http.HandlerFunc
		config   func(*testConfig)
		creds    gate.Credentials
		end      func(t *testing.T, f foyer)
	}{
		"the answer completes": {
			end: func(t *testing.T, f foyer) {
				_, _ = io.ReadAll(f.openAs(t.Context(), t, "a", "/k8s/api/v1/configmaps").Body)
			},
		},
		"the browser goes away": {
			upstream: holdingHandler,
			end: func(t *testing.T, f foyer) {
				ctx, cancel := context.WithCancel(t.Context())
				f.openAs(ctx, t, "a", watchTarget)
				cancel()
			},
		},
		"the session ends": {
			upstream: holdingHandler,
			creds:    endedSession{},
			config:   func(c *testConfig) { c.SessionCheckInterval = checkEvery },
			end:      func(t *testing.T, f foyer) { f.endsAborted(t, "a", watchTarget) },
		},
		"the response duration is up": {
			upstream: holdingHandler,
			config:   func(c *testConfig) { c.MaxResponseDuration = 50 * time.Millisecond },
			end:      func(t *testing.T, f foyer) { f.endsAborted(t, "a", watchTarget) },
		},
		"the answer is held back": {
			upstream: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(w, "<script>")
			},
			end: func(t *testing.T, f foyer) {
				if resp := f.openAs(t.Context(), t, "a", "/k8s/api/v1/configmaps"); resp.StatusCode != http.StatusBadGateway {
					t.Fatalf("status %d, want 502", resp.StatusCode)
				}
			},
		},
		"the API server drops the connection": {
			upstream: func(w http.ResponseWriter, _ *http.Request) {
				conn, _, err := http.NewResponseController(w).Hijack()
				if err == nil {
					_ = conn.Close()
				}
			},
			end: func(t *testing.T, f foyer) {
				if resp := f.openAs(t.Context(), t, "a", "/k8s/api/v1/configmaps"); resp.StatusCode != http.StatusBadGateway {
					t.Fatalf("status %d, want 502", resp.StatusCode)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			// HTTP/1.1 upstream, which lets a test drop the connection.
			api := newAPIServerWith(t, false, tc.upstream)
			m := metrics.New()
			creds := tc.creds
			if creds == nil {
				creds = bySession{}
			}
			f := newFoyerWith(t, api, creds, frontOptions{config: func(c *testConfig) {
				c.MaxSessionConcurrentRequests, c.Metrics = 1, m
				if tc.config != nil {
					tc.config(c)
				}
			}})
			tc.end(t, f)
			deadline := time.Now().Add(within)
			for !containsLine(scrape(t, m), "krm_foyer_requests_in_flight 0") {
				if time.Now().After(deadline) {
					t.Fatalf("a request stayed in flight:\n%s", scrape(t, m))
				}
				time.Sleep(10 * time.Millisecond)
			}
			if _, ok := creds.(endedSession); !ok {
				f.eventuallyLetThrough(t, "a")
			}
		})
	}
}

func holdingHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"type":"ADDED"}`+"\n")
	_ = http.NewResponseController(w).Flush()
	<-r.Context().Done()
}

// endedSession is a session that has ended by the first check.
type endedSession struct{}

func (endedSession) Token(r *http.Request) (gate.Credential, *interruption.Interruption) {
	return credentials{token: userToken, session: "a", live: func(context.Context) bool { return false }}.Token(r)
}

func containsLine(text, line string) bool {
	return slices.Contains(strings.Split(text, "\n"), line)
}
