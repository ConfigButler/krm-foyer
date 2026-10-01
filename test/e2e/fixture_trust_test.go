//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// These specs involve no krm-foyer code. They establish that the fixture tells the
// truth: that the API server, not anything we wrote, decides who a token belongs to
// and what that user may do. Every krm-foyer spec compares its answers against this
// behavior, so if these fail, nothing else in the suite means anything.
var _ = Describe("The fixture's API server", Label("fixture"), func() {
	It("identifies a Dex user by the verified email in their token", func(ctx SpecContext) {
		token := fx.login(ctx, alice, foyerClient, foyerSecret)

		a := fx.direct(ctx, token, http.MethodPost, "/apis/authentication.k8s.io/v1/selfsubjectreviews",
			[]byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`))

		Expect(a.Code).To(Equal(http.StatusCreated), "%s", a.Body)
		var review struct {
			Status struct {
				UserInfo struct {
					Username string `json:"username"`
				} `json:"userInfo"`
			} `json:"status"`
		}
		Expect(json.Unmarshal(a.Body, &review)).To(Succeed())
		Expect(review.Status.UserInfo.Username).To(Equal(aliceK8sName))
	})

	It("lets RBAC alone decide, and changes its answer as soon as a grant changes", func(ctx SpecContext) {
		ns := fx.namespace()
		token := fx.login(ctx, alice, foyerClient, foyerSecret)
		path := "/api/v1/namespaces/" + ns + "/configmaps"

		By("refusing a user who has no grant, naming that user")
		before := fx.direct(ctx, token, http.MethodGet, path, nil)
		Expect(before.Code).To(Equal(http.StatusForbidden))
		Expect(before.status().Reason).To(Equal("Forbidden"))
		Expect(before.status().Message).To(ContainSubstring(`User "` + aliceK8sName + `"`))

		By("allowing the same token once a RoleBinding exists")
		revoke := fx.grant(ns, aliceK8sName, "configmaps", "list")
		eventually(ctx, func() int { return fx.direct(ctx, token, http.MethodGet, path, nil).Code }).
			Should(Equal(http.StatusOK))

		By("refusing it again once the RoleBinding is gone, with no new login")
		revoke()
		eventually(ctx, func() int { return fx.direct(ctx, token, http.MethodGet, path, nil).Code }).
			Should(Equal(http.StatusForbidden))
	})

	It("keeps one user's grant from reaching another user", func(ctx SpecContext) {
		ns := fx.namespace()
		fx.grant(ns, aliceK8sName, "configmaps", "list")
		bobToken := fx.login(ctx, bob, foyerClient, foyerSecret)

		a := fx.direct(ctx, bobToken, http.MethodGet, "/api/v1/namespaces/"+ns+"/configmaps", nil)

		Expect(a.Code).To(Equal(http.StatusForbidden))
		Expect(a.status().Message).To(ContainSubstring(`User "oidc:bob@example.com"`))
	})

	It("rejects a genuine Dex token that was issued to a different client", func(ctx SpecContext) {
		// Trusting an issuer is not trusting every token it signs: the audience must be
		// krm-foyer. Otherwise any application using the same Dex could act as its users.
		token := fx.login(ctx, alice, otherClient, otherSecret)

		a := fx.direct(ctx, token, http.MethodGet, "/api/v1/namespaces", nil)

		Expect(a.Code).To(Equal(http.StatusUnauthorized))
	})

	It("rejects a token whose payload was altered", func(ctx SpecContext) {
		token := tamper(fx.login(ctx, alice, foyerClient, foyerSecret))

		a := fx.direct(ctx, token, http.MethodGet, "/api/v1/namespaces", nil)

		Expect(a.Code).To(Equal(http.StatusUnauthorized))
	})

	It("records the requesting user, and no impersonation, in the audit log", func(ctx SpecContext) {
		ns := fx.namespace()
		token := fx.login(ctx, alice, foyerClient, foyerSecret)

		a := fx.direct(ctx, token, http.MethodGet, "/api/v1/namespaces/"+ns+"/configmaps", nil)

		eventually(ctx, func() []auditEvent { return fx.audited(a.Marker) }).Should(HaveLen(1))
		e := fx.audited(a.Marker)[0]
		Expect(e.User.Username).To(Equal(aliceK8sName))
		Expect(e.ImpersonatedUser).To(BeNil())
		Expect(e.ResponseStatus.Code).To(Equal(http.StatusForbidden))
	})
})
