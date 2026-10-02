# Which pages krm-foyer serves itself

Decision (2026-09-30): krm-foyer ships a **small set of server-rendered pages**, embedded
in the Go binary. It does not ship a single-page application, a resource browser or a
Node build. Getting started is covered by the start page and by examples that live next
to the service, not inside it.

Amended (2026-10-01): krm-foyer does not host the application's own files either. The
ingress routes `/` to the application and krm-foyer's prefixes to krm-foyer; see
[sharing one domain](ingress.md#decision-2026-10-01-sharing-one-domain-with-other-services).

## The problem

krm-foyer is a backend. Its [API contract](design.md#api-contract) answers JSON to
JavaScript: an unauthenticated API request gets a 401, not a redirect. That is right for
applications, but a person who has just deployed the service and opens it in a browser
needs more than a 404 to confirm it works. The login flow also has steps where the
browser is on a krm-foyer URL and no application page exists yet:

- The OIDC provider sends the browser back to `/auth/callback`. When that callback fails,
  something has to explain why.
- After `POST /auth/logout`, the browser needs somewhere to go.
- Before an application is configured, `/` has nothing to serve.

## What ships in the binary

| Path | Page | Why it is ours |
| --- | --- | --- |
| `/` | Start page, for requests that reach krm-foyer itself: a port-forward, or before routing is set up | Confirms the deployment works and shows what to route next. It links to the docs and to the session page. Behind an ingress, `/` belongs to the application and this page is never seen. |
| `/auth/login?return_to=/path` | No page: a redirect to the OIDC provider | The return path is validated as a local path before it is stored in the login transaction. |
| `/auth/callback` | A redirect to the stored return path on success. On failure, an error page with a stable reason and a "try again" link | An application cannot render this: its code is not loaded yet. |
| `/auth/logged-out` | Plain confirmation with a "sign in again" link, and a note that the issuer may still have the user signed in | Where logout lands when the application does not supply its own destination. `POST /auth/logout` answers `204`, and the caller navigates here or to its own page. |
| `/auth/whoami` | Who Kubernetes takes you to be (username, groups and extra, from a SelfSubjectReview), with the session's issuer and expiry. Never tokens | The first thing to check when Kubernetes answers 403. It shows the API server's view, not the token's claims, because that is what RBAC matches. `/auth/session` stays the JSON form for code. |
| `/_foyer/access` | What you may do in a namespace, from a SelfSubjectRulesReview, and a "can I?" form answered by a SelfSubjectAccessReview | `kubectl auth can-i` for someone with only a browser. Code asks the same reviews natively. See [what may I do](design.md#what-may-i-do). |
| `/k8s/...`, `/stream` when krm-foyer interrupts | For a browser navigation only: sign in, redirect notice, held-back content, store unavailable. Same status code as the JSON form | The proxy is explorable in a tab. These say what krm-foyer decided, as opposed to what Kubernetes answered. See [interruptions](design.md#interruptions). |
| `/healthz`, `/readyz` | Plain text | For probes, not people. |
| `/_foyer/...` | The pages' stylesheet and images | The one prefix krm-foyer reserves for its own pages and files that are not part of login, so they never collide with an application on the same origin. |

Rules for these pages:

- **Go `html/template` and `embed`, no JavaScript build.** The pages are static apart from
  a few values, so a Node toolchain would cost more than the pages are worth. It would also
  add an npm dependency tree to a security-sensitive binary.
- **No inline script and a strict Content-Security-Policy.** The service that holds session
  cookies should be the easiest one to audit.
- **Every page is replaceable.** Operators can point the logged-out and error destinations
  at their own application URLs. The built-in pages are defaults, not branding.
- **Nothing reveals configuration to anonymous visitors.** The start page says which features
  are switched on, never issuer secrets or cluster addresses.

## What deliberately does not ship

- **A resource browser or dashboard.** Raw API answers are viewable in a tab, as JSON;
  there is no UI on top of them. A dashboard would compete with the applications krm-foyer
  exists to serve, and name.md picked "foyer" partly so the name would not suggest one.
- **A login page with its own form.** Login happens at the OIDC provider. krm-foyer only
  redirects to it.
- **A frontend framework or component library.** Applications choose their own.

## Getting started quickly

The pages above make a deployment verifiable. What makes it easy to *build on* is
separate:

1. **Routing recipes, not hosting.** A Gateway API `HTTPRoute`, an nginx server block and
   a Vite dev-server proxy that put the application and krm-foyer on one origin. These are
   documentation and examples, not code in krm-foyer.
2. **A tiny browser helper** at `/_foyer/foyer.js`, served by krm-foyer as a plain ES
   module ([source](../internal/pages/assets/foyer.js)). It has five calls: `session()`,
   `login(returnTo)`, `logout(next)`, `requireSession()` and `k8s(path, options)`. The
   last puts the CSRF header on every change and reports each answer as an outcome
   (`ok`, `signed-out`, `refused`, `missing`, `conflict`, `invalid`, `error`) with the
   `Status` message, without retrying anything Kubernetes answered. Its one resend is of
   a change krm-foyer refused for a stale CSRF token, which never reached Kubernetes.
   `requireSession()` sends a signed-out page to login, for deployments whose ingress
   cannot run the [login gate](ingress.md#decision-2026-10-01-a-login-gate-for-the-applications-pages).
   Every application needs these, and they are easy to get subtly wrong. There is no npm
   package until someone needs one outside a krm-foyer origin.
3. **[`examples/hello/`](../examples/hello)** (exists): one HTML file, one script and a
   stylesheet, with no bundler and no backend of its own. It signs in, lists, creates and
   edits Notes (a small custom resource) through `/k8s`, and shows Kubernetes' 403 and 409
   answers as they are. An nginx front door serves it at `/` and sends krm-foyer's
   prefixes to krm-foyer, on one origin. `task demo` starts it in a disposable cluster,
   and the e2e suite's browser specs drive it in Chromium. It reloads on request; following
   changes live through `/stream` comes with streams.

The start page and the auth pages came first, because the OIDC work needed them anyway.
The routing recipes come with the ingress work.

## Revisit when

- Two adopters write the same page themselves. That is a sign it belongs in the service.
- The helper grows past the session and the proof every request needs. Then it belongs
  in its own package, or in krm-stream.
