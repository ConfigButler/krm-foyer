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

> **Status: design proposal plus a server skeleton.** The skeleton serves health
> endpoints and a start page. Login, the API proxy and streams are specified in
> [docs/design.md](docs/design.md) but not implemented yet.

## Documentation

| Document | What it answers |
| --- | --- |
| [docs/design.md](docs/design.md) | The service contract: routes, access boundaries, sessions, streams and release criteria |
| [docs/bff-choice.md](docs/bff-choice.md) | Whether your application should use a universal BFF like this one, a domain backend, or both |
| [docs/frontend.md](docs/frontend.md) | Which pages krm-foyer serves itself, and why it ships no single-page application |
| [docs/name.md](docs/name.md) | Why it is called krm-foyer |

## Run it

```bash
task run          # http://localhost:8080
task verify       # everything CI checks
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
