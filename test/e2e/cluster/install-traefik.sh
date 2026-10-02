#!/usr/bin/env bash
# Installs Traefik into the e2e fixture as its Gateway API implementation: the official
# chart, at the version gitops-reverser's e2e uses, with traefik-values.yaml. Run after
# start-cluster.sh; safe to rerun (helm upgrade --install).
#
# k3s ships a Traefik of its own; start-cluster.sh disables it, so the k3s installation
# stays minimal and everything on top of it is installed here, by script, at pinned
# versions. The chart also installs the Gateway API CRDs (v1.4.0, standard channel).
set -euo pipefail

CHART="oci://ghcr.io/traefik/helm/traefik"
# https://github.com/traefik/traefik-helm-chart/releases/tag/v39.0.5; pinned by digest too.
CHART_VERSION="39.0.5"
CHART_DIGEST="sha256:16c74e2926fa9f5b063d0a48c1595e974710dfb70519943e1e124b6f8c93c14b"

here="$(cd "$(dirname "$0")" && pwd)"
E2E_DIR="${E2E_DIR:-$(cd "$here/../../.." && pwd)/.e2e}"
export KUBECONFIG="$E2E_DIR/kubeconfig"

echo "== traefik $CHART_VERSION"
helm upgrade --install traefik "$CHART:$CHART_VERSION@$CHART_DIGEST" \
  --namespace traefik-system --create-namespace \
  --values "$here/traefik-values.yaml" \
  --wait --timeout 3m >/dev/null
kubectl wait --for=condition=Accepted gatewayclass/traefik --timeout=60s >/dev/null
echo "traefik ready: GatewayClass traefik"
