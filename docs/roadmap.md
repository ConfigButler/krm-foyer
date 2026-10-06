# Roadmap

What is left to build, and in what order. The [design](design.md) says what krm-foyer
must do; this file tracks how far along that is.

**The core of the security model is proved against a real cluster, and a person can use
it: `task demo` starts an example application in a browser.** Server-held tokens, access
decided by Kubernetes alone and no service-account fallback each have an e2e spec that
tries to get past them, against a real API server and Dex, with krm-foyer's service
account as cluster-admin bait. Each other requirement becomes a property of krm-foyer
when its test exists and passes, and not before. Request/response
[bounds](bounds.md) and session cancellation have tests. The hello example follows
notes live through krm-stream. A rehearsal holds 1800 streams of 200 identities on one
replica, both per-user and shared; sharing asks the API server about every user.

## Order of work

The next work should make the existing service usable and operable by someone outside
this repository. Each change ends with `task verify` green; a checked box requires
an implemented test, not just a design or a pending spec.

For the audience release, two PRs are specified in the
[encrypted cookie sessions and configurable login plan](investigations/audience-release-plan.md).
Both are done. The first: established sessions survive restarts in encrypted HttpOnly
cookies sealed with stable keys, with no session database, and sliding idle expiry is
gone. Logins in progress stay sealed with a key of the process's own. Logout clears the
browser cookie and ends what is open in the process; copied cookies remain usable until
their fixed expiry. The second: configured login parameters, session claims,
`/auth/whoami` and the fixed Kubernetes attribution mappings gitops-reverser consumes. QR generation and
provider-specific handoff belong to the application/provider integration; coffee's
backend work is separate. The broader backlog below remains subject to that decision;
its shared-session store proposal is superseded, and distributed coordination needs a
new design only when multiple replicas or stronger revocation guarantees are required.

1. **Finish the deployment path.** The Helm chart is being developed separately.
   Review it against the contract and install it through e2e. Give the pod no API
   grants in per-user mode; for sharing, use an explicitly configured, narrowly granted
   projected token. Include resource
   requests/limits, network policy, verified backend TLS, the OIDC client and cluster
   authentication setup, and protected metrics. The current fixture deliberately gives
   the pod cluster-admin bait and is not that deployment. Document certificate, CA and
   client-secret rotation, restart/sign-out behavior, and supported Kubernetes/issuer
   versions. Add certificate reload and tests for draining traffic during termination.
   Finish the targeted ingress tests using the existing Traefik fixture.
2. **Capacity and failure behavior beyond one tiny scope.** Measure large snapshots,
   many distinct scopes, staggered subscriptions, sustained writes, slow readers and
   reconnect bursts; record API-server load as well as foyer memory. Stream count does
   not bound snapshot/cache bytes. Decide and test memory/admission budgets, including
   active sessions and anonymous login traffic, before claiming production capacity.
   Exercise API-server and issuer outages, rotating shared credentials, and recovery.
   Preserve the current rehearsal as a regression test for watch consolidation.
3. **A second application and an integration guide.** Use another API group without new
   backend handlers, with a domain operator showing pending, accepted, rejected and
   failed outcomes. Document helper outcomes, conditional saves, unknown write results,
   stream errors and draft preservation at sign-out. State browser support: only
   Chromium has browser e2e today. Add `/_foyer/access` beside `/auth/whoami` to make
   identity and RBAC problems diagnosable. This establishes
   reuse and identifies which lifecycle features the application actually needs.
4. **Refresh, when sessions must outlive short ID tokens.** Serialize refresh per
   session, bound it, make logout win every race, never replay a mutation, and scan
   refreshed credentials for leaks. Measure disablement for the chosen issuer
   configuration. This can be delivered and tested on one replica before adding a
   distributed store; it need not wait for high availability.
5. **Coordination, before multiple replicas.** The encrypted cookies remove the need
   for a shared token store. Decide how callbacks reach a login in progress sealed with
   another process's key, keys rotate consistently, per-session bounds work across
   processes and responses are cancelled. Explicitly design any stronger logout
   or refresh guarantees; cookie clearing alone does not revoke copied sessions.
   Test those decisions before enabling rolling updates or claiming availability
   across replicas. A database is not a prerequisite for the audience release.
6. **Release and maintenance evidence.** Add vulnerability scanning to the CI image,
   exercise the published artifact as an adopter, and document upgrades, rollback and
   image/provenance verification. The release workflow already requests an SBOM and
   build provenance; multi-arch builds and cosign image signing remain separate work.
   Add docs/link checks early enough to prevent status drift; keep broader coverage
   targets secondary to tests of specific failure modes.

The login gate and identity for a domain backend (`/auth/check`) exist since Voter
asked for them, with a browser identity proved in the cluster's authentication
configuration as the first step of application scope; see
[implementer feedback](implementer-feedback.md). A scope list in krm-foyer,
external-login modes and upgrade protocols stay driven by actual adopter requirements.

## Completed milestones

These numbers identify earlier investigation notes, not the priority of future work.

| Milestone | What exists now |
| --- | --- |
| 1–2: proxy and login | OIDC with PKCE/state/nonce, sessions (first in memory, now sealed in the browser's cookie), CSRF, interruption pages and differential/audit tests against Dex and Kubernetes |
| 3: browser demo | `task demo`, the hello CRD editor, browser helper and Chromium journeys through Traefik |
| 4: bounds | Request rate/concurrency, response duration/bytes, session cancellation and metrics |
| 5: streams | krm-stream, live notes, conflict/draft recovery and a 200-identity rehearsal |
| 5b: shared watches | Opt-in shared resources, API-server subject/access reviews, decision reuse, bounded rechecks and watch/access metrics |

The [krm-stream investigations](investigations/krm-stream-feedback.md) retain the
version-specific findings behind milestones 5 and 5b. Their superseded workarounds
are historical, not instructions for the current integration.

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
- [x] The implemented `foyer` authentication/access specs: login, identity, differential answers,
      RBAC changes, watches, path and subresource refusals, CSRF, logout and the token
      scan, each checked by deploying a krm-foyer broken on purpose
- [x] Login gate and identity check through the real Traefik: a forged identity header,
      writes without CSRF proof and from another origin, signed-out page loads and scripts
- [ ] The remaining `foyer` specs: refusal of a refresh, and spoofed `Host` and
      `X-Forwarded-*` through the ingress
- [x] Browser e2e: log in, read, create, edit, get refused with 403, hit a 409, log out,
      and no credential within the page's reach. Chromium driven from the Go suite
      with chromedp, keeping browser e2e in Go. Node separately runs the helper unit
      tests; there is no application JavaScript build
- [x] `task demo`: the e2e fixture with the hello example behind a front door, for a
      browser on this machine
- [ ] Coverage baseline that ratchets upward
- [x] Fuzz tests for path checking, the upstream response check, the CSRF rule and
      return paths, with a short fuzz run of each in `task verify`
- [x] Helm chart with `values.schema.json`, `helm lint`, `helm template` tests, and e2e
      that installs through the chart
- [x] Publish the chart with each release: an OCI artifact beside the image, signed with
      cosign and described to Artifact Hub
- [x] Release workflow configured to generate an SBOM and attest build provenance
- [x] Image and chart signed with cosign (keyless); the release notes show how to verify
      them, and the SBOM is attached to the release with its signature and provenance
- [ ] Multi-arch image
- [ ] Dependency/image vulnerability scanning inside the CI image
- [ ] Docs lint: markdownlint and link checking
- [ ] Parse the whole squash message the way release-please does, once a dropped
      changelog entry makes it worth it

### Later, when an adopter needs it

- [ ] Proxy subresources, with CSRF proof required on every request through them,
      whatever the method; a test that a cross-site `GET` navigation is refused before it
      reaches the backend

- [x] [Application scope](application-scope.md), first step: a browser identity in the
      cluster's authentication config, documented and proved by one e2e spec
- [ ] Application scope, then: a scope list in krm-foyer for clusters where the
      authentication config cannot change

### Deployment

- [x] krm-foyer serves TLS from a mounted certificate (the e2e deployment does)
- [ ] It reloads the certificate on rotation
- [ ] Behind an ingress: the public URL from configuration and no trust in `Host` or
      `X-Forwarded-*`, with targeted e2e specs through the Traefik fixture. See [ingress](ingress.md)
- [x] Browser fixture behind Traefik with re-encryption and a BackendTLSPolicy for each
      backend; targeted spoofing and buffering checks remain above
- [ ] A NetworkPolicy in the chart that admits only the ingress to krm-foyer's port, and
      only the monitoring system to the metrics port
- [ ] Helm chart values for both models
- [ ] Rolling updates that refuse no connection: krm-foyer stops listening as soon as
      it is told to stop, while its Service may still route to it for a moment, so a
      rollout refuses connections briefly (seen by the rehearsal, which restarts it). A
      wait before shutdown, or readiness turned off first, with a test
- [x] Routing recipes for one shared domain: a Gateway API `HTTPRoute`, a Traefik
      `IngressRoute` and an nginx server block (all run by the e2e fixture, nginx's
      copied from the document), and a Vite dev-server proxy (written, not run). See
      [ingress](ingress.md)
- [x] `requireSession()` in the browser helper for navigation to login
- [x] Login gate: `GET /auth/check` for an ingress gating the application's pages, with
      nginx and Traefik recipes
- [x] Identity from the check for a domain backend, never the token
      (`?identity=true`, `Krm-Foyer-Identity`), asked for by Voter
- [ ] Room Pass's QR login run end to end through krm-foyer: a
      [recipe](room-pass.md) exists; the fixture needs Room Pass's own Dex and a browser spec

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
- [x] A test scans responses and logs for tokens krm-foyer holds and client secrets;
      session IDs are allowed only in the session cookie's issuing `Set-Cookie` header
- [x] The same scan opens nothing up for the session cookie: its value, as set and as
      decoded, holds no token or secret (unit tests; e2e)
- [ ] The same for tokens obtained by refresh
- [x] Upstream bodies reach the browser decoded: the browser's `Accept-Encoding` is
      dropped, and a gzip answer from the API server arrives uncompressed without
      `Content-Encoding`
- [x] The response-byte bound counts decoded bytes (a test with a small body that expands
      past the bound)
- [x] Exec, attach, port-forward and the service, node and pod proxy subresources return
      an explicit unsupported error
- [ ] Every [interruption](design.md#interruptions) row has a test, and the differential
      specs treat that table as the only exceptions

### Login and sessions

- [x] OIDC authorization code with PKCE, state and nonce, through a maintained library
      (unit tests against an issuer that misbehaves on request; e2e against Dex)
- [x] Sessions sealed (AES-256-GCM) into the cookie with stable, rotatable keys: a new
      session at every login, a fixed absolute expiry, ended with the ID token while
      there is no refresh; every bit authenticated (unit tests; e2e against Dex)
- [x] A session survives an orderly restart and a forced replacement of the pod, with
      the same CSRF token and end; a login in progress does not, and a fresh one works
      (unit tests; e2e)
- [x] Logout clears the cookie and ends the session's open responses in the process; a
      copied cookie stays a session until its expiry, recorded as the chosen limit
      (unit tests; e2e)
- [x] A token too large for the cookie is refused at login, with no cookie set
- [ ] Refresh is serialized per session and bounded
- [ ] A refused refresh ends the session at once: 401s, and its streams close
- [ ] The disablement bound measured for Dex in the fixture (remove a user, time the
      refused refresh), and documented per issuer configuration; elsewhere the documented
      bound is the absolute session expiry
- [x] CSRF proof and same-origin checks on every mutation through `/k8s`, with repeated
      header fields refused (unit tests and a fuzz property; e2e against Dex)
- [x] The same checks on logout
- [x] An unauthenticated API request gets a JSON 401, not a redirect (unit tests; e2e against Dex)
- [x] `/auth/session`
- [x] `/auth/whoami` from a SelfSubjectReview, sharing the shared watches' lookup
      (unit tests; e2e against the API server, compared with a direct review)
- [x] Login parameters: configured names, defaults and allowed values reach the issuer
      from `oidc.*`; anything else, krm-foyer's own parameters included, is refused
      before a login starts (unit tests under two issuers' names; e2e against Dex)
- [x] Session claims: display name, groups and connector from the verified token, in a
      fixed shape (unit tests; e2e against Dex)
- [x] The attribution extras gitops-reverser reads, mapped by the API server from signed
      claims, in `/auth/whoami`, the audit event and an admission request, with spoofed
      headers changing nothing (e2e)
- [ ] `/_foyer/access`: the rules for a namespace from a SelfSubjectRulesReview, and a
      "can I?" form answered by a SelfSubjectAccessReview. See
      [what may I do](design.md#what-may-i-do)
- [ ] More than one replica: logins in progress, per-session bounds and logout's reach
      across processes ([order of work](#order-of-work), item 5). Sessions themselves
      need no shared storage: they are in their cookies
- [ ] The remaining [session lifecycle](design.md#session-lifecycle) bounds, each with
      a test: logout racing a refresh, and a refused refresh
- [x] A native watch open across logout and expiry is aborted and cancelled at the API
      server within the session-check interval (one replica; e2e against the real
      cluster)
- [x] Session IDs never appear in krm-foyer's logs or error pages

### Proxy semantics

- [x] `Status` errors, content types, patch types, dry-run and Server-Side Apply pass
      through unchanged (the differential specs)
- [x] A dropped create response is not replayed (`TestMutationsAreNotReplayed`)
- [ ] The same guarantee across refresh and logout races
- [x] Native watches and logs stream without buffering, and cancellation reaches the
      upstream
- [x] Every open response ends when its session ends: at logout and at expiry it is
      aborted, never ended cleanly, and cancelled at the API server (unit tests over
      every pair of protocols; e2e against the real cluster, with the audit log as
      witness, each checked by deploying a krm-foyer broken on purpose)
- [x] [Bounds](bounds.md) on the request rate per session, concurrent requests per
      session and per replica, response duration and response bytes, each reached by a
      test
- [x] A response cut short by a bound is aborted, never ended cleanly, and cancelled at
      the API server (unit tests over HTTP/1.1 and HTTP/2, and e2e with the audit log as
      witness)

### Streams

- [x] Host krm-stream, with RBAC deciding what a user may watch, and no list of
      resources in krm-foyer (unit tests; e2e with the audit log as witness, checked by
      deploying a krm-foyer that opens watches as its service account). Which resources
      use a shared watch is configuration for efficiency, not access
- [x] User-authenticated watches first, with the hello example following its notes live:
      a change made elsewhere appears without a reload, a change to a note being edited
      is a conflict the page shows, and a 409 from a change the stream does not show
      saves nothing until asked again (browser specs, each checked against a broken
      example)
- [x] Bounds on browser subscriptions, with upstream watches counted separately:
      streams per session and per replica, apart from requests, and the watches they
      hold at the API server counted on their own (unit tests and e2e, each checked
      against a build broken on purpose). Upstream watches have no separate bound:
      there are no more watches than streams ([why](bounds.md#streams))
- [x] Recovery after a disconnect: a watch the API server ends, or ends with 410 Gone,
      is opened again on the same stream with a fresh snapshot (unit tests); a dropped
      connection between browser and krm-foyer is recovered by krm-stream's client, with
      what changed meanwhile and the user's draft kept (browser spec, which a page that
      does not retry failed)
- [x] Shared watches with per-subscriber SubjectAccessReview and bounded rechecks: one
      watch for every user's streams of a scope, opened as the shared-watch identity and
      never the bait (e2e, the audit log as witness); a user RBAC refuses gets nothing
      from it; a revoked grant ends that user's stream at the next recheck and nobody
      else's; logout leaves the watch to the others; tokens never mix; a failed review
      is never an allow (unit tests, each checked against a build broken on purpose, and
      e2e). Configured per resource; see [watches](watches.md)
- [x] A stream ends when its session or its token expires, whichever comes first, and
      logout closes that session's streams: aborted, and its watch cancelled at the API
      server within the session-check interval (one replica; unit tests, and e2e with
      the audit log as witness, checked by deploying a krm-foyer whose streams the gate
      could not cancel)
- [x] A rehearsal with 200 identities, as a repeatable test, which also sets the
      per-replica defaults in [bounds](bounds.md): 1800 streams on one replica, part of
      the e2e suite, with what it measured in [bounds](bounds.md#measured-the-rehearsal).
      Run twice, per-user and shared, the shared run also measuring the access checks.
      Native watches through `/k8s` are not measured, by decision ([watches](watches.md))

### Seeing what happened

- [x] One log line per refusal (krm-foyer's own, the API server's 401, 403, 409 or 422,
      and a stream's refusals) with user, route and reason, and who refused (unit
      tests, each checked against a build broken on purpose)
- [x] Metrics on every bound, requests in flight, why responses are cut short, and
      interruptions by reason, on a listener of their own (see
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
