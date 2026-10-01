#!/usr/bin/env bash
# Puts the hello example in front of krm-foyer, so a person can use it from a browser on
# this machine: the example's resources and grants in the cluster, and the front door, an
# nginx container on the fixture's network that serves examples/hello/web at / and sends
# krm-foyer's prefixes to krm-foyer. Run after deploy-foyer.sh; safe to rerun.
#
# The front door publishes two ports, on loopback only: 8443 (the application and
# krm-foyer, https://foyer.localhost:8443) and 5556 (Dex, https://dex.localhost:5556).
# Browsers resolve *.localhost to loopback themselves, so no hosts-file entry is needed.
#
# Writes E2E_DIR/front-door-env: where the browser test finds the front door.
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-krm-foyer-e2e}"
NETWORK="${NETWORK:-krm-foyer-e2e}"
FRONT_DOOR_IP="${FRONT_DOOR_IP:-172.29.250.20}"
CONTAINER="${CLUSTER_NAME}-front-door"
VOLUME="${CLUSTER_NAME}-front-door"
# The same nginx as the test issuer in start-cluster.sh.
NGINX_IMAGE="nginx:1.31.6-alpine@sha256:df221db836e1754089190208cee7eeda94f233197056426eda74a43ab1abeac2"
BUSYBOX_IMAGE="busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../../.." && pwd)"
hello="$repo/examples/hello"
E2E_DIR="${E2E_DIR:-$repo/.e2e}"
[ -f "$E2E_DIR/foyer-env" ] || { echo "krm-foyer is not deployed; run task e2e-deploy first" >&2; exit 1; }
# shellcheck source=/dev/null
. "$E2E_DIR/env"
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

echo "== front door at $FRONT_DOOR_IP"
stage="$E2E_DIR/front-door"
rm -rf "$stage"
mkdir -p "$stage"
sed -e "s|FOYER_UPSTREAM|$FOYER_ADDR|" -e "s|DEX_UPSTREAM|$DEX_IP:5556|" \
  "$here/front-door-nginx.conf" > "$stage/nginx.conf"
# The same certificate as krm-foyer: the front door answers for the same public name.
cp "$E2E_DIR/foyer/tls.crt" "$E2E_DIR/foyer/tls.key" "$E2E_DIR/ca.crt" "$stage/"
cp -R "$hello/web" "$stage/web"
chmod -R a+rX "$stage"
config_hash="$(tar -C "$stage" -cf - --sort=name --mtime=@0 --owner=0 --group=0 . | sha256sum | cut -c1-16)"
running_hash="$(docker inspect "$CONTAINER" --format '{{if .State.Running}}{{index .Config.Labels "config-hash"}}{{end}}' 2>/dev/null || true)"
if [ "$running_hash" != "$config_hash" ]; then
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  # Recreated, so files deleted from the example do not linger.
  docker volume rm "$VOLUME" >/dev/null 2>&1 || true
  docker volume create "$VOLUME" >/dev/null
  tar -C "$stage" -cf - . | docker run --rm -i -v "$VOLUME:/v" "$BUSYBOX_IMAGE" tar -xf - -C /v
  docker run -d --name "$CONTAINER" --label "config-hash=$config_hash" \
    --network "$NETWORK" --ip "$FRONT_DOOR_IP" \
    -p 127.0.0.1:8443:8443 -p 127.0.0.1:5556:5556 \
    -v "$VOLUME:/etc/front-door:ro" "$NGINX_IMAGE" \
    nginx -c /etc/front-door/nginx.conf -g 'daemon off;' >/dev/null
fi

# The front door, reached on the network by its public name, must pass /auth/ through to
# krm-foyer (which answers 401 to a browser with no session) and serve the example.
front() { curl -sS --cacert "$E2E_DIR/ca.crt" --resolve "foyer.localhost:8443:$FRONT_DOOR_IP" "$@"; }
code=""
for _ in $(seq 1 30); do
  code="$(front -o /dev/null -w '%{http_code}' https://foyer.localhost:8443/auth/session 2>/dev/null || true)"
  [ "$code" = 401 ] && break
  sleep 1
done
if [ "$code" != 401 ]; then
  docker logs "$CONTAINER" --tail 30 >&2
  echo "the front door does not reach krm-foyer (last answer: ${code:-none})" >&2
  exit 1
fi
front -f https://foyer.localhost:8443/ | grep -q '<title>Hello, krm-foyer</title>'

cat > "$E2E_DIR/front-door-env" <<EOF
FRONT_DOOR_IP=$FRONT_DOOR_IP
FRONT_DOOR_CONTAINER=$CONTAINER
EOF
echo "the hello example is at $FOYER_URL"
