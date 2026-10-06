#!/usr/bin/env bash
# Room Pass's QR login in the e2e fixture (docs/room-pass.md): Room Pass 2.0.0 and the
# Dex behind it (room-pass/room-pass.yaml), its CRDs and a Room, a test application with
# the QR entry point (room-pass/room-app.yaml), the two hosts' routes in Traefik
# (room-pass/routes.yaml), and a third krm-foyer on the application's host
# (foyer-room-values.yaml). Run after deploy-foyer.sh and front-door.sh; safe to rerun.
#
# Everything Room Pass's is here and in room-pass/, in the fixture: krm-foyer knows
# nothing of it beyond its generic login settings.
#
# Writes E2E_DIR/room-env: where the suite finds it.
set -euo pipefail

# Room Pass 2.0.0 (2185782f6f349731909eafdb290007bc3feb168c), the release
# room-pass/crds.yaml is from.
ROOM_PASS_IMAGE="ghcr.io/sunib/room-pass:2.0.0@sha256:eb5fee5161e52eb6de3983c811ddc883838cbee1dfea33fb013374696e189416"
# The same Dex and nginx as start-cluster.sh.
DEX_IMAGE="ghcr.io/dexidp/dex:v2.45.1@sha256:8499afd690c437f52301efd2b05b2455da5bd2dfc20332cd697dc9937f808462"
NGINX_IMAGE="nginx:1.31.6-alpine@sha256:df221db836e1754089190208cee7eeda94f233197056426eda74a43ab1abeac2"
# The application's host, which krm-foyer, Room Pass's /join and the test application
# share, and the issuer's: with the front door's port, which is part of each origin.
ROOM_HOST="room.localhost:8443"
ROOM_PASS_HOST="room-pass.localhost:8443"
# The issuer's address for the API server and pods; start-cluster.sh maps the name here.
ROOM_PASS_SERVICE_IP="10.43.0.212"

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../../.." && pwd)"
E2E_DIR="${E2E_DIR:-$repo/.e2e}"
[ -f "$E2E_DIR/foyer-env" ] || { echo "krm-foyer is not deployed; run task e2e-deploy first" >&2; exit 1; }
# shellcheck source=/dev/null
. "$E2E_DIR/foyer-env"
export KUBECONFIG="$E2E_DIR/kubeconfig"

echo "== certificates"
mkdir -p "$E2E_DIR/room"
# cert NAME HOST...: a certificate for every HOST at room/NAME.crt, from the fixture CA,
# renewed when it expires within a day, or the CA or the names changed.
cert() {
  local name="$1" crt="$E2E_DIR/room/$1.crt" key="$E2E_DIR/room/$1.key" fresh=1 host sans=""
  shift
  [ -f "$crt" ] && openssl x509 -checkend 86400 -noout -in "$crt" >/dev/null 2>&1 \
    && openssl verify -CAfile "$E2E_DIR/ca.crt" "$crt" >/dev/null 2>&1 || fresh=""
  for host in "$@"; do
    sans="$sans${sans:+,}DNS:$host"
    [ -n "$fresh" ] && openssl x509 -noout -checkhost "$host" -in "$crt" | grep -q 'does match' || fresh=""
  done
  [ -n "$fresh" ] && return 0
  openssl req -newkey rsa:2048 -nodes -subj "/CN=$1" -keyout "$key" -out "$E2E_DIR/room/$name.csr" 2>/dev/null
  openssl x509 -req -in "$E2E_DIR/room/$name.csr" -CA "$E2E_DIR/ca.crt" -CAkey "$E2E_DIR/ca.key" \
    -CAcreateserial -days 30 -extfile <(printf 'subjectAltName=%s' "$sans") -out "$crt" 2>/dev/null
}
cert edge room.localhost room-pass.localhost
cert krm-foyer-room room.localhost krm-foyer-room.krm-foyer.svc
cert room-app room-app.fixture.svc
tls() { kubectl -n "$1" create secret tls "$2" --cert "$E2E_DIR/room/$3.crt" --key "$E2E_DIR/room/$3.key" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null; }

echo "== Room Pass's CRDs"
kubectl apply -f "$here/room-pass/crds.yaml" >/dev/null
kubectl wait --for=condition=Established crd/rooms.room-pass.koudijs.dev crd/participants.room-pass.koudijs.dev \
  --timeout=60s >/dev/null

echo "== Room Pass and its Dex"
fill() {
  sed -e "s|ROOM_PASS_IMAGE|$ROOM_PASS_IMAGE|" -e "s|DEX_IMAGE|$DEX_IMAGE|" -e "s|NGINX_IMAGE|$NGINX_IMAGE|" \
    -e "s|ROOM_PASS_HOST|$ROOM_PASS_HOST|g" -e "s|ROOM_HOST|$ROOM_HOST|g" \
    -e "s|ROOM_PASS_SERVICE_IP|$ROOM_PASS_SERVICE_IP|" -e "s|CONFIG_HASH|${config_hash:-}|" "$@"
}
kubectl create namespace room-pass --dry-run=client -o yaml | kubectl apply -f - >/dev/null
# The keys Room Pass seals its cookies with, made once and kept, so its participants stay
# enrolled across reruns.
if ! kubectl -n room-pass get secret room-pass-cookie >/dev/null 2>&1; then
  kubectl -n room-pass create secret generic room-pass-cookie \
    --from-file=hash-key=<(head -c 32 /dev/urandom) --from-file=block-key=<(head -c 32 /dev/urandom) >/dev/null
fi
fill "$here/room-pass/dex-config.yaml" > "$E2E_DIR/room/dex-config.yaml"
kubectl -n room-pass create configmap dex --from-file=config.yaml="$E2E_DIR/room/dex-config.yaml" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
# A month from now, every run: a Room that has ended takes no one.
fill "$here/room-pass/room.yaml" | sed "s|ENDS_AT|$(date -u -d '+30 days' +%Y-%m-%dT%H:%M:%SZ)|" \
  | kubectl apply -f - >/dev/null
config_hash="$(cat "$E2E_DIR/room/dex-config.yaml" "$here/room-pass/room-pass.yaml" | sha256sum | cut -c1-16)"
fill "$here/room-pass/room-pass.yaml" | kubectl apply -f - >/dev/null
for d in dex room-pass; do
  if ! kubectl -n room-pass rollout status "deployment/$d" --timeout=180s; then
    kubectl -n room-pass describe pods -l "app=$d" >&2
    kubectl -n room-pass logs "deployment/$d" --tail=50 >&2 || true
    exit 1
  fi
done

echo "== the test application"
kubectl -n fixture create configmap room-app --from-file=nginx.conf="$here/room-pass/room-app-nginx.conf" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
tls fixture room-app-tls room-app
config_hash="$(cat "$here/room-pass/room-app-nginx.conf" "$E2E_DIR/room/room-app.crt" | sha256sum | cut -c1-16)"
fill "$here/room-pass/room-app.yaml" | kubectl apply -f - >/dev/null
if ! kubectl -n fixture rollout status deployment/room-app --timeout=120s; then
  kubectl -n fixture describe pods -l app=room-app >&2
  kubectl -n fixture logs deployment/room-app --tail=50 >&2 || true
  exit 1
fi

echo "== routes for $ROOM_HOST and $ROOM_PASS_HOST"
for ns in room-pass krm-foyer fixture; do tls "$ns" room-edge-tls edge; done
fill "$here/room-pass/routes.yaml" | kubectl apply -f - >/dev/null

echo "== krm-foyer on $ROOM_HOST"
apply() { kubectl -n krm-foyer "$@" --dry-run=client -o yaml | kubectl apply -f - >/dev/null; }
tls krm-foyer krm-foyer-room-tls krm-foyer-room
# The client secret of krm-foyer in room-pass/dex-config.yaml.
apply create secret generic krm-foyer-room-oidc --from-literal=client-secret=krm-foyer-room-e2e-secret
if ! kubectl -n krm-foyer get secret krm-foyer-room-session-keys >/dev/null 2>&1; then
  kubectl -n krm-foyer create secret generic krm-foyer-room-session-keys \
    --from-literal=session-keys="$(head -c 32 /dev/urandom | base64)" >/dev/null
fi
config_hash="$(cat "$E2E_DIR/room/krm-foyer-room.crt" "$E2E_DIR/ca.crt" | sha256sum | cut -c1-16)"
helm upgrade --install krm-foyer-room "$repo/charts/krm-foyer" --namespace krm-foyer \
  -f "$here/foyer-room-values.yaml" \
  --set-string image.repository="${FOYER_IMAGE%:*}" --set-string image.tag="${FOYER_IMAGE#*:}" \
  --set-string "podAnnotations.krm-foyer\.test/config-hash=$config_hash" >/dev/null
# Ready once it has read the issuer's discovery document, through Traefik and Room Pass.
if ! kubectl -n krm-foyer rollout status deployment/krm-foyer-room --timeout=180s; then
  kubectl -n krm-foyer describe pods -l app.kubernetes.io/instance=krm-foyer-room >&2
  kubectl -n krm-foyer logs deployment/krm-foyer-room --tail=50 >&2 || true
  exit 1
fi

echo "== checks, through the front door"
# As a browser reaches both hosts: the issuer's discovery, krm-foyer's 401 on the
# application's host, the join endpoint refusing a code Room Pass would ignore, and Dex
# unreachable at a path Room Pass does not forward. Traefik takes a moment to load new
# routes, so each is given 30 seconds.
front() { curl -sS --cacert "$E2E_DIR/ca.crt" --resolve "room.localhost:8443:127.0.0.1" \
  --resolve "room-pass.localhost:8443:127.0.0.1" -o /dev/null -w '%{http_code}' "$@"; }
expect_code() {
  local url="$1" want="$2" code=""
  for _ in $(seq 1 30); do
    code="$(front "$url" || true)"
    [ "$code" = "$want" ] && return 0
    sleep 1
  done
  echo "$url answered $code, want $want" >&2
  exit 1
}
expect_code "https://$ROOM_PASS_HOST/.well-known/openid-configuration" 200
expect_code "https://$ROOM_PASS_HOST/not-a-dex-path" 404
expect_code "https://$ROOM_HOST/auth/session" 401
expect_code "https://$ROOM_HOST/join-room?code=not-a-code" 400
expect_code "https://$ROOM_HOST/room/" 200

cat > "$E2E_DIR/room-env" <<ENV
ROOM_URL=https://$ROOM_HOST
ROOM_PASS_ISSUER=https://$ROOM_PASS_HOST
ROOM_PASS_NAMESPACE=room-pass
ROOM_NAME=e2e
ROOM_AUDIENCE_GROUP=demo:krm-foyer-e2e
ROOM_FOYER_RELEASE=krm-foyer-room
ENV
echo "Room Pass's QR login is at https://$ROOM_HOST/join-room?code=<the Room's code>"
