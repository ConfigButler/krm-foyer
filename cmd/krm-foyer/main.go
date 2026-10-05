// Command krm-foyer serves browser login, Kubernetes API access and live krm-stream
// resources on one origin. See docs/design.md for the service contract.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ConfigButler/krm-foyer/internal/auth"
	"github.com/ConfigButler/krm-foyer/internal/gate"
	"github.com/ConfigButler/krm-foyer/internal/metrics"
	"github.com/ConfigButler/krm-foyer/internal/proxy"
	"github.com/ConfigButler/krm-foyer/internal/server"
	"github.com/ConfigButler/krm-foyer/internal/session"
	"github.com/ConfigButler/krm-foyer/internal/stream"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, err := parseConfig(os.Args[1:], os.ReadFile, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		logger.Error("invalid configuration", "err", err)
		os.Exit(2)
	}
	if err := run(cfg, logger); err != nil {
		logger.Error("krm-foyer stopped", "err", err)
		os.Exit(1)
	}
}

// config is the command line, checked.
type config struct {
	listen          string
	tlsCert, tlsKey string
	// metricsListen is where /metrics is served, apart from the origin. Empty
	// serves no metrics.
	metricsListen string
	// login is nil when no sign-in is configured: krm-foyer then serves its start
	// page and probes only.
	login *loginConfig
}

// loginConfig is sign-in and the API half. They come together, because the API
// half's only credential is a session and a session only comes from login, but each
// package keeps its own section. What handler wires (the sessions, the credentials,
// the gate, the logger and the metrics) is left out here.
type loginConfig struct {
	auth       auth.Config
	sessions   session.Config
	gate       gate.Config
	kubernetes proxy.Config
	// shared is nil without shared watches.
	shared *stream.SharedConfig
	// streamWrites bounds each write of a stream to the browser.
	streamWrites time.Duration
}

func parseConfig(args []string, readFile func(string) ([]byte, error), output io.Writer) (config, error) {
	fs := flag.NewFlagSet("krm-foyer", flag.ContinueOnError)
	fs.SetOutput(output)
	var (
		cfg                            config
		publicURL, issuer, clientID    string
		secretFile, issuerCAFile       string
		sessionKeysFile, loginFile     string
		scopes, apiServer, apiServerCA string
		absolute, checkEvery           time.Duration
		maxDuration                    time.Duration
		perSession, inAll, burst       int
		sessionStreams, allStreams     int
		perSecond                      float64
		maxBytes                       int64
		sharedResources, sharedToken   string
		recheck, decisionTTL           time.Duration
		streamWrites                   time.Duration
		sharedQPS                      float64
	)
	fs.StringVar(&cfg.listen, "listen", ":8080", "address to listen on")
	fs.StringVar(&cfg.tlsCert, "tls-cert-file", "", "serve TLS with this certificate (PEM); needs -tls-key-file")
	fs.StringVar(&cfg.tlsKey, "tls-key-file", "", "the private key for -tls-cert-file (PEM)")
	fs.StringVar(&cfg.metricsListen, "metrics-listen", ":9090", "address to serve Prometheus metrics on, apart from the origin; empty serves none")
	fs.StringVar(&publicURL, "public-url", "", "krm-foyer's origin as browsers reach it, such as https://app.example.com")
	fs.StringVar(&issuer, "oidc-issuer", "", "the OIDC issuer URL the API server trusts")
	fs.StringVar(&clientID, "oidc-client-id", "", "krm-foyer's client ID at the issuer; the API server must accept ID tokens for it")
	fs.StringVar(&secretFile, "oidc-client-secret-file", "", "a file holding the client secret")
	fs.StringVar(&issuerCAFile, "oidc-ca-file", "", "extra CA certificates (PEM) to trust for the issuer, besides the system's")
	fs.StringVar(&scopes, "oidc-scopes", strings.Join(auth.DefaultScopes, ","), "comma-separated scopes to ask for at login")
	fs.StringVar(&apiServer, "kubernetes-server", "", "the API server's URL, such as https://kubernetes.default.svc")
	fs.StringVar(&apiServerCA, "kubernetes-ca-file", "", "the CA certificates (PEM) that sign the API server's certificate")
	fs.StringVar(&sessionKeysFile, "session-keys-file", "",
		"a file of the keys session cookies are sealed with, one per line in standard base64, 32 bytes each: "+
			"the first seals new cookies, every one opens them. See docs/design.md, \"Sessions\"")
	fs.StringVar(&loginFile, "login-config-file", "",
		"a YAML file of extra authorization parameters for the issuer and the claims /auth/session shows; "+
			"see docs/design.md, \"Login parameters\"")
	fs.DurationVar(&absolute, "session-absolute-timeout", 8*time.Hour,
		"end any session this long after login, however it is used; the token's expiry ends it sooner")
	fs.DurationVar(&checkEvery, "session-check-interval", gate.DefaultSessionCheckInterval,
		"how often an open response asks whether its session is still live; it is cut short when not")
	fs.DurationVar(&maxDuration, "max-response-duration", gate.DefaultMaxResponseDuration,
		"how long a response, a watch above all, may stay open; it is cut short then")
	fs.IntVar(&perSession, "max-session-concurrent-requests", gate.DefaultMaxSessionConcurrentRequests,
		"how many requests, watches included, one session may have in flight; more get 429")
	fs.IntVar(&inAll, "max-concurrent-requests", gate.DefaultMaxConcurrentRequests,
		"how many requests, watches included, this replica may have in flight; more get 429")
	fs.IntVar(&sessionStreams, "max-session-streams", gate.DefaultMaxSessionStreams,
		"how many streams one session may have open, counted apart from requests; more get 429")
	fs.IntVar(&allStreams, "max-streams", gate.DefaultMaxStreams,
		"how many streams this replica may have open, counted apart from requests; more get 429")
	fs.Float64Var(&perSecond, "session-request-rate", gate.DefaultSessionRequestRate,
		"how many requests a second one session may send, once its burst is spent; more get 429 with Retry-After")
	fs.IntVar(&burst, "session-request-burst", gate.DefaultSessionRequestBurst,
		"how many requests one session may send at once")
	fs.Int64Var(&maxBytes, "max-response-bytes", proxy.DefaultMaxResponseBytes,
		"the most decoded bytes a response may have; past it, 502 if known in advance, otherwise it is cut short")
	fs.StringVar(&sharedResources, "shared-watch-resources", "",
		"resources whose streams share one watch per scope, as kubectl names them, comma-separated "+
			"(notes.hello.krm-foyer.example,configmaps); needs -shared-watch-token-file. See docs/watches.md")
	fs.StringVar(&sharedToken, "shared-watch-token-file", "",
		"the token of the identity shared watches are opened with: a service account that may list and watch "+
			"the shared resources and create subjectaccessreviews, and nothing else")
	fs.DurationVar(&recheck, "shared-watch-recheck-interval", stream.DefaultRecheckInterval,
		"how often the API server is asked again whether each user of a shared watch may still list and watch it")
	fs.DurationVar(&decisionTTL, "shared-watch-decision-ttl", stream.DefaultDecisionTTL,
		"how long one user's access decision for one scope is reused by their other streams and rechecks")
	fs.Float64Var(&sharedQPS, "shared-watch-qps", stream.DefaultSharedQPS,
		"how many requests a second the shared-watch identity may send, reviews and watches for every user; bursts are twice as many")
	fs.DurationVar(&streamWrites, "stream-write-timeout", stream.DefaultWriteTimeout,
		"how long one write of a stream to the browser may take; a browser that stops reading ends its stream then")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if (cfg.tlsCert == "") != (cfg.tlsKey == "") {
		return config{}, errors.New("-tls-cert-file and -tls-key-file go together")
	}

	required := map[string]string{
		"-public-url": publicURL, "-oidc-issuer": issuer, "-oidc-client-id": clientID,
		"-oidc-client-secret-file": secretFile, "-session-keys-file": sessionKeysFile,
		"-kubernetes-server": apiServer,
	}
	var missing []string
	for name, v := range required {
		if v == "" {
			missing = append(missing, name)
		}
	}
	switch {
	case len(missing) == len(required):
		if issuerCAFile != "" || apiServerCA != "" || loginFile != "" || sharedResources != "" || sharedToken != "" {
			return config{}, errors.New("-oidc-ca-file, -kubernetes-ca-file, -login-config-file and the -shared-watch flags need the sign-in flags too")
		}
		return cfg, nil // no sign-in and no API proxy: the start page and probes only
	case len(missing) > 0:
		slices.Sort(missing)
		return config{}, fmt.Errorf("sign-in and the API proxy need all of %s; missing %s",
			"-public-url, -oidc-issuer, -oidc-client-id, -oidc-client-secret-file, -session-keys-file and -kubernetes-server",
			strings.Join(missing, ", "))
	}

	// Sign-in.
	secret, err := readFile(secretFile)
	if err != nil {
		return config{}, fmt.Errorf("reading the client secret: %w", err)
	}
	login := &loginConfig{auth: auth.Config{
		PublicURL: publicURL, Issuer: issuer, ClientID: clientID,
		// Secrets mounted from files often end in a newline that is not part of them.
		ClientSecret: strings.TrimRight(string(secret), "\r\n"),
	}}
	if login.auth.ClientSecret == "" {
		return config{}, errors.New("the client secret file is empty")
	}
	for _, s := range strings.Split(scopes, ",") {
		if s = strings.TrimSpace(s); s != "" {
			login.auth.Scopes = append(login.auth.Scopes, s)
		}
	}
	if issuerCAFile != "" {
		if login.auth.RootCAs, err = x509.SystemCertPool(); err != nil {
			return config{}, err
		}
		if err := appendCAs(login.auth.RootCAs, issuerCAFile, readFile); err != nil {
			return config{}, err
		}
	}

	if loginFile != "" {
		text, err := readFile(loginFile)
		if err != nil {
			return config{}, fmt.Errorf("reading the login configuration: %w", err)
		}
		if login.auth.Login, err = auth.ParseLoginConfig(text); err != nil {
			return config{}, fmt.Errorf("-login-config-file: %w", err)
		}
		if err := login.auth.Login.Validate(); err != nil {
			return config{}, fmt.Errorf("-login-config-file: %w", err)
		}
	}

	// Sessions. A mutation must come from the public URL's origin. Without its keys,
	// krm-foyer does not start: a key of its own making would end every session at
	// the next restart, and differ from any other replica's.
	keys, err := readFile(sessionKeysFile)
	if err != nil {
		return config{}, fmt.Errorf("reading the session keys: %w", err)
	}
	login.sessions = session.Config{Origin: publicURL, AbsoluteTimeout: absolute}
	if login.sessions.Keys, err = session.ParseKeys(keys); err != nil {
		return config{}, fmt.Errorf("-session-keys-file: %w", err)
	}
	if absolute <= 0 {
		return config{}, fmt.Errorf("-session-absolute-timeout must be positive, got %v", absolute)
	}

	// The bounds every request through the API half is held to.
	if checkEvery <= 0 {
		return config{}, fmt.Errorf("-session-check-interval must be positive, got %v", checkEvery)
	}
	login.gate.SessionCheckInterval = checkEvery
	if maxDuration <= 0 {
		return config{}, fmt.Errorf("-max-response-duration must be positive, got %v", maxDuration)
	}
	login.gate.MaxResponseDuration = maxDuration
	if perSession <= 0 || inAll <= 0 {
		return config{}, fmt.Errorf("-max-session-concurrent-requests and -max-concurrent-requests must be positive, got %d and %d", perSession, inAll)
	}
	login.gate.MaxSessionConcurrentRequests, login.gate.MaxConcurrentRequests = perSession, inAll
	if sessionStreams <= 0 || allStreams <= 0 {
		return config{}, fmt.Errorf("-max-session-streams and -max-streams must be positive, got %d and %d", sessionStreams, allStreams)
	}
	login.gate.MaxSessionStreams, login.gate.MaxStreams = sessionStreams, allStreams
	if perSecond <= 0 || burst <= 0 {
		return config{}, fmt.Errorf("-session-request-rate and -session-request-burst must be positive, got %v and %d", perSecond, burst)
	}
	login.gate.SessionRequestRate, login.gate.SessionRequestBurst = perSecond, burst
	if streamWrites <= 0 {
		return config{}, fmt.Errorf("-stream-write-timeout must be positive, got %v", streamWrites)
	}
	login.streamWrites = streamWrites

	// The API proxy.
	if maxBytes <= 0 {
		return config{}, fmt.Errorf("-max-response-bytes must be positive, got %d", maxBytes)
	}
	login.kubernetes.MaxResponseBytes = maxBytes
	if login.kubernetes.Server, err = url.Parse(apiServer); err != nil {
		return config{}, fmt.Errorf("-kubernetes-server: %w", err)
	}
	if apiServerCA != "" {
		login.kubernetes.RootCAs = x509.NewCertPool()
		if err := appendCAs(login.kubernetes.RootCAs, apiServerCA, readFile); err != nil {
			return config{}, err
		}
	}

	// Shared watches, for the resources named: off unless both flags are given.
	if (sharedResources == "") != (sharedToken == "") {
		return config{}, errors.New("-shared-watch-resources and -shared-watch-token-file go together")
	}
	if sharedResources != "" {
		resources, err := stream.ParseResources(sharedResources)
		if err != nil {
			return config{}, fmt.Errorf("-shared-watch-resources: %w", err)
		}
		if len(resources) == 0 {
			return config{}, errors.New("-shared-watch-resources names no resource")
		}
		if sharedQPS <= 0 {
			return config{}, fmt.Errorf("-shared-watch-qps must be positive, got %v", sharedQPS)
		}
		if recheck <= 0 || decisionTTL <= 0 || decisionTTL > recheck {
			return config{}, fmt.Errorf("-shared-watch-recheck-interval and -shared-watch-decision-ttl must be positive, "+
				"the lifetime no longer than the interval; got %v and %v", recheck, decisionTTL)
		}
		login.shared = &stream.SharedConfig{TokenFile: sharedToken, Resources: resources,
			RecheckInterval: recheck, DecisionTTL: decisionTTL, QPS: float32(sharedQPS)}
	}
	cfg.login = login
	return cfg, nil
}

func appendCAs(pool *x509.CertPool, file string, readFile func(string) ([]byte, error)) error {
	pem, err := readFile(file)
	if err != nil {
		return fmt.Errorf("reading CA certificates: %w", err)
	}
	if !pool.AppendCertsFromPEM(pem) {
		return fmt.Errorf("no PEM certificates in %s", file)
	}
	return nil
}

// handler builds every route from cfg, recording into m. With login configured it
// returns a function that discovers the issuer in the background.
func handler(cfg config, logger *slog.Logger, m *metrics.Metrics) (http.Handler, func(context.Context), error) {
	if cfg.login == nil {
		return server.New(server.Config{Version: version}), func(context.Context) {}, nil
	}
	l := *cfg.login
	sessions, err := session.New(l.sessions)
	if err != nil {
		return nil, nil, err
	}
	l.auth.Sessions, l.auth.Logger = sessions, logger
	login, err := auth.New(l.auth)
	if err != nil {
		return nil, nil, err
	}
	// Login is the API half's only credential source for what a user does: there is no
	// other way to give krm-foyer a token, and no service-account fallback. Shared
	// watches, when configured, hold an identity of their own for the watches alone,
	// and ask the API server about every user who reads them.
	l.gate.Credentials, l.gate.Logger, l.gate.Metrics = login, logger, m
	g, err := gate.New(l.gate)
	if err != nil {
		return nil, nil, err
	}
	l.kubernetes.Gate = g
	api, err := proxy.New(l.kubernetes)
	if err != nil {
		return nil, nil, err
	}
	streams, err := stream.New(stream.Config{
		Server: l.kubernetes.Server, RootCAs: l.kubernetes.RootCAs, Gate: g, Shared: l.shared,
		WriteTimeout: l.streamWrites,
	})
	if err != nil {
		return nil, nil, err
	}
	return server.New(server.Config{
		Version: version, Kubernetes: api, Stream: streams, WhoAmI: streams.WhoAmI(), Auth: login.Handler(), Ready: login.Ready,
	}), login.Run, nil
}

func run(cfg config, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	m := metrics.New()
	h, discover, err := handler(cfg, logger, m)
	if err != nil {
		return err
	}
	go discover(ctx)

	// Either listener failing stops krm-foyer: running without the metrics it was
	// configured to serve would hide that they are missing.
	errs := make(chan error, 2)

	// Metrics have a listener of their own, never the origin, where any page could
	// read them. Plain HTTP: what they say is counts, and the deployment keeps the
	// port to the monitoring system (docs/bounds.md, "Metrics").
	var metricsSrv *http.Server
	if cfg.metricsListen != "" {
		metricsSrv = &http.Server{
			Addr:              cfg.metricsListen,
			Handler:           metricsHandler(m),
			ReadHeaderTimeout: 10 * time.Second,
			ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		}
		go func() {
			if err := metricsSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("metrics listener: %w", err)
			}
		}()
	}

	srv, endRequests := newServer(cfg.listen, h, logger)
	srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}

	go func() {
		logger.Info("krm-foyer listening", "addr", cfg.listen, "version", version,
			"tls", cfg.tlsCert != "", "login", cfg.login != nil)
		if cfg.tlsCert != "" {
			errs <- srv.ListenAndServeTLS(cfg.tlsCert, cfg.tlsKey)
		} else {
			errs <- srv.ListenAndServe()
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	// Kubernetes sends SIGTERM and waits terminationGracePeriodSeconds (30s by
	// default) before killing the pod, so stop well inside that.
	if metricsSrv != nil {
		metricsCtx, cancel := context.WithTimeout(context.Background(), shutdownDrain)
		_ = metricsSrv.Shutdown(metricsCtx)
		cancel()
	}
	shutdown(srv, endRequests, shutdownDrain, shutdownTimeout, logger)
	if err := <-errs; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// How krm-foyer stops: requests in flight get shutdownDrain to finish by
// themselves. Streams never do, so then every request still open is ended, and
// whatever remains at shutdownTimeout is cut.
const (
	shutdownDrain   = 5 * time.Second
	shutdownTimeout = 20 * time.Second
)

// newServer serves h on addr. endRequests ends the context of every request it
// serves, the streams too, which otherwise stay open until the browser leaves.
func newServer(addr string, h http.Handler, logger *slog.Logger) (srv *http.Server, endRequests context.CancelFunc) {
	requests, endRequests := context.WithCancel(context.Background())
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		// A keep-alive connection with no request on it is closed after this.
		IdleTimeout: 2 * time.Minute,
		BaseContext: func(net.Listener) context.Context { return requests },
		ErrorLog:    slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}, endRequests
}

// shutdown stops srv: no new connections, drain for the requests in flight to
// finish, then endRequests for the streams, and a hard close at timeout.
func shutdown(srv *http.Server, endRequests context.CancelFunc, drain, timeout time.Duration, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Shutdown(ctx) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(drain):
		endRequests()
		err = <-done
	}
	endRequests()
	if err != nil {
		logger.Warn("requests were still open when krm-foyer stopped; closing them", "after", timeout)
		_ = srv.Close()
		return
	}
	logger.Info("krm-foyer shut down cleanly")
}

// metricsHandler serves /metrics and nothing else.
func metricsHandler(m *metrics.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	return mux
}
