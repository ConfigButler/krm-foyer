// Package stream is krm-foyer's /stream: live Kubernetes resources for the browser,
// served by krm-stream's gateway (github.com/ConfigButler/krm-stream). A watch is
// opened as the signed-in user, so Kubernetes decides what a user may watch, as it
// does for /k8s; krm-foyer keeps no list of resources of its own. Resources
// configured for it share one watch per scope instead (see shared.go), and the API
// server still decides, by a SubjectAccessReview for each user.
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
	"k8s.io/client-go/rest"

	"github.com/ConfigButler/krm-foyer/internal/gate"
	"github.com/ConfigButler/krm-foyer/internal/interruption"
	"github.com/ConfigButler/krm-foyer/internal/metrics"
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
	// Shared turns shared watches on for some resources. Nil streams every
	// resource with the user's own token.
	Shared *SharedConfig
	// WriteTimeout bounds each write to the browser. A browser that stops reading
	// ends its stream within it, instead of holding the stream, and its rechecks,
	// until the response duration is up. Zero means DefaultWriteTimeout.
	WriteTimeout time.Duration
}

// DefaultWriteTimeout is the WriteTimeout when none is configured.
const DefaultWriteTimeout = 10 * time.Second

// Streams serves /stream/v1. Create it with New.
type Streams struct {
	server    url.URL
	transport http.RoundTripper
	gate      *gate.Gate
	logger    *slog.Logger
	writes    time.Duration
	// shared is nil without shared watches.
	shared *shared
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
	s := &Streams{
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
		gate:   cfg.Gate,
		logger: cfg.Gate.Logger(),
		writes: cfg.WriteTimeout,
	}
	if s.writes == 0 {
		s.writes = DefaultWriteTimeout
	}
	if s.writes < 0 {
		return nil, fmt.Errorf("the write timeout must be positive, got %v", s.writes)
	}
	if cfg.Shared != nil {
		sh, err := newShared(*cfg.Shared, s)
		if err != nil {
			return nil, err
		}
		s.shared = sh
	}
	return s, nil
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
	opts := gateway.Options{
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
		// Every write to the browser is bounded, so one that stopped reading ends its
		// stream rather than holding it, and a shared stream's rechecks, which wait
		// for the write in progress.
		WriteTimeout: s.writes,
	}
	// A scope the gateway refuses goes the user's way, and is refused there.
	if scope, err := gateway.ScopeFromQuery(r.URL.Query()); err == nil && s.shared.serves(scope) {
		sh := s.shared
		// The user is found again, as Kubernetes knows them, for the reviews.
		opts.Principal = func(r *http.Request) (gateway.Principal, error) { return u.principal(r.Context(), a.Credential) }
		// The shared watch is opened with krm-foyer's own identity, so the API server
		// is asked whether this user may list and watch the scope: before the stream
		// is served from it, at each new snapshot, and every recheck interval.
		opts.Authorizer = sh.authorize
		opts.ReauthorizationInterval, opts.ReauthorizationTimeout = sh.interval, CheckTimeout
		opts.Clients = func(context.Context, string, gateway.Principal) (gateway.Backend, error) { return sh.backend, nil }
		// Only the shared resources, never any resource: the shared identity may watch
		// those alone.
		opts.Scopes = gateway.ScopePolicy{Targets: []string{""}, Resources: sh.resources, AllowLabelSelector: true}
	}
	gateway.Handler(opts).ServeHTTP(w, r)

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
		s.logger.Debug("stream resynchronised", "cause", class(d.Err))
		return
	}
	s.logger.Warn("API server could not serve a stream", "code", string(d.Code), "terminal", d.Terminal,
		"cause", class(d.Err))
}

// class is upstreamerr.Class, which also names the redirect krm-stream's client
// refuses as a redirect.
func class(err error) string {
	if errors.Is(err, kube.ErrRedirectRefused) {
		return "redirect"
	}
	return upstreamerr.Class(err)
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
	kb, err := kube.NewBackendForConfig(u.userConfig(cred))
	if err != nil {
		return nil, err
	}
	return &backend{streams: u.streams, kube: kb}, nil
}

// userConfig reaches the API server as the user behind cred. Built by hand, with
// nothing from the environment: no kubeconfig, no in-cluster service account, no
// proxy. The token is the user's and only the user's, and goes to the configured API
// server alone: client-go would follow a redirect and send the token along, so
// krm-stream's client refuses every redirect, as /k8s does, and ends the stream.
func (u *upstream) userConfig(cred gate.Credential) *rest.Config {
	return &rest.Config{
		Host:        u.streams.server.String(),
		BearerToken: cred.Token,
		Transport:   u.streams.transport,
		UserAgent:   u.userAgent,
	}
}

// backend is krm-stream's Kubernetes backend, counting the watches open at the API
// server.
type backend struct {
	streams *Streams
	kube    *kube.Backend
}

func (b *backend) Watch(ctx context.Context, scope gateway.Scope) (gateway.Watcher, error) {
	w, err := b.kube.Watch(ctx, scope)
	if err != nil {
		return nil, err
	}
	return &watcher{Watcher: w, done: b.streams.gate.Metrics().UpstreamWatch(metrics.IdentityUser)}, nil
}

// watcher passes on an open watch's events, and counts the watch while it is open.
type watcher struct {
	gateway.Watcher
	done func()
	stop sync.Once
}

func (w *watcher) Stop() {
	w.Watcher.Stop()
	w.stop.Do(w.done)
}
