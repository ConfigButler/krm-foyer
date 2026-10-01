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

	It("rejects a genuine token whose claims were rewritten after signing", func(ctx SpecContext) {
		token := fx.login(ctx, alice, foyerClient, foyerSecret)
		code, _ := fx.selfSubjectReview(ctx, token)
		Expect(code).To(Equal(http.StatusCreated), "the unaltered token must be accepted, or the refusal below proves nothing")

		code, user := fx.selfSubjectReview(ctx, impersonateIn(token, bob))

		Expect(code).To(Equal(http.StatusUnauthorized))
		Expect(user).To(BeEmpty())
	})

	// Mapping the username from email is only safe if the issuer vouched for the address.
	// Kubernetes' own check lets a token without email_verified through, so the fixture
	// adds a rule; these tokens come from the test issuer, under the same rules as Dex.
	It("accepts a minted token with email_verified true, so the refusals below are about the claim", func(ctx SpecContext) {
		code, user := fx.selfSubjectReview(ctx, fx.mint(map[string]any{"email": "carol@example.com", "email_verified": true}))

		Expect(code).To(Equal(http.StatusCreated))
		Expect(user).To(Equal("oidc:carol@example.com"))
	})

	DescribeTable("refuses an email the issuer did not verify",
		func(ctx SpecContext, claims map[string]any) {
			claims["email"] = "carol@example.com"

			code, user := fx.selfSubjectReview(ctx, fx.mint(claims))

			Expect(code).To(Equal(http.StatusUnauthorized))
			Expect(user).To(BeEmpty())
		},
		Entry("without email_verified", map[string]any{}),
		Entry("with email_verified false", map[string]any{"email_verified": false}),
		Entry(`with email_verified the string "true"`, map[string]any{"email_verified": "true"}),
	)

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
