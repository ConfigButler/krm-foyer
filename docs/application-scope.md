# Application scope

Should krm-foyer limit what a browser application can do with a user's Kubernetes access,
beyond what RBAC already limits? This document explains why that question exists, what
an answer would block, and how it could be built.

**Decision (2026-10-01): not now.** krm-foyer starts without any scope layer. Kubernetes
alone decides what a request may do. This document records the reasoning, so the
question can be picked up again when an adopter needs it (see [revisit when](#revisit-when)).

## Why RBAC alone is not the whole answer

RBAC answers "what may alice do?", and it answers for every client alice uses: `kubectl`,
CI, and now a web application. It has no notion of "alice, through the coffee-menu
application". Scope is not about restricting the user. A user who wants to do more can
already use `kubectl`. It is about restricting **what code running in the browser can do
with the user's credential**.

That matters because a browser session is *ambient*. A `kubectl` token is used only by
the person holding it. A session cookie is used by whatever JavaScript runs on the page:
the application's own code, every dependency in its bundle, and anything injected into
it. CSRF protection stops other sites; it does nothing against script on the origin
itself.

OAuth has a name for this: *scopes*. RBAC says what the user may do; a scope says what
this application may do on the user's behalf. Kubernetes has no scopes. A token's
audience is all or nothing: the cluster accepts it or it does not.

## What a scope would block

Take alice, who is namespace admin for her day job, using a small coffee-menu
application. Without a scope, her session there carries her full Kubernetes access:

| Attack | Without a scope | With a scope of coffeeconfigs only |
| --- | --- | --- |
| Cross-site scripting in the application, or a compromised npm dependency in its bundle | Reads every Secret in the namespace, creates a RoleBinding or a privileged pod | Edits coffee menus |
| A careless or compromised service on the shared origin, which is [one trust boundary](ingress.md#what-a-shared-origin-costs) | The same | The same limit |
| A stolen session ID, which is a [bearer credential](design.md#login-and-sessions) | Everything alice can do with `kubectl`, until the session ends | Only the application's resources |
| `pods/exec`, `nodes/proxy`, `services/proxy` | Refused as unsupported for now; once supported, reachable for anyone whose RBAC grants them | Never reachable unless listed |
| An administrator signing in to a small application | The application becomes a cluster console | The application stays a coffee-menu editor |

Every row is about limiting the damage when the frontend goes wrong. None of them is
about a user getting around RBAC.

**Without a scope, deploy krm-foyer for users whose Kubernetes grants match what the
application needs.** That is the cost of the current decision, and the
[design](design.md#access) says so.

## Two ways to build it

### A scope list in krm-foyer

krm-foyer reads a list of what the application is for, and refuses everything else
before it reaches Kubernetes:

```yaml
scope:
  - group: workspaces.example.com
    version: v1
    resource: workspacerequests
    namespaces: [team-a, team-b]
    verbs: [get, list, watch, create]
  - group: ""                 # the core API
    version: v1
    resource: pods
    subresources: [log]       # subresources are off unless listed
    namespaces: [team-a]
    verbs: [get]
nonResourceURLs: [/version]
```

It must stay coarse: resource, verb, namespace and subresource. Anything finer is
Kubernetes' job and would only duplicate it:

- **Object names** are RBAC's `resourceNames`.
- **Label and field selectors** cannot prove ownership; a user chooses them. Ownership
  belongs to the domain's RBAC and admission.

What it has to get right:

- **Default deny.** An empty list exposes nothing. A list can only take away what RBAC
  grants, never add to it.
- **One parser.** The path krm-foyer checks is byte-for-byte the path it forwards, and
  non-canonical paths are rejected rather than normalized. See the
  [one-parser rule](ingress.md#why-routing-by-path-is-safe-when-forward-auth-is-not).
- **Verbs from the request, not the method alone.** `watch=true` on a collection is
  watch, not list; deleting a collection is not deleting an object.
- **Alternate versions are separate entries.** `v1` does not cover `v1beta1`.
- **Discovery grants nothing.**
- **The same list scopes streams**, so there is one source of truth.

It works on any cluster, including managed ones. The cost is a second list next to RBAC,
which someone has to keep in step with the application.

### A browser identity in Kubernetes

The API server's `AuthenticationConfiguration` can map tokens for the `krm-foyer`
audience to their own identity: a username such as `browser:alice@example.com`, or a
group. RBAC then grants that identity only what the application needs. Kubernetes
enforces the scope itself, and the audit log records it.

This is the purest form of "Kubernetes decides". Its costs:

- **RBAC twice.** Each grant is written for `alice` and for `browser:alice`, or through a
  group for each.
- **Domain logic that matches on identity breaks.** Admission rules and owner fields that
  expect `alice` see `browser:alice`.
- **It needs control of the API server's authentication configuration.** Managed
  clusters often do not allow that, so it cannot be the only answer.

The e2e fixture already configures that authenticator, so it can show this working
without any krm-foyer code.

## How the rest of the design would change

With a scope layer, three things would come back:

- A 403 [interruption](design.md#interruptions) for "not in scope", as distinct from
  Kubernetes refusing.
- A third state on the [what may I do](design.md#what-may-i-do) page: allowed, refused
  by Kubernetes, or out of scope.
- Unit and fuzz tests for the matcher, and e2e specs showing that an empty scope exposes
  nothing even to a cluster administrator.

## Revisit when

- An adopter's users have grants much broader than the application needs, as most
  administrators and developers do.
- An application loads third-party script, or shares its origin with services that are
  not fully trusted.
- A security review asks what a cross-site-scripting bug in a frontend could reach.

When that happens, document the browser identity first, since it needs no krm-foyer
code, and build the scope list for clusters where that is not possible.
