package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ConfigButler/krm-foyer/internal/metrics"
)

// scrape returns what Prometheus would read from m.
func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	return w.Body.String()
}

// assertMetric checks that m has sample. Some samples are recorded as a handler
// finishes, which may be after the browser has read the whole response, so it waits
// for the sample a while before failing.
func assertMetric(t *testing.T, m *metrics.Metrics, sample string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !strings.Contains(scrape(t, m), sample+"\n") {
		if time.Now().After(deadline) {
			t.Errorf("metrics lack %q", sample)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A response stays open no longer than the response duration: then it is aborted,
// and the request to the API server is cancelled, however live its session.
func TestResponseDurationIsBounded(t *testing.T) {
	for upName, upstream := range protocols {
		t.Run(upName, func(t *testing.T) {
			const limit = 150 * time.Millisecond
			api, sent, cancelled := streamingAPIServer(t, upstream)
			m := metrics.New()
			s := &session{}
			f := newFoyerWith(t, api, s.credentials(), frontOptions{config: func(c *testConfig) {
				c.MaxResponseDuration, c.SessionCheckInterval, c.Metrics = limit, time.Hour, m
			}})
			start := time.Now()
			body := f.open(t, "/k8s/api/v1/configmaps?watch=1&timeoutSeconds=600")
			<-sent
			readsAborted(t, body)
			if open := time.Since(start); open < limit {
				t.Errorf("cut short after %v, before its limit of %v", open, limit)
			}
			closedWithin(t, cancelled, "the request to the API server was not cancelled")
			if !strings.Contains(f.logs.String(), `"cause":"response_duration"`) {
				t.Errorf("no log line names why the response was cut short:\n%s", f.logs)
			}
			assertMetric(t, m, `krm_foyer_bound_reached_total{bound="response_duration"} 1`)
			assertMetric(t, m, `krm_foyer_responses_cut_short_total{cause="response_duration"} 1`)
			assertMetric(t, m, `krm_foyer_bound_usage_ratio_bucket{bound="response_duration",le="0.9"} 0`)
			assertMetric(t, m, `krm_foyer_bound_usage_ratio_bucket{bound="response_duration",le="1"} 1`)
		})
	}
}

// A response that ends within its duration is not touched, and the share of the
// bound it used is recorded.
func TestAResponseWithinItsDurationIsNotCut(t *testing.T) {
	api := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"kind":"ConfigMapList"}`)
	})
	m := metrics.New()
	f := newFoyerWith(t, api, credentials{token: userToken}, frontOptions{config: func(c *testConfig) { c.Metrics = m }})
	if a := f.get(t, "/k8s/api/v1/configmaps"); a.StatusCode != http.StatusOK || a.Body != `{"kind":"ConfigMapList"}` {
		t.Fatalf("%d %q", a.StatusCode, a.Body)
	}
	assertMetric(t, m, `krm_foyer_bound_usage_ratio_bucket{bound="response_duration",le="0.1"} 1`)
	assertMetric(t, m, `krm_foyer_bound_reached_total{bound="response_duration"} 0`)
	// Without one configured, the limit is the default, in seconds.
	assertMetric(t, m, `krm_foyer_bound_limit{bound="response_duration"} 1800`)
}
