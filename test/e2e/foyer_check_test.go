//go:build e2e

package e2e

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// backendSaw is what the stand-in domain backend behind /auth/check?identity=true
// (hello-web-nginx.conf, /public/whoami) answers: the identity header it received, and
// the method.
type backendSaw struct {
	Identity string `json:"identity"`
	Method   string `json:"method"`
}

// checkIdentity is Krm-Foyer-Identity, decoded.
type checkIdentity struct {
	UserInfo    json.RawMessage `json:"userInfo"`
	DisplayName string          `json:"displayName"`
	Connector   string          `json:"connector"`
	Issuer      string          `json:"issuer"`
	ExpiresAt   string          `json:"expiresAt"`
}

// forgedIdentity is a Krm-Foyer-Identity a browser makes up: the API server's own
// administrator, from the audience connector.
var forgedIdentity = base64.RawURLEncoding.EncodeToString(
	[]byte(`{"userInfo":{"username":"system:admin","groups":["system:masters"]},"connector":"room-pass"}`))

// The routes of traefik-routes.yaml: Traefik's ForwardAuth asking /auth/check, as
// docs/ingress.md's Traefik recipe has it, through the real Traefik.
var _ = Describe("krm-foyer's check, behind Traefik", Label("foyer"), func() {
	backend := func(ctx SpecContext, b *browser, method string, header http.Header) (answer, backendSaw) {
		h := http.Header{"Krm-Foyer-Identity": {forgedIdentity}}
		for k, v := range spoofedIdentity {
			h[k] = v
		}
		for k, v := range header {
			h[k] = v
		}
		a := b.do(ctx, method, "/public/whoami", nil, h)
		var saw backendSaw
		if a.Code == http.StatusOK {
			Expect(json.Unmarshal(a.Body, &saw)).To(Succeed(), "%s", a.Body)
		}
		return a, saw
	}

	It("tells a domain backend who the user is, as the API server says, whatever the browser forges", func(ctx SpecContext) {
		b := fx.frontDoorBrowser()

		By("refusing a signed-out browser before the backend is reached")
		a, _ := backend(ctx, b, http.MethodGet, nil)
		Expect(a.Code).To(Equal(http.StatusUnauthorized), "%s", a.Body)
		Expect(a.Header.Get("Krm-Foyer-Interruption")).To(Equal("Unauthorized"))
		Expect(a.status().Kind).To(Equal("Status"))

		By("passing on the API server's answer for the signed-in user, never the browser's copy")
		csrf := b.signedIn(ctx, alice)
		a, saw := backend(ctx, b, http.MethodGet, nil)
		// The backend's tripwire answers 400 to any cookie: a 200 means Traefik removed it.
		Expect(a.Code).To(Equal(http.StatusOK), "%s", a.Body)
		Expect(saw.Method).To(Equal(http.MethodGet))
		raw, err := base64.RawURLEncoding.DecodeString(saw.Identity)
		Expect(err).NotTo(HaveOccurred(), "the backend received %q", saw.Identity)
		var id checkIdentity
		Expect(json.Unmarshal(raw, &id)).To(Succeed())
		Expect(string(raw)).NotTo(ContainSubstring("system:admin"))
		Expect(string(raw)).NotTo(ContainSubstring("mallory"))
		Expect(string(raw)).NotTo(MatchRegexp(jwtShape))
		Expect(id.DisplayName).To(Equal("alice"))
		Expect(id.Issuer).To(Equal(fx.dexIssuer))

		By("naming exactly whom /auth/whoami names")
		who := b.get(ctx, "/auth/whoami")
		Expect(who.Code).To(Equal(http.StatusOK), "%s", who.Body)
		var whoami struct {
			UserInfo json.RawMessage `json:"userInfo"`
		}
		Expect(json.Unmarshal(who.Body, &whoami)).To(Succeed())
		Expect(id.UserInfo).To(MatchJSON(whoami.UserInfo))
		var info whoamiAnswer
		Expect(json.Unmarshal(who.Body, &info)).To(Succeed())
		Expect(info.UserInfo.Username).To(Equal(aliceK8sName))

		By("holding a write to the backend to the rules of a write to /k8s")
		a, _ = backend(ctx, b, http.MethodPost, http.Header{"Origin": {fx.foyerURL}})
		Expect(a.Code).To(Equal(http.StatusForbidden), "%s", a.Body)
		Expect(a.status().Reason).To(Equal("CSRFProofRequired"))
		a, _ = backend(ctx, b, http.MethodPost, http.Header{"Origin": {"https://evil.example"}, "X-Csrf-Token": {csrf}})
		Expect(a.Code).To(Equal(http.StatusForbidden), "%s", a.Body)
		Expect(a.status().Reason).To(Equal("CrossOriginRequest"))
		a, saw = backend(ctx, b, http.MethodPost, http.Header{"Origin": {fx.foyerURL}, "X-Csrf-Token": {csrf}})
		Expect(a.Code).To(Equal(http.StatusOK), "%s", a.Body)
		Expect(saw.Method).To(Equal(http.MethodPost))
		Expect(saw.Identity).NotTo(BeEmpty())

		By("refusing the session once it is signed out")
		out := b.do(ctx, http.MethodPost, "/auth/logout", nil, http.Header{"Origin": {fx.foyerURL}, "X-Csrf-Token": {csrf}})
		Expect(out.Code).To(Equal(http.StatusNoContent))
		a, _ = backend(ctx, b, http.MethodGet, nil)
		Expect(a.Code).To(Equal(http.StatusUnauthorized), "%s", a.Body)
	})

	It("sends a signed-out page load behind the login gate to the login, and back to that page", func(ctx SpecContext) {
		b := fx.frontDoorBrowser()
		page := http.Header{"Sec-Fetch-Mode": {"navigate"}, "Sec-Fetch-Dest": {"document"}}

		By("answering a script with the 401 it can act on")
		a := b.do(ctx, http.MethodGet, "/members/", nil, http.Header{"Sec-Fetch-Mode": {"cors"}, "Sec-Fetch-Dest": {"empty"}})
		Expect(a.Code).To(Equal(http.StatusUnauthorized), "%s", a.Body)

		By("sending a page load to the login, which returns to the page")
		a = b.do(ctx, http.MethodGet, "/members/", nil, page)
		Expect(a.Code).To(Equal(http.StatusFound), "%s", a.Body)
		// Absolute, at the public URL: Traefik would resolve a relative one against the
		// check's address, krm-foyer's Service.
		Expect(a.Header.Get("Location")).To(Equal(fx.foyerURL + "/auth/login?" + url.Values{"return_to": {"/members/"}}.Encode()))
		start := b.get(ctx, a.Header.Get("Location"))
		Expect(start.Code).To(Equal(http.StatusFound), "%s", start.Body)
		authorize, err := url.Parse(start.Header.Get("Location"))
		Expect(err).NotTo(HaveOccurred())
		done := b.get(ctx, b.atDex(ctx, authorize, alice))
		Expect(done.Code).To(Equal(http.StatusSeeOther))
		Expect(done.Header.Get("Location")).To(Equal("/members/"))

		By("serving the page once signed in, with no cookie past Traefik")
		a = b.do(ctx, http.MethodGet, "/members/", nil, page)
		Expect(a.Code).To(Equal(http.StatusOK), "%s", a.Body)
		Expect(string(a.Body)).To(ContainSubstring("For signed-in members."))
	})
})
