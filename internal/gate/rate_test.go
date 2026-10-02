package gate

import (
	"math/rand/v2"
	"sync"
	"testing"
	"time"
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

// The property, stated from outside the bucket: in any stretch of time, a session
// gets at most its burst plus the rate times the stretch's length let through; and a
// session that never sends faster than the rate is never refused. Random request
// times, many seeds.
func TestRequestRateProperty(t *testing.T) {
	const rate, burst = 5.0, 4
	for seed := range uint64(200) {
		rng := rand.New(rand.NewPCG(seed, seed)) //nolint:gosec // reproducible request times, not secrets
		c := newClock()
		r := newRate(rate, burst, c.Now, nil)
		var allowed []time.Time
		for range 300 {
			// Gaps from nothing to a little over the refill time of one request.
			c.Advance(time.Duration(rng.Float64() * float64(time.Second) / rate * 1.2))
			if ok, _ := r.allow("a"); ok {
				allowed = append(allowed, c.Now())
			}
		}
		for i := range allowed {
			for j := i; j < len(allowed); j++ {
				stretch := allowed[j].Sub(allowed[i]).Seconds()
				if n := float64(j - i + 1); n > burst+rate*stretch+1e-9 {
					t.Fatalf("seed %d: %v requests let through in %.3fs; at most %v", seed, n, stretch, burst+rate*stretch)
				}
			}
		}

		steady := newRate(rate, burst, c.Now, nil)
		for i := range 100 {
			c.Advance(time.Duration(float64(time.Second) / rate))
			if ok, _ := steady.allow("b"); !ok {
				t.Fatalf("seed %d: request %d at exactly the rate was refused", seed, i)
			}
		}
	}
}

// A session that stops sending is forgotten once its burst has refilled, so the
// buckets of past sessions do not pile up.
func TestIdleSessionsAreForgotten(t *testing.T) {
	c := newClock()
	r := newRate(1, 10, c.Now, nil)
	for _, s := range []string{"a", "b", "c"} {
		r.allow(s)
	}
	c.Advance(11 * time.Second) // past the sweep, and past a full refill
	r.allow("d")
	if n := r.sessions(); n != 1 {
		t.Errorf("%d sessions remembered, want only the one still sending", n)
	}
}
