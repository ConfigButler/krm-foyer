#!/usr/bin/env bash
# Runs once, when the container is created. Idempotent: a rebuild re-runs it.
set -euo pipefail

log() { echo "[post-create] $*"; }

workspace_dir="${1:-${containerWorkspaceFolder:-$(pwd)}}"
log "workspace: ${workspace_dir}"

# ---------------------------------------------------------------- git identity --
# Reported, not fatal: a contributor without one still gets a working container,
# and Git itself refuses the commit that needs it.
git_name="$(git config --get user.name || true)"
git_email="$(git config --get user.email || true)"
[ -z "${git_name}" ] && git_name="${GIT_USER_NAME:-}"
[ -z "${git_email}" ] && git_email="${GIT_USER_EMAIL:-}"
if [ -n "${git_name}" ]; then
  git config --global --get user.name >/dev/null 2>&1 || git config --global user.name "${git_name}"
fi
if [ -n "${git_email}" ]; then
  git config --global --get user.email >/dev/null 2>&1 || git config --global user.email "${git_email}"
fi
if [ -z "${git_name}" ] || [ -z "${git_email}" ]; then
  log "warning: no Git identity. Set user.name/user.email, or pass GIT_USER_NAME/GIT_USER_EMAIL."
fi
git config --global --add safe.directory "${workspace_dir}"

# Signing is configured by post-start.sh, which runs right after this on the first
# start and is where a forwarded SSH agent can be expected to exist.

# ------------------------------------------------------- home ownership (READ ME) --
# Every named volume under /home/vscode is created by Docker as ROOT-owned, because
# the image has no such directory to inherit ownership from. Without this chown,
# `claude` and `codex` cannot log in, `gh auth login` cannot store a token and
# kubectl cannot write a kubeconfig, with no hint why.
log "ensuring cache dirs exist, then fixing ownership under /home/vscode"
sudo mkdir -p \
  /home/vscode/.cache/go-build \
  /home/vscode/.cache/golangci-lint \
  /home/vscode/.claude \
  /home/vscode/.codex \
  /home/vscode/.config/gh \
  /home/vscode/.kube \
  /home/vscode/persisted-home
sudo chown -R vscode:vscode /home/vscode || true
[ -d "${workspace_dir}" ] && sudo chown -R vscode:vscode "${workspace_dir}" || true

# Claude Code stores its credentials in ~/.claude.json, outside ~/.claude/. Symlink it
# into the persisted-home volume so a rebuild does not sign you out.
touch /home/vscode/persisted-home/.claude.json
rm -f /home/vscode/.claude.json
ln -s /home/vscode/persisted-home/.claude.json /home/vscode/.claude.json

# --------------------------------------------------------------------- the repo --
log "go modules"
(cd "${workspace_dir}" && go mod download)

cat <<'MSG'

  krm-foyer is ready.

    task           list every task
    task run       start the server on :8080
    task test      go test -race
    task lint      go vet, golangci-lint, actionlint, hadolint
    task verify    everything CI checks, in CI's order

  The service contract lives in docs/design.md.

MSG
log "done"
