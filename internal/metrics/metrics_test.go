package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scrape returns what Prometheus would read from m.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(w.Result().Body)
	if w.Code != http.StatusOK {
		t.Fatalf("scrape: %d\n%s", w.Code, body)
	}
	return string(body)
}

// Names, types and labels are what alerts are written against, so they are pinned
// as scraped. A change here is a breaking change, and docs/bounds.md says so.
func TestNamesAsScraped(t *testing.T) {
	m := New()
	m.Interruption("Unauthorized")
	m.Interruption("Unauthorized")
	m.Interruption("BadRequest")
	got := scrape(t, m)
	// Every cause is there before anything was cut short.
	for _, want := range []string{
		"# TYPE krm_foyer_responses_cut_short_total counter",
		`krm_foyer_responses_cut_short_total{cause="session_ended"} 0`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scrape lacks %q", want)
		}
	}
	m.CutShort(CauseSessionEnded)
	m.StreamOpen()
	closed := m.StreamOpen()
	closed()
	m.UpstreamWatch(IdentityUser)
	m.UpstreamWatch(IdentityShared)()
	m.SharedSubscriptionOpened()
	m.SharedSubscriptionOpened()
	m.SharedSubscriptionOpened()
	m.SharedSubscriptionClosed()
	m.SharedOverflow()
	m.AccessCheck(SourceAPIServer, ResultAllowed, 0.02)
	m.AccessCheck(SourceCache, ResultAllowed, 0)
	m.AccessCheck(SourceCache, ResultAllowed, 0)
	m.AccessCheck(SourceAPIServer, ResultDenied, 0.03)
	m.SubjectReview(SubjectResolved)
	m.BoundLimit(BoundResponseDuration, 1800)
	m.BoundUsage(BoundResponseDuration, 0.2)
	m.BoundUsage(BoundResponseDuration, 3) // past the limit is all of it
	m.BoundReached(BoundResponseDuration)
	got = scrape(t, m)
	for _, want := range []string{
		"# TYPE krm_foyer_interruptions_total counter",
		`krm_foyer_interruptions_total{reason="Unauthorized"} 2`,
		`krm_foyer_interruptions_total{reason="BadRequest"} 1`,
		`krm_foyer_responses_cut_short_total{cause="session_ended"} 1`,
		`krm_foyer_responses_cut_short_total{cause="response_duration"} 0`,
		"# TYPE krm_foyer_bound_limit gauge",
		`krm_foyer_bound_limit{bound="response_duration"} 1800`,
		"# TYPE krm_foyer_bound_usage_ratio histogram",
		`krm_foyer_bound_usage_ratio_bucket{bound="response_duration",le="0.25"} 1`,
		`krm_foyer_bound_usage_ratio_bucket{bound="response_duration",le="0.9"} 1`,
		`krm_foyer_bound_usage_ratio_bucket{bound="response_duration",le="1"} 2`,
		`krm_foyer_bound_usage_ratio_sum{bound="response_duration"} 1.2`,
		"# TYPE krm_foyer_streams_open gauge",
		"krm_foyer_streams_open 1",
		"# TYPE krm_foyer_upstream_watches_open gauge",
		`krm_foyer_upstream_watches_open{identity="user"} 1`,
		`krm_foyer_upstream_watches_open{identity="shared"} 0`,
		"# TYPE krm_foyer_shared_subscriptions_open gauge",
		"krm_foyer_shared_subscriptions_open 2",
		"# TYPE krm_foyer_shared_overflows_total counter",
		"krm_foyer_shared_overflows_total 1",
		"# TYPE krm_foyer_access_checks_total counter",
		`krm_foyer_access_checks_total{result="allowed",source="api_server"} 1`,
		`krm_foyer_access_checks_total{result="allowed",source="cache"} 2`,
		`krm_foyer_access_checks_total{result="denied",source="api_server"} 1`,
		`krm_foyer_access_checks_total{result="error",source="api_server"} 0`,
		"# TYPE krm_foyer_access_check_duration_seconds histogram",
		`krm_foyer_access_check_duration_seconds_bucket{le="0.025"} 1`,
		"krm_foyer_access_check_duration_seconds_count 2",
		"# TYPE krm_foyer_subject_reviews_total counter",
		`krm_foyer_subject_reviews_total{result="resolved"} 1`,
		`krm_foyer_subject_reviews_total{result="refused"} 0`,
		"# TYPE krm_foyer_bound_reached_total counter",
		`krm_foyer_bound_reached_total{bound="response_duration"} 1`,
		// The Go runtime's and the process's come along: a leaked response shows
		// as goroutines that never go away.
		"# TYPE go_goroutines gauge",
		"# TYPE process_open_fds gauge",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scrape lacks %q", want)
		}
	}
}

// A package under test may leave its metrics out.
func TestNilRecordsNothing(t *testing.T) {
	var m *Metrics
	m.Interruption("Unauthorized")
	m.CutShort(CauseSessionEnded)
	m.BoundLimit(BoundResponseDuration, 1)
	m.BoundUsage(BoundResponseDuration, 1)
	m.BoundReached(BoundResponseDuration)
	m.StreamOpen()()
	m.UpstreamWatch(IdentityUser)()
	m.SharedSubscriptionOpened()
	m.SharedSubscriptionClosed()
	m.SharedOverflow()
	m.AccessCheck(SourceAPIServer, ResultAllowed, 1)
	m.SubjectReview(SubjectResolved)
}
