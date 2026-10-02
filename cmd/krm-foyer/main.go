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
// package keeps its own section. What handler wires (the session store, the
// sessions, the credentials, the gate, the logger and the metrics) is left out here.
type loginConfig struct {
	auth       auth.Config
	sessions   session.Config
	gate       gate.Config
	kubernetes proxy.Config
}

func parseConfig(args []string, readFile func(string) ([]byte, error), output io.Writer) (config, error) {
	fs := flag.NewFlagSet("krm-foyer", flag.ContinueOnError)
	fs.SetOutput(output)
	var (
		cfg                            config
		publicURL, issuer, clientID    string
		secretFile, issuerCAFile       string
		scopes, apiServer, apiServerCA string
		idle, absolute, checkEvery     time.Duration
		maxDuration                    time.Duration
		perSession, inAll, burst       int
		perSecond                      float64
		maxBytes                       int64
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
	fs.DurationVar(&idle, "session-idle-timeout", time.Hour, "end a session unused for this long")
	fs.DurationVar(&absolute, "session-absolute-timeout", 8*time.Hour, "end any session this long after login")
	fs.DurationVar(&checkEvery, "session-check-interval", gate.DefaultSessionCheckInterval,
		"how often an open response asks whether its session is still live; it is cut short when not")
	fs.DurationVar(&maxDuration, "max-response-duration", gate.DefaultMaxResponseDuration,
		"how long a response, a watch above all, may stay open; it is cut short then. Below -session-idle-timeout")
	fs.IntVar(&perSession, "max-session-concurrent-requests", gate.DefaultMaxSessionConcurrentRequests,
		"how many requests, watches included, one session may have in flight; more get 429")
	fs.IntVar(&inAll, "max-concurrent-requests", gate.DefaultMaxConcurrentRequests,
		"how many requests, watches included, this replica may have in flight; more get 429")
	fs.Float64Var(&perSecond, "session-request-rate", gate.DefaultSessionRequestRate,
		"how many requests a second one session may send, once its burst is spent; more get 429 with Retry-After")
	fs.IntVar(&burst, "session-request-burst", gate.DefaultSessionRequestBurst,
		"how many requests one session may send at once")
	fs.Int64Var(&maxBytes, "max-response-bytes", proxy.DefaultMaxResponseBytes,
		"the most decoded bytes a response may have; past it, 502 if known in advance, otherwise it is cut short")
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
		"-oidc-client-secret-file": secretFile, "-kubernetes-server": apiServer,
	}
	var missing []string
	for name, v := range required {
		if v == "" {
			missing = append(missing, name)
		}
	}
	switch {
	case len(missing) == len(required):
		if issuerCAFile != "" || apiServerCA != "" {
			return config{}, errors.New("-oidc-ca-file and -kubernetes-ca-file need the sign-in flags too")
		}
		return cfg, nil // no sign-in and no API proxy: the start page and probes only
	case len(missing) > 0:
		slices.Sort(missing)
		return config{}, fmt.Errorf("sign-in and the API proxy need all of %s; missing %s",
			"-public-url, -oidc-issuer, -oidc-client-id, -oidc-client-secret-file and -kubernetes-server",
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

	// Sessions. A mutation must come from the public URL's origin.
	login.sessions = session.Config{Origin: publicURL, IdleTimeout: idle, AbsoluteTimeout: absolute}

	// The bounds every request through the API half is held to.
	if checkEvery <= 0 {
		return config{}, fmt.Errorf("-session-check-interval must be positive, got %v", checkEvery)
	}
	login.gate.SessionCheckInterval = checkEvery
	// A reconnect counts as use and an open response does not, so a tab that only
	// watches stays signed in only if its watches end, and reconnect, within the
	// idle timeout.
	if maxDuration <= 0 || maxDuration >= idle {
		return config{}, fmt.Errorf("-max-response-duration must be positive and below -session-idle-timeout (%v), got %v", idle, maxDuration)
	}
	login.gate.MaxResponseDuration = maxDuration
	if perSession <= 0 || inAll <= 0 {
		return config{}, fmt.Errorf("-max-session-concurrent-requests and -max-concurrent-requests must be positive, got %d and %d", perSession, inAll)
	}
	login.gate.MaxSessionConcurrentRequests, login.gate.MaxConcurrentRequests = perSession, inAll
	if perSecond <= 0 || burst <= 0 {
		return config{}, fmt.Errorf("-session-request-rate and -session-request-burst must be positive, got %v and %d", perSecond, burst)
	}
	login.gate.SessionRequestRate, login.gate.SessionRequestBurst = perSecond, burst

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
	l.sessions.Store = session.NewMemory(nil)
	sessions, err := session.New(l.sessions)
	if err != nil {
		return nil, nil, err
	}
	l.auth.Sessions, l.auth.Logger = sessions, logger
	login, err := auth.New(l.auth)
	if err != nil {
		return nil, nil, err
	}
	// Login is the API half's only credential source: there is no other way to give
	// krm-foyer a token, and no service-account fallback.
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
	streams, err := stream.New(stream.Config{Server: l.kubernetes.Server, RootCAs: l.kubernetes.RootCAs, Gate: g})
	if err != nil {
		return nil, nil, err
	}
	return server.New(server.Config{
		Version: version, Kubernetes: api, Stream: streams, Auth: login.Handler(), Ready: login.Ready,
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

	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

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
	// default) before killing the pod, so finish in-flight requests well inside that.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutdownCtx)
	}
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errs; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("krm-foyer shut down cleanly")
	return nil
}

// metricsHandler serves /metrics and nothing else.
func metricsHandler(m *metrics.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	return mux
}
