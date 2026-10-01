#!/usr/bin/env bash
# Brings up the e2e fixture: a k3d cluster whose API server trusts a Dex issuer, and
# records who made every request in an audit log. Safe to rerun: an existing cluster is
# reused, and Dex and the mounted configuration are refreshed every time.
#
# Everything sits on one Docker network, and nothing is published on the host. That is
# what makes the same script work in the devcontainer (Docker runs beside us, so a
# published port is on the host, not here) and on a CI runner (where it is the host).
# The devcontainer joins the network; a CI runner routes to it directly.
#
# Outputs, all under E2E_DIR (default .e2e, gitignored):
#   kubeconfig  admin access for the suite's own setup, never the user's default kubeconfig
#   ca.crt      the CA that signed Dex's certificate
#   env         addresses the suite reads
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-krm-foyer-e2e}"
NETWORK="${NETWORK:-krm-foyer-e2e}"
SUBNET="${SUBNET:-172.29.250.0/24}"
DEX_IP="${DEX_IP:-172.29.250.10}"
DEX_HOST="dex.krm-foyer.test"
VOLUME="${CLUSTER_NAME}-config"
DEX_CONTAINER="${CLUSTER_NAME}-dex"
SERVER_CONTAINER="k3d-${CLUSTER_NAME}-server-0"
# The same k3s release gitops-reverser's e2e runs on.
K3S_IMAGE="${K3S_IMAGE:-rancher/k3s:v1.36.4-k3s1@sha256:edad48e12bf81c3a09ac1c05c0c0ffaaa22145980b989d6fae84543a76b83657}"
DEX_IMAGE="ghcr.io/dexidp/dex:v2.45.1@sha256:8499afd690c437f52301efd2b05b2455da5bd2dfc20332cd697dc9937f808462"
BUSYBOX_IMAGE="busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

here="$(cd "$(dirname "$0")" && pwd)"
E2E_DIR="${E2E_DIR:-$(cd "$here/../../.." && pwd)/.e2e}"
mkdir -p "$E2E_DIR/config"
chmod 700 "$E2E_DIR"

echo "== certificates"
if [ ! -f "$E2E_DIR/ca.crt" ]; then
  openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=krm-foyer e2e CA" \
    -keyout "$E2E_DIR/ca.key" -out "$E2E_DIR/ca.crt" 2>/dev/null
fi
if [ ! -f "$E2E_DIR/config/dex.crt" ]; then
  openssl req -newkey rsa:2048 -nodes -subj "/CN=$DEX_HOST" \
    -keyout "$E2E_DIR/config/dex.key" -out "$E2E_DIR/dex.csr" 2>/dev/null
  openssl x509 -req -in "$E2E_DIR/dex.csr" -CA "$E2E_DIR/ca.crt" -CAkey "$E2E_DIR/ca.key" \
    -CAcreateserial -days 30 -extfile <(printf 'subjectAltName=DNS:%s' "$DEX_HOST") \
    -out "$E2E_DIR/config/dex.crt" 2>/dev/null
fi

echo "== configuration"
cp "$here/dex.yaml" "$here/audit-policy.yaml" "$E2E_DIR/config/"
# Indent the CA into the block scalar the authentication config leaves for it.
awk -v ca="$E2E_DIR/ca.crt" '
  $0 == "CA_PEM" { while ((getline line < ca) > 0) print "        " line; next }
  { print }
' "$here/authentication-config.yaml" > "$E2E_DIR/config/authentication-config.yaml"

echo "== network $NETWORK ($SUBNET)"
docker network inspect "$NETWORK" >/dev/null 2>&1 \
  || docker network create --subnet "$SUBNET" "$NETWORK" >/dev/null

echo "== dex at https://$DEX_HOST:5556 ($DEX_IP)"
# Dex keeps its signing keys in memory, so a restarted Dex signs with new keys, and a
# running API server goes on rejecting every token as a bad signature. Restart Dex only
# when its configuration changed, and then restart the API server too (below).
#
# The configuration is only rewritten then, too. Dex watches its certificate files, and a
# reload that catches a half-written or unreadable key leaves it with no certificate at
# all: every TLS handshake fails until the next reload. That made the suite fail at random.
# The files get their final mode here, before they are copied, so no reader ever sees a
# key it cannot open. Dex runs as a non-root user; .e2e itself stays 0700.
chmod 0644 "$E2E_DIR"/config/*
config_hash="$(cat "$E2E_DIR"/config/* | sha256sum | cut -c1-16)"
running_hash="$(docker inspect "$DEX_CONTAINER" --format '{{if .State.Running}}{{index .Config.Labels "config-hash"}}{{end}}' 2>/dev/null || true)"
dex_restarted=false
if [ "$running_hash" != "$config_hash" ]; then
  docker rm -f "$DEX_CONTAINER" >/dev/null 2>&1 || true
  # A named volume, not a bind mount: with Docker beside us, a path in this container
  # does not exist for the daemon.
  docker volume create "$VOLUME" >/dev/null
  tar -C "$E2E_DIR/config" -cf - . | docker run --rm -i -v "$VOLUME:/v" "$BUSYBOX_IMAGE" tar -xf - -C /v
  # Two ways to find Dex by name, for two kinds of caller: the network alias serves the
  # API server (it resolves through Docker's DNS), and --host-alias below puts the name
  # in CoreDNS for pods.
  docker run -d --name "$DEX_CONTAINER" --label "config-hash=$config_hash" \
    --network "$NETWORK" --ip "$DEX_IP" --network-alias "$DEX_HOST" \
    -v "$VOLUME:/etc/krm-foyer-e2e:ro" "$DEX_IMAGE" dex serve /etc/krm-foyer-e2e/dex.yaml >/dev/null
  dex_restarted=true
fi

echo "== cluster $CLUSTER_NAME"
if ! k3d cluster get "$CLUSTER_NAME" >/dev/null 2>&1; then
  # --timeout bounds --wait: a server that never becomes ready fails here, not at the
  # CI job's own timeout. A malformed authentication config looks exactly like that:
  # the API server exits and k3d waits for an API that never answers (Voter lost a
  # 30-minute CI run to it). If this times out, read `docker logs $SERVER_CONTAINER`.
  # Components the suite does not need are disabled; an
  # unready metrics-server also breaks full API discovery for its first minute.
  k3d cluster create "$CLUSTER_NAME" \
    --image "$K3S_IMAGE" --servers 1 --agents 0 --wait --timeout 180s \
    --network "$NETWORK" \
    --host-alias "$DEX_IP:$DEX_HOST" \
    --kubeconfig-update-default=false --kubeconfig-switch-context=false \
    --volume "$VOLUME:/etc/krm-foyer-e2e@server:0" \
    --k3s-arg "--disable=traefik,servicelb,metrics-server@server:0" \
    --k3s-arg "--kube-apiserver-arg=authentication-config=/etc/krm-foyer-e2e/authentication-config.yaml@server:0" \
    --k3s-arg "--kube-apiserver-arg=audit-policy-file=/etc/krm-foyer-e2e/audit-policy.yaml@server:0" \
    --k3s-arg "--kube-apiserver-arg=audit-log-path=/etc/krm-foyer-e2e/audit.log@server:0" \
    --k3s-arg "--kube-apiserver-arg=audit-log-maxsize=50@server:0"
elif [ "$dex_restarted" = true ]; then
  echo "Dex or its configuration changed; restarting the API server so it fetches new keys"
  docker restart "$SERVER_CONTAINER" >/dev/null
fi

echo "== kubeconfig"
# Inside the devcontainer, join the cluster's network so its addresses are reachable.
if [ -f /.dockerenv ]; then
  docker network connect "$NETWORK" "$(hostname)" 2>/dev/null || true
fi
server_ip="$(docker inspect "$SERVER_CONTAINER" \
  --format "{{(index .NetworkSettings.Networks \"$NETWORK\").IPAddress}}")"
k3d kubeconfig get "$CLUSTER_NAME" \
  | sed "s|server: .*|server: https://$server_ip:6443|" > "$E2E_DIR/kubeconfig"
chmod 600 "$E2E_DIR/kubeconfig"
export KUBECONFIG="$E2E_DIR/kubeconfig"
# After a restart the API server takes a few seconds to listen again.
for _ in $(seq 1 45); do
  kubectl get --raw /readyz >/dev/null 2>&1 && break
  sleep 2
done
kubectl wait --for=condition=Ready "node/$SERVER_CONTAINER" --timeout=90s >/dev/null

cat > "$E2E_DIR/env" <<EOF
CLUSTER_NAME=$CLUSTER_NAME
SERVER_CONTAINER=$SERVER_CONTAINER
API_SERVER=https://$server_ip:6443
DEX_ISSUER=https://$DEX_HOST:5556
DEX_IP=$DEX_IP
EOF
echo "fixture ready: KUBECONFIG=$E2E_DIR/kubeconfig"
