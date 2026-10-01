// Package proxy is krm-foyer's API half: it sends a browser's /k8s request to the
// API server with the user's own token, and lets the API server decide. It makes
// no access decision of its own. What it does refuse is listed as interruptions in
// docs/design.md.
package proxy

import (
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
)

// Credentials gives the proxy the token of the user behind a request. It is the
// only way a token reaches the proxy. Login and sessions sit behind it; there is no
// other source, no default and no service account.
type Credentials interface {
	// Token returns the user's bearer token, ErrNoCredential when the request has no
	// signed-in user, an *Interruption when the session refuses the request (a
	// mutation without CSRF proof, say), or another error when none of that can be
	// established.
	Token(r *http.Request) (string, error)
}

// ErrNoCredential means the request has no signed-in user. The proxy answers 401.
var ErrNoCredential = errors.New("no credential for this request")

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
}

// Proxy serves Prefix. Create it with New.
type Proxy struct {
	server      url.URL
	transport   http.RoundTripper
	credentials Credentials
	logger      *slog.Logger
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

// setResponseHeaders sets what every answer under Prefix carries, proxied or not,
// replacing anything upstream sent.
func setResponseHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
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
		p.interrupt(w, r, &Interruption{
			Status: http.StatusNotImplemented, Reason: "NotImplemented",
			Message: "upgrade protocols (exec, attach, port-forward, WebSocket) are not supported by krm-foyer",
		})
		return
	}

	token, err := p.credentials.Token(r)
	var sessionRefused *Interruption
	switch {
	case errors.Is(err, ErrNoCredential) || err == nil && token == "":
		// An empty token would make the request anonymous. Never send one.
		p.interrupt(w, r, &Interruption{
			Status: http.StatusUnauthorized, Reason: "Unauthorized",
			Message: "not signed in",
		})
		return
	case errors.As(err, &sessionRefused):
		p.interrupt(w, r, sessionRefused)
		return
	case err != nil:
		p.logger.Error("credential lookup failed", "err", err)
		p.interrupt(w, r, &Interruption{
			Status: http.StatusServiceUnavailable, Reason: "ServiceUnavailable",
			Message: "the session could not be checked; try again later",
		})
		return
	}

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
			h.Set("Authorization", "Bearer "+token)
			pr.Out.Header = h
		},
		Transport: p.transport,
		// Watches and logs stream: write every chunk as it arrives.
		FlushInterval:  -1,
		ModifyResponse: checkResponse,
		ErrorHandler:   p.upstreamError,
	}
	rp.ServeHTTP(&headWriter{ResponseWriter: w}, r)
}

// checkResponse applies the upstream response rules (docs/design.md). An error it
// returns is an *Interruption, answered by upstreamError.
func checkResponse(resp *http.Response) error {
	if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.StatusCode != http.StatusNotModified {
		return &Interruption{
			Status: http.StatusBadGateway, Reason: "BadGateway",
			Message: "the API server answered with a redirect, which krm-foyer does not follow or pass on",
			Causes:  []Cause{{Reason: "Redirect", Field: "Location", Message: resp.Header.Get("Location")}},
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
	setResponseHeaders(h)
	resp.Header = h
	// Trailers are headers too, and none is on the allowlist. The transport fills
	// this in again when the body ends; headWriter discards that copy.
	resp.Trailer = nil
	return nil
}

// emptyBody reports whether resp has no body for the browser to interpret. A HEAD
// response never has one; its Content-Length describes the GET.
func emptyBody(resp *http.Response) bool {
	return resp.Request != nil && resp.Request.Method == http.MethodHead ||
		resp.ContentLength == 0 || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified
}

func heldBack(field, value string) *Interruption {
	return &Interruption{
		Status: http.StatusBadGateway, Reason: "BadGateway",
		Message: "the API server answered with content krm-foyer does not pass on: " + field + " " + value,
		Causes:  []Cause{{Reason: "HeldBack", Field: field, Message: value}},
	}
}

func (p *Proxy) upstreamError(w http.ResponseWriter, r *http.Request, err error) {
	var refused *Interruption
	if !errors.As(err, &refused) {
		if r.Context().Err() != nil {
			return // the browser went away; nobody is left to answer
		}
		p.logger.Warn("API server unreachable", "err", err)
		refused = &Interruption{
			Status: http.StatusBadGateway, Reason: "BadGateway",
			Message: "the API server could not be reached",
		}
	}
	p.interrupt(w, r, refused)
}

func (p *Proxy) interrupt(w http.ResponseWriter, r *http.Request, i *Interruption) {
	path, _, _ := strings.Cut(r.RequestURI, "?")
	p.logger.Info("interruption", "status", i.Status, "reason", i.Reason, "message", i.Message,
		"method", r.Method, "path", path)
	i.write(w)
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
