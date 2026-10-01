# Working in this repository

- `task verify` is the gate. Run it before saying a change is done. CI runs the same tasks.
- [docs/design.md](docs/design.md) is the contract. If code and document disagree, one of
  them is a bug; fix the one that is wrong in the same change.
- Commit messages are conventional commits, and the prefix is load-bearing: release-please
  cuts versions from it (`fix:` patch, `feat:` minor, `feat!:` breaking).
- Security boundaries (tokens never reach the browser, default-deny allowlist, no service
  account fallback) need tests that try to get past them, not only tests of the happy path.
- Pin new GitHub Actions by full commit SHA with the version in a comment, and new base
  images by digest. Tool versions live in the ENV block of `.devcontainer/Dockerfile`.
- CI runs every check inside the `ci` stage of `.devcontainer/Dockerfile` (see
  `.github/workflows/ci.yml`). A tool a check needs goes into that stage, never into a
  workflow step that installs it on the runner.
- The repository only squash-merges, and the squash commit takes the PR title, so PR titles
  are conventional commits too. `.github/workflows/pr-title.yml` checks them.
- In the devcontainer, Docker runs beside the container (docker-outside-of-docker). A port
  published with `docker run -p` is on the host, not reachable at `localhost` from here;
  use a Docker network, as `task image-smoke` does.
