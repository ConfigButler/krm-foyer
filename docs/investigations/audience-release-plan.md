# Release plan: encrypted cookie sessions and configurable login

**Planned 2026-10-05. Both PRs are implemented.** Where the implementation differs
from this plan, [PR 1 as built](#pr-1-as-built) and [PR 2 as built](#pr-2-as-built) say
so; [service design](../design.md) is the contract.

The next two PRs make foyer usable for a live audience: deployments preserve
sessions, applications can carry configured login options in their own links and
QR codes, and Kubernetes receives the identity needed for Git attribution.
Coffee's backend authentication and authorization will be solved in its own
application. There is no third PR for forwarding identity to coffee in this plan.

[Service design](../design.md) remains the contract for implemented behavior.
Each implementation PR updates that contract, its configuration documentation and
the [roadmap](../roadmap.md) together with its tests.

| Order | Proposed PR title | Result |
| --- | --- | --- |
| 1 | `feat!: keep sessions in encrypted cookies across restarts` | Established sessions survive replacement of the foyer process without a session database |
| 2 | `feat: configure login parameters and expose Kubernetes identity` | Applications choose approved login options; session metadata, `/auth/whoami` and Git attribution have explicit contracts |

## Ownership

Foyer implements OIDC, keeps credentials inside an authenticated, encrypted
HttpOnly cookie and calls Kubernetes with the user's own token. Only foyer holds
the encryption keys; page scripts receive identity metadata and CSRF proof, never
the plaintext token. It does not need to recognize the OIDC product, understand
a room code or decide which connector makes someone a voter.

| Concern | Owner |
| --- | --- |
| Login links and QR generation | Application |
| Allowed extra authorization parameters, defaults and requested scopes | Deployment configuration |
| Meaning and validity of a login hint, room code or connector selection | Identity provider and its integration |
| PKCE, state, nonce, callback binding, local return paths and session lifecycle | Foyer |
| Authenticated username, groups and attribution extras | Kubernetes authentication configuration |
| Access to Kubernetes objects | Kubernetes authorization and admission |
| Coffee endpoints, orders and their authentication | Coffee application |

Foyer will have no Room Pass cookie name, room-code alphabet, normalization,
expiry rules or malformed-room-code tests. Tests cover the generic forwarding
boundary, including parameter ownership and encoding. They do not judge whether
an opaque provider input is a valid room code.

## PR 1: encrypted cookie sessions

### Scope

Use the encrypted-cookie approach already used by voter. Replace the in-memory
token store with a versioned cookie containing the ID token, CSRF proof, a random
session identifier, issuance time and fixed expiry. Include only the identity
metadata needed by the session endpoint; account for the extra fields in PR 2.
Use a maintained authenticated-encryption implementation; voter's cookie codec is
the starting reference. No SQLite, persistent volume, token journal or durable
revocation store is part of this PR.

Keep the cookie name `__Host-krm-foyer-session` and its `Secure`, `HttpOnly`,
`Path=/`, no `Domain`, and `SameSite=Lax` attributes. Bind the authenticated
payload to its format version and cookie purpose. Reject duplicate cookies,
tampering, unsupported versions and invalid payloads before calling Kubernetes.
Encrypt and authenticate the entire payload; signing alone is insufficient.

The cookie's expiry is the earlier of the configured absolute session timeout
and ID-token expiry. Validate it on every request using server time, regardless
of whether the browser still sends the cookie. A restart or request does not
renew either deadline. There is no token refresh or sliding idle timeout in this
first version. Remove the old idle-timeout option and its dependent validation
in the implementation PR; document that configuration change explicitly.

Do not reissue the cookie on routine requests. This avoids a late ordinary
response restoring the cookie after logout and keeps expiry behavior simple.
A fresh successful login issues a new session identifier and CSRF proof. Use a
non-secret derived handle of that identifier for per-session request/stream
bounds; ciphertext changes must not create a new rate-limit identity.

### Keys, size and deployment

- Mount stable encryption/authentication keys from a pre-created Kubernetes
  Secret. The chart references the Secret and does not generate new keys on each
  upgrade. The pod filesystem remains read-only and holds no session database.
- Configure one active key for issuing cookies and a bounded set of previous
  keys for reading them during rotation. Validate key material at startup; absent
  or invalid keys prevent startup. Never silently generate ephemeral keys or
  accept an unreadable cookie through a fallback identity.
- Normal rotation keeps previous keys until their cookies have expired.
  Emergency removal invalidates every cookie protected only by a removed key;
  it is not selective per-user revocation. Restoring an old key can make its
  unexpired cookies usable again, so document that consequence for rollback.
- Enforce a conservative encoded-cookie budget, accounting for its name and
  attributes as well as encryption/base64 overhead. Voter uses 3,800 bytes for
  the encoded value; verify the full header budget for foyer. Fail login clearly
  before setting an oversized cookie. Never truncate a token, split it into an
  unbounded set of cookies or silently switch to server storage.
- Test realistic token and group sizes. Cookie size is an integration constraint
  for the configured issuer; additional session metadata in PR 2 must fit too.
- Keep one replica and `Recreate` for this PR. Established sessions survive
  replacement with the same keys; connections reconnect after the interruption.
  Multiple replicas still need a plan for unfinished logins, response cancellation
  and process-local bounds, even when they can decode the same session cookie.

### Logout and open responses

Logout remains a CSRF-protected POST. It clears the browser's session cookie and
answers 204. The frontend closes its live subscriptions and discards cached
session/CSRF state. Foyer also signals a process-local registry to cancel that
session's already-open requests; this registry coordinates active responses and
does not store durable sessions or revocations.

Clearing a cookie does not revoke a copy. A copied cookie can authenticate again
until its fixed expiry, including after logout, a new login or a restart. A
request racing logout can already be in flight. Local response cancellation is
best effort and cannot provide a global logout guarantee or prevent a copied
cookie from opening another stream. Document this as the chosen lifecycle, and
do not describe logout as server-side session destruction.

Every open response still ends by its authenticated session/token expiry within
the existing session-check bound, and is subject to the configured maximum
response duration. A browser clearing its cookie alone is not a signal an
already-open server response can observe. Adapt the gate's liveness checks to
expiry and local cancellation; remove the assumption that every check must find
a session in a store.

### Unfinished logins and migration

Keep the existing bounded, browser-bound, single-use login transactions in
[`internal/auth/transactions.go`](../../internal/auth/transactions.go). Their key
remains the process's own: a restart during the issuer round trip requires
starting login again. Established audience sessions survive. Do not promise
durable single-use consumption by moving those transactions into a stateless
cookie; that is a separate protocol change.

Existing opaque in-memory session cookies cannot be migrated after their process
ends. The first upgrade requires a new login; subsequent replacements with the
same keys preserve valid encrypted cookies. This PR changes credential custody,
logout and expiry semantics. Update the design, frontend/bounds documentation,
chart values, configuration errors and tests together; the breaking PR title
records that migration while the project remains below 1.0.

### Acceptance

| Scenario | Required evidence |
| --- | --- |
| Established session, orderly restart or abrupt process replacement | The original cookie still authenticates with the same keys; CSRF proof and deadlines retain their values |
| Restart between authorization redirect and callback | The old transaction is refused and a fresh login succeeds |
| Logout | The browser cookie is cleared; locally registered open requests are cancelled |
| Copied cookie after logout or restart | It remains usable before its fixed expiry; the test records this deliberate limit |
| Ordinary response races logout | It sends no renewed session cookie; an already-authorized request may complete |
| Expiry, including during downtime | Expired cookies are rejected; open responses end within the expiry-check bound |
| Modified, duplicate, oversized, wrong-key or unsupported-version cookie | Refused without an upstream request; oversized login emits no session cookie |
| Stable keys and key rotation | Restart and planned overlap preserve sessions; removing the old key rejects its cookies |
| Credential exposure | In the browser, tokens occur only inside authenticated ciphertext in the HttpOnly cookie; krm-foyer sends the plaintext token only to the configured API server, as the bearer credential. Plaintext tokens, cookie values and keys do not leak through logs, URLs or script-readable responses |
| Existing audience rehearsal | Record cookie/header overhead, encryption cost and reconnect behavior with 200 identities and 1,800 streams |

Run `task verify`, including real-cluster restart evidence, browser cookie/CSRF
checks and attempts to forge or replay the credential. Update the existing
token-leak tests to retain plaintext-leak detection while allowing the intended
encrypted cookie; do not exempt all `Set-Cookie` values from inspection.

### PR 1 as built

- **Logins in progress** were moved, after this plan was written (#34), from a
  server-side table into sealed cookies of their own, with a key the process generates
  at start. That keeps this plan's lifecycle: a restart refuses the callback, and a
  fresh login succeeds. They do not use the session keys.
- **Keys:** at most four, one per line in a file from a Secret, the first sealing. Each
  is used only through HKDF-derived keys: one for AES-256-GCM, one for the 8-byte ID a
  cookie names its key by, so opening never tries keys in turn.
- **A new login in the same browser** also ends the open responses of the session it
  replaces, as logout does; the old cookie is not revoked.
- **Lowering `-session-absolute-timeout`** shortens cookies already issued: the end is
  the earlier of the sealed end and the issue time plus the configured timeout.
- **Budget:** 3,800 bytes of encoded value, as voter. A Dex-shaped token with 30
  groups of 24 characters gives a 3.2 KB cookie; the token can be about 2,600 bytes.
- **Rehearsal:** besides cookie sizes, it restarts krm-foyer with all 1,800 streams
  open and times every stream's reconnect with the cookies from before.

## PR 2: configurable login and Kubernetes identity

### Extra authorization parameters

Add a provider-independent configuration for extra query parameters sent to the
discovered authorization endpoint. Configuration supplies their names, optional
defaults and whether the browser may supply a value. An optional value allowlist
supports choices such as connector selection. Absence from configuration means
the parameter is not forwarded.

Proposed configuration shape, to be exposed through a mounted configuration file
and corresponding Helm values:

```yaml
login:
  authorizationParameters:
    connector_id:
      default: audience
      allowFromRequest: true
      allowedValues: [audience, operator]
    login_hint:
      allowFromRequest: true
  sessionClaims:
    displayName: /name
    groups: /groups
    connector: /federated_claims/connector_id
```

The parameter and connector-claim names above are deployment examples, not
built-in knowledge. Another issuer can use different names through configuration.
Claim paths use JSON Pointer; implement a small lookup, not an expression or
template language. Existing issuer, client and scope configuration stays separate.
Provider-specific scopes are selected by the deployment, never added by detecting
an issuer product.

Reserve an `oidc.` prefix on `/auth/login` for browser-supplied extra parameters.
The application can generate this URL itself, including encoding it as a QR code:

```text
https://app.example.com/auth/login?return_to=%2F&oidc.connector_id=audience&oidc.login_hint=opaque-value
```

Foyer removes the `oidc.` prefix and adds approved values to its authorization
request. It does not interpret `opaque-value`. The configuration's allowlist is
server-owned; a QR code carries selected values, never permission to enable new
parameters. The QR generator and any application-side allowlist are conveniences,
not proof that the request came from a trusted caller.

Define the forwarding contract precisely:

1. A configured default applies when the request omits that parameter. A permitted
   request value overrides it. A default must satisfy any configured value list.
2. Unknown `oidc.*` parameters, repeated values and values outside an explicit
   value list produce a stable 400 before starting a login transaction. A supplied
   value for a configuration-only parameter is also refused.
3. Foyer owns `client_id`, `redirect_uri`, `response_type`, `response_mode`,
   `scope`, `state`, `nonce`, `code_challenge` and `code_challenge_method`.
   Reject configuration or request input that tries to replace these. Scope
   selection remains trusted deployment configuration. Also reserve `request`
   and `request_uri`, which could replace the authorization request, and refuse
   credential parameters such as `client_secret`. Extra parameters cannot replace
   the configured/discovered endpoint.
4. Encode each value as query data exactly once. Bound total request and parameter
   sizes using generic limits. Do not trim, uppercase or otherwise normalize a
   provider's value, and do not log those values or reflect them in error pages.
5. `return_to` stays local and is never forwarded as an issuer parameter. Login
   recovery preserves validated connector/option selection for the transaction's
   lifetime without echoing opaque hints into error-page links; after expiry, the
   application supplies a fresh login link or QR code.

The browser helper may accept an optional parameter map while retaining its
existing `login(returnTo)` call. Foyer does not generate QR images or discover
connectors. Direct links work without the helper.

### The current Room Pass handoff is an integration dependency

Voter currently writes a named handoff cookie because its room code does not
automatically travel through the issuer redirect chain. Generic authorization
parameters alone do not make that cookie protocol disappear.

The application/provider integration must supply a supported entry URL or carry
an approved opaque hint through that chain. Any required handoff cookie remains
in that integration. Before switching voter to foyer, prove one real QR-to-login
journey there. Do not add a Room Pass branch or room-code validity tests to foyer
to compensate for missing support upstream.

### Session metadata and whoami

Extend `/auth/session` with `displayName`, `groups` and an optional `connector`.
Read these only from the verified ID token. Default display name and groups to
the `name` and `groups` claim paths; leave connector mapping unset until
configured. Preserve existing issuer, subject, email, CSRF and expiry fields.
Optional missing claims yield empty metadata; present claims with incompatible
types produce a stable login error. Keep an explicit response shape rather than
returning an arbitrary bag of claims.

The connector in a login URL is a requested option. The connector returned in a
session is verified token metadata. Never copy the former into the latter. These
fields help the application present identity; Kubernetes still decides access.

Implement `GET /auth/whoami` by creating a fresh SelfSubjectReview with the
session's own token. Return Kubernetes' `userInfo`, plus the session issuer and
expiry, as JSON with `Cache-Control: no-store`. Keep issuer groups in
`/auth/session` distinguishable from Kubernetes' mapped groups in `userInfo`.
Use the configured API server and verified TLS, bound the call, refuse redirects
and preserve upstream denials. A missing, invalid or expired cookie is 401;
unavailable identity infrastructure produces a bounded failure, not an identity
guess. Do not infer a Kubernetes username from a token claim or fall back
to another credential. Reuse the existing shared-watch identity lookup machinery
where that avoids a second implementation of the same boundary.

### The two fixed gitops-reverser attribution fields

Gitops-reverser consumes these exact keys from Kubernetes `user.extra` in audit
events and admission requests:

| Fixed key | Signed OIDC claim | Use |
| --- | --- | --- |
| `configbutler.ai/claims/display-name` | `name` | Git author name |
| `configbutler.ai/claims/email` | `email` | Git author email |

They are **UserInfo extra keys, not HTTP request headers**. Pin these two names in
the integration recipe and fixture tests for the first implementation. There is
no need for a configurable attribution-key framework.

```mermaid
sequenceDiagram
    participant F as Foyer
    participant K as Kubernetes API server
    participant G as gitops-reverser
    F->>K: Request with the user's signed ID token
    K->>K: Verify token; map name/email into user.extra
    K->>G: Admission request and/or audit event with UserInfo
    G->>G: Read the two fixed attribution keys
```

Foyer already forwards the bearer token that contains those claims. The cluster
operator adds these entries to the existing JWT authenticator's
`claimMappings.extra`:

```yaml
extra:
  - key: configbutler.ai/claims/display-name
    valueExpression: "claims.?name.orValue('')"
  - key: configbutler.ai/claims/email
    valueExpression: "claims.?email.orValue('')"
```

This is a fragment to merge into the authenticator, not a replacement for its
issuer, audience, username, groups or validation rules. Retain its email
verification policy, particularly when email contributes to the username.
Kubernetes documents this mechanism under
[structured authentication configuration](https://kubernetes.io/docs/reference/access-authn-authz/authentication/#using-authentication-configuration).

Do not synthesize `Impersonate-Extra-*` or `X-Remote-*` headers. Ordinary custom
headers do not populate these fields on the bearer-token path. Switching to
impersonation or trusted request-header authentication would change who establishes
identity and break the current design. Browser-supplied identity headers continue
to be stripped. The correct delivery is the signed token plus Kubernetes' mapping.

`/auth/whoami` makes that mapping visible in `userInfo.extra`. An integration
check must also inspect the accepted write's audit event and a captured admission
request, because those are the two inputs gitops-reverser actually consumes.
Use fixed expected values for the two keys. Missing claims remain missing/empty;
foyer must not invent author information from query parameters or headers.

### Acceptance

| Scenario | Required evidence |
| --- | --- |
| Two differently configured issuer parameter names | Same foyer code forwards the configured names and defaults |
| Application-generated link or QR URL | Approved values reach the authorization endpoint with unchanged decoded values |
| Unknown, duplicate, disallowed or protocol-owned parameter | Refused before redirect; no replacement of PKCE, state, nonce, client or callback |
| Provider-specific input | Treated as opaque data; no room-code interpretation or validity tests |
| Requested connector differs from verified claim | Session reports only the verified claim |
| Valid session, missing optional claims | Predictable metadata shape, no invented identity |
| Kubernetes identity lookup | The API server sees the user's credential and returns its actual mapped identity |
| Spoofed display-name/email, forwarding or impersonation headers | No change to `userInfo`, admission identity or audit attribution |
| Accepted mutation and CommitRequest-style admission | Both fixed extras come from signed claims; no impersonated actor or service-account fallback |
| Issuer or API refusal, invalid or expired session cookie | Bounded failure and no successful-looking identity fallback |
| Credential exposure | New routes and logs pass the existing token-leak checks |

Use a generic test issuer for parameter forwarding, and the cluster fixture for
identity and attribution. The QR generator's image tests and the real provider's
handoff tests belong to their respective projects. Run `task verify` for the PR.

### PR 2 as built

- **Configuration** is `-login-config-file`, YAML or JSON with the shape above minus
  the `login:` wrapper; the chart's `login` values render it into a ConfigMap whose
  checksum rolls the pod. Unknown keys stop startup.
- **Bounds:** at most 16 parameters and 64 allowed values each; a value is at most 512
  bytes, and a login request adds at most 1,024 bytes of names and values. Empty values
  are refused. `client_assertion` and `client_assertion_type` are reserved too.
- **Retry:** a failed login's link repeats only values of parameters that have
  `allowedValues`; a free value such as `login_hint` is never echoed.
- **Session claims** are not stored beside the token: `/auth/session` reads them from
  the token sealed in the cookie, which login verified, so groups are not paid for twice
  in the cookie budget. Login checks they can be read.
- **`/auth/whoami`** lives with the stream code, whose shared-watch reviews already sent
  the same SelfSubjectReview; both now use one function. It goes through the gate, so
  the per-session rate and concurrency bounds hold.
- **Admission evidence** comes from a ValidatingAdmissionPolicy in `Warn` mode that
  reports the admission request's `userInfo` in a warning on an accepted write. The API
  server gives a webhook, such as gitops-reverser's, the same `userInfo`.

## Application migration and release checks

Voter updates its login links from `return=` to `return_to=` and supplies its
configured extra parameters under `oidc.*`. It adapts to foyer's existing expiry
format and obtains Kubernetes identity from `/auth/whoami`. Application settings
such as namespace, coffee configuration and `canVote` remain application-owned;
they do not become generic foyer session fields.

Coffee's per-user Kubernetes reads and domain endpoints are explicitly outside
both PRs. This plan adds no coffee proxy, backend identity-check endpoint, token
handoff or service-account substitute. The coffee application will choose its own
integration before migrating those endpoints.

Release evidence for this scope is: a QR/link login through the intended issuer,
the expected display metadata, a Kubernetes write attributed through both fixed
extras, a replacement of the foyer pod that preserves the session, and a logout
that clears the browser cookie and cancels locally registered responses. Verify
that copied cookies remain usable only until the documented fixed expiry; do not
claim global revocation. Mark these complete only
when their tests exist and pass. This plan does not mark unrelated release
criteria in the main design complete.

## Evidence used for this plan

- Foyer at `de1ad46`: [auth](../../internal/auth/auth.go),
  [transactions](../../internal/auth/transactions.go),
  [sessions](../../internal/session/session.go) and
  [shared-watch identity lookup](../../internal/stream/shared.go).
- Voter at `51dbb1f`: [login and current handoff](https://github.com/sunib/voter/blob/51dbb1f0928794bdd83339b3794bbba1755b2df2/voter/oidc.go),
  [encrypted session payload and cookie limits](https://github.com/sunib/voter/blob/51dbb1f0928794bdd83339b3794bbba1755b2df2/voter/participant_session.go),
  [cookie codec](https://github.com/sunib/voter/blob/51dbb1f0928794bdd83339b3794bbba1755b2df2/voter/session_cookie.go),
  [session response](https://github.com/sunib/voter/blob/51dbb1f0928794bdd83339b3794bbba1755b2df2/voter/oidc_handlers.go)
  and [API-server claim mappings](https://github.com/sunib/voter/blob/51dbb1f0928794bdd83339b3794bbba1755b2df2/test/e2e/authentication-config.yaml).
- Gitops-reverser at `077bb88c`: the
  [audit extractor](https://github.com/ConfigButler/gitops-reverser/blob/077bb88cb4fe1baac7ff5125bb7350939fed7b25/internal/queue/audit_event_parsing.go)
  and [admission extractor](https://github.com/ConfigButler/gitops-reverser/blob/077bb88cb4fe1baac7ff5125bb7350939fed7b25/internal/webhook/validate_operator_types_handler.go)
  read the same two fixed keys. The
  [attribution wiring](https://github.com/ConfigButler/gitops-reverser/blob/077bb88cb4fe1baac7ff5125bb7350939fed7b25/docs/architecture.md#wiring-oidc-author-claims)
  documents their mapping from signed claims.
