// Package pages renders the few pages krm-foyer serves itself (docs/frontend.md):
// Go templates and a stylesheet, embedded, with no script.
package pages

import (
	"bytes"
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
)

//go:embed templates assets
var files embed.FS

var templates = template.Must(template.ParseFS(files, "templates/*.html"))

// CSP is the policy on every page: same-origin stylesheets and images, nothing else.
// No inline script can run, and no page can be framed.
const CSP = "default-src 'none'; style-src 'self'; img-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// Assets is the stylesheet and images, served under /_foyer/.
func Assets() fs.FS {
	assets, err := fs.Sub(files, "assets")
	if err != nil {
		panic(err) // the embed pattern above guarantees the directory exists
	}
	return assets
}

// Render answers with the named page and status. Pages are never cached: they can
// show who is signed in.
func Render(w http.ResponseWriter, status int, name string, data any) {
	var body bytes.Buffer
	if err := templates.ExecuteTemplate(&body, name, data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(body.Len()))
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", CSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_, _ = w.Write(body.Bytes())
}
