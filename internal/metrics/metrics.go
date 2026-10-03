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
	inFlight      prometheus.Gauge
	streamsOpen   prometheus.Gauge
	upstream      *prometheus.GaugeVec
	subscriptions prometheus.Gauge
	overflows     prometheus.Counter
	accessChecks  *prometheus.CounterVec
	checkSeconds  prometheus.Histogram
	subjects      *prometheus.CounterVec
}

// The bounds, as the bound label of the krm_foyer_bound_* metrics names them.
const (
	BoundResponseDuration          = "response_duration"
	BoundSessionConcurrentRequests = "session_concurrent_requests"
	BoundConcurrentRequests        = "concurrent_requests"
	// BoundSessionRequestRate has a limit only: the rate a session's burst refills
	// at. Usage and refusals are counted against the burst.
	BoundSessionRequestRate  = "session_request_rate"
	BoundSessionRequestBurst = "session_request_burst"
	BoundResponseBytes       = "response_bytes"
	// BoundSessionStreams and BoundStreams are how many streams one session, and
	// this replica, may have open: counted apart from requests through /k8s.
	BoundSessionStreams = "session_streams"
	BoundStreams        = "streams"
)

// The causes of krm_foyer_responses_cut_short_total: why krm-foyer cut a response
// short. A bound that cuts responses short is a cause under its own name.
const (
	CauseSessionEnded = "session_ended"
)

// Whose identity an upstream watch is opened with, as the identity label of
// krm_foyer_upstream_watches_open names it.
const (
	// IdentityUser is a watch opened with the signed-in user's own token, for one
	// stream.
	IdentityUser = "user"
	// IdentityShared is a shared watch, opened once with the shared-watch identity
	// and read by every stream of its scope.
	IdentityShared = "shared"
)

// Where an access decision for a shared watch came from, and what it was: the
// source and result labels of krm_foyer_access_checks_total.
const (
	SourceAPIServer = "api_server"
	SourceCache     = "cache"
	ResultAllowed   = "allowed"
	ResultDenied    = "denied"
	ResultError     = "error"
)

// What a SelfSubjectReview that resolves a subscriber's Kubernetes identity came to:
// the result label of krm_foyer_subject_reviews_total.
const (
	SubjectResolved = "resolved"
	SubjectRefused  = "refused"
	SubjectError    = "error"
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
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "krm_foyer_requests_in_flight",
			Help: "Requests through /k8s open now, watches included.",
		}),
		streamsOpen: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "krm_foyer_streams_open",
			Help: "Streams through /stream open now.",
		}),
		upstream: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "krm_foyer_upstream_watches_open",
			Help: "Watches open at the API server now, by whose identity opened them: a user's, for one stream, " +
				"or the shared-watch identity's, for every stream of a scope.",
		}, []string{"identity"}),
		subscriptions: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "krm_foyer_shared_subscriptions_open",
			Help: "Streams reading from a shared watch now. Divided by the shared upstream watches, the reuse of each.",
		}),
		overflows: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "krm_foyer_shared_overflows_total",
			Help: "Times a stream fell so far behind its shared watch that it was given a fresh snapshot from the cache.",
		}),
		accessChecks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "krm_foyer_access_checks_total",
			Help: "Decisions whether a user may list and watch a shared scope, by where they came from " +
				"(the API server's SubjectAccessReviews, or a recent decision) and what they were.",
		}, []string{"source", "result"}),
		checkSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "krm_foyer_access_check_duration_seconds",
			Help:    "How long an access decision took that asked the API server: its list and watch SubjectAccessReviews.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}),
		subjects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "krm_foyer_subject_reviews_total",
			Help: "SelfSubjectReviews that resolved who a user is to Kubernetes before a shared stream, by result.",
		}, []string{"result"}),
	}
	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.interruptions,
		m.cutShort,
		m.boundLimit,
		m.boundUsage,
		m.boundReached,
		m.inFlight,
		m.streamsOpen,
		m.upstream,
		m.subscriptions,
		m.overflows,
		m.accessChecks,
		m.checkSeconds,
		m.subjects,
	)
	// Every cause is there from the start, so a dashboard shows a zero rather than
	// no data.
	for _, cause := range []string{CauseSessionEnded, BoundResponseDuration, BoundResponseBytes} {
		m.cutShort.WithLabelValues(cause)
	}
	for _, identity := range []string{IdentityUser, IdentityShared} {
		m.upstream.WithLabelValues(identity)
	}
	for _, source := range []string{SourceAPIServer, SourceCache} {
		for _, result := range []string{ResultAllowed, ResultDenied, ResultError} {
			if source == SourceCache && result == ResultError {
				continue // an error is never kept
			}
			m.accessChecks.WithLabelValues(source, result)
		}
	}
	for _, result := range []string{SubjectResolved, SubjectRefused, SubjectError} {
		m.subjects.WithLabelValues(result)
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

// InFlight counts a request through /k8s in flight; done counts it out again.
func (m *Metrics) InFlight() (done func()) {
	if m == nil {
		return func() {}
	}
	m.inFlight.Inc()
	return m.inFlight.Dec
}

// StreamOpen counts a stream through /stream open; done counts it out again.
func (m *Metrics) StreamOpen() (done func()) {
	if m == nil {
		return func() {}
	}
	m.streamsOpen.Inc()
	return m.streamsOpen.Dec
}

// UpstreamWatch counts a watch open at the API server, opened with identity (see
// IdentityUser); done counts it out again.
func (m *Metrics) UpstreamWatch(identity string) (done func()) {
	if m == nil {
		return func() {}
	}
	g := m.upstream.WithLabelValues(identity)
	g.Inc()
	return g.Dec
}

// SharedSubscriptionOpened counts a stream that started reading from a shared watch,
// and SharedSubscriptionClosed one that stopped.
func (m *Metrics) SharedSubscriptionOpened() {
	if m == nil {
		return
	}
	m.subscriptions.Inc()
}

// SharedSubscriptionClosed: see SharedSubscriptionOpened.
func (m *Metrics) SharedSubscriptionClosed() {
	if m == nil {
		return
	}
	m.subscriptions.Dec()
}

// SharedOverflow counts a stream that fell behind its shared watch.
func (m *Metrics) SharedOverflow() {
	if m == nil {
		return
	}
	m.overflows.Inc()
}

// AccessCheck counts one access decision for a shared scope, from source with
// result. One that asked the API server took seconds.
func (m *Metrics) AccessCheck(source, result string, seconds float64) {
	if m == nil {
		return
	}
	m.accessChecks.WithLabelValues(source, result).Inc()
	if source == SourceAPIServer {
		m.checkSeconds.Observe(seconds)
	}
}

// SubjectReview counts one SelfSubjectReview, by result.
func (m *Metrics) SubjectReview(result string) {
	if m == nil {
		return
	}
	m.subjects.WithLabelValues(result).Inc()
}
