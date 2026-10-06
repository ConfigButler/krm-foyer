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

	// Every form a token's client can take, minted by the test issuer, which the API
	// server holds to Dex's rules. The client is azp when there is one, and then it must
	// be one of the audiences; without azp it is the audience, a string or a list of
	// one. Only krm-foyer and kubectl are clients, and anything else, or any doubt
	// about which client it is, is no identity at all: never the command line's name,
	// whose grants are the broad ones.
	Describe("by the token's client", Ordered, ContinueOnFailure, func() {
		const carol = "carol@example.com"
		var secrets string

		BeforeAll(func(ctx SpecContext) {
			ns := fx.namespace()
			secrets = "/api/v1/namespaces/" + ns + "/secrets"
			// The command line's grants, to its name and to one of its groups.
			fx.grant(ns, "kubectl:"+carol, "secrets", "get")
			fx.grantGroup(ns, "kubectl:admins", "secrets", "list")
			cli := mintFor(map[string]any{"aud": cliClient})
			eventually(ctx, func() int { return fx.direct(ctx, cli, http.MethodGet, secrets, nil).Code }).
				Should(Equal(http.StatusOK), "the group grant must work, or the refusals below prove nothing")
		})

		DescribeTable("names a token by its client",
			func(ctx SpecContext, claims map[string]any, prefix string) {
				token := mintFor(claims)
				code, name, groups := fx.selfSubjectReviewGroups(ctx, token)
				Expect(code).To(Equal(http.StatusCreated))
				Expect(name).To(Equal(prefix + carol))
				Expect(groups).To(ContainElement(prefix + "admins"))

				listed := fx.direct(ctx, token, http.MethodGet, secrets, nil).Code
				if prefix == "kubectl:" {
					Expect(listed).To(Equal(http.StatusOK))
				} else {
					Expect(listed).To(Equal(http.StatusForbidden), "the command line's group grant reached a browser token")
				}
			},
			Entry("krm-foyer's audience", map[string]any{"aud": foyerClient}, "oidc:"),
			Entry("krm-foyer's audience as a list of one", map[string]any{"aud": []string{foyerClient}}, "oidc:"),
			Entry("krm-foyer's audience, and krm-foyer as azp", map[string]any{"aud": foyerClient, "azp": foyerClient}, "oidc:"),
			// Dex's cross-client scope: krm-foyer asked for another client's audience.
			Entry("krm-foyer with the command line's audience too, as azp",
				map[string]any{"aud": []string{cliClient, foyerClient}, "azp": foyerClient}, "oidc:"),
			Entry("the command line's audience", map[string]any{"aud": cliClient}, "kubectl:"),
			Entry("the command line's audience as a list of one", map[string]any{"aud": []string{cliClient}}, "kubectl:"),
			Entry("the command line with krm-foyer's audience too, as azp",
				map[string]any{"aud": []string{foyerClient, cliClient}, "azp": cliClient}, "kubectl:"),
		)

		DescribeTable("refuses a token whose client is unknown or in doubt, and with it every grant",
			func(ctx SpecContext, claims map[string]any) {
				token := mintFor(claims)
				code, name, groups := fx.selfSubjectReviewGroups(ctx, token)
				Expect(code).To(Equal(http.StatusUnauthorized), "named %q, groups %q", name, groups)
				Expect(name).To(BeEmpty())
				Expect(fx.direct(ctx, token, http.MethodGet, secrets, nil).Code).To(Equal(http.StatusUnauthorized))
				Expect(fx.direct(ctx, token, http.MethodGet, secrets+"/any", nil).Code).To(Equal(http.StatusUnauthorized))
			},
			Entry("azp naming a client that is not an audience",
				map[string]any{"aud": foyerClient, "azp": "another-browser"}),
			Entry("azp naming krm-foyer, which is not an audience",
				map[string]any{"aud": cliClient, "azp": foyerClient}),
			Entry("azp naming another client among krm-foyer's audiences",
				map[string]any{"aud": []string{foyerClient, otherClient}, "azp": otherClient}),
			// What Dex issues another client that lists kubectl as a trusted peer.
			Entry("azp naming another client given the command line's audience",
				map[string]any{"aud": []string{cliClient, otherClient}, "azp": otherClient}),
			Entry("two audiences and no azp", map[string]any{"aud": []string{foyerClient, cliClient}}),
			Entry("two audiences the other way round and no azp", map[string]any{"aud": []string{cliClient, foyerClient}}),
			Entry("azp as a list", map[string]any{"aud": foyerClient, "azp": []string{foyerClient}}),
			Entry("azp as a number", map[string]any{"aud": foyerClient, "azp": 7}),
			Entry("azp as null", map[string]any{"aud": foyerClient, "azp": nil}),
			Entry("azp empty", map[string]any{"aud": foyerClient, "azp": ""}),
			Entry("another client's audience", map[string]any{"aud": otherClient}),
		)
	})
})

// mintFor mints a verified token for carol in the group admins, with claims on top.
func mintFor(claims map[string]any) string {
	all := map[string]any{"email": "carol@example.com", "email_verified": true, "groups": []string{"admins"}}
	for k, v := range claims {
		all[k] = v
	}
	return fx.mint(all)
}
