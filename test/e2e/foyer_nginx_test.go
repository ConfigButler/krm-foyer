//go:build e2e

package e2e

import (
	"net/http"
	"net/url"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// docs/ingress.md's nginx recipe, as the document has it (front-door.sh copies it out
// of the text), in front of the main krm-foyer: a signed-out page load behind the gate
// goes to krm-foyer's login, and the login brings the browser back to the very page it
// asked for, whatever that page's query holds.
var _ = Describe("The nginx recipe", Label("foyer"), func() {
	// signInThrough loads page through nginx signed out, follows the gate to krm-foyer's
	// login and through Dex, and returns where the callback sends the browser.
	signInThrough := func(ctx SpecContext, b *browser, page string) (login *url.URL, back string) {
		GinkgoHelper()
		gate := b.get(ctx, page)
		Expect(gate.Code).To(Equal(http.StatusFound), "%s", gate.Body)
		login, err := url.Parse(gate.Header.Get("Location"))
		Expect(err).NotTo(HaveOccurred())
		Expect(login.Path).To(Equal("/auth/login"))

		started := b.get(ctx, login.String())
		Expect(started.Code).To(Equal(http.StatusFound), "krm-foyer refused the login the gate started: %s", started.Body)
		authorize, err := url.Parse(started.Header.Get("Location"))
		Expect(err).NotTo(HaveOccurred())
		callback := b.atDex(ctx, authorize, alice)
		done := b.get(ctx, callback)
		Expect(done.Code).To(Equal(http.StatusSeeOther), "%s", done.Body)
		return login, done.Header.Get("Location")
	}

	DescribeTable("returns to the whole page after login",
		func(ctx SpecContext, page string) {
			b := fx.nginxBrowser()
			login, back := signInThrough(ctx, b, page)
			Expect(back).To(Equal(page))
			Expect(login.Query()).To(Equal(url.Values{"return_to": {page}}),
				"the gate's login link carries the page as one return_to, and nothing else")

			By("and the backend receives the page as the browser asked for it")
			a := b.get(ctx, back)
			Expect(a.Code).To(Equal(http.StatusOK), "%s", a.Body)
			Expect(string(a.Body)).To(Equal("request=" + page + "\n"))
		},
		Entry("with several query parameters", "/admin/edit?name=x&tab=history"),
		Entry("with percent-encoding and a literal plus, each kept as sent",
			"/admin/search?q=a%26b%3Dc%25&tag=x+y&sum=1%2B1"),
		Entry("with a fragment-like and an equals sign encoded in a value", "/admin/edit?name=%23top&expr=a%3Db"),
		// Inside the page's query, these are the page's own. They must not reach the
		// login as krm-foyer's parameters, or as login parameters for the issuer.
		Entry("with parameters named like the login's own",
			"/admin/edit?return_to=%2Fevil&oidc.connector_id=other&name=x"),
		Entry("with them unencoded", "/admin/edit?return_to=/evil&oidc.login_hint=mallory"),
	)

	It("leaves krm-foyer's local-path check in charge of where the browser goes", func(ctx SpecContext) {
		b := fx.nginxBrowser()
		// nginx matches //admin/x as /admin/x, and gates it; the page the browser asked
		// for still starts with two slashes, which a browser reads as another host.
		gate := b.get(ctx, "https://foyer.localhost:8443//admin/x")
		Expect(gate.Code).To(Equal(http.StatusFound), "%s", gate.Body)
		login := b.get(ctx, gate.Header.Get("Location"))
		Expect(login.Code).To(Equal(http.StatusBadRequest), "%s", login.Body)
		Expect(login.Header.Get("Location")).To(BeEmpty())
	})

	It("turns a script's 401 into the login too, and passes a signed-in page and its identity to the backends", func(ctx SpecContext) {
		b := fx.nginxBrowser()
		a := b.do(ctx, http.MethodGet, "/admin/edit?name=x", nil, http.Header{"Sec-Fetch-Mode": {"cors"}})
		Expect(a.Code).To(Equal(http.StatusFound), "nginx turns every 401 from the check into the login: %s", a.Body)

		_, back := signInThrough(ctx, b, "/admin/")
		Expect(back).To(Equal("/admin/"))
		identity := b.get(ctx, "/public/whoami")
		Expect(identity.Code).To(Equal(http.StatusOK), "%s", identity.Body)
		Expect(string(identity.Body)).To(MatchRegexp(`(?m)^identity=[A-Za-z0-9_-]+$`))
	})
})
