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
}

// New returns metrics on a registry of their own, with the Go runtime's and the
// process's beside them.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		interruptions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "krm_foyer_interruptions_total",
			Help: "Answers krm-foyer gave instead of the API server's, by the reason in the Krm-Foyer-Interruption header.",
		}, []string{"reason"}),
	}
	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.interruptions,
	)
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
