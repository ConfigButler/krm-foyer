# krm-foyer

**The browser's way in to Kubernetes: login, API access and live krm-stream resources.**

krm-foyer is a backend for frontend (BFF) for browser applications built on Kubernetes
APIs. It owns OIDC login and server-side sessions, proxies an allowlisted set of Kubernetes
API routes with the user's own credential, and hosts
[krm-stream](https://github.com/ConfigButler/krm-stream) resource streams on the same
origin as your frontend. The browser never holds a cluster credential.

KRM is the [Kubernetes Resource Model](https://github.com/kubernetes/design-proposals-archive/blob/main/architecture/resource-management.md):
the idea that everything is a declarative resource with a spec and a status. krm-foyer is
for applications whose domain is modelled that way.

> **Status: a design and a server skeleton.** The skeleton serves health endpoints and a
> start page, and an e2e fixture puts a real API server behind a real Dex. Login, the API
> proxy and streams are specified in [docs/design.md](docs/design.md) but not implemented.
> The principles below are requirements, not properties of the code yet; the
> [roadmap](docs/roadmap.md) tracks which have tests.

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
- **Default deny, and krm-foyer only narrows.** With an empty allowlist, nothing is
  exposed. The allowlist can only take away what RBAC grants, never add to it.
- **Kubernetes semantics, exactly.** Status codes, errors, patch types and conflicts pass
  through unchanged, and nothing is retried on the user's behalf.
  [Why](docs/heritage.md#what-broke-on-stage)

## Documentation

| Document | What it answers |
| --- | --- |
| [docs/vision.md](docs/vision.md) | Why krm-foyer exists, who it helps, what it expects from your domain, and what it will not become |
| [docs/design.md](docs/design.md) | The contract: routes, access boundaries, sessions, upstream responses, streams and release criteria |
| [docs/roadmap.md](docs/roadmap.md) | What exists, what is next, and in what order |
| [docs/bff-choice.md](docs/bff-choice.md) | Whether your application should use a universal BFF like this one, a domain backend, or both |
| [docs/ingress.md](docs/ingress.md) | Terminating TLS itself or behind an ingress, sharing one domain with other services, and the login gate for an ingress |
| [docs/frontend.md](docs/frontend.md) | Which pages krm-foyer serves itself, and why it ships no single-page application |
| [docs/testing.md](docs/testing.md) | How the tests prove krm-foyer invents neither authentication nor authorization, and how to run the e2e fixture |
| [docs/heritage.md](docs/heritage.md) | Where it comes from: the Voter demo, what broke on stage, and its sibling projects |
| [docs/name.md](docs/name.md) | Why it is called krm-foyer |

## Run it

```bash
task run          # http://localhost:8080
task verify       # everything CI checks, including e2e against k3d and Dex
```

Or as a container:

```bash
task image
docker run --rm -p 8080:8080 ghcr.io/configbutler/krm-foyer:$(git describe --tags --always)
```

The repository comes with a devcontainer that has Go, Task, the linters CI runs, and
k3d/kubectl for testing against a real API server. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[Apache 2.0](LICENSE)
