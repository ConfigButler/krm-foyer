# Devcontainer for krm-foyer: a Go service that sits between a browser, an OIDC
# provider and a Kubernetes API server.
#
# Adapted from krm-stream's devcontainer, so both repositories build with the same
# Go and lint the same way. What it needs beyond Go: Task, the linters CI runs, and
# k3d + kubectl for proving the proxy against a real API server rather than a fake.
#
# Stage 1 (ci): everything `task verify` needs. CI runs every check inside this stage
#                (see .github/workflows/ci.yml), so it is the one place tools come from.
# Stage 2 (dev): editor tooling, Node (for agent CLIs)
#                and the non-root `vscode` user.

# Base pinned by digest: the same image krm-stream and the Dockerfile at the repo root build on.
FROM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS ci

# NODE_MAJOR -> the current Node LTS line, for the browser helper's tests (`node --test`,
#   no npm packages). Set here rather than in the tool block below because the apt step
#   that installs it comes first.
ENV DEBIAN_FRONTEND=noninteractive \
    NODE_MAJOR=24
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# shellcheck is here for actionlint, which shells out to it for every `run:` block and
# silently skips that half of its job when it is missing.
RUN apt-get update \
    && apt-get -y install --no-install-recommends \
    git curl ca-certificates vim less jq sudo acl gnupg unzip openssh-client shellcheck \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
       -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && chmod a+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
       > /etc/apt/sources.list.d/github-cli.list \
    && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
       > /etc/apt/sources.list.d/docker.list \
    && curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
       | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg \
    && chmod a+r /etc/apt/keyrings/nodesource.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_${NODE_MAJOR}.x nodistro main" \
       > /etc/apt/sources.list.d/nodesource.list \
    && apt-get update \
    && apt-get -y install --no-install-recommends gh docker-ce-cli docker-buildx-plugin nodejs \
    && apt-get autoremove -y && apt-get clean -y && rm -rf /var/lib/apt/lists/*

# Tool versions, in one place. CI runs inside this stage, so a bump here is a bump
# everywhere.
# GOLANGCI_LINT_VERSION -> https://github.com/golangci/golangci-lint/releases
# TASK_VERSION          -> https://github.com/go-task/task/releases
# KUBECTL_VERSION / K3D_VERSION -> kept in step with krm-stream, so a local cluster
#   behaves like the one the stream gateway is tested against.
# ACTIONLINT_VERSION    -> https://github.com/rhysd/actionlint/releases
# HADOLINT_VERSION      -> https://github.com/hadolint/hadolint/releases
# HELM_VERSION / HELM_SHA256 -> https://github.com/helm/helm/releases; the checksum is
#   get.helm.sh/helm-<version>-linux-amd64.tar.gz.sha256sum. The e2e fixture installs
#   Traefik with it.
ENV PATH="/go/bin:/usr/local/go/bin:${PATH}" \
    GOLANGCI_LINT_VERSION=v2.13.2 \
    TASK_VERSION=v3.53.1 \
    KUBECTL_VERSION=v1.37.0 \
    K3D_VERSION=v5.9.0 \
    ACTIONLINT_VERSION=1.7.12 \
    HADOLINT_VERSION=2.15.1 \
    HELM_VERSION=v4.3.0 \
    HELM_SHA256=86584a54def73570558f66f5111cc53dfed56689637ae32c1201205d494f54fb

RUN test "$(dpkg --print-architecture)" = "amd64" \
    || (echo "This devcontainer currently supports amd64 only." && exit 1)

RUN curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b /usr/local/bin ${GOLANGCI_LINT_VERSION}

RUN curl -fsSL "https://github.com/go-task/task/releases/download/${TASK_VERSION}/task_linux_amd64.tar.gz" \
    | tar -xzO task > /usr/local/bin/task && chmod +x /usr/local/bin/task

RUN curl -fsSL -o /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
    && chmod +x /usr/local/bin/kubectl

RUN curl -fsSL -o /usr/local/bin/k3d "https://github.com/k3d-io/k3d/releases/download/${K3D_VERSION}/k3d-linux-amd64" \
    && chmod +x /usr/local/bin/k3d

RUN curl -fsSL "https://github.com/rhysd/actionlint/releases/download/v${ACTIONLINT_VERSION}/actionlint_${ACTIONLINT_VERSION}_linux_amd64.tar.gz" \
    | tar -xzO actionlint > /usr/local/bin/actionlint \
    && chmod +x /usr/local/bin/actionlint \
    && actionlint --version

RUN curl -fsSL -o /tmp/helm.tar.gz "https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz" \
    && echo "${HELM_SHA256}  /tmp/helm.tar.gz" | sha256sum -c - \
    && tar -xzO -f /tmp/helm.tar.gz linux-amd64/helm > /usr/local/bin/helm \
    && chmod +x /usr/local/bin/helm \
    && rm /tmp/helm.tar.gz \
    && helm version --short

RUN curl -fsSL -o /usr/local/bin/hadolint "https://github.com/hadolint/hadolint/releases/download/v${HADOLINT_VERSION}/hadolint-Linux-x86_64" \
    && chmod +x /usr/local/bin/hadolint \
    && hadolint --version

WORKDIR /workspaces

# Shared Go dir access for both root (build) and vscode (dev).
RUN groupadd --gid 2000 godev \
    && chgrp -R godev /go && chmod -R 2775 /go \
    && setfacl -d -m g:godev:rwx /go && setfacl -d -m u::rwx /go && setfacl -d -m o::rx /go

# hadolint ignore=SC2016
RUN printf '%s\n' 'export PATH="/go/bin:/usr/local/go/bin:${PATH}"' \
    > /etc/profile.d/go-toolchain-path.sh && chmod 0644 /etc/profile.d/go-toolchain-path.sh

ENV DEBIAN_FRONTEND=dialog
CMD ["/bin/bash"]

# --------------------------------------------------------------------- dev ---
FROM ci AS dev

USER root
ENV DEBIAN_FRONTEND=noninteractive
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# Node comes from the ci stage (NODE_MAJOR there); the Codex CLI below needs it too.
ENV DLV_VERSION=v1.27.2 \
    GOPLS_VERSION=v0.23.0 \
    GOIMPORTS_VERSION=v0.50.0 \
    STATICCHECK_VERSION=v0.8.1 \
    CODEX_VERSION=0.154.0

RUN apt-get update \
    && apt-get -y install --no-install-recommends bash-completion \
    && apt-get autoremove -y && apt-get clean -y && rm -rf /var/lib/apt/lists/*

# Codex CLI. npm's prefix is /usr, in the container's writable layer, so it has to be baked
# into the image; installed at post-create, a rebuild would silently lose it. Its login lands
# in ~/.codex, which devcontainer.json mounts as a volume.
RUN npm install -g "@openai/codex@${CODEX_VERSION}" \
    && npm cache clean --force \
    && codex --version

# hadolint ignore=SC2016
RUN printf '%s\n' \
    '' \
    'if ! shopt -oq posix; then' \
    '  if [ -f /usr/share/bash-completion/bash_completion ]; then' \
    '    . /usr/share/bash-completion/bash_completion' \
    '  fi' \
    '  if command -v task >/dev/null 2>&1 && ! complete -p task >/dev/null 2>&1; then' \
    '    eval "$(task --completion bash)"' \
    '  fi' \
    'fi' \
    >> /etc/bash.bashrc

RUN go install github.com/go-delve/delve/cmd/dlv@${DLV_VERSION} \
    && go install golang.org/x/tools/gopls@${GOPLS_VERSION} \
    && go install golang.org/x/tools/cmd/goimports@${GOIMPORTS_VERSION} \
    && go install honnef.co/go/tools/cmd/staticcheck@${STATICCHECK_VERSION}

RUN groupadd --gid 1000 vscode \
    && useradd --uid 1000 --gid vscode --shell /bin/bash --create-home vscode \
    && echo 'vscode ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers.d/vscode \
    && chmod 0440 /etc/sudoers.d/vscode \
    && usermod -aG godev vscode \
    && groupadd -f docker && usermod -aG docker vscode

RUN chown -R vscode:vscode /workspaces && chmod -R 755 /workspaces

USER vscode
ENV DEBIAN_FRONTEND=dialog
CMD ["/bin/bash"]
