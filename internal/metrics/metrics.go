// Package metrics keeps krm-foyer's Prometheus metrics. Every instrument here has a
// place in the code that records it, and docs/bounds.md, "Metrics", says what each
// means. Names, labels and buckets are an interface people build alerts on: a test
// pins them.
//
// Labels take values from fixed sets only (a bound, a cause, an interruption reason),
// never from a request: under /k8s a path names namespaces and objects.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics records what krm-foyer does. Its methods do nothing on a nil *Metrics, so
// a package under test can leave it out.
type Metrics struct {
	registry      *prometheus.Registry
	interruptions *prometheus.CounterVec
	cutShort      *prometheus.CounterVec
	boundLimit    *prometheus.GaugeVec
	boundUsage    *prometheus.HistogramVec
	boundReached  *prometheus.CounterVec
}

// The bounds, as the bound label of the krm_foyer_bound_* metrics names them.
const (
	BoundResponseDuration = "response_duration"
)

// The causes of krm_foyer_responses_cut_short_total: why krm-foyer cut a response
// short. A bound that cuts responses short is a cause under its own name.
const (
	CauseSessionEnded = "session_ended"
)

// New returns metrics on a registry of their own, with the Go runtime's and the
// process's beside them.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		interruptions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "krm_foyer_interruptions_total",
			Help: "Answers krm-foyer gave instead of the API server's, by the reason in the Krm-Foyer-Interruption header.",
		}, []string{"reason"}),
		cutShort: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "krm_foyer_responses_cut_short_total",
			Help: "Responses krm-foyer cut short, by why: aborted towards the browser and cancelled at the API server.",
		}, []string{"cause"}),
		boundLimit: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "krm_foyer_bound_limit",
			Help: "The configured limit of each bound, in its own unit: seconds, bytes, requests, or requests a second.",
		}, []string{"bound"}),
		boundUsage: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "krm_foyer_bound_usage_ratio",
			Help: "How much of its bound each request or response let through used, from 0 to 1. " +
				"1 means the whole bound was used, not that anything was refused.",
			Buckets: []float64{0.1, 0.25, 0.5, 0.75, 0.9, 1},
		}, []string{"bound"}),
		boundReached: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "krm_foyer_bound_reached_total",
			Help: "Requests refused, and responses cut short, because a bound was reached.",
		}, []string{"bound"}),
	}
	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.interruptions,
		m.cutShort,
		m.boundLimit,
		m.boundUsage,
		m.boundReached,
	)
	// Every cause is there from the start, so a dashboard shows a zero rather than
	// no data.
	for _, cause := range []string{CauseSessionEnded, BoundResponseDuration} {
		m.cutShort.WithLabelValues(cause)
	}
	return m
}

// Handler serves the metrics in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Interruption counts one answer krm-foyer gave instead of the API server's.
func (m *Metrics) Interruption(reason string) {
	if m == nil {
		return
	}
	m.interruptions.WithLabelValues(reason).Inc()
}

// CutShort counts one response krm-foyer cut short, and why.
func (m *Metrics) CutShort(cause string) {
	if m == nil {
		return
	}
	m.cutShort.WithLabelValues(cause).Inc()
}

// BoundLimit records the configured limit of a bound, and starts its count of
// refusals at zero.
func (m *Metrics) BoundLimit(bound string, limit float64) {
	if m == nil {
		return
	}
	m.boundLimit.WithLabelValues(bound).Set(limit)
	m.boundReached.WithLabelValues(bound)
}

// BoundUsage records how much of its bound one request or response used, as a
// fraction of the limit. Past the limit counts as all of it.
func (m *Metrics) BoundUsage(bound string, ratio float64) {
	if m == nil {
		return
	}
	m.boundUsage.WithLabelValues(bound).Observe(min(ratio, 1))
}

// BoundReached counts one request refused, or one response cut short, by a bound.
func (m *Metrics) BoundReached(bound string) {
	if m == nil {
		return
	}
	m.boundReached.WithLabelValues(bound).Inc()
}
