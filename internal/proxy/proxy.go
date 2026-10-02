// Package proxy is krm-foyer's API half: it sends a browser's /k8s request to the
// API server with the user's own token, and lets the API server decide. It makes
// no access decision of its own. What it does refuse is listed as interruptions in
// docs/design.md.
package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/ConfigButler/krm-foyer/internal/interruption"
	"github.com/ConfigButler/krm-foyer/internal/metrics"
)

// Credentials gives the proxy the token of the user behind a request. It is the
// only way a token reaches the proxy. Login and sessions sit behind it; there is no
// other source, no default and no service account.
type Credentials interface {
	// Token returns the user's credential, or the interruption to answer instead:
	// no signed-in user, a request the session refuses (a mutation without CSRF
	// proof, say), or a session that could not be checked. The proxy answers with
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

// Config is what the proxy needs to reach one API server.
type Config struct {
	// Server is the API server's URL: https, with no path.
	Server *url.URL
	// RootCAs verifies the API server's certificate. Nil means the system roots.
	RootCAs *x509.CertPool
	// Credentials is where the user's token comes from.
	Credentials Credentials
	// Logger receives one line per interruption. Nil discards them.
	Logger *slog.Logger
	// Metrics counts interruptions. Nil counts nothing.
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
	// SessionRequestRate is how many requests a second one session may send, in
	// bursts of up to SessionRequestBurst. Zero means the defaults below.
	SessionRequestRate  float64
	SessionRequestBurst int
	// MaxResponseBytes is the most decoded bytes a response may have. Zero means
	// DefaultMaxResponseBytes.
	MaxResponseBytes int64
	// Now is the clock the request rate is measured by. Nil means time.Now.
	Now func() time.Time
}

// DefaultMaxResponseBytes is the response-byte limit when none is configured.
const DefaultMaxResponseBytes = 32 << 20

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

// DefaultMaxResponseDuration is the response duration when none is configured: the
// shortest time the API server keeps a watch open that names no timeoutSeconds.
const DefaultMaxResponseDuration = 30 * time.Minute

// DefaultSessionCheckInterval is the session-check interval when none is configured.
const DefaultSessionCheckInterval = 5 * time.Second

// Proxy serves Prefix. Create it with New.
type Proxy struct {
	server      url.URL
	transport   http.RoundTripper
	credentials Credentials
	logger      *slog.Logger
	metrics     *metrics.Metrics
	checkEvery  time.Duration
	maxDuration time.Duration
	concurrency *concurrency
	rate        *rate
	maxBytes    int64
}

// New returns a proxy to cfg.Server.
func New(cfg Config) (*Proxy, error) {
	if cfg.Server == nil || cfg.Server.Scheme != "https" || cfg.Server.Host == "" ||
		(cfg.Server.Path != "" && cfg.Server.Path != "/") || cfg.Server.RawQuery != "" || cfg.Server.User != nil {
		return nil, fmt.Errorf("API server must be an https URL with no path, query or user info, got %q", cfg.Server)
	}
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
	maxBytes := cfg.MaxResponseBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxResponseBytes
	}
	if maxBytes < 0 {
		return nil, fmt.Errorf("the response-byte limit must be positive, got %d", maxBytes)
	}
	cfg.Metrics.BoundLimit(metrics.BoundResponseBytes, float64(maxBytes))
	return &Proxy{
		server: url.URL{Scheme: cfg.Server.Scheme, Host: cfg.Server.Host},
		transport: &http.Transport{
			// Pinned: never HTTP_PROXY or friends from the environment.
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSClientConfig:     &tls.Config{RootCAs: cfg.RootCAs, MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: 10 * time.Second,
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
			// Compression stays on: with the browser's Accept-Encoding dropped, the
			// transport asks for gzip and decodes it, so the browser gets plain bytes.
		},
		credentials: cfg.Credentials,
		logger:      logger,
		metrics:     cfg.Metrics,
		checkEvery:  checkEvery,
		maxDuration: maxDuration,
		concurrency: newConcurrency(perSession, total, cfg.Metrics),
		rate:        newRate(perSecond, burst, cfg.Now, cfg.Metrics),
		maxBytes:    maxBytes,
	}, nil
}

// requestHeaders are the only browser headers that go upstream. Everything else is
// dropped: Authorization, Impersonate-*, Cookie, forwarding headers and
// Accept-Encoding among them.
var requestHeaders = []string{"Accept", "Content-Type", "User-Agent"}

// responseHeaders are the only upstream headers that reach the browser.
var responseHeaders = []string{
	"Content-Type", "Content-Length", "Audit-Id", "Warning", "Retry-After",
	"X-Kubernetes-Pf-Flowschema-Uid", "X-Kubernetes-Pf-Prioritylevel-Uid",
}

// contentTypes are the media types allowed back to the browser: what the API server
// serves, and text/plain for logs. Anything else, HTML above all, is held back.
var contentTypes = map[string]bool{
	"application/json":                    true,
	"application/yaml":                    true,
	"application/vnd.kubernetes.protobuf": true,
	"application/cbor":                    true,
	"application/cbor-seq":                true,
	"application/com.github.proto-openapi.spec.v2@v1.0+protobuf": true,
	"application/com.github.proto-openapi.spec.v3@v1.0+protobuf": true,
	"text/plain": true,
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// RequestURI is the request target as it arrived. r.URL.Path is a decoded copy
	// and is never consulted: one parse, of the bytes that are forwarded.
	raw, _, _ := strings.Cut(r.RequestURI, "?")
	path, refused := CheckPath(raw)
	if refused != nil {
		p.interrupt(w, r, refused)
		return
	}
	// Any Upgrade field counts, even after an empty one: Get reads only the first.
	if r.Method == http.MethodConnect || len(r.Header.Values("Upgrade")) > 0 {
		p.interrupt(w, r, &interruption.Interruption{
			Status: http.StatusNotImplemented, Reason: "NotImplemented",
			Message: "upgrade protocols (exec, attach, port-forward, WebSocket) are not supported by krm-foyer",
		})
		return
	}

	cred, refused := p.credentials.Token(r)
	if refused == nil && cred.Token == "" {
		// An empty token would make the request anonymous. Never send one.
		refused = interruption.NotSignedIn()
	}
	if refused != nil {
		p.interrupt(w, r, refused)
		return
	}
	if ok, wait := p.rate.allow(cred.Session); !ok {
		p.interrupt(w, r, p.rate.refuse(wait))
		return
	}
	release, refused := p.concurrency.acquire(cred.Session)
	if refused != nil {
		p.interrupt(w, r, refused)
		return
	}
	defer release()

	// From here the request may reach the API server. Its context is cancelled when
	// krm-foyer cuts the response short, which cancels the request upstream too.
	ctx, cut := context.WithCancelCause(r.Context())
	r = r.WithContext(ctx)
	defer p.guard(w, r, cut, cred.Live)()

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{
				Scheme: p.server.Scheme, Host: p.server.Host,
				// Opaque is written to the request line as is, so the path sent is
				// byte-for-byte the path checked. CheckPath refused "//", so Go
				// cannot read it as an authority.
				Opaque:   path,
				RawQuery: pr.In.URL.RawQuery,
			}
			pr.Out.Host = ""
			h := http.Header{}
			for _, k := range requestHeaders {
				if v, ok := pr.In.Header[k]; ok {
					h[k] = v
				}
			}
			h.Set("Authorization", "Bearer "+cred.Token)
			pr.Out.Header = h
		},
		Transport: p.transport,
		// Watches and logs stream: write every chunk as it arrives.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if err := checkResponse(resp); err != nil {
				return err
			}
			return p.limitBytes(r, cut, resp)
		},
		// ReverseProxy hands its error handler the outgoing request, whose headers
		// are the allowlisted ones. How to answer depends on the browser's request.
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) { p.upstreamError(w, r, err) },
	}
	rp.ServeHTTP(&headWriter{ResponseWriter: w}, r)
}

// checkResponse applies the upstream response rules (docs/design.md). An error it
// returns is an *interruption.Interruption, answered by upstreamError.
func checkResponse(resp *http.Response) error {
	if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.StatusCode != http.StatusNotModified {
		return &interruption.Interruption{
			Status: http.StatusBadGateway, Reason: "BadGateway",
			Message: "the API server answered with a redirect, which krm-foyer does not follow or pass on",
			Causes:  []interruption.Cause{{Reason: "Redirect", Field: "Location", Message: resp.Header.Get("Location")}},
		}
	}
	// Go's transport removes Content-Encoding when it decoded the body itself, so
	// one still here is an encoding the browser did not ask for and cannot be
	// bounded by its decoded size.
	if enc := resp.Header.Values("Content-Encoding"); len(enc) > 0 {
		return heldBack("Content-Encoding", strings.Join(enc, ", "))
	}
	// Repeated header fields are ambiguous: the browser takes the last usable value,
	// and Get would check the first. Content-Type has exactly one meaning or none.
	if cts := resp.Header.Values("Content-Type"); len(cts) > 1 {
		return heldBack("Content-Type", strings.Join(cts, ", "))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" || !emptyBody(resp) {
		// A browser splits a Content-Type on commas and uses the last type it can
		// parse; Go's parser does not split. The API server never sends a comma, so
		// refusing one keeps the two from ever having to agree.
		media, _, err := mime.ParseMediaType(ct)
		if err != nil || !contentTypes[media] || strings.Contains(ct, ",") {
			return heldBack("Content-Type", ct)
		}
	}

	h := http.Header{}
	for _, k := range responseHeaders {
		if v, ok := resp.Header[k]; ok {
			h[k] = v
		}
	}
	interruption.SetHeaders(h)
	resp.Header = h
	// Trailers are headers too, and none is on the allowlist. The transport fills
	// this in again when the body ends; headWriter discards that copy.
	resp.Trailer = nil
	return nil
}

// limitBytes holds resp to the response-byte limit: refused with a 502 when its
// length is known to be past it, and otherwise cut short when byte limit+1 arrives.
// The bytes counted are decoded ones, since the transport decodes gzip.
func (p *Proxy) limitBytes(r *http.Request, cut context.CancelCauseFunc, resp *http.Response) error {
	if resp.ContentLength > p.maxBytes && !emptyBody(resp) {
		p.metrics.BoundReached(metrics.BoundResponseBytes)
		return tooLarge(p.maxBytes)
	}
	resp.Body = &limitedBody{body: resp.Body, limit: p.maxBytes, metrics: p.metrics, over: func() {
		p.metrics.BoundReached(metrics.BoundResponseBytes)
		// The copy is not blocked: it ends with this read, and ReverseProxy aborts
		// the response once the bytes within the limit are written.
		p.cut(r, cut, cutResponseBytes)
	}}
	return nil
}

// emptyBody reports whether resp has no body for the browser to interpret. A HEAD
// response never has one; its Content-Length describes the GET.
func emptyBody(resp *http.Response) bool {
	return resp.Request != nil && resp.Request.Method == http.MethodHead ||
		resp.ContentLength == 0 || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified
}

func heldBack(field, value string) *interruption.Interruption {
	return &interruption.Interruption{
		Status: http.StatusBadGateway, Reason: "BadGateway",
		Message: "the API server answered with content krm-foyer does not pass on: " + field + " " + value,
		Causes:  []interruption.Cause{{Reason: "HeldBack", Field: field, Message: value}},
	}
}

func (p *Proxy) upstreamError(w http.ResponseWriter, r *http.Request, err error) {
	var refused *interruption.Interruption
	if !errors.As(err, &refused) {
		if cutShort(r.Context()) {
			// The request may have reached Kubernetes, so no interruption may say it
			// did not, and an empty answer would pass for a complete one.
			panic(http.ErrAbortHandler)
		}
		if r.Context().Err() != nil {
			return // the browser went away; nobody is left to answer
		}
		p.logger.Warn("API server unreachable", "err", err)
		refused = &interruption.Interruption{
			Status: http.StatusBadGateway, Reason: "BadGateway",
			Message: "the API server could not be reached",
		}
	}
	p.interrupt(w, r, refused)
}

func (p *Proxy) interrupt(w http.ResponseWriter, r *http.Request, i *interruption.Interruption) {
	path, _, _ := strings.Cut(r.RequestURI, "?")
	p.logger.Info("interruption", "status", i.Status, "reason", i.Reason, "message", i.Message,
		"method", r.Method, "path", path)
	p.metrics.Interruption(i.Reason)
	i.Serve(w, r)
}

// headWriter lets through only the response head that checkResponse approved.
// ReverseProxy writes upstream headers at two other moments, both past the
// allowlist: an informational (1xx) response's headers before the final response,
// and trailers after the body. The first are dropped with the 1xx status; header
// writes after the head is sent go to a map nobody reads.
type headWriter struct {
	http.ResponseWriter
	sent    bool
	discard http.Header
}

func (w *headWriter) Header() http.Header {
	if w.sent {
		if w.discard == nil {
			w.discard = http.Header{}
		}
		return w.discard
	}
	return w.ResponseWriter.Header()
}

func (w *headWriter) WriteHeader(code int) {
	if code >= 100 && code < 200 {
		return
	}
	w.sent = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *headWriter) Write(b []byte) (int, error) {
	w.sent = true
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach Flush on the real writer.
func (w *headWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// cutCause is why krm-foyer cut a response short. Its text is the cause label of
// krm_foyer_responses_cut_short_total.
type cutCause string

func (c cutCause) Error() string { return string(c) }

const (
	cutSessionEnded     = cutCause(metrics.CauseSessionEnded)
	cutResponseDuration = cutCause(metrics.BoundResponseDuration)
	cutResponseBytes    = cutCause(metrics.BoundResponseBytes)
)

// cutShort reports whether krm-foyer cut short the request ctx belongs to, rather
// than the browser leaving.
func cutShort(ctx context.Context) bool {
	_, ok := context.Cause(ctx).(cutCause)
	return ok
}

// guard watches the response to r while it is open, and cuts it short when its
// session ends or its duration is up. A session check that does not answer within
// an interval counts as a session that has ended. The returned function stops the
// guard; it returns once the guard has.
func (p *Proxy) guard(w http.ResponseWriter, r *http.Request, cut context.CancelCauseFunc, live func(context.Context) bool) (stop func()) {
	ctx := r.Context()
	start := time.Now()
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		tick := time.NewTicker(p.checkEvery)
		defer tick.Stop()
		deadline := time.NewTimer(p.maxDuration)
		defer deadline.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-deadline.C:
				p.metrics.BoundReached(metrics.BoundResponseDuration)
				p.cut(r, cut, cutResponseDuration)
			case <-tick.C:
				if p.live(ctx, live) || ctx.Err() != nil {
					continue
				}
				p.cut(r, cut, cutSessionEnded)
			}
			// The response may be blocked writing to a browser that stopped reading.
			// A deadline in the past fails that write, so the handler returns and the
			// abort happens.
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now())
			return
		}
	}()
	return func() {
		cut(nil)
		<-exited
		p.metrics.BoundUsage(metrics.BoundResponseDuration, float64(time.Since(start))/float64(p.maxDuration))
	}
}

// cut cuts the response to r short, for why: the request to the API server is
// cancelled, and with it the copy of its body, which aborts the browser's response.
func (p *Proxy) cut(r *http.Request, cut context.CancelCauseFunc, why cutCause) {
	path, _, _ := strings.Cut(r.RequestURI, "?")
	p.logger.Info("response cut short", "cause", string(why), "method", r.Method, "path", path)
	p.metrics.CutShort(string(why))
	cut(why)
}

// live asks whether a session is still live, giving it one interval to answer.
func (p *Proxy) live(ctx context.Context, live func(context.Context) bool) bool {
	if live == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, p.checkEvery)
	defer cancel()
	return live(ctx) && ctx.Err() == nil
}
