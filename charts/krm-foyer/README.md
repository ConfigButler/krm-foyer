# krm-foyer chart

Installs [krm-foyer](../../README.md): one Deployment with one replica (sessions live in
memory), a Service for the origin, a Service for metrics, and the pod's service account.
With `sharedWatches.resources` set, it also creates the
[shared-watch identity](../../docs/watches.md) and its grants.

```bash
kubectl create namespace krm-foyer
kubectl -n krm-foyer create secret generic krm-foyer-oidc --from-literal=client-secret=...
kubectl -n krm-foyer create secret tls krm-foyer-tls --cert tls.crt --key tls.key
helm install krm-foyer oci://ghcr.io/configbutler/charts/krm-foyer --version <release> \
  --namespace krm-foyer \
  --set publicURL=https://app.example.com \
  --set oidc.issuer=https://issuer.example.com,oidc.clientID=krm-foyer \
  --set oidc.clientSecret.secretName=krm-foyer-oidc,tls.secretName=krm-foyer-tls
```

The API server must accept ID tokens from that issuer for that client, and an ingress or
gateway must route `/auth`, `/k8s`, `/stream` and `/_foyer` on the application's origin
to the Service ([ingress](../../docs/ingress.md)).

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
- **Run more than one replica**, until sessions have shared storage.

The tests are in [cmd/krm-foyer/chart_test.go](../../cmd/krm-foyer/chart_test.go): the
rendered arguments parsed by the binary's own flag parsing, the schema's refusals, and
the grants. `task lint-helm` lints it.
