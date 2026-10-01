#!/usr/bin/env bash
# Deploys krm-foyer into the e2e fixture that start-cluster.sh brought up. Safe to rerun:
# the Deployment rolls only when the image, the certificate or a secret changed.
#
# IMAGE is the image to deploy, as `task image` built it. It is imported into the
# cluster with `k3d image import` under a tag derived from its ID, so a rebuilt image is
# a new tag and nothing is ever pulled.
#
# Writes E2E_DIR/foyer-env: where the suite reaches krm-foyer.
set -euo pipefail

: "${IMAGE:?set IMAGE to the krm-foyer image to deploy}"
CLUSTER_NAME="${CLUSTER_NAME:-krm-foyer-e2e}"
FOYER_HOST="foyer.localhost"
# Browsers reach krm-foyer through the front door on this port (front-door.sh), so it is
# part of the public URL; the suite goes to the NodePort directly, under the same name.
FOYER_URL="https://$FOYER_HOST:8443"
NODE_PORT=30443

here="$(cd "$(dirname "$0")" && pwd)"
E2E_DIR="${E2E_DIR:-$(cd "$here/../../.." && pwd)/.e2e}"
[ -f "$E2E_DIR/env" ] || { echo "no fixture; run task e2e-up first" >&2; exit 1; }
# shellcheck source=/dev/null
. "$E2E_DIR/env"
export KUBECONFIG="$E2E_DIR/kubeconfig"

echo "== image"
id="$(docker image inspect --format '{{.Id}}' "$IMAGE" | cut -d: -f2 | cut -c1-12)"
tag="krm-foyer:e2e-$id"
docker tag "$IMAGE" "$tag"
k3d image import --cluster "$CLUSTER_NAME" "$tag" >/dev/null

echo "== certificate for $FOYER_HOST"
# Outside config/, which Dex and the API server mount: a new krm-foyer certificate must
# not restart them.
mkdir -p "$E2E_DIR/foyer"
if [ ! -f "$E2E_DIR/foyer/tls.crt" ] \
  || ! openssl x509 -checkend 86400 -noout -in "$E2E_DIR/foyer/tls.crt" >/dev/null 2>&1 \
  || ! openssl verify -CAfile "$E2E_DIR/ca.crt" "$E2E_DIR/foyer/tls.crt" >/dev/null 2>&1 \
  || ! openssl x509 -noout -checkhost "$FOYER_HOST" -in "$E2E_DIR/foyer/tls.crt" | grep -q 'does match'; then
  openssl req -newkey rsa:2048 -nodes -subj "/CN=$FOYER_HOST" \
    -keyout "$E2E_DIR/foyer/tls.key" -out "$E2E_DIR/foyer/tls.csr" 2>/dev/null
  openssl x509 -req -in "$E2E_DIR/foyer/tls.csr" -CA "$E2E_DIR/ca.crt" -CAkey "$E2E_DIR/ca.key" \
    -CAcreateserial -days 30 -extfile <(printf 'subjectAltName=DNS:%s' "$FOYER_HOST") \
    -out "$E2E_DIR/foyer/tls.crt" 2>/dev/null
fi

echo "== krm-foyer"
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: krm-foyer
EOF
apply() { kubectl -n krm-foyer "$@" --dry-run=client -o yaml | kubectl apply -f - >/dev/null; }
apply create secret tls krm-foyer-tls --cert "$E2E_DIR/foyer/tls.crt" --key "$E2E_DIR/foyer/tls.key"
# The client secret of the static client in dex.yaml.
apply create secret generic krm-foyer-oidc --from-literal=client-secret=krm-foyer-e2e-secret
apply create configmap krm-foyer-issuer-ca --from-file=ca.crt="$E2E_DIR/ca.crt"
config_hash="$(cat "$E2E_DIR/foyer/tls.crt" "$E2E_DIR/ca.crt" | sha256sum | cut -c1-16)"
sed -e "s|IMAGE|$tag|" -e "s|CONFIG_HASH|$config_hash|" "$here/foyer.yaml" | kubectl apply -f - >/dev/null
if ! kubectl -n krm-foyer rollout status deployment/krm-foyer --timeout=120s; then
  kubectl -n krm-foyer describe pods >&2
  kubectl -n krm-foyer logs deployment/krm-foyer --tail=50 >&2 || true
  exit 1
fi

server_ip="${API_SERVER#https://}"
server_ip="${server_ip%:*}"
cat > "$E2E_DIR/foyer-env" <<EOF
FOYER_URL=$FOYER_URL
FOYER_ADDR=$server_ip:$NODE_PORT
FOYER_NAMESPACE=krm-foyer
FOYER_SERVICE_ACCOUNT=system:serviceaccount:krm-foyer:krm-foyer
EOF
echo "krm-foyer ready at $FOYER_URL ($server_ip:$NODE_PORT)"
