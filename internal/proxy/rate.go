package proxy

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ConfigButler/krm-foyer/internal/interruption"
	"github.com/ConfigButler/krm-foyer/internal/metrics"
)

// rate is a token bucket per session: a session may send a burst of requests at
// once, then so many a second. It stops a runaway page, not a determined user, who
// can open another session (docs/bounds.md).
type rate struct {
	perSecond float64
	burst     int
	now       func() time.Time
	metrics   *metrics.Metrics

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newRate(perSecond float64, burst int, now func() time.Time, m *metrics.Metrics) *rate {
	if now == nil {
		now = time.Now
	}
	m.BoundLimit(metrics.BoundSessionRequestRate, perSecond)
	m.BoundLimit(metrics.BoundSessionRequestBurst, float64(burst))
	return &rate{perSecond: perSecond, burst: burst, now: now, metrics: m, buckets: map[string]*bucket{}, lastSweep: now()}
}

// allow takes one request from session's bucket, or reports how long until one is
// there.
func (r *rate) allow(session string) (ok bool, wait time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.sweep(now)
	b := r.buckets[session]
	if b == nil {
		b = &bucket{tokens: float64(r.burst), at: now}
		r.buckets[session] = b
	}
	b.tokens = min(float64(r.burst), b.tokens+now.Sub(b.at).Seconds()*r.perSecond)
	b.at = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / r.perSecond * float64(time.Second))
	}
	b.tokens--
	r.metrics.BoundUsage(metrics.BoundSessionRequestBurst, 1-b.tokens/float64(r.burst))
	return true, 0
}

// sweep forgets the sessions whose bucket has refilled since they last sent: a full
// bucket is the same as none. It runs once per refill time. The caller holds mu.
func (r *rate) sweep(now time.Time) {
	refill := time.Duration(float64(r.burst) / r.perSecond * float64(time.Second))
	if now.Sub(r.lastSweep) < refill {
		return
	}
	for s, b := range r.buckets {
		if now.Sub(b.at) >= refill {
			delete(r.buckets, s)
		}
	}
	r.lastSweep = now
}

func (r *rate) sessions() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buckets)
}

// refuse answers a request past the session's rate. It was refused before anything
// was sent, so it did not reach Kubernetes and may be sent again after Retry-After.
func (r *rate) refuse(wait time.Duration) *interruption.Interruption {
	r.metrics.BoundReached(metrics.BoundSessionRequestBurst)
	seconds := max(1, int(math.Ceil(wait.Seconds())))
	return &interruption.Interruption{
		Status: http.StatusTooManyRequests, Reason: "RequestRateExceeded",
		Message: "this session sent more than " + strconv.Itoa(r.burst) + " requests at once, or more than " +
			strconv.FormatFloat(r.perSecond, 'f', -1, 64) + " a second; try again in " + strconv.Itoa(seconds) + "s",
		Causes:     []interruption.Cause{{Reason: "BoundReached", Field: metrics.BoundSessionRequestBurst, Message: strconv.Itoa(r.burst)}},
		RetryAfter: seconds,
	}
}
