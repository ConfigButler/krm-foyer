# Feedback for krm-foyer

Notes for krm-foyer's maintainers. They cover what Voter would still need before it
can delete its own login, session, proxy and stream code and run on krm-foyer instead.
It uses the same format as [krm-stream-feedback.md](krm-stream-feedback.md) and
[gitops-reverser-feedback.md](gitops-reverser-feedback.md): each entry says what Voter
needs, what krm-foyer does today, and whether we think krm-foyer should change. That
includes entries where we concluded it should not.

Against **krm-foyer 0.2.0** (`cfd290d`) and Voter at `51dbb1f`, 2026-10-05.
[k8s-front-adoption.md](k8s-front-adoption.md) is the older recommendation. It still
holds for the domain questions it raises; this page is about the transport.

---

## Summary

0.2.0 closed most of what Voter asked for. Sessions are kept in encrypted cookies.
Login parameters are configurable. `/auth/whoami` exists, the attribution extras are
in place, shared watches are guarded by SubjectAccessReviews, and the chart can be
installed. About 2,000 of Voter's 4,950 lines of Go are plumbing that krm-foyer
replaces as it stands.

What is still missing is not in the proxy itself. It is how krm-foyer meets the two
things Voter keeps:

1. **A domain backend on the same origin that needs to know who is calling.**
   Voting and the coffee storefront have rules that RBAC cannot express. Today they
   read the user from Voter's own session. Under krm-foyer they get nothing.
   ([entry 1](#1-identity-for-a-domain-backend-on-the-same-origin))
2. **The Room Pass QR login.** Nobody has run it end to end through krm-foyer. Running
   it is the release evidence the audience-release plan itself asks for.
   ([entry 2](#2-prove-the-room-pass-qr-login-through-krm-foyer))

Below those come two smaller asks: routing recipes for one shared host
([entry 3](#3-routing-recipes-for-one-host-traefik-and-vite)), and an application
scope for the operator, who signs in as cluster-admin
([entry 4](#4-the-operator-signs-in-as-cluster-admin)).
[What Voter does not need](#what-voter-does-not-need) lists roadmap items that can wait
as far as Voter is concerned.

---

## krm-foyer's answer (2026-10-05)

| Entry | What changed |
| --- | --- |
| [1. Identity for a domain backend](#1-identity-for-a-domain-backend-on-the-same-origin) | `/auth/check` exists. It is a 204 or the refusal `/k8s` would give, judged on the request the ingress forwards, so a write to the backend needs the session's CSRF proof from krm-foyer's origin. With `?identity=true`, the 204 carries `Krm-Foyer-Identity`: the API server's `userInfo` with the session's `displayName` and `connector`, in one base64url JSON header, never the token. A SelfSubjectReview is reused per token for 30 seconds, within the session's request rate. The e2e suite forges the header through the real Traefik and checks the backend never sees it. See [the check](ingress.md#the-check) |
| [2. Room Pass's QR login](#2-prove-the-room-pass-qr-login-through-krm-foyer) | [A recipe](room-pass.md) from Room Pass 2.0.0's own files: the join endpoint, krm-foyer's values (`federated:id` and `groups` scopes, `connector_id`, `sessionClaims.connector`), the shared host's routes and the release checks. Not yet run end to end. That needs Room Pass's own Dex in the fixture, since the fixture's Dex cannot safely take an `authproxy` connector, and a browser spec. It is its own pull request |
| [3. Routing recipes](#3-routing-recipes-for-one-host-traefik-and-vite) | A Traefik `IngressRoute` with ForwardAuth middlewares, run by the e2e fixture beside its Gateway API routes; an nginx `auth_request` block; a Vite dev-server proxy, in which Vite takes `task demo`'s public URL so cookies and CSRF work unchanged. The Vite recipe is not run by the suite. A redirect from the check is now absolute, because Traefik resolves a relative one against the check's address |
| [4. The operator as cluster-admin](#4-the-operator-signs-in-as-cluster-admin) | The browser identity recipe: one username expression gives krm-foyer's tokens (by `azp`, or the audience) a different name from the command line's, so `cluster-admin` stays on the command-line name. The fixture now does this, and a spec proves krm-foyer is refused what the command-line name was granted |

## What 0.2.0 already covers

| Voter today | krm-foyer 0.2.0 | Voter code that goes away |
| --- | --- | --- |
| OIDC with PKCE, state, nonce and a browser-binding cookie; logins in progress kept in memory | `/auth/login`, `/auth/callback`, logins in progress sealed in cookies | `voter/oidc.go:1-400` |
| `connector=` restricted by `OIDC_CONNECTOR_CHOICES` | `login.authorizationParameters.connector_id` with `allowedValues`, sent as `oidc.connector_id=` | `connectorFor` in `oidc.go` |
| Session sealed in `__Host-voter-session` with a 3,800-byte budget, ended by the earlier of the max age and the ID token's expiry | Same design, the same budget, rotatable keys | `participant_session.go`, `session_cookie.go` |
| `/auth/session` with `displayName`, `groups`, `connector`, CSRF | `sessionClaims.connector: /federated_claims/connector_id` | Most of `oidc_handlers.go` |
| `/auth/whoami` (SelfSubjectReview, rendered as YAML) | `/auth/whoami` (JSON) | `oidc_handlers.go:93-123` |
| Kubernetes clients built per request with the user's ID token, no impersonation | `/k8s` with the user's token, nothing to fall back to | `participant_kube.go`, `http_handlers.go:18-48` |
| `/public/stream`: shared watches as the ServiceAccount, a SubjectAccessReview per subscriber, rechecked every 30 s | `/stream/v1` with `sharedWatches`, an identity of its own and bounded rechecks | `stream_runtime.go`, `participant_stream.go` |
| `refusalRecorder`: one log line per 4xx | One log line per refusal, with the subject and who refused | `oidc_handlers.go:191-238` |
| Stream metrics on a separate listener | `-metrics-listen` | `streamMetrics` |
| Thin handlers: get and patch a CoffeeConfig, list, create and patch Databases, set a round's state | Native `/k8s` paths | Most of `participant_coffee.go`, `participant_databases.go` and `participant_save.go` |
| `/auth/rules` (a SelfSubjectRulesReview, flattened) | A POST of a SelfSubjectRulesReview through `/k8s`, which the design recommends to code | `participant_authz.go` |
| The operator page creates and deletes the `voter-audience-coffee-admin` RoleBinding with the operator's token | The same request through `/k8s` | `operator_audience_grant.go` |

`k8s.koudijs.dev/2-gitops/krm-foyer` already runs 0.2.0 with the hello example on
`foyer.k8s.koudijs.dev`. Two things that deployment worked out apply to Voter too:

- The `audience:server:client_id:kubernetes` cross-client scope, which makes Dex issue
  a token the API server accepts.
- The NetworkPolicy values.

The hello example runs on its own host, though. Voter has to share one host with
Room Pass, and none of the open entries below has been tried there.

---

## 1. Identity for a domain backend on the same origin

**done · `/auth/check?identity=true`, see [the check](ingress.md#the-check)**

Some of Voter is domain logic, and it stays in a backend:

- **Votes** (`participant_quiz.go:193-295`). The round must be `live`. Only the
  `room-pass` connector may vote. The round's `generation` must match. Answers are
  validated against the questions. The submission name is chosen by the server, so a
  second vote is a 409.
- **The coffee storefront and orders** (`participant_storefront.go`,
  `coffee_orders.go`). Prices and vouchers are computed on the server. Orders are not
  Kubernetes objects at all.

Each of these handlers needs to know who is calling. Today they read it from Voter's own
session cookie. With krm-foyer the cookie is krm-foyer's, sealed with keys only
krm-foyer has, so a backend behind the same host learns nothing. The roadmap has this
under *"Later, when a hybrid application asks: identity headers from the check for a
domain backend, never the token"*. **Voter is that hybrid application, and it is asking.**

What we would need:

- `/auth/check` usable as a Traefik `ForwardAuth` target. On success it answers `204`
  with identity headers: the **Kubernetes** username, UID, groups and extras from the
  SelfSubjectReview, plus `connector` and `displayName` from the session claims. For a
  `fetch` the answer is a `401` in the shape `/k8s` already uses. Never the token.
- Inbound copies of those headers stripped, with a test that a spoofed header changes
  nothing. That is the same test `/k8s` already has for `Impersonate-*`.
- A bounded cost per check. A SelfSubjectReview on every request to a vote endpoint is
  fine at Voter's scale, and the shared-watch lookup already reuses decisions.

The alternative is Voter changing its domain instead: votes go straight to `/k8s`, and
admission enforces the rules ([bff-choice.md](https://github.com/ConfigButler/krm-foyer/blob/main/docs/bff-choice.md#example-a-quiz-with-one-submission-per-participant),
options A to C). We have not chosen yet. A ValidatingAdmissionPolicy cannot read the
round to check that it is `live`, so the quiz would need a webhook. The storefront has
no such way out, because orders are not resources. The audience-release plan says
*"Coffee's backend authentication and authorization will be solved in its own
application"*. In practice that application is Voter's coffee storefront, so it needs
this entry too.

## 2. Prove the Room Pass QR login through krm-foyer

**recipe written · [room-pass.md](room-pass.md); the end-to-end run is still open**

The audience-release plan says: *"Before switching voter to foyer, prove one real
QR-to-login journey there."* That has not happened. krm-foyer's fixture signs in with
Dex static users, and the deployed instance allows only `connector_id: github`.

To be clear about what we are **not** asking for: the join code should not go through
krm-foyer. Room Pass builds the join URL after Dex, so `oidc.*` has nothing to carry
it in. Room Pass's own contract
([qr-join.md](https://github.com/sunib/room-pass/blob/main/docs/qr-join.md)) is a
`Secure`, `HttpOnly`, `SameSite=Lax` cookie `__Host-room-pass-joincode`, set by the
application on the join host. Voter keeps that endpoint, about 85 lines
(`voter/oidc.go:401-484`). It sets the cookie and then redirects to
`/auth/login?return_to=…&oidc.connector_id=room-pass`. The audience-release plan
assigns it to the integration in the same way.

What we would like:

- Have the e2e fixture, or a documented recipe, run this flow against Room Pass's
  [reference deployment](https://github.com/sunib/room-pass/tree/main/deploy) (Room
  Pass, Dex `authproxy`, an edge), which Room Pass's CI now runs: QR → app endpoint
  sets the cookie → krm-foyer login → Room Pass `/join` → Dex → `/auth/callback` →
  `/auth/session` showing `connector: room-pass`.
- Then check the four other release items from the plan against that login: display
  metadata, a write attributed through both extras, a pod replacement that keeps the
  session, and logout.

The breakage we expect is the host split. The cookie arrives only when the app, Room
Pass's `/join` and krm-foyer share one host. If they don't, the flow quietly falls back
to typing the code. A browser test is the only thing that catches it. That leads to
entry 3.

## 3. Routing recipes for one host: Traefik and Vite

**done · Traefik run by the e2e fixture, Vite written: see [recipes](ingress.md#recipes) and [the dev-server proxy](ingress.md#during-development-a-dev-server-proxy)**

Voter would serve four things from one host:

- krm-foyer: `/auth/*`, `/k8s/*`, `/stream/v1`, `/_foyer/*`
- Voter's static SPA: `/`
- Voter's domain backend: `/public/*`
- Room Pass: `/join`, `/bind`, …

The roadmap names Gateway API, nginx and Vite. This cluster routes with **Traefik
`IngressRoute`** (`voter-demo/ingress.yaml`). A Traefik recipe would match it directly.
It would also be where entry 1's `ForwardAuth` middleware and krm-foyer's own `/`
start page get sorted out, because `/` has to belong to the application.

The **Vite dev-server proxy** matters more than it looks. Voter's local loop is Vite
plus a mock backend (`frontend/dev/kube-mock-plugin.ts`). Under krm-foyer that mock
either goes or becomes a mock of krm-foyer. A recipe for pointing Vite at `task demo`'s
krm-foyer would settle which one. Cookies and CSRF through a dev proxy are where we
expect to lose an afternoon.

## 4. The operator signs in as cluster-admin

**done · [a browser identity](application-scope.md#a-browser-identity-in-kubernetes), proved by one e2e spec**

The operator logs in through GitHub as `github:simonkoudijs@gmail.com`, and that user
is bound to `cluster-admin` (`auth/rbac/humans-rbac.yaml`). Today Voter's backend uses
that token in a few specific handlers. Under krm-foyer, any script on Voter's origin
holds cluster-admin for as long as the operator is signed in. That is the case the
application-scope page lists: *"An administrator signing in to a small application"*.

Voter can work around this without krm-foyer: give the operator a second, narrow
identity just for the demo. The roadmap's first step under application scope is
*"document a browser identity in the cluster's authentication config, proved by one
e2e spec"*. A Dex recipe for that would answer it: the same person, a different
connector or claim, a different username, a narrow Role. We are not asking for
krm-foyer's scope list.

---

## What Voter does not need

So these can be ranked lower against Voter's use:

- **Refresh.** Room Pass's `authproxy` connector issues no refresh tokens, and a
  session that ends with its ID token is what Voter does today.
- **More than one replica.** Voter runs one replica with `Recreate`.
- **The `/_foyer/access` page.** Voter's identity screen can use the native reviews
  through `/k8s`, as the design suggests.
- **Proxy subresources, upgrade protocols, and the native-watch connector for
  krm-stream.**
- **Certificate reload.** It is nice to have. A rollout that briefly refuses
  connections is something krm-stream's reconnect already handles on stage.

## What stays Voter's work

None of these is an ask. They are listed so nobody expects them from krm-foyer:

- Move the browser client from krm-stream 0.4.0 to 0.7.0, and `/public/stream` to
  `/stream/v1`.
- Change login links (`return=` → `return_to=`, `connector=` → `oidc.connector_id=`).
  The hard-coded ones are in `AdminScreen.vue`, `DatabaseEditScreen.vue`,
  `RoomScreen.vue` and `OrderScreen.vue`. The QR code (`RoomScreen.vue:118`) points
  at `/auth/login?code=` today. Once krm-foyer owns `/auth/login`, it has to point at
  Voter's own QR endpoint instead.
- Move application settings out of `/auth/session` into a static `config.json`:
  `namespace`, `coffeeConfigName`, `roomName`, `commitURLTemplate`. Compute `canVote`
  from `connector` in the browser for display only; the domain backend enforces it.
- **Review grants before exposing `/k8s`.** Participants hold `create` and `list` on
  `quizsubmissions`. Today only the vote handler uses `create`, and only the aggregate
  results come back. Through `/k8s` a participant could create a submission without
  the handler's rules, and list everyone's answers with the submitter's name in a
  label. Those grants need narrowing, or admission, before the switch. Results already
  live in `QuizSession.status` ([live-results-design.md](live-results-design.md)), so
  `list` is no longer needed for them.
- **Keep a CoffeeConfig save to spec only.** The handler refuses anything outside
  `spec`. Through `/k8s`, the editor's `patch` grant would also allow labels and
  annotations, which gitops-reverser would commit. That needs a
  ValidatingAdmissionPolicy.
- **Keep a CoffeeConfig save together with its CommitRequest.** Today that is two
  writes on the server with partial success reported. It becomes two writes from the
  browser, with the same partial-success handling.
- **Give krm-foyer a client and audience in Dex on Voter's host.** Follow the
  cross-client-scope pattern `foyer.k8s.koudijs.dev` already uses.

## Something we could give back

`frontend/src/api/liveEditableResource.ts` (329 lines) is Voter's generic edit loop over
krm-stream. It covers keeping a draft, showing a conflict, a guarded save and retry. The
roadmap's integration guide (*"helper outcomes, conditional saves, unknown write
results, … draft preservation at sign-out"*) describes the same ground. If it would help,
we can extract it as the example rather than have the guide written from scratch.

## Suggested order

1. **Operator-only pilot, now.** It needs entry 3, and entry 4 decided. With the GitHub
   login, Voter's admin and CoffeeConfig editor run through `/k8s` and `/stream/v1`.
   This is the roadmap's own *"Voter's CoffeeConfig editor running on krm-foyer"*.
2. **The audience login.** It needs entry 2. Participant login moves as one piece,
   since there can only be one session cookie on the origin.
3. **Votes and the storefront.** They need entry 1, or a decision in Voter to move
   votes to admission. After this step Voter keeps its domain handlers, the quiz
   reconciler, the QR endpoint and static hosting, and nothing else.
