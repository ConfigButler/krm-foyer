#!/usr/bin/env bash
# Removes everything start-cluster.sh created, including the generated certificates.
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-krm-foyer-e2e}"
NETWORK="${NETWORK:-krm-foyer-e2e}"
here="$(cd "$(dirname "$0")" && pwd)"
E2E_DIR="${E2E_DIR:-$(cd "$here/../../.." && pwd)/.e2e}"

k3d cluster delete "$CLUSTER_NAME" >/dev/null 2>&1 || true
docker rm -f "${CLUSTER_NAME}-dex" >/dev/null 2>&1 || true
if [ -f /.dockerenv ]; then
  docker network disconnect "$NETWORK" "$(hostname)" >/dev/null 2>&1 || true
fi
docker network rm "$NETWORK" >/dev/null 2>&1 || true
docker volume rm "${CLUSTER_NAME}-config" >/dev/null 2>&1 || true
rm -rf "$E2E_DIR"
echo "fixture removed"
