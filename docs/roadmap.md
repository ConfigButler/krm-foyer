# Roadmap

What is left to build, and in what order. The [design](design.md) says what krm-foyer
must do; this file tracks how far along that is.

**Nothing in the design's security model is implemented yet.** Server-held tokens,
access decided by Kubernetes alone and no service-account fallback are requirements. Each becomes a property of
krm-foyer when the test that tries to get past it exists and passes, and not before.

## Order of work

Each step makes pending specs real and ends with `task verify` green.

1. **The proxy, unit tests only.** Path checking first, as a pure function. Then the
   proxy, which takes the user's credential from the session and nothing else, so it can
   be tested against an `httptest` API server before login exists. There is deliberately no test-only way to hand krm-foyer a token: a back door
   behind a build tag is still a back door.
2. **OIDC login and sessions, deployed into the e2e fixture.** krm-foyer runs in the
   cluster with a cluster-admin bait service account, and the suite logs in by walking
   Dex's login form with a cookie jar. The differential, audit, session, CSRF, logout and
   token-scan specs go green together.
3. **Streams**, once the first two hold.
4. **An example domain** with pending, accepted, rejected and failed outcomes, and a
   second frontend on a different API group, with no application-specific code in
   krm-foyer. Then measure the operational cost against keeping auth and transport in
   each application.

## Checklist

Checked items exist today. An item is done when its test exists, not when its code does.
Security items need tests that try to get past the boundary.

### Engineering

- [x] `task verify` as the single gate, run the same way by CI
- [x] `go test -race` on every change
- [x] Image smoke test on a private Docker network
- [x] Actions pinned by SHA and tool versions in the devcontainer's ENV block
- [x] CI runs every check inside the devcontainer's `ci` stage, so nothing is installed
      on the runner
- [x] release-please with conventional commits, and build provenance for the image
- [x] e2e fixture on k3d: pinned k3s, Dex as the issuer, the API server trusting it
      through an `AuthenticationConfiguration`, and an audit log as witness. See
      [testing](testing.md)
- [x] The e2e job green in CI
- [x] PR title check for conventional commits (squash merges take the PR title)
- [ ] krm-foyer deployed into the e2e fixture (image imported with `k3d image import`),
      with the pending `foyer` specs made real
- [ ] Browser e2e with Playwright: log in, read, edit, get refused with 403, hit a 409,
      log out
- [ ] Coverage baseline that ratchets upward
- [ ] Fuzz tests for path checking and policy matching, with a short fuzz run in CI
- [ ] Helm chart with `values.schema.json`, `helm lint`, `helm template` tests, and e2e
      that installs through the chart
- [ ] Signed multi-arch image (cosign keyless) with an SBOM
- [ ] Docs lint: markdownlint and link checking
- [ ] Parse the whole squash message the way release-please does, once a dropped
      changelog entry makes it worth it

### Later, when an adopter needs it

- [ ] [Application scope](application-scope.md): first document a browser identity in the
      cluster's authentication config, proved by one e2e spec; then a scope list in
      krm-foyer for clusters where that is not possible

### Deployment

- [ ] krm-foyer serves TLS from a mounted certificate and reloads it on rotation
- [ ] Behind an ingress: the public URL from configuration and no trust in `Host` or
      `X-Forwarded-*`, with one e2e spec running nginx in front. See [ingress](ingress.md)
- [ ] Behind an ingress with re-encryption: the ingress verifies krm-foyer's certificate
- [ ] A NetworkPolicy in the chart that admits only the ingress to krm-foyer's port
- [ ] Helm chart values for both models
- [ ] Routing recipes for one shared domain: a Gateway API `HTTPRoute`, an nginx server
      block and a Vite dev-server proxy
- [ ] Login gate: `GET /auth/check` for an ingress gating the application's pages, with
      nginx and Traefik recipes, and `requireSession()` in the helper where there is no
      ingress support
- [ ] Later, when a hybrid application asks: identity headers from the check for a domain
      backend, never the token

### Access

- [ ] Every answer through krm-foyer equals the API server's answer for the same token
      (the differential specs)
- [ ] Non-canonical paths (`//`, `..`, encoded slashes) are rejected, not normalized, and
      the path forwarded is byte-for-byte the path received
- [ ] Browser-supplied `Authorization`, `Impersonate-*` and forwarding headers are
      stripped, and the session cookie never reaches Kubernetes
- [ ] Upstream responses: redirects are not followed or passed on, `Set-Cookie` and CORS
      headers are dropped, and every response is `Cache-Control: no-store`. See
      [upstream responses](design.md#upstream-responses)
- [ ] [Interruptions](design.md#interruptions) as pages for browser navigations and as
      `Status` for code, with the same status code; tests that a `fetch` cannot get the
      page form and that no Kubernetes answer is replaced
- [ ] A redirect notice that shows the full target and continues only on a click, and a
      held-back page shown as escaped text
- [ ] A test proves that no request falls back to the service account
- [ ] A test proves that no response, on any route, contains a token krm-foyer holds,
      including tokens obtained by refresh
- [ ] Exec, attach and port-forward return an explicit unsupported error

### Login and sessions

- [ ] OIDC authorization code with PKCE, state and nonce, through a maintained library
- [ ] Opaque server-side sessions: rotated at login, with idle and absolute expiry
- [ ] Refresh is serialized per session and bounded
- [ ] CSRF proof and same-origin checks on every mutation and on logout
- [ ] An unauthenticated API request gets a JSON 401, not a redirect
- [ ] `/auth/whoami` from a SelfSubjectReview, and `/auth/session`
- [ ] `/_foyer/access`: the rules for a namespace from a SelfSubjectRulesReview, and a
      "can I?" form answered by a SelfSubjectAccessReview. See
      [what may I do](design.md#what-may-i-do)
- [ ] Shared session storage, so more than one replica works
- [ ] The [session lifecycle](design.md#session-lifecycle) bounds, each with a test:
      logout seen by every replica at once, logout racing a refresh, the session store
      unavailable, and a stream open across logout and expiry
- [ ] Session IDs never appear in krm-foyer's logs or error pages

### Proxy semantics

- [ ] `Status` errors, content types, patch types, dry-run and Server-Side Apply pass
      through unchanged
- [ ] Mutations are never replayed, including after the session is refreshed
- [ ] Native watches and logs stream without buffering, and cancellation reaches the
      upstream
- [ ] Bounds on page size, response bytes, request rate, watch duration and concurrent
      streams

### Streams

- [ ] Host krm-stream, with RBAC deciding what a user may watch; which resources use a
      shared watch is configuration for efficiency, not access
- [ ] User-authenticated watches first
- [ ] Shared watches with per-subscriber SubjectAccessReview and bounded rechecks
- [ ] A stream ends when its session or its token expires, whichever comes first, and
      logout closes that session's streams
- [ ] A rehearsal with 200 identities, as a repeatable test

### Seeing what happened

- [ ] One log line per refusal (policy denial, upstream 401, 403, 409 or 422) with
      subject, route and reason
- [ ] Metrics for requests, refusals, active sessions and active streams

### Making it easy for others

- [ ] One task that brings up k3d, Dex and krm-foyer with a sample CRD, in minutes
- [ ] A minimal example frontend with no framework and no build step, which calls
      `fetch('/k8s/apis/...')`. It lives in `examples/`, not in the binary
- [ ] A small, framework-independent JavaScript helper: log in on a 401, show a 403 as a
      refusal, and treat a 409 as a conflict to reconcile
- [ ] A page for frontend developers: "the responses you will get and what they mean"
      (`generation` versus `resourceVersion`, 409 versus 403, what an empty list means)
- [ ] Voter's CoffeeConfig editor running on krm-foyer, replacing its own handlers
- [ ] A second consumer with a different API group: the reuse evidence the
      [release criteria](design.md#release-criteria) ask for
