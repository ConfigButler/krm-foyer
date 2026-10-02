# Roadmap

What is left to build, and in what order. The [design](design.md) says what krm-foyer
must do; this file tracks how far along that is.

**The core of the security model is proved against a real cluster, and a person can use
it: `task demo` starts an example application in a browser.** Server-held tokens, access
decided by Kubernetes alone and no service-account fallback each have an e2e spec that
tries to get past them, against a real API server and Dex, with krm-foyer's service
account as cluster-admin bait. Each other requirement becomes a property of krm-foyer
when its test exists and passes, and not before. Until the bounds in step 4 exist, no
release is fit for a cluster that matters.

## Order of work

Each step makes pending specs real and ends with `task verify` green.

1. **The proxy, unit tests only** (done). Path checking first, as a pure function. Then the
   proxy, which takes the user's credential from the session and nothing else, so it can
   be tested against an `httptest` API server before login exists. There is deliberately no test-only way to hand krm-foyer a token: a back door
   behind a build tag is still a back door. So the binary does not serve `/k8s` until
   step 2 gives the proxy a credential source.
2. **OIDC login and sessions, deployed into the e2e fixture** (done). krm-foyer runs in the
   cluster with a cluster-admin bait service account, and the suite logs in by walking
   Dex's login form with a cookie jar. The differential, audit, session, CSRF, logout and
   token-scan specs go green together. The [interruption](design.md#interruptions) pages
   come with it: the 401 page's sign-in link needs login, and this is the first step at
   which a person can browse `/k8s`.

   Sessions live in memory, one replica, and there is no refresh: krm-foyer does not ask
   for `offline_access`, holds no refresh token, and a session ends with its ID token.
   That keeps the step small and gives the token scan a complete list of what to look
   for. Five changes, in order: sessions and CSRF (unit tests); OIDC login and the
   binary serving `/k8s`; interruption pages; deployment into the fixture with the login
   and identity specs; then the differential, CSRF, logout and token-scan specs.

3. **A working demo** (done). Moved ahead of refresh, shared storage and bounds
   (2026-10-01), because a first useful experience shows what the rest must serve, and
   the backend already worked against a real Dex and API server. One command, `task
   demo`, starts the [hello example](../examples/hello) for a browser on this machine:
   everything runs in one k3d cluster, a Gateway API front door (Traefik, the chart
   gitops-reverser uses) puts the example and krm-foyer on one origin, and
   `kubectl port-forward` brings the front door and Dex to
   `*.localhost`, which browsers resolve to loopback with no setup. The example
   signs in, lists, creates and edits a custom resource, shows Kubernetes' 403 and 409,
   and signs out, with no application-specific code in krm-foyer; the generic browser
   helper (`/_foyer/foyer.js`) came with it. Browser specs drive that journey in
   Chromium. It reloads on request rather than streaming, and keeps one replica,
   in-memory sessions and a new login when the ID token expires.
4. **Bounds**, before anyone runs krm-foyer for real: the request rate per session,
   concurrent requests per session and per replica, response duration and response
   bytes (counted decoded), each configurable with a documented default and a test that
   reaches it. Every open response ends when its session ends, at logout or expiry, and
   is cancelled at the API server, proved against the real cluster. Metrics show how
   close real traffic comes to each limit. Native watches stay open and are not a
   special case: krm-foyer does not tell them apart, and no bound needs it to. A bound on
   page size was left out (2026-10-02): it would not reliably bound what a list costs.
   See [bounds](bounds.md). Until this step is done, no release is fit for a cluster
   that matters.
5. **Live notes with krm-stream** (was step 6). Moved ahead of refresh and shared
   storage (2026-10-02), because live state is the experience krm-foyer is for: `/k8s`
   for reads and changes, krm-stream for what changes while a page is open. krm-foyer
   hosts krm-stream with user-authenticated upstream watches, and the hello example
   follows its notes live instead of reloading. Its bounds come with it: browser
   subscriptions and upstream watches counted separately (with sharing, several
   subscriptions use one upstream watch, and the `/k8s` bounds do not cover watches
   krm-stream opens itself), subscriptions closed when their session ends, and
   recovery after a disconnect tested. The rehearsal with 200 identities measures what
   one replica holds, which the per-replica defaults in [bounds](bounds.md) only
   assume.
6. **Refresh and shared session storage, when the demo shows the need** (was step 5, and
   before that 2b): refresh serialized per session and bounded, a refused refresh ending
   the session, the disablement bound measured for Dex, and a shared store so more than
   one replica works. The token scan then also reads the store, for tokens krm-foyer
   obtained by refresh.
7. **An example domain** with pending, accepted, rejected and failed outcomes, and a
   second frontend on a different API group, with no application-specific code in
   krm-foyer. Then measure the operational cost against keeping auth and transport in
   each application.

## Versions

krm-foyer stays below 1.0: `fix:` is a patch, and `feat:` and `feat!:` are a minor.
1.0 is a deliberate decision, made once other people run krm-foyer and are happy with
it, not a side effect of a commit.

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
- [x] krm-foyer deployed into the e2e fixture (image imported with `k3d image import`),
      with its service account as cluster-admin bait
- [x] The `foyer` specs for step 2 made real: login, identity, differential answers,
      RBAC changes, watches, path and subresource refusals, CSRF, logout and the token
      scan, each checked by deploying a krm-foyer broken on purpose
- [ ] The remaining `foyer` specs: refusal of a refresh (step 6), the ingress (later)
- [x] Browser e2e: log in, read, create, edit, get refused with 403, hit a 409, log out,
      and no credential within the page's reach. Chromium driven from the Go suite
      with chromedp, rather than Playwright: no Node or npm in CI, one language for the
      suite
- [x] `task demo`: the e2e fixture with the hello example behind a front door, for a
      browser on this machine
- [ ] Coverage baseline that ratchets upward
- [x] Fuzz tests for path checking, the upstream response check, the CSRF rule and
      return paths, with a short fuzz run of each in `task verify`
- [ ] Helm chart with `values.schema.json`, `helm lint`, `helm template` tests, and e2e
      that installs through the chart
- [ ] Signed multi-arch image (cosign keyless) with an SBOM
- [ ] Docs lint: markdownlint and link checking
- [ ] Parse the whole squash message the way release-please does, once a dropped
      changelog entry makes it worth it

### Later, when an adopter needs it

- [ ] Proxy subresources, with CSRF proof required on every request through them,
      whatever the method; a test that a cross-site `GET` navigation is refused before it
      reaches the backend

- [ ] [Application scope](application-scope.md): first document a browser identity in the
      cluster's authentication config, proved by one e2e spec; then a scope list in
      krm-foyer for clusters where that is not possible

### Deployment

- [x] krm-foyer serves TLS from a mounted certificate (the e2e deployment does)
- [ ] It reloads the certificate on rotation
- [ ] Behind an ingress: the public URL from configuration and no trust in `Host` or
      `X-Forwarded-*`, with one e2e spec running nginx in front. See [ingress](ingress.md)
- [ ] Behind an ingress with re-encryption: the ingress verifies krm-foyer's certificate
- [ ] A NetworkPolicy in the chart that admits only the ingress to krm-foyer's port, and
      only the monitoring system to the metrics port
- [ ] Helm chart values for both models
- [ ] Routing recipes for one shared domain: a Gateway API `HTTPRoute`, an nginx server
      block and a Vite dev-server proxy
- [ ] Login gate: `GET /auth/check` for an ingress gating the application's pages, with
      nginx and Traefik recipes, and `requireSession()` in the helper where there is no
      ingress support
- [ ] Later, when a hybrid application asks: identity headers from the check for a domain
      backend, never the token

### Access

- [x] Every answer through krm-foyer equals the API server's answer for the same token
      (the differential specs: allowed, forbidden, missing, conflicting, stale, invalid,
      unsupported media type, dry-run and Server-Side Apply)
- [x] Non-canonical paths (`//`, `..`, encoded slashes) are rejected, not normalized, and
      the path forwarded is byte-for-byte the path received
- [x] Browser-supplied `Authorization`, `Impersonate-*` and forwarding headers are
      stripped, and the session cookie never reaches Kubernetes (unit tests, and e2e with
      the audit log as witness)
- [x] Upstream responses: redirects are not followed or passed on, `Set-Cookie` and CORS
      headers are dropped, and every response is `Cache-Control: no-store`. See
      [upstream responses](design.md#upstream-responses)
- [x] [Interruptions](design.md#interruptions) as `Status` for code
- [x] Interruptions as pages for browser navigations, with the same status code; tests
      that a `fetch` cannot get the page form and that no Kubernetes answer is replaced
- [x] A redirect notice that shows the full target and continues only on a click, linking
      only to absolute web URLs
- [ ] A held-back page that shows the response as escaped text, truncated at a bound
- [x] A test proves that no request falls back to the service account: with the bait in
      place, a request without a session gets 401 and never reaches the API server, and
      the audit log names the user for every request with one
- [x] A test proves that no response, on any route, and no log line contains a token
      krm-foyer holds, a session ID or a client secret
- [ ] The same for tokens obtained by refresh, read from the session store (step 6)
- [x] Upstream bodies reach the browser decoded: the browser's `Accept-Encoding` is
      dropped, and a gzip answer from the API server arrives uncompressed without
      `Content-Encoding`
- [ ] The response-byte bound counts decoded bytes (a test with a small body that expands
      past the bound)
- [x] Exec, attach, port-forward and the service, node and pod proxy subresources return
      an explicit unsupported error
- [ ] Every [interruption](design.md#interruptions) row has a test, and the differential
      specs treat that table as the only exceptions

### Login and sessions

- [x] OIDC authorization code with PKCE, state and nonce, through a maintained library
      (unit tests against an issuer that misbehaves on request; e2e in step 2)
- [x] Opaque server-side sessions: rotated at login, with idle and absolute expiry, and
      ended with the ID token while there is no refresh (unit tests; e2e in step 2)
- [ ] Refresh is serialized per session and bounded (step 6)
- [ ] A refused refresh ends the session at once: 401s, and its streams close (step 6)
- [ ] The disablement bound measured for Dex in the fixture (remove a user, time the
      refused refresh), and documented per issuer configuration; elsewhere the documented
      bound is the absolute session expiry
- [x] CSRF proof and same-origin checks on every mutation through `/k8s`, with repeated
      header fields refused (unit tests and a fuzz property; e2e in step 2)
- [x] The same checks on logout
- [x] An unauthenticated API request gets a JSON 401, not a redirect (unit tests; e2e in
      step 2)
- [x] `/auth/session`
- [ ] `/auth/whoami` from a SelfSubjectReview
- [ ] `/_foyer/access`: the rules for a namespace from a SelfSubjectRulesReview, and a
      "can I?" form answered by a SelfSubjectAccessReview. See
      [what may I do](design.md#what-may-i-do)
- [ ] Shared session storage, so more than one replica works (step 6)
- [ ] The [session lifecycle](design.md#session-lifecycle) bounds, each with a test:
      logout seen by every replica at once, logout racing a refresh, the session store
      unavailable, and a stream open across logout and expiry
- [x] Session IDs never appear in krm-foyer's logs or error pages

### Proxy semantics

- [x] `Status` errors, content types, patch types, dry-run and Server-Side Apply pass
      through unchanged (the differential specs)
- [ ] Mutations are never replayed, including after the session is refreshed
- [x] Native watches and logs stream without buffering, and cancellation reaches the
      upstream
- [x] Every open response ends when its session ends: at logout and at expiry it is
      aborted, never ended cleanly, and cancelled at the API server (unit tests over
      every pair of protocols; e2e against the real cluster, with the audit log as
      witness, each checked by deploying a krm-foyer broken on purpose)
- [ ] [Bounds](bounds.md) on the request rate per session, concurrent requests per
      session and per replica, response duration and response bytes, each reached by a
      test (step 4)
- [ ] A response cut short by a bound is aborted, never ended cleanly, and cancelled at
      the API server (unit tests over HTTP/1.1 and HTTP/2, and e2e with the audit log as
      witness)

### Streams

- [ ] Host krm-stream, with RBAC deciding what a user may watch; which resources use a
      shared watch is configuration for efficiency, not access
- [ ] User-authenticated watches first, with the hello example following its notes live
      (step 5)
- [ ] Bounds on browser subscriptions and on upstream watches, counted separately
- [ ] Shared watches with per-subscriber SubjectAccessReview and bounded rechecks
- [ ] A stream ends when its session or its token expires, whichever comes first, and
      logout closes that session's streams
- [ ] A rehearsal with 200 identities, as a repeatable test, which also sets the
      per-replica defaults in [bounds](bounds.md)

### Seeing what happened

- [ ] One log line per refusal (policy denial, upstream 401, 403, 409 or 422) with
      subject, route and reason
- [ ] Metrics on every bound, requests in flight, why responses are cut short, and
      interruptions by reason, on a listener of their own (step 4; see
      [metrics](bounds.md#metrics))
- [ ] Metrics for requests and active sessions

### Making it easy for others

- [x] One task that brings up k3d, Dex and krm-foyer with a sample CRD, in minutes
      (`task demo`)
- [x] A minimal example frontend with no framework and no build step, which calls
      `/k8s/apis/...`. It lives in `examples/`, not in the binary
- [x] A small, framework-independent JavaScript helper: log in on a 401, show a 403 as a
      refusal, and treat a 409 as a conflict to reconcile (`/_foyer/foyer.js`)
- [ ] Deployment examples that show who krm-foyer suits without a scope: users granted a
      Role that matches the application, next to a note on why it is the wrong tool for
      users with broad grants such as cluster-admin. See
      [application scope](application-scope.md)
- [ ] A page for frontend developers: "the responses you will get and what they mean"
      (`generation` versus `resourceVersion`, 409 versus 403, what an empty list means)
- [ ] Voter's CoffeeConfig editor running on krm-foyer, replacing its own handlers
- [ ] A second consumer with a different API group: the reuse evidence the
      [release criteria](design.md#release-criteria) ask for
