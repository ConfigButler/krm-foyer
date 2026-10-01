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
  const res = await fetch('/auth/logout', { method: 'POST', headers: await proof() });
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
 * failed is the caller's to make again, after the user has seen why. The one resend is
 * of a change krm-foyer refused for its CSRF proof, which never reached Kubernetes: the
 * user signed in again elsewhere, so the proof is read anew and the change sent once more.
 *
 *   await k8s('/api/v1/namespaces/default/configmaps')
 *   await k8s(path, { method: 'PUT', body: object })
 *   await k8s(path, { method: 'PATCH', body: patch, contentType: 'application/merge-patch+json' })
 */
export async function k8s(path, options = {}) {
  const answer = await send(path, options);
  if (answer.status === 403 && answer.reason === 'CSRFProofRequired') {
    csrf = null;
    return send(path, options);
  }
  return answer;
}

async function send(path, { method = 'GET', body, contentType = 'application/json' }) {
  const headers = { Accept: 'application/json' };
  if (method !== 'GET' && method !== 'HEAD') {
    Object.assign(headers, await proof());
  }
  if (body !== undefined) {
    headers['Content-Type'] = contentType;
  }
  const res = await fetch('/k8s' + path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
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
