//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The two user.extra keys gitops-reverser reads for a commit's author, which the
// fixture's authentication configuration maps from the signed token.
const (
	displayNameKey = "configbutler.ai/claims/display-name"
	emailKey       = "configbutler.ai/claims/email"
)

// spoofedIdentity is every header a browser might send to claim an identity or the
// attribution extras: impersonation, a front proxy's, and a bearer token of its own.
// None may change who the API server takes the user to be.
var spoofedIdentity = http.Header{
	"Impersonate-User":  {"system:admin"},
	"Impersonate-Group": {"system:masters"},
	"Impersonate-Extra-Configbutler.ai%2fclaims%2femail":        {"mallory@example.com"},
	"Impersonate-Extra-Configbutler.ai%2fclaims%2fdisplay-name": {"Mallory"},
	"X-Remote-User": {"mallory"},
	"X-Remote-Extra-Configbutler.ai%2fclaims%2femail": {"mallory@example.com"},
	"X-Forwarded-User":  {"mallory"},
	"X-Forwarded-Email": {"mallory@example.com"},
}

// whoamiAnswer is /auth/whoami's answer.
type whoamiAnswer struct {
	UserInfo struct {
		Username string              `json:"username"`
		UID      string              `json:"uid"`
		Groups   []string            `json:"groups"`
		Extra    map[string][]string `json:"extra"`
	} `json:"userInfo"`
	Issuer    string `json:"issuer"`
	ExpiresAt string `json:"expiresAt"`
}

var _ = Describe("krm-foyer's identity", Label("foyer"), func() {
	It("passes a login link's approved options to Dex, and refuses any other, before Dex sees it", func(ctx SpecContext) {
		b := fx.browser()
		link := "/auth/login?" + url.Values{
			"return_to": {"/room"}, "oidc.connector_id": {"local"}, "oidc.login_hint": {alice},
		}.Encode()
		a := b.get(ctx, link)
		Expect(a.Code).To(Equal(http.StatusFound), "%s", a.Body)
		authorize, err := url.Parse(a.Header.Get("Location"))
		Expect(err).NotTo(HaveOccurred())
		q := authorize.Query()
		Expect(q["connector_id"]).To(Equal([]string{"local"}))
		Expect(q["login_hint"]).To(Equal([]string{alice}))
		Expect(q).NotTo(HaveKey("return_to"))

		By("signing in at the connector chosen, and arriving where the link said")
		done := b.get(ctx, b.atDex(ctx, authorize, alice))
		Expect(done.Code).To(Equal(http.StatusSeeOther))
		Expect(done.Header.Get("Location")).To(Equal("/room"))

		By("refusing a connector not on the list, an unknown option and a protocol parameter")
		for _, query := range []string{"oidc.connector_id=ldap", "oidc.acr_values=x", "oidc.redirect_uri=https%3A%2F%2Fevil.example%2F"} {
			other := fx.browser()
			assertLoginRefused(other.get(ctx, "/auth/login?"+query), http.StatusBadRequest, "login-parameter-refused")
		}
	})

	It("shows the signed claims in /auth/session, the same shape when one is missing", func(ctx SpecContext) {
		b := fx.browser()
		b.signedIn(ctx, alice)
		a := b.get(ctx, "/auth/session")
		var s map[string]any
		Expect(json.Unmarshal(a.Body, &s)).To(Succeed())
		// Dex names the user after its static password's username. Groups are not
		// asked for, so there are none: an empty list, never a missing field.
		Expect(s).To(HaveKeyWithValue("displayName", "alice"))
		Expect(s).To(HaveKeyWithValue("groups", BeEmpty()))
		Expect(s).NotTo(HaveKey("connector"))
	})

	It("answers /auth/whoami with the API server's own identity of the user, extras included, whatever the browser claims", func(ctx SpecContext) {
		alice := signIn(ctx, alice)
		a := alice.b.do(ctx, http.MethodGet, "/auth/whoami?"+url.Values{emailKey: {"mallory@example.com"}}.Encode(), nil, spoofedIdentity)
		Expect(a.Code).To(Equal(http.StatusOK), "%s", a.Body)
		Expect(a.Header.Get("Cache-Control")).To(Equal("no-store"))
		var who whoamiAnswer
		Expect(json.Unmarshal(a.Body, &who)).To(Succeed())
		Expect(who.UserInfo.Username).To(Equal(aliceK8sName))
		Expect(who.UserInfo.Extra).To(HaveKeyWithValue(displayNameKey, []string{"alice"}))
		Expect(who.UserInfo.Extra).To(HaveKeyWithValue(emailKey, []string{alice.name}))
		Expect(who.Issuer).To(Equal(fx.dexIssuer))
		Expect(who.ExpiresAt).To(Equal(alice.b.session(ctx).ExpiresAt))
		Expect(string(a.Body)).NotTo(ContainSubstring("mallory"))
		Expect(string(a.Body)).NotTo(ContainSubstring("Mallory"))
		assertAuditedAs(ctx, a.Marker, aliceK8sName, http.StatusCreated)

		By("matching what the API server tells the user's own token directly")
		direct := fx.direct(ctx, alice.token, http.MethodPost, "/apis/authentication.k8s.io/v1/selfsubjectreviews",
			[]byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`))
		var review struct {
			Status struct {
				UserInfo json.RawMessage `json:"userInfo"`
			} `json:"status"`
		}
		Expect(json.Unmarshal(direct.Body, &review)).To(Succeed())
		var viaFoyer struct {
			UserInfo json.RawMessage `json:"userInfo"`
		}
		Expect(json.Unmarshal(a.Body, &viaFoyer)).To(Succeed())
		Expect(viaFoyer.UserInfo).To(MatchJSON(review.Status.UserInfo))

		By("answering a browser without a session with 401, and asking the API server nothing")
		out := fx.browser().get(ctx, "/auth/whoami")
		Expect(out.Code).To(Equal(http.StatusUnauthorized))
		assertNeverAudited(ctx, out.Marker)
	})

	It("attributes an accepted write to the signed claims, in admission and in the audit log, whatever the browser claims", func(ctx SpecContext) {
		ns := fx.namespace()
		fx.grant(ns, aliceK8sName, "configmaps", "create")
		captureAdmission(ns)
		alice := signIn(ctx, alice)

		a := alice.viaFoyer(ctx, http.MethodPost, "/api/v1/namespaces/"+ns+"/configmaps", configMap("attributed"), spoofedIdentity)
		Expect(a.Code).To(Equal(http.StatusCreated), "%s", a.Body)

		By("in the admission request, as a validating admission policy received it")
		Expect(a.Header.Values("Warning")).To(ContainElement(ContainSubstring(
			fmt.Sprintf("admission saw user=%s display-name=alice email=%s", aliceK8sName, alice.name))))

		By("in the audit event, with no impersonation and not as krm-foyer's account")
		assertAuditedAs(ctx, a.Marker, aliceK8sName, http.StatusCreated)
		events := fx.audited(a.Marker)
		Expect(events[0].User.Extra).To(HaveKeyWithValue(displayNameKey, []string{"alice"}))
		Expect(events[0].User.Extra).To(HaveKeyWithValue(emailKey, []string{alice.name}))
		for _, e := range events {
			for k, v := range e.User.Extra {
				Expect(strings.Join(v, ",")).NotTo(ContainSubstring("mallory"), k)
			}
		}
	})

	It("leaves an extra out when the signed token has no such claim, and lets nothing else fill it", func(ctx SpecContext) {
		minted := fx.mint(map[string]any{"email": "carol@example.com", "email_verified": true})
		a := fx.directWith(ctx, minted, http.MethodPost, "/apis/authentication.k8s.io/v1/selfsubjectreviews",
			[]byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`), nil)
		Expect(a.Code).To(Equal(http.StatusCreated), "%s", a.Body)
		var review struct {
			Status struct {
				UserInfo struct {
					Extra map[string][]string `json:"extra"`
				} `json:"userInfo"`
			} `json:"status"`
		}
		Expect(json.Unmarshal(a.Body, &review)).To(Succeed())
		Expect(review.Status.UserInfo.Extra).NotTo(HaveKey(displayNameKey))
		Expect(review.Status.UserInfo.Extra).To(HaveKeyWithValue(emailKey, []string{"carol@example.com"}))
	})
})

// captureAdmission makes every ConfigMap created in ns show, in a warning on the
// answer, the identity its admission request carried: a validating admission policy
// in Warn mode, which admits the write and reports what it saw. The API server builds
// a webhook's AdmissionRequest userInfo from the same identity, so this is what
// gitops-reverser's admission webhook would receive.
func captureAdmission(ns string) {
	GinkgoHelper()
	name := "e2e-capture-" + ns
	extra := func(key string) string {
		return fmt.Sprintf(`('%[1]s' in request.userInfo.extra ? request.userInfo.extra['%[1]s'].join(',') : '<none>')`, key)
	}
	policy := fmt.Sprintf(`apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: %[1]s
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: [""]
        apiVersions: [v1]
        operations: [CREATE]
        resources: [configmaps]
  validations:
    - expression: "false"
      messageExpression: >-
        'admission saw user=' + request.userInfo.username +
        ' display-name=' + %[3]s +
        ' email=' + %[4]s
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: %[1]s
spec:
  policyName: %[1]s
  validationActions: [Warn]
  matchResources:
    namespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: %[2]s
`, name, ns, extra(displayNameKey), extra(emailKey))
	fx.kubectlApply(policy)
	DeferCleanup(func() {
		fx.kubectl("delete", "validatingadmissionpolicybinding,validatingadmissionpolicy", name, "--ignore-not-found")
	})
	// A policy takes a moment to apply: wait until a write shows its warning.
	Eventually(func() string {
		probe := "probe-" + randomID()
		out := fx.kubectlWithWarnings("-n", ns, "create", "configmap", probe)
		fx.kubectl("-n", ns, "delete", "configmap", probe, "--ignore-not-found")
		return out
	}).WithTimeout(60 * time.Second).WithPolling(time.Second).Should(ContainSubstring("admission saw"))
}
