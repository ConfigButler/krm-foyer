// krm-foyer's browser helper, served at /_foyer/foyer.js as a plain ES module. It covers
// what every application on a krm-foyer origin needs and easily gets subtly wrong: the
// session, signing in and out, and the CSRF proof on every change through /k8s. It
// holds no token, because there is none in the browser to hold. See docs/frontend.md.
//
//   import { requireSession, k8s, logout } from '/_foyer/foyer.js';

// The current session's CSRF proof, from /auth/session: {header, token}, or null.
let csrf = null;

/**
 * Who is signed in, from /auth/session: {authenticated: false}, or
 * {authenticated: true, email, issuer, subject, expiresAt}.
 */
export async function session() {
  const res = await fetch('/auth/session', { cache: 'no-store' });
  if (res.status !== 200 && res.status !== 401) {
    throw new Error(`/auth/session answered ${res.status}`);
  }
  const s = await res.json();
  csrf = s.authenticated ? { header: s.csrfHeader, token: s.csrfToken } : null;
  return s;
}

/** Sends the browser to sign in, and back to returnTo afterwards: this page by default. */
export function login(returnTo = location.pathname + location.search) {
  location.assign('/auth/login?' + new URLSearchParams({ return_to: returnTo }));
}

/**
 * The session, or, for a signed-out browser, a sign-in that comes back here. Then it
 * never resolves: the page is on its way out.
 */
export async function requireSession() {
  const s = await session();
  if (!s.authenticated) {
    login();
    return new Promise(() => {});
  }
  return s;
}

/** Ends the session, then sends the browser to next. */
export async function logout(next = '/auth/logged-out') {
  const res = await change('/auth/logout', { method: 'POST' });
  if (res.status !== 204) {
    throw new Error(`/auth/logout answered ${res.status}`);
  }
  csrf = null;
  location.assign(next);
}

async function proof() {
  if (!csrf) {
    await session();
  }
  return csrf ? { [csrf.header]: csrf.token } : {};
}

// change sends a request that changes state, with the session's CSRF proof. The proof
// goes stale when the user signs in again in another tab. krm-foyer then refuses the
// request before it reaches Kubernetes, and says so in the Krm-Foyer-Interruption
// header, which it never passes on from upstream. Only that refusal is resent, once:
// a body saying the same could be any API's answer to a request that did arrive.
async function change(url, init) {
  const send = async () => fetch(url, { ...init, headers: { ...init.headers, ...(await proof()) } });
  const res = await send();
  if (res.headers.get('Krm-Foyer-Interruption') !== 'CSRFProofRequired') {
    return res;
  }
  csrf = null;
  return send();
}

// What each status means for an application. Anything else that is not 2xx is 'error'.
const outcomes = {
  401: 'signed-out', // the session ended, or Kubernetes no longer takes its token: sign in again
  403: 'refused', // Kubernetes (or krm-foyer, see reason) will not do this for this user
  404: 'missing',
  409: 'conflict', // changed since it was read, or already exists: reload and decide
  422: 'invalid', // the object breaks the resource's rules
};

/**
 * Sends one request to the Kubernetes API through krm-foyer, and answers what came back:
 * {status, outcome, object, reason, message}. outcome is 'ok' or one of the outcomes
 * above; object is the parsed JSON answer; reason and message come from a Status.
 *
 * Nothing Kubernetes answered is retried: its answer is the answer, and a change that
 * failed is the caller's to make again, after the user has seen why. (A change with a
 * stale CSRF proof never reaches Kubernetes; see change.)
 *
 *   await k8s('/api/v1/namespaces/default/configmaps')
 *   await k8s(path, { method: 'PUT', body: object })
 *   await k8s(path, { method: 'PATCH', body: patch, contentType: 'application/merge-patch+json' })
 */
export async function k8s(path, { method = 'GET', body, contentType = 'application/json' } = {}) {
  const init = { method, headers: { Accept: 'application/json' } };
  if (body !== undefined) {
    init.headers['Content-Type'] = contentType;
    init.body = JSON.stringify(body);
  }
  const reads = method === 'GET' || method === 'HEAD';
  const res = await (reads ? fetch('/k8s' + path, init) : change('/k8s' + path, init));
  let object = null;
  if ((res.headers.get('Content-Type') || '').includes('json')) {
    object = await res.json().catch(() => null);
  }
  const status = object && object.kind === 'Status' ? object : {};
  return {
    status: res.status,
    outcome: res.ok ? 'ok' : outcomes[res.status] || 'error',
    object,
    reason: status.reason || '',
    message: status.message || '',
  };
}
