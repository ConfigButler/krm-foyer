#!/usr/bin/env bash
# Forwards the two things a browser needs to this container's loopback, the way
# gitops-reverser's hack/e2e/setup-port-forwards.sh does:
#
#   127.0.0.1:8443  Traefik, the front door: the hello example and krm-foyer, https://foyer.localhost:8443
#   127.0.0.1:5556  Dex, https://dex.localhost:5556
#
# Browsers resolve *.localhost to loopback. In a VS Code devcontainer, VS Code forwards
# both ports to the machine the browser runs on (devcontainer.json), so this works
# wherever Docker runs. The browser specs run Chromium in this container's network
# namespace, so they take exactly this path too.
#
# Each forward is detached with setsid so it outlives this shell, and is always
# restarted: kubectl port-forward follows one pod, and a rolled pod ends it.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
E2E_DIR="${E2E_DIR:-$(cd "$here/../../.." && pwd)/.e2e}"
export KUBECONFIG="$E2E_DIR/kubeconfig"

# forward NAMESPACE SERVICE PORT:REMOTE NAME PATH: svc/SERVICE on 127.0.0.1:PORT, checked by
# asking https://NAME:PORT/PATH.
forward() {
  local namespace="$1" service="$2" ports="$3" name="$4" path="$5" log="$E2E_DIR/port-forward-$2.log"
  local port="${ports%:*}"
  pkill -f "kubectl port-forward .*svc/$service $ports" 2>/dev/null || true
  # Wait for the old forward to let go of the port.
  for _ in $(seq 1 20); do
    timeout 1 bash -c "echo >/dev/tcp/127.0.0.1/$port" 2>/dev/null || break
    sleep 0.25
  done
  setsid kubectl port-forward --address 127.0.0.1 -n "$namespace" "svc/$service" "$ports" \
    >"$log" 2>&1 </dev/null &
  local pid=$!
  for _ in $(seq 1 30); do
    if ! kill -0 "$pid" 2>/dev/null; then
      echo "the port-forward to $service stopped:" >&2
      cat "$log" >&2
      exit 1
    fi
    if curl -fsS --cacert "$E2E_DIR/ca.crt" --resolve "$name:$port:127.0.0.1" \
      -o /dev/null "https://$name:$port$path" 2>/dev/null; then
      echo "https://$name:$port -> svc/$service"
      return 0
    fi
    sleep 0.5
  done
  echo "https://$name:$port does not answer through the port-forward to $service" >&2
  cat "$log" >&2
  exit 1
}

forward fixture dex 5556:5556 dex.localhost /.well-known/openid-configuration
# Traefik's Service answers on 443; the browser's origin keeps port 8443.
forward traefik-system traefik 8443:443 foyer.localhost /
