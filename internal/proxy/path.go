package proxy

import (
	"net/http"
	"strings"

	"github.com/ConfigButler/krm-foyer/internal/interruption"
)

// Prefix is where krm-foyer serves the Kubernetes API. It is stripped before a
// request goes upstream and is the only change made to the path.
const Prefix = "/k8s"

// CheckPath takes the path of a request target exactly as it arrived, without the
// query, and returns the path to send to the API server: the same bytes minus
// Prefix. It refuses, with an *interruption.Interruption, a path that is not canonical, is not
// one of the API routes, or names a subresource krm-foyer does not support.
//
// It works on the escaped form only and never decodes into a second form, so the
// path checked and the path forwarded cannot differ. See "Path hygiene" in
// docs/design.md and "Why routing by path is safe" in docs/ingress.md.
func CheckPath(raw string) (string, *interruption.Interruption) {
	upstream, ok := strings.CutPrefix(raw, Prefix)
	if !ok || (upstream != "" && upstream[0] != '/') {
		return "", notAPIRoute()
	}
	if upstream == "" || upstream == "/" {
		return "", notAPIRoute()
	}

	segments := strings.Split(upstream[1:], "/")
	for _, s := range segments {
		if reason := nonCanonical(s); reason != "" {
			return "", &interruption.Interruption{
				Status: http.StatusBadRequest, Reason: "BadRequest",
				Message: "non-canonical path: " + reason + "; krm-foyer rejects such paths instead of normalizing them",
			}
		}
	}

	switch segments[0] {
	case "api", "apis":
		if what := unsupported(segments); what != "" {
			return "", &interruption.Interruption{
				Status: http.StatusNotImplemented, Reason: "NotImplemented",
				Message: "the " + what + " subresource is not supported by krm-foyer",
			}
		}
	case "version":
		if len(segments) != 1 {
			return "", notAPIRoute()
		}
	case "openapi":
		if len(segments) < 2 {
			return "", notAPIRoute()
		}
	default:
		return "", notAPIRoute()
	}
	return upstream, nil
}

func notAPIRoute() *interruption.Interruption {
	return &interruption.Interruption{
		Status: http.StatusNotFound, Reason: "NotFound",
		Message: "not a Kubernetes API route: krm-foyer serves " + Prefix + "/api, " + Prefix + "/apis, " + Prefix + "/version and " + Prefix + "/openapi",
	}
}

// nonCanonical says why one path segment is not in its single canonical form, or
// returns "" when it is. Canonical means: not empty, not a dot segment, only
// characters RFC 3986 allows unencoded in a segment, and percent-encoding with
// upper-case hex only for characters that need it. An encoded "/" needs no rule of
// its own: "/" is never unreserved, but it is refused because it would split the
// segment for the API server and not for this check.
func nonCanonical(s string) string {
	switch s {
	case "":
		return "empty segment (repeated or trailing slash)"
	case ".", "..":
		return "dot segment"
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '%' {
			if !pchar(c) {
				return "character that must be percent-encoded"
			}
			continue
		}
		if i+2 >= len(s) || !upperHex(s[i+1]) || !upperHex(s[i+2]) {
			return "malformed or lower-case percent-encoding"
		}
		decoded := unhex(s[i+1])<<4 | unhex(s[i+2])
		if decoded == '/' {
			return "encoded slash"
		}
		if unreserved(decoded) {
			return "percent-encoding of a character that needs none"
		}
		i += 2
	}
	return ""
}

// unsupported returns the name of a subresource krm-foyer refuses in a resource
// path (segments start at "api" or "apis"), or "". Exec, attach and port-forward
// need upgrade protocols; the proxy subresources reach arbitrary applications and
// are deferred (docs/design.md, API contract).
func unsupported(segments []string) string {
	// Skip /api/{version} or /apis/{group}/{version}.
	rest := segments[min(len(segments), 2):]
	if segments[0] == "apis" {
		rest = segments[min(len(segments), 3):]
	}
	if len(rest) > 0 && rest[0] == "watch" { // the deprecated /watch/ prefix
		rest = rest[1:]
	}
	if len(rest) > 0 && rest[0] == "proxy" { // the removed /api/v1/proxy/ form
		return "proxy"
	}
	if len(rest) >= 3 && rest[0] == "namespaces" {
		rest = rest[2:]
	}
	// rest is now resource, name, subresource, ...
	if len(rest) >= 3 {
		switch rest[2] {
		case "exec", "attach", "portforward", "proxy":
			return rest[2]
		}
	}
	return ""
}

func unreserved(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
		c == '-' || c == '.' || c == '_' || c == '~'
}

// pchar reports whether c may appear unencoded in a path segment (RFC 3986:
// unreserved, sub-delims, ":" and "@").
func pchar(c byte) bool {
	return unreserved(c) || strings.IndexByte("!$&'()*+,;=:@", c) >= 0
}

func upperHex(c byte) bool { return '0' <= c && c <= '9' || 'A' <= c && c <= 'F' }

func unhex(c byte) byte {
	if c <= '9' {
		return c - '0'
	}
	return c - 'A' + 10
}
