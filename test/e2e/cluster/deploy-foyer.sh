#!/usr/bin/env bash
# Deploys krm-foyer into the e2e fixture that start-cluster.sh brought up, with the chart
# (charts/krm-foyer), as two releases: krm-foyer (foyer-values.yaml) and krm-foyer-brief
# (foyer-brief-values.yaml on top). Safe to rerun: a Deployment rolls only when the
# image, the chart, the certificate or a secret changed.
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
# Its Service's name, for an ingress that calls krm-foyer itself: Traefik's ForwardAuth
# to /auth/check (traefik-routes.yaml) verifies krm-foyer's certificate by this name.
FOYER_SERVICE_HOST="krm-foyer.krm-foyer.svc"
# Browsers reach krm-foyer through the front door on this port (front-door.sh), so it is
# part of the public URL; the suite goes to the NodePort directly, under the same name.
FOYER_URL="https://$FOYER_HOST:8443"
NODE_PORT=30443
# A second krm-foyer whose sessions end within a minute (foyer-brief-values.yaml).
BRIEF_NODE_PORT=30444

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../../.." && pwd)"
E2E_DIR="${E2E_DIR:-$repo/.e2e}"
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
# Outside apiserver/, which the API server mounts: a new krm-foyer certificate must
# not restart them.
mkdir -p "$E2E_DIR/foyer"
if [ ! -f "$E2E_DIR/foyer/tls.crt" ] \
  || ! openssl x509 -checkend 86400 -noout -in "$E2E_DIR/foyer/tls.crt" >/dev/null 2>&1 \
  || ! openssl verify -CAfile "$E2E_DIR/ca.crt" "$E2E_DIR/foyer/tls.crt" >/dev/null 2>&1 \
  || ! openssl x509 -noout -checkhost "$FOYER_HOST" -in "$E2E_DIR/foyer/tls.crt" | grep -q 'does match' \
  || ! openssl x509 -noout -checkhost "$FOYER_SERVICE_HOST" -in "$E2E_DIR/foyer/tls.crt" | grep -q 'does match'; then
  openssl req -newkey rsa:2048 -nodes -subj "/CN=$FOYER_HOST" \
    -keyout "$E2E_DIR/foyer/tls.key" -out "$E2E_DIR/foyer/tls.csr" 2>/dev/null
  openssl x509 -req -in "$E2E_DIR/foyer/tls.csr" -CA "$E2E_DIR/ca.crt" -CAkey "$E2E_DIR/ca.key" \
    -CAcreateserial -days 30 -extfile <(printf 'subjectAltName=DNS:%s,DNS:%s' "$FOYER_HOST" "$FOYER_SERVICE_HOST") \
    -out "$E2E_DIR/foyer/tls.crt" 2>/dev/null
fi

echo "== krm-foyer"
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: krm-foyer
EOF
# A fixture from before the chart has the same objects, applied with kubectl, which Helm
# refuses to take over. Remove them once.
if kubectl -n krm-foyer get deployment krm-foyer >/dev/null 2>&1 \
  && [ "$(kubectl -n krm-foyer get deployment krm-foyer -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}')" != Helm ]; then
  echo "removing the krm-foyer applied before the chart"
  kubectl -n krm-foyer delete deployment,service --all >/dev/null
  kubectl -n krm-foyer delete serviceaccount krm-foyer krm-foyer-shared --ignore-not-found >/dev/null
  kubectl -n krm-foyer delete secret krm-foyer-shared-token --ignore-not-found >/dev/null
  kubectl delete clusterrole krm-foyer-shared-watches --ignore-not-found >/dev/null
  kubectl delete clusterrolebinding krm-foyer-shared-watches krm-foyer-shared-reviews --ignore-not-found >/dev/null
fi
apply() { kubectl -n krm-foyer "$@" --dry-run=client -o yaml | kubectl apply -f - >/dev/null; }
apply create secret tls krm-foyer-tls --cert "$E2E_DIR/foyer/tls.crt" --key "$E2E_DIR/foyer/tls.key"
# The client secret of the static client in dex.yaml.
apply create secret generic krm-foyer-oidc --from-literal=client-secret=krm-foyer-e2e-secret
apply create configmap krm-foyer-issuer-ca --from-file=ca.crt="$E2E_DIR/ca.crt"
# Session keys, one per release, made once and never replaced: a redeploy keeps every
# session, as an upgrade must.
for keys in krm-foyer-session-keys krm-foyer-brief-session-keys; do
  if ! kubectl -n krm-foyer get secret "$keys" >/dev/null 2>&1; then
    kubectl -n krm-foyer create secret generic "$keys" \
      --from-literal=session-keys="$(head -c 32 /dev/urandom | base64)" >/dev/null
  fi
done
kubectl apply -f "$here/foyer-bait.yaml" >/dev/null
config_hash="$(cat "$E2E_DIR/foyer/tls.crt" "$E2E_DIR/ca.crt" | sha256sum | cut -c1-16)"
# install RELEASE VALUES...: the chart, with the image imported above. A new certificate
# or secret changes the config hash, which restarts the pod.
install() {
  local release="$1"
  shift
  local values=()
  for v in "$@"; do values+=(-f "$here/$v"); done
  helm upgrade --install "$release" "$repo/charts/krm-foyer" --namespace krm-foyer "${values[@]}" \
    --set-string image.repository="${tag%:*}" --set-string image.tag="${tag#*:}" \
    --set-string "podAnnotations.krm-foyer\.test/config-hash=$config_hash" >/dev/null
  if ! kubectl -n krm-foyer rollout status "deployment/$release" --timeout=120s; then
    kubectl -n krm-foyer describe pods -l "app.kubernetes.io/instance=$release" >&2
    kubectl -n krm-foyer logs "deployment/$release" --tail=50 >&2 || true
    exit 1
  fi
}
# The brief release uses the main release's service accounts, so it goes second.
install krm-foyer foyer-values.yaml
install krm-foyer-brief foyer-values.yaml foyer-brief-values.yaml

server_ip="${API_SERVER#https://}"
server_ip="${server_ip%:*}"
cat > "$E2E_DIR/foyer-env" <<EOF
FOYER_URL=$FOYER_URL
FOYER_ADDR=$server_ip:$NODE_PORT
FOYER_BRIEF_ADDR=$server_ip:$BRIEF_NODE_PORT
FOYER_NAMESPACE=krm-foyer
FOYER_SERVICE_ACCOUNT=system:serviceaccount:krm-foyer:krm-foyer
FOYER_SHARED_ACCOUNT=system:serviceaccount:krm-foyer:krm-foyer-shared
EOF
echo "krm-foyer ready at $FOYER_URL ($server_ip:$NODE_PORT)"
