//go:build e2e

package e2e

import (
	"net/http"
	"net/url"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	sessionCookie = "__Host-krm-foyer-session"
	loginCookie   = "__Host-krm-foyer-login"
	// jwtShape matches the start of a JWT: a base64url JSON header, then a dot.
	jwtShape = `eyJ[A-Za-z0-9_-]{8,}\.`
)

func cookiesSet(a answer) []*http.Cookie {
	return (&http.Response{Header: a.Header}).Cookies()
}

// assertLoginRefused checks a refused login: krm-foyer's error page with its reason.
func assertLoginRefused(a answer, status int, reason string) {
	GinkgoHelper()
	Expect(a.Code).To(Equal(status), "%s", a.Body)
	Expect(a.Header.Get("Content-Type")).To(HavePrefix("text/html"))
	Expect(string(a.Body)).To(ContainSubstring("<code>" + reason + "</code>"))
	for _, c := range cookiesSet(a) {
		if c.Name == sessionCookie {
			Expect(c.Value).To(BeEmpty(), "a refused login set a session")
		}
	}
}

// assertNeverAudited checks that the request marked marker never reached the API
// server. The audit log is written asynchronously, so an empty answer means nothing
// on its own: a sentinel request made afterwards must show up first.
func assertNeverAudited(ctx SpecContext, marker string) {
	GinkgoHelper()
	sentinel := fx.direct(ctx, "", http.MethodGet, "/version", nil)
	eventually(ctx, func() []auditEvent { return fx.audited(sentinel.Marker) }).ShouldNot(BeEmpty())
	Expect(fx.audited(marker)).To(BeEmpty(), "the request reached the API server")
}

// assertAuditedAs waits for the audit event of the request marked marker, and checks
// the API server took it to be username's, with no impersonation.
func assertAuditedAs(ctx SpecContext, marker, username string, code int) {
	GinkgoHelper()
	var events []auditEvent
	eventually(ctx, func() []auditEvent { events = fx.audited(marker); return events }).Should(HaveLen(1))
	e := events[0]
	Expect(e.User.Username).To(Equal(username))
	Expect(e.User.Username).NotTo(Equal(fx.foyerAccount))
	Expect(e.ImpersonatedUser).To(BeNil())
	Expect(e.UserAgent).To(Equal(marker))
	Expect(e.ResponseStatus.Code).To(Equal(code))
}

// The claims krm-foyer has to prove before anyone should put it in front of a cluster.
// They are pending until the routes exist; each one becomes a real spec in the change
// that implements it. docs/testing.md explains the method behind them.
//
// Two techniques carry most of the weight:
//
//   - Differential answers. For the same user and the same request, the answer through
//     krm-foyer must equal the answer the API server gives that user's own token
//     directly (fixture.direct). If krm-foyer made any decision of its own, the two
//     would differ.
//   - The audit log as witness. The API server records who it believed made each
//     request. krm-foyer can influence what it sends, but not what the API server writes
//     down, so the log settles whose credential was used.
//
// The e2e deployment gives krm-foyer's service account cluster-admin on purpose. A
// fallback to it would then turn a 403 into a 200, which is loud, rather than into
// another 403, which is silent.
var _ = Describe("krm-foyer", Label("foyer"), func() {
	Context("does not invent authentication", func() {
		It("logs in through the configured issuer with PKCE, state and nonce, and sets only an opaque HttpOnly cookie", func(ctx SpecContext) {
			b := fx.browser()

			By("sending the browser to Dex with PKCE, state and nonce")
			start := b.get(ctx, "/auth/login?return_to="+url.QueryEscape("/apps/check?view=list"))
			Expect(start.Code).To(Equal(http.StatusFound))
			authorize, err := url.Parse(start.Header.Get("Location"))
			Expect(err).NotTo(HaveOccurred())
			q := authorize.Query()
			Expect(q.Get("client_id")).To(Equal(foyerClient))
			Expect(q.Get("redirect_uri")).To(Equal(fx.foyerURL + "/auth/callback"))
			Expect(q.Get("response_type")).To(Equal("code"))
			Expect(q.Get("code_challenge_method")).To(Equal("S256"))
			Expect(q.Get("code_challenge")).To(HaveLen(43))
			Expect(q.Get("state")).NotTo(BeEmpty())
			Expect(q.Get("nonce")).NotTo(BeEmpty())
			Expect(q.Get("scope")).NotTo(ContainSubstring("offline_access"), "no refresh token while there is no refresh")
			Expect(cookiesSet(start)).To(ConsistOf(HaveField("Name", loginCookie)))

			By("coming back from Dex to the path it asked for, with only an opaque session cookie")
			back := b.get(ctx, b.atDex(ctx, authorize, alice))
			Expect(back.Code).To(Equal(http.StatusSeeOther), "%s", back.Body)
			Expect(back.Header.Get("Location")).To(Equal("/apps/check?view=list"))
			set := cookiesSet(back)
			Expect(set).To(ConsistOf(HaveField("Name", loginCookie), HaveField("Name", sessionCookie)))
			for _, c := range set {
				Expect(c.Secure).To(BeTrue(), c.Name)
				Expect(c.HttpOnly).To(BeTrue(), c.Name)
				Expect(c.SameSite).To(Equal(http.SameSiteLaxMode), c.Name)
				Expect(c.Path).To(Equal("/"), c.Name)
				Expect(c.Domain).To(BeEmpty(), c.Name)
				if c.Name == loginCookie {
					Expect(c.MaxAge).To(BeNumerically("<", 0), "the login cookie outlives the callback")
					continue
				}
				// 32 random bytes, and nothing shaped like a token.
				Expect(c.Value).To(MatchRegexp(`^[A-Za-z0-9_-]{43}$`))
			}
		})

		It("answers /auth/session with identity and CSRF information, and never a token", func(ctx SpecContext) {
			b := fx.browser()
			b.signedIn(ctx, alice)
			a := b.get(ctx, "/auth/session")
			Expect(a.Code).To(Equal(http.StatusOK))
			Expect(a.Header.Get("Content-Type")).To(Equal("application/json"))
			Expect(a.Header.Get("Cache-Control")).To(Equal("no-store"))
			var s sessionState
			Expect(a.decode(&s)).To(Succeed())
			Expect(s.Authenticated).To(BeTrue())
			Expect(s.Email).To(Equal(alice))
			Expect(s.Issuer).To(Equal(fx.dexIssuer))
			Expect(s.CSRFToken).To(HaveLen(43))
			Expect(s.CSRFHeader).To(Equal("X-CSRF-Token"))
			Expect(s.ExpiresAt).NotTo(BeEmpty())
			Expect(string(a.Body)).NotTo(MatchRegexp(jwtShape), "a token in /auth/session")

			By("and without a session, a 401 that says so and nothing more")
			anonymous := fx.browser().get(ctx, "/auth/session")
			Expect(anonymous.Code).To(Equal(http.StatusUnauthorized))
			Expect(strings.TrimSpace(string(anonymous.Body))).To(Equal(`{"authenticated":false}`))
		})

		It("answers an API request without a session with a JSON 401, not a redirect and not the service account's view", func(ctx SpecContext) {
			// The bait could list every namespace. A 401 is not its answer.
			path := "/k8s/api/v1/namespaces"
			aliceToken := fx.login(ctx, alice, foyerClient, foyerSecret)
			for name, header := range map[string]http.Header{
				"nothing":                 nil,
				"a made-up session":       {"Cookie": {sessionCookie + "=" + strings.Repeat("A", 43)}},
				"the user's own token":    {"Authorization": {"Bearer " + aliceToken}},
				"an impersonation header": {"Impersonate-User": {"system:admin"}},
			} {
				a := fx.browser().do(ctx, http.MethodGet, path, nil, header)
				Expect(a.Code).To(Equal(http.StatusUnauthorized), name)
				Expect(a.Header.Get("Content-Type")).To(Equal("application/json"), name)
				Expect(a.Header.Get("Location")).To(BeEmpty(), name)
				Expect(a.status().Kind).To(Equal("Status"), name)
				Expect(a.status().Reason).To(Equal("Unauthorized"), name)
				Expect(string(a.Body)).NotTo(ContainSubstring("NamespaceList"), name)
				assertNeverAudited(ctx, a.Marker)
			}

			By("and a person opening it in a tab gets the same 401 as a page with a sign-in link")
			page := fx.browser().do(ctx, http.MethodGet, path, nil, http.Header{"Sec-Fetch-Dest": {"document"}})
			Expect(page.Code).To(Equal(http.StatusUnauthorized))
			Expect(page.Header.Get("Content-Type")).To(HavePrefix("text/html"))
			Expect(string(page.Body)).To(ContainSubstring(`href="/auth/login?return_to=%2Fk8s%2Fapi%2Fv1%2Fnamespaces"`))
		})

		It("sends the user's own token: the audit log names the user, not krm-foyer's service account, and no impersonation", func(ctx SpecContext) {
			ns := fx.namespace()
			b := fx.browser()
			b.signedIn(ctx, alice)
			path := "/k8s/api/v1/namespaces/" + ns + "/configmaps"

			By("being refused as alice, where the service account would be allowed")
			refused := b.get(ctx, path)
			Expect(refused.Code).To(Equal(http.StatusForbidden), "%s", refused.Body)
			Expect(refused.status().Message).To(ContainSubstring(`User "` + aliceK8sName + `"`))
			assertAuditedAs(ctx, refused.Marker, aliceK8sName, http.StatusForbidden)

			By("being allowed as alice once alice is granted")
			fx.grant(ns, aliceK8sName, "configmaps", "list")
			var allowed answer
			eventually(ctx, func() int { allowed = b.get(ctx, path); return allowed.Code }).Should(Equal(http.StatusOK))
			assertAuditedAs(ctx, allowed.Marker, aliceK8sName, http.StatusOK)
		})

		It("drops Authorization and Impersonate-* headers sent by the browser", func(ctx SpecContext) {
			ns := fx.namespace()
			bobK8sName := "oidc:" + bob
			fx.grant(ns, bobK8sName, "configmaps", "list")
			bobToken := fx.login(ctx, bob, foyerClient, foyerSecret)

			// alice may impersonate bob. Without that, a forwarded Impersonate-User
			// would be refused by the API server for alice anyway, and a 403 would
			// pass this spec for the wrong reason.
			role := "e2e-impersonate-" + randomID()
			fx.kubectl("create", "clusterrole", role, "--verb=impersonate", "--resource=users", "--resource-name="+bobK8sName)
			fx.kubectl("create", "clusterrolebinding", role, "--clusterrole="+role, "--user="+aliceK8sName)
			DeferCleanup(func() {
				fx.kubectl("delete", "clusterrolebinding", role)
				fx.kubectl("delete", "clusterrole", role)
			})
			path := "/api/v1/namespaces/" + ns + "/configmaps"
			eventually(ctx, func() int {
				return fx.directAs(ctx, fx.login(ctx, alice, foyerClient, foyerSecret), bobK8sName, path).Code
			}).Should(Equal(http.StatusOK), "the fixture does not let alice impersonate bob")

			b := fx.browser()
			b.signedIn(ctx, alice)
			// Each of these, if forwarded, makes the request bob's and turns alice's 403
			// into a 200. One per request, so none can hide another.
			for name, header := range map[string]http.Header{
				"Authorization":    {"Authorization": {"Bearer " + bobToken}},
				"Impersonate-User": {"Impersonate-User": {bobK8sName}},
				"Impersonate-* together": {
					"Impersonate-User": {bobK8sName}, "Impersonate-Group": {"system:authenticated"},
					"Impersonate-Uid": {"bob-uid"}, "Impersonate-Extra-Scopes": {"all"},
				},
				"front-proxy headers": {"X-Remote-User": {bobK8sName}, "X-Remote-Group": {"system:masters"}},
			} {
				a := b.do(ctx, http.MethodGet, "/k8s"+path, nil, header)
				Expect(a.Code).To(Equal(http.StatusForbidden), "%s: %s", name, a.Body)
				assertAuditedAs(ctx, a.Marker, aliceK8sName, http.StatusForbidden)
			}
		})

		It("rejects a callback with a forged state, a replayed code or a foreign return path", func(ctx SpecContext) {
			By("refusing a forged state, and starting no session")
			b := fx.browser()
			callback, err := url.Parse(b.atDex(ctx, b.startLogin(ctx, "/"), alice))
			Expect(err).NotTo(HaveOccurred())
			q := callback.Query()
			q.Set("state", "forged")
			callback.RawQuery = q.Encode()
			assertLoginRefused(b.get(ctx, callback.String()), http.StatusBadRequest, "state-mismatch")
			Expect(b.cookie(sessionCookie)).To(BeEmpty())

			By("refusing a replayed callback, even with the login cookie put back")
			b = fx.browser()
			authorize := b.startLogin(ctx, "/")
			loginID := b.cookie(loginCookie)
			replayed := b.atDex(ctx, authorize, alice)
			Expect(b.get(ctx, replayed).Code).To(Equal(http.StatusSeeOther))
			again := fx.browser()
			again.setCookie(loginCookie, loginID)
			assertLoginRefused(again.get(ctx, replayed), http.StatusBadRequest, "login-not-in-progress")
			Expect(again.cookie(sessionCookie)).To(BeEmpty())

			By("refusing an attacker's callback in a victim's browser (login CSRF)")
			attacker, victim := fx.browser(), fx.browser()
			planted := attacker.atDex(ctx, attacker.startLogin(ctx, "/"), bob)
			assertLoginRefused(victim.get(ctx, planted), http.StatusBadRequest, "login-not-in-progress")
			victim.startLogin(ctx, "/")
			assertLoginRefused(victim.get(ctx, planted), http.StatusBadRequest, "state-mismatch")
			Expect(victim.cookie(sessionCookie)).To(BeEmpty())

			By("refusing a return path that leaves the origin, before Dex is involved")
			for _, p := range []string{"https://evil.example/", "//evil.example", "///evil.example", "/\\evil.example", "/\t/evil.example"} {
				a := fx.browser().get(ctx, "/auth/login?"+url.Values{"return_to": {p}}.Encode())
				assertLoginRefused(a, http.StatusBadRequest, "return-path-not-local")
				Expect(a.Header.Get("Location")).To(BeEmpty())
			}
		})
	})

	Context("does not invent authorization", func() {
		PIt("returns the API server's own answer for allowed, forbidden, missing, conflicting and invalid requests")
		PIt("follows a RoleBinding change on the next request, with no restart and no new login")
		PIt("passes a watch through exactly as the API server answers it, for a user who may list but not watch")
		PIt("rejects a non-canonical path instead of forwarding it")
		PIt("refuses service, pod and node proxy subresources as unsupported, without reaching the backend")
	})

	Context("keeps the credential on the server", func() {
		PIt("never returns an ID, access or refresh token in any response the suite received")
		PIt("destroys the session on logout, so a replayed cookie gets a 401")
		PIt("ends the session when Dex refuses a refresh for a removed user, and records how long that took")
		PIt("requires CSRF proof and a same-origin request for mutations and logout")
	})

	// docs/ingress.md: krm-foyer terminates TLS itself by default, or sits behind a
	// transparent ingress. These run with an nginx container in front of it.
	Context("behind an ingress", func() {
		PIt("takes its redirect URI and CSRF origin from configuration, whatever Host or X-Forwarded-Host says")
		PIt("delivers watch events without the ingress buffering them")
		PIt("shares the origin with an application served by nginx on /, without seeing its requests")
		PIt("sends a signed-out page load to login through /auth/check, and back to that page afterwards")
		PIt("answers /auth/check with a status only: no token and no identity headers")
		PIt("ignores a return path from the ingress header that is not a local path")
	})
})
