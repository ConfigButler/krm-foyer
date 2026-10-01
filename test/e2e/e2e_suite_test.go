//go:build e2e

// Package e2e runs krm-foyer against a real API server that trusts a real Dex issuer.
// See docs/testing.md for what each part of the suite is meant to prove.
package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var fx *fixture

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "krm-foyer e2e")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	fx = loadFixture()

	By("waiting until the API server accepts a Dex token")
	// The API server fetches the issuer's discovery document in the background and
	// retries until Dex answers, so for a moment after bring-up every token is 401.
	token := fx.login(ctx, alice, foyerClient, foyerSecret)
	Eventually(func() int {
		return fx.direct(ctx, token, http.MethodPost, "/apis/authentication.k8s.io/v1/selfsubjectreviews",
			[]byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`)).Code
	}).WithContext(ctx).WithTimeout(90 * time.Second).WithPolling(2 * time.Second).
		Should(Equal(http.StatusCreated))

	By("waiting until the API server accepts a token from the test issuer")
	minted := fx.mint(map[string]any{"email": "carol@example.com", "email_verified": true})
	Eventually(func() int {
		code, _ := fx.selfSubjectReview(ctx, minted)
		return code
	}).WithContext(ctx).WithTimeout(90 * time.Second).WithPolling(2 * time.Second).
		Should(Equal(http.StatusCreated))

	By("checking the bait: krm-foyer's service account is cluster-admin, and its token is in the pod")
	// The no-fallback specs rely on a fallback being loud. If the bait were missing, a
	// fallback would be a quiet 403, and those specs would pass for the wrong reason.
	Expect(fx.kubectl("auth", "can-i", "*", "*", "--as="+fx.foyerAccount)).To(Equal("yes"))
	Expect(fx.kubectl("-n", fx.foyerNamespace, "get", "pods", "-l", "app=krm-foyer", "-o",
		"jsonpath={.items[*].spec.containers[0].volumeMounts[*].mountPath}")).
		To(ContainSubstring("/var/run/secrets/kubernetes.io/serviceaccount"))

	By("waiting until krm-foyer answers")
	Eventually(func() int {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fx.foyerURL+"/readyz", nil)
		Expect(err).NotTo(HaveOccurred())
		resp, err := fx.client.Do(req)
		if err != nil {
			return 0
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}).WithContext(ctx).WithTimeout(60 * time.Second).WithPolling(time.Second).Should(Equal(http.StatusOK))
}, NodeTimeout(3*time.Minute))

// The token scan, once more after every spec: no response krm-foyer sent the suite,
// and nothing it logged, holds a credential. See credentialLeaks.
var _ = AfterSuite(func() {
	if fx == nil || fx.foyerURL == "" {
		return
	}
	leaks, _ := fx.credentialLeaks()
	Expect(leaks).To(BeEmpty())
})

// eventually polls for things the API server records asynchronously, like audit events.
func eventually(ctx context.Context, f any) AsyncAssertion {
	return Eventually(f).WithContext(ctx).WithTimeout(20 * time.Second).WithPolling(500 * time.Millisecond)
}
