package proxy

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ConfigButler/krm-foyer/internal/interruption"
	"github.com/ConfigButler/krm-foyer/internal/metrics"
)

// clock is a clock a test moves by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// requestAs sends a GET as session and reads the answer to its end.
func (f foyer) requestAs(t *testing.T, session string) answer {
	t.Helper()
	return f.request(t, http.MethodGet, "/k8s/api/v1/configmaps", nil, http.Header{sessionHeader: {session}})
}

// A session may send a burst of requests at once, then so many a second. The one
// past it gets 429 with Retry-After, and never reaches the API server; another
// session is not affected; and after waiting, the session may send again.
func TestRequestRatePerSession(t *testing.T) {
	api := newAPIServer(t, nil)
	m := metrics.New()
	c := newClock()
	f := newFoyerWith(t, api, bySession{}, frontOptions{config: func(cfg *testConfig) {
		cfg.SessionRequestRate, cfg.SessionRequestBurst, cfg.Now, cfg.Metrics = 1, 3, c.Now, m
	}})
	for i := range 3 {
		if a := f.requestAs(t, "a"); a.StatusCode != http.StatusOK {
			t.Fatalf("request %d of a burst of 3: %d", i+1, a.StatusCode)
		}
	}
	a := f.requestAs(t, "a")
	if a.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("past the burst: %d, want 429", a.StatusCode)
	}
	s := readStatus(t, a)
	if s.Reason != "RequestRateExceeded" || s.Details == nil || len(s.Details.Causes) != 1 ||
		s.Details.Causes[0].Reason != "BoundReached" || s.Details.Causes[0].Field != metrics.BoundSessionRequestBurst ||
		s.Details.Causes[0].Message != "3" {
		t.Errorf("Status %+v does not name the bound and its limit", s)
	}
	if got := a.Header.Values("Retry-After"); len(got) != 1 || got[0] != "1" {
		t.Errorf("Retry-After %q, want 1", got)
	}
	if n := len(api.received()); n != 3 {
		t.Errorf("the API server received %d requests; the refused one reached it", n)
	}
	// A person who opened the URL in a tab gets the same 429 as a page, and the same
	// Retry-After.
	page := f.request(t, http.MethodGet, "/k8s/api/v1/configmaps", nil, http.Header{sessionHeader: {"a"}, "Sec-Fetch-Dest": {"document"}})
	if page.StatusCode != http.StatusTooManyRequests || !strings.Contains(page.Body, "Too many requests") ||
		page.Header.Get("Retry-After") != "1" || page.Header.Get(interruption.Header) != "RequestRateExceeded" {
		t.Errorf("navigation: %d, Retry-After %q:\n%s", page.StatusCode, page.Header.Get("Retry-After"), page.Body)
	}
	if a := f.requestAs(t, "b"); a.StatusCode != http.StatusOK {
		t.Errorf("another session was refused: %d", a.StatusCode)
	}
	c.Advance(time.Second)
	if a := f.requestAs(t, "a"); a.StatusCode != http.StatusOK {
		t.Errorf("after Retry-After: %d", a.StatusCode)
	}

	assertMetric(t, m, `krm_foyer_bound_limit{bound="session_request_rate"} 1`)
	assertMetric(t, m, `krm_foyer_bound_limit{bound="session_request_burst"} 3`)
	assertMetric(t, m, `krm_foyer_bound_reached_total{bound="session_request_burst"} 2`) // the request, and the page
	// The share of the burst spent, after each of a's first three requests: a third,
	// two thirds, all of it. Then b's first, and a's after a second's refill.
	assertMetric(t, m, `krm_foyer_bound_usage_ratio_bucket{bound="session_request_burst",le="0.5"} 2`)
	assertMetric(t, m, `krm_foyer_bound_usage_ratio_bucket{bound="session_request_burst",le="0.75"} 3`)
	assertMetric(t, m, `krm_foyer_bound_usage_ratio_bucket{bound="session_request_burst",le="1"} 5`)
}

// Retry-After is the wait for the next request to be let through, rounded up to
// whole seconds, and never zero.
func TestRetryAfterIsTheWaitRoundedUp(t *testing.T) {
	for _, tc := range []struct {
		rate   float64
		waited time.Duration
		want   string
	}{
		{rate: 1, want: "1"},
		{rate: 0.25, want: "4"},
		{rate: 0.25, waited: time.Second, want: "3"},
		{rate: 0.4, want: "3"},
		{rate: 100, want: "1"},
	} {
		c := newClock()
		f := newFoyerWith(t, newAPIServer(t, nil), bySession{}, frontOptions{config: func(cfg *testConfig) {
			cfg.SessionRequestRate, cfg.SessionRequestBurst, cfg.Now = tc.rate, 1, c.Now
		}})
		f.requestAs(t, "a")
		c.Advance(tc.waited)
		if got := f.requestAs(t, "a").Header.Get("Retry-After"); got != tc.want {
			t.Errorf("rate %v, waited %v: Retry-After %q, want %q", tc.rate, tc.waited, got, tc.want)
		}
	}
}
