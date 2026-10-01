// Package server assembles krm-foyer's HTTP routes.
package server

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed pages
var pages embed.FS

var templates = template.Must(template.ParseFS(pages, "pages/*.html"))

// Config is what the routes need to know about this deployment.
type Config struct {
	// Version is shown on the start page and should match the image tag.
	Version string
}

// New returns the handler for every route krm-foyer serves.
func New(cfg Config) http.Handler {
	assets, err := fs.Sub(pages, "pages/assets")
	if err != nil {
		panic(err) // the embed pattern above guarantees the directory exists
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", plain("ok"))
	// Ready and healthy are the same thing until there is an OIDC issuer and a
	// cluster to depend on; then readiness checks those and liveness does not.
	mux.HandleFunc("GET /readyz", plain("ok"))
	// /_foyer/ is the one path prefix krm-foyer reserves for its own files, so it
	// never collides with an application served on the same origin.
	mux.Handle("GET /_foyer/", http.StripPrefix("/_foyer/", noListing(http.FileServerFS(assets))))
	// {$} matches "/" only, and nothing else falls through to an application: krm-foyer
	// owns its prefixes and the ingress routes the rest elsewhere. Behind an ingress this
	// page is only seen when krm-foyer is reached directly; see docs/ingress.md.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		render(w, "start.html", cfg)
	})
	return securityHeaders(mux)
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

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := templates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// securityHeaders applies to every response. krm-foyer's own pages load nothing
// but same-origin stylesheets, so the policy can be this strict; application
// pages will need their own policy once static hosting exists.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
