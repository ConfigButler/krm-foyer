# Service design

krm-foyer is a backend for frontend (BFF) for browser applications built on Kubernetes
APIs. It owns OIDC login and server-side sessions, proxies Kubernetes API requests with
the user's own credential, and hosts krm-stream resource
streams on the frontend's origin.

This document is the contract: what krm-foyer must do. **None of it is implemented yet.**
Every guarantee below is a requirement until the test that tries to break it exists; the
[roadmap](roadmap.md) tracks which do. Why the service exists, and what it expects from
the domains behind it, is in the [vision](vision.md).

## Architecture and ownership

```mermaid
flowchart LR
    UI[Browser frontend] -->|HttpOnly session cookie| F[krm-foyer]
    F <-->|OIDC login and token exchange| IDP[OIDC provider]
    F -->|Native API with user credential| K[Kubernetes API server]
    F --> S[krm-stream gateway]
    S -->|Authorized watches| K
    S -->|Resource events through krm-foyer| UI
    O[Domain operator] <-->|Process resources and publish status| K
```

Deploy on the same origin as the frontend. An ingress or gateway routes krm-foyer's
prefixes (`/auth/`, `/k8s/`, `/stream`, `/_foyer/`) to it and everything else to the
frontend and any domain backend; krm-foyer does not host application files or forward
traffic outside its prefixes. Every service on that origin is inside the user's trust
boundary, since it can act as the signed-in user; see
[sharing one domain](ingress.md#decision-2026-10-01-sharing-one-domain-with-other-services).
The initial design uses one configured cluster per deployment and a configurable OIDC
provider. No particular identity provider or frontend framework is required.

krm-foyer terminates TLS itself from a mounted certificate, reloaded when it changes, or
sits behind an ingress that terminates TLS. Both are supported, but they are not equally
safe: behind an ingress, the hop to krm-foyer carries session cookies, and in plain HTTP
anything that can observe that hop can take a session. Plain HTTP is acceptable only
where nothing but the ingress can reach krm-foyer; otherwise the ingress re-encrypts and
verifies krm-foyer's certificate. See [both TLS models](ingress.md#decision-2026-10-01-both-tls-models).
Its public URL is configuration: the OIDC redirect URI, same-origin and CSRF checks and return
paths use it, never `Host` or `X-Forwarded-*`. An ingress in front must be transparent: no
buffering of streams, and no authentication or header rewriting of its own on krm-foyer's
routes. An ingress's external-authentication feature (`auth_request`, ForwardAuth) is used only as a
login gate for the application's pages, through `/auth/check`. It never decides on `/k8s` or
`/stream` traffic. The [ingress decision](ingress.md) explains why, and what would make
more worth revisiting.

krm-foyer has two halves. The **login half** (`/auth/...`) obtains the user's OIDC token
and keeps it in the server-side session. The **API half** (`/k8s`, `/stream`) takes the
user's token from one credential interface and sends the request with that token, and
the API server validates it and decides. The API half's contract is the user's own
token, checked by Kubernetes. Login is a convenience behind that interface: it could
later accept tokens obtained elsewhere, or move into a separate program, without changing
the API half.

| Component or team | Responsibility |
| --- | --- |
| krm-foyer | OIDC client, sessions, CSRF protection, fixed upstream routing, API proxy and stream host configuration |
| [krm-stream](https://github.com/ConfigButler/krm-stream) | Resource-stream protocol, recovery, projections, optional watch sharing and browser draft/reconciliation primitives |
| Kubernetes | Discovery, persistence, RBAC, admission execution, API validation and write concurrency |
| Domain team | Resource contracts, admission rules, controller processing, trusted status and domain guarantees |
| Frontend team | Forms, resource queries, save intent, navigation and presentation of domain outcomes |
| Platform team | Deployment, grants that match each application, availability and upgrades |

Application-specific endpoints, DTO transformations, result aggregation and business plugins
are outside krm-foyer's scope; the [vision](vision.md#staying-small) lists what stays out.
Domain services and operators own those responsibilities. Streaming and reconciliation
come from krm-stream, not from a reimplementation here.

## API contract

| Route | Behavior |
| --- | --- |
| `/auth/login` | Start OIDC authorization-code login with PKCE, state and nonce |
| `/auth/callback` | Validate the callback and establish a session |
| `/auth/session` | Return minimal identity/session state and CSRF information, never bearer tokens |
| `/auth/logout` | CSRF-protected POST that destroys the server session |
| `/auth/check` | 204 or 401 (or 302 to login on request) for an ingress gating the application's pages; never a token or identity. See the [login gate](ingress.md#decision-2026-10-01-a-login-gate-for-the-applications-pages) |
| `/k8s/api/...` | Proxy core Kubernetes APIs after stripping `/k8s` |
| `/k8s/apis/...` | Proxy grouped APIs, including CRDs and aggregated APIs |
| `/k8s/api`, `/k8s/apis`, `/k8s/version`, `/k8s/openapi/...` | Proxy discovery and schema endpoints |
| `/stream` | Serve krm-stream, for whatever the user may watch |
| `/auth/whoami` | Who Kubernetes takes the user to be, from a SelfSubjectReview, plus the session's issuer and expiry. Never tokens |
| `/_foyer/access` | A page showing what the user may do, from Kubernetes' own reviews. See [what may I do](#what-may-i-do) |

For example, POSTing to
`/k8s/apis/workspaces.example.com/v1/namespaces/team-a/workspacerequests` creates a
resource through Kubernetes (the [vision](vision.md#what-it-takes-from-the-domain) follows
that request through its lifecycle). The response retains Kubernetes' status code and object shape.
Whether it succeeds is Kubernetes' decision alone.

Preserve bodies, Kubernetes `Status` errors, content types, relevant headers and query
parameters. Support pagination, selectors, native watches, CRUD, patch types, dry-run and
Server-Side Apply using their [native semantics](https://kubernetes.io/docs/reference/using-api/api-concepts/).
Clients choose concurrency preconditions and field ownership. The proxy must not force
apply, convert PATCH to PUT or automatically replay mutations after authentication recovery.
A lost response may already have committed a write.

The service has no hardcoded resource catalogue. Its initial transport scope covers ordinary
HTTP APIs, streaming logs and native HTTP watches. Exec, attach and port-forward require
separate upgrade-protocol support and tests; return an explicit unsupported error until
implemented. The browser cannot supply an arbitrary upstream URL.

**Service, node and pod proxy subresources are deferred** and get the same explicit
unsupported error. RBAC would still decide who may use them, but what sits behind them
is an arbitrary application, and an arbitrary application may change state on a `GET`.
krm-foyer cannot tell from the method, and a cross-site page can make a signed-in
browser send a `GET` by navigating to it; holding back the HTML that comes back happens
after the backend has acted. So when they are supported, **every request through a proxy
subresource, whatever its method, needs CSRF proof** and is refused without it, which
also means they cannot be opened by typing a URL into a tab. The
[upstream response](#upstream-responses) rules apply on top. Kubernetes' own `GET`s are
safe by API convention, so the rest of `/k8s` stays navigable.

### Upstream responses

Stripping request headers covers what goes up. What comes back crosses the same
boundary, onto an origin the browser trusts, and needs rules of its own. They hold for
every route, and matter most for aggregated APIs and, once supported, proxy
subresources, which return whatever their backend sends:

- **Content types are allowlisted.** JSON, YAML, Kubernetes protobuf and the watch
  stream types pass; `text/plain` passes for logs. Anything else, `text/html` above all,
  is held back. A pod log or a proxied service must not be able to serve a page that
  runs as the user on the shared origin.
- **Every proxied response carries** `X-Content-Type-Options: nosniff`,
  `Content-Security-Policy: default-src 'none'; sandbox` and `Cache-Control: no-store`,
  replacing anything upstream sent. A shared cache in front of the origin must never
  hold one user's answer for another. `/auth/session` and `/auth/check` are `no-store`
  too.
- **Response headers are allowlisted** (content type and length, `Audit-Id`, `Warning`,
  `Retry-After` and the API priority-and-fairness headers). Everything else is dropped,
  including `Set-Cookie`, which would reach every service on the origin, and
  `Access-Control-*`: krm-foyer is same-origin only and never grants CORS.
- **Bodies reach the browser decoded.** Dropping `Content-Encoding` while passing a
  gzip body on would hand the browser bytes it cannot read. krm-foyer drops the
  browser's `Accept-Encoding`, so Go's transport asks the API server for gzip and
  decodes it transparently (it does not when the request already names an encoding),
  and the browser receives the body uncompressed with no `Content-Encoding`. The bound
  on response bytes counts decoded bytes, so a small compressed body cannot expand past
  it. A response that still carries a `Content-Encoding` is held back. Compressing
  towards the browser is a later performance choice, not part of the boundary.
- **Redirects are not followed and not passed on automatically.** A `Location` from an
  aggregated API could send the browser anywhere, so the user decides.

A held-back response is answered as an [interruption](#interruptions): code gets a
`Status` with status 502 and the reason, and a person browsing gets a page that
explains it. For a redirect, both carry the target; for HTML, the page shows its source
as escaped text. Every interruption is logged.

### Interruptions

krm-foyer is meant to be explorable in a browser, not only through a library. Opening
`/k8s/api/v1/namespaces/team-a/configmaps` in a tab shows the API server's JSON, exactly
as code would receive it. Wherever krm-foyer itself stands between the user and that
answer, it says so in a form the requester can read:

| Interruption | Status | For code | For a person browsing |
| --- | --- | --- | --- |
| No session | 401 | `Status`, reason `Unauthorized` | A page with a **Sign in** link that returns to this URL |
| Upstream redirect | 502 | `Status` with the target in `details` | A notice naming the full target, with a link the user can follow |
| Upstream content held back | 502 | `Status` with the content type | A page showing the response as escaped text, truncated at a bound |
| Session store unavailable | 503 | `Status` | A page saying so, with no retry loop |
| Non-canonical path | 400 | `Status` naming the [path rule](#access) | A page saying so |
| Missing CSRF proof or cross-origin request | 403 | `Status` with a reason that is not RBAC's | A page saying so |
| Unsupported protocol or subresource | 501 | `Status` naming what is unsupported | A page saying so |
| A [bound](#access) reached | 429, or 502 when a response exceeds its size bound | `Status` naming the bound, with `Retry-After` where waiting helps | A page saying so |

This table is the complete list of answers krm-foyer gives instead of the API server's.
Anything not in it is the API server's answer.

Rules:

- **The status code is the same in both forms.** Only the body differs. A client that
  checks the code sees one behavior.
- **A page is chosen only for a browser navigation:** a `GET` with `Sec-Fetch-Dest:
  document`. Browsers set that header, page scripts cannot, and non-browser clients do
  not send it, so `fetch`, the helper and `kubectl`-style clients always get JSON.
- **Answers from Kubernetes are never replaced.** A 403 from RBAC, a 404 or a 409 is the
  API server's answer and reaches the tab as its JSON. Interruptions are only what
  krm-foyer itself decided. For a 403, [`/_foyer/access`](#what-may-i-do) is where a
  person finds out why.
- **Permission is a link, never an action.** Following a redirect is a navigation the
  user starts; nothing is re-sent, and no request with a body is ever continued. An
  upstream HTML page is never rendered on the origin, with or without consent: consent
  to run unknown script as yourself is not consent anyone can meaningfully give.
- **The pages follow the [page rules](frontend.md#what-ships-in-the-binary):** no script,
  a strict Content-Security-Policy, `no-store`, and nothing about configuration beyond
  what the requester is allowed to see.

## Access

**Kubernetes alone decides what a request may do.** krm-foyer adds no access rules of
its own: whatever the user's RBAC allows is reachable through `/k8s` and `/stream`, and
whatever it refuses is refused by the API server, with the API server's answer. There is
no allowlist, so there is nothing to configure and no second set of rules to keep in step
with RBAC.

The consequence has to be said plainly: **a session on krm-foyer carries the user's full
Kubernetes access.** Any script running on the application's origin can use all of it.
Deploy krm-foyer for users whose grants match what the application needs, and keep the
origin to services you trust with that access. Limiting what an application may do
beyond RBAC is a separate, deferred design: [application scope](application-scope.md).

What krm-foyer still does on every request, none of it an access decision:

- **Path hygiene.** Non-canonical paths (encoded slashes, dot segments, repeated slashes,
  needless percent-encoding) are rejected rather than normalized, and the path forwarded
  is byte-for-byte the path received, minus the `/k8s` prefix. Real Kubernetes clients
  never send such paths, and a scope layer added later can rely on there being one parse.
- **Bounds** on page size, response bytes, request rate, watch duration and concurrent
  streams. They protect krm-foyer and the API server; they do not prevent export. A user
  allowed to list a namespace can retrieve all of it through repeated pages.
- **Never a privileged service account** as a fallback for a user's request.

### What may I do

Kubernetes already answers this question, and code should ask it there: a
SelfSubjectAccessReview through `/k8s` says whether the user may do one thing, and a
SelfSubjectRulesReview lists what they may do in a namespace. Without a scope layer,
those native answers are exactly what requests through krm-foyer will get.

For a person with a browser tab, krm-foyer adds one page, `/_foyer/access`:

- **The rules for a namespace,** from a SelfSubjectRulesReview sent with the user's own
  token, as a table of resources and verbs. When Kubernetes marks the answer incomplete,
  which it does for authorizers that cannot list rules, the page says so in plain words.
- **A "can I?" form** (verb, resource, namespace, optionally a name) answered by a
  SelfSubjectAccessReview, which is authoritative for every authorizer. The form is a
  `GET`; asking changes nothing.
- **A page only.** Code uses the native reviews above; a second JSON shape for the same
  answer would only be something to keep in step.
- **Signed-in users only, about themselves.** Anonymous visitors get the 401
  [interruption](#interruptions). Reviews are audited as the user, and rate-limited per
  session.
- **It shows no objects and changes nothing.** Rules and answers, not a resource browser.

**Review grants before deploying.** An existing application may enforce rules that its
users' Kubernetes grants do not express. Once krm-foyer serves those users, every grant
they hold is reachable from the browser, even if the frontend still calls the old endpoint. Likewise, existing
list/watch grants may reveal records that an application previously returned only as aggregates.
Audit effective grants and deploy required admission/read boundaries before enabling access.

If a migration narrows raw reads, coordinate the replacement read path: an old result handler
using the user's token will also lose those permissions. Admission cannot filter ordinary
resource reads. If a product instead adopts asynchronous acceptance, explicitly redefine
what may be stored and what may be processed before exposing creates.

## Login and sessions

Tokens and session contents stay server-side. The browser holds only an opaque session ID
in a Secure, HttpOnly, host-scoped cookie with appropriate SameSite settings. Server-side
sessions support per-session revocation and refresh-token custody. They require shared
storage across replicas, bounded logout propagation and a defined failure policy.

**The session ID is a bearer credential.** Whoever holds it can call krm-foyer as the
user, from anywhere, until the session ends. `HttpOnly` keeps it from page scripts and
`Secure` from plain-HTTP requests; neither keeps it out of a log file, a co-hosted
service that sees the `Cookie` header, or an unencrypted proxy hop. So krm-foyer treats it
like a token: it never logs it or puts it in a URL or error page, and the session store
keys sessions by a hash of the ID, so reading the store does not yield usable IDs.

An encrypted HttpOnly cookie can also keep tokens unreadable by JavaScript; the reason
for choosing opaque sessions is revocation and lifecycle control. Clearing a browser cookie
alone does not invalidate a copied stateless session. The BFF pattern is described in [OAuth 2.0 for Browser-Based Applications](https://www.rfc-editor.org/rfc/rfc10017.html).

Use a maintained OIDC library. Bind login transactions to the browser; validate issuer,
audience and callback state; accept only validated local return paths. Rotate session IDs
at login, enforce idle/absolute expiry and serialize supported refreshes per session. Reject
requests when session validity cannot be established. The configured issuer must provide a
credential accepted by the cluster; an arbitrary OIDC login or access token is insufficient.

Unauthenticated API requests return a documented 401 login-required response, not an HTML
redirect; a person browsing gets the same 401 with a sign-in link (see
[interruptions](#interruptions)). An optional framework-independent helper handles login navigation and return paths.
Expose authentication-required state before navigation so an editor can offer draft copy-out
or an explicitly designed preservation flow. A 403 represents permission denial, not a login
loop. Bound refresh attempts and report persistent issuer/cluster configuration errors.

Require CSRF proof and same-origin checks for mutations, logout and, once supported,
every request through a proxy subresource. Strip browser-supplied
Authorization, impersonation and untrusted forwarding headers; never forward the session
cookie to Kubernetes. Pin upstream destinations, verify TLS, bound request/stream resources
and avoid credential/body logging. HttpOnly does not prevent malicious same-origin JavaScript
from making requests as the user.

### Session lifecycle

Revocation is as fast as its slowest path. Each row is a requirement with a test:

| Event | Required behavior |
| --- | --- |
| Logout | The session is deleted from shared storage before the logout response is sent. No replica caches a session's validity, so every replica refuses the ID on its next request |
| Logout during a refresh | Logout wins. The refreshed tokens are discarded, never written back into a deleted session |
| Session store unavailable | Fail closed: API and stream requests get 503. Never a stale local copy, never anonymous, never the service account |
| Streams open at logout | Closed on every replica within the session-check interval, a configured bound with a documented default |
| Token expiry or failed refresh | API requests get 401; streams close at the token's expiry or the session's, whichever comes first |
| RBAC change | Kubernetes applies it on the next request. A shared watch applies it within its SubjectAccessReview recheck interval; that is reauthorization, not revocation |
| Issuer refuses a refresh | The session ends at once: API requests get 401 and its streams close. krm-foyer never retries a refusal into a success or keeps using the old token past its expiry |
| User disabled at the issuer | Provider-dependent; see below. The only bound krm-foyer itself guarantees is the session's absolute expiry |

Disabling a user reaches krm-foyer only as a refused refresh. Until then the API server
accepts the ID token krm-foyer already holds, so the user keeps access for the rest of
that token's lifetime **plus** however long the issuer goes on granting refreshes after
the account is disabled. That second part belongs to the issuer, and OAuth leaves it to
the implementation: an issuer may cache an upstream identity provider's answer, or not
check it on refresh at all. So krm-foyer documents the bound per issuer configuration,
and claims one only where a test has disabled a user and measured when the refresh is
refused. Elsewhere the documented bound is the absolute session expiry. Operators who
need a tighter one shorten the token lifetime or the absolute expiry, or choose an issuer
that checks the account on every refresh.

Revoking a session does not revoke tokens the issuer handed to other clients, and does
not undo writes Kubernetes already accepted.

## Streams and editing

krm-foyer supplies krm-stream's principal resolution, authorization and backend
selection. What a user may watch is what RBAC lets them watch. Which resources use a
shared watch is configuration for efficiency, not for access. Start with
user-authenticated watches. Optional shared watches use a
narrowly scoped service account, Kubernetes-resolved identities and per-subscriber
SubjectAccessReview checks. Bound reauthorization and session expiry; logout closes that
session's streams without disrupting others. Measure authorization load as well as watch
savings. Disable stream buffering and propagate cancellation through the proxy.

A stream projection is a view transformation. It cannot protect confidential fields if the
same user can GET the full resource through `/k8s`. Restrict raw routes or separate the
read models when disclosure differs; Kubernetes RBAC does not provide general field-level
permissions.

Keep draft capture and guarded reconciliation in the frontend. A raw GET used to reconcile
a projected editor must follow the same view contract and guards against late responses or
changed UID. Never manufacture redaction revisions from a raw GET. Verify the library APIs
support the chosen composition.

Use conditional PATCH with UID/resourceVersion checks where required. Do not replace a full
resource with its projected view or assume arrays merge safely. A write response must not
overwrite newer watch state or later typing: consume it as a receipt or through guarded
reconciliation. A restricted projected-write policy needs its own validation; transparent
proxying does not supply that policy. Multi-step domain workflows retain explicit handling
of partial success.

## Release criteria

| Capability | Required evidence |
| --- | --- |
| Reuse | Two frontends using different API groups without application-specific backend handlers |
| Authentication | Callback failure, expiry, refresh, restart and logout tests across replicas |
| Session lifecycle | Every bound in [session lifecycle](#session-lifecycle) measured by a test, including logout racing refresh, store outage, streams open at logout and a refused refresh; a disablement bound is claimed only for issuer configurations where a test measured it |
| Credential custody | No token krm-foyer holds, including refreshed ones, and no session ID in any response, log line or error page the suite collects |
| Access | Every answer through krm-foyer equals the API server's answer for the same token, except the [interruptions](#interruptions), each with a test that it happens exactly when the table says; non-canonical paths are rejected; nothing falls back to the service account |
| Proxy semantics | Kubernetes errors and patch types preserved; conflicting writes and ambiguous create outcomes handled without automatic replay |
| Upstream responses | An upstream that sends HTML, `Set-Cookie`, CORS headers, cache headers or a redirect has none of them reach the browser unasked; a gzip response arrives decoded, and the size bound holds for its decoded bytes |
| Proxy subresources | Refused as unsupported until supported; then refused without CSRF proof for every method, including a cross-site `GET` navigation |
| Interruptions | Each interruption gives the same status to code and to a navigation; scripts cannot obtain the page form; no Kubernetes answer is ever replaced |
| What may I do | `/_foyer/access` agrees with what requests actually get, and follows a RoleBinding change without a new login |
| Streams | Cancellation, recovery, expiry, subscriber isolation and measured load under declared capacity targets |
| Editing | Conditional saves and guarded reconciliation preserve newer state and drafts |
| Example domain | Pending, accepted, rejected and failed processing demonstrated; protected status and restart/duplicate tests prove its declared guarantees |
| Operations | Owned session storage, deployment/upgrade procedures, telemetry and tested failure behavior |

The domain operator's tests establish domain guarantees; the gateway's tests establish
transport and access behavior. Publish the service with its own integration fixture,
versioned image and documented supported protocols.

## Related decisions

- [Application scope](application-scope.md): why a browser application might be limited
  beyond RBAC, and why krm-foyer starts without it.
- [Ingress and TLS](ingress.md): both TLS models, sharing one domain, the login gate,
  and why an ingress never makes the access decision.
- [Pages](frontend.md): which pages krm-foyer serves itself.
- [Testing](testing.md): how the suite proves krm-foyer invents neither authentication
  nor authorization.
- [Name](name.md): why it is called krm-foyer.
