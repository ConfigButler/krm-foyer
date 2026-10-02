// Package gate is what every request to the API half passes before it may reach the
// API server, and what watches its response while it is open: the user's credential,
// the request rate and concurrency bounds, and the guard that cuts a response short
// when its session ends or its duration is up. /k8s and /stream share one gate, so a
// session's request rate is one budget for both. See docs/bounds.md.
package gate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ConfigButler/krm-foyer/internal/interruption"
	"github.com/ConfigButler/krm-foyer/internal/metrics"
)

// Credentials gives the API half the token of the user behind a request. It is the
// only way a token reaches it. Login and sessions sit behind it; there is no other
// source, no default and no service account.
type Credentials interface {
	// Token returns the user's credential, or the interruption to answer instead:
	// no signed-in user, a request the session refuses (a mutation without CSRF
	// proof, say), or a session that could not be checked. The gate answers with
	// it as it is.
	Token(r *http.Request) (Credential, *interruption.Interruption)
}

// Credential is the user's token, and how to tell whether it may still be used.
type Credential struct {
	// Token is the user's bearer token.
	Token string
	// Session names the session the token came from, for the bounds kept per
	// session. It is opaque, and never logged or sent anywhere.
	Session string
	// Live reports whether the token may still be used, without counting as use:
	// false once the session it came from has ended, and false when that cannot be
	// told before ctx ends. Every open response asks it once per session-check
	// interval, and is cut short when it says no. Nil counts as no.
	Live func(ctx context.Context) bool
}

// Config is the gate's credential source and its bounds.
type Config struct {
	// Credentials is where the user's token comes from.
	Credentials Credentials
	// Logger receives one line per interruption and per response cut short. Nil
	// discards them.
	Logger *slog.Logger
	// Metrics counts them. Nil counts nothing.
	Metrics *metrics.Metrics
	// SessionCheckInterval is how often an open response asks whether its session
	// is still live. Zero means DefaultSessionCheckInterval.
	SessionCheckInterval time.Duration
	// MaxResponseDuration is how long a response may stay open. Zero means
	// DefaultMaxResponseDuration.
	MaxResponseDuration time.Duration
	// MaxSessionConcurrentRequests and MaxConcurrentRequests are how many requests
	// may be in flight at once for one session, and for this replica. Zero means
	// the defaults below.
	MaxSessionConcurrentRequests, MaxConcurrentRequests int
	// MaxSessionStreams and MaxStreams are how many streams may be open at once for
	// one session, and for this replica, counted apart from requests. Zero means the
	// defaults below.
	MaxSessionStreams, MaxStreams int
	// SessionRequestRate is how many requests a second one session may send, in
	// bursts of up to SessionRequestBurst. Zero means the defaults below.
	SessionRequestRate  float64
	SessionRequestBurst int
	// Now is the clock the request rate is measured by. Nil means time.Now.
	Now func() time.Time
}

// The request rate when none is configured. See docs/bounds.md for why.
const (
	DefaultSessionRequestRate  = 20
	DefaultSessionRequestBurst = 100
)

// The concurrency limits when none is configured. See docs/bounds.md for why.
const (
	DefaultMaxSessionConcurrentRequests = 64
	DefaultMaxConcurrentRequests        = 2000
)

// The stream limits when none is configured. See docs/bounds.md for why.
const (
	DefaultMaxSessionStreams = 32
	DefaultMaxStreams        = 2000
)

// DefaultMaxResponseDuration is the response duration when none is configured: the
// shortest time the API server keeps a watch open that names no timeoutSeconds.
const DefaultMaxResponseDuration = 30 * time.Minute

// DefaultSessionCheckInterval is the session-check interval when none is configured.
const DefaultSessionCheckInterval = 5 * time.Second

// Gate lets requests through to the API server. Create it with New.
type Gate struct {
	credentials Credentials
	logger      *slog.Logger
	metrics     *metrics.Metrics
	checkEvery  time.Duration
	maxDuration time.Duration
	requests    *concurrency
	streams     *concurrency
	rate        *rate
}

// New returns a gate with cfg's credential source and bounds.
func New(cfg Config) (*Gate, error) {
	if cfg.Credentials == nil {
		return nil, errors.New("no credential source")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	checkEvery := cfg.SessionCheckInterval
	if checkEvery == 0 {
		checkEvery = DefaultSessionCheckInterval
	}
	if checkEvery < 0 {
		return nil, fmt.Errorf("session-check interval must be positive, got %v", checkEvery)
	}
	maxDuration := cfg.MaxResponseDuration
	if maxDuration == 0 {
		maxDuration = DefaultMaxResponseDuration
	}
	if maxDuration < 0 {
		return nil, fmt.Errorf("response duration must be positive, got %v", maxDuration)
	}
	cfg.Metrics.BoundLimit(metrics.BoundResponseDuration, maxDuration.Seconds())
	perSession, total := cfg.MaxSessionConcurrentRequests, cfg.MaxConcurrentRequests
	if perSession == 0 {
		perSession = DefaultMaxSessionConcurrentRequests
	}
	if total == 0 {
		total = DefaultMaxConcurrentRequests
	}
	if perSession < 0 || total < 0 {
		return nil, fmt.Errorf("concurrency limits must be positive, got %d per session and %d in all", perSession, total)
	}
	sessionStreams, allStreams := cfg.MaxSessionStreams, cfg.MaxStreams
	if sessionStreams == 0 {
		sessionStreams = DefaultMaxSessionStreams
	}
	if allStreams == 0 {
		allStreams = DefaultMaxStreams
	}
	if sessionStreams < 0 || allStreams < 0 {
		return nil, fmt.Errorf("stream limits must be positive, got %d per session and %d in all", sessionStreams, allStreams)
	}
	perSecond, burst := cfg.SessionRequestRate, cfg.SessionRequestBurst
	if perSecond == 0 {
		perSecond = DefaultSessionRequestRate
	}
	if burst == 0 {
		burst = DefaultSessionRequestBurst
	}
	if perSecond < 0 || burst < 0 {
		return nil, fmt.Errorf("the request rate must be positive, got %v a second in bursts of %d", perSecond, burst)
	}
	return &Gate{
		credentials: cfg.Credentials,
		logger:      logger,
		metrics:     cfg.Metrics,
		checkEvery:  checkEvery,
		maxDuration: maxDuration,
		requests:    newConcurrency(requests, perSession, total, cfg.Metrics),
		streams:     newConcurrency(streams, sessionStreams, allStreams, cfg.Metrics),
		rate:        newRate(perSecond, burst, cfg.Now, cfg.Metrics),
	}, nil
}

// Logger is where the gate logs, for the routes behind it to log alike.
func (g *Gate) Logger() *slog.Logger { return g.logger }

// Metrics is what the gate records into, for the routes behind it to record alike.
// It may be nil.
func (g *Gate) Metrics() *metrics.Metrics { return g.metrics }

// Interrupt answers r with i, instead of the API server, and logs and counts it.
func (g *Gate) Interrupt(w http.ResponseWriter, r *http.Request, i *interruption.Interruption) {
	path, _, _ := strings.Cut(r.RequestURI, "?")
	g.logger.Info("interruption", "status", i.Status, "reason", i.Reason, "message", i.Message,
		"method", r.Method, "path", path)
	g.metrics.Interruption(i.Reason)
	i.Serve(w, r)
}

// Admission is a request the gate let through. Close it when the response has
// ended, exactly once.
type Admission struct {
	// Credential is the user's token, and nothing else may be sent upstream.
	Credential
	// Request is the request let through. Its context ends when the gate cuts the
	// response short, which cancels anything sent upstream with it.
	Request *http.Request

	gate  *Gate
	cut   context.CancelCauseFunc
	close func()
}

// Admit lets r through, or answers it with an interruption and returns nil: no
// signed-in user, a request the session refuses, or a bound reached. From then on
// the response to it is guarded, and cut short when its session ends or its
// duration is up.
func (g *Gate) Admit(w http.ResponseWriter, r *http.Request) *Admission {
	return g.admit(w, r, g.requests)
}

// AdmitStream lets r through as a stream, as Admit does, but counts it against the
// stream limits instead of the request limits. The request rate is one budget for
// both.
func (g *Gate) AdmitStream(w http.ResponseWriter, r *http.Request) *Admission {
	return g.admit(w, r, g.streams)
}

func (g *Gate) admit(w http.ResponseWriter, r *http.Request, slots *concurrency) *Admission {
	cred, refused := g.credentials.Token(r)
	if refused == nil && cred.Token == "" {
		// An empty token would make the request anonymous. Never send one.
		refused = interruption.NotSignedIn()
	}
	if refused != nil {
		g.Interrupt(w, r, refused)
		return nil
	}
	if ok, wait := g.rate.allow(cred.Session); !ok {
		g.Interrupt(w, r, g.rate.refuse(wait))
		return nil
	}
	release, refused := slots.acquire(cred.Session)
	if refused != nil {
		g.Interrupt(w, r, refused)
		return nil
	}

	// From here the request may reach the API server. Its context is cancelled when
	// the gate cuts the response short, which cancels the request upstream too.
	ctx, cut := context.WithCancelCause(r.Context())
	a := &Admission{Credential: cred, Request: r.WithContext(ctx), gate: g, cut: cut}
	stop := g.guard(w, a)
	a.close = func() {
		stop()
		release()
	}
	return a
}

// Close stops guarding the response and frees its slot.
func (a *Admission) Close() { a.close() }

// Cut cuts the response short, for why: the request to the API server is cancelled,
// and with it whatever copies its answer, which aborts the browser's response.
func (a *Admission) Cut(why Cause) {
	path, _, _ := strings.Cut(a.Request.RequestURI, "?")
	a.gate.logger.Info("response cut short", "cause", string(why), "method", a.Request.Method, "path", path)
	a.gate.metrics.CutShort(string(why))
	a.cut(why)
}

// Cause is why krm-foyer cut a response short. Its text is the cause label of
// krm_foyer_responses_cut_short_total.
type Cause string

func (c Cause) Error() string { return string(c) }

// The causes of a response cut short.
const (
	CauseSessionEnded     = Cause(metrics.CauseSessionEnded)
	CauseResponseDuration = Cause(metrics.BoundResponseDuration)
	CauseResponseBytes    = Cause(metrics.BoundResponseBytes)
)

// CutShort reports whether the gate cut short the request ctx belongs to, rather
// than the browser leaving.
func CutShort(ctx context.Context) bool {
	_, ok := context.Cause(ctx).(Cause)
	return ok
}

// guard watches the response to a while it is open, and cuts it short when its
// session ends or its duration is up. A session check that does not answer within
// an interval counts as a session that has ended. The returned function stops the
// guard; it returns once the guard has.
func (g *Gate) guard(w http.ResponseWriter, a *Admission) (stop func()) {
	ctx := a.Request.Context()
	start := time.Now()
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		tick := time.NewTicker(g.checkEvery)
		defer tick.Stop()
		deadline := time.NewTimer(g.maxDuration)
		defer deadline.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-deadline.C:
				g.metrics.BoundReached(metrics.BoundResponseDuration)
				a.Cut(CauseResponseDuration)
			case <-tick.C:
				if g.live(ctx, a.Live) || ctx.Err() != nil {
					continue
				}
				a.Cut(CauseSessionEnded)
			}
			// The response may be blocked writing to a browser that stopped reading.
			// A deadline in the past fails that write, so the handler returns and the
			// abort happens.
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now())
			return
		}
	}()
	return func() {
		a.cut(nil)
		<-exited
		g.metrics.BoundUsage(metrics.BoundResponseDuration, float64(time.Since(start))/float64(g.maxDuration))
	}
}

// live asks whether a session is still live, giving it one interval to answer.
func (g *Gate) live(ctx context.Context, live func(context.Context) bool) bool {
	if live == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, g.checkEvery)
	defer cancel()
	return live(ctx) && ctx.Err() == nil
}
