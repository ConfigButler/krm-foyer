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
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/ConfigButler/krm-stream/gateway"
	"github.com/ConfigButler/krm-stream/gateway/kube"
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
	// MinWatchLife is how long a watch must stay open after its snapshot for its end
	// to be the API server's routine timeout, opened again at once. One that ends
	// sooner is failing, and is left to the browser's client to retry, after a wait.
	// Zero means DefaultMinWatchLife.
	MinWatchLife time.Duration
}

// DefaultMinWatchLife is the MinWatchLife when none is configured. The API server
// keeps a watch open for half an hour or more.
const DefaultMinWatchLife = time.Second

// Streams serves /stream/v1. Create it with New.
type Streams struct {
	server    url.URL
	transport http.RoundTripper
	gate      *gate.Gate
	logger    *slog.Logger
	watchLife time.Duration
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
	watchLife := cfg.MinWatchLife
	if watchLife == 0 {
		watchLife = DefaultMinWatchLife
	}
	if watchLife < 0 {
		return nil, fmt.Errorf("the least life of a watch must be positive, got %v", watchLife)
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
		gate:      cfg.Gate,
		logger:    cfg.Gate.Logger(),
		watchLife: watchLife,
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
	gateway.Handler(gateway.Options{
		// The gate has found the user; the gateway only carries the credential back
		// to Clients.
		Principal: func(*http.Request) (gateway.Principal, error) { return a.Credential, nil },
		// Kubernetes decides: every watch is opened with the user's own token, and
		// the API server refuses what RBAC does not allow.
		Authorizer: gateway.AllowAll{},
		Clients:    u.backend,
		// krm-foyer keeps no list of resources (docs/design.md, "Access"): what the
		// user may watch is what the API server lets them, since every watch is
		// theirs. The gateway still parses the scope itself and refuses one it will
		// not serve, an API-server address or a credential among them; only the
		// unnamed target, the one cluster, exists.
		Scopes:      gateway.ScopePolicy{Targets: []string{""}, AnyResource: true, AllowLabelSelector: true},
		Projections: gateway.ProjectionPolicyFunc(project),
		Diagnostics: s.diagnose,
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

// diagnose logs what went wrong behind an error the browser was sent: its code and
// the kind of failure, never its text, which the API server or whatever answered
// instead wrote (see package upstream). A recovered resync is routine.
func (s *Streams) diagnose(d gateway.Diagnostic) {
	if d.Code == gateway.CodeResyncRequired {
		s.logger.Debug("stream resynchronised", "cause", upstreamerr.Class(d.Err))
		return
	}
	s.logger.Warn("API server could not serve a stream", "code", string(d.Code), "terminal", d.Terminal,
		"cause", upstreamerr.Class(d.Err))
}

// upstream reaches the API server for one stream, as its user.
type upstream struct {
	streams   *Streams
	userAgent string
}

// backend returns a krm-stream backend that acts as the user behind p. The gateway
// asks for one at every snapshot cycle.
func (u *upstream) backend(_ context.Context, _ string, p gateway.Principal) (gateway.Backend, error) {
	cred, ok := p.(gate.Credential)
	if !ok || cred.Token == "" {
		return nil, gateway.Unauthenticated("not signed in")
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
		return nil, err
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return upstreamerr.ErrRedirect }
	client, err := dynamic.NewForConfigAndClient(cfg, httpClient)
	if err != nil {
		return nil, err
	}
	return &backend{streams: u.streams, kube: kube.NewBackend(client)}, nil
}

// backend is krm-stream's Kubernetes backend, which maps the API server's answers to
// the protocol's codes, with two things it does not do: a redirect ends the stream
// rather than being retried, and a watch that ends before it was of use is a failure
// rather than a routine end (docs/investigations/krm-stream-feedback.md). It also
// counts the watches open at the API server.
type backend struct {
	streams *Streams
	kube    *kube.Backend
}

func (b *backend) Watch(ctx context.Context, scope gateway.Scope) (gateway.Watcher, error) {
	w, err := b.kube.Watch(ctx, scope)
	if errors.Is(err, upstreamerr.ErrRedirect) {
		return nil, &gateway.StreamError{Code: gateway.CodeInternal, Terminal: true,
			Message: upstreamerr.ErrRedirect.Error(), Cause: err}
	}
	if err != nil {
		return nil, err
	}
	return &watcher{streams: b.streams, Watcher: w, done: b.streams.gate.Metrics().UpstreamWatch()}, nil
}

// watcher passes on an open watch's events, and counts the watch while it is open.
type watcher struct {
	streams *Streams
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
	switch {
	case ctx.Err() != nil:
	case err == nil && ev.Type == gateway.WatchBookmark && ev.InitialEventsEnd:
		w.synced = time.Now()
	case err == nil && ev.Type == gateway.WatchError && w.early():
		// The gateway would start a new snapshot at once, on this connection; for a
		// watch that was of no use yet, the browser's client waits first.
		var se *gateway.StreamError
		if ev.Err == nil || errors.As(ev.Err, &se) && se.Code == gateway.CodeResyncRequired {
			return ev, failing(ev.Err)
		}
	case errors.Is(err, gateway.ErrWatchClosed) && w.early():
		return ev, failing(err)
	}
	return ev, err
}

// early reports whether the watch is ending before its snapshot was complete, or
// hardly after it: before it was of use.
func (w *watcher) early() bool {
	return w.synced.IsZero() || time.Since(w.synced) < w.streams.watchLife
}

// errEnded is the cause of the answer for a watch that ended before it was of use.
// It must not wrap gateway.ErrWatchClosed, or a resync: the gateway would read either
// as a reason to open the watch again at once, on this connection.
var errEnded = errors.New("the watch ended before it was of use")

// failing is the retryable answer for a watch that ended before it was of use; why it
// ended is logged by kind (see Streams.diagnose).
func failing(why error) error {
	se := gateway.UpstreamUnavailable("the API server ended the watch before it was of use", 0)
	se.Cause = fmt.Errorf("%w (%s)", errEnded, upstreamerr.Class(why))
	return se
}
