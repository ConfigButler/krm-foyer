# hello: notes in the browser, as Kubernetes resources

The smallest application on krm-foyer: one HTML file, one script and a stylesheet, with
no build step and no backend of its own. It lists, creates and edits Notes, a custom
resource, as the signed-in user. Kubernetes decides what that user may do, and the page
shows its answers as they are: a 403 when RBAC says no, a 409 when someone else changed
the note first.

```bash
task demo   # from the repository root; task e2e-down removes it
```

Then open <https://foyer.localhost:8443> and sign in with password `password` as
`alice@example.com` (may edit) or `bob@example.com` (may only read). To see a 409, open
the page in two tabs, save in one, then save in the other.

## What is here

| File | |
| --- | --- |
| [web/index.html](web/index.html), [web/app.js](web/app.js), [web/style.css](web/style.css) | The application. Copy these to start your own |
| [manifests.yaml](manifests.yaml) | The `Note` resource, the `hello` namespace, and the grants: alice edits, bob reads. These grants are the whole of the access rules |
| [notes.yaml](notes.yaml) | Two notes to start with |

## How it fits together

The application and krm-foyer share one origin. In the e2e fixture, a Gateway
([gateway.yaml](../../test/e2e/cluster/gateway.yaml), implemented by Traefik) sends `/` to
a file server for these files and `/auth`, `/k8s`, `/stream` and `/_foyer` to krm-foyer.
Any ingress or gateway that routes by path can do the same.

`app.js` imports krm-foyer's helper from `/_foyer/foyer.js`:

```js
import { session, login, logout, k8s } from '/_foyer/foyer.js';

const s = await session();          // {authenticated, email, ...}; never a token
if (!s.authenticated) login();      // to the issuer, and back to this page

const list = await k8s('/apis/hello.krm-foyer.example/v1/namespaces/hello/notes');
const saved = await k8s(path, { method: 'PUT', body: note });
if (saved.outcome === 'conflict') { /* 409: reload, and let the user decide */ }
if (saved.outcome === 'refused') { /* 403: saved.message says why */ }
```

The helper adds the CSRF header to every change and retries nothing that Kubernetes
answered. Things to keep when you copy this:

- **Put text from the cluster into `textContent` or `value`, never into HTML.** Other
  users write these notes, and anything that runs on this origin acts as the signed-in
  user.
- **Send the object back with the `resourceVersion` you read.** Then a change made in
  the meantime is a 409 instead of being overwritten.
- **Serve the page with a strict Content-Security-Policy** that allows scripts from your
  own origin only, as the fixture's file server does
  ([hello-web-nginx.conf](../../test/e2e/cluster/hello-web-nginx.conf)).

`?namespace=other` points the page at another namespace; the browser specs use that to
work in a namespace of their own.
