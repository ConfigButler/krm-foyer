# krm-foyer chart

Installs [krm-foyer](../../README.md): one Deployment with one replica, a Service for the origin, a Service for metrics, the pod's service account, and
a NetworkPolicy that admits traffic to krm-foyer's ports alone.
With `sharedWatches.resources` set, it also creates the
[shared-watch identity](../../docs/watches.md) and its grants.

```bash
kubectl create namespace krm-foyer
kubectl -n krm-foyer create secret generic krm-foyer-oidc --from-literal=client-secret=...
kubectl -n krm-foyer create secret generic krm-foyer-session-keys \
  --from-literal=session-keys="$(head -c 32 /dev/urandom | base64)"
kubectl -n krm-foyer create secret tls krm-foyer-tls --cert tls.crt --key tls.key
helm install krm-foyer oci://ghcr.io/configbutler/charts/krm-foyer --version <release> \
  --namespace krm-foyer \
  --set publicURL=https://app.example.com \
  --set oidc.issuer=https://issuer.example.com,oidc.clientID=krm-foyer \
  --set oidc.clientSecret.secretName=krm-foyer-oidc,tls.secretName=krm-foyer-tls \
  --set sessionKeys.secretName=krm-foyer-session-keys
```

Sessions are sealed (AES-GCM) into the browser's cookie with the keys in
`sessionKeys`, so they survive an upgrade or a restart; nothing about them is stored in
the cluster. The chart never creates or changes that Secret: keys it generated would
change on every upgrade and end every session. Whoever can read it can read every
user's token from their cookie, so grant `get` on it to no one who should not have that.
To rotate, put a new key on the first line and keep the old one after it until the
sessions it sealed have expired, restarting krm-foyer after each change
([sessions](../../docs/design.md#sessions)).

**Upgrading from 0.1:** `sessionKeys.secretName` is new and required, and
`bounds.sessionIdleTimeout` is gone; the schema refuses it. Everyone signs in again once,
after the first upgrade: the in-memory sessions of 0.1 end with its pod.

The API server must accept ID tokens from that issuer for that client, and an ingress or
gateway must route `/auth`, `/k8s`, `/stream` and `/_foyer` on the application's origin
to the Service ([ingress](../../docs/ingress.md)).

Ask the issuer for every claim the API server's authentication configuration reads. The
default scopes are `openid email profile`; with Dex, a configuration that reads groups
needs `groups`, and one that reads `federated_claims` needs `federated:id`, or the API
server refuses every token after a login that succeeded:

```yaml
oidc:
  scopes: [openid, email, profile, groups, federated:id]
```

Without `tls.secretName`, krm-foyer serves plain HTTP behind the ingress, and the chart
needs `networkPolicy.from` to name the ingress's pods, since that hop carries session
cookies:

```yaml
networkPolicy:
  from:
    - namespaceSelector:
        matchLabels: { kubernetes.io/metadata.name: ingress }
      podSelector:
        matchLabels: { app.kubernetes.io/name: traefik }
```

[values.yaml](values.yaml) documents every value, and
[values.schema.json](values.schema.json) checks them: a misspelt key or a missing
required value fails the install.

What the chart will not do:

- **Grant the pod's service account anything, or mount its token.** krm-foyer never
  sends a request as its pod. `serviceAccount.automountToken` exists for the e2e
  fixture's bait, which makes that account cluster-admin to prove nothing falls back
  to it.
- **Use the pod's account as the shared-watch identity.** The binary allows it when
  pointed at the projected token; the chart refuses, so the pod's account stays bound
  to nothing.
- **Create the session keys.** They must outlive every upgrade.
- **Run more than one replica.** Sessions would survive it, but logins in progress,
  the per-session bounds and logout's reach are still each pod's own
  ([roadmap](../../docs/roadmap.md#order-of-work)).

The tests are in [cmd/krm-foyer/chart_test.go](../../cmd/krm-foyer/chart_test.go): the
rendered arguments parsed by the binary's own flag parsing, the schema's refusals, and
the grants. `task lint-helm` lints it.
