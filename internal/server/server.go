// Package server assembles krm-foyer's HTTP routes.
package server

import (
	"net/http"
	"path"
	"strings"

	"github.com/ConfigButler/krm-foyer/internal/pages"
)

// Config is what the routes need to know about this deployment.
type Config struct {
	// Version is shown on the start page and should match the image tag.
	Version string
	// Kubernetes serves /k8s: the API proxy. Nil leaves /k8s unrouted.
	Kubernetes http.Handler
	// Stream serves /stream/v1: live resources from krm-stream. Nil leaves it
	// unrouted.
	Stream http.Handler
	// Auth serves /auth/: login, logout and the session. Nil leaves it unrouted.
	Auth http.Handler
	// WhoAmI serves GET /auth/whoami. Nil leaves it to Auth.
	WhoAmI http.Handler
	// Ready reports whether krm-foyer can serve logins yet. Nil means always.
	Ready func() bool
}

// New returns the handler for every route krm-foyer serves.
func New(cfg Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", plain("ok"))
	// Liveness never depends on the issuer; readiness does, so a pod that cannot
	// reach it yet gets no traffic, and is not restarted for it either.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if cfg.Ready != nil && !cfg.Ready() {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("waiting for the OIDC issuer\n"))
			return
		}
		plain("ok")(w, r)
	})
	if cfg.Auth != nil {
		mux.Handle("/auth/", cfg.Auth)
	}
	if cfg.WhoAmI != nil {
		mux.Handle("GET /auth/whoami", cfg.WhoAmI)
	}
	if cfg.Stream != nil {
		// Every method, so that the stream answers one that is not a GET with an
		// interruption of its own rather than ServeMux's plain 405.
		mux.Handle("/stream/v1", cfg.Stream)
	}
	// /_foyer/ is the one path prefix krm-foyer reserves for its own files, so it
	// never collides with an application served on the same origin.
	mux.Handle("GET /_foyer/", http.StripPrefix("/_foyer/", noListing(http.FileServerFS(pages.Assets()))))
	// {$} matches "/" only, and nothing else falls through to an application: krm-foyer
	// owns its prefixes and the ingress routes the rest elsewhere. Behind an ingress this
	// page is only seen when krm-foyer is reached directly; see docs/ingress.md.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		pages.Render(w, http.StatusOK, "start.html", struct {
			Version string
			Login   bool
		}{cfg.Version, cfg.Auth != nil})
	})
	site := securityHeaders(mux)
	if cfg.Kubernetes == nil {
		return site
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isKubernetes(r) {
			// The proxy sets its own response headers, and gets the request before
			// ServeMux, which would redirect a non-canonical path to a cleaned one.
			// Rejecting it is the proxy's job: see docs/ingress.md.
			cfg.Kubernetes.ServeHTTP(w, r)
			return
		}
		site.ServeHTTP(w, r)
	})
}

// isKubernetes reports whether r is for /k8s in any spelling: as received, decoded
// or cleaned. A disguised /k8s path must reach the proxy to be refused there, not
// be redirected by ServeMux to a path the proxy would accept.
func isKubernetes(r *http.Request) bool {
	raw, _, _ := strings.Cut(r.RequestURI, "?")
	for _, p := range []string{raw, r.URL.Path, path.Clean(r.URL.Path)} {
		if p == "/k8s" || strings.HasPrefix(p, "/k8s/") {
			return true
		}
	}
	return false
}

// noListing answers 404 for directories, which http.FileServer would otherwise
// list. There is nothing secret in them, but a listing is an interface nobody
// designed.
func noListing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func plain(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(body + "\n"))
	}
}

// securityHeaders applies to every response. krm-foyer's own pages load nothing
// but same-origin stylesheets, so the policy can be this strict. Application
// pages are served separately and need their own policy.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", pages.CSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
