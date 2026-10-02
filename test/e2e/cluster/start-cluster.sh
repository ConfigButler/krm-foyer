#!/usr/bin/env bash
# Brings up the e2e fixture: a k3d cluster whose API server trusts a Dex issuer, and
# records who made every request in an audit log. Safe to rerun: an existing cluster is
# reused, and the issuers and the mounted configuration are refreshed every time.
#
# Everything runs in the cluster, as in gitops-reverser's e2e: Dex, the test issuer, and
# later krm-foyer and the front door. Nothing is published but the API server's port, on
# loopback. People and the suite reach the cluster in two ways:
#   - over the cluster's Docker network, which the devcontainer (or the CI container)
#     joins: the API server and NodePorts, for the suite;
#   - through kubectl port-forward on this container's localhost (port-forward.sh): Dex
#     and the front door, for a browser, which VS Code forwards to the machine it runs on.
#
# Two issuers: Dex, for real logins, and a static test issuer whose signing key the suite
# holds, for tokens with claims Dex never issues.
#
# Outputs, all under E2E_DIR (default .e2e, gitignored):
#   kubeconfig  admin access for the suite's own setup, never the user's default kubeconfig
#   ca.crt      the CA that signed every certificate in the fixture
#   issuer-signing.key  the test issuer's signing key, for the suite to mint tokens with
#   env         addresses the suite reads
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-krm-foyer-e2e}"
NETWORK="${NETWORK:-krm-foyer-e2e}"
SUBNET="${SUBNET:-172.29.250.0/24}"
GATEWAY="${GATEWAY:-172.29.250.1}"
# A name under .localhost, so a browser on this machine needs no hosts-file entry: it
# resolves it to loopback itself, where port-forward.sh forwards Dex's port. Inside the
# cluster an alias sends the same name to Dex's Service, so the browser, krm-foyer and the
# API server all use one issuer URL.
DEX_HOST="dex.localhost"
ISSUER_HOST="issuer.krm-foyer.test"
ISSUER_URL="https://$ISSUER_HOST:8443"
# Fixed ClusterIPs in k3s's default Service range (10.43.0.0/16). The API server runs on
# the node, not in a pod, so it cannot use cluster DNS; --host-alias puts these in the
# node's /etc/hosts (and in CoreDNS, for pods) when the cluster is created.
DEX_SERVICE_IP="10.43.200.10"
ISSUER_SERVICE_IP="10.43.200.11"
VOLUME="${CLUSTER_NAME}-config"
SERVER_CONTAINER="k3d-${CLUSTER_NAME}-server-0"
# The same k3s release gitops-reverser's e2e runs on.
K3S_IMAGE="${K3S_IMAGE:-rancher/k3s:v1.36.4-k3s1@sha256:edad48e12bf81c3a09ac1c05c0c0ffaaa22145980b989d6fae84543a76b83657}"
DEX_IMAGE="ghcr.io/dexidp/dex:v2.45.1@sha256:8499afd690c437f52301efd2b05b2455da5bd2dfc20332cd697dc9937f808462"
NGINX_IMAGE="nginx:1.31.6-alpine@sha256:df221db836e1754089190208cee7eeda94f233197056426eda74a43ab1abeac2"
BUSYBOX_IMAGE="busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

here="$(cd "$(dirname "$0")" && pwd)"
E2E_DIR="${E2E_DIR:-$(cd "$here/../../.." && pwd)/.e2e}"
mkdir -p "$E2E_DIR/apiserver" "$E2E_DIR/tls"
chmod 700 "$E2E_DIR"

echo "== certificates"
# Thirty-day certificates in a directory that outlives the cluster, so a rerun renews any
# that expire within a day. A new CA means new server certificates signed by it, and a new
# authentication config, which restarts the API server.
expiring() { [ ! -f "$1" ] || ! openssl x509 -checkend 86400 -noout -in "$1" >/dev/null 2>&1; }
if expiring "$E2E_DIR/ca.crt"; then
  rm -f "$E2E_DIR"/tls/*.crt
  openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=krm-foyer e2e CA" \
    -keyout "$E2E_DIR/ca.key" -out "$E2E_DIR/ca.crt" 2>/dev/null
fi
# names CERT HOST: whether CERT is for HOST. -checkhost exits 0 either way.
names() { openssl x509 -noout -checkhost "$2" -in "$1" 2>/dev/null | grep -q 'does match'; }
# server_cert NAME HOST: a certificate for HOST at tls/NAME.crt, signed by the CA.
server_cert() {
  if ! expiring "$E2E_DIR/tls/$1.crt" && names "$E2E_DIR/tls/$1.crt" "$2"; then return 0; fi
  openssl req -newkey rsa:2048 -nodes -subj "/CN=$2" \
    -keyout "$E2E_DIR/tls/$1.key" -out "$E2E_DIR/tls/$1.csr" 2>/dev/null
  openssl x509 -req -in "$E2E_DIR/tls/$1.csr" -CA "$E2E_DIR/ca.crt" -CAkey "$E2E_DIR/ca.key" \
    -CAcreateserial -days 30 -extfile <(printf 'subjectAltName=DNS:%s' "$2") \
    -out "$E2E_DIR/tls/$1.crt" 2>/dev/null
}
server_cert dex "$DEX_HOST"
server_cert test-issuer "$ISSUER_HOST"
# The hello example's file server (front-door.sh): the Gateway verifies it by its
# Service name.
server_cert hello-web hello-web.fixture.svc
# Kept across runs: a new key is a new JWKS, which the API server only picks up minutes
# later.
[ -f "$E2E_DIR/issuer-signing.key" ] \
  || openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$E2E_DIR/issuer-signing.key" 2>/dev/null

echo "== API server configuration"
# The only files the API server needs from outside the cluster: whom to trust, and what
# to audit. Indent the CA into the block scalar the authentication config leaves for it.
cp "$here/audit-policy.yaml" "$E2E_DIR/apiserver/"
awk -v ca="$E2E_DIR/ca.crt" '
  $0 == "CA_PEM" { while ((getline line < ca) > 0) print "        " line; close(ca); next }
  { print }
' "$here/authentication-config.yaml" > "$E2E_DIR/apiserver/authentication-config.yaml"
chmod 0644 "$E2E_DIR"/apiserver/*
apiserver_hash="$(cat "$E2E_DIR"/apiserver/* | sha256sum | cut -c1-16)"
# A named volume, not a bind mount: with Docker beside us, a path in this container does
# not exist for the daemon.
put_config() {
  docker volume create "$VOLUME" >/dev/null
  tar -C "$E2E_DIR/apiserver" -cf - . | docker run --rm -i -v "$VOLUME:/v" "$BUSYBOX_IMAGE" tar -xf - -C /v
  echo "$apiserver_hash" > "$E2E_DIR/apiserver.hash"
}

echo "== network $NETWORK ($SUBNET)"
docker network inspect "$NETWORK" >/dev/null 2>&1 \
  || docker network create --subnet "$SUBNET" --gateway "$GATEWAY" "$NETWORK" >/dev/null
# k3d reads the network's gateway to find the host. Newer Docker engines (as on GitHub's
# runners) only report a gateway that was set explicitly, hence --gateway above.

echo "== cluster $CLUSTER_NAME"
if ! k3d cluster get "$CLUSTER_NAME" >/dev/null 2>&1; then
  put_config
  # --timeout bounds --wait: a server that never becomes ready fails here, not at the
  # CI job's own timeout. A malformed authentication config looks exactly like that:
  # the API server exits and k3d waits for an API that never answers (Voter lost a
  # 30-minute CI run to it). If this times out, read `docker logs $SERVER_CONTAINER`.
  # The issuers do not exist yet; the API server starts anyway and fetches their keys
  # once they answer. Components the suite does not need are disabled; an unready
  # metrics-server also breaks full API discovery for its first minute.
  #
  # k3d publishes the API server's port on every interface by default, through a load
  # balancer container. One server needs no load balancer, and the port goes to loopback
  # only (a random free one): the suite reaches the API server over the network.
  k3d cluster create "$CLUSTER_NAME" \
    --image "$K3S_IMAGE" --servers 1 --agents 0 --wait --timeout 180s \
    --network "$NETWORK" --no-lb --api-port 127.0.0.1:0 \
    --host-alias "$DEX_SERVICE_IP:$DEX_HOST" \
    --host-alias "$ISSUER_SERVICE_IP:$ISSUER_HOST" \
    --kubeconfig-update-default=false --kubeconfig-switch-context=false \
    --volume "$VOLUME:/etc/krm-foyer-e2e@server:0" \
    --k3s-arg "--disable=traefik,servicelb,metrics-server@server:0" \
    --k3s-arg "--kube-apiserver-arg=authentication-config=/etc/krm-foyer-e2e/authentication-config.yaml@server:0" \
    --k3s-arg "--kube-apiserver-arg=audit-policy-file=/etc/krm-foyer-e2e/audit-policy.yaml@server:0" \
    --k3s-arg "--kube-apiserver-arg=audit-log-path=/etc/krm-foyer-e2e/audit.log@server:0" \
    --k3s-arg "--kube-apiserver-arg=audit-log-maxsize=50@server:0"
elif ! docker exec "$SERVER_CONTAINER" grep -q "^${DEX_SERVICE_IP}[[:space:]].*${DEX_HOST}" /etc/hosts; then
  # --host-alias is fixed at creation, so a cluster from an older script cannot find Dex.
  echo "cluster $CLUSTER_NAME has no alias $DEX_HOST -> $DEX_SERVICE_IP (made by an older script); run task e2e-down" >&2
  exit 1
elif [ "$(cat "$E2E_DIR/apiserver.hash" 2>/dev/null)" != "$apiserver_hash" ]; then
  echo "the API server's configuration changed; restarting it"
  put_config
  docker restart "$SERVER_CONTAINER" >/dev/null
fi

echo "== nothing published beyond loopback"
# The fixture's API server accepts tokens from an issuer whose key sits in .e2e, and has
# an admin kubeconfig next to it. Neither should be reachable from another machine.
public="$(for c in $(docker ps -q --filter "label=k3d.cluster=$CLUSTER_NAME"); do
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

echo "== issuers: Dex at https://$DEX_HOST:5556, test issuer at $ISSUER_URL"
# The test issuer serves its discovery document and the public half of the signing key.
modulus="$(openssl rsa -in "$E2E_DIR/issuer-signing.key" -noout -modulus 2>/dev/null | cut -d= -f2 \
  | basenc --base16 -d | basenc --base64url -w0 | tr -d '=')"
stage="$E2E_DIR/test-issuer"
mkdir -p "$stage"
printf '{"issuer":"%s","jwks_uri":"%s/jwks.json","id_token_signing_alg_values_supported":["RS256"]}\n' \
  "$ISSUER_URL" "$ISSUER_URL" > "$stage/discovery.json"
printf '{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":"e2e","n":"%s","e":"AQAB"}]}\n' \
  "$modulus" > "$stage/jwks.json"
cp "$here/issuer-nginx.conf" "$stage/nginx.conf"

kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: fixture
EOF
apply() { kubectl -n fixture "$@" --dry-run=client -o yaml | kubectl apply -f - >/dev/null; }
# The rehearsal's users, after alice and bob: as many as the 200-identity rehearsal
# (test/e2e/rehearsal_test.go) signs in, each with alice's password hash, "password".
REHEARSAL_USERS=200
password_hash="$(sed -n 's/^    hash: "\(.*\)"$/\1/p' "$here/dex.yaml" | head -1)"
{
  cat "$here/dex.yaml"
  for i in $(seq -f '%03g' 1 "$REHEARSAL_USERS"); do
    printf '  - email: rehearsal-%s@example.com\n    hash: "%s"\n    username: rehearsal-%s\n    userID: rehearsal-%s\n' \
      "$i" "$password_hash" "$i" "$i"
  done
} > "$E2E_DIR/dex-config.yaml"
apply create configmap dex --from-file=config.yaml="$E2E_DIR/dex-config.yaml"
apply create secret tls dex-tls --cert "$E2E_DIR/tls/dex.crt" --key "$E2E_DIR/tls/dex.key"
apply create configmap test-issuer --from-file="$stage"
apply create secret tls test-issuer-tls --cert "$E2E_DIR/tls/test-issuer.crt" --key "$E2E_DIR/tls/test-issuer.key"
# A changed configuration or certificate rolls the pods. Dex keeps its signing keys in
# the cluster (dex.yaml), so a rolled Dex still signs with the keys the API server has.
config_hash="$(cat "$E2E_DIR/dex-config.yaml" "$E2E_DIR"/tls/*.crt "$stage"/* | sha256sum | cut -c1-16)"
sed -e "s|DEX_IMAGE|$DEX_IMAGE|" -e "s|NGINX_IMAGE|$NGINX_IMAGE|" \
  -e "s|DEX_SERVICE_IP|$DEX_SERVICE_IP|" -e "s|ISSUER_SERVICE_IP|$ISSUER_SERVICE_IP|" \
  -e "s|CONFIG_HASH|$config_hash|" "$here/issuers.yaml" | kubectl apply -f - >/dev/null
for d in dex test-issuer; do
  if ! kubectl -n fixture rollout status "deployment/$d" --timeout=120s; then
    kubectl -n fixture describe pods -l "app=$d" >&2
    kubectl -n fixture logs "deployment/$d" --tail=50 >&2 || true
    exit 1
  fi
done

cat > "$E2E_DIR/env" <<EOF
CLUSTER_NAME=$CLUSTER_NAME
SERVER_CONTAINER=$SERVER_CONTAINER
API_SERVER=https://$server_ip:6443
DEX_ISSUER=https://$DEX_HOST:5556
TEST_ISSUER=$ISSUER_URL
REHEARSAL_USERS=$REHEARSAL_USERS
EOF
echo "fixture ready: KUBECONFIG=$E2E_DIR/kubeconfig"
