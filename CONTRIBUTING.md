# Contributing

## Prerequisites

Open the repository in its devcontainer. It provides Go, Task, golangci-lint, actionlint,
hadolint, `kubectl` and `k3d`, at the versions CI uses.

```bash
task          # list every task
task test     # go test -race ./...
task lint     # go vet, golangci-lint, actionlint, hadolint
task test-e2e # a real API server trusting a real Dex; see docs/testing.md
task verify   # the whole gate, in CI's order; if this passes, CI passes
```

## Design rules

- The service contract is [docs/design.md](docs/design.md). A change to a route's behavior
  changes that document in the same pull request.
- Default deny. An empty configuration exposes no Kubernetes API and no stream scope.
- The browser never receives a bearer token, and the service never falls back to its own
  service account for a user's request.
- Preserve Kubernetes semantics: status codes, `Status` errors, patch types and
  concurrency preconditions pass through unchanged. Never replay a mutation.
- No application-specific endpoints. Domain logic belongs in the domain's operator and
  admission, not here. See [docs/bff-choice.md](docs/bff-choice.md).
- krm-foyer's own pages are server-rendered and have no JavaScript build. See
  [docs/frontend.md](docs/frontend.md).

## Commits and releases

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/), because
release-please reads them to cut versions: `fix:` is a patch, `feat:` a minor and `feat!:`
a breaking change. Before 1.0 a breaking change bumps the minor version.

Pushes to `main` keep a release pull request up to date. Merging it tags the release, and
the image `ghcr.io/configbutler/krm-foyer:<version>` is published once CI has passed on
that commit.

## Style

- Format with `task fmt` (gofmt and goimports).
- Comments explain constraints and non-obvious choices, not what the next line does.
- Test the guarantee a change promises, not only the happy path. Security boundaries need
  tests that try to get past them.
