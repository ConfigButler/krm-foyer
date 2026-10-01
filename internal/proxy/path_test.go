package proxy

import (
	"net/http"
	"net/url"
	"path"
	"strings"
	"testing"
)

// Paths real Kubernetes clients send are forwarded, minus /k8s and nothing else.
func TestCheckPathAccepts(t *testing.T) {
	for _, raw := range []string{
		"/k8s/api",
		"/k8s/apis",
		"/k8s/version",
		"/k8s/openapi/v2",
		"/k8s/openapi/v3/apis/apps/v1",
		"/k8s/api/v1",
		"/k8s/api/v1/namespaces/team-a/configmaps",
		"/k8s/api/v1/namespaces/team-a/configmaps/settings",
		"/k8s/api/v1/namespaces/team-a/status",
		"/k8s/api/v1/namespaces/team-a/pods/web-0/log",
		"/k8s/api/v1/namespaces/team-a/pods/web-0/eviction",
		"/k8s/api/v1/nodes/node-1/status",
		"/k8s/apis/apps/v1/namespaces/team-a/deployments/web/scale",
		"/k8s/apis/workspaces.example.com/v1/namespaces/team-a/workspacerequests",
		// Names that happen to spell an unsupported subresource are just names.
		"/k8s/api/v1/namespaces/team-a/configmaps/exec",
		"/k8s/api/v1/namespaces/proxy/configmaps",
		// RBAC names contain ":"; Go clients send it raw, encodeURIComponent encodes it.
		"/k8s/apis/rbac.authorization.k8s.io/v1/clusterroles/system:aggregate-to-admin",
		"/k8s/apis/rbac.authorization.k8s.io/v1/clusterroles/system%3Aaggregate-to-admin",
		// A character that needs encoding is left for the API server to judge.
		"/k8s/api/v1/namespaces/team-a/configmaps/a%20b",
		"/k8s/api/v1/namespaces/team-a/configmaps/%25",
	} {
		got, refused := CheckPath(raw)
		if refused != nil {
			t.Errorf("CheckPath(%q) refused: %v", raw, refused)
			continue
		}
		if want := strings.TrimPrefix(raw, "/k8s"); got != want {
			t.Errorf("CheckPath(%q) = %q, want %q", raw, got, want)
		}
	}
}

// Non-canonical paths are rejected with 400, not normalized: there is one parse
// of a path, and it is of the bytes that are forwarded.
func TestCheckPathRejectsNonCanonical(t *testing.T) {
	for _, raw := range []string{
		"/k8s//api/v1/pods",
		"/k8s/api//v1/pods",
		"/k8s/api/v1/pods/",
		"/k8s/api/v1/../v1/pods",
		"/k8s/api/./v1/pods",
		"/k8s/api/v1/namespaces/..",
		"/k8s/api/v1/namespaces/team-a%2Fsecrets",
		"/k8s/api/v1/namespaces/team-a%2fsecrets",
		"/k8s/api/v1/namespaces/%2E%2E",
		"/k8s/api/v1/namespaces/%2e",
		"/k8s/api/v1/namespaces/te%61m-a",
		"/k8s/%61pi/v1/pods",
		"/k8s/api/v1/namespaces/team%2Da",
		"/k8s/api/v1/namespaces/team%7Ea",
		"/k8s/apis/rbac.authorization.k8s.io/v1/clusterroles/system%3aadmin",
		"/k8s/api/v1/pods%",
		"/k8s/api/v1/pods%4",
		"/k8s/api/v1/pods%zz",
		"/k8s/api/v1/a b",
		"/k8s/api/v1/a\\b",
		"/k8s/api/v1/a<b",
		"/k8s/api/v1/a\x00b",
		"/k8s/api/v1/caf\xc3\xa9",
		"/k8s/api/v1/a#b",
	} {
		_, refused := CheckPath(raw)
		if refused == nil || refused.Status != http.StatusBadRequest {
			t.Errorf("CheckPath(%q) = %v, want 400", raw, refused)
		}
	}
}

// Only the API routes of the contract are served under /k8s. The rest of the API
// server (healthz, metrics, logs) and anything outside /k8s is not a route.
func TestCheckPathOnlyAPIRoutes(t *testing.T) {
	for _, raw := range []string{
		"", "*", "/", "/api/v1/pods", "/k8s", "/k8s/", "/k8sapi/v1", "/k8s-x/api",
		"http://evil.example/k8s/api",
		"/k8s/healthz", "/k8s/livez", "/k8s/metrics", "/k8s/logs/kube-apiserver.log",
		"/k8s/.well-known/openid-configuration", "/k8s/openid/v1/jwks",
		"/k8s/version/x", "/k8s/openapi",
	} {
		_, refused := CheckPath(raw)
		if refused == nil || refused.Status != http.StatusNotFound {
			t.Errorf("CheckPath(%q) = %v, want 404", raw, refused)
		}
	}
}

// Upgrade-protocol subresources and the proxy subresources answer 501 before
// anything reaches the API server.
func TestCheckPathUnsupportedSubresources(t *testing.T) {
	for _, raw := range []string{
		"/k8s/api/v1/namespaces/team-a/pods/web-0/exec",
		"/k8s/api/v1/namespaces/team-a/pods/web-0/attach",
		"/k8s/api/v1/namespaces/team-a/pods/web-0/portforward",
		"/k8s/api/v1/namespaces/team-a/pods/web-0/proxy",
		"/k8s/api/v1/namespaces/team-a/pods/web-0:8080/proxy/admin/delete",
		"/k8s/api/v1/namespaces/team-a/services/web/proxy",
		"/k8s/api/v1/namespaces/team-a/services/https:web:443/proxy",
		"/k8s/api/v1/nodes/node-1/proxy/configz",
		"/k8s/api/v1/proxy/namespaces/team-a/services/web",
		"/k8s/api/v1/watch/namespaces/team-a/pods/web-0/exec",
		"/k8s/apis/example.com/v1/namespaces/team-a/things/a/proxy",
		"/k8s/apis/example.com/v1/things/a/exec",
	} {
		_, refused := CheckPath(raw)
		if refused == nil || refused.Status != http.StatusNotImplemented {
			t.Errorf("CheckPath(%q) = %v, want 501", raw, refused)
		}
	}
}

// FuzzCheckPath states the one-parser rule as a property: a path that is accepted
// is forwarded byte-for-byte minus /k8s, and the API server's own decoding of it
// can neither add a segment nor be cleaned into a different path.
func FuzzCheckPath(f *testing.F) {
	for _, seed := range []string{
		"/k8s/api/v1/namespaces/team-a/configmaps",
		"/k8s/apis/rbac.authorization.k8s.io/v1/clusterroles/system%3Aadmin",
		"/k8s/api/v1/namespaces/a%2Fb", "/k8s/api/v1/%2e%2E", "/k8s//api", "/k8s/api/v1/a%20b",
		"/k8s/api/v1/namespaces/a/pods/b/exec", "/k8s/version",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		forwarded, refused := CheckPath(raw)
		if refused != nil {
			return
		}
		if "/k8s"+forwarded != raw {
			t.Fatalf("CheckPath(%q) forwards %q", raw, forwarded)
		}
		decoded, err := url.PathUnescape(forwarded)
		if err != nil {
			t.Fatalf("CheckPath(%q) accepted a path that does not decode: %v", raw, err)
		}
		if got, want := strings.Count(decoded, "/"), strings.Count(forwarded, "/"); got != want {
			t.Fatalf("CheckPath(%q): decoding changes the segments (%d slashes, was %d)", raw, got, want)
		}
		if path.Clean(decoded) != decoded {
			t.Fatalf("CheckPath(%q): the decoded path %q cleans to %q", raw, decoded, path.Clean(decoded))
		}
	})
}
