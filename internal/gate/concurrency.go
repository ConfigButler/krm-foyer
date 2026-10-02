package gate

import (
	"net/http"
	"strconv"
	"sync"

	"github.com/ConfigButler/krm-foyer/internal/interruption"
	"github.com/ConfigButler/krm-foyer/internal/metrics"
)

// concurrency counts what is open of one kind, requests or streams, per session and
// for this replica, and refuses one past either limit. Requests are counted alike,
// watches included: krm-foyer does not tell watches apart (docs/bounds.md, "Native
// watches"). Streams are counted on their own.
type concurrency struct {
	kind              kind
	perSession, total int
	metrics           *metrics.Metrics

	mu       sync.Mutex
	sessions map[string]int
	inFlight int
}

// kind is what a concurrency counts, as its bounds, refusals and metrics name it.
type kind struct {
	// noun is what is counted, in a refusal's message.
	noun string
	// reason is a refusal's reason.
	reason string
	// perSession and total are the bounds, as the metrics name them.
	perSession, total string
	// open counts one in the metrics; done counts it out.
	open func(*metrics.Metrics) (done func())
}

var (
	requests = kind{
		noun: "requests in flight", reason: "TooManyConcurrentRequests",
		perSession: metrics.BoundSessionConcurrentRequests, total: metrics.BoundConcurrentRequests,
		open: (*metrics.Metrics).InFlight,
	}
	streams = kind{
		noun: "streams open", reason: "TooManyStreams",
		perSession: metrics.BoundSessionStreams, total: metrics.BoundStreams,
		open: (*metrics.Metrics).StreamOpen,
	}
)

func newConcurrency(k kind, perSession, total int, m *metrics.Metrics) *concurrency {
	m.BoundLimit(k.perSession, float64(perSession))
	m.BoundLimit(k.total, float64(total))
	return &concurrency{kind: k, perSession: perSession, total: total, metrics: m, sessions: map[string]int{}}
}

// acquire takes a slot for a request of session, or returns the interruption to
// answer instead. release gives the slot back; call it exactly once.
func (c *concurrency) acquire(session string) (release func(), refused *interruption.Interruption) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessions[session] >= c.perSession {
		return nil, c.refuse(c.kind.perSession, c.perSession,
			"this session has "+strconv.Itoa(c.perSession)+" "+c.kind.noun+", the most krm-foyer allows")
	}
	if c.inFlight >= c.total {
		return nil, c.refuse(c.kind.total, c.total,
			"krm-foyer has "+strconv.Itoa(c.total)+" "+c.kind.noun+", the most it allows")
	}
	c.sessions[session]++
	c.inFlight++
	c.metrics.BoundUsage(c.kind.perSession, float64(c.sessions[session])/float64(c.perSession))
	c.metrics.BoundUsage(c.kind.total, float64(c.inFlight)/float64(c.total))
	done := c.kind.open(c.metrics)
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
		Status: http.StatusTooManyRequests, Reason: c.kind.reason, Message: message,
		Causes: []interruption.Cause{{Reason: "BoundReached", Field: bound, Message: strconv.Itoa(limit)}},
	}
}
