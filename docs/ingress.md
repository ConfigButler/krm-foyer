# Running behind an ingress, and delegating login

Two questions about how krm-foyer meets the network. Should it terminate TLS itself, or
sit behind an ingress? And should it work with an ingress's external-authentication
feature, such as nginx's `auth_request` or Traefik's ForwardAuth, either by answering
those checks or by letting the ingress run the OIDC login? The [service design](design.md)
records the result; this document records the reasons.

## Decision (2026-10-01): both TLS models

krm-foyer supports two ways of being deployed. They are **not** equally safe:

| Model | How | When | What it assumes |
| --- | --- | --- | --- |
| **krm-foyer terminates TLS** | Certificate and key are mounted files, for example from cert-manager. krm-foyer reloads them when they change, so rotation needs no restart | The default | Nothing beyond the usual: no hop carries a session cookie in plain text |
| **Behind an ingress** | The ingress or gateway terminates the browser's TLS and forwards to krm-foyer | An organization that routes everything through one ingress, or a team that already manages certificates there | The ingress is trusted, and the hop behind it is protected (see below) |

Behind an ingress, every request on the hop to krm-foyer carries the session cookie, and
the [session ID is a bearer credential](design.md#login-and-sessions). `Secure` only
protects the browser's own connection; it says nothing about the hops after it. So that
hop is one of two things:

- **Re-encrypted and verified.** The ingress connects to krm-foyer over TLS and checks
  its certificate against a CA it is configured with. This is the choice wherever the
  cluster network is shared with workloads you do not fully trust.
- **Plain HTTP on a network where only the ingress can reach krm-foyer.** A
  NetworkPolicy admits the ingress's pods to krm-foyer's port and nothing else, and the
  cluster network is not observable by other tenants. Anything that can read that hop
  can take a session. Operators choosing this accept that assumption explicitly; the
  chart's default is the policy, not an open port.

What both models do share is that **krm-foyer never works out its own public address
from the request.** It is configured with its public URL. That URL is the
OIDC redirect URI, the origin that CSRF and same-origin checks compare against, and the
base for return paths. `Host` and `X-Forwarded-*` headers do not change any of those, so
a spoofed header cannot poison a redirect or the CSRF origin. That protects against
header tricks; it does nothing for a cookie read off an unencrypted hop. Forwarded client
addresses are used only for logging and rate limiting, and only when they come from proxy
addresses the operator configured.

Session cookies are always `Secure`, in both models. Behind an ingress the browser still
sees HTTPS, and browsers accept `Secure` cookies from `http://localhost`, so local
development needs no exception.

An ingress in front of krm-foyer has to stay transparent:

- **No response buffering** on `/k8s` watches and `/stream`, and read timeouts longer
  than a watch. krm-foyer sends `X-Accel-Buffering: no` on streaming responses, which
  nginx honors. Other proxies need their own setting.
- **No authentication of its own** on krm-foyer's routes. The ingress may limit request
  sizes and rates, but it must not add or rewrite `Authorization` or `Impersonate-*`
  headers.
- **HTTP/2 to the browser.** Over HTTP/1.1, a browser opens at most six connections per
  origin, and long-lived watches use them up. Both models give the browser HTTP/2 once
  TLS is in place.

The e2e fixture runs krm-foyer with its own TLS, because that is the default. The
ingress model gets one e2e spec with an nginx container in front of krm-foyer. That spec
checks two things: a spoofed `Host` or `X-Forwarded-Host` changes neither the redirect
URI nor the CSRF origin, and watch events arrive without delay.

## Decision (2026-10-01): sharing one domain with other services

One domain normally serves several things: the application's frontend, krm-foyer, and
often a domain backend. **The ingress or gateway routes by path. krm-foyer owns four
prefixes and nothing else.** It does not host the application's files, and it never
forwards traffic that is not its own.

| Path | Routed to |
| --- | --- |
| `/auth/`, `/k8s/`, `/stream`, `/_foyer/` | krm-foyer |
| `/api/` (for example) | The application's domain backend, if it has one |
| `/` and everything else | The frontend's static file server or CDN |

With Gateway API, that is one `HTTPRoute`:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: workspaces
spec:
  parentRefs: [{ name: public }]
  hostnames: [workspaces.example.com]
  rules:
    - matches:
        - path: { type: PathPrefix, value: /auth }
        - path: { type: PathPrefix, value: /k8s }
        - path: { type: PathPrefix, value: /stream }
        - path: { type: PathPrefix, value: /_foyer }
      backendRefs: [{ name: krm-foyer, port: 8443 }]
    - matches:
        - path: { type: PathPrefix, value: /api }
      backendRefs: [{ name: workspaces-api, port: 8080 }]
    - backendRefs: [{ name: workspaces-frontend, port: 8080 }]
```

During development, the frontend's dev server plays the ingress's role: Vite's
`server.proxy`, for example, sends the four prefixes to a krm-foyer on a local cluster,
so the browser still sees one origin. The e2e fixture and the examples use a small nginx
container the same way.

Sending everything through krm-foyer would be wrong for several reasons:

- **Blast radius.** A routing bug in krm-foyer can then only affect krm-foyer's routes.
- **Resource bounds.** Its limits are sized for API calls and streams, not for static
  assets.
- **Scaling.** Each service scales on its own.
- **Size.** It keeps the binary small: no static hosting, no single-page-app fallback,
  no catch-all proxy.

Paths outside its prefixes get a 404 from krm-foyer, which a unit test pins.

### Why routing by path is safe when forward-auth is not

Design A below is rejected because two programs parse the same path differently. Path
routing also has two parsers, the ingress and krm-foyer, so it is fair to ask why it is
acceptable. The difference is which program does what:

- **In forward-auth, the ingress forwards to the API server.** krm-foyer checks the path
  the ingress *reports*, and the API server receives the path the ingress *sends*. If the
  two differ, the request that runs is not the request krm-foyer checked, and nothing in
  krm-foyer can see that it happened.
- **In path routing, the ingress only picks a service.** krm-foyer checks the path it
  receives and sends that same path on. If the ingress parses a path differently,
  the request reaches the wrong service: a krm-foyer path lands at the frontend, or an
  application path lands at krm-foyer and gets a 404. That is a broken page, not access.
  The ingress never talks to the API server, so nothing it does can skip a check.

The rule is: **the code that checks a request is the code that forwards it.** Path routing
keeps that rule and forward-auth breaks it, so the parser argument still applies. It
now applies inside krm-foyer: Go keeps a decoded path (`URL.Path`) and an escaped one
(`URL.RawPath`), and a check on one followed by forwarding the other is the same
bug in a single binary. So krm-foyer:

- **rejects non-canonical paths instead of normalizing them:** encoded slashes, dot
  segments, repeated slashes, and percent-encoding of characters that need none. Real
  Kubernetes clients never send these, because resource names cannot contain them.
  Rejecting removes the question of which normalization is right.
- **builds the upstream URL from the path it checked,** never from the incoming request's
  raw form. A fuzz test asserts that the path forwarded is byte-for-byte the path checked.

Two routing details still matter, for availability:

- **Match whole path segments.** Gateway API's `PathPrefix` does this: `/k8s` matches
  `/k8s/api` but not `/k8sfoo`. An nginx `location /k8s` also matches `/k8sfoo`; write
  `location /k8s/` instead. An application route such as `/streams` must not be caught by
  krm-foyer's `/stream`.
- **The ingress may rewrite the path before krm-foyer sees it.** For example, nginx decodes and
  normalizes it when `proxy_pass` carries a URI. That is safe, because krm-foyer checks
  whatever arrives. At worst, a request that should have worked gets a 400.

### What a shared origin costs

One domain is convenient for the browser, with no CORS and one cookie jar. It also makes
everything on it **one trust boundary**:

- **Any service on the origin can act as the signed-in user.** It can serve JavaScript
  that reads the CSRF token from `/auth/session` and calls `/k8s`, exactly as the
  application does. Put only services on the origin that you would trust with the user's
  Kubernetes access. Anything else belongs on its own host name: the session cookie is
  host-only, so it never reaches a subdomain.
- **The session cookie reaches every service on the host.** The `__Host-` prefix requires
  `Path=/`, so the browser sends the cookie with every request. The ID is opaque, but it
  is a bearer credential: anyone who holds it can call krm-foyer as the user, from
  anywhere, until the session ends. A co-hosted service that logs `Cookie` headers hands
  sessions to whoever reads those logs. Co-hosted services must not log or keep it.
- **The prefixes are fixed.** An application that already uses `/auth` has to move it.
  Make them configurable when an adopter needs it, not before.

### How a domain backend knows the user

Choose in this order:

1. **Need no backend endpoint.** Model the command as a resource that an operator
   processes, and the read view as `status` (see the [decision guide](bff-choice.md)).
   Voter did this with the quiz tally: it moved into `QuizSession.status`, and the results
   endpoint became a watch.
2. **The backend logs in by itself.** It is its own OIDC client, with its own session
   cookie under a different name. The identity provider remembers the user, so the second
   sign-in is a redirect without a prompt. If the backend calls Kubernetes as the user, as
   Voter's `/public/*` handlers do today, it uses its own token for that user. The cluster
   accepts the token once the backend's client ID is added to the issuer's audiences.
   Nobody shares or forwards credentials. The cost is two sessions, so logout has to end
   both.
3. **Later: krm-foyer answers an identity check** for the backend (option C below),
   identity only. This avoids the second session, but the backend gets no token.

krm-foyer never hands its tokens to another service. That would spread custody of the
credential to code that krm-foyer does not test.

## Decision (2026-10-01): a login gate for the application's pages

The ingress can ask krm-foyer one question before serving the application's pages: *is
this browser signed in?* If it is not, the browser goes to the login, and comes back to
the page it asked for. Every frontend needs this, and it is wasteful to write it in each
one. It is a narrow use of forward-auth, and it is not the design rejected below.

**`GET /auth/check`** answers:

- `204` when the session is valid;
- `401` when it is not;
- `302` to `/auth/login?return_to=<page>` when it is not, if the request asks for it with
  `?redirect=true`.

It never returns a token or identity headers. The page the user wanted arrives in the
ingress's original-URI header (`X-Original-URI` in nginx, `X-Forwarded-Uri` in Traefik).
It is validated as a local path exactly like `return_to`, and anything else becomes `/`.

Ingresses differ in what they do with a refusal, which is why there are two refusals:

- **nginx `auth_request`** accepts only 2xx, 401 and 403 from the check. It turns the 401
  into the redirect itself, with `error_page 401 = @login`.
- **Traefik ForwardAuth and Envoy's external authorization** pass the check's response to
  the browser, so they use `?redirect=true`.

Gateway API has no standard external-auth filter yet. Without one, the browser helper's
`requireSession()` does the same at page load, in a few lines of JavaScript. The gate is
an improvement for whoever has it, not a requirement.

How to deploy it:

- **Gate page routes only.** JavaScript bundles and images are public by nature; gating
  them only turns an expired session into broken asset loads.
- **Never gate krm-foyer's own prefixes.** `/auth/` would loop. `/k8s` and `/stream` check
  the session themselves and answer a JSON 401.

**The gate is about experience, not security.** A frontend's files contain no data. What
protects the data is the API server checking the user's token on every `/k8s` and
`/stream` request. A missing or misconfigured gate only shows a page to a signed-out
browser, whose API calls then get 401. That is why the gate does not break
the [one-parser rule](#why-routing-by-path-is-safe-when-forward-auth-is-not): it grants
nothing, so there is no decision for two parsers to disagree on.

It does not remove all frontend code either. A session can end while a page is open: the
idle timeout runs out, or a refresh fails. Then `/k8s` answers 401 and the application
decides what happens next. An editor first offers to keep the user's draft, as
[Login and sessions](design.md#login-and-sessions) requires, and then calls
`login(returnTo)` from the helper. The gate handles arriving signed out; the helper
handles being signed out midway.

`/auth/check` belongs to the login side of krm-foyer. If login ever moves into a
separate program, the check moves with it.

## Other external-authentication designs

"Support nginx `auth_request`" can also mean three broader designs. They have very
different consequences, so this section takes them one at a time.

### A. The ingress asks krm-foyer, then sends `/k8s` traffic to the API server itself

The ingress sends each request to an auth endpoint on krm-foyer. krm-foyer checks the
session and answers 200 with the user's token in a response header.
The ingress then forwards the request to the API server with that token.

**Do not build this.** It splits the boundary that krm-foyer exists to keep in one place:

- **The check and the request are parsed by two different programs.** krm-foyer checks
  the path the ingress *reports*; the API server receives the path the ingress
  *forwards*. Repeated slashes, `%2F` and `..` are handled differently by nginx, Traefik,
  Envoy and Go. Today that only undermines path hygiene; with an
  [application scope](application-scope.md) it would be a bypass, and it would sit in
  ingress configuration that krm-foyer cannot test. [Access](design.md#access)
  requires forwarding exactly the path that was checked, and that only works when one
  program does both.
- **The token leaves krm-foyer.** It travels in a response header to the ingress, where
  access logs and debug settings can record it.
- **Streams do not fit.** `/stream` needs the principal inside krm-stream, not a yes or
  no in front of it.
- **Every ingress is different.** nginx, Traefik, Envoy and Gateway API each have their
  own external-auth configuration, and each would need its own tests. Gateway API has no
  standard for it yet, and the Kubernetes project has retired ingress-nginx, the most
  common place `auth_request` was configured.
- **We have already tried this.** Voter's first design (March 2026) was Traefik
  ForwardAuth deciding from a cookie Voter issued, followed by impersonation. It was
  removed because the application ended up writing the identity the audit log recorded.
  Design A avoids impersonation, but it keeps the other half of that mistake: the
  component that makes the decision is not the one that enforces it.

### B. The ingress runs the OIDC login and passes the user's token to krm-foyer

An auth proxy such as oauth2-proxy logs the user in, keeps the session and adds
`Authorization: Bearer <id token>` to every request. krm-foyer has no login of its own.
It proxies with the token it received.

This one is a reasonable design. **It does not break the central claim.** The token is
the user's own, the API server checks it, and nobody impersonates anyone. Organizations
with a standard SSO proxy will ask for it. The costs are real, though:

- **Most of [Login and sessions](design.md#login-and-sessions) moves out of krm-foyer
  and out of its tests:**
  - per-session revocation and logout that closes the session's streams;
  - refresh serialization;
  - JSON 401 responses instead of redirects (oauth2-proxy redirects unless configured
    otherwise);
  - CSRF tokens.

  The proxy turns a cookie into a header, so a cross-site request carries the user's
  credential, and only `SameSite` stands in the way.
- **krm-foyer would have to accept an `Authorization` header** that the design tells it
  to strip. It cannot tell a header the proxy set from one the browser sent, unless the
  proxy always overwrites it. That is one more piece of configuration that has to be
  right.
- **Twice the authentication setups to test,** before there is a first adopter. Voter's
  adoption review pointed out that no second consuming application exists yet.
- **It hollows the product out.** An auth proxy in front of a plain proxy is
  roughly what teams build today. Done properly, login is where most of krm-foyer's value
  is.

So: **not now, but keep it possible.** The code takes the user's credential from exactly
one place, the session, through one small interface. A later "external login" mode would
add a second source behind that interface, without touching the proxy or streams.

Revisit this when an adopter already has an auth proxy they cannot give up. That mode
would be:

- off by default;
- accept the token only from a configured header, only from configured proxy addresses;
- require an `Origin` check and a custom request header on every mutation;
- document which guarantees from the design it no longer provides.

### C. krm-foyer tells a domain backend who the user is

This is the login gate, plus identity. For a hybrid application's own domain endpoints, the
check also answers with identity headers (user, groups), and the backend trusts them.

This fits the [hybrid option](bff-choice.md) in the decision guide, and it is small: one
handler, with no change to `/k8s`. It still creates a contract of its own:

- The receiving backend has to trust identity headers. The ingress must strip those
  headers from browser requests, and only the ingress may reach that backend.
  - Voter's Room Pass relied on exactly this kind of header trust. Keeping it safe took
    a NetworkPolicy and validation rules on the API server to contain forged headers.
- If the token were included, every such backend would hold Kubernetes credentials.
  krm-foyer would then share custody of the credential with services it does not test.

**Later, and identity only.** Build it when a hybrid application asks, on top of
`/auth/check`, and without the token.
A domain backend that needs to act on Kubernetes as the user is a separate design
question.

## Is this too opinionated?

The other direction is the risk. krm-foyer's opinion is that **login and the proxy are
one unit of trust**: the code that checks a request is the code that sends
it, with a credential it has kept itself. That is what keeps the service small enough to
test completely, and it is what the e2e suite can prove. Supporting every combination of ingress and
auth proxy would make krm-foyer less opinionated, much larger, and impossible to test
across all of those combinations.

The flexibility people need is in the network, not in who decides. Both TLS models are
supported, a transparent ingress is fine, the ingress can gate pages on the login, and
the seam for external login exists if it is ever needed.
