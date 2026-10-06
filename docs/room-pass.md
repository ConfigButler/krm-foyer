# Recipe: Room Pass's QR login through krm-foyer

How an application on krm-foyer signs in an audience with
[Room Pass](https://github.com/sunib/room-pass)'s QR join: scan a code, type a display
name, and arrive signed in. Voter asked for this in the
[implementer feedback](implementer-feedback.md) (entry 2), as the release evidence its
audience-release plan requires before switching to krm-foyer.

**Status: run end to end (2026-10-06)** against Room Pass 2.0.0's released image
(`ghcr.io/sunib/room-pass:2.0.0`, revision `2185782`, pinned by digest) and its CRDs,
behind its own Dex, in the e2e fixture, by a real Chromium
([room_pass_test.go](../test/e2e/room_pass_test.go), fixture in
[room-pass.sh](../test/e2e/cluster/room-pass.sh)). [What runs](#what-the-e2e-suite-runs)
lists each check. Running it corrected three details of the first version of this page:
how Room Pass signs out, which the recipe got wrong; the code's `SameSite=Lax`, which a
typed URL does not test; and the display name, which arrives folded.

## The journey

```text
QR code ─► <app>/join-room?code=K7Q2      the application's endpoint, on krm-foyer's host
             sets __Host-room-pass-joincode=K7Q2, then 302 ─►
           /auth/login?return_to=/room&oidc.connector_id=room-pass     krm-foyer
             ─► Dex (Room Pass's issuer host) /auth?connector_id=room-pass
             ─► Room Pass /join on krm-foyer's host: the code is filled in from the
                cookie, the participant types a display name
             ─► Dex ─► /auth/callback (krm-foyer) ─► 303 /room
           /auth/session: {"displayName": "Ada-Lovelace", "groups": ["demo:my-talk"], "connector": "room-pass"}
```

The display name is the one Room Pass stores: what the participant typed, folded to
letters, digits and dashes (`Ada Lovelace` becomes `Ada-Lovelace`), as its join page
shows before they continue. The email is made from it, `ada-lovelace@koudijs.dev.test`.

Three programs share one host: the application, krm-foyer, and Room Pass's `/join`,
`/bind` and `/logout`. That is the point most likely to break. Room Pass's join cookie
reaches `/join` only when the application set it on the same host
(`__Host-` cookies have no `Domain`), and with `SameSite=Lax`. If either is wrong, the
flow does not fail. It quietly falls back to asking the participant to type the code.
Only a browser test catches that, and only one that opens the QR URL the way a scanner
hands it on: from a link, another site's navigation. A URL typed into the address bar,
or opened by a test driver's own navigation, carries even a `Strict` cookie through the
issuer's redirects, so it passes with the mistake Room Pass warns about. The e2e spec
follows a link from another site, and fails with `Strict` (checked by changing it).

## The join endpoint stays the application's

The join code does not go through krm-foyer. Room Pass builds the join URL after Dex, so
no `oidc.*` login parameter could carry it, and the code is not krm-foyer's business.
The application keeps a small endpoint that follows Room Pass's
[cookie contract](https://github.com/sunib/room-pass/blob/main/docs/qr-join.md):

- the value is the room code, bounded before it is set: upper-case `A-Z0-9`, at most 12
  characters (Room Pass ignores anything else);
- `Secure`, `HttpOnly`, `Path=/`, **`SameSite=Lax`**: `Strict` breaks the flow, because
  the cookie must survive the redirects through the issuer host;
- a lifetime of a few minutes; `/join` expires it when it uses it;
- never logged, and never in the authorization request. The code is in the QR URL's
  query, so that includes the edge's access log for that route: in Traefik,
  `observability: {accessLogs: false}` on its own route; in nginx, `access_log off` in
  its `location`.

```go
// GET /join-room?code=K7Q2: the QR code's target. Voter's is voter/oidc.go:401-484.
func joinRoom(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(r.URL.Query().Get("code"))
	if len(code) == 0 || len(code) > 12 || strings.Trim(code, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" {
		http.Error(w, "not a room code", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "__Host-room-pass-joincode", Value: code, Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 300,
	})
	// The destination is the application's own, fixed here, never from the request.
	http.Redirect(w, r, "/auth/login?return_to=%2Froom&oidc.connector_id=room-pass", http.StatusFound)
}
```

Room Pass's `cmd/room-qr` points QR codes at `LOGIN_PATH`, `/auth/login` by default. Set
it to this endpoint: krm-foyer's `/auth/login` takes `return_to`, not `code` or `return`.

## krm-foyer's configuration

```yaml
# Helm values
oidc:
  issuer: https://login.example.com           # Room Pass's issuer host: Dex behind Room Pass
  clientID: voter
  # federated:id makes Dex add federated_claims.connector_id; groups carries the
  # Room's audience group. Add audience:server:client_id:kubernetes if the API server
  # accepts that audience rather than this client's.
  scopes: [openid, email, profile, groups, "federated:id"]
login:
  authorizationParameters:
    connector_id:
      allowFromRequest: true
      allowedValues: [room-pass, github]       # the audience, and the operator
  sessionClaims:
    connector: /federated_claims/connector_id  # name and groups are the defaults
```

In Room Pass's Dex, krm-foyer is a static client with the redirect URI
`https://<host>/auth/callback`, and Room Pass's `ALLOWED_RETURN_URLS` (and the Room's
`allowedReturnURLs`) name `https://<host>/`; `/join` refuses with 400 otherwise.
Room Pass's `JOIN_ORIGIN` is the shared host, port included, since Room Pass routes by
`Host`.

The ID token from a Room Pass login carries `name` (the display name), `email`
(`<participant>@<domain>`, verified), `groups` (the Room's `audienceGroup`, such as
`demo:my-talk`) and `federated_claims.connector_id: room-pass`. Room Pass's reference
API-server configuration (`deploy/apiserver/authentication-config.yaml`) maps the
username to `demo:<sub>`, requires `connector_id in ['room-pass']`, and requires every
group to start with `demo:`. The e2e fixture uses those rules, and also requires the
token's client to be krm-foyer, by the rules of the
[browser identity](application-scope.md#a-browser-identity-in-kubernetes). Whatever the
mapping, `/auth/whoami` and `Krm-Foyer-Identity` show the name it produced.

## The edge: one host, four owners

| Path on the shared host | Goes to |
| --- | --- |
| `/auth/`, `/k8s/`, `/stream/`, `/_foyer/` | krm-foyer |
| `/join`, `/bind`, `/logout` (exact) | Room Pass |
| `/join-room` | The application's join endpoint |
| `/public/` | The application's domain backend, behind [`/auth/check?identity=true`](ingress.md#identity-for-a-domain-backend) |
| everything else | The application's files |

Room Pass's issuer host (`login.example.com`) goes to Room Pass entirely; it forwards
only Dex's protocol paths to Dex, and nothing may expose Dex's Service directly. The
edge strips `X-Remote-*` from every request it sends Room Pass, as Room Pass's own
`deploy/edge/traefik/middlewares.yaml` does: Dex's `authproxy` connector believes those
headers. A NetworkPolicy lets only Room Pass's pods reach Dex. In Traefik that is the
[IngressRoute recipe](ingress.md#recipes) plus Room Pass's routes; the fixture's are in
[routes.yaml](../test/e2e/cluster/room-pass/routes.yaml). The API server, and krm-foyer,
fetch the issuer's discovery and keys at the issuer's host too, so it has to resolve
and be reachable from the control plane and from pods, not only from browsers.

**Logout is two programs, each with its own contract.**

- krm-foyer's `POST /auth/logout`, with the session's CSRF proof, clears krm-foyer's
  session cookie and ends the session's open streams.
- Room Pass's `/logout` accepts only a `POST` with Room Pass's own CSRF token, which only
  its join page holds: a redirect or a link to it is refused with 403. So after
  krm-foyer's logout, send the browser to `/join`. An enrolled participant sees
  "You're already enrolled as …" and a **Sign out of this browser** button, which posts
  to `/logout`, clears Room Pass's `__Host-rp-session` and returns to `/join`, now asking
  for a code.

Neither revokes anything. A copy of krm-foyer's cookie taken before its logout is still
a session until it expires ([Sessions](design.md#sessions)), and the e2e spec checks
exactly that. Room Pass's session cookie is sealed the same way, and the participant
stays enrolled, so the name they chose stays taken: signing in again means joining
under another name. An ID token already issued stays valid at the API server until it
expires. To shut a participant out, revoke the Participant in Room Pass and let the
token's lifetime pass, or end the Room.

## What the e2e suite runs

[room_pass_test.go](../test/e2e/room_pass_test.go) drives Chromium through the
fixture's two hosts, `room.localhost` (the application, krm-foyer-room and Room Pass's
`/join`) and `room-pass.localhost` (the issuer), both through Traefik. The fixture
([room-pass.sh](../test/e2e/cluster/room-pass.sh), [room-pass/](../test/e2e/cluster/room-pass/))
has Room Pass and its Dex with the `authproxy` connector, the CRDs and a Room, cookie
keys and RBAC, certificates from the fixture CA, the API server trusting the issuer as a
third JWT issuer under Room Pass's reference rules, the routes of the table above, and
a test application whose `/join-room` implements the cookie contract in nginx. Nothing
Room Pass's is in krm-foyer: krm-foyer-room is the chart with the generic settings
above ([foyer-room-values.yaml](../test/e2e/cluster/foyer-room-values.yaml)).

| Release check | What the spec does |
| --- | --- |
| The QR journey | Takes the Room's current code as `cmd/room-qr` does, from its status, and follows `https://room.localhost:8443/join-room?code=…` from a link on another site. Expects Room Pass's join page on the shared host with the code shown as scanned and no code field, and the code in no URL but the QR code's own: not in the authorization request, not in Room Pass's handoff URLs. Types a display name and expects to arrive at `/room/` |
| The silent fallback | The same login without the join cookie shows the code field, so the check above can fail. Changing the cookie to `SameSite=Strict` makes it fail with "Room Pass asked for the room code" |
| Display metadata | `/auth/session` has `connector: room-pass`, the folded display name, the made-up email and the Room's audience group |
| A write attributed through both extras | `/auth/whoami` shows `demo:<Dex subject>`, never the typed name, the audience group and both extras. A ConfigMap created from the page with the session's CSRF proof, under a grant to the audience group, has both extras in its audit event, and no impersonation. A Secret, not granted, is the API server's 403 naming that user |
| A pod replacement keeps the session | krm-foyer-room is restarted; the browser's session, its claims and a write still work, with no new login |
| Logout | krm-foyer's `POST /auth/logout`, then Room Pass's sign-out form on `/join`; each cookie is gone, and a new scan asks for a display name again. A copy of krm-foyer's cookie taken before logout still answers `/auth/session`: logout revokes nothing |
| No credential leaks | No page script sees a cookie or storage; no response header or URL the browser saw, nor `/auth/session` or `/auth/whoami`, holds a JWT; krm-foyer-room's, Room Pass's and the application's logs hold no JWT, no cookie value and no join code |

A second spec checks the `authproxy` boundary. From a pod labelled as Room Pass in its
namespace, Dex answers; from a pod with another label there, or with Room Pass's label in
another namespace, the connection is refused: k3s enforces the NetworkPolicy, and
deleting it makes the spec fail. Through the edge, a login followed with forged
`X-Remote-*` headers on every request ends at Room Pass's join form and never at
krm-foyer's callback, and Room Pass's callback and completion paths refuse such a request
outright.

Not run: a phone. The browser is Chromium, which is what Room Pass's own browser test
uses too; Safari's handling of `SameSite` and of a camera-opened URL is not covered.
