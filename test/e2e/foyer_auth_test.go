//go:build e2e

package e2e

import (
	. "github.com/onsi/ginkgo/v2"
)

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
		PIt("logs in through the configured issuer with PKCE, state and nonce, and sets only an opaque HttpOnly cookie")
		PIt("answers /auth/session with identity and CSRF information, and never a token")
		PIt("answers an API request without a session with a JSON 401, not a redirect and not the service account's view")
		PIt("sends the user's own token: the audit log names the user, not krm-foyer's service account, and no impersonation")
		PIt("drops Authorization and Impersonate-* headers sent by the browser")
		PIt("rejects a callback with a forged state, a replayed code or a foreign return path")
	})

	Context("does not invent authorization", func() {
		PIt("returns the API server's own answer for allowed, forbidden, missing, conflicting and invalid requests")
		PIt("follows a RoleBinding change on the next request, with no restart and no new login")
		PIt("passes a watch through exactly as the API server answers it, for a user who may list but not watch")
		PIt("rejects a non-canonical path instead of forwarding it")
	})

	Context("keeps the credential on the server", func() {
		PIt("never returns an ID, access or refresh token in any response the suite received")
		PIt("destroys the session on logout, so a replayed cookie gets a 401")
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
