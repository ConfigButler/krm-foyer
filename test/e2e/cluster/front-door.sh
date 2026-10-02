#!/usr/bin/env bash
# Puts the hello example in front of krm-foyer, so a person can use it from a browser:
# the example's resources and grants, a file server for its pages (hello-web.yaml), and
# the front door, a Gateway with two HTTPRoutes (gateway.yaml) that Traefik implements
# (install-traefik.sh). Then port-forward.sh forwards Traefik and Dex to this container's
# loopback, where a browser finds https://foyer.localhost:8443. Run after deploy-foyer.sh;
# safe to rerun.
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

echo "== the example's pages"
# The nginx front door of earlier versions of this script; the Gateway replaced it.
kubectl -n fixture delete deployment,service front-door --ignore-not-found >/dev/null
kubectl -n fixture delete configmap front-door fixture-ca --ignore-not-found >/dev/null
apply() { kubectl -n fixture "$@" --dry-run=client -o yaml | kubectl apply -f - >/dev/null; }
apply create configmap hello-web-config --from-file=nginx.conf="$here/hello-web-nginx.conf"
apply create configmap hello-web --from-file="$hello/web"
# Its certificate is from start-cluster.sh; the Gateway checks it against the fixture CA.
apply create secret tls hello-web-tls --cert "$E2E_DIR/tls/hello-web.crt" --key "$E2E_DIR/tls/hello-web.key"
apply create configmap hello-web-ca --from-file=ca.crt="$E2E_DIR/ca.crt"
config_hash="$(cat "$here/hello-web-nginx.conf" "$hello"/web/* "$E2E_DIR/tls/hello-web.crt" | sha256sum | cut -c1-16)"
sed -e "s|NGINX_IMAGE|$NGINX_IMAGE|" -e "s|CONFIG_HASH|$config_hash|" "$here/hello-web.yaml" \
  | kubectl apply -f - >/dev/null
if ! kubectl -n fixture rollout status deployment/hello-web --timeout=120s; then
  kubectl -n fixture describe pods -l app=hello-web >&2
  kubectl -n fixture logs deployment/hello-web --tail=50 >&2 || true
  exit 1
fi

echo "== front door: Gateway and routes"
# The same certificate as krm-foyer: the Gateway answers for the same public name.
apply create secret tls front-door-tls --cert "$E2E_DIR/foyer/tls.crt" --key "$E2E_DIR/foyer/tls.key"
kubectl apply -f "$here/gateway.yaml" >/dev/null
# Accepted: the Gateway admitted the route. ResolvedRefs: its backends, and their
# BackendTLSPolicies' CAs, were found.
kubectl -n fixture wait --for=condition=Programmed gateway/front-door --timeout=60s >/dev/null
for route in fixture/hello krm-foyer/krm-foyer; do
  for condition in Accepted ResolvedRefs; do
    kubectl -n "${route%/*}" wait --for=jsonpath="{.status.parents[0].conditions[?(@.type==\"$condition\")].status}"=True \
      "httproute/${route#*/}" --timeout=60s >/dev/null
  done
done

echo "== port-forwards"
"$here/port-forward.sh"
# Through the forward, as a browser: the example's page, and /auth/ reaching krm-foyer
# (which answers 401 to a browser with no session).
front() { curl -sS --cacert "$E2E_DIR/ca.crt" --resolve "foyer.localhost:8443:127.0.0.1" "$@"; }
front -f https://foyer.localhost:8443/ | grep -q '<title>Hello, krm-foyer</title>'
code="$(front -o /dev/null -w '%{http_code}' https://foyer.localhost:8443/auth/session)"
[ "$code" = 401 ] || { echo "the front door does not reach krm-foyer (/auth/session: $code)" >&2; exit 1; }
echo "the hello example is at $FOYER_URL"
