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
}, NodeTimeout(3*time.Minute))

// eventually polls for things the API server records asynchronously, like audit events.
func eventually(ctx context.Context, f any) AsyncAssertion {
	return Eventually(f).WithContext(ctx).WithTimeout(20 * time.Second).WithPolling(500 * time.Millisecond)
}
