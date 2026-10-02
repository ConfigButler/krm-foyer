package proxy

import (
	"net/http"
	"strconv"
	"sync"

	"github.com/ConfigButler/krm-foyer/internal/interruption"
	"github.com/ConfigButler/krm-foyer/internal/metrics"
)

// concurrency counts the requests in flight, per session and for this replica, and
// refuses one past either limit. It counts every request alike, watches included:
// krm-foyer does not tell watches apart (docs/bounds.md, "Native watches").
type concurrency struct {
	perSession, total int
	metrics           *metrics.Metrics

	mu       sync.Mutex
	sessions map[string]int
	inFlight int
}

func newConcurrency(perSession, total int, m *metrics.Metrics) *concurrency {
	m.BoundLimit(metrics.BoundSessionConcurrentRequests, float64(perSession))
	m.BoundLimit(metrics.BoundConcurrentRequests, float64(total))
	return &concurrency{perSession: perSession, total: total, metrics: m, sessions: map[string]int{}}
}

// acquire takes a slot for a request of session, or returns the interruption to
// answer instead. release gives the slot back; call it exactly once.
func (c *concurrency) acquire(session string) (release func(), refused *interruption.Interruption) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessions[session] >= c.perSession {
		return nil, c.refuse(metrics.BoundSessionConcurrentRequests, c.perSession,
			"this session has "+strconv.Itoa(c.perSession)+" requests in flight, the most krm-foyer allows")
	}
	if c.inFlight >= c.total {
		return nil, c.refuse(metrics.BoundConcurrentRequests, c.total,
			"krm-foyer has "+strconv.Itoa(c.total)+" requests in flight, the most it allows")
	}
	c.sessions[session]++
	c.inFlight++
	c.metrics.BoundUsage(metrics.BoundSessionConcurrentRequests, float64(c.sessions[session])/float64(c.perSession))
	c.metrics.BoundUsage(metrics.BoundConcurrentRequests, float64(c.inFlight)/float64(c.total))
	done := c.metrics.InFlight()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.sessions[session]--; c.sessions[session] == 0 {
				delete(c.sessions, session)
			}
			c.inFlight--
			done()
		})
	}, nil
}

// refuse answers a request past a concurrency limit. It was refused before anything
// was sent, so the request did not reach Kubernetes and may be sent again; there is
// no Retry-After, since nobody knows when a slot frees.
func (c *concurrency) refuse(bound string, limit int, message string) *interruption.Interruption {
	c.metrics.BoundReached(bound)
	return &interruption.Interruption{
		Status: http.StatusTooManyRequests, Reason: "TooManyConcurrentRequests", Message: message,
		Causes: []interruption.Cause{{Reason: "BoundReached", Field: bound, Message: strconv.Itoa(limit)}},
	}
}
