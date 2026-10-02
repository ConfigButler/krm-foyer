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
	got = scrape(t, m)
	for _, want := range []string{
		"# TYPE krm_foyer_interruptions_total counter",
		`krm_foyer_interruptions_total{reason="Unauthorized"} 2`,
		`krm_foyer_interruptions_total{reason="BadRequest"} 1`,
		`krm_foyer_responses_cut_short_total{cause="session_ended"} 1`,
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
}
