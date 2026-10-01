# krm-foyer

**Browser login, Kubernetes API access and live resource streams.**

krm-foyer is a proposed backend for frontend (BFF) for applications built on Kubernetes
APIs. It handles authentication, server-side credentials and resource transport so teams
can build browser interfaces without implementing those components for every application.

Its purpose is to **make well-designed Kubernetes Resource Model (KRM) domains usable
from the browser**. Teams define resources, lifecycles and domain rules; krm-foyer connects
their frontends to those APIs. This document specifies the intended service contract;
it is not a claim of released functionality.

## Who it helps

A platform team can expose approved APIs through one maintained authentication and transport
service. A frontend developer can read resources, submit changes and observe progress using
Kubernetes objects and krm-stream. A domain team can support browsers, command-line clients
and automation through the same resource contract.

A workspace portal, equipment reservation service and configuration editor can all use
the same transport. Their domain contracts differ: provisioning is asynchronous, scarce
reservations need arbitration, and editing needs conflict handling. The
[decision guide](bff-choice.md#compare-application-fits) compares these cases with reporting
and payment workflows.

Adding a resource whose permissions and invariants are already enforced should require
configuration and frontend work, not another backend transport handler. The benefit is
reuse across applications. A separate service also adds deployment, session storage and
support costs; it does not guarantee less total code for a single application.

## Domain APIs built with KRM

CustomResourceDefinitions (CRDs) can express desired state or immutable requests. Admission
checks constraints before storage; domain controllers process resources and publish status.
The frontend creates or edits a resource and observes its outcome.

### Example: requesting a workspace

A user chooses a template and submits an immutable `WorkspaceRequest`. Assume this example
domain permits a user to request several workspaces; one-per-person enforcement would need
an additional identity and uniqueness contract.

1. The browser checks its session and starts OIDC login if required. After login, it POSTs
   the resource with its CSRF proof. Admission validates the template and binds ownership
   to trusted identity. It rejects invalid input before storage.
2. Kubernetes returns HTTP 201 and the stored object's UID. The UI displays **Request
   received**, not **Workspace ready**, and subscribes to the permitted resource stream.
3. The domain controller evaluates eligibility and capacity. It publishes acceptance or
   rejection in protected status. A policy rejection has a stable reason the UI can explain.
4. For accepted work, the controller provisions using a stable operation key tied to the
   request UID. After a restart, it discovers or resumes the same work rather than blindly
   creating another workspace. The UI displays **Provisioning** and any retryable failure.
5. Once ready, status supplies an authorized workspace reference. The UI offers **Open
   workspace**. That destination enforces its own access checks; knowing its URL is not a grant.

A lost create response leaves the outcome uncertain: the frontend uses an authorized
lookup and the domain's stable request key to reconcile before offering a retry. The
controller contract defines retryable versus terminal failures. A disconnected browser
shows stale/unknown progress; it must not turn missing updates into a domain rejection.
Cancellation and cleanup need their own lifecycle rules rather than assuming DELETE
instantly cancels external work.

This is an illustrative domain, not a built-in API or a proven implementation. Its acceptance,
provisioning and ready states are domain outcomes transported by krm-foyer and krm-stream.

The domain backend lives in controllers and admission, commonly packaged as a Kubernetes
operator. Domain developers design and test its guarantees; the platform team operates it.
The [operator pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
supports this division of responsibility. A status field alone does not enforce correctness.

This approach suits durable intent, observable processing and reuse across clients. It
requires domain knowledge plus expertise in reconciliation, authorization, concurrency,
API evolution and failure recovery. Immediate transactions, complex private queries and
high-volume event storage may fit a conventional service better. The
[BFF decision guide](bff-choice.md) explains those tradeoffs and the different mechanisms
needed for one stored object, one accepted request and one external effect.

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
[sharing one domain](ingress.md#decision-2026-10-01-sharing-one-domain-with-other-services). The initial design uses one configured cluster per deployment and a configurable
OIDC provider. No particular identity provider or frontend framework is required.

krm-foyer terminates TLS itself from a mounted certificate, reloaded when it changes, or
serves plain HTTP behind an ingress that terminates TLS. Both models are supported. Its
public URL is configuration: the OIDC redirect URI, same-origin and CSRF checks and return
paths use it, never `Host` or `X-Forwarded-*`. An ingress in front must be transparent: no
buffering of streams, and no authentication or header rewriting of its own on krm-foyer's
routes. An ingress's external-authentication feature (`auth_request`, ForwardAuth) is used only as a
login gate for the application's pages, through `/auth/check`. It never decides on `/k8s` or
`/stream` traffic. The [ingress decision](ingress.md) explains why, and what would make
more worth revisiting.

krm-foyer has two halves. The **login half** (`/auth/...`) obtains the user's OIDC token
and keeps it in the server-side session. The **API half** (`/k8s`, `/stream`) takes the
user's token from one credential interface, applies the allowlist and sends the request
with that token, and the API server validates it. The API half's contract is the user's own
token, checked by Kubernetes. Login is a convenience behind that interface: it could
later accept tokens obtained elsewhere, or move into a separate program, without changing
the API half.

| Component or team | Responsibility |
| --- | --- |
| krm-foyer | OIDC client, sessions, CSRF protection, fixed upstream routing, API proxy and stream host configuration |
| krm-stream | Resource-stream protocol, recovery, projections, optional watch sharing and browser draft/reconciliation primitives |
| Kubernetes | Discovery, persistence, RBAC, admission execution, API validation and write concurrency |
| Domain team | Resource contracts, admission rules, controller processing, trusted status and domain guarantees |
| Frontend team | Forms, resource queries, save intent, navigation and presentation of domain outcomes |
| Platform team | Deployment, grants, exposure configuration, availability and upgrades |

Application-specific endpoints, DTO transformations, result aggregation and business plugins
are outside krm-foyer's scope. Domain services and operators own those responsibilities.
Use the existing krm-stream implementation for streaming and reconciliation.

## API contract

| Proposed route | Behavior |
| --- | --- |
| `/auth/login` | Start OIDC authorization-code login with PKCE, state and nonce |
| `/auth/callback` | Validate the callback and establish a session |
| `/auth/session` | Return minimal identity/session state and CSRF information, never bearer tokens |
| `/auth/logout` | CSRF-protected POST that destroys the server session |
| `/auth/check` | 204 or 401 (or 302 to login on request) for an ingress gating the application's pages; never a token or identity. See the [login gate](ingress.md#decision-2026-10-01-a-login-gate-for-the-applications-pages) |
| `/k8s/api/...` | Proxy core Kubernetes APIs after stripping `/k8s` |
| `/k8s/apis/...` | Proxy grouped APIs, including CRDs and aggregated APIs |
| `/k8s/api`, `/k8s/apis`, `/k8s/version`, `/k8s/openapi/...` | Proxy explicitly permitted discovery/schema endpoints |
| `/stream` | Serve krm-stream with configured scopes and projections |

For example, POSTing to
`/k8s/apis/workspaces.example.com/v1/namespaces/team-a/workspacerequests` creates a
resource through Kubernetes. The response retains Kubernetes' status code and object shape.
The route is available only when explicitly permitted by exposure policy and authorized
by Kubernetes.

Preserve bodies, Kubernetes `Status` errors, content types, relevant headers and query
parameters. Support pagination, selectors, native watches, CRUD, patch types, dry-run and
Server-Side Apply using their [native semantics](https://kubernetes.io/docs/reference/using-api/api-concepts/).
Clients choose concurrency preconditions and field ownership. The proxy must not force
apply, convert PATCH to PUT or automatically replay mutations after authentication recovery.
A lost response may already have committed a write.

The service has no hardcoded resource catalogue. Its initial transport scope covers ordinary
HTTP APIs, streaming logs and native HTTP watches. Exec, attach and port-forward require
separate upgrade-protocol support and tests; return an explicit unsupported error until
implemented. Service/node/pod proxy subresources require explicit opt-in. The browser
cannot supply an arbitrary upstream URL.

## Access boundaries

An operator-configured allowlist is mandatory from the first release. Empty configuration
exposes no Kubernetes APIs or stream scopes. Explicitly configure groups, versions,
resources, namespaces, verbs, subresources and permitted non-resource URLs. Distinguish
get/list/watch and collection deletion. Reject non-canonical paths (encoded slashes, dot
segments, repeated slashes) instead of normalizing them, forward exactly the path that was
checked, and reject unknown or ambiguous scopes. Discovery does not grant access.

Query parameters are part of policy evaluation. Recognize `watch=true` on a collection
path as watch access, not an ordinary list. Explicitly configure allowed label/field
selectors and enforce any required scope on every request, including requests with
`limit` and `continue`. Reject unsupported or ambiguous query combinations. Selectors
may narrow an authorized scope; user-controlled selectors cannot establish ownership.

Bound page sizes, response bytes, request rates, watch duration and concurrent streams.
Do not claim these bounds prevent export: a user allowed to list a namespace can normally
retrieve it through repeated pages. If full enumeration is unacceptable, narrow the
underlying readable resources or provide a restricted domain view.

Apply restrictions to raw APIs and streams. Kubernetes RBAC and admission remain authoritative;
the allowlist further limits which permissions browser clients can exercise through this service.
Never use a privileged service account as a fallback for a user's direct API request.

**Review exposure before enabling a route.** An existing application may enforce rules that
its users' Kubernetes grants do not express. A generic route can bypass those rules as soon
as it is available, even if the frontend still calls the old endpoint. Likewise, existing
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

An encrypted HttpOnly cookie can also keep tokens unreadable by JavaScript; the reason
for choosing opaque sessions is revocation and lifecycle control. Clearing a browser cookie
alone does not invalidate a copied stateless session. Revoking a server session does not
revoke independently issued Kubernetes credentials or undo accepted writes. The BFF pattern
is described in [OAuth 2.0 for Browser-Based Applications](https://www.rfc-editor.org/rfc/rfc10017.html).

Use a maintained OIDC library. Bind login transactions to the browser; validate issuer,
audience and callback state; accept only validated local return paths. Rotate session IDs
at login, enforce idle/absolute expiry and serialize supported refreshes per session. Reject
requests when session validity cannot be established. The configured issuer must provide a
credential accepted by the cluster; an arbitrary OIDC login or access token is insufficient.

Unauthenticated API requests return a documented 401 login-required response, not an HTML
redirect. An optional framework-independent helper handles login navigation and return paths.
Expose authentication-required state before navigation so an editor can offer draft copy-out
or an explicitly designed preservation flow. A 403 represents permission denial, not a login
loop. Bound refresh attempts and report persistent issuer/cluster configuration errors.

Require CSRF proof and same-origin checks for mutations and logout. Strip browser-supplied
Authorization, impersonation and untrusted forwarding headers; never forward the session
cookie to Kubernetes. Pin upstream destinations, verify TLS, bound request/stream resources
and avoid credential/body logging. HttpOnly does not prevent malicious same-origin JavaScript
from making requests as the user.

## Streams and editing

krm-foyer supplies krm-stream's principal resolution, authorization, backend selection and
scope configuration. Start with user-authenticated watches. Optional shared watches use a
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
| Exposure | Empty policy denies access; paths, selectors, watch queries, pagination, alternate versions and subresources cannot bypass restrictions |
| Proxy semantics | Kubernetes errors and patch types preserved; conflicting writes and ambiguous create outcomes handled without automatic replay |
| Streams | Cancellation, recovery, expiry, subscriber isolation and measured load under declared capacity targets |
| Editing | Conditional saves and guarded reconciliation preserve newer state and drafts |
| Example domain | Pending, accepted, rejected and failed processing demonstrated; protected status and restart/duplicate tests prove its declared guarantees |
| Operations | Owned session storage, deployment/upgrade procedures, telemetry and tested failure behavior |

The domain operator's tests establish domain guarantees; the gateway's tests establish
transport and access behavior. Publish the service with its own integration fixture,
versioned image and documented supported protocols.

## Status and next decision

The [naming assessment](name.md) records why the service is called krm-foyer, and the
[frontend decision](frontend.md) records which pages the service itself serves.

The service is a design proposal. This repository holds a server skeleton with health
endpoints and a start page, and an [e2e fixture](testing.md) whose API server trusts a real
Dex issuer; none of the routes above are implemented yet. The next decision is whether to fund an independent
prototype and assign its maintainers. Product and platform leads should agree on one
example domain, supported protocols, session-store ownership and measurable load targets.

Build the prototype in this order:

1. Implement OIDC sessions and the default-deny native API proxy in an isolated fixture.
2. Add krm-stream and demonstrate the workspace request lifecycle, including rejection,
   uncertain create outcomes, controller recovery and session expiry.
3. Integrate a second frontend/API group without domain-specific gateway handlers, then
   measure the full operational cost against keeping authentication/transport in each app.

Maintainers assess the release criteria with domain and platform owners before publishing
an initial release. Each consuming application then makes its own adoption decision using
the [decision guide](bff-choice.md#record-the-adoption-decision). Prototype approval does
not authorize routing an existing application's resources through the new service.
