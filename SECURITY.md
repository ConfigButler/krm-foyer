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

- **A token reaching the browser.** An ID, access or refresh token in a response body,
  header, readable cookie, log line or error message.
- **A session ID leaking.** The session ID is a bearer credential: one in a log line, a
  URL, an error page, or anywhere outside the cookie is a vulnerability.
- **A request reaching Kubernetes that the allowlist should have refused.** Including
  through path normalization, alternate API versions, subresources, `watch=true`, selectors,
  pagination or discovery.
- **A request made with the wrong credential.** Another user's, or the service's own
  service account standing in for a user.
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

- Anything a user can do with permissions Kubernetes RBAC and the configured allowlist
  already grant them. Enumerating a namespace you are allowed to list is not a leak.
- Denial of service by an already authenticated user within the configured limits.
