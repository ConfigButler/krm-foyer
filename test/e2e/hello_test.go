//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// headlessShell is Chromium for the browser specs, driven over the DevTools protocol.
const headlessShell = "chromedp/headless-shell:151.0.7922.109@sha256:2d349b544a1ea6b5b5fd7c0fe99215ff662339c57407ee2e8c0a11af93516b04"

// The hello example, used the way a person uses it: in a real browser, through the front
// door, with Dex's own login form. This is the claim that krm-foyer is usable, not only
// correct: an application with no backend of its own signs in, reads and changes a
// resource, and shows Kubernetes' 403 and 409 answers, and its script never holds a
// credential.
//
// The browser shares this container's network namespace, so foyer.localhost and
// dex.localhost resolve to loopback and reach the port-forwards (port-forward.sh), exactly
// as a person's browser does through VS Code's forwarding.
var _ = Describe("The hello example", Label("browser"), Ordered, func() {
	var (
		tab context.Context
		ns  string
	)

	BeforeAll(func(ctx SpecContext) {
		tab = startBrowser(ctx)
		ns = fx.namespace()
		fx.grant(ns, aliceK8sName, "notes.hello.krm-foyer.example", "get", "list", "watch", "create", "patch")
		fx.grant(ns, bobK8sName, "notes.hello.krm-foyer.example", "get", "list", "watch")
	}, NodeTimeout(2*time.Minute))

	noteText := func(name string) string {
		return fx.kubectl("-n", ns, "get", "notes.hello.krm-foyer.example", name, "-o", "jsonpath={.spec.text}")
	}

	It("signs alice in through Dex and lists the notes she may read", func() {
		run(tab, chromedp.Navigate(fx.foyerURL+"/?namespace="+ns))
		run(tab, chromedp.WaitVisible("#sign-in", chromedp.ByID), chromedp.Click("#sign-in", chromedp.ByID))
		signInAtDex(tab, alice)

		// Back where she started, namespace included: the return path survived the trip.
		var location, who string
		run(tab, chromedp.WaitVisible("#signed-in", chromedp.ByID),
			chromedp.Location(&location), chromedp.Text("#who", &who, chromedp.ByID))
		Expect(location).To(Equal(fx.foyerURL + "/?namespace=" + ns))
		Expect(who).To(Equal(alice))
		expectStatus(tab, "ok", "0 notes")
	})

	It("keeps every credential out of the page's reach", func() {
		var cookies, storage, sessionAnswer string
		run(tab,
			chromedp.Evaluate(`document.cookie`, &cookies),
			chromedp.Evaluate(`JSON.stringify([localStorage, sessionStorage])`, &storage),
			chromedp.Evaluate(`fetch('/auth/session').then(r => r.text())`, &sessionAnswer, awaitPromise),
		)
		Expect(cookies).To(BeEmpty(), "the session cookie must be HttpOnly")
		Expect(storage).To(Equal("[{},{}]"))
		Expect(sessionAnswer).To(ContainSubstring(`"authenticated":true`))
		Expect(sessionAnswer).NotTo(MatchRegexp(jwtShape))
	})

	It("creates and edits a note as alice", func() {
		run(tab,
			chromedp.SetValue("#new-name", "groceries", chromedp.ByID),
			chromedp.SetValue("#new-text", "milk", chromedp.ByID),
			chromedp.Click("#create", chromedp.ByID),
		)
		expectStatus(tab, "ok", "Created groceries.")
		Expect(noteText("groceries")).To(Equal("milk"))
		// The new note reaches the list through the stream, like anyone else's.
		expectEditor(tab, "groceries", "milk")

		run(tab,
			typeInto(`li[data-name="groceries"] textarea`, "milk, eggs"),
			chromedp.Click(`li[data-name="groceries"] .save`),
		)
		expectStatus(tab, "ok", "Saved groceries.")
		Expect(noteText("groceries")).To(Equal("milk, eggs"))
	})

	It("shows a change made elsewhere as it happens, without reloading", func() {
		fx.kubectl("-n", ns, "patch", "notes.hello.krm-foyer.example", "groceries",
			"--type=merge", "-p", `{"spec":{"text":"bread"}}`)
		expectEditor(tab, "groceries", "bread")
	})

	// krm-stream's store merges what arrives into what the user is typing: a change to
	// the same text is a conflict the page shows, and nothing is overwritten either way
	// until the user decides.
	It("shows a change to a note alice is editing as a conflict, and lets her take it", func() {
		run(tab, typeInto(`li[data-name="groceries"] textarea`, "bread, unsaved"))
		fx.kubectl("-n", ns, "patch", "notes.hello.krm-foyer.example", "groceries",
			"--type=merge", "-p", `{"spec":{"text":"bread, butter"}}`)

		var conflict, draft string
		run(tab,
			chromedp.WaitVisible(`li[data-name="groceries"] .conflict`),
			chromedp.Text(`li[data-name="groceries"] .conflict`, &conflict),
			chromedp.Value(`li[data-name="groceries"] textarea`, &draft),
		)
		Expect(conflict).To(ContainSubstring("bread, butter"))
		Expect(draft).To(Equal("bread, unsaved"), "a change from elsewhere replaced what alice typed")
		Expect(noteText("groceries")).To(Equal("bread, butter"))

		run(tab,
			chromedp.Click(`li[data-name="groceries"] .take-theirs`),
			chromedp.WaitNotVisible(`li[data-name="groceries"] .conflict`),
		)
		expectEditor(tab, "groceries", "bread, butter")
	})

	// A change to what the stream does not show (the last-applied-configuration
	// annotation, which every projection removes) moves the resourceVersion without an
	// event, so the page holds an older one. Kubernetes answers the save with 409; the
	// page keeps alice's text, saves nothing behind her back, and catches up.
	It("shows a 409 when the note changed out of sight, and saves only when asked again", func() {
		fx.kubectl("-n", ns, "annotate", "notes.hello.krm-foyer.example", "groceries", "--overwrite",
			`kubectl.kubernetes.io/last-applied-configuration={"note":"changed out of sight"}`)
		run(tab,
			typeInto(`li[data-name="groceries"] textarea`, "milk, eggs, flour"),
			chromedp.Click(`li[data-name="groceries"] .save`),
		)
		expectStatus(tab, "conflict", "failed with 409")
		expectStatus(tab, "conflict", "save again")
		var draft string
		run(tab, chromedp.Value(`li[data-name="groceries"] textarea`, &draft))
		Expect(draft).To(Equal("milk, eggs, flour"))
		Expect(noteText("groceries")).To(Equal("bread, butter"))

		run(tab, chromedp.Click(`li[data-name="groceries"] .save`))
		expectStatus(tab, "ok", "Saved groceries.")
		Expect(noteText("groceries")).To(Equal("milk, eggs, flour"))
	})

	// Creating a note adds it to the list, and leaves every other editor as it was:
	// text typed and not saved yet is the user's, not the page's to throw away.
	It("keeps an unsaved edit when alice creates another note", func() {
		run(tab,
			typeInto(`li[data-name="groceries"] textarea`, "flour, unsaved"),
			chromedp.SetValue("#new-name", "chores", chromedp.ByID),
			chromedp.SetValue("#new-text", "sweep", chromedp.ByID),
			chromedp.Click("#create", chromedp.ByID),
		)
		expectStatus(tab, "ok", "Created chores.")
		Expect(noteText("chores")).To(Equal("sweep"))
		expectEditor(tab, "chores", "sweep")

		var draft string
		var names []string
		run(tab,
			chromedp.Value(`li[data-name="groceries"] textarea`, &draft),
			chromedp.Evaluate(`[...document.querySelectorAll('#notes li')].map((li) => li.dataset.name)`, &names),
		)
		Expect(draft).To(Equal("flour, unsaved"))
		Expect(names).To(Equal([]string{"chores", "groceries"}), "in order of name")
		Expect(noteText("groceries")).To(Equal("milk, eggs, flour"))
	})

	// Signing in again starts a new session with a new CSRF token, and a page loaded before
	// still holds the old one. Its changes and its logout must still work.
	It("keeps saving and signs out after alice signs in again in another tab", func() {
		other, closeOther := chromedp.NewContext(tab)
		DeferCleanup(closeOther)
		Expect(chromedp.Run(other)).To(Succeed()) // opens the tab: see startBrowser
		signInAgain := func() {
			run(other, chromedp.Navigate(fx.foyerURL+"/auth/login?return_to=/"))
			signInAtDex(other, alice)
			run(other, chromedp.WaitVisible("#signed-in", chromedp.ByID))
		}

		signInAgain()
		run(tab,
			typeInto(`li[data-name="groceries"] textarea`, "bread, butter"),
			chromedp.Click(`li[data-name="groceries"] .save`),
		)
		expectStatus(tab, "ok", "Saved groceries.")
		Expect(noteText("groceries")).To(Equal("bread, butter"))

		// Again, so the logout below starts from a stale token too.
		signInAgain()
	})

	It("signs alice out", func() {
		var location string
		run(tab, chromedp.Click("#logout", chromedp.ByID))
		Eventually(func() string {
			run(tab, chromedp.Location(&location))
			return location
		}).WithTimeout(10 * time.Second).Should(Equal(fx.foyerURL + "/auth/logged-out"))

		run(tab, chromedp.Navigate(fx.foyerURL+"/?namespace="+ns), chromedp.WaitVisible("#sign-in", chromedp.ByID))
	})

	It("shows bob, who may only read, Kubernetes' 403 when he edits", func() {
		run(tab, chromedp.Click("#sign-in", chromedp.ByID))
		signInAtDex(tab, bob)
		var who string
		run(tab, chromedp.WaitVisible("#signed-in", chromedp.ByID), chromedp.Text("#who", &who, chromedp.ByID))
		Expect(who).To(Equal(bob))
		expectStatus(tab, "ok", "2 notes")
		expectEditor(tab, "groceries", "bread, butter")

		run(tab,
			typeInto(`li[data-name="groceries"] textarea`, "bob was here"),
			chromedp.Click(`li[data-name="groceries"] .save`),
		)
		// The message is the API server's own, naming bob: RBAC refused, not krm-foyer.
		expectStatus(tab, "refused", `User "oidc:bob@example.com" cannot patch resource "notes"`)
		Expect(noteText("groceries")).To(Equal("bread, butter"))
	})

	// The stream ends with the session: krm-foyer cuts it short, krm-stream's client
	// opens it again, gets the 401, and stops; the page says so.
	It("ends bob's live view when he signs out in another tab", func() {
		other, closeOther := chromedp.NewContext(tab)
		DeferCleanup(closeOther)
		Expect(chromedp.Run(other)).To(Succeed())
		run(other,
			chromedp.Navigate(fx.foyerURL+"/?namespace="+ns),
			chromedp.WaitVisible("#logout", chromedp.ByID),
			chromedp.Click("#logout", chromedp.ByID),
		)
		expectStatus(tab, "signed-out", "signed out")
		run(tab, chromedp.WaitVisible("#sign-in", chromedp.ByID))
	})
})

// startBrowser starts Chromium in this container's network namespace and returns a tab
// in it. Both are gone when the spec tree is done.
func startBrowser(ctx context.Context) context.Context {
	// Trust exactly the certificates of the front door and Dex, by their public keys:
	// the fixture's CA is not in the browser's store.
	trusted := []string{
		spki(filepath.Join(fx.dir, "foyer", "tls.crt")),
		spki(filepath.Join(fx.dir, "tls", "dex.crt")),
	}
	// This container: the devcontainer, or the CI job's container. Docker names it by
	// its hostname unless told otherwise.
	self, err := os.Hostname()
	Expect(err).NotTo(HaveOccurred())
	name := "krm-foyer-e2e-browser-" + randomID()
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--rm", "--name", name,
		"--network", "container:"+self, headlessShell,
		"--user-data-dir=/tmp/profile",
		"--ignore-certificate-errors-spki-list="+strings.Join(trusted, ","),
	).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "%s", out)
	DeferCleanup(func(ctx SpecContext) { _ = exec.CommandContext(ctx, "docker", "rm", "-f", name).Run() })

	devtools := "http://127.0.0.1:9222"
	Eventually(func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, devtools+"/json/version", nil)
		Expect(err).NotTo(HaveOccurred())
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())

	allocator, cancelAllocator := chromedp.NewRemoteAllocator(context.Background(), devtools)
	tab, cancelTab := chromedp.NewContext(allocator)
	DeferCleanup(func() { cancelTab(); cancelAllocator() })
	// The first Run opens the tab, which lives as long as the context it is given: so this
	// one gets the tab's own context, not run's shorter one.
	Expect(chromedp.Run(tab)).To(Succeed())
	return tab
}

// spki is the browser's pin for the certificate in file: the SHA-256 of its public key.
func spki(file string) string {
	raw, err := os.ReadFile(file)
	Expect(err).NotTo(HaveOccurred())
	block, _ := pem.Decode(raw)
	Expect(block).NotTo(BeNil())
	cert, err := x509.ParseCertificate(block.Bytes)
	Expect(err).NotTo(HaveOccurred())
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// run performs actions in the tab, each run bounded so a missing element fails the spec
// instead of hanging it.
func run(tab context.Context, actions ...chromedp.Action) {
	GinkgoHelper()
	ctx, cancel := context.WithTimeout(tab, 20*time.Second)
	defer cancel()
	Expect(chromedp.Run(ctx, actions...)).To(Succeed())
}

// signInAtDex fills in Dex's login form as user, once the browser has been sent there.
func signInAtDex(tab context.Context, user string) {
	GinkgoHelper()
	run(tab, chromedp.WaitVisible("#login", chromedp.ByID))
	var location string
	run(tab, chromedp.Location(&location))
	Expect(location).To(HavePrefix(fx.dexIssuer + "/"))
	run(tab,
		chromedp.SetValue("#login", user, chromedp.ByID),
		chromedp.SetValue("#password", password, chromedp.ByID),
		chromedp.Click("#submit-login", chromedp.ByID),
	)
}

// typeInto puts text into the field at sel as typing does: the value, then an input
// event, which the page listens for.
func typeInto(sel, text string) chromedp.Action {
	return chromedp.Tasks{
		chromedp.SetValue(sel, text),
		chromedp.Evaluate(`document.querySelector(`+strconv.Quote(sel)+`).dispatchEvent(new Event('input', {bubbles: true}))`, nil),
	}
}

// expectEditor waits for the editor of note name to show text.
func expectEditor(tab context.Context, name, text string) {
	GinkgoHelper()
	sel := `li[data-name="` + name + `"] textarea`
	var got string
	Eventually(func(g Gomega) {
		ctx, cancel := context.WithTimeout(tab, 5*time.Second)
		defer cancel()
		g.Expect(chromedp.Run(ctx, chromedp.WaitVisible(sel), chromedp.Value(sel, &got))).To(Succeed())
		g.Expect(got).To(Equal(text))
	}).WithTimeout(20 * time.Second).WithPolling(250 * time.Millisecond).Should(Succeed())
}

// expectStatus waits for the example's status line to report outcome with text in it.
func expectStatus(tab context.Context, outcome, text string) {
	GinkgoHelper()
	var got, gotOutcome string
	Eventually(func(g Gomega) {
		ctx, cancel := context.WithTimeout(tab, 5*time.Second)
		defer cancel()
		g.Expect(chromedp.Run(ctx,
			chromedp.Text("#status", &got, chromedp.ByID),
			chromedp.AttributeValue("#status", "data-outcome", &gotOutcome, nil, chromedp.ByID),
		)).To(Succeed())
		g.Expect(gotOutcome).To(Equal(outcome), "status: %s", got)
		g.Expect(got).To(ContainSubstring(text))
	}).WithTimeout(20 * time.Second).WithPolling(250 * time.Millisecond).Should(Succeed())
}

func awaitPromise(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }
