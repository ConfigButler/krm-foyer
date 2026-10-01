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
		It("returns the API server's own answer for allowed, forbidden, missing, conflicting and invalid requests", func(ctx SpecContext) {
			ns := fx.namespace()
			alice := signIn(ctx, alice)
			fx.grant(ns, alice.k8sName, "configmaps", "get", "list", "create", "update", "patch")
			cms := "/api/v1/namespaces/" + ns + "/configmaps"
			fx.kubectl("-n", ns, "create", "configmap", "taken", "--from-literal=k=v")
			eventually(ctx, func() int { return fx.direct(ctx, alice.token, http.MethodGet, cms, nil).Code }).
				Should(Equal(http.StatusOK))
			apply := http.Header{"Content-Type": {"application/apply-patch+yaml"}}
			merge := http.Header{"Content-Type": {"application/merge-patch+json"}}

			for _, tc := range []struct {
				name, method, path string
				body               []byte
				header             http.Header
				code               int
			}{
				{"allowed list", http.MethodGet, cms, nil, nil, http.StatusOK},
				{"allowed get", http.MethodGet, cms + "/taken", nil, nil, http.StatusOK},
				{"forbidden resource", http.MethodGet, "/api/v1/namespaces/" + ns + "/secrets", nil, nil, http.StatusForbidden},
				{"forbidden cluster scope", http.MethodGet, "/api/v1/namespaces", nil, nil, http.StatusForbidden},
				{"forbidden verb", http.MethodDelete, cms + "/taken", nil, nil, http.StatusForbidden},
				{"missing", http.MethodGet, cms + "/nope", nil, nil, http.StatusNotFound},
				{"missing, patched", http.MethodPatch, cms + "/nope", []byte(`{"data":{"a":"b"}}`), merge, http.StatusNotFound},
				{"already exists", http.MethodPost, cms, configMap("taken"), nil, http.StatusConflict},
				{"stale resourceVersion", http.MethodPut, cms + "/taken",
					[]byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"taken","resourceVersion":"1"}}`), nil, http.StatusConflict},
				{"invalid name", http.MethodPost, cms, configMap("Not_Valid"), nil, http.StatusUnprocessableEntity},
				{"unsupported media type", http.MethodPost, cms, []byte("k=v"), http.Header{"Content-Type": {"text/plain"}}, http.StatusUnsupportedMediaType},
				{"dry run", http.MethodPost, cms + "?dryRun=All", configMap("dry"), nil, http.StatusCreated},
				{"server-side apply, dry run", http.MethodPatch, cms + "/applied?fieldManager=e2e&dryRun=All",
					[]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: applied\ndata:\n  k: v\n"), apply, http.StatusCreated},
				// Authorization comes before routing: an unknown group is a 403 for alice.
				{"not an API group", http.MethodGet, "/apis/nothing.example.com/v1/things", nil, nil, http.StatusForbidden},
			} {
				By(tc.name)
				direct, _ := alice.compare(ctx, tc.method, tc.path, tc.body, tc.header)
				Expect(direct.Code).To(Equal(tc.code), "%s: the fixture answered differently than this spec expects: %s", tc.name, direct.Body)
			}

			By("creating for real: both get 201 and the same object, and both objects exist")
			created := []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"generateName":"real-"},"data":{"k":"v"}}`)
			direct, via := alice.compare(ctx, http.MethodPost, cms, created, nil)
			Expect(direct.Code).To(Equal(http.StatusCreated))
			names := fx.kubectl("-n", ns, "get", "configmaps", "-o", "name")
			for _, a := range []answer{direct, via} {
				var cm struct {
					Metadata struct{ Name string } `json:"metadata"`
				}
				Expect(a.decode(&cm)).To(Succeed())
				Expect(cm.Metadata.Name).To(HavePrefix("real-"))
				Expect(names).To(ContainSubstring("configmap/" + cm.Metadata.Name))
			}
			Expect(names).NotTo(ContainSubstring("configmap/dry"))

			By("asking Kubernetes who the user is: the same answer either way")
			review := []byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`)
			_, via = alice.compare(ctx, http.MethodPost, "/apis/authentication.k8s.io/v1/selfsubjectreviews", review, nil)
			Expect(string(via.Body)).To(ContainSubstring(`"username":"` + aliceK8sName + `"`))
		})

		It("follows a RoleBinding change on the next request, with no restart and no new login", func(ctx SpecContext) {
			ns := fx.namespace()
			alice := signIn(ctx, alice)
			path := "/api/v1/namespaces/" + ns + "/configmaps"
			Expect(alice.viaFoyer(ctx, http.MethodGet, path, nil, nil).Code).To(Equal(http.StatusForbidden))

			revoke := fx.grant(ns, alice.k8sName, "configmaps", "list")
			eventually(ctx, func() int { return alice.viaFoyer(ctx, http.MethodGet, path, nil, nil).Code }).
				Should(Equal(http.StatusOK))

			revoke()
			eventually(ctx, func() int { return alice.viaFoyer(ctx, http.MethodGet, path, nil, nil).Code }).
				Should(Equal(http.StatusForbidden))
		})

		It("passes a watch through exactly as the API server answers it, for a user who may list but not watch", func(ctx SpecContext) {
			ns := fx.namespace()
			alice := signIn(ctx, alice)
			fx.kubectl("-n", ns, "create", "configmap", "watched", "--from-literal=k=v")
			fx.grant(ns, alice.k8sName, "configmaps", "list")
			path := "/api/v1/namespaces/" + ns + "/configmaps"
			eventually(ctx, func() int { return alice.viaFoyer(ctx, http.MethodGet, path, nil, nil).Code }).
				Should(Equal(http.StatusOK))

			By("refusing the watch with the API server's own 403")
			direct, _ := alice.compare(ctx, http.MethodGet, path+"?watch=1", nil, nil)
			Expect(direct.Code).To(Equal(http.StatusForbidden))

			By("streaming it once watch is granted")
			fx.grant(ns, alice.k8sName, "configmaps", "watch")
			watch := path + "?watch=1&timeoutSeconds=2"
			eventually(ctx, func() int { return fx.direct(ctx, alice.token, http.MethodGet, watch, nil).Code }).
				Should(Equal(http.StatusOK))
			direct, via := alice.compare(ctx, http.MethodGet, watch, nil, nil)
			for _, a := range []answer{direct, via} {
				Expect(string(a.Body)).To(And(ContainSubstring(`"type":"ADDED"`), ContainSubstring(`"name":"watched"`)))
			}
		})

		It("rejects a non-canonical path instead of forwarding it", func(ctx SpecContext) {
			alice := signIn(ctx, alice)
			for _, path := range []string{
				"/api/v1//namespaces",
				"/api/v1/./namespaces",
				"/api/v1/namespaces/default/../kube-system/secrets",
				"/api/v1/namespaces/default%2Fsecrets",
				"/api/v1/namespaces/",
				"/api/v1/n%61mespaces",
			} {
				a := alice.viaFoyer(ctx, http.MethodGet, path, nil, nil)
				Expect(a.Code).To(Equal(http.StatusBadRequest), "%s: %s", path, a.Body)
				Expect(a.status().Kind).To(Equal("Status"), path)
				assertNeverAudited(ctx, a.Marker)
			}
		})

		It("refuses service, pod and node proxy subresources as unsupported, without reaching the backend", func(ctx SpecContext) {
			alice := signIn(ctx, alice)
			for _, path := range []string{
				"/api/v1/namespaces/default/pods/web-0/proxy",
				"/api/v1/namespaces/default/pods/web-0:8080/proxy/admin",
				"/api/v1/namespaces/default/services/web:80/proxy",
				"/api/v1/nodes/k3d-krm-foyer-e2e-server-0/proxy/metrics",
				"/api/v1/namespaces/default/pods/web-0/exec?command=sh",
				"/api/v1/namespaces/default/pods/web-0/attach",
				"/api/v1/namespaces/default/pods/web-0/portforward",
			} {
				a := alice.viaFoyer(ctx, http.MethodGet, path, nil, nil)
				Expect(a.Code).To(Equal(http.StatusNotImplemented), "%s: %s", path, a.Body)
				Expect(a.status().Kind).To(Equal("Status"), path)
				assertNeverAudited(ctx, a.Marker)
			}
		})
	})

	Context("keeps the credential on the server", func() {
		It("never returns an ID, access or refresh token in any response the suite received", func(ctx SpecContext) {
			By("making krm-foyer answer in every way it can: login, session, API, pages, refusals, logout")
			alice := signIn(ctx, alice)
			alice.b.get(ctx, "/auth/session")
			alice.viaFoyer(ctx, http.MethodGet, "/api/v1/namespaces", nil, nil)
			alice.viaFoyer(ctx, http.MethodPost, "/apis/authentication.k8s.io/v1/selfsubjectreviews",
				[]byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`), nil)
			alice.viaFoyer(ctx, http.MethodGet, "/api/v1//namespaces", nil, http.Header{"Sec-Fetch-Dest": {"document"}})
			alice.b.do(ctx, http.MethodPost, "/k8s/api/v1/namespaces", nil, nil)
			fx.browser().do(ctx, http.MethodGet, "/k8s/api/v1/namespaces", nil, http.Header{"Sec-Fetch-Dest": {"document"}})
			fx.browser().get(ctx, "/auth/login?return_to=//evil.example")
			fx.browser().get(ctx, "/auth/callback?code=x&state=y")
			alice.b.do(ctx, http.MethodPost, "/auth/logout", nil, alice.proof())
			fx.browser().get(ctx, "/auth/logged-out")

			By("scanning everything krm-foyer sent this suite so far, and everything it logged")
			leaks, responses := fx.credentialLeaks()
			Expect(responses).To(BeNumerically(">", 10), "the scan saw too little to mean anything")
			Expect(leaks).To(BeEmpty())
			// AfterSuite scans again, once every spec has run.
		})

		It("destroys the session on logout, so a replayed cookie gets a 401", func(ctx SpecContext) {
			alice := signIn(ctx, alice)
			copied := alice.b.cookie(sessionCookie)
			Expect(alice.viaFoyer(ctx, http.MethodGet, "/version", nil, nil).Code).To(Equal(http.StatusOK))

			out := alice.b.do(ctx, http.MethodPost, "/auth/logout", nil, alice.proof())
			Expect(out.Code).To(Equal(http.StatusNoContent), "%s", out.Body)
			Expect(alice.b.cookie(sessionCookie)).To(BeEmpty(), "the cookie was not cleared")

			replay := fx.browser()
			replay.setCookie(sessionCookie, copied)
			a := replay.get(ctx, "/k8s/version")
			Expect(a.Code).To(Equal(http.StatusUnauthorized))
			assertNeverAudited(ctx, a.Marker)
			Expect(replay.get(ctx, "/auth/session").Code).To(Equal(http.StatusUnauthorized))
		})

		PIt("ends the session when Dex refuses a refresh for a removed user, and records how long that took")

		It("requires CSRF proof and a same-origin request for mutations and logout", func(ctx SpecContext) {
			ns := fx.namespace()
			alice := signIn(ctx, alice)
			fx.grant(ns, alice.k8sName, "configmaps", "create", "delete")
			cms := "/k8s/api/v1/namespaces/" + ns + "/configmaps"
			other := signIn(ctx, alice.name)
			for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
				for name, tc := range map[string]struct {
					header http.Header
					reason string
				}{
					"no proof":                {http.Header{"Origin": {fx.foyerURL}}, "CSRFProofRequired"},
					"another session's proof": {http.Header{"Origin": {fx.foyerURL}, "X-Csrf-Token": {other.csrf}}, "CSRFProofRequired"},
					"another origin":          {http.Header{"Origin": {"https://evil.example"}, "X-Csrf-Token": {alice.csrf}}, "CrossOriginRequest"},
					"no origin":               {http.Header{"X-Csrf-Token": {alice.csrf}}, "CrossOriginRequest"},
				} {
					a := alice.b.do(ctx, method, cms+"/target", configMap("target"), tc.header)
					Expect(a.Code).To(Equal(http.StatusForbidden), "%s %s: %s", method, name, a.Body)
					Expect(a.status().Reason).To(Equal(tc.reason), "%s %s", method, name)
					assertNeverAudited(ctx, a.Marker)
				}
			}

			By("letting the same request through with both")
			Expect(alice.b.do(ctx, http.MethodPost, cms, configMap("made"), alice.proof()).Code).To(Equal(http.StatusCreated))

			By("refusing a logout from another origin or without proof, and keeping the session")
			for _, header := range []http.Header{
				{"Origin": {fx.foyerURL}},
				{"Origin": {"https://evil.example"}, "X-Csrf-Token": {alice.csrf}},
			} {
				Expect(alice.b.do(ctx, http.MethodPost, "/auth/logout", nil, header).Code).To(Equal(http.StatusForbidden))
				Expect(alice.b.session(ctx).Authenticated).To(BeTrue())
			}
		})
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
