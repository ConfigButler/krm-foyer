# Alternatives

krm-foyer does one narrow thing. It lets a browser application on the same origin call
the Kubernetes API as the signed-in user. It runs the OIDC login itself and keeps the
token on the server, so the browser holds only an HttpOnly cookie. It forwards the
user's own token, so RBAC decides, and it requires a CSRF proof on every change. This
page lists the tools nearby and the goals each one meets, so you can tell when one of
them fits better.

It has two parts:

- [**Instead of krm-foyer**](#instead-of-krm-foyer): other ways to let people reach the
  Kubernetes API.
- [**Instead of the browser helper**](#instead-of-the-browser-helper): JavaScript
  libraries an application could use in place of `/_foyer/foyer.js`.

It was written in October 2026. Facts about other projects (releases, maintenance,
dependencies) come from their repositories and package registries on 2026-10-02, and
they go stale. The choice of OIDC issuer has [its own page](investigations/choosing-an-issuer.md).

## What krm-foyer is for

Use it when all of these hold:

- **The user is in a browser**, in an application you build, not in `kubectl` or a
  dashboard someone else built.
- **Kubernetes should decide with the user's identity.** API requests use the user's
  token. Opt-in shared watches use a separate identity and API-server reviews of every
  subscriber; no service account substitutes for a user's API request, and nothing
  impersonates them.
- **Login tokens stay on the server.** A script injected into the page can still act as
  the user and read anything their grants allow, including Secrets or token-issuing
  APIs if granted. HttpOnly protects the session cookie; it does not narrow Kubernetes
  access. See [application scope](application-scope.md).
- **Your API server trusts an OIDC issuer**, or you can make it trust one.

It is weaker when you want a ready-made UI, a CLI, or a cluster whose API server you
cannot configure.

## Instead of krm-foyer

The axes that matter are where the login happens, where the token lives, and what
identity the API server sees. A tool that impersonates holds a service account allowed
to act as any user, and that account is the prize. A tool that forwards the user's own
token leaves the decision with RBAC, as krm-foyer does.

| Tool | Its goal | Login, and where the token lives | What the API server sees | UI or building block | Status (2026-10-02) |
| --- | --- | --- | --- | --- | --- |
| [Headlamp](https://github.com/kubernetes-sigs/headlamp) | A Kubernetes web UI with plugins (SIG UI) | Its backend runs the code flow; PKCE is opt-in (`-oidc-use-pkce`). The ID token itself is in an HttpOnly, `SameSite=Strict` cookie, and the refresh token stays on the server | The user's token, as `Bearer`. An opt-in `--unsafe-use-service-account-token` falls back to its service account | UI | Apache-2.0, active (0.45, August 2026) |
| [oauth2-proxy](https://github.com/oauth2-proxy/oauth2-proxy) | A login in front of any upstream | It runs the login. The token is encrypted in the cookie, or kept in Redis with a ticket in the cookie | The user's ID token, as `Bearer` (`pass-authorization-header`) | Building block | MIT, active (7.15, October 2026) |
| [kube-oidc-proxy](https://github.com/TremoloSecurity/kube-oidc-proxy) (TremoloSecurity fork) | OIDC for clusters whose API server cannot trust an issuer (managed clusters) | None. kubectl brings its own token | Impersonation, by its own service account | Building block | Apache-2.0, active (1.0.12, May 2026). [Jetstack's original](https://github.com/jetstack/kube-oidc-proxy) has been archived since May 2024 |
| [Pinniped](https://github.com/vmware/pinniped) Concierge impersonation proxy | kubectl login across clusters and identity providers | The pinniped CLI and a kubeconfig on the user's machine | Impersonation, through a service account allowed nothing else | Building block for a CLI | Apache-2.0, active (0.47, July 2026). More in [choosing an issuer](investigations/choosing-an-issuer.md) |
| [kubectl proxy](https://kubernetes.io/docs/reference/kubectl/generated/kubectl_proxy/) | Local access for one developer | The kubeconfig on that machine | That kubeconfig's credentials | Building block, localhost only | Ships with kubectl. Its own docs warn that `--disable-filter` opens it to cross-site request forgery |
| [OpenUnison](https://github.com/OpenUnison/openunison-k8s) | A sign-in portal for kubectl and dashboards | Its portal | The user's ID token, or impersonation through a bundled kube-oidc-proxy. It can front Headlamp | Portal | Apache-2.0, active (1.0.51, September 2026) |
| [Rancher](https://github.com/rancher/rancher) | Managing many clusters | Rancher's login | Impersonation, through a service account per user in `cattle-impersonation-system` | Platform and UI | Apache-2.0, active (2.15, September 2026) |
| [Teleport](https://github.com/gravitational/teleport) | An access platform: SSO, audit and access requests across many kinds of resources | Its own proxy and short-lived certificates | Impersonation, by its own service account | Platform | Active (18.10, July 2026) |
| [kube-rbac-proxy](https://github.com/kube-rbac-proxy/kube-rbac-proxy) | An RBAC check (a SubjectAccessReview) in front of *another* service, such as a metrics endpoint | None | It does not proxy to the API server | Sidecar | Apache-2.0, alpha, active (0.23, September 2026) |
| [Kubernetes Dashboard](https://github.com/kubernetes-retired/dashboard) | A web UI | A token pasted into the UI | The user's token | UI | **Archived in January 2026.** Its README points to Headlamp |

No other maintained, standalone service that we found does what krm-foyer does as one
piece: a browser login, a session cookie with the token kept on the server, the user's
own token forwarded with no service-account fallback, a CSRF proof on every change, and
filtered responses. Headlamp's backend comes closest but is not packaged for other
applications. That is a negative search result, not a proof.

### When another one is the better pick

- **You want a finished dashboard, not a backend for your own application:** Headlamp.
  Its login model is the nearest to krm-foyer's. The differences are that its cookie
  holds the token itself rather than an opaque session ID, PKCE is opt-in, and a
  service-account fallback is available.
- **Many applications need only "signed in, token forwarded":** oauth2-proxy. It is
  generic and widely used. We found no CSRF protection of its own for requests to the
  upstream (only for its login flow), and it does not filter Kubernetes responses, so the
  application behind it has to handle both. [Ingress design B](ingress.md#b-the-ingress-runs-the-oidc-login-and-passes-the-users-token-to-krm-foyer)
  weighs putting it in front of krm-foyer.
- **The clients are kubectl and other CLI tools:** Pinniped, or kube-oidc-proxy when the
  API server cannot trust your issuer and a service account that impersonates is
  acceptable. Use the TremoloSecurity fork, not the archived original.
- **One developer on their own machine:** kubectl proxy. Do not expose it on a shared
  port.
- **You already run Rancher, Teleport or OpenUnison, or need multi-cluster access, audit
  or access requests:** that platform. The cost is a large component with impersonation
  rights.
- **You want RBAC to guard your own service's endpoints:** kube-rbac-proxy. It solves a
  different problem, and people often confuse the two.
- **Not for new work:** the Kubernetes Dashboard, which is archived.

## Instead of the browser helper

The helper at `/_foyer/foyer.js` is about 120 lines of dependency-free JavaScript
([source](../internal/pages/assets/foyer.js), [docs](frontend.md#getting-started-quickly)).
It reads the session, sends the browser to sign in and out, puts the CSRF header on every
change, and turns each answer into an outcome. A library could do some of that, so we
looked for one that does it all. The question was whether a published Kubernetes client
can act as the transport, using a relative base URL, the session cookie and one extra
header per change, in a browser with no Node polyfills.

| Library | In a browser? | Relative URL, cookie, CSRF header | Watch | Weight | Status (2026-10-02) | What it is good for |
| --- | --- | --- | --- | --- | --- | --- |
| [@kubernetes/client-node](https://github.com/kubernetes-client/javascript) 2.0 | No. Its build imports `node:fs`, `https`, `tls`, `net` and `child_process` | Not in practice | Yes, Node only | About 62 MB unpacked, 16 dependencies | Apache-2.0, active | The official client for Node: operators, CLIs, backends |
| [kubernetes-fluent-client](https://github.com/defenseunicorns/kubernetes-fluent-client) 3.12 (Pepr) | No. It is built on client-node, undici and node-fetch | No | Yes, Node only | Heavy | Apache-2.0, active | A typed, fluent API for Node controllers |
| [@cloudydeno/kubernetes-client](https://jsr.io/@cloudydeno/kubernetes-client) 0.8 (JSR) | No. It targets Deno or Node 20+ | No. It connects in-cluster, through kubectl, from a kubeconfig, or through kubectl proxy | Yes, with a reflector | Small | MIT, released January 2026 | Deno scripts and tools, with good generated types |
| [@openshift/dynamic-plugin-sdk-utils](https://github.com/openshift/dynamic-plugin-sdk) 5.0 | Yes | Yes. `setUtilsConfig({ appFetch })` takes your own fetch | Over WebSocket, not over a streamed HTTP response | Peer dependencies on React, Redux, react-redux, redux-thunk and the plugin SDK | Apache-2.0, active | Plugins for the OpenShift console |
| [Headlamp](https://github.com/kubernetes-sigs/headlamp) `lib/k8s` | Yes, inside Headlamp | No. It is tied to Headlamp's own backend and cluster routing | Yes | Published only inside `@kinvolk/headlamp-plugin`, with MUI, Redux and Monaco | Apache-2.0, active | Headlamp plugins |
| [kubernetes-client](https://www.npmjs.com/package/kubernetes-client) 9.0 (GoDaddy) | No. It is built on `request` | No | Node only | Old dependencies | Unmaintained | Nothing new |
| [kubernetes-models](https://github.com/tommy351/kubernetes-models-ts) (`@kubernetes-models/*`) | Yes | Not a transport | — | No cost with `import type`. The runtime classes bring ajv | MIT, active (August 2026) | **Types** for core resources and CRDs, matching the JSON as sent |
| [kubernetes-types](https://www.npmjs.com/package/kubernetes-types) 1.30 | Yes, types only | Not a transport | — | No dependencies | Apache-2.0, last release May 2024 (Kubernetes 1.30) | Types, but stale |

`k8s-client-js`, `@kubernetes-typescript/*` and `kube-fetch` are not on npm.

### What we concluded

- **No transport fits.** The Node clients do not run in a page. The clients that do run
  in a browser bring an application framework that is much larger than the helper. None
  of them knows a cookie session with a CSRF proof, so we would have to wrap them anyway.
- **The helper stays our own, small and without dependencies.** Its security-relevant
  paths (no token, a proof on every change, a resend only of krm-foyer's own
  [refusal](design.md#interruptions)) stay short enough to audit, and they are tested
  under `node --test` ([foyer.test.js](../internal/pages/foyer.test.js)).
- **Live views use krm-stream.** The helper handles sessions and API calls; the
  [hello example](../examples/hello) uses krm-stream for snapshots, recovery and drafts.
  Native watches remain available for clients that already implement that protocol;
  see [which watch to use](watches.md).
- **For types, use `kubernetes-models` with `import type`.** It costs nothing at run
  time, is current, and covers CRDs. client-node's model classes do not fit, because they
  turn timestamps into `Date` objects and no longer match the JSON.

Look again when an application needs the helper outside a krm-foyer origin, or when a
browser-first client appears that takes your own `fetch` and has no framework attached.
