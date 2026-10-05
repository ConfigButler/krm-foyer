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

**Documented and proved (2026-10-05).** This is the first step the decision names, and
it needs no krm-foyer code. Voter's operator asked for it
([implementer feedback](implementer-feedback.md), entry 4): they sign in as a
cluster-admin, and through krm-foyer every script on Voter's origin would hold that.

The API server's `AuthenticationConfiguration` can give one person two usernames, told
apart by the OIDC client the issuer gave the token to: krm-foyer's tokens one name, the
command line's another. RBAC then grants the browser's name only what the application
needs, and keeps the broad grants on the other. Kubernetes enforces the scope itself,
and the audit log records which name acted.

The client is the token's **authorized party**: `azp` when the issuer sets it, which
Dex does when a client asks for another audience with the cross-client scope
`audience:server:client_id:<id>`, and the audience otherwise. So the same expression
works for a cluster that accepts krm-foyer's own audience, as the e2e fixture does, and
for one where krm-foyer asks for the cluster's audience, as Voter's does:

```yaml
apiVersion: apiserver.config.k8s.io/v1
kind: AuthenticationConfiguration
jwt:
  - issuer:
      url: https://dex.example.com
      # Both clients' tokens are accepted. With the cross-client scope, both carry
      # the audience kubernetes, and krm-foyer's carries azp: krm-foyer.
      audiences: [kubernetes]
    claimMappings:
      username:
        # The browser's name: foyer:<email>. The command line keeps github:<email>,
        # and with it the cluster-admin binding.
        expression: "((has(claims.azp) ? claims.azp : claims.aud) == 'krm-foyer' ? 'foyer:' : 'github:') + claims.email"
      groups:
        # The same split for groups, so that no group binding reaches both.
        expression: "has(claims.groups) ? dyn(claims.groups).map(g, ((has(claims.azp) ? claims.azp : claims.aud) == 'krm-foyer' ? 'foyer:' : 'github:') + g) : []"
    claimValidationRules:
      # A username from email is only an identity if the issuer vouched for it.
      - expression: "has(claims.email_verified) && type(claims.email_verified) == bool && claims.email_verified"
        message: "email_verified must be the boolean true"
```

Then bind the browser's name narrowly: `foyer:simonkoudijs@gmail.com` gets a Role over
the application's resources, and `github:simonkoudijs@gmail.com` keeps `cluster-admin`.

Which side gets the new name is a choice. Give it to whichever has fewer grants to
rewrite: in Voter's cluster that is the browser, since the existing bindings are for the
command line. The e2e fixture does the reverse, so that its many specs keep their
names: krm-foyer's tokens are `oidc:<email>` and the command line's are
`kubectl:<email>` ([authentication-config.yaml](../test/e2e/cluster/authentication-config.yaml),
with a `kubectl` client in [dex.yaml](../test/e2e/cluster/dex.yaml)). Its spec
([foyer_scope_test.go](../test/e2e/foyer_scope_test.go)) signs alice in both ways,
checks the API server's two names for her, grants the command-line name `list` on
Secrets, and shows krm-foyer refused with the API server's 403 for `oidc:alice`.

What to know before using it:

- **An issuer that sends a list of audiences and no `azp` fails the expression,** and
  the API server refuses the token. That fails closed. If your issuer does that, compare
  with `in` instead.
- **RBAC twice.** Each grant the person needs in both places is written for both names,
  or through a group for each.
- **Domain logic that matches on identity sees the browser's name.** Admission rules and
  owner fields that expect `github:alice` see `foyer:alice`. Attribution through the
  [extras](design.md#attribution) is unaffected, since those come from the same claims.
- **It needs control of the API server's authentication configuration.** Managed
  clusters often do not allow that, so it cannot be the only answer. That is what the
  scope list above is for.
- **It scopes by client, not by application.** Two applications behind one krm-foyer
  client share the browser's name. Give each its own krm-foyer and client if their
  grants should differ.

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

The browser identity is now documented and proved, above. Build the scope list when an
adopter's cluster cannot change its authentication configuration.
