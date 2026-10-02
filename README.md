# krm-foyer

**The browser's way in to Kubernetes: login, API access and live krm-stream resources.**

krm-foyer is a backend for frontend (BFF) for browser applications built on Kubernetes
APIs. It owns OIDC login and server-side sessions, proxies Kubernetes API requests with
the user's own credential, and hosts
[krm-stream](https://github.com/ConfigButler/krm-stream) resource streams on the same
origin as your frontend. The browser never holds a cluster credential.

KRM is the [Kubernetes Resource Model](https://github.com/kubernetes/design-proposals-archive/blob/main/architecture/resource-management.md):
the idea that everything is a declarative resource with a spec and a status. krm-foyer is
for applications whose domain is modelled that way.

> **Status: a working prototype, not yet for a cluster that matters.** Sign-in through
> OIDC, server-side sessions and the API proxy work, and e2e specs against a real API
> server and Dex try to get past each security boundary. `task demo` runs an example
> application against them in your browser. Still missing: the bounds on request rate and
> response size that make it safe in front of a real cluster, token refresh, more than
> one replica, and streams. The [roadmap](docs/roadmap.md) tracks which of the principles
> below have tests.

## Try it

In the devcontainer:

```bash
task demo
```

This starts a disposable k3d cluster with Dex, krm-foyer and the
[hello example](examples/hello) in it, and port-forwards the example and Dex into the
devcontainer, where VS Code forwards them to your machine, also when Docker runs elsewhere. Open <https://foyer.localhost:8443> and
sign in as `alice@example.com`, who may edit the notes, or `bob@example.com`, who may only
read them. The password is `password`. The certificates come from the fixture's own CA, so
import `.e2e/ca.crt` into your browser or accept the warnings. `task e2e-down` removes it
all. A fixture made by an older version of the scripts is refused with a message saying
so; run `task e2e-down` once, then `task demo` again.

The example is one HTML file and one script with no backend of its own: everything it
does goes through `/k8s` as the signed-in user, and the 403 and 409 it shows are
Kubernetes' own answers.

## Principles

- **The issuer decides who you are; Kubernetes decides what you may do.** Every request
  reaches the API server with the user's own OIDC token, and RBAC and admission answer it.
  No impersonation, and no fallback to krm-foyer's service account.
- **Tokens stay on the server.** The browser holds only an opaque session ID in a Secure,
  HttpOnly cookie. Frontend code never sees a token. The session ID is itself a bearer
  credential, and is guarded like one.
- **One domain is one trust boundary.** The application, krm-foyer and any domain backend
  share an origin, routed by path. Everything on that domain can act as the signed-in
  user, so host only what you would trust with that access.
  [More](docs/ingress.md#what-a-shared-origin-costs)
- **No access rules of our own.** krm-foyer has no allowlist: what RBAC allows the user
  is reachable, and what it refuses is refused by the API server. So a session carries the
  user's full Kubernetes access; give users grants that match the application.
  [Why, and what a scope would add](docs/application-scope.md)
- **Kubernetes semantics, exactly.** Status codes, errors, patch types and conflicts pass
  through unchanged, and nothing is retried on the user's behalf.
  [Why](docs/heritage.md#what-broke-on-stage)

## Documentation

| Document | What it answers |
| --- | --- |
| [docs/vision.md](docs/vision.md) | Why krm-foyer exists, who it helps, what it expects from your domain, and what it will not become |
| [docs/design.md](docs/design.md) | The contract: routes, access, sessions, upstream responses, streams and release criteria |
| [docs/application-scope.md](docs/application-scope.md) | Why a browser application might be limited beyond RBAC, what that would block, and why krm-foyer starts without it |
| [docs/roadmap.md](docs/roadmap.md) | What exists, what is next, and in what order |
| [docs/bff-choice.md](docs/bff-choice.md) | Whether your application should use a universal BFF like this one, a domain backend, or both |
| [docs/ingress.md](docs/ingress.md) | Terminating TLS itself or behind an ingress, sharing one domain with other services, and the login gate for an ingress |
| [docs/frontend.md](docs/frontend.md) | Which pages krm-foyer serves itself, and why it ships no single-page application |
| [docs/testing.md](docs/testing.md) | How the tests prove krm-foyer invents neither authentication nor authorization, and how to run the e2e fixture |
| [docs/investigations/choosing-an-issuer.md](docs/investigations/choosing-an-issuer.md) | Which OIDC issuers fit krm-foyer (Dex, Pinniped, Keycloak, authentik, Authelia, OpenUnison), and why the tests use Dex |
| [docs/heritage.md](docs/heritage.md) | Where it comes from: the Voter demo, what broke on stage, and its sibling projects |
| [docs/name.md](docs/name.md) | Why it is called krm-foyer |

## Run it

```bash
task run          # http://localhost:8080
task verify       # everything CI checks, including e2e against k3d and Dex
```

Without flags, krm-foyer serves its start page and probes only. Sign-in and the API
proxy come together, and need:

| Flag | |
| --- | --- |
| `-public-url` | krm-foyer's origin as browsers reach it, such as `https://app.example.com` |
| `-oidc-issuer`, `-oidc-client-id`, `-oidc-client-secret-file` | The issuer the API server trusts, and krm-foyer's client there |
| `-kubernetes-server` | The API server, such as `https://kubernetes.default.svc` |

Optional: `-oidc-ca-file` and `-kubernetes-ca-file` (CAs to trust), `-oidc-scopes`,
`-session-idle-timeout` (1h), `-session-absolute-timeout` (8h), `-tls-cert-file` and
`-tls-key-file` to serve TLS, and `-listen` (`:8080`). Sessions live in memory: run one
replica.

Or as a container:

```bash
task image
docker run --rm -p 8080:8080 ghcr.io/configbutler/krm-foyer:$(git describe --tags --always)
```

The repository comes with a devcontainer that has Go, Task, the linters CI runs, and
k3d/kubectl for testing against a real API server. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[Apache 2.0](LICENSE)
