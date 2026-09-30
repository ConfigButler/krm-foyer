package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	New(Config{Version: "v0.0.0-test"}).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	return rec
}

func TestProbes(t *testing.T) {
	for _, path := range []string{"/healthz", "/readyz"} {
		rec := get(t, path)
		if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
			t.Errorf("GET %s = %d %q, want 200 \"ok\\n\"", path, rec.Code, rec.Body.String())
		}
	}
}

func TestStartPage(t *testing.T) {
	rec := get(t, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "v0.0.0-test") {
		t.Error("start page does not show the version")
	}
}

func TestStylesheet(t *testing.T) {
	rec := get(t, "/_foyer/style.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /_foyer/style.css = %d, want 200", rec.Code)
	}
}

// The start page answers "/" only. Anything else must not fall through to it,
// or a mistyped API path would get HTML instead of an error.
func TestUnknownPathIsNotFound(t *testing.T) {
	for _, path := range []string{"/nope", "/k8s/api/v1/pods", "/_foyer/"} {
		if rec := get(t, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	for _, path := range []string{"/", "/healthz", "/nope"} {
		rec := get(t, path)
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("GET %s: Content-Security-Policy = %q", path, csp)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s: missing X-Content-Type-Options", path)
		}
	}
}
