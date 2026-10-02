# Choosing an OIDC issuer

krm-foyer signs users in through an OpenID Connect issuer. The issuer must be one that
the Kubernetes API server also trusts, because krm-foyer forwards the user's own token and
lets the API server decide who it belongs to. krm-foyer does not ship an issuer. You bring
your own.

This page explains which issuer krm-foyer's tests and `task demo` use and why, and how
the other common choices fit. It covers [Dex](https://dexidp.io),
[Pinniped](https://pinniped.dev), [Keycloak](https://www.keycloak.org),
[authentik](https://goauthentik.io), [Authelia](https://www.authelia.com) and
[OpenUnison](https://openunison.github.io). It was written in October 2026. Only Dex is
tested; what it says about the others comes from their documentation and, for Pinniped,
its source.

## Short answer

- **Dex is the issuer in our tests and in the demo.** It runs as one container with its
  own users, can hand the test suite a token without a browser, keeps its state in the
  cluster, and its tokens can live long enough for krm-foyer as it is today.
- **Any issuer that meets the [requirements](#what-krm-foyer-needs-from-an-issuer) can
  work.** krm-foyer has no Dex-specific code.
- **Several issuers need token refresh, which krm-foyer does not do yet.** Pinniped and
  OpenUnison issue tokens that last minutes. Today a krm-foyer session ends when its ID
  token expires. Refresh is [roadmap step 5](../roadmap.md#order-of-work), and krm-foyer
  must support it before those issuers are a comfortable choice.

## What krm-foyer needs from an issuer

1. **The authorization code flow with PKCE**, for a confidential client whose redirect URI
   is krm-foyer's public URL plus `/auth/callback`.
2. **An ID token the API server accepts.** The API server must be able to fetch the
   issuer's keys, and its `AuthenticationConfiguration` must list the issuer with
   krm-foyer's client ID as the audience. Our fixture's
   [authentication-config.yaml](../../test/e2e/cluster/authentication-config.yaml) is a
   worked example.
3. **Claims your API server can map to a user.** The fixture maps the username from
   `email`, and only when `email_verified` is the boolean `true`. An issuer that sends
   other claims needs other `claimMappings`. Keep a rule that refuses the `system:` prefix
   either way.
4. **Scopes krm-foyer may ask for.** krm-foyer asks for `openid email profile` by default;
   `-oidc-scopes` changes that. It refuses `offline_access` until it can refresh.
5. **An ID token that lasts as long as you want a session to last**, for now. Without
   refresh, the token's expiry ends the session. Once refresh lands, short tokens become
   an advantage instead.

## The issuers side by side

| Issuer | What it is | State it keeps | Fit for krm-foyer today |
| --- | --- | --- | --- |
| Dex | A small OIDC issuer, with its own users or in front of LDAP, GitHub, SAML or another OIDC provider | Kubernetes custom resources with `storage: type: kubernetes`; no volume | Tested. Used by the tests and the demo |
| Pinniped | Kubernetes login: a Supervisor that federates an upstream provider, and a Concierge for clusters you cannot configure | Kubernetes Secrets; no volume. Needs an upstream provider | Works, with sessions of 30 minutes at most until refresh lands |
| Keycloak | Full identity management: users, roles, brokering, OIDC and SAML | A relational database. Its built-in `dev-file` database is not for production | Should work. Raise the token lifespan for krm-foyer's client until refresh lands |
| authentik | Authentication flows and application integrations; documents Kubernetes CLI login | PostgreSQL (Redis is no longer needed since 2025.10). Uploaded files need a volume or S3 | Should work, once `email_verified` is fixed (see below) |
| Authelia | Reverse-proxy authentication with an OIDC provider; documents kubelogin | SQLite, PostgreSQL or MySQL. Several replicas also need Redis for sessions | Should work |
| OpenUnison | Kubernetes login portal for kubectl and dashboards, with optional access provisioning | The login portal keeps sessions as Kubernetes custom resources; some provisioning features add a database | Works with short tokens only until refresh lands |

If you want no database at all, Dex, Pinniped and OpenUnison's login portal keep their
state in the cluster. Keycloak, authentik and Authelia each bring a database to run, back
up and upgrade. In return, they also manage users, roles and second factors, which Dex
and Pinniped leave to an upstream provider.

## Why the tests use Dex

| Need | Dex | The alternatives |
| --- | --- | --- |
| Users without an outside service | `staticPasswords`, in the same container | Pinniped and OpenUnison need an upstream provider. Keycloak, authentik and Authelia have users, but also a database |
| A user's token without a browser | The password grant. The suite asks the API server directly what each user may do and compares that with krm-foyer's answer | A registered Pinniped web client may use only `authorization_code`, `refresh_token` and token exchange |
| A session longer than the ID token, without refresh | ID token lifetime is set in `expiry` | Pinniped: 2 minutes by default, 30 at most. OpenUnison: about a minute in its examples |
| Parts to install | One Deployment | Pinniped and OpenUnison: the issuer plus an upstream provider. The others: the issuer plus its database |

The security boundaries krm-foyer tests are the same with any issuer: the token stays on
the server, and the API server makes every access decision. The smallest fixture tests
the same boundaries.

## krm-foyer's own storage

Choosing an issuer with no database does not remove krm-foyer's own **session store**: the
session, and after refresh lands, the refresh token. That store belongs to krm-foyer
whatever the issuer is. Today it is in memory, so krm-foyer runs one replica.

Pinniped and OpenUnison show an approach worth copying: keep sessions in the cluster.
Pinniped's Supervisor keeps them as Secrets and removes expired ones with a garbage
collector (`internal/fositestorage` in its source). A store like that would let krm-foyer
run several replicas with no Redis or database. Two things need care: how often it writes
to etcd (not on every request), and who can read Secrets in its namespace. Keying sessions
by a hash of the ID, as krm-foyer already does, helps with the second.

## Notes per issuer

### Dex

Use `storage: type: kubernetes`, as our fixture does ([dex.yaml](../../test/e2e/cluster/dex.yaml)),
and grant only what that storage needs ([issuers.yaml](../../test/e2e/cluster/issuers.yaml)).
Do not use `memory` storage. A restarted pod signs with new keys, and the API server goes
on refusing every token until it fetches them again. We measured 221 seconds on k3s 1.36.

### Pinniped

These notes were checked against Pinniped's `main` branch of August 2026.

**The API server must trust the Supervisor directly.** Add the FederationDomain's issuer to
the API server's `AuthenticationConfiguration` with krm-foyer's `OIDCClient` name as the
audience. A Concierge `JWTAuthenticator` with the same issuer and audience also works.

Pinniped's guide for web applications describes a different path:

1. Exchange the token for one scoped to a single cluster (RFC 8693).
2. Trade that token for a short-lived mTLS client certificate through the Concierge's
   `TokenCredentialRequest`.

krm-foyer does neither, so the per-cluster audience and the client certificates are not
available through krm-foyer.

**Ask only for scopes the client allows.** A Pinniped `OIDCClient` accepts `openid`,
`offline_access`, `username`, `groups` and `pinniped:request-audience`. Start krm-foyer
with `-oidc-scopes openid,username,groups`.

**Map identity from Pinniped's claims.** Pinniped's ID tokens carry `username` and `groups`,
and no `email` or `email_verified`. Map the username from `username`.

**Expect short sessions until refresh lands.** ID tokens last 2 minutes by default, or 30
at most with `tokenLifetimes.idTokenSeconds: 1800`. After that the browser gets the
documented 401 and must sign in again.

**What it gives you once refresh works:**

- **Fast removal of disabled users.** The Supervisor checks the user with the upstream
  provider on every refresh, and its tokens are short. That is the bound the
  [session lifecycle](../design.md#session-lifecycle) table leaves to the issuer.
- **Group changes that arrive on their own.** A refresh picks up new memberships.
- **Several upstream providers and identity transformations** in one FederationDomain.
- **Managed clusters** whose API server flags you cannot set. The Concierge's impersonation
  proxy can accept Supervisor tokens there. krm-foyer has not been tested against it.

### Keycloak

Register krm-foyer as a confidential client with the standard flow. Keycloak's ID tokens
carry `email` and `email_verified`, so the fixture's claim rules fit, as long as users'
emails are verified in Keycloak. The ID token follows the access token lifespan, which is
short by default. Raise it for krm-foyer's client until refresh lands.

### authentik

Since 2025.10, authentik's default `email` scope mapping sends `email_verified: false`,
because authentik has no single source for whether an email is verified. The fixture's
rule refuses such a token with 401. Either add a scope mapping that returns the user's
real verification status, or map the username from another claim, such as
`preferred_username`. Do not return `true` for every user to get past the rule; the rule
exists so that only a vouched-for name becomes an identity.

### Authelia

Authelia's OIDC provider works with any client that uses the standard flow. One replica can
run on SQLite with sessions in memory. Several replicas need PostgreSQL or MySQL, and Redis
for sessions.

### OpenUnison

Register krm-foyer with a `Trust` custom resource. OpenUnison is usually installed with its
own Kubernetes identity provider, whose issuer the API server trusts; check that the
`Trust` you create issues tokens from that issuer, for an audience the API server lists.
Its tokens are short-lived, so it needs refresh, as Pinniped does.

## Before we call another issuer supported

None of these is scheduled. They would follow refresh:

- [ ] Refresh and shared session storage ([roadmap step 5](../roadmap.md#order-of-work))
- [ ] An e2e fixture for the issuer, running the login, refresh and logout journeys with
  its default token lifetimes. For Pinniped or OpenUnison, Dex can be the upstream
- [ ] The disablement bound measured for that issuer: disable the user, then time how long
  until the refresh is refused
