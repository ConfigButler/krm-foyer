# How krm-foyer is tested

krm-foyer sits between browsers and a cluster, and its main promise is a negative one:
**it does not invent authentication or authorization.** Dex, or whichever issuer is
configured, says who the user is. The API server says what that user may do. krm-foyer
carries the user's credential and decides nothing about access. This document
describes how the tests prove that.

## What has to be proved

| Claim | What would break it | How the suite catches it |
| --- | --- | --- |
| Identity comes from the issuer | krm-foyer asserting a user name, impersonating, or accepting a token issued to another client | The audit log names the user and shows no impersonation; tokens for other clients are rejected |
| Permission comes from RBAC | krm-foyer deciding something itself, or caching a decision | The same request gets the same answer through krm-foyer as it does directly; a RoleBinding change shows on the next request |
| No service-account fallback | A request without a usable user credential being sent with krm-foyer's own identity | The e2e deployment gives krm-foyer's service account cluster-admin, so a fallback turns a 403 into a 200 |
| One parse of each path | A path krm-foyer reads one way and Kubernetes another | Non-canonical paths are rejected; fuzzing shows the path forwarded is byte-for-byte the path received |
| The credential stays on the server | A token or session ID in a response body, header, page or log line | Every response and log line is scanned for every token involved, including those krm-foyer obtained by refresh |

## Four techniques

**Differential answers.** The suite logs in to Dex as a user and keeps that user's own
token. For any request, it asks the API server directly with that token and then asks
krm-foyer with that user's session. Status code, content type and body must be the same,
for successes as much as for refusals: transparent answers are the promise, and an empty
or altered object would break it as surely as a wrong status. Only what the API server
generates afresh for each request is set aside, and named: a list's `resourceVersion`,
and on a write the new object's `uid`, `creationTimestamp`, `resourceVersion`,
`managedFields` times and generated name. If krm-foyer made any decision of its own, or
changed what it passed on, the two would differ. This one technique covers most of "does
not invent authorization", and it needs no list of expected answers to maintain: the API
server supplies the expected answer.

**The audit log as witness.** The fixture's API server writes an audit log. krm-foyer can
influence what it sends, but not what the API server writes down, so the log settles
whose credential a request used. Each request the suite makes carries a unique
User-Agent, which finds its audit event.

**A bait service account.** In the e2e deployment, krm-foyer's own service account is
cluster-admin. This makes the most dangerous bug the loudest one: a fallback would turn
a refusal into success.

**Token scan.** No response the suite received from krm-foyer, and no line krm-foyer
logged, may contain a token, a client secret or a session ID. The scan runs as a spec and
again after the whole suite. It looks for every token the suite obtained, exactly, and for
anything shaped like a JWT. The second covers the tokens krm-foyer holds and never showed
the suite: the ID tokens it got by redeeming codes, and its own service-account token.
krm-foyer holds no refresh token yet, since it asks for no `offline_access`. When refresh
arrives (roadmap step 6), its session store moves out of process, and the scan also reads
it as admin for the opaque refresh tokens no pattern can find.

A session ID has exactly one place it belongs: the `Set-Cookie` header that issues it,
on the login callback and wherever the ID is rotated. The scan allows the ID there, and
only as the value of krm-foyer's own session cookie. A session ID anywhere else in that
response, in any other response, or in a log line still fails the run.

## The layers

| Layer | Runs with | What it covers |
| --- | --- | --- |
| Unit | `task test` (`go test -race ./...`) | Path checking, header handling, upstream response rules, session and cookie rules, page rendering. Fast and exhaustive |
| e2e | `task test-e2e` | A real API server trusting a real Dex, and krm-foyer deployed in the cluster in front of it. The fixture's own specs validate the fixture; the `foyer` specs prove the claims above |
| Browser | `task test-e2e` (label `browser`) | The [hello example](../examples/hello) in Chromium, through the front door: Dex's own login form, a 409, a 403, logout, and no credential within the page's reach. Only for what a Go HTTP client cannot show |

All three run in `task verify` and in CI.

### Unit tests

These use the standard library `testing` package, with table tests. Most boundary
bugs are here, where they are cheap to find:

- **Path checking** ([internal/proxy](../internal/proxy)) is a pure function: a path in, the upstream path or a refusal out.
  The table covers [path hygiene](design.md#access): encoded slashes, `..`, repeated
  slashes and needless percent-encoding are all rejected. The fuzz property: for any path
  accepted, the path sent upstream is byte-for-byte the path received, minus `/k8s`.
  That is the
  [one-parser rule](ingress.md#why-routing-by-path-is-safe-when-forward-auth-is-not)
  in test form.
- **The proxy** runs against an `httptest` server standing in for the API server,
  which records what reached it. That shows what e2e cannot see directly: the browser's
  `Authorization`, `Impersonate-*` and cookie headers never arrive, and the user's token
  always does. Each test that matters runs over HTTP/1.1 and HTTP/2 to the API server.
- **The upstream response check** is fuzzed from the browser's side: whatever the
  upstream sends, an approved response has only allowlisted headers, no encoding, and a
  single content type that a browser cannot read as anything but an allowed one. The
  property is written independently of the check, so it can catch the check's own
  blind spots, such as a repeated `Content-Type` field.
- **Interruption pages**: every row of the interruptions table is triggered twice, as
  code and as a browser navigation, and must give the same status both times. A
  `fetch`, an iframe, another method, a capitalized or repeated `Sec-Fetch-Dest` and
  `Accept: text/html` alone all get JSON, and the API server's own 401, 403, 404, 409,
  422, 429, 500 and 503 reach a navigation unchanged.
- **Sessions** ([internal/session](../internal/session)): rotation at login (a planted
  ID is never adopted), idle, absolute and token expiry, logout winning over a request
  that is recording activity, and a store failure never read as "no session". Expiry
  runs against a store that never expires anything as well, so the session code decides
  on its own. Every way of presenting other than exactly one well-formed cookie is no
  session. Checking whether a session is still live, as an open response does, never
  counts as use.
- **Open responses end with their session** ([internal/proxy](../internal/proxy)), over
  every pair of HTTP/1.1 and HTTP/2 towards the browser and towards the API server: when
  the session ends, the browser's response is aborted, never ended cleanly, and the
  request to the API server is cancelled. So is a response whose session ends before
  the API server answers, one whose session check hangs, and one whose browser has
  stopped reading; a live session's response is never touched. The same holds for a
  response open longer than its duration.
- **Concurrency bounds** ([internal/proxy](../internal/proxy)): a request past a
  session's or the replica's limit gets a 429 naming the bound and never reaches the
  API server, and every way a request can end (complete, the browser leaving, the
  session ending, its duration up, an answer held back, a dropped connection) gives its
  slot back.
- **The request rate** ([internal/proxy](../internal/proxy)), against a clock the test
  moves: past a session's burst, a 429 with `Retry-After` (the wait rounded up to whole
  seconds, in both forms) that never reaches the API server. Its property is stated from
  outside the bucket and checked over random request times: in any stretch of time a
  session gets at most its burst plus the rate times the stretch let through, and a
  session that never sends faster than the rate is never refused.
- **Login** ([internal/auth](../internal/auth)) runs against a fake issuer in the test
  that behaves like a strict one (PKCE enforced, codes single use) unless told to
  misbehave: a token for another audience or issuer, expired, signed by a stranger, with
  another nonce or none. A browser with a cookie jar walks each flow, including login
  CSRF (the attacker's callback in the victim's browser), replayed and malformed
  callbacks and an expired login. Every response that browser received, and every line
  krm-foyer logged, is then scanned for ID and access tokens, the client secret,
  authorization codes and PKCE verifiers, and for session IDs outside the `Set-Cookie`
  that issues them. The fake issuer can also refuse a token request by echoing it, in
  each place an OAuth error has room for text, so the scan proves those refusals are
  logged by status and error code only. The proxy runs behind login as the binary wires it, so a
  refused request is shown never to reach the API server, and each session error,
  however wrapped, becomes one answer that is never RBAC's `Forbidden`. Dex is the e2e suite's issuer, for what a real login
  does; the fake issuer is for refusals Dex never causes.
- **Return paths** are fuzzed against a model of how a browser resolves a `Location`
  (the WHATWG URL standard's leniencies: backslashes, stripped tabs, any number of
  leading slashes), not against the check itself: whatever is accepted stays on the
  origin.
- **CSRF and same origin** are fuzzed with the rule stated from outside: a request is let
  through exactly when it is a `GET` or `HEAD`, or carries one proof field equal to the
  session's token and one `Origin` equal to the configured origin (or, with no `Origin`,
  one `Sec-Fetch-Site: same-origin`). Repeated fields, which `Header.Get` would read only
  the first of, are part of the input.

### e2e tests

The suite lives in [test/e2e](../test/e2e) behind the `e2e` build tag. It uses Ginkgo
and Gomega, like gitops-reverser's suite. It has two parts:

- **The fixture's API server** (label `fixture`) involves no krm-foyer code. It shows that
  the fixture tells the truth: a Dex user is identified by their email, and only when the
  issuer says that email is verified (`email_verified` absent, `false` or a string is
  refused); RBAC alone decides and follows grant changes; tokens for another client, or
  with claims rewritten after signing, are rejected; and the audit log names the user.
  Every refusal has a matching acceptance next to it, so a 401 cannot pass for the wrong
  reason. If these fail, nothing else in the suite means anything.
- **krm-foyer** (label `foyer`) is the list of claims. The suite signs in the way a
  person does: a cookie jar opens `/auth/login`, walks Dex's login form and comes back
  through the callback. Claims not built yet are pending specs, each made real in the
  change that implements it; `task test-e2e -- -v -ginkgo.v` lists them.
- **Mutation checks against the cluster.** A boundary spec is trusted once a build of
  krm-foyer that breaks the boundary on purpose, deployed into the fixture, makes it
  fail: a service-account fallback, a forwarded `Authorization` or `Impersonate-User`,
  the service account's token sent instead of the user's, an unchecked `state`. The
  impersonation spec first passed for the wrong reason (alice could not impersonate
  anyone, so a forwarded header was refused anyway); it now grants alice the right to
  impersonate bob, so a forwarded header would turn its 403 into a 200.
- **Bounds and the session lifecycle** (in the `foyer` label) read krm-foyer's metrics
  through the API server's service proxy, as admin. A native watch through `/k8s` is
  aborted when its session logs out and when it expires, and the audit log shows the API
  server completed it within a minute although it asked for `timeoutSeconds=600`: the
  cancellation reached the API server. The same is shown for a watch open longer than
  its duration. Each was seen to fail against a build that did not cut the response. A
  session's third request while two watches are open gets krm-foyer's own 429 and never
  reaches the API server, which a build without the per-session check failed. So does a
  session sending faster than its rate, with `Retry-After`, which a build whose rate
  never refuses failed.
- **The hello example** (label `browser`) is the claim that krm-foyer is usable, not
  only correct. Chromium ([chromedp/headless-shell](https://hub.docker.com/r/chromedp/headless-shell),
  pinned by digest, driven from Go with chromedp) runs in the network namespace of the
  container the suite runs in, so `*.localhost` reaches the same port-forwards a
  person's browser reaches through VS Code. It trusts
  exactly the front door's and Dex's certificates, by their public keys. Its specs were
  each seen to fail against a broken build: the browser not trusting Dex, the example
  re-reading and saving again on a 409, the helper leaving out the CSRF header, and the
  helper not reading a new CSRF token after the user signed in again in another tab.

## The e2e fixture

```mermaid
flowchart LR
    subgraph C[This container: devcontainer or CI job]
      T[Test runner]
      B[Chromium, or your browser<br/>through VS Code]
      PF[kubectl port-forward<br/>127.0.0.1:8443, :5556]
    end
    subgraph K3S[k3d cluster]
      K[k3s API server]
      D[Dex<br/>dex.localhost:5556]
      I[Test issuer<br/>issuer.krm-foyer.test:8443]
      FD[Front door<br/>Traefik: Gateway, HTTPRoutes]
      W[hello-web<br/>nginx, files only, TLS]
      F[krm-foyer<br/>foyer.localhost:8443]
      A[(audit.log)]
    end
    B --> PF
    T -->|password grant, login form| PF
    PF --> D
    PF --> FD
    T -->|DevTools| B
    T -->|bearer token, directly| K
    T -->|session cookie, NodePort| F
    FD -->|/| W
    FD -->|/auth, /k8s, /_foyer, TLS verified| F
    F -->|user's token| K
    F -->|discovery and code exchange| D
    K -->|discovery and keys| D
    K -->|discovery and keys| I
    K --> A
    T -->|docker exec| A
```

Everything runs in the cluster, as in gitops-reverser's e2e; what a browser needs comes
out through `kubectl port-forward`.

[start-cluster.sh](../test/e2e/cluster/start-cluster.sh) creates a Docker network and a
single-node k3d cluster on it, then deploys the two issuers into the cluster
([issuers.yaml](../test/e2e/cluster/issuers.yaml)). The API server trusts both through an
[AuthenticationConfiguration](../test/e2e/cluster/authentication-config.yaml), under the
same rules, and records requests with an [audit policy](../test/e2e/cluster/audit-policy.yaml).
The API server is not a pod and cannot use cluster DNS: k3d's `--host-alias` puts each
issuer's name in the node's `/etc/hosts`, pointing at its Service's fixed ClusterIP, and
in CoreDNS for pods, so every caller uses the same issuer URL. Dex keeps its state in
custom resources, its signing keys included: with memory storage, a restarted Dex signs
with new keys, and the API server refused every token for 221 seconds before it fetched
them. The devcontainer joins the network, and a CI runner is the Docker host, so both reach
the API server the same way. k3d always publishes the API server's port; it is bound to
loopback, and the script fails if anything is published on another interface.

[deploy-foyer.sh](../test/e2e/cluster/deploy-foyer.sh) deploys krm-foyer from
[foyer.yaml](../test/e2e/cluster/foyer.yaml). The image `task image` built is imported
with `k3d image import` under a tag derived from its ID, so the Deployment rolls exactly
when the binary changes. krm-foyer serves TLS for `foyer.localhost` with a
certificate from the fixture CA, trusts Dex through the same CA, and reaches the API
server at `kubernetes.default.svc`. The suite reaches it through a NodePort on the node's
address on the Docker network: the `foyer` specs test krm-foyer, not the front door. Its
service account is cluster-admin and its token is mounted, as bait; the suite checks both
before it starts.

Beside it runs a second, brief krm-foyer ([foyer-brief.yaml](../test/e2e/cluster/foyer-brief.yaml)):
the same image, certificate, public URL and Dex client, but sessions that end 45
seconds after login, a session check every second, responses cut short after 20
seconds, and per session two requests in flight and ten at once, then one a second. The specs that wait for a session to expire or a bound to be reached use it, on
a NodePort of its own, so no other spec has to race its session.

The front door is Gateway API, implemented by Traefik: the official chart, at the version
gitops-reverser's e2e uses. k3s's own Traefik is disabled, so k3s stays minimal, and
[install-traefik.sh](../test/e2e/cluster/install-traefik.sh) installs Traefik with Helm
and [traefik-values.yaml](../test/e2e/cluster/traefik-values.yaml) (`task e2e-up` runs
it). The chart brings the Gateway API CRDs.

[front-door.sh](../test/e2e/cluster/front-door.sh) applies the hello example's
[resources](../examples/hello/manifests.yaml), deploys a file server for its pages
([hello-web.yaml](../test/e2e/cluster/hello-web.yaml): nginx, files only, over TLS), and
applies [gateway.yaml](../test/e2e/cluster/gateway.yaml): a Gateway for `foyer.localhost`,
an `HTTPRoute` sending `/` to the file server without the `Cookie` header, one sending
`/auth`, `/k8s`, `/stream` and `/_foyer` to krm-foyer, and a `BackendTLSPolicy` for each
backend under which Traefik verifies its certificate (with a wrong hostname in it, every
request fails). The file server answers 400 to any request that still carries a cookie,
so every signed-in browser spec fails if the route stops removing it. Then
[port-forward.sh](../test/e2e/cluster/port-forward.sh) forwards Traefik to
`127.0.0.1:8443` and Dex to `127.0.0.1:5556` in this container, detached, and checks both
by their public names. Browsers resolve `foyer.localhost` and `dex.localhost` to loopback,
and VS Code forwards both ports to the machine the browser runs on, keeping their numbers
(`devcontainer.json`), so `https://foyer.localhost:8443` works with no hosts-file entry,
wherever Docker runs. A port-forward follows one pod: when Dex or Traefik rolls,
run `test/e2e/cluster/port-forward.sh` (or `task e2e-deploy`) again.

The test issuer is nginx serving a discovery document and a JWKS. The suite holds its
signing key (`.e2e/issuer-signing.key`), so it can mint tokens with claims Dex never
issues. Use it for claims; use Dex for anything a real login would do.

Dex has two static users, `alice@example.com` and `bob@example.com` (password
`password`), which Kubernetes sees as `oidc:alice@example.com` and
`oidc:bob@example.com`. There are two clients: `krm-foyer`, whose tokens the cluster
accepts, and `other-app`, whose tokens it must reject.

```bash
task e2e-up     # start or reuse the fixture, with Traefik (about a minute the first time)
task e2e-deploy # build the image, deploy krm-foyer and the front door, and port-forward
task demo       # e2e-up and e2e-deploy, then how to sign in from your browser
task test-e2e   # run the suite; brings the fixture up and deploys krm-foyer first
task e2e-down   # remove the cluster, Dex, the network and the certificates
```

When something fails, the API server's view is usually the answer:
`docker logs k3d-krm-foyer-e2e-server-0` shows authenticator errors, and
`docker exec k3d-krm-foyer-e2e-server-0 cat /etc/krm-foyer-e2e/audit.log` shows who it
thought was asking. `KUBECONFIG=.e2e/kubeconfig kubectl ...` gives admin access for
looking around.

## Order of work

The [roadmap](roadmap.md#order-of-work) sets the order: the proxy, then login and
sessions deployed into the fixture, then streams. Each step makes
pending specs real.

To keep krm-foyer small, the OIDC work uses maintained libraries (`coreos/go-oidc` and
`golang.org/x/oauth2`), and the proxy uses `net/http/httputil`. Neither the service nor
its tests need `client-go`.
