# Security policy

## Reporting a vulnerability

**Do not open a public issue.** Report privately through GitHub's
[report a vulnerability](https://github.com/ConfigButler/krm-foyer/security/advisories/new)
form, which opens an advisory only the maintainers can see.

Expect an acknowledgement within 3 working days and an assessment within 10. If a fix is
warranted we will agree a disclosure date with you, and credit you in the advisory unless
you would rather we did not.

## Supported versions

Pre-1.0: there is no released version yet. Once there is, the latest released minor
receives security fixes.

## What counts as a vulnerability here

krm-foyer holds users' sessions and forwards their requests to a Kubernetes API server, so
we treat the following as security bugs:

- **A login or shared-watch token reaching the browser.** A token krm-foyer holds in a response body,
  header, readable cookie, log line or error message.
- **A session ID leaking.** The session ID is a bearer credential: one in a log line, a
  URL, an error page, or anywhere outside the cookie is a vulnerability.
- **krm-foyer deciding access.** A request answered differently through krm-foyer than the
  API server answers the same token directly, except the documented
  [interruptions](docs/design.md#interruptions), or a non-canonical path forwarded
  instead of rejected. Shared streams must follow their documented review and
  reauthorization contract.
- **A request made with the wrong credential.** Another user's, or the service's own
  service account standing in for a user.
- **A shared watch read without the API server's say.** A stream served from a shared
  watch to a user the API server did not allow `list` and `watch` on that scope, a
  decision reused for a different subject or scope, or the shared-watch identity used
  for anything but shared watches and their reviews.
- **Session and login flaws.** Session fixation, a missing CSRF check on a mutation or
  logout, an open redirect through the login return path, or a callback accepted without
  state, nonce or PKCE validation.
- **Forwarding what must be stripped.** A browser-supplied `Authorization`, impersonation or
  forwarding header, or the session cookie, reaching the API server. In the other
  direction: an upstream `Set-Cookie` or CORS header reaching the browser, a redirect
  followed without the user's click, or upstream HTML rendered on the origin.
- **A page where code expects JSON.** An interruption page that a script can obtain, or
  one that replaces an answer from Kubernetes.
- **A session outliving its revocation** beyond the bounds in the
  [design](docs/design.md#session-lifecycle).

## What does not

- Anything a user can do with permissions Kubernetes RBAC already grants them, including
  through the browser, including reading Secrets or creating tokens where those
  permissions are granted. The server-held-token guarantee is not a filter on
  authorized Kubernetes resource data. krm-foyer has no [application scope](docs/application-scope.md)
  yet, so a session carries the user's full access by design. Enumerating a namespace you
  are allowed to list is not a leak.
- Denial of service by an already authenticated user within the configured limits.
