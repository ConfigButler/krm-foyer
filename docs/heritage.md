# Heritage

Where krm-foyer comes from, why it exists as its own service, and the list of what we want
it to have eventually. The [service design](design.md) is the contract. This file explains
the reasons behind it and tracks progress; it binds nothing.

## Why this exists

krm-foyer is the foundation for a series of talks with one argument: **a browser application
can use the Kubernetes API as its backend.** Custom resources are the schema, RBAC does the
authorization, the audit log is the history, and a refusal the user sees is a real answer
from the API server.

Concretely, every request a user makes carries **their own OIDC token, and the API server
checks it.** That is also what lets krm-stream authorize a stream for that user. Logging
in to obtain the token is a convenience krm-foyer offers too. It is kept separate, so it
can later accept tokens obtained elsewhere or move into a program of its own.

The talks need a demo that proves this. The audience needs to be able to go home and do the
same thing without first writing an OIDC client, a session store, a proxy and a stream
gateway. krm-foyer is those four things, built once, kept small, and tested hard enough
that other people can build on it.

## Where it comes from

### Voter: the demo that proved the idea

[Voter](https://github.com/sunib/voter) is a conference demo built between March and
September 2026. Attendees scan a QR code, enrol through Room Pass, get their own Kubernetes
identity from Dex, and then vote in a quiz and edit a coffee menu from their phones. Every
action is an API call made with their own token, and every save becomes a Git commit through
[gitops-reverser](https://github.com/ConfigButler/gitops-reverser).

Voter settled several questions that krm-foyer's design now takes as given:

| Voter's finding | Where it lives in krm-foyer |
| --- | --- |
| The first design (March 2026) used an auth service with `Impersonate-User`. It was removed: impersonation lets the application write its own audit provenance | Requests go to Kubernetes with the user's own credential. There is no impersonation and no service-account fallback |
| Tokens stay server-side. JavaScript receives identity and a CSRF token from `/auth/session`, never a bearer token | [Login and sessions](design.md#login-and-sessions) |
| `/auth/whoami` (SelfSubjectReview) is the first thing to check when Kubernetes answers 403 | [/auth/whoami](frontend.md#what-ships-in-the-binary) |
| One shared watch can serve two hundred browsers, if each subscriber is re-checked with a SubjectAccessReview | [Streams and editing](design.md#streams-and-editing) |
| A 30-second SubjectAccessReview recheck covers RBAC for the captured subject. It does not cover identity-provider changes, and must not be described as revocation | Bounded reauthorization, stated honestly |

Voter also showed what a generic service would remove. Its browser never calls
`/apis/...` directly. Each feature has its own `/public/*` handler, and the streams
are limited by a Go variable that lists five resources plus namespace and name pins from
environment variables. Every application built this way writes that code again. krm-foyer
replaces it with configuration: an allowlist and the native API paths.

The service was designed inside Voter under the working name **k8s-front**.
Voter's adoption review recommended building it as a separate product with its own tests,
and warned that no second consuming application exists yet. That warning is why "two
frontends, two API groups, no application-specific handlers" is the first
[release criterion](design.md#release-criteria).

### What broke on stage

On 2026-09-17, Voter ran in front of about two hundred people. 67 people signed in and 38
ballots were recorded. The [post-mortem](https://github.com/sunib/voter/blob/main/docs/post-demo-2026-09-17.md)
found three defects, and each one shaped a rule here:

1. **The vote guard compared `resourceVersion` when it should have compared `generation`.**
   A controller writing `status` moved `resourceVersion` about once a second, so anyone who
   read the questions carefully was refused. krm-foyer's rule: pass Kubernetes semantics
   through exactly, and never add a guard of our own on top of them.
2. **A 409 conflict reached people as the API server's raw text.** Some read "the object has
   been modified; please apply your changes to the latest version" as "the CRD is not the
   newest version". The proxy is right to pass the `Status` through unchanged. What was
   missing is a frontend helper and documentation that tell a 409 (retry) apart from a 403
   (refusal).
3. **Nothing recorded the refusals.** The failures had to be reconstructed from
   `creationTimestamp`s four days later. krm-foyer logs every refusal, whether from policy or
   from upstream, with the subject and the reason.

The post-mortem's closing line is this project's working rule too: *being built on the API
means inheriting its semantics exactly, not approximately.*

### krm-stream: the library next door

[krm-stream](https://github.com/ConfigButler/krm-stream) owns the watch-to-browser protocol,
shared watches, projections, drafts and reconciliation. Voter was its first real consumer, at
0.4.0. krm-foyer hosts krm-stream; it does not reimplement any of it. Problems found while
building on it are reported to krm-stream, the way Voter did in its consumer feedback notes.

### gitops-reverser: how we build it

[gitops-reverser](https://github.com/ConfigButler/gitops-reverser) is the mature project, and
krm-foyer follows its engineering conventions rather than inventing new ones: a devcontainer
that holds every tool at pinned versions, Task as the only entry point, CI that runs the same
tasks, e2e against a real k3d cluster, actions pinned by SHA, and releases cut by
release-please.

krm-foyer does one thing differently on purpose: a single `task verify` gate instead of a
validation sequence written down in `AGENTS.md`. A small project can afford a single gate.

## Staying small

The project stays useful by staying small. These stay out:

- Application-specific endpoints, DTOs or aggregation. Domain logic belongs in operators and
  admission. See the [decision guide](bff-choice.md).
- A single-page application or resource browser. See the [frontend decision](frontend.md).
- Impersonation, and any fallback to krm-foyer's own service account for a user's request.
- Exec, attach and port-forward, until they have their own design and tests.
- A reimplementation of anything krm-stream already provides.

A feature that needs any of these belongs in the application or in krm-stream.

## Checklist

What we want to have eventually, grouped by area. Checked items exist today. An item is
done when its test exists, not when its code does. Security items need tests that try to
get past the boundary.

### Engineering (following gitops-reverser)

- [x] `task verify` as the single gate, run the same way by CI
- [x] `go test -race` on every change
- [x] Image smoke test on a private Docker network
- [x] Actions pinned by SHA and tool versions in the devcontainer's ENV block
- [x] CI runs every check inside the devcontainer's `ci` stage, as gitops-reverser does,
      so nothing is installed on the runner
- [x] release-please with conventional commits, and build provenance for the image
- [x] **e2e fixture on k3d**, as in gitops-reverser: pinned k3s, Dex as the issuer, the API
      server trusting it through an `AuthenticationConfiguration`, and an audit log as
      witness. A Ginkgo suite under `test/e2e/` drives it. See [testing](testing.md)
- [x] The e2e job green in CI
- [ ] krm-foyer deployed into the e2e fixture (image imported with `k3d image import`),
      with the pending `foyer` specs made real
- [ ] **Browser e2e** with Playwright: log in, read, edit, get refused with 403, hit a 409,
      log out. This is Voter's stage choreography as a test
- [ ] Coverage baseline that ratchets upward (gitops-reverser's `.coverage-baseline` and
      `cover-check`)
- [ ] Fuzz tests for path checking and policy matching, with a short fuzz run in CI
- [ ] Helm chart with `values.schema.json`, `helm lint`, `helm template` tests, and e2e that
      installs through the chart
- [ ] Signed multi-arch image (cosign keyless) with an SBOM
- [x] PR title check for conventional commits (squash merges take the PR title)
- [ ] Parse the whole squash message the way release-please does, as gitops-reverser's
      "Squash message parses" check does, once a dropped changelog entry makes it worth it
- [ ] Docs lint: markdownlint and link checking

### Deployment

- [ ] krm-foyer serves TLS from a mounted certificate and reloads it on rotation
- [ ] Plain HTTP behind an ingress, with the public URL from configuration and no trust in
      `Host` or `X-Forwarded-*`. One e2e spec runs nginx in front. See [ingress](ingress.md)
- [ ] Helm chart values for both models
- [ ] Routing recipes for one shared domain: a Gateway API `HTTPRoute`, an nginx server
      block and a Vite dev-server proxy. krm-foyer owns `/auth/`, `/k8s/`, `/stream` and
      `/_foyer/`; the application and any domain backend get the rest
- [ ] Login gate: `GET /auth/check` for an ingress gating the application's pages, with
      nginx and Traefik recipes, and `requireSession()` in the helper where there is no
      ingress support
- [ ] Later, when a hybrid application asks: identity headers from the check for a domain
      backend, never the token

### Access boundaries

- [ ] An empty policy exposes no API route and no stream scope
- [ ] The allowlist covers group, version, resource, namespace, verb, subresource and
      non-resource URL
- [ ] Non-canonical paths (`//`, `..`, encoded slashes) are rejected, not normalized, and
      the path forwarded is byte-for-byte the path checked
- [ ] `watch=true` on a collection counts as watch, not list
- [ ] Selectors, `limit` and `continue` are part of policy evaluation
- [ ] Browser-supplied `Authorization`, `Impersonate-*` and forwarding headers are stripped,
      and the session cookie never reaches Kubernetes
- [ ] A test proves that no request falls back to the service account
- [ ] A test proves that no response, on any route, contains a token
- [ ] Exec, attach and port-forward return an explicit unsupported error

### Login and sessions

- [ ] OIDC authorization code with PKCE, state and nonce, through a maintained library
- [ ] Opaque server-side sessions: rotated at login, with idle and absolute expiry
- [ ] Refresh is serialized per session and bounded
- [ ] CSRF proof and same-origin checks on every mutation and on logout
- [ ] An unauthenticated API request gets a JSON 401, not a redirect
- [ ] `/auth/whoami` and `/auth/session`, plus a SelfSubjectRulesReview view: "what may I do?"
      is the best slide in the talk
- [ ] Shared session storage, so more than one replica works

### Proxy semantics

- [ ] `Status` errors, content types, patch types, dry-run and Server-Side Apply pass through
      unchanged
- [ ] Mutations are never replayed, including after the session is refreshed
- [ ] Native watches and logs stream without buffering, and cancellation reaches the upstream
- [ ] Bounds on page size, response bytes, request rate, watch duration and concurrent streams

### Streams

- [ ] Host krm-stream with scopes from configuration, replacing Voter's hardcoded scope
      policy
- [ ] User-authenticated watches first
- [ ] Shared watches with per-subscriber SubjectAccessReview and bounded rechecks
- [ ] A stream ends when its session or its token expires, whichever comes first, and
      logout closes that session's streams
- [ ] A rehearsal with 200 identities, as a repeatable test, not a one-off

### Seeing what happened

- [ ] One log line per refusal (policy denial, upstream 401, 403, 409 or 422) with subject,
      route and reason
- [ ] Metrics for requests, refusals, active sessions and active streams

### Making it easy for others

- [ ] One task that brings up k3d, Dex and krm-foyer with a sample CRD, in minutes
- [ ] A minimal example frontend with no framework and no build step, which calls
      `fetch('/k8s/apis/...')`. It lives in `examples/`, not in the binary
- [ ] A small, framework-independent JavaScript helper: log in on a 401, show a 403 as a
      refusal, and treat a 409 as a conflict to reconcile, with our own wording
- [ ] A page for frontend developers: "the responses you will get and what they mean"
      (`generation` versus `resourceVersion`, 409 versus 403, what an empty list means)
- [ ] Voter's CoffeeConfig editor running on krm-foyer, replacing its own handlers
- [ ] A second consumer with a different API group: the reuse evidence the release criteria
      ask for
