// The hello example: Notes, a Kubernetes custom resource, followed live and edited from
// the browser. Changes go to /k8s on this origin, where krm-foyer sends them on to the
// API server as the signed-in user; what the notes are now comes from krm-foyer's
// /stream, a krm-stream resource stream watched as that same user. There is no backend
// of its own, and no token here: the browser holds only a session cookie that this
// script cannot read.
//
// It shows Kubernetes' answers as they are. A 403 is RBAC saying no, a 409 means the note
// changed since this page last heard of it, and nothing is retried behind the user's back.

import { session, login, logout, k8s } from '/_foyer/foyer.js';
// krm-stream's browser library, one file vendored from its npm package (task
// vendor-krm-stream). It keeps what the server sent apart from what the user typed.
import { LiveResourceStore, applyStreamEvent, connectResourceStream, resourceStreamURL } from './krm-stream.js';

const namespace = new URLSearchParams(location.search).get('namespace') || 'hello';
const group = 'hello.krm-foyer.example';
const notes = `/apis/${group}/v1/namespaces/${encodeURIComponent(namespace)}/notes`;
const text = ['spec', 'text'];

const $ = (id) => document.getElementById(id);

// The store holds every note twice: as Kubernetes last sent it, and as the user is
// editing it. A change that arrives is merged into what the user typed; where both
// changed the same text, it records a conflict instead of choosing.
const store = new LiveResourceStore();
// The editor of each note, by uid. Editors are made once and kept, so a change that
// arrives never throws away a cursor, a selection or text not saved yet.
const editors = new Map();
let connection = null;

// busy keeps button disabled while work runs, so a second click cannot send the same
// change twice.
async function busy(button, work) {
  button.disabled = true;
  try {
    await work();
  } finally {
    button.disabled = false;
  }
}

// What to tell the user for each outcome the helper reports.
const explain = {
  'signed-out': 'You are signed out. Sign in again to go on.',
  // RBAC's refusals have reason Forbidden; krm-foyer's own (a missing CSRF proof) do not.
  refused: 'Refused.',
  conflict: 'Kubernetes has a newer version of this note than this page had, and nothing was saved.',
  invalid: 'Kubernetes did not accept the note.',
  missing: 'It is not there (any more).',
  error: 'Something went wrong.',
};

function say(message, outcome) {
  $('status').textContent = message;
  $('status').dataset.outcome = outcome;
}

// report shows a failed request: what was being done, what that outcome means, and
// what Kubernetes (or krm-foyer) said, verbatim.
function report(doing, answer) {
  const said = answer.message ? ` It said: “${answer.message}”` : '';
  say(`${doing} failed with ${answer.status}. ${explain[answer.outcome]}${said}`, answer.outcome);
  if (answer.outcome === 'signed-out') {
    signedOut();
  }
}

function signedOut() {
  connection?.close();
  $('signed-in').hidden = true;
  $('signed-in-as').hidden = true;
  $('signed-out').hidden = false;
}

// What the stream's connection is doing, in a word or two.
function showConnection(state) {
  const words = {
    connecting: 'Connecting…',
    syncing: 'Catching up…',
    live: 'Live',
    retrying: `Reconnecting in ${Math.ceil((state.retryInMs || 0) / 1000)}s…`,
    terminal: 'Stopped',
    exhausted: 'Disconnected: reload the page to try again',
    closed: '',
  };
  $('live').textContent = words[state.status] ?? state.status;
  $('live').dataset.state = state.status;
}

// connect follows the notes of the namespace into the store, and resolves once the
// first snapshot is complete. Opened again, it takes a fresh snapshot, and every
// unsaved edit stays where it is.
function connect() {
  connection?.close();
  return new Promise((synced) => {
    connection = connectResourceStream(
      resourceStreamURL('/stream/v1', { group, version: 'v1', resource: 'notes', namespace }),
      (event) => {
        applyStreamEvent(store, event);
        if (event.type === 'synced') {
          synced();
        }
      },
      {
        onError: (code, message, terminal) => {
          if (!terminal) {
            return; // the connection recovers on its own
          }
          if (code === 'UNAUTHENTICATED') {
            say(explain['signed-out'], 'signed-out');
            signedOut();
            return;
          }
          say(`The live view stopped: ${message}`, code === 'FORBIDDEN' ? 'refused' : 'error');
        },
      },
    );
    showConnection(connection.state);
    connection.subscribe(showConnection);
    // A rendering bug stops the stream; say so rather than leave it unhandled.
    connection.closed.catch((err) => say(`The live view stopped: ${err.message}`, 'error'));
  });
}

// render brings the editors in line with the store: one per note, in order of name.
// Text from the cluster only ever goes into textContent and value, never into HTML:
// other users write these notes.
function render() {
  const ids = new Set(store.ids());
  for (const [id, editor] of editors) {
    if (!ids.has(id)) {
      editor.li.remove();
      editors.delete(id);
    }
  }
  const items = [...ids].map((id) => editorFor(id));
  items.sort((a, b) => a.name.localeCompare(b.name));
  items.forEach((editor, i) => {
    if ($('notes').children[i] !== editor.li) {
      $('notes').insertBefore(editor.li, $('notes').children[i] || null);
    }
    const draft = store.draft(editor.id).spec.text;
    // Only a change the user did not make moves the text: what they typed is the draft.
    if (editor.text.value !== draft) {
      editor.text.value = draft;
    }
    const conflict = store.conflicts(editor.id).find((c) => c.path.join('/') === text.join('/'));
    editor.conflict.hidden = !conflict;
    if (conflict) {
      editor.theirs.textContent = conflict.theirs;
    }
  });
}

function editorFor(id) {
  if (editors.has(id)) {
    return editors.get(id);
  }
  const name = store.server(id).metadata.name;
  const li = document.createElement('li');
  li.dataset.name = name;
  const label = document.createElement('label');
  label.textContent = name;
  const area = document.createElement('textarea');
  area.maxLength = 280;
  area.rows = 2;
  area.addEventListener('input', () => store.setValue(id, text, area.value));
  label.append(area);

  // Shown when someone else changed the text this user is editing.
  const conflict = document.createElement('p');
  conflict.className = 'conflict';
  conflict.hidden = true;
  const theirs = document.createElement('q');
  const takeTheirs = document.createElement('button');
  takeTheirs.type = 'button';
  takeTheirs.className = 'take-theirs';
  takeTheirs.textContent = 'Take theirs';
  takeTheirs.addEventListener('click', () => store.takeTheirs(id, text));
  conflict.append('Someone else changed this to ', theirs, '. Save to replace it with yours, or ', takeTheirs, '.');

  const save = document.createElement('button');
  save.type = 'button';
  save.className = 'save';
  save.textContent = 'Save';
  save.addEventListener('click', () => busy(save, () => saveNote(id, name, area.value)));
  li.append(label, conflict, save);
  const editor = { id, name, li, text: area, conflict, theirs };
  editors.set(id, editor);
  return editor;
}

// saveNote sends the user's change as a merge patch, with the uid and resourceVersion
// of the note as this page last heard of it: if anyone changed it since, Kubernetes
// answers 409 instead of overwriting their change.
async function saveNote(id, name, value) {
  store.setValue(id, text, value);
  // Captured together, before any await: the patch is the user's edit and nothing else.
  const intent = store.captureSave(id);
  if (!intent) {
    say(`Nothing to save in ${name}.`, 'ok');
    return;
  }
  const answer = await k8s(`${notes}/${encodeURIComponent(name)}`, {
    method: 'PATCH',
    contentType: 'application/merge-patch+json',
    body: { ...intent.patch, metadata: { uid: intent.uid, resourceVersion: intent.resourceVersion } },
  });
  if (answer.outcome === 'ok') {
    // Nothing to adopt: the saved note comes back through the stream, like any change.
    say(`Saved ${name}.`, 'ok');
    return;
  }
  report(`Saving ${name}`, answer);
  if (answer.outcome === 'conflict') {
    // The stream showed no change, so it was one the stream does not show. A fresh
    // snapshot brings the version Kubernetes has now; the user's text stays.
    say(`${$('status').textContent} Catching up…`, 'conflict');
    await connect();
    say(`Saving ${name} failed with 409. ${explain.conflict} The page has caught up, and your text is still here: save again to replace what Kubernetes has.`, 'conflict');
  }
}

function create(event) {
  event.preventDefault();
  return busy($('create'), createNote);
}

async function createNote() {
  const name = $('new-name').value;
  const answer = await k8s(notes, {
    method: 'POST',
    body: {
      apiVersion: `${group}/v1`,
      kind: 'Note',
      metadata: { name },
      spec: { text: $('new-text').value },
    },
  });
  if (answer.outcome !== 'ok') {
    report(`Creating ${name}`, answer);
    return;
  }
  $('new-note').reset();
  // Nothing to add: the new note arrives through the stream, as anyone's would.
  say(`Created ${name}.`, 'ok');
}

async function start() {
  $('namespace').textContent = namespace;
  $('raw').href = `/k8s${notes}`;
  $('sign-in').addEventListener('click', () => login());
  $('logout').addEventListener('click', () => logout());
  $('new-note').addEventListener('submit', create);

  const s = await session();
  if (!s.authenticated) {
    $('signed-out').hidden = false;
    return;
  }
  $('who').textContent = s.email;
  $('signed-in-as').hidden = false;
  $('signed-in').hidden = false;
  store.subscribe(render);
  await connect();
  say(`${store.ids().length} notes, live: a change made elsewhere shows here as it happens.`, 'ok');
}

start().catch((err) => say(`krm-foyer could not be reached: ${err.message}`, 'error'));
