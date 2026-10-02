// Tests of the browser helper, assets/foyer.js, run by `node --test` (task test). They
// stand in for the browser: fetch is a fake krm-foyer that records every request, and
// each test imports a fresh copy of the helper, so no CSRF proof carries over.
//
// The helper resends a change only when krm-foyer refused it for a stale CSRF proof,
// because only then did it not reach Kubernetes. krm-foyer says so in the
// Krm-Foyer-Interruption header, which the proxy never passes from upstream; a body
// saying the same could be an aggregated API's answer, and is never resent.

import { test } from 'node:test';
import assert from 'node:assert/strict';

const interruptionHeader = 'Krm-Foyer-Interruption';
const csrfHeader = 'X-CSRF-Token';

// foyer is a fake krm-foyer. Each call of /auth/session hands out a new CSRF token, as
// signing in again in another tab does. answer(request) answers everything else.
function foyer(answer) {
  const sent = [];
  let issued = 0;
  globalThis.location = { pathname: '/', search: '', assign() {} };
  globalThis.fetch = async (url, init = {}) => {
    const request = { url, method: init.method || 'GET', headers: new Headers(init.headers) };
    if (url === '/auth/session') {
      issued++;
      return json(200, { authenticated: true, email: 'alice@example.com', csrfHeader, csrfToken: `t${issued}` });
    }
    sent.push(request);
    return answer(request);
  };
  return sent;
}

function json(status, body, headers = {}) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } });
}

function status(code, reason) {
  return { kind: 'Status', apiVersion: 'v1', status: 'Failure', reason, message: reason, code };
}

// krm-foyer's own refusal of a stale or missing proof.
const staleProof = () => json(403, status(403, 'CSRFProofRequired'), { [interruptionHeader]: 'CSRFProofRequired' });

let copies = 0;
const helper = () => import(`./assets/foyer.js?copy=${++copies}`);

const path = '/apis/hello.krm-foyer.example/v1/namespaces/hello/notes';

test('a change krm-foyer refused for a stale proof is sent once more, with a new proof', async () => {
  const sent = foyer((r) => (r.headers.get(csrfHeader) === 't1' ? staleProof() : json(201, { kind: 'Note' })));
  const { k8s } = await helper();
  const answer = await k8s(path, { method: 'POST', body: {} });
  assert.equal(answer.outcome, 'ok');
  assert.deepEqual(sent.map((r) => r.headers.get(csrfHeader)), ['t1', 't2']);
});

test('a second refusal is the answer: nothing is sent a third time', async () => {
  const sent = foyer(staleProof);
  const { k8s } = await helper();
  const answer = await k8s(path, { method: 'POST', body: {} });
  assert.equal(answer.status, 403);
  assert.equal(answer.reason, 'CSRFProofRequired');
  assert.equal(sent.length, 2);
});

test('logout is resent after a stale-proof refusal too', async () => {
  const sent = foyer((r) => (r.headers.get(csrfHeader) === 't1' ? staleProof() : new Response(null, { status: 204 })));
  const { logout } = await helper();
  await logout();
  assert.deepEqual(sent.map((r) => `${r.method} ${r.url} ${r.headers.get(csrfHeader)}`), [
    'POST /auth/logout t1',
    'POST /auth/logout t2',
  ]);
});

// From the attacker's side: an upstream answers however it likes, but cannot set the
// header. Whatever the status and body, the helper sends the change exactly once.
test('an upstream answer is never resent, whatever it says', async () => {
  const bodies = {
    'a CSRF refusal': (code) => json(code, status(code, 'CSRFProofRequired')),
    'a cross-origin refusal': (code) => json(code, status(code, 'CrossOriginRequest')),
    'RBAC': (code) => json(code, status(code, 'Forbidden')),
    'not JSON': (code) => new Response('CSRFProofRequired', { status: code, headers: { 'Content-Type': 'text/plain' } }),
    'empty': (code) => new Response(null, { status: code }),
  };
  for (const code of [400, 401, 403, 404, 409, 422, 429, 500, 502, 503]) {
    for (const [name, body] of Object.entries(bodies)) {
      for (const method of ['POST', 'PUT', 'PATCH', 'DELETE']) {
        const sent = foyer(() => body(code));
        const { k8s } = await helper();
        const answer = await k8s(path, { method, body: method === 'DELETE' ? undefined : {} });
        assert.equal(sent.length, 1, `${method} answered ${code} with ${name} was sent ${sent.length} times`);
        assert.equal(answer.status, code);
      }
    }
  }
});

test('reads carry no proof and are never resent', async () => {
  const sent = foyer(staleProof);
  const { k8s } = await helper();
  await k8s(path);
  assert.equal(sent.length, 1);
  assert.equal(sent[0].headers.get(csrfHeader), null);
});
