#!/usr/bin/env bash
# Brings up the e2e fixture: a k3d cluster whose API server trusts a Dex issuer, and
# records who made every request in an audit log. Safe to rerun: an existing cluster is
# reused, and Dex and the mounted configuration are refreshed every time.
#
# Everything sits on one Docker network. That is what makes the same script work in the
# devcontainer (Docker runs beside us, so a published port is on the host, not here) and
# on a CI runner (where it is the host). The devcontainer joins the network; a CI runner
# routes to it directly. k3d always publishes the API server's port; it is bound to
# loopback, and the script fails if anything in the fixture is published more widely.
#
# Two issuers: Dex, for real logins, and a static test issuer whose signing key the suite
# holds, for tokens with claims Dex never issues.
#
# Outputs, all under E2E_DIR (default .e2e, gitignored):
#   kubeconfig  admin access for the suite's own setup, never the user's default kubeconfig
#   ca.crt      the CA that signed both issuers' certificates
#   issuer-signing.key  the test issuer's signing key, for the suite to mint tokens with
#   env         addresses the suite reads
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-krm-foyer-e2e}"
NETWORK="${NETWORK:-krm-foyer-e2e}"
SUBNET="${SUBNET:-172.29.250.0/24}"
GATEWAY="${GATEWAY:-172.29.250.1}"
DEX_IP="${DEX_IP:-172.29.250.10}"
DEX_HOST="dex.krm-foyer.test"
ISSUER_IP="${ISSUER_IP:-172.29.250.11}"
ISSUER_HOST="issuer.krm-foyer.test"
ISSUER_URL="https://$ISSUER_HOST:8443"
VOLUME="${CLUSTER_NAME}-config"
DEX_CONTAINER="${CLUSTER_NAME}-dex"
ISSUER_CONTAINER="${CLUSTER_NAME}-issuer"
SERVER_CONTAINER="k3d-${CLUSTER_NAME}-server-0"
# The same k3s release gitops-reverser's e2e runs on.
K3S_IMAGE="${K3S_IMAGE:-rancher/k3s:v1.36.4-k3s1@sha256:edad48e12bf81c3a09ac1c05c0c0ffaaa22145980b989d6fae84543a76b83657}"
DEX_IMAGE="ghcr.io/dexidp/dex:v2.45.1@sha256:8499afd690c437f52301efd2b05b2455da5bd2dfc20332cd697dc9937f808462"
NGINX_IMAGE="nginx:1.31.6-alpine@sha256:df221db836e1754089190208cee7eeda94f233197056426eda74a43ab1abeac2"
BUSYBOX_IMAGE="busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

here="$(cd "$(dirname "$0")" && pwd)"
E2E_DIR="${E2E_DIR:-$(cd "$here/../../.." && pwd)/.e2e}"
mkdir -p "$E2E_DIR/config"
chmod 700 "$E2E_DIR"

echo "== certificates"
# Thirty-day certificates in a directory that outlives the cluster, so a rerun renews any
# that expire within a day. A new CA means new server certificates signed by it; all of
# them change the configuration hash below, which restarts the issuers and the API server.
expiring() { [ ! -f "$1" ] || ! openssl x509 -checkend 86400 -noout -in "$1" >/dev/null 2>&1; }
if expiring "$E2E_DIR/ca.crt"; then
  rm -f "$E2E_DIR/config/dex.crt" "$E2E_DIR/config/issuer-tls.crt"
  openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=krm-foyer e2e CA" \
    -keyout "$E2E_DIR/ca.key" -out "$E2E_DIR/ca.crt" 2>/dev/null
fi
# server_cert NAME HOST: a certificate for HOST at config/NAME.crt, signed by the CA.
server_cert() {
  expiring "$E2E_DIR/config/$1.crt" || return 0
  openssl req -newkey rsa:2048 -nodes -subj "/CN=$2" \
    -keyout "$E2E_DIR/config/$1.key" -out "$E2E_DIR/$1.csr" 2>/dev/null
  openssl x509 -req -in "$E2E_DIR/$1.csr" -CA "$E2E_DIR/ca.crt" -CAkey "$E2E_DIR/ca.key" \
    -CAcreateserial -days 30 -extfile <(printf 'subjectAltName=DNS:%s' "$2") \
    -out "$E2E_DIR/config/$1.crt" 2>/dev/null
}
server_cert dex "$DEX_HOST"
server_cert issuer-tls "$ISSUER_HOST"
# The test issuer's signing key stays outside config/, so it is never mounted anywhere.
# It is kept across runs: a new key is a new JWKS, which restarts the API server.
[ -f "$E2E_DIR/issuer-signing.key" ] \
  || openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$E2E_DIR/issuer-signing.key" 2>/dev/null

echo "== configuration"
cp "$here/dex.yaml" "$here/audit-policy.yaml" "$E2E_DIR/config/"
# Indent the CA into the block scalar the authentication config leaves for it.
awk -v ca="$E2E_DIR/ca.crt" '
  $0 == "CA_PEM" { while ((getline line < ca) > 0) print "        " line; close(ca); next }
  { print }
' "$here/authentication-config.yaml" > "$E2E_DIR/config/authentication-config.yaml"

# The test issuer serves its discovery document and the public half of the signing key.
modulus="$(openssl rsa -in "$E2E_DIR/issuer-signing.key" -noout -modulus 2>/dev/null | cut -d= -f2 \
  | basenc --base16 -d | basenc --base64url -w0 | tr -d '=')"
printf '{"issuer":"%s","jwks_uri":"%s/jwks.json","id_token_signing_alg_values_supported":["RS256"]}\n' \
  "$ISSUER_URL" "$ISSUER_URL" > "$E2E_DIR/config/issuer-discovery.json"
printf '{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":"e2e","n":"%s","e":"AQAB"}]}\n' \
  "$modulus" > "$E2E_DIR/config/issuer-jwks.json"
cp "$here/issuer-nginx.conf" "$E2E_DIR/config/"

echo "== network $NETWORK ($SUBNET)"
docker network inspect "$NETWORK" >/dev/null 2>&1 \
  || docker network create --subnet "$SUBNET" --gateway "$GATEWAY" "$NETWORK" >/dev/null
# k3d reads the network's gateway to find the host. Newer Docker engines (as on GitHub's
# runners) only report a gateway that was set explicitly, hence --gateway above.

echo "== dex at https://$DEX_HOST:5556 ($DEX_IP), test issuer at $ISSUER_URL ($ISSUER_IP)"
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
# The two issuers start and stop together; one without the other counts as changed.
[ "$(docker inspect "$ISSUER_CONTAINER" --format '{{.State.Running}}' 2>/dev/null)" = true ] || running_hash=""
dex_restarted=false
if [ "$running_hash" != "$config_hash" ]; then
  docker rm -f "$DEX_CONTAINER" "$ISSUER_CONTAINER" >/dev/null 2>&1 || true
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
  # Only the API server talks to the test issuer, so the network alias is enough.
  docker run -d --name "$ISSUER_CONTAINER" \
    --network "$NETWORK" --ip "$ISSUER_IP" --network-alias "$ISSUER_HOST" \
    -v "$VOLUME:/etc/krm-foyer-e2e:ro" "$NGINX_IMAGE" \
    nginx -c /etc/krm-foyer-e2e/issuer-nginx.conf -g 'daemon off;' >/dev/null
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
  #
  # k3d publishes the API server's port on every interface by default, through a load
  # balancer container. One server needs no load balancer, and the port goes to loopback
  # only (a random free one): the suite reaches the API server over the network.
  k3d cluster create "$CLUSTER_NAME" \
    --image "$K3S_IMAGE" --servers 1 --agents 0 --wait --timeout 180s \
    --network "$NETWORK" --no-lb --api-port 127.0.0.1:0 \
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

echo "== nothing published beyond loopback"
# The fixture's API server accepts tokens from an issuer whose key sits in .e2e, and has
# an admin kubeconfig next to it. Neither should be reachable from another machine.
public="$(for c in $(docker ps -q --filter "label=k3d.cluster=$CLUSTER_NAME") "$DEX_CONTAINER" "$ISSUER_CONTAINER"; do
  docker inspect "$c" --format '{{.Name}}{{range $p, $b := .NetworkSettings.Ports}}{{range $b}} {{.HostIp}}:{{.HostPort}}->{{$p}}{{end}}{{end}}'
done | awk '{ for (i = 2; i <= NF; i++) if ($i !~ /^(127\.0\.0\.1|\[?::1\]?):/) print $1, $i }')"
if [ -n "$public" ]; then
  echo "published beyond loopback (a cluster from an older script? run task e2e-down):" >&2
  echo "$public" >&2
  exit 1
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
TEST_ISSUER=$ISSUER_URL
EOF
echo "fixture ready: KUBECONFIG=$E2E_DIR/kubeconfig"
