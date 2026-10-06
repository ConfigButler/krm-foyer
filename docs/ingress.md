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
| **krm-foyer terminates TLS** | Certificate and key are mounted files, for example from cert-manager. Currently a restart loads a changed certificate; automatic reload is planned | Used by the e2e fixture | Nothing beyond the usual: no hop carries a session cookie in plain text |
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
  can take a session. Operators choosing this accept that assumption explicitly: the
  chart renders that policy by default, and refuses to install plain HTTP until
  `networkPolicy.from` names the ingress's pods. Whether the cluster's network plugin
  enforces it is still the operator's to check.

What both models do share is that **krm-foyer never works out its own public address
from the request.** It is configured with its public URL. That URL is the
OIDC redirect URI, the origin that CSRF and same-origin checks compare against, and the
base for return paths. `Host` and `X-Forwarded-*` headers do not change any of those, so
a spoofed header cannot poison a redirect or the CSRF origin. That protects against
header tricks; it does nothing for a cookie read off an unencrypted hop. Forwarded client
addresses are not used for logging or rate limiting. Current rate limits are keyed by
session, and there is no trusted-proxy-address configuration.

Session cookies are always `Secure`, in both models. Behind an ingress the browser still
sees HTTPS. Login configuration requires an HTTPS public URL, including locally;
`task demo` supplies TLS for the browser. Plain `task run` serves only the start page,
assets and probes.

An ingress in front of krm-foyer has to stay transparent:

- **No response buffering** on `/k8s` watches and `/stream`, and read timeouts longer
  than a watch. krm-stream sends `X-Accel-Buffering: no` on `/stream/v1`; the `/k8s`
  proxy flushes every chunk but does not emit that header. Configure buffering off
  for both routes in the ingress rather than depending on a response header.
- **No authentication of its own** on krm-foyer's routes. The ingress may limit request
  sizes and rates, but it must not add or rewrite `Authorization` or `Impersonate-*`
  headers.
- **HTTP/2 to the browser.** Over HTTP/1.1, a browser opens at most six connections per
  origin, and long-lived watches use them up. Both models give the browser HTTP/2 once
  TLS is in place.

The e2e fixture runs krm-foyer with its own TLS, because that is the default, and its
browser specs already reach krm-foyer through a gateway (Traefik, re-encrypting). The
dedicated ingress specs remain pending: spoofed `Host` or `X-Forwarded-Host` must
change neither the redirect URI nor the CSRF origin, and watch events must arrive
without buffering. The existing browser journeys exercise routing and live updates,
but do not replace those targeted checks.

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
    # Static files need no cookie, so the session's never reaches them.
    - filters:
        - type: RequestHeaderModifier
          requestHeaderModifier: { remove: [Cookie] }
      backendRefs: [{ name: workspaces-frontend, port: 8443 }]
```

During development, the frontend's dev server plays the ingress's role (see the
[Vite recipe](#during-development-a-dev-server-proxy)). The e2e fixture runs this recipe for real:
[gateway.yaml](../test/e2e/cluster/gateway.yaml) is a Gateway, an `HTTPRoute` per
namespace and a `BackendTLSPolicy` per backend that makes the gateway verify its
certificate, implemented by Traefik.

Sending everything through krm-foyer would be wrong for several reasons:

- **Blast radius.** A routing bug in krm-foyer can then only affect krm-foyer's routes.
- **Resource bounds.** Its limits are sized for API calls and streams, not for static
  assets.
- **Scaling.** Each service scales on its own.
- **Size.** It keeps the binary small: no static hosting, no single-page-app fallback,
  no catch-all proxy.

Paths outside its prefixes get a 404 from krm-foyer, which a unit test pins.

### During development: a dev-server proxy

A frontend's development loop is its dev server, with hot reload. The dev server takes the
ingress's place: it serves the application's files, and proxies krm-foyer's four prefixes
to a krm-foyer on a local cluster, `task demo`'s for example. The browser still sees one
origin, so cookies and CSRF work as they do in production, with nothing mocked.

The one rule: **the browser's origin is krm-foyer's public URL.** The session cookie is
`__Host-` and `Secure`, the login comes back to `<public URL>/auth/callback`, and a write
needs an `Origin` equal to the public URL. A dev server on another origin breaks all
three, and rewriting `Origin` in the proxy to make writes pass would hide exactly the
bugs the loop should show. So the dev server takes the front door's address:
`https://foyer.localhost:8443`, `task demo`'s public URL, with the fixture's certificate
for that name (import `.e2e/ca.crt` into the browser, as for `task demo`).

```ts
// vite.config.ts: the application's files from Vite, krm-foyer's prefixes from the
// task demo cluster, all on https://foyer.localhost:8443.
import { readFileSync } from 'node:fs'
import { Agent } from 'node:https'
import { defineConfig } from 'vite'

const e2e = '../krm-foyer/.e2e' // a krm-foyer checkout after `task demo`
// FOYER_ADDR is krm-foyer's NodePort on the k3d node, reachable from where task demo
// ran; the front door's Traefik is not used.
const addr = /^FOYER_ADDR=(.*)$/m.exec(readFileSync(`${e2e}/foyer-env`, 'utf8'))![1]
const foyer = {
  target: `https://${addr}`,
  // Verify krm-foyer by the name its certificate holds, against the fixture CA.
  agent: new Agent({ ca: readFileSync(`${e2e}/ca.crt`), servername: 'foyer.localhost' }),
  secure: true,
  // The browser's own Host, Origin and Cookie go through unchanged.
  changeOrigin: false,
}

export default defineConfig({
  server: {
    host: 'foyer.localhost',
    port: 8443,
    strictPort: true,
    https: { cert: readFileSync(`${e2e}/foyer/tls.crt`), key: readFileSync(`${e2e}/foyer/tls.key`) },
    proxy: { '/auth/': foyer, '/k8s/': foyer, '/stream/': foyer, '/_foyer/': foyer },
  },
})
```

Before starting Vite, stop the port-forward that puts Traefik on that port:
`pkill -f 'kubectl port-forward .*svc/traefik '` (`test/e2e/cluster/port-forward.sh`
brings it back). Dex stays on its own forward at `https://dex.localhost:5556`.

What this leaves the application to decide is whether its own backend mock goes. Under
krm-foyer, `/k8s` and `/stream` are the real API server, so a mock of Kubernetes is no
longer needed for the loop. What remains to mock is the application's domain backend, if
it has one, and it sees the same `Krm-Foyer-Identity` only when something asks
`/auth/check` for it, which a dev server does not. Proxy it with a fixed identity
header in development, or run it behind the fixture's Traefik.

This recipe is written from the fixture's facts (its public URL, certificate and
NodePort), but the e2e suite does not run Vite.

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
  `Path=/`, so the browser sends the cookie with every request. Its contents are sealed,
  but it is a bearer credential: anyone who holds it can call krm-foyer as the user, from
  anywhere, until the session expires, logout or not. A co-hosted service that logs
  `Cookie` headers hands sessions to whoever reads those logs. Co-hosted services must not log or keep it.
- **Every hop on the origin needs the protection of the hop to krm-foyer**, re-encrypted
  and verified or isolated as [above](#decision-2026-10-01-both-tls-models). The cookie
  crosses each one, and so do the answers: a script tampered with on the way to the
  browser runs as the user, so a plain hop to a static file server is as open as one to
  krm-foyer. A service that needs no cookie, a file server above all, should not get it:
  the gateway removes `Cookie` on that route, as in the recipe above.
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
3. **krm-foyer answers an identity check** for the backend: [`/auth/check?identity=true`](#identity-for-a-domain-backend)
   (option C below), identity only. This avoids the second session, but the backend gets
   no token.

krm-foyer never hands its tokens to another service. That would spread custody of the
credential to code that krm-foyer does not test.

## Decision (2026-10-01): a login gate for the application's pages

**Built (2026-10-05), with identity for a domain backend** (option C below), because Voter
asked for both ([implementer feedback](implementer-feedback.md), entry 1).

The ingress can ask krm-foyer one question before serving a request: *is this browser
signed in, and may it make this request?* For a page, if it is not, the browser goes to
the login and comes back to the page it asked for. Every frontend needs this, and it is
wasteful to write it in each one. For a domain backend, the ingress can ask a second
question too: *who is it?* Both are narrow uses of forward-auth, and neither is the
design rejected below: the ingress never sends anything to the API server.

### The check

**`GET /auth/check`** answers:

| Situation | Answer |
| --- | --- |
| Signed in, and the request may be made | `204`, with `Cache-Control: no-store` |
| With `?identity=true` | `204` and a `Krm-Foyer-Identity` header: see [identity](#identity-for-a-domain-backend) |
| No session | `401`, the interruption `/k8s` gives: a `Status` for code, a page with a **Sign in** link for a person |
| No session, `?redirect=true`, and a page load | `302` to `/auth/login?return_to=<page>` |
| A write without the session's CSRF proof, or from another origin | `403`, `CSRFProofRequired` or `CrossOriginRequest`, as for `/k8s` |
| The API server cannot say who the user is (identity only) | Its own refusal, or `503`; never an identity guessed from the token |
| An option other than `redirect=true` and `identity=true`, or a bad `X-Forwarded-Method` | `400`, so that a typo such as `identity=1` fails loudly |

The check is of **the request the ingress forwards**, not of the check's own request.
krm-foyer reads three things from it, all of which the browser could equally send to
krm-foyer itself, so none is trusted for more than that:

- **The method,** from `X-Forwarded-Method`. A method other than `GET` or `HEAD` is a
  write, and needs what a write to `/k8s` needs: the session's CSRF token in
  `X-CSRF-Token`, and `Origin` (or `Sec-Fetch-Site: same-origin`) naming krm-foyer's
  origin. Without the header the check is of a `GET`. Methods are case-sensitive, so
  `get` is a write. **The ingress must set this header itself** (Traefik always does;
  the nginx recipe below sets it), or a browser's own copy decides how a write is checked.
- **The page,** from `X-Forwarded-Uri` (Traefik) or `X-Original-URI` (nginx). It is
  validated as a local path exactly like `return_to`, and anything else becomes `/`. It
  only decides where the login returns to.
- **The browser's own headers:** `Cookie`, `Origin`, `X-CSRF-Token`, and the
  `Sec-Fetch-*` headers that decide between a page and a `Status`, and between a
  redirect and a `401`. Only a page load (no `Sec-Fetch-Mode`, or `navigate`, on a `GET`)
  is redirected; a script gets the `401` it can act on.

### Identity for a domain backend

With `?identity=true`, a `204` carries one header, `Krm-Foyer-Identity`: the unpadded
base64url encoding of this JSON.

```json
{
  "userInfo": {
    "username": "oidc:alice@example.com",
    "uid": "...",
    "groups": ["oidc:voters", "system:authenticated"],
    "extra": { "configbutler.ai/claims/display-name": ["Alice"] }
  },
  "displayName": "Alice",
  "connector": "room-pass",
  "issuer": "https://dex.example.com",
  "expiresAt": "2026-10-05T20:00:00Z"
}
```

- `userInfo` is **the API server's answer** to a SelfSubjectReview sent with the user's own
  token, exactly as [`/auth/whoami`](design.md#whoami) shows it: the name RBAC and the audit
  log use, with the groups and extras the cluster's authentication configuration
  maps. A backend that records who did something records the same name Kubernetes does.
- `displayName` and `connector` are the session's, as `/auth/session` shows them
  ([session claims](design.md#session-claims)). `connector` is present only when one is
  configured. A backend can, for example, accept votes only from `room-pass` logins.
- **Never a token.** The backend learns who the user is; it cannot act as them in
  Kubernetes. A backend that needs that is a separate design question.

One header, not one per field. An ingress copies it whole, a backend parses one value,
and group names may hold commas, which a list in a header cannot carry safely.

A check with identity goes through the same [bounds](bounds.md) as a request to `/k8s`:
it counts against the session's request rate. The API server's answer is reused for 30
seconds per token (never past the session's end), so a backend checked on every request
costs one SelfSubjectReview per user per 30 seconds. The answer depends only on the
token and the cluster's authentication configuration, so 30 seconds is how long a change
to that configuration can take to reach a backend. **Access decisions are not reused or
made here:** what the user may do in Kubernetes is still the API server's decision on
every `/k8s` request, and what the user may do in the backend is the backend's.

What the deployment must get right, because krm-foyer cannot check it:

- **The ingress removes the browser's copy of `Krm-Foyer-Identity`** before it adds
  krm-foyer's. Traefik's ForwardAuth does this for every header in
  `authResponseHeaders`; the nginx recipe below overwrites it. The e2e suite sends a
  forged one through Traefik and checks the backend never sees it.
- **Only the ingress reaches the backend,** on a route that always asks the check, so
  that nothing can send it a header the check did not write. A NetworkPolicy admitting only
  the ingress to the backend's port is the way to enforce that. Room Pass contained its
  own header trust the same way.
- **The backend's route asks with `identity=true`.** A page-gate route (no identity)
  does not remove the header, so a backend behind it would believe whatever the browser
  sent.

### Recipes

The e2e fixture runs the Traefik recipe for real
([traefik-routes.yaml](../test/e2e/cluster/traefik-routes.yaml)), beside its Gateway API
routes on the same origin: `/public/` is a domain backend behind the identity check, and
`/members/` pages behind the login gate. Its specs try a forged identity header, writes
without CSRF proof and from another origin, a signed-out page load and a signed-out script,
all through the real Traefik.

**Traefik, `IngressRoute` and ForwardAuth.** For a cluster that routes with Traefik's own
resources, as Voter's does. krm-foyer's own prefixes stay on a plain route (or the
`HTTPRoute` above); only the application's routes get a middleware.

```yaml
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata: { name: foyer-identity }
spec:
  forwardAuth:
    # krm-foyer's Service, by a name its certificate holds, verified against its CA.
    address: https://krm-foyer.krm-foyer.svc/auth/check?identity=true
    tls: { caSecret: krm-foyer-ca }
    # Removes the browser's copy, then copies krm-foyer's.
    authResponseHeaders: [Krm-Foyer-Identity]
    trustForwardHeader: false
---
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata: { name: foyer-gate }
spec:
  forwardAuth:
    address: https://krm-foyer.krm-foyer.svc/auth/check?redirect=true
    tls: { caSecret: krm-foyer-ca }
    trustForwardHeader: false
---
# The backend and the file server have no use for the session cookie.
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata: { name: no-cookie }
spec:
  headers:
    customRequestHeaders: { Cookie: "" }
---
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata: { name: voter }
spec:
  entryPoints: [websecure]
  routes:
    # krm-foyer's prefixes, whole segments: Traefik's PathPrefix is a plain string
    # prefix, so /stream would also catch /streams. A Service in another namespace
    # needs the CRD provider's allowCrossNamespace; or keep this route beside krm-foyer.
    - match: Host(`voter.example.com`) && (PathPrefix(`/auth/`) || PathPrefix(`/k8s/`) || PathPrefix(`/stream/`) || PathPrefix(`/_foyer/`))
      services: [{ name: krm-foyer, port: 443, scheme: https, serversTransport: krm-foyer }]
    - match: Host(`voter.example.com`) && PathPrefix(`/public/`)
      middlewares: [{ name: foyer-identity }, { name: no-cookie }]
      services: [{ name: voter-backend, port: 8080 }]
    # Pages behind the login gate. Files the pages load (scripts, images) stay public.
    - match: Host(`voter.example.com`) && PathPrefix(`/admin/`)
      middlewares: [{ name: foyer-gate }, { name: no-cookie }]
      services: [{ name: voter-web, port: 8080 }]
    - match: Host(`voter.example.com`)
      middlewares: [{ name: no-cookie }]
      services: [{ name: voter-web, port: 8080 }]
  tls: { secretName: voter-tls }
```

Traefik sends the check a `GET` with the browser's headers and its own `X-Forwarded-Method`
and `X-Forwarded-Uri`, and passes a non-2xx answer to the browser as it is: the `401`
`Status` to a script, the sign-in page or the `302` to a page load. Its `rule`s are
matched by length unless `priority` says otherwise; give the gated routes a priority when
they share an entry point with Gateway API routes, as the fixture does.

**nginx, `auth_request`.** nginx accepts only 2xx, 401 and 403 from the check, and turns
the 401 into the login itself, so it does not use `redirect=true`. The page goes into
the login link as one `return_to` value, so it has to be encoded, and stock nginx has
no directive that encodes a variable for a query. The recipe uses
[njs](https://nginx.org/en/docs/njs/), which the official nginx images include: load it
at the top of `nginx.conf` with `load_module modules/ngx_http_js_module.so;`.

```nginx
# The page the browser asked for, encoded as one query value (foyer.js, below).
js_import foyer from /etc/nginx/foyer.js;
js_set $foyer_return_to foyer.returnTo;

location = /_check {
    internal;
    proxy_pass https://krm-foyer.krm-foyer.svc/auth/check?identity=true;
    proxy_ssl_verify on;
    proxy_ssl_trusted_certificate /etc/nginx/krm-foyer-ca.crt;
    proxy_ssl_name krm-foyer.krm-foyer.svc;
    proxy_pass_request_body off;
    proxy_set_header Content-Length "";
    # Set here, so the browser's own copies count for nothing.
    proxy_set_header X-Forwarded-Method $request_method;
    proxy_set_header X-Original-URI $request_uri;
}

location /public/ {
    auth_request /_check;
    auth_request_set $foyer_identity $upstream_http_krm_foyer_identity;
    # Overwrites the browser's copy, with krm-foyer's or with nothing.
    proxy_set_header Krm-Foyer-Identity $foyer_identity;
    proxy_set_header Cookie "";
    proxy_pass http://voter-backend:8080;
}

location /admin/ {
    auth_request /_check;
    error_page 401 = @login;
    proxy_set_header Cookie "";
    proxy_pass http://voter-web:8080;
}

location @login {
    # Relative, so the browser stays on the origin it used, whatever port nginx has.
    absolute_redirect off;
    return 302 /auth/login?return_to=$foyer_return_to;
}
```

```js
// foyer.js: the raw request target, path and query, as one query value. Every byte
// that would end the value or change its meaning (& = + # % and the rest) is
// percent-encoded, so krm-foyer decodes exactly the target the browser sent.
function returnTo(r) {
    return encodeURIComponent(r.variables.request_uri);
}

export default { returnTo };
```

`$request_uri` is the raw request target, so the page comes back exactly as the
browser asked for it, its own encoding included. Put in the query as it is, as an
earlier version of this recipe did, it would not: the page's `&` starts a parameter of
the login's (`/admin/edit?name=x&tab=history` came back as `/admin/edit?name=x`), its
`%26` and `+` are decoded once too often, and a parameter of the page's named
`return_to` or `oidc.login_hint` becomes the login's own. krm-foyer's login checks the
decoded value as a local path and refuses anything else, so it cannot send the browser
off-site. The e2e fixture runs this recipe as written here
([nginx-door.conf](../test/e2e/cluster/nginx-door.conf) takes it from this page), in
front of krm-foyer, and walks a login from a gated page with several parameters, with
percent-encoding and a literal `+`, and with parameters named like the login's own.

### How to deploy it

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

It does not remove all frontend code either. A session can end while a page is open: it
reaches its absolute timeout or its token's expiry, or, later, a refresh fails. Then `/k8s` answers 401 and the application
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
  own external-auth configuration, and each would need its own tests. Gateway API's
  experimental filter also needs implementation-specific validation. The Kubernetes
  project has retired ingress-nginx, the most
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
  - logout that closes the session's streams, and the session lifecycle;
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

**Built, identity only (2026-10-05),** when Voter, a hybrid application, asked:
[identity for a domain backend](#identity-for-a-domain-backend), on top of
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
supported, a transparent ingress is fine, the login gate can cover page loads, and
the seam for external login exists if it is ever needed.
