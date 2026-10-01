//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// fixture is what start-cluster.sh left behind: where the API server and Dex are, and
// how to trust them. Nothing in it belongs to krm-foyer; it is the ground truth that
// krm-foyer's answers are compared against.
type fixture struct {
	dir             string
	apiServer       string
	dexIssuer       string
	serverContainer string
	// client trusts the cluster CA and the fixture CA, and reaches Dex by its issuer
	// name without the test runner needing a DNS entry for it.
	client *http.Client
}

const (
	password     = "password"
	foyerClient  = "krm-foyer"
	foyerSecret  = "krm-foyer-e2e-secret"
	otherClient  = "other-app"
	otherSecret  = "other-app-e2e-secret"
	alice        = "alice@example.com"
	bob          = "bob@example.com"
	aliceK8sName = "oidc:alice@example.com"
)

func loadFixture() *fixture {
	dir := os.Getenv("E2E_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", ".e2e")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "env"))
	Expect(err).NotTo(HaveOccurred(), "no e2e fixture found; run `task e2e-up` (or `task test-e2e`)")
	env := map[string]string{}
	for line := range strings.Lines(string(raw)) {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			env[k] = v
		}
	}

	roots := x509.NewCertPool()
	fixtureCA, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	Expect(err).NotTo(HaveOccurred())
	Expect(roots.AppendCertsFromPEM(fixtureCA)).To(BeTrue())
	clusterCA := kubectlOut(dir, "config", "view", "--raw", "-o",
		"jsonpath={.clusters[0].cluster.certificate-authority-data}")
	pem, err := base64.StdEncoding.DecodeString(clusterCA)
	Expect(err).NotTo(HaveOccurred())
	Expect(roots.AppendCertsFromPEM(pem)).To(BeTrue())

	issuer, err := url.Parse(env["DEX_ISSUER"])
	Expect(err).NotTo(HaveOccurred())
	dexAddr := net.JoinHostPort(env["DEX_IP"], issuer.Port())
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == issuer.Host {
				addr = dexAddr
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}

	return &fixture{
		dir:             dir,
		apiServer:       env["API_SERVER"],
		dexIssuer:       env["DEX_ISSUER"],
		serverContainer: env["SERVER_CONTAINER"],
		client: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
			// A test inspects redirects itself; following them hides what was answered.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// login returns a Dex ID token for user, issued to clientID. It uses the password
// grant so the suite can hold a user's own credential and ask the API server directly.
func (f *fixture) login(ctx context.Context, user, clientID, secret string) string {
	form := url.Values{
		"grant_type": {"password"},
		"username":   {user},
		"password":   {password},
		"scope":      {"openid email profile"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.dexIssuer+"/token", strings.NewReader(form.Encode()))
	Expect(err).NotTo(HaveOccurred())
	req.SetBasicAuth(clientID, secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.client.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	Expect(resp.StatusCode).To(Equal(http.StatusOK), "Dex token endpoint: %s", body)
	var tok struct {
		IDToken string `json:"id_token"`
	}
	Expect(json.Unmarshal(body, &tok)).To(Succeed())
	Expect(tok.IDToken).NotTo(BeEmpty())
	return tok.IDToken
}

// answer is one HTTP response, reduced to what the suite compares.
type answer struct {
	Code   int
	Header http.Header
	Body   []byte
	// Marker is the User-Agent the request carried, which finds it in the audit log.
	Marker string
}

// status decodes a Kubernetes Status body; it is empty for any other body.
func (a answer) status() (s struct {
	Kind    string `json:"kind"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Code    int    `json:"code"`
}) {
	_ = json.Unmarshal(a.Body, &s)
	return s
}

// direct asks the API server itself, with a bearer token, exactly as kubectl would.
// This is the reference answer for any request krm-foyer proxies.
func (f *fixture) direct(ctx context.Context, token, method, path string, body []byte) answer {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.apiServer+path, r)
	Expect(err).NotTo(HaveOccurred())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	marker := "krm-foyer-e2e/" + randomID()
	req.Header.Set("User-Agent", marker)
	resp, err := f.client.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	return answer{Code: resp.StatusCode, Header: resp.Header, Body: b, Marker: marker}
}

// auditEvent is the part of a kube-apiserver audit event the suite asserts on.
type auditEvent struct {
	AuditID    string `json:"auditID"`
	Stage      string `json:"stage"`
	RequestURI string `json:"requestURI"`
	Verb       string `json:"verb"`
	UserAgent  string `json:"userAgent"`
	User       struct {
		Username string   `json:"username"`
		Groups   []string `json:"groups"`
	} `json:"user"`
	ImpersonatedUser *struct {
		Username string `json:"username"`
	} `json:"impersonatedUser"`
	ResponseStatus struct {
		Code int `json:"code"`
	} `json:"responseStatus"`
}

// audited returns the completed audit events for requests that carried marker as
// their User-Agent. The API server writes the log asynchronously, so callers poll.
func (f *fixture) audited(marker string) []auditEvent {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "exec", f.serverContainer,
		"cat", "/etc/krm-foyer-e2e/audit.log").Output()
	Expect(err).NotTo(HaveOccurred())
	var events []auditEvent
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if !bytes.Contains(sc.Bytes(), []byte(marker)) {
			continue
		}
		var e auditEvent
		Expect(json.Unmarshal(sc.Bytes(), &e)).To(Succeed())
		if e.Stage == "ResponseComplete" {
			events = append(events, e)
		}
	}
	return events
}

// namespace creates a namespace for the current spec and deletes it afterwards.
func (f *fixture) namespace() string {
	ns := "e2e-" + randomID()
	f.kubectl("create", "namespace", ns)
	DeferCleanup(func() { f.kubectl("delete", "namespace", ns, "--wait=false") })
	return ns
}

// grant binds a Role allowing verbs on core resources in ns to a Kubernetes username.
// The returned function removes the binding again.
func (f *fixture) grant(ns, username, resource string, verbs ...string) (revoke func()) {
	name := "e2e-" + randomID()
	f.kubectl("-n", ns, "create", "role", name, "--resource="+resource, "--verb="+strings.Join(verbs, ","))
	f.kubectl("-n", ns, "create", "rolebinding", name, "--role="+name, "--user="+username)
	return func() { f.kubectl("-n", ns, "delete", "rolebinding", name) }
}

// kubectl runs as the fixture's admin, for setup only. Nothing a spec asserts on is
// obtained this way: an admin's answer says nothing about what a user may do.
func (f *fixture) kubectl(args ...string) string { return kubectlOut(f.dir, args...) }

func kubectlOut(dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", filepath.Join(dir, "kubeconfig")}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	Expect(err).NotTo(HaveOccurred(), "kubectl %s: %s", strings.Join(args, " "), stderr.String())
	return strings.TrimSpace(string(out))
}

func randomID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// tamper flips one character in the JWT payload, leaving a well-formed token whose
// signature no longer matches.
func tamper(jwt string) string {
	parts := strings.Split(jwt, ".")
	Expect(parts).To(HaveLen(3))
	p := []byte(parts[1])
	if p[5] == 'A' {
		p[5] = 'B'
	} else {
		p[5] = 'A'
	}
	return fmt.Sprintf("%s.%s.%s", parts[0], p, parts[2])
}
