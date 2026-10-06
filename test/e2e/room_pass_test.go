//go:build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Room Pass's QR login through krm-foyer (docs/room-pass.md), in a real browser, against
// Room Pass 2.0.0 and the Dex behind it (test/e2e/cluster/room-pass.sh): a participant
// scans the code on the presenter's screen, types a display name, and is signed in to
// the application through krm-foyer, with Kubernetes naming them as Room Pass's rules
// say. Three programs share the application's host, room.localhost: krm-foyer-room,
// Room Pass's /join, and the test application with the QR entry point. If their routes
// or the join cookie go wrong, the flow does not fail: Room Pass quietly asks for the
// code again. Only a browser shows that, so this spec checks the join page itself.
var _ = Describe("Room Pass's QR login", Label("browser", "room-pass"), Ordered, func() {
	var (
		room                 roomFixture
		tab                  context.Context
		started              time.Time
		ns                   string
		code, display, email string
		seen                 *seenTraffic
		name                 string
	)

	BeforeAll(func(ctx SpecContext) {
		room = loadRoom()
		started = time.Now()
		tab = startBrowser(ctx, filepath.Join(fx.dir, "room", "edge.crt"))
		seen = watchTraffic(tab)
		ns = fx.namespace()
		// The Room's audience group may write ConfigMaps here: the participant's grant.
		fx.grantGroup(ns, room.audienceGroup, "configmaps", "get", "create")
		// A name nobody in the Room has taken: the name is the participant's identity.
		name = "Ada Lovelace " + randomID()
		display = strings.ReplaceAll(name, " ", "-")
		email = strings.ToLower(display) + "@koudijs.dev.test"
	}, NodeTimeout(2*time.Minute))

	It("arrives at Room Pass with the scanned code already supplied", func() {
		code = room.currentCode()
		// From a link on another site, as a scanner app or a chat hands the URL on. The
		// redirects after it are then cross-site, which is what SameSite=Lax is for:
		// typed into the address bar, even a Strict cookie would come along.
		follow(tab, fx.foyerURL+"/", room.url+"/join-room?code="+code)

		// QR -> /join-room (the application) -> krm-foyer's login -> Dex -> Room Pass's
		// callback, /bind and confirm -> /join, back on the application's host.
		Expect(atJoinPage(tab)).To(HavePrefix(room.url + "/join?handoff="))
		pill, field := joinPage(tab)
		Expect(field).To(BeFalse(), "Room Pass asked for the room code: the join cookie did not reach /join")
		Expect(pill).To(Equal(code))

		By("having kept the code out of every URL but the QR code's own, and out of the authorization request")
		for _, u := range seen.urls() {
			if strings.HasPrefix(u, room.url+"/join-room?") {
				continue
			}
			Expect(u).NotTo(ContainSubstring(code), "the code travelled in a URL")
		}
		Expect(seen.urls()).To(ContainElement(HavePrefix(room.issuer+"/auth?")), "the login went through Room Pass's Dex")
		Expect(seen.urls()).To(ContainElement(ContainSubstring("connector_id=room-pass")))
	})

	It("signs the participant in with only a display name, and returns to the application's page", func() {
		run(tab,
			chromedp.SendKeys("#rp-name", name, chromedp.ByID),
			chromedp.Click(`form[action="/join"] button`),
		)
		Eventually(func() string {
			var location string
			run(tab, chromedp.Location(&location))
			return location
		}).WithTimeout(20 * time.Second).Should(Equal(room.url + "/room/"))
		var heading string
		run(tab, chromedp.Text("p", &heading))
		Expect(heading).To(Equal("You are in the room."))
	})

	It("shows connector room-pass and the display name in /auth/session, and keeps every credential from the page", func() {
		s := pageSession(tab)
		Expect(s.Authenticated).To(BeTrue())
		Expect(s.Issuer).To(Equal(room.issuer))
		Expect(s.Connector).To(Equal("room-pass"))
		Expect(s.DisplayName).To(Equal(display), "Room Pass stores the name folded, as it was shown on the join page")
		Expect(s.Email).To(Equal(email))
		Expect(s.Groups).To(Equal([]string{room.audienceGroup}))

		var cookies, storage string
		run(tab,
			chromedp.Evaluate(`document.cookie`, &cookies),
			chromedp.Evaluate(`JSON.stringify([localStorage, sessionStorage])`, &storage),
		)
		Expect(cookies).To(BeEmpty(), "every cookie on the application's host must be HttpOnly")
		Expect(storage).To(Equal("[{},{}]"))
		Expect(inPage(tab, `fetch('/auth/session').then(r => r.text())`)).NotTo(MatchRegexp(jwtShape))
	})

	var username string
	It("is the participant to Kubernetes, with both attribution extras on a permitted write", func() {
		var answer struct {
			UserInfo struct {
				Username string              `json:"username"`
				Groups   []string            `json:"groups"`
				Extra    map[string][]string `json:"extra"`
			} `json:"userInfo"`
		}
		whoami := inPage(tab, `fetch('/auth/whoami').then(r => r.text())`)
		Expect(whoami).NotTo(MatchRegexp(jwtShape))
		Expect(json.Unmarshal([]byte(whoami), &answer)).To(Succeed(), whoami)
		who := answer.UserInfo
		// Dex's opaque subject, never the name the participant typed.
		Expect(who.Username).To(HavePrefix("demo:"))
		Expect(who.Username).NotTo(ContainSubstring(display))
		Expect(who.Groups).To(ContainElement(room.audienceGroup))
		Expect(who.Extra).To(HaveKeyWithValue("configbutler.ai/claims/display-name", []string{display}))
		Expect(who.Extra).To(HaveKeyWithValue("configbutler.ai/claims/email", []string{email}))
		username = who.Username

		cm := "room-" + randomID()
		created := pageWrite(tab, "/k8s/api/v1/namespaces/"+ns+"/configmaps", string(configMap(cm)))
		Expect(created.Status).To(Equal(http.StatusCreated), created.Body)
		Expect(fx.kubectl("-n", ns, "get", "configmap", cm, "-o", "name")).To(Equal("configmap/" + cm))

		var write auditEvent
		eventually(context.Background(), func() []auditEvent {
			return slices.DeleteFunc(fx.audited(cm), func(e auditEvent) bool { return e.Verb != "create" })
		}).Should(HaveLen(1))
		write = slices.DeleteFunc(fx.audited(cm), func(e auditEvent) bool { return e.Verb != "create" })[0]
		Expect(write.ResponseStatus.Code).To(Equal(http.StatusCreated))
		Expect(write.User.Username).To(Equal(username))
		Expect(write.ImpersonatedUser).To(BeNil())
		Expect(write.User.Extra).To(HaveKeyWithValue("configbutler.ai/claims/display-name", []string{display}))
		Expect(write.User.Extra).To(HaveKeyWithValue("configbutler.ai/claims/email", []string{email}))

		By("and nothing the group was not granted")
		refused := pageWrite(tab, "/k8s/api/v1/namespaces/"+ns+"/secrets", `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"no"}}`)
		Expect(refused.Status).To(Equal(http.StatusForbidden), refused.Body)
		Expect(refused.Body).To(ContainSubstring(`User \"` + username + `\"`))
	})

	It("keeps the session when krm-foyer's pod is replaced", func(ctx SpecContext) {
		room.replaceFoyer(ctx)
		Eventually(func() int {
			return inPageStatus(tab, "/auth/whoami")
		}).WithTimeout(time.Minute).WithPolling(time.Second).Should(Equal(http.StatusOK))
		s := pageSession(tab)
		Expect(s.Authenticated).To(BeTrue())
		Expect(s.Connector).To(Equal("room-pass"))
		Expect(s.DisplayName).To(Equal(display))
		created := pageWrite(tab, "/k8s/api/v1/namespaces/"+ns+"/configmaps", string(configMap("after-"+randomID())))
		Expect(created.Status).To(Equal(http.StatusCreated), created.Body)
	}, SpecTimeout(3*time.Minute))

	It("signs out of krm-foyer, then of Room Pass, each by its own contract", func() {
		// A copy of the session cookie, taken as anyone who read it could. Logout clears
		// the browser's cookie; it revokes nothing (docs/design.md, "Sessions").
		copied := cookieNamed(tab, room.url, sessionCookie)
		Expect(copied).NotTo(BeEmpty())

		By("krm-foyer: POST /auth/logout with the session's CSRF proof")
		Expect(inPage(tab, `fetch('/auth/session').then(r => r.json()).then(s =>
			fetch('/auth/logout', {method: 'POST', headers: {[s.csrfHeader]: s.csrfToken}})).then(r => String(r.status))`)).
			To(Equal("204"))
		Expect(inPageStatus(tab, "/auth/session")).To(Equal(http.StatusUnauthorized))
		Expect(cookieNamed(tab, room.url, sessionCookie)).To(BeEmpty())

		By("which is not a revocation: the copy taken before is still a session until it expires")
		Expect(room.sessionWith(copied)).To(Equal(http.StatusOK))

		By("Room Pass: its sign-out form on /join, a POST with its own CSRF proof")
		run(tab, chromedp.Navigate(room.url+"/join"), chromedp.WaitVisible(`form[action="/logout"] button`))
		var welcome string
		run(tab, chromedp.Text("body", &welcome))
		Expect(welcome).To(ContainSubstring("You’re already enrolled as " + display))
		Expect(cookieNamed(tab, room.url, "__Host-rp-session")).NotTo(BeEmpty())
		run(tab, chromedp.Click(`form[action="/logout"] button`))
		Eventually(func() bool {
			_, field := joinPage(tab)
			return field
		}).WithTimeout(20*time.Second).Should(BeTrue(), "after Room Pass's sign-out, /join asks for a code again")
		Expect(cookieNamed(tab, room.url, "__Host-rp-session")).To(BeEmpty())

		By("so a new scan asks for a name again: this browser is no participant any more")
		follow(tab, fx.foyerURL+"/", room.url+"/join-room?code="+room.currentCode())
		atJoinPage(tab)
		var page string
		run(tab, chromedp.Text("body", &page))
		Expect(page).To(ContainSubstring("Choose a display name to join."))
		Expect(page).NotTo(ContainSubstring("already enrolled"))
	})

	// The failure this spec exists for, made on purpose: without the join cookie on the
	// application's host, Room Pass asks for the code, and the check above notices.
	It("tells the silent fallback apart: without the join cookie, Room Pass asks for the code", func() {
		run(tab, chromedp.Navigate(room.url+"/auth/login?return_to=%2Froom%2F&oidc.connector_id=room-pass"))
		atJoinPage(tab)
		pill, field := joinPage(tab)
		Expect(pill).To(BeEmpty())
		Expect(field).To(BeTrue())
	})

	It("leaves no credential in a response the browser saw, or in the logs of the edge, krm-foyer and Room Pass", func() {
		Expect(seen.leaks()).To(BeEmpty())
		for _, logs := range []struct{ ns, deployment string }{
			{fx.foyerNamespace, room.foyerRelease}, {room.namespace, "room-pass"}, {"fixture", "room-app"},
			{"traefik-system", "traefik"},
		} {
			text := fx.kubectl("-n", logs.ns, "logs", "deployment/"+logs.deployment,
				"--since-time="+started.UTC().Format(time.RFC3339), "--tail=-1")
			Expect(text).NotTo(ContainSubstring("/join-room?code="), "the QR URL in the log of %s", logs.deployment)
			Expect(text).NotTo(MatchRegexp(jwtShape), "the log of %s", logs.deployment)
			Expect(text).NotTo(ContainSubstring(code), "the join code in the log of %s", logs.deployment)
			for _, c := range seen.cookieValues() {
				Expect(text).NotTo(ContainSubstring(c), "a cookie in the log of %s", logs.deployment)
			}
		}
	})
})

// Dex's authproxy connector believes the X-Remote-* headers on its callback. Room Pass
// is the only thing that may set them: the edge strips them, Room Pass strips them, and
// a NetworkPolicy keeps everything but Room Pass from Dex's port.
var _ = Describe("Room Pass's Dex", Label("room-pass"), func() {
	It("is reachable only from Room Pass, so nothing else can forge the headers it believes", func(ctx SpecContext) {
		room := loadRoom()
		dex := "http://dex." + room.namespace + ".svc:5556/.well-known/openid-configuration"

		By("from a pod labelled as Room Pass, in its namespace: the policy's one way in")
		out, err := room.probe(ctx, room.namespace, "app=room-pass", dex)
		Expect(err).NotTo(HaveOccurred(), "%s", out)
		Expect(out).To(MatchRegexp(`"issuer":\s*"` + regexp.QuoteMeta(room.issuer) + `"`))

		By("from any other pod, here or in another namespace: no connection at all")
		// k3s's policy controller rejects the connection rather than dropping it. The
		// pod above reached the same Service a moment ago, so the refusal is the policy's.
		for _, from := range []struct{ ns, labels string }{
			{room.namespace, "app=not-room-pass"}, {"fixture", "app=room-pass"},
		} {
			out, err := room.probe(ctx, from.ns, from.labels, dex)
			Expect(err).To(HaveOccurred(), "a pod in %s with %s reached Dex: %s", from.ns, from.labels, out)
			Expect(out).To(ContainSubstring("can't connect to remote host"), "%s", out)
		}
	})

	It("never takes an identity from a browser's headers, even on Room Pass's own paths", func(ctx SpecContext) {
		room := loadRoom()
		c := room.client()
		// A browser's cookie jar: Room Pass binds a login to the browser that started it.
		jar, err := cookiejar.New(nil)
		Expect(err).NotTo(HaveOccurred())
		c.Jar = jar
		forged := http.Header{
			"X-Remote-User": {"mallory"}, "X-Remote-User-Id": {"mallory"}, "X-Remote-User-Name": {"mallory"},
			"X-Remote-User-Email": {"mallory@example.com"}, "X-Remote-Group": {"demo:krm-foyer-e2e"},
		}
		get := func(target string) *http.Response {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			Expect(err).NotTo(HaveOccurred())
			for k, v := range forged {
				req.Header[k] = v
			}
			resp, err := c.Do(req)
			Expect(err).NotTo(HaveOccurred())
			_ = resp.Body.Close()
			return resp
		}

		By("following a login from krm-foyer with the headers on every request: it ends at the join form")
		next := room.url + "/auth/login?return_to=%2Froom%2F&oidc.connector_id=room-pass"
		for range 10 {
			resp := get(next)
			if resp.StatusCode == http.StatusOK {
				break
			}
			Expect(resp.StatusCode).To(BeNumerically(">=", 300), "%s answered %d", next, resp.StatusCode)
			Expect(resp.StatusCode).To(BeNumerically("<", 400), "%s answered %d", next, resp.StatusCode)
			loc, err := resp.Request.URL.Parse(resp.Header.Get("Location"))
			Expect(err).NotTo(HaveOccurred())
			Expect(loc.Path).NotTo(Equal("/auth/callback"), "Dex issued a code for headers a browser sent")
			next = loc.String()
		}
		Expect(next).To(HavePrefix(room.url + "/join?handoff="))

		By("and Room Pass's callback and completion refuse them outright")
		Expect(get(room.issuer + "/callback/room-pass").StatusCode).To(Equal(http.StatusBadRequest))
		Expect(get(room.issuer + "/room-pass/complete?handoff=x").StatusCode).To(Equal(http.StatusForbidden))
	})
})

// roomFixture is what room-pass.sh left behind (E2E_DIR/room-env).
type roomFixture struct {
	url, issuer, namespace, name, audienceGroup, foyerRelease string
}

func loadRoom() roomFixture {
	GinkgoHelper()
	env := readEnv(filepath.Join(fx.dir, "room-env"), "Room Pass is not deployed; run `task e2e-deploy` (or `task test-e2e`)")
	return roomFixture{
		url: env["ROOM_URL"], issuer: env["ROOM_PASS_ISSUER"], namespace: env["ROOM_PASS_NAMESPACE"],
		name: env["ROOM_NAME"], audienceGroup: env["ROOM_AUDIENCE_GROUP"], foyerRelease: env["ROOM_FOYER_RELEASE"],
	}
}

// currentCode is the Room's newest join code, as Room Pass's presenter tool reads it:
// from the Room's status, with an operator's credential.
func (r roomFixture) currentCode() string {
	GinkgoHelper()
	var status struct {
		Status struct {
			ValidJoinCodes []struct {
				Code     string    `json:"code"`
				IssuedAt time.Time `json:"issuedAt"`
			} `json:"validJoinCodes"`
		} `json:"status"`
	}
	Expect(json.Unmarshal([]byte(fx.kubectl("-n", r.namespace, "get", "room", r.name, "-o", "json")), &status)).To(Succeed())
	codes := status.Status.ValidJoinCodes
	Expect(codes).NotTo(BeEmpty())
	newest := slices.MaxFunc(codes, func(a, b struct {
		Code     string    `json:"code"`
		IssuedAt time.Time `json:"issuedAt"`
	}) int {
		return a.IssuedAt.Compare(b.IssuedAt)
	})
	return newest.Code
}

// replaceFoyer restarts krm-foyer-room and returns once its new pod is the only one.
func (r roomFixture) replaceFoyer(ctx context.Context) {
	GinkgoHelper()
	selector := "app.kubernetes.io/instance=" + r.foyerRelease
	pods := func() string {
		return fx.kubectl("-n", fx.foyerNamespace, "get", "pods", "-l", selector,
			"-o", `jsonpath={range .items[*]}{.metadata.name}={.status.phase}{" "}{end}`)
	}
	old := pods()
	fx.kubectl("-n", fx.foyerNamespace, "rollout", "restart", "deployment/"+r.foyerRelease)
	fx.kubectl("-n", fx.foyerNamespace, "rollout", "status", "deployment/"+r.foyerRelease, "--timeout=120s")
	eventually(ctx, func() error {
		now := strings.Fields(pods())
		if len(now) != 1 || !strings.HasSuffix(now[0], "=Running") || strings.Contains(old, now[0]) {
			return fmt.Errorf("pods %q, before %q", now, old)
		}
		return nil
	}).WithTimeout(2 * time.Minute).Should(Succeed())
}

// client reaches both of the Room's hosts through the front door's port-forward, as a
// browser does, and follows no redirect.
func (r roomFixture) client() *http.Client {
	GinkgoHelper()
	roots := x509.NewCertPool()
	ca, err := os.ReadFile(filepath.Join(fx.dir, "ca.crt"))
	Expect(err).NotTo(HaveOccurred())
	Expect(roots.AppendCertsFromPEM(ca)).To(BeTrue())
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				_, port, _ := net.SplitHostPort(addr)
				return dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// sessionWith asks krm-foyer-room for /auth/session with a session cookie of this value.
func (r roomFixture) sessionWith(cookie string) int {
	GinkgoHelper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, r.url+"/auth/session", nil)
	Expect(err).NotTo(HaveOccurred())
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	resp, err := r.client().Do(req)
	Expect(err).NotTo(HaveOccurred())
	_ = resp.Body.Close()
	return resp.StatusCode
}

// probe fetches target from a short-lived pod in ns with labels, waiting at most five
// seconds, and returns what it printed. The pod is never ready, so no Service selecting
// its labels sends it traffic.
func (r roomFixture) probe(ctx context.Context, ns, labels, target string) (string, error) {
	GinkgoHelper()
	pod := "probe-" + randomID()
	overrides := `{"spec":{"automountServiceAccountToken":false,"containers":[{"name":"` + pod + `","image":"` + busyboxImage +
		`","args":["wget","-q","-T","5","-O","-",` + strconv.Quote(target) + `],` +
		`"readinessProbe":{"exec":{"command":["false"]}},` +
		`"securityContext":{"runAsNonRoot":true,"runAsUser":65534,"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}`
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", filepath.Join(fx.dir, "kubeconfig"),
		"-n", ns, "run", pod, "--rm", "-i", "--quiet", "--restart=Never",
		"--image="+busyboxImage, "--labels="+labels, "--overrides="+overrides).CombinedOutput()
	return string(out), err
}

// busyboxImage is start-cluster.sh's busybox.
const busyboxImage = "busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

// follow opens page, then a link on it to target: a navigation that another site
// started.
func follow(tab context.Context, page, target string) {
	GinkgoHelper()
	run(tab, chromedp.Navigate(page), chromedp.Evaluate(`{
		const a = document.createElement('a');
		a.href = `+strconv.Quote(target)+`;
		document.body.append(a);
		a.click();
	}`, nil))
}

// atJoinPage waits for Room Pass's join form and returns its URL. If it never comes,
// it fails with where the browser is instead, and what that page says.
func atJoinPage(tab context.Context) string {
	GinkgoHelper()
	var location, text string
	Eventually(func() bool {
		ctx, cancel := context.WithTimeout(tab, 5*time.Second)
		defer cancel()
		var form bool
		err := chromedp.Run(ctx,
			chromedp.Location(&location),
			chromedp.Evaluate(`document.body ? document.body.innerText : ''`, &text),
			chromedp.Evaluate(`document.readyState === 'complete' && !!document.getElementById('rp-name')`, &form),
		)
		return err == nil && form
	}).WithTimeout(20*time.Second).WithPolling(250*time.Millisecond).Should(BeTrue(),
		"no join form; the browser is at %s, which says:\n%.600s", location, text)
	return location
}

// joinPage reads Room Pass's join form: the code the page says was scanned, and
// whether it shows a field to type a code into instead.
func joinPage(tab context.Context) (scanned string, codeField bool) {
	GinkgoHelper()
	var state struct {
		Scanned string `json:"scanned"`
		Field   bool   `json:"field"`
	}
	raw := inPage(tab, `JSON.stringify({
		scanned: document.querySelector('.scanned strong')?.textContent ?? '',
		field: [...document.querySelectorAll('input[name="code"]')].some(i => i.type !== 'hidden'),
	})`)
	Expect(json.Unmarshal([]byte(raw), &state)).To(Succeed(), raw)
	return state.Scanned, state.Field
}

// roomSession is /auth/session's answer with the session claims.
type roomSession struct {
	sessionState
	DisplayName string   `json:"displayName"`
	Groups      []string `json:"groups"`
	Connector   string   `json:"connector"`
}

func pageSession(tab context.Context) roomSession {
	GinkgoHelper()
	raw := inPage(tab, `fetch('/auth/session').then(r => r.text())`)
	var s roomSession
	Expect(json.Unmarshal([]byte(raw), &s)).To(Succeed(), raw)
	return s
}

// inPage evaluates script in the tab's page and returns its string result.
func inPage(tab context.Context, script string) string {
	GinkgoHelper()
	var out string
	run(tab, chromedp.Evaluate(script, &out, awaitPromise))
	return out
}

// inPageStatus is the status of a GET of path from the page, or 0 if it failed.
func inPageStatus(tab context.Context, path string) int {
	GinkgoHelper()
	n, err := strconv.Atoi(inPage(tab, `fetch(`+strconv.Quote(path)+`).then(r => String(r.status), () => "0")`))
	Expect(err).NotTo(HaveOccurred())
	return n
}

// pageWrite POSTs body to path from the page, as the application would: with the
// session's CSRF proof, which only a page on the origin can read.
func pageWrite(tab context.Context, path, body string) (a struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
},
) {
	GinkgoHelper()
	raw := inPage(tab, `fetch('/auth/session').then(r => r.json()).then(s => fetch(`+strconv.Quote(path)+`, {
		method: 'POST',
		headers: {'Content-Type': 'application/json', [s.csrfHeader]: s.csrfToken},
		body: `+strconv.Quote(body)+`,
	})).then(async r => JSON.stringify({status: r.status, body: await r.text()}))`)
	Expect(json.Unmarshal([]byte(raw), &a)).To(Succeed(), raw)
	return a
}

// cookieNamed is the browser's cookie name for target, HttpOnly or not, or "".
func cookieNamed(tab context.Context, target, name string) string {
	GinkgoHelper()
	var cookies []*network.Cookie
	run(tab, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		cookies, err = network.GetCookies().WithURLs([]string{target}).Do(ctx)
		return err
	}))
	for _, c := range cookies {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// seenTraffic is every request URL and response header the browser saw, from the
// DevTools protocol, for the credential scan.
type seenTraffic struct {
	mu      sync.Mutex
	urlList []string
	headers []string
	cookies []string
}

func watchTraffic(tab context.Context) *seenTraffic {
	s := &seenTraffic{}
	chromedp.ListenTarget(tab, func(ev any) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch e := ev.(type) {
		case *network.EventRequestWillBeSent:
			s.urlList = append(s.urlList, e.Request.URL)
		case *network.EventResponseReceivedExtraInfo:
			// The raw headers, Set-Cookie included.
			for k, v := range e.Headers {
				value := fmt.Sprint(v)
				s.headers = append(s.headers, k+": "+value)
				if strings.EqualFold(k, "Set-Cookie") {
					for line := range strings.SplitSeq(value, "\n") {
						if _, rest, ok := strings.Cut(line, "="); ok {
							if v, _, _ := strings.Cut(rest, ";"); len(v) > 16 {
								s.cookies = append(s.cookies, v)
							}
						}
					}
				}
			}
		}
	})
	return s
}

func (s *seenTraffic) urls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.urlList)
}

func (s *seenTraffic) cookieValues() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.cookies)
}

// leaks is every URL or response header that holds something shaped like a JWT.
func (s *seenTraffic) leaks() (leaks []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, text := range append(slices.Clone(s.urlList), s.headers...) {
		if m := jwt.FindString(text); m != "" {
			leaks = append(leaks, fmt.Sprintf("%.60s: %.24s...", text, m))
		}
	}
	return leaks
}
