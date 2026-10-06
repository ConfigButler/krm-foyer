//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	// testIssuer is trusted by the API server under the same rules as Dex, and the suite
	// holds its signing key, so it can mint tokens with claims Dex never issues.
	testIssuer string
	signingKey *rsa.PrivateKey
	// client trusts the cluster CA and the fixture CA, and reaches Dex and krm-foyer
	// by their names without the test runner needing DNS entries for them.
	client *http.Client

	// foyerURL is krm-foyer's public URL, deployed by deploy-foyer.sh, and
	// foyerAccount the service account it runs as: cluster-admin, as bait.
	foyerURL, foyerNamespace, foyerAccount string
	// sharedAccount is the identity shared watches are opened with: narrow, and not
	// the bait.
	sharedAccount string
	// briefTransport reaches the brief krm-foyer, whose sessions end within a minute,
	// instead of the main one.
	briefTransport http.RoundTripper
	// frontDoorTransport reaches krm-foyer's name through Traefik, the front door, as
	// a browser does: krm-foyer's routes and the application's, on one origin.
	frontDoorTransport http.RoundTripper
	// nginxTransport reaches krm-foyer's name through docs/ingress.md's nginx recipe
	// (nginx-door.conf), in front of the main krm-foyer.
	nginxTransport http.RoundTripper
	// rehearsalUsers is how many rehearsal users Dex has: rehearsal-001@example.com
	// and on, with alice's password (start-cluster.sh).
	rehearsalUsers int

	// seen is every response krm-foyer sent the suite, and tokens every token the
	// suite obtained, for the token scan.
	seenMu sync.Mutex
	seen   []seenResponse
	tokens []string
}

// remember records a token for the token scan.
func (f *fixture) remember(tokens ...string) {
	f.seenMu.Lock()
	defer f.seenMu.Unlock()
	for _, t := range tokens {
		if t != "" {
			f.tokens = append(f.tokens, t)
		}
	}
}

const (
	password    = "password"
	foyerClient = "krm-foyer"
	// The operator's command line: a client whose tokens the API server maps to
	// kubectl:<email>, not oidc:<email> (authentication-config.yaml).
	cliClient    = "kubectl"
	cliSecret    = "kubectl-e2e-secret"
	foyerSecret  = "krm-foyer-e2e-secret"
	otherClient  = "other-app"
	otherSecret  = "other-app-e2e-secret"
	alice        = "alice@example.com"
	bob          = "bob@example.com"
	aliceK8sName = "oidc:alice@example.com"
	bobK8sName   = "oidc:bob@example.com"
)

func loadFixture() *fixture {
	dir := os.Getenv("E2E_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", ".e2e")
	}
	env := readEnv(filepath.Join(dir, "env"), "no e2e fixture found; run `task e2e-up` (or `task test-e2e`)")
	for k, v := range readEnv(filepath.Join(dir, "foyer-env"), "krm-foyer is not deployed; run `task e2e-deploy` (or `task test-e2e`)") {
		env[k] = v
	}

	roots := x509.NewCertPool()
	fixtureCA, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	Expect(err).NotTo(HaveOccurred())
	Expect(roots.AppendCertsFromPEM(fixtureCA)).To(BeTrue())
	clusterCA := kubectlOut(dir, "config", "view", "--raw", "-o",
		"jsonpath={.clusters[0].cluster.certificate-authority-data}")
	clusterPEM, err := base64.StdEncoding.DecodeString(clusterCA)
	Expect(err).NotTo(HaveOccurred())
	Expect(roots.AppendCertsFromPEM(clusterPEM)).To(BeTrue())

	keyPEM, err := os.ReadFile(filepath.Join(dir, "issuer-signing.key"))
	Expect(err).NotTo(HaveOccurred())
	block, _ := pem.Decode(keyPEM)
	Expect(block).NotTo(BeNil())
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	Expect(err).NotTo(HaveOccurred())
	Expect(key).To(BeAssignableToTypeOf(&rsa.PrivateKey{}))

	issuer, err := url.Parse(env["DEX_ISSUER"])
	Expect(err).NotTo(HaveOccurred())
	rehearsalUsers, err := strconv.Atoi(env["REHEARSAL_USERS"])
	Expect(err).NotTo(HaveOccurred(), "REHEARSAL_USERS in the fixture's env (an older fixture? run task e2e-up)")
	// Every string contains the empty one: a missing account would pass assertions
	// that it never appears.
	Expect(env["FOYER_SHARED_ACCOUNT"]).NotTo(BeEmpty(), "FOYER_SHARED_ACCOUNT in foyer-env (an older deployment? run task e2e-deploy)")
	Expect(env["FOYER_NGINX_ADDR"]).NotTo(BeEmpty(), "FOYER_NGINX_ADDR in foyer-env (an older deployment? run task e2e-deploy)")
	foyer, err := url.Parse(env["FOYER_URL"])

	Expect(err).NotTo(HaveOccurred())
	// The names the suite reaches without DNS: Dex through the port-forward on this
	// container's loopback (port-forward.sh), as a browser does, and krm-foyer directly
	// through its NodePort on the node: the foyer specs test krm-foyer, not the front door.
	// The brief instance (foyer-brief-values.yaml) answers under the same name, on another
	// NodePort.
	transportTo := func(foyerAddr string) *http.Transport {
		addrs := map[string]string{
			issuer.Host: net.JoinHostPort("127.0.0.1", issuer.Port()),
			foyer.Host:  foyerAddr,
		}
		dialer := &net.Dialer{Timeout: 5 * time.Second}
		return &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if mapped, ok := addrs[addr]; ok {
					addr = mapped
				}
				return dialer.DialContext(ctx, network, addr)
			},
		}
	}
	transport := transportTo(env["FOYER_ADDR"])

	return &fixture{
		dir:             dir,
		apiServer:       env["API_SERVER"],
		dexIssuer:       env["DEX_ISSUER"],
		serverContainer: env["SERVER_CONTAINER"],
		testIssuer:      env["TEST_ISSUER"],
		signingKey:      key.(*rsa.PrivateKey),
		foyerURL:        env["FOYER_URL"],
		foyerNamespace:  env["FOYER_NAMESPACE"],
		foyerAccount:    env["FOYER_SERVICE_ACCOUNT"],
		sharedAccount:   env["FOYER_SHARED_ACCOUNT"],
		briefTransport:  transportTo(env["FOYER_BRIEF_ADDR"]),
		// The front door: Traefik, through the port-forward on this container's
		// loopback (port-forward.sh), under the same name as krm-foyer, as a browser
		// reaches it.
		frontDoorTransport: transportTo(net.JoinHostPort("127.0.0.1", foyer.Port())),
		nginxTransport:     transportTo(env["FOYER_NGINX_ADDR"]),
		rehearsalUsers:     rehearsalUsers,
		client: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
			// A test inspects redirects itself; following them hides what was answered.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func readEnv(file, missing string) map[string]string {
	raw, err := os.ReadFile(file)
	Expect(err).NotTo(HaveOccurred(), missing)
	env := map[string]string{}
	for line := range strings.Lines(string(raw)) {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			env[k] = v
		}
	}
	return env
}

// login returns a Dex ID token for user, issued to clientID. It uses the password
// grant so the suite can hold a user's own credential and ask the API server directly.
func (f *fixture) login(ctx context.Context, user, clientID, secret string) string {
	return f.loginScopes(ctx, user, clientID, secret, "openid email profile")
}

// loginScopes is login, asking Dex for scope.
func (f *fixture) loginScopes(ctx context.Context, user, clientID, secret, scope string) string {
	form := url.Values{
		"grant_type": {"password"},
		"username":   {user},
		"password":   {password},
		"scope":      {scope},
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
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	Expect(json.Unmarshal(body, &tok)).To(Succeed())
	Expect(tok.IDToken).NotTo(BeEmpty())
	f.remember(tok.IDToken, tok.AccessToken, tok.RefreshToken)
	return tok.IDToken
}

// mint returns a token from the test issuer for the krm-foyer audience, valid for five
// minutes, carrying claims on top of iss, aud, sub, iat and exp.
func (f *fixture) mint(claims map[string]any) string {
	now := time.Now()
	payload := map[string]any{
		"iss": f.testIssuer,
		"aud": foyerClient,
		"sub": "minted-" + randomID(),
		"iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
	}
	for k, v := range claims {
		payload[k] = v
	}
	signed := b64JSON(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "e2e"}) + "." + b64JSON(payload)
	digest := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.signingKey, crypto.SHA256, digest[:])
	Expect(err).NotTo(HaveOccurred())
	token := signed + "." + base64.RawURLEncoding.EncodeToString(sig)
	f.remember(token)
	return token
}

func b64JSON(v any) string {
	b, err := json.Marshal(v)
	Expect(err).NotTo(HaveOccurred())
	return base64.RawURLEncoding.EncodeToString(b)
}

// selfSubjectReview asks the API server who it takes token for. The answer is 201 with
// the username, or the refusal's status code with no username.
func (f *fixture) selfSubjectReview(ctx context.Context, token string) (int, string) {
	code, name, _ := f.selfSubjectReviewGroups(ctx, token)
	return code, name
}

// selfSubjectReviewGroups is selfSubjectReview with the groups the API server mapped.
func (f *fixture) selfSubjectReviewGroups(ctx context.Context, token string) (int, string, []string) {
	a := f.direct(ctx, token, http.MethodPost, "/apis/authentication.k8s.io/v1/selfsubjectreviews",
		[]byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`))
	var review struct {
		Status struct {
			UserInfo struct {
				Username string   `json:"username"`
				Groups   []string `json:"groups"`
			} `json:"userInfo"`
		} `json:"status"`
	}
	_ = json.Unmarshal(a.Body, &review)
	return a.Code, review.Status.UserInfo.Username, review.Status.UserInfo.Groups
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
	return f.directWith(ctx, token, method, path, body, nil)
}

// directWith is direct with extra request headers, such as a patch's Content-Type.
func (f *fixture) directWith(ctx context.Context, token, method, path string, body []byte, header http.Header) answer {
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
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := f.client.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	return answer{Code: resp.StatusCode, Header: resp.Header, Body: b, Marker: marker}
}

// directAs asks the API server directly with token, impersonating username. The
// suite uses it to check its own setup: that an impersonation grant works.
func (f *fixture) directAs(ctx context.Context, token, username, path string) answer {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.apiServer+path, nil)
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Impersonate-User", username)
	resp, err := f.client.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	return answer{Code: resp.StatusCode, Header: resp.Header, Body: b}
}

// auditEvent is the part of a kube-apiserver audit event the suite asserts on.
type auditEvent struct {
	AuditID string `json:"auditID"`
	Stage   string `json:"stage"`
	// When the API server received the request, and when it completed it.
	RequestReceivedTimestamp time.Time `json:"requestReceivedTimestamp"`
	StageTimestamp           time.Time `json:"stageTimestamp"`
	RequestURI               string    `json:"requestURI"`
	Verb                     string    `json:"verb"`
	UserAgent                string    `json:"userAgent"`
	User                     struct {
		Username string              `json:"username"`
		Groups   []string            `json:"groups"`
		Extra    map[string][]string `json:"extra"`
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

// grantGroup is grant, to a Kubernetes group instead of a username.
func (f *fixture) grantGroup(ns, group, resource string, verbs ...string) {
	name := "e2e-" + randomID()
	f.kubectl("-n", ns, "create", "role", name, "--resource="+resource, "--verb="+strings.Join(verbs, ","))
	f.kubectl("-n", ns, "create", "rolebinding", name, "--role="+name, "--group="+group)
}

// replaceFoyer replaces the main krm-foyer's pod by running kubectl with args in its
// namespace (a rollout restart, or a forced delete), and returns once the new pod is
// the only one and answers. Sessions are in their cookies, so they survive this.
func (f *fixture) replaceFoyer(ctx context.Context, args ...string) {
	GinkgoHelper()
	pods := func() string {
		return f.kubectl("-n", f.foyerNamespace, "get", "pods", "-l", "app.kubernetes.io/instance=krm-foyer",
			"-o", `jsonpath={range .items[*]}{.metadata.name}={.status.phase}{" "}{end}`)
	}
	old := pods()
	f.kubectl(append([]string{"-n", f.foyerNamespace}, args...)...)
	f.kubectl("-n", f.foyerNamespace, "rollout", "status", "deployment/krm-foyer", "--timeout=120s")
	// The old pod stops listening as soon as it is told to stop, and may still be
	// routed to for a moment: wait until it is gone, and the new one answers.
	eventually(ctx, func() error {
		now := strings.Fields(pods())
		if len(now) != 1 || !strings.HasSuffix(now[0], "=Running") || strings.Contains(old, now[0]) {
			return fmt.Errorf("pods %q, before %q", now, old)
		}
		return nil
	}).WithTimeout(2 * time.Minute).Should(Succeed())
	eventually(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.foyerURL+"/auth/session", nil)
		if err != nil {
			return err
		}
		resp, err := f.browser().client.Do(req)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			return fmt.Errorf("/auth/session answered %d", resp.StatusCode)
		}
		return nil
	}).Should(Succeed())
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

// kubectlApply applies a manifest as the fixture's admin, for setup only.
func (f *fixture) kubectlApply(manifest string) {
	GinkgoHelper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", filepath.Join(f.dir, "kubeconfig"), "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "kubectl apply: %s", out)
}

// kubectlWithWarnings runs kubectl as the fixture's admin and returns what it printed,
// the API server's warnings, which kubectl writes to stderr, included.
func (f *fixture) kubectlWithWarnings(args ...string) string {
	GinkgoHelper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", filepath.Join(f.dir, "kubeconfig")}, args...)...).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "kubectl %s: %s", strings.Join(args, " "), out)
	return string(out)
}

func randomID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// impersonateIn rewrites the email claim of a genuine token to someone else's, keeping
// the original header and signature. The payload stays valid JSON, so a refusal can only
// come from the signature check, not from a token the API server could not parse.
func impersonateIn(jwt, email string) string {
	parts := strings.Split(jwt, ".")
	Expect(parts).To(HaveLen(3))
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	Expect(err).NotTo(HaveOccurred())
	var claims map[string]any
	Expect(json.Unmarshal(raw, &claims)).To(Succeed())
	Expect(claims).To(HaveKey("email"))
	claims["email"] = email
	parts[1] = b64JSON(claims)

	// The altered payload must still decode to the intended claims.
	raw, err = base64.RawURLEncoding.DecodeString(parts[1])
	Expect(err).NotTo(HaveOccurred())
	Expect(json.Unmarshal(raw, &claims)).To(Succeed())
	Expect(claims).To(HaveKeyWithValue("email", email))
	return strings.Join(parts, ".")
}
