// The hello example: list, create and edit Notes, a Kubernetes custom resource, from the
// browser. Every request goes to /k8s on this origin, where krm-foyer sends it on to the
// API server as the signed-in user. There is no backend of its own, and no token here:
// the browser holds only a session cookie that this script cannot read.
//
// It shows Kubernetes' answers as they are. A 403 is RBAC saying no, a 409 means the note
// changed since it was loaded, and nothing is retried behind the user's back.

import { session, login, logout, k8s } from '/_foyer/foyer.js';

const namespace = new URLSearchParams(location.search).get('namespace') || 'hello';
const notes = `/apis/hello.krm-foyer.example/v1/namespaces/${encodeURIComponent(namespace)}/notes`;

const $ = (id) => document.getElementById(id);

// busy keeps button disabled while work runs, so a second click cannot send the same
// change twice: the second POST would fail with 409 and hide the first one's success.
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
  conflict: 'Someone changed this since you loaded it. Reload to see their version, then make your change again.',
  invalid: 'Kubernetes did not accept the note.',
  missing: 'It is not there (any more). Reload to see what is.',
  error: 'Something went wrong.',
};

function say(text, outcome) {
  $('status').textContent = text;
  $('status').dataset.outcome = outcome;
}

// report shows a failed request: what was being done, what that outcome means, and
// what Kubernetes (or krm-foyer) said, verbatim.
function report(doing, answer) {
  const said = answer.message ? ` It said: “${answer.message}”` : '';
  say(`${doing} failed with ${answer.status}. ${explain[answer.outcome]}${said}`, answer.outcome);
  if (answer.outcome === 'signed-out') {
    $('signed-in').hidden = true;
    $('signed-in-as').hidden = true;
    $('signed-out').hidden = false;
  }
}

async function reload() {
  const answer = await k8s(notes);
  if (answer.outcome !== 'ok') {
    report('Listing notes', answer);
    return;
  }
  const items = answer.object.items.sort((a, b) => a.metadata.name.localeCompare(b.metadata.name));
  $('notes').replaceChildren(...items.map(noteItem));
  say(`${items.length} notes, as of now. Reload to see changes others made.`, 'ok');
}

// noteItem builds one note's editor. Text from the cluster only ever goes into
// textContent and value, never into HTML: other users write these notes.
function noteItem(note) {
  const li = document.createElement('li');
  li.dataset.name = note.metadata.name;
  const name = document.createElement('label');
  name.textContent = note.metadata.name;
  const text = document.createElement('textarea');
  text.value = note.spec.text;
  text.maxLength = 280;
  text.rows = 2;
  name.append(text);
  const save = document.createElement('button');
  save.type = 'button';
  save.className = 'save';
  save.textContent = 'Save';
  save.addEventListener('click', () => busy(save, async () => {
    // A PUT of the object as it was loaded, metadata.resourceVersion included: if anyone
    // changed the note since, Kubernetes answers 409 instead of overwriting their change.
    const changed = structuredClone(note);
    changed.spec.text = text.value;
    const answer = await k8s(`${notes}/${encodeURIComponent(note.metadata.name)}`, { method: 'PUT', body: changed });
    if (answer.outcome !== 'ok') {
      report(`Saving ${note.metadata.name}`, answer);
      return;
    }
    note = answer.object;
    say(`Saved ${note.metadata.name}.`, 'ok');
  }));
  li.append(name, save);
  return li;
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
      apiVersion: 'hello.krm-foyer.example/v1',
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
  await reload();
  say(`Created ${name}.`, 'ok');
}

async function start() {
  $('namespace').textContent = namespace;
  $('raw').href = `/k8s${notes}`;
  $('sign-in').addEventListener('click', () => login());
  $('logout').addEventListener('click', () => logout());
  $('reload').addEventListener('click', reload);
  $('new-note').addEventListener('submit', create);

  const s = await session();
  if (!s.authenticated) {
    $('signed-out').hidden = false;
    return;
  }
  $('who').textContent = s.email;
  $('signed-in-as').hidden = false;
  $('signed-in').hidden = false;
  await reload();
}

start().catch((err) => say(`krm-foyer could not be reached: ${err.message}`, 'error'));
