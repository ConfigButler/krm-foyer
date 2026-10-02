#!/usr/bin/env bash
# Puts the hello example in front of krm-foyer, so a person can use it from a browser:
# the example's resources and grants, and the front door, nginx in the cluster that serves
# examples/hello/web at / and sends krm-foyer's prefixes to krm-foyer (front-door.yaml).
# Then port-forward.sh forwards the front door and Dex to this container's loopback, where
# a browser finds https://foyer.localhost:8443. Run after deploy-foyer.sh; safe to rerun.
set -euo pipefail

# The same nginx as the test issuer in start-cluster.sh.
NGINX_IMAGE="nginx:1.31.6-alpine@sha256:df221db836e1754089190208cee7eeda94f233197056426eda74a43ab1abeac2"

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../../.." && pwd)"
hello="$repo/examples/hello"
E2E_DIR="${E2E_DIR:-$repo/.e2e}"
[ -f "$E2E_DIR/foyer-env" ] || { echo "krm-foyer is not deployed; run task e2e-deploy first" >&2; exit 1; }
# shellcheck source=/dev/null
. "$E2E_DIR/foyer-env"
export KUBECONFIG="$E2E_DIR/kubeconfig"

echo "== the hello example's resources"
kubectl apply -f "$hello/manifests.yaml" >/dev/null
kubectl wait --for=condition=Established crd/notes.hello.krm-foyer.example --timeout=60s >/dev/null
# Created only when missing, so a rerun keeps what people wrote.
if ! kubectl create -f "$hello/notes.yaml" >/dev/null 2>"$E2E_DIR/notes.err" \
  && grep -v AlreadyExists "$E2E_DIR/notes.err" | grep -q .; then
  cat "$E2E_DIR/notes.err" >&2
  exit 1
fi

echo "== front door"
apply() { kubectl -n fixture "$@" --dry-run=client -o yaml | kubectl apply -f - >/dev/null; }
apply create configmap front-door --from-file=nginx.conf="$here/front-door-nginx.conf"
apply create configmap hello-web --from-file="$hello/web"
apply create configmap fixture-ca --from-file=ca.crt="$E2E_DIR/ca.crt"
# The same certificate as krm-foyer: the front door answers for the same public name.
apply create secret tls front-door-tls --cert "$E2E_DIR/foyer/tls.crt" --key "$E2E_DIR/foyer/tls.key"
config_hash="$(cat "$here/front-door-nginx.conf" "$hello"/web/* "$E2E_DIR/ca.crt" "$E2E_DIR/foyer/tls.crt" \
  | sha256sum | cut -c1-16)"
sed -e "s|NGINX_IMAGE|$NGINX_IMAGE|" -e "s|CONFIG_HASH|$config_hash|" "$here/front-door.yaml" \
  | kubectl apply -f - >/dev/null
if ! kubectl -n fixture rollout status deployment/front-door --timeout=120s; then
  kubectl -n fixture describe pods -l app=front-door >&2
  kubectl -n fixture logs deployment/front-door --tail=50 >&2 || true
  exit 1
fi

echo "== port-forwards"
"$here/port-forward.sh"
# Through the forward, as a browser: the example's page, and /auth/ reaching krm-foyer
# (which answers 401 to a browser with no session).
front() { curl -sS --cacert "$E2E_DIR/ca.crt" --resolve "foyer.localhost:8443:127.0.0.1" "$@"; }
front -f https://foyer.localhost:8443/ | grep -q '<title>Hello, krm-foyer</title>'
code="$(front -o /dev/null -w '%{http_code}' https://foyer.localhost:8443/auth/session)"
[ "$code" = 401 ] || { echo "the front door does not reach krm-foyer (/auth/session: $code)" >&2; exit 1; }
echo "the hello example is at $FOYER_URL"
