# Recipe: Room Pass's QR login through krm-foyer

How an application on krm-foyer signs in an audience with
[Room Pass](https://github.com/sunib/room-pass)'s QR join: scan a code, type a display
name, and arrive signed in. Voter asked for this in the
[implementer feedback](implementer-feedback.md) (entry 2), as the release evidence its
audience-release plan requires before switching to krm-foyer.

**Status: written, not yet run end to end.** Every name, header and route below comes
from Room Pass 2.0.0's own files (`docs/qr-join.md`, `deploy/`) and from krm-foyer's
configuration. No journey has run against the two together yet. The
[last section](#proving-it) says what running it takes, and which parts krm-foyer's e2e
suite already covers with Dex's static users.

## The journey

```text
QR code ─► <app>/join-room?code=K7Q2      the application's endpoint, on krm-foyer's host
             sets __Host-room-pass-joincode=K7Q2, then 302 ─►
           /auth/login?return_to=/room&oidc.connector_id=room-pass     krm-foyer
             ─► Dex (Room Pass's issuer host) /auth?connector_id=room-pass
             ─► Room Pass /join on krm-foyer's host: the code is filled in from the
                cookie, the participant types a display name
             ─► Dex ─► /auth/callback (krm-foyer) ─► 303 /room
           /auth/session: {"displayName": "...", "groups": ["demo:my-talk"], "connector": "room-pass"}
```

Three programs share one host: the application, krm-foyer, and Room Pass's `/join`,
`/bind` and `/logout`. That is the point most likely to break. Room Pass's join cookie
reaches `/join` only when the application set it on the same host
(`__Host-` cookies have no `Domain`). If the hosts differ, the flow does not fail. It
quietly falls back to asking the participant to type the code. Only a browser test
catches that.

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
- never logged, and never in the authorization request.

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
group to start with `demo:`. Whatever the mapping, `/auth/whoami` and
`Krm-Foyer-Identity` show the name it produced.

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
edge strips `X-Remote-*` from every request, as Room Pass's own
`deploy/edge/traefik/middlewares.yaml` does: Dex's `authproxy` connector believes those
headers. In Traefik that is the [IngressRoute recipe](ingress.md#recipes) plus Room
Pass's routes.

**Logout is two programs.** krm-foyer's `POST /auth/logout` ends the krm-foyer session.
Room Pass's `/logout` ends its participant binding. Call krm-foyer's first, then send the
browser to Room Pass's.

## Proving it

The audience-release plan's checks, against a Room Pass login:

| Check | krm-foyer side, already proved with Dex static users |
| --- | --- |
| The login itself, `connector: room-pass` in `/auth/session` | Login parameters reach Dex as configured and refuse others; `sessionClaims.connector` (`foyer_identity_test.go`) |
| Display metadata | `/auth/session`'s `displayName`, `/auth/whoami`'s extras, `Krm-Foyer-Identity` (`foyer_identity_test.go`, `foyer_check_test.go`) |
| A write attributed through both extras | Admission and the audit event of a write carry both extras (`foyer_identity_test.go`) |
| A pod replacement keeps the session | Sessions are sealed cookies; the rehearsal restarts krm-foyer under 1,800 streams |
| Logout | Logout clears the cookie and ends the session's open streams (`foyer_auth_test.go`, `foyer_stream_test.go`) |

What is not proved is the part that is Room Pass's: the cookie crossing to `/join` on the
shared host, the `authproxy` connector's claims arriving in krm-foyer's session, and
`SameSite=Lax` holding through the issuer host's redirects. Running that needs, in the
e2e fixture: a second Dex in front of Room Pass's public `ghcr.io/sunib/room-pass:2.0.0`
image, as in Room Pass's `deploy/` (the fixture's Dex cannot take an `authproxy`
connector, as its port is reachable directly and anyone could forge the headers); an
issuer host for it, with the API server trusting it as a third JWT issuer; Room Pass's
CRDs, a Room and its cookie Secret; the routes above; and a Chromium spec walking the QR
URL. That is a fixture change of a day or two, and it should be its own pull request.
