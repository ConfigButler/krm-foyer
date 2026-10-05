//go:build e2e

package e2e

import (
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// A browser identity, the first step of docs/application-scope.md: the cluster's own
// authentication configuration names one person differently by the client the token
// was issued to, so an operator's broad grants go to their command-line name and never
// reach a script on an application's origin. Nothing in krm-foyer is involved; this
// proves the recipe the document gives.
var _ = Describe("A browser identity", Label("foyer"), func() {
	It("names the same person differently on the command line, and keeps that name's grants out of krm-foyer's reach", func(ctx SpecContext) {
		cli := fx.login(ctx, alice, cliClient, cliSecret)
		code, name := fx.selfSubjectReview(ctx, cli)
		Expect(code).To(Equal(http.StatusCreated))
		Expect(name).To(Equal("kubectl:" + alice))

		u := signIn(ctx, alice)
		code, name = fx.selfSubjectReview(ctx, u.token)
		Expect(code).To(Equal(http.StatusCreated))
		Expect(name).To(Equal(aliceK8sName))

		By("granting the command-line name, as an operator's admin grant would be")
		ns := fx.namespace()
		fx.grant(ns, "kubectl:"+alice, "secrets", "list")
		secrets := "/api/v1/namespaces/" + ns + "/secrets"
		Expect(fx.direct(ctx, cli, http.MethodGet, secrets, nil).Code).To(Equal(http.StatusOK))

		By("refusing the same person through krm-foyer, as the API server would refuse oidc:alice")
		a := u.viaFoyer(ctx, http.MethodGet, secrets, nil, nil)
		Expect(a.Code).To(Equal(http.StatusForbidden), "%s", a.Body)
		Expect(a.status().Message).To(ContainSubstring(`User "` + aliceK8sName + `"`))
		Expect(fx.direct(ctx, u.token, http.MethodGet, secrets, nil).Code).To(Equal(http.StatusForbidden))

		By("still refusing a client the cluster does not accept")
		code, _ = fx.selfSubjectReview(ctx, fx.login(ctx, alice, otherClient, otherSecret))
		Expect(code).To(Equal(http.StatusUnauthorized))
	})
})
