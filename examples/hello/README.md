# hello: notes in the browser, as Kubernetes resources

The smallest application on krm-foyer: one HTML file, one script, a stylesheet and
krm-stream's browser library, with no build step and no backend of its own. It follows
Notes, a custom resource, live, and creates and edits them as the signed-in user.
Kubernetes decides what that user may do, and the page shows its answers as they are: a
403 when RBAC says no, a 409 when the note changed first.

```bash
task demo   # from the repository root; task e2e-down removes it
```

Then open <https://foyer.localhost:8443> and sign in with password `password` as
`alice@example.com` (may edit) or `bob@example.com` (may only read); the e2e suite's
200 rehearsal users, `rehearsal-001@example.com` to `rehearsal-200@example.com`, may read
as bob does. Open the page in two
tabs: a note saved in one changes in the other as you watch. Type in a note in one tab
without saving, save a change to it in the other, and the first shows the conflict,
keeping what you typed.

## What is here

| File | |
| --- | --- |
| [web/index.html](web/index.html), [web/app.js](web/app.js), [web/style.css](web/style.css) | The application. Copy these to start your own |
| [web/krm-stream.js](web/krm-stream.js) | [krm-stream](https://github.com/ConfigButler/krm-stream)'s browser library, the single-file bundle from its npm package. Vendored with `task vendor-krm-stream`; `task verify` checks it is the published file, byte for byte |
| [manifests.yaml](manifests.yaml) | The `Note` resource, the `hello` namespace, and the grants: alice edits, bob reads. These grants are the whole of the access rules |
| [notes.yaml](notes.yaml) | Two notes to start with |

## How it fits together

The application and krm-foyer share one origin. In the e2e fixture, a Gateway
([gateway.yaml](../../test/e2e/cluster/gateway.yaml), implemented by Traefik) sends `/` to
a file server for these files and `/auth`, `/k8s`, `/stream` and `/_foyer` to krm-foyer.
Any ingress or gateway that routes by path can do the same.

`app.js` reads through `/stream` and writes through `/k8s`. In the fixture, notes are
[shared](../../docs/watches.md): every open page reads one watch at the API server, and
krm-foyer asks the API server whether each user may. The page cannot tell, and does not
change either way.

```js
import { session, login, k8s } from '/_foyer/foyer.js';
import { LiveResourceStore, connectManagedResourceStream, resourceStreamURL } from './krm-stream.js';

const s = await session();          // {authenticated, email, ...}; never a token
if (!s.authenticated) login();      // to the issuer, and back to this page

// The notes as they are, and as they change, for what RBAC lets this user see.
const store = new LiveResourceStore();
connectManagedResourceStream(resourceStreamURL('/stream/v1',
  { group: 'hello.krm-foyer.example', version: 'v1', resource: 'notes', namespace: 'hello' }), store);

// A save is the user's edit alone, on the version they last saw.
const { uid, resourceVersion, patch } = store.captureSave(id);
const saved = await k8s(path, { method: 'PATCH', contentType: 'application/merge-patch+json',
  body: { ...patch, metadata: { uid, resourceVersion } } });
if (saved.outcome === 'conflict') { /* 409: keep the text, catch up, let the user save again */ }
if (saved.outcome === 'refused') { /* 403: saved.message says why */ }
```

The store keeps what Kubernetes sent apart from what the user typed. A change that
arrives is merged into the text being edited; where both changed it, the store records a
conflict and the page shows it, and nothing is overwritten until the user takes theirs
or saves. The helper adds the CSRF header to every change and retries nothing that
Kubernetes answered. Things to keep when you copy this:

- **Put text from the cluster into `textContent` or `value`, never into HTML.** Other
  users write these notes, and anything that runs on this origin acts as the signed-in
  user.
- **Save the user's edit with the `uid` and `resourceVersion` they last saw**, as
  `captureSave` gives them. Then a change made in the meantime is a 409 instead of being
  overwritten. Do not take the version from a newer read: it would hide that change.
- **Let the saved note come back through the stream.** The page adopts nothing from the
  answer to a save, so a change made after it can never be overwritten by it.
- **Make editors once, and keep them.** Rebuilding the list on every change would throw
  away cursors and text not saved yet.
- **Serve the page with a strict Content-Security-Policy** that allows scripts from your
  own origin only, as the fixture's file server does
  ([hello-web-nginx.conf](../../test/e2e/cluster/hello-web-nginx.conf)).

A 409 can still happen when the page shows the latest text. Two saves can cross: each
is sent before the other's change has come back through the stream. And a change to what
the stream does not show, such as the `kubectl.kubernetes.io/last-applied-configuration` annotation
that `kubectl apply` writes, moves the note's version without an event. The page then
opens its stream again for the current version, keeps the user's text, and saves only
when asked again. See krm-stream's
[saving guide](https://github.com/ConfigButler/krm-stream/blob/main/docs/saving.md#why-a-quiet-stream-can-still-reject-a-save).

`?namespace=other` points the page at another namespace; the browser specs use that to
work in a namespace of their own.
