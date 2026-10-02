// Package stream is krm-foyer's /stream: live Kubernetes resources for the browser,
// served by krm-stream's gateway (github.com/ConfigButler/krm-stream). Every watch
// is opened as the signed-in user, so Kubernetes decides what a user may watch, as
// it does for /k8s; krm-foyer keeps no list of resources of its own.
package stream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/ConfigButler/krm-stream/gateway"
	"github.com/ConfigButler/krm-stream/gateway/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/ConfigButler/krm-foyer/internal/gate"
	"github.com/ConfigButler/krm-foyer/internal/interruption"
	upstreamerr "github.com/ConfigButler/krm-foyer/internal/upstream"
)

// Config is what streams need to reach one API server.
type Config struct {
	// Server is the API server's URL: https, with no path.
	Server *url.URL
	// RootCAs verifies the API server's certificate. Nil means the system roots.
	RootCAs *x509.CertPool
	// Gate lets requests through: the user's credential, and the bounds every
	// request and response is held to. A stream is cut short by it like any other
	// response.
	Gate *gate.Gate
	// RetryDelay is the first wait before reopening a watch the API server could
	// not serve. Zero means DefaultRetryDelay.
	RetryDelay time.Duration
}

// DefaultRetryDelay is the first wait before reopening a watch the API server could
// not serve. It doubles with every failure in a row, up to maxRetryDelay.
const DefaultRetryDelay = time.Second

const maxRetryDelay = 30 * time.Second

// Streams serves /stream/v1. Create it with New.
type Streams struct {
	server     url.URL
	transport  http.RoundTripper
	gate       *gate.Gate
	logger     *slog.Logger
	retryDelay time.Duration
}

// New returns streams from cfg.Server.
func New(cfg Config) (*Streams, error) {
	if cfg.Server == nil || cfg.Server.Scheme != "https" || cfg.Server.Host == "" ||
		(cfg.Server.Path != "" && cfg.Server.Path != "/") || cfg.Server.RawQuery != "" || cfg.Server.User != nil {
		return nil, fmt.Errorf("API server must be an https URL with no path, query or user info, got %q", cfg.Server)
	}
	if cfg.Gate == nil {
		return nil, errors.New("no gate")
	}
	delay := cfg.RetryDelay
	if delay == 0 {
		delay = DefaultRetryDelay
	}
	if delay < 0 {
		return nil, fmt.Errorf("the retry delay must be positive, got %v", delay)
	}
	return &Streams{
		server: url.URL{Scheme: cfg.Server.Scheme, Host: cfg.Server.Host},
		// The same pinned transport as the proxy's. The user's token is added per
		// request by client-go, from the rest.Config built for that user alone.
		transport: &http.Transport{
			// Pinned: never HTTP_PROXY or friends from the environment.
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSClientConfig:     &tls.Config{RootCAs: cfg.RootCAs, MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: 10 * time.Second,
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
		gate:       cfg.Gate,
		logger:     cfg.Gate.Logger(),
		retryDelay: delay,
	}, nil
}

func (s *Streams) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.gate.Interrupt(w, r, &interruption.Interruption{
			Status: http.StatusMethodNotAllowed, Reason: "MethodNotAllowed",
			Message: "a stream is opened with GET",
		})
		return
	}
	a := s.gate.AdmitStream(w, r)
	if a == nil {
		return
	}
	defer a.Close()
	r = a.Request

	u := &upstream{streams: s, userAgent: r.UserAgent()}
	q := r.URL.Query()
	gateway.Handler(gateway.Options{
		// The gate has found the user; the gateway only carries the credential back
		// to Clients.
		Principal: func(*http.Request) (gateway.Principal, error) { return a.Credential, nil },
		// Kubernetes decides: every watch is opened with the user's own token, and
		// the API server refuses what RBAC does not allow.
		Authorizer: gateway.AllowAll{},
		Clients:    u.backend,
		// krm-stream asks the host for a list of the resources it streams. krm-foyer
		// keeps none (docs/design.md, "Access"), so the list is the one resource
		// asked for, in any namespace. The gateway still parses the scope itself and
		// refuses one it will not serve, an API-server address or a credential among
		// them; only the unnamed target, the one cluster, exists.
		Scopes: gateway.ScopePolicy{
			Targets: []string{""},
			Resources: []gateway.GroupResource{{
				Group: q.Get("group"), Resource: q.Get("resource"),
				Scope: gateway.ResourceScopeNamespaced, AllowAllNamespaces: true,
			}},
			AllowLabelSelector: true,
		},
		Projections: gateway.ProjectionPolicyFunc(project),
	}).ServeHTTP(w, r)

	if gate.CutShort(r.Context()) {
		// The gateway returns quietly when its context ends. A stream the gate cut
		// short must not end cleanly: abort it, as every response cut short is.
		panic(http.ErrAbortHandler)
	}
}

// project grants any built-in projection: none hides anything from a user who may
// read the whole object through /k8s (docs/design.md, "Streams and editing").
func project(_ context.Context, _ gateway.Principal, _ gateway.Scope, requested gateway.Projection) (gateway.Projection, error) {
	switch requested {
	case "":
		return gateway.ProjectionFull, nil
	case gateway.ProjectionRaw, gateway.ProjectionFull, gateway.ProjectionSpec:
		return requested, nil
	default:
		return "", gateway.ScopeInvalid("no such projection: " + string(requested))
	}
}

// upstream reaches the API server for one stream, as its user. It counts the
// failures in a row, so the wait before the next attempt grows, and keeps the Status
// of the last error on an open watch, which krm-stream's adapter would lose.
type upstream struct {
	streams   *Streams
	userAgent string
	failures  int
	// healthy is when the stream last had a complete snapshot; zero when it has
	// failed since.
	healthy time.Time

	mu     sync.Mutex
	status *metav1.Status
}

// backend returns a krm-stream backend that acts as the user behind p. The gateway
// asks for one at every snapshot cycle.
func (u *upstream) backend(ctx context.Context, _ string, p gateway.Principal) (gateway.Backend, error) {
	cred, ok := p.(gate.Credential)
	if !ok || cred.Token == "" {
		return nil, &gateway.StreamError{Code: gateway.CodeUnauthenticated, Message: "not signed in", Terminal: true}
	}
	// Built by hand, with nothing from the environment: no kubeconfig, no in-cluster
	// service account, no proxy. The token is the user's and only the user's, and goes
	// to the configured API server alone: client-go would follow a redirect and send
	// the token along, so its client refuses every redirect, as /k8s does.
	cfg := &rest.Config{
		Host:        u.streams.server.String(),
		BearerToken: cred.Token,
		Transport:   u.streams.transport,
		UserAgent:   u.userAgent,
	}
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, u.unavailable(ctx, err)
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return upstreamerr.ErrRedirect }
	client, err := dynamic.NewForConfigAndClient(cfg, httpClient)
	if err != nil {
		return nil, u.unavailable(ctx, err)
	}
	return &backend{upstream: u, kube: kube.NewBackend(dynamicClient{Interface: client, u: u})}, nil
}

// backend is krm-stream's Kubernetes backend, with the API server's errors mapped to
// the codes of krm-stream's protocol. krm-stream v0.4.0 answers every one of them
// as a terminal INTERNAL carrying the error's text, and an error on an open watch as
// a resync, at once; see docs/investigations/krm-stream-feedback.md, asks 1 and 2.
type backend struct {
	upstream *upstream
	kube     *kube.Backend
}

func (b *backend) Watch(ctx context.Context, scope gateway.Scope) (gateway.Watcher, error) {
	w, err := b.kube.Watch(ctx, scope)
	if err != nil {
		return nil, b.upstream.refusal(ctx, err)
	}
	return &watcher{upstream: b.upstream, Watcher: w, done: b.upstream.streams.gate.Metrics().UpstreamWatch()}, nil
}

// watcher maps an open watch's errors to the protocol's codes, waits before the next
// attempt after a failure, and counts the watch while it is open.
type watcher struct {
	upstream *upstream
	gateway.Watcher
	done func()
	stop sync.Once
	// synced is when its snapshot was complete; zero until then.
	synced time.Time
}

func (w *watcher) Stop() {
	w.Watcher.Stop()
	w.stop.Do(w.done)
}

func (w *watcher) Next(ctx context.Context) (gateway.WatchEvent, error) {
	ev, err := w.Watcher.Next(ctx)
	u := w.upstream
	switch {
	case ctx.Err() != nil:
		return ev, err
	case err == nil && ev.Type == gateway.WatchBookmark && ev.InitialEventsEnd:
		w.synced = time.Now()
		if u.healthy.IsZero() {
			u.healthy = w.synced
		}
		return ev, nil
	case err == nil && ev.Type == gateway.WatchError:
		// The Status the adapter turned into a resync, kept by dynamicClient.
		status := u.takeStatus()
		if status != nil && (status.Code == http.StatusGone || status.Reason == metav1.StatusReasonExpired) {
			return ev, nil // continuity lost: a new snapshot, at once
		}
		if status != nil {
			if refused := refusalOf(status); refused != nil {
				return ev, refused
			}
		}
		return ev, u.unavailable(ctx, &apierrors.StatusError{ErrStatus: statusOrUnknown(status)})
	case errors.Is(err, gateway.ErrWatchClosed):
		// The API server ends a watch at its timeout, and it is opened again at once.
		// One that ends before its snapshot, or hardly after it, is failing instead.
		if w.synced.IsZero() || time.Since(w.synced) < u.streams.retryDelay {
			return ev, u.unavailable(ctx, err)
		}
		return ev, err
	case err == nil:
		return ev, nil
	}
	var se *gateway.StreamError
	if errors.As(err, &se) {
		return ev, err
	}
	return ev, u.unavailable(ctx, err)
}

func statusOrUnknown(s *metav1.Status) metav1.Status {
	if s == nil {
		return metav1.Status{Code: 0, Reason: metav1.StatusReasonUnknown}
	}
	return *s
}

// refusal maps the API server's answer to opening a watch to the protocol's code.
// Anything that may succeed later is tried again, after a wait.
func (u *upstream) refusal(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, upstreamerr.ErrRedirect) {
		u.log(err)
		return &gateway.StreamError{Code: gateway.CodeInternal, Terminal: true, Message: upstreamerr.ErrRedirect.Error()}
	}
	var api apierrors.APIStatus
	if errors.As(err, &api) {
		status := api.Status()
		if refused := refusalOf(&status); refused != nil {
			return refused
		}
	}
	return u.unavailable(ctx, err)
}

// refusalOf is the protocol's terminal answer for one of Kubernetes' refusals, or nil
// for an answer that may change. A 403 keeps Kubernetes' own message, as /k8s passes
// it on.
func refusalOf(status *metav1.Status) error {
	switch status.Code {
	case http.StatusUnauthorized:
		return &gateway.StreamError{Code: gateway.CodeUnauthenticated, Terminal: true,
			Message: "the API server did not accept the session's credential"}
	case http.StatusForbidden:
		return gateway.Forbidden(status.Message)
	case http.StatusNotFound:
		return gateway.ScopeInvalid("the API server does not serve this resource")
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return gateway.ScopeInvalid(status.Message)
	}
	return nil
}

// log records a failure by its class. Its text is never logged: see package upstream.
func (u *upstream) log(err error) {
	u.streams.logger.Warn("API server could not serve a stream", "cause", upstreamerr.Class(err), "failures", u.failures+1)
}

// unavailable waits before the next attempt, longer with every failure in a row and
// at least as long as the API server asked, then says the API server is unavailable.
// A stream that was healthy for longer than the longest wait starts counting again.
func (u *upstream) unavailable(ctx context.Context, err error) error {
	if !u.healthy.IsZero() && time.Since(u.healthy) > maxRetryDelay {
		u.failures = 0
	}
	u.healthy = time.Time{}
	u.log(err)
	wait := min(maxRetryDelay, u.streams.retryDelay<<min(u.failures, 16))
	// Jitter, so that streams cut off together do not come back together.
	wait = wait/2 + rand.N(wait/2+1) //nolint:gosec // spreading retries, not a secret
	if seconds, ok := apierrors.SuggestsClientDelay(err); ok {
		wait = max(wait, time.Duration(seconds)*time.Second)
	}
	u.failures++
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
	}
	return &gateway.StreamError{Code: gateway.CodeUpstreamUnavailable,
		Message: "the API server could not serve the stream; trying again"}
}

func (u *upstream) setStatus(s *metav1.Status) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status = s
}

func (u *upstream) takeStatus() *metav1.Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.status
	u.status = nil
	return s
}

// dynamicClient is the user's dynamic client, with every watch it opens passing the
// Status of an ERROR event to upstream before krm-stream's adapter reduces it to a
// resync.
type dynamicClient struct {
	dynamic.Interface
	u *upstream
}

func (c dynamicClient) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return namespaceable{NamespaceableResourceInterface: c.Interface.Resource(gvr), u: c.u}
}

type namespaceable struct {
	dynamic.NamespaceableResourceInterface
	u *upstream
}

func (n namespaceable) Namespace(ns string) dynamic.ResourceInterface {
	return resource{ResourceInterface: n.NamespaceableResourceInterface.Namespace(ns), u: n.u}
}

func (n namespaceable) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	return n.u.keepStatus(n.NamespaceableResourceInterface.Watch(ctx, opts))
}

type resource struct {
	dynamic.ResourceInterface
	u *upstream
}

func (r resource) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	return r.u.keepStatus(r.ResourceInterface.Watch(ctx, opts))
}

func (u *upstream) keepStatus(w watch.Interface, err error) (watch.Interface, error) {
	if err != nil {
		return nil, err
	}
	sw := &statusWatch{in: w, out: make(chan watch.Event), stopped: make(chan struct{}), u: u}
	go sw.pass()
	return sw, nil
}

// statusWatch passes on a watch's events, keeping the Status of each ERROR event.
// Unlike watch.Filter, it stops passing them on when it is stopped, so an event
// nobody will read never holds its goroutine, and the watch under it, for ever.
type statusWatch struct {
	in      watch.Interface
	out     chan watch.Event
	stopped chan struct{}
	stop    sync.Once
	u       *upstream
}

func (w *statusWatch) ResultChan() <-chan watch.Event { return w.out }

func (w *statusWatch) Stop() {
	w.stop.Do(func() {
		close(w.stopped)
		w.in.Stop()
	})
}

func (w *statusWatch) pass() {
	defer close(w.out)
	for e := range w.in.ResultChan() {
		if s, ok := e.Object.(*metav1.Status); ok && e.Type == watch.Error {
			w.u.setStatus(s.DeepCopy())
		}
		select {
		case w.out <- e:
		case <-w.stopped:
			return
		}
	}
}
