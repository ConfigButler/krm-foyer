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
# The rehearsal's users (start-cluster.sh) may read the notes, as bob may, so a person
# can sign in to the example as any of them.
rehearsal_users="$(sed -n 's/^REHEARSAL_USERS=//p' "$E2E_DIR/env")"
users=()
for i in $(seq -f '%03g' 1 "${rehearsal_users:-0}"); do users+=("--user=oidc:rehearsal-$i@example.com"); done
if [ "${#users[@]}" -gt 0 ]; then
  kubectl -n hello create rolebinding rehearsal-reads-notes --role=note-reader "${users[@]}" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
fi
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

echo "== front door: Traefik's routes behind /auth/check"
# ForwardAuth verifies krm-foyer by the fixture CA. Both keys, as Traefik reads either.
kubectl -n fixture create secret generic foyer-check-ca \
  --from-file=ca.crt="$E2E_DIR/ca.crt" --from-file=tls.ca="$E2E_DIR/ca.crt" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl apply -f "$here/traefik-routes.yaml" >/dev/null

echo "== the nginx recipe, in front of krm-foyer on a NodePort"
# docs/ingress.md's nginx recipe as the document has it: its nginx block, and its njs
# script, copied out of the document, so a spec of the recipe is a spec of the text.
stage="$E2E_DIR/nginx-door"
mkdir -p "$stage"
recipe() {
  awk -v fence='```'"$1" '
    /^\*\*nginx, `auth_request`\.\*\*/ { section = 1 }
    section && $0 == fence { inside = 1; next }
    inside && $0 == "```" { exit }
    inside { print }
  ' "$repo/docs/ingress.md"
}
recipe nginx > "$stage/recipe.conf"
recipe js > "$stage/foyer.js"
[ -s "$stage/recipe.conf" ] || { echo "no nginx recipe found in docs/ingress.md" >&2; exit 1; }
apply create configmap nginx-door \
  --from-file=nginx.conf="$here/nginx-door.conf" \
  --from-file=foyer-upstream.conf="$here/nginx-door-upstream.conf" \
  --from-file=recipe.conf="$stage/recipe.conf" --from-file=foyer.js="$stage/foyer.js" \
  --from-file=ca.crt="$E2E_DIR/ca.crt"
config_hash="$(cat "$here"/nginx-door* "$stage"/* "$E2E_DIR/ca.crt" "$E2E_DIR/foyer/tls.crt" | sha256sum | cut -c1-16)"
sed -e "s|NGINX_IMAGE|$NGINX_IMAGE|" -e "s|CONFIG_HASH|$config_hash|" "$here/nginx-door.yaml" \
  | kubectl apply -f - >/dev/null
if ! kubectl -n fixture rollout status deployment/nginx-door --timeout=120s; then
  kubectl -n fixture describe pods -l app=nginx-door >&2
  kubectl -n fixture logs deployment/nginx-door --tail=50 >&2 || true
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
# Traefik's routes ask krm-foyer first: a signed-out fetch is its 401, a page load its
# redirect to the login. Anything else is a route or a ForwardAuth that does not work.
# Traefik takes a moment to load new routes, and until then the Gateway's catch-all
# answers, so this waits up to 30 seconds for each.
expect_code() {
  local path="$1" want="$2" what="$3" code=""
  for _ in $(seq 1 30); do
    code="$(front -o /dev/null -w '%{http_code}' "https://foyer.localhost:8443$path")"
    [ "$code" = "$want" ] && return 0
    sleep 1
  done
  echo "$path is not $what (signed out: $code, want $want)" >&2
  exit 1
}
expect_code /public/whoami 401 "behind /auth/check"
expect_code /members/ 302 "behind the login gate"
echo "the hello example is at $FOYER_URL"
