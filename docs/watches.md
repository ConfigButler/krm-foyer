# Watches: which kind to use

A page that shows live Kubernetes state can get it three ways through krm-foyer. This
page helps you choose one, then explains how to set up the one that needs setting up:
shared watches. **Use a stream for anything live in a page, share it when
many users watch the same thing, and treat native watches as a fallback.**

## The three kinds

| | Per-user stream | Shared stream | Native watch through `/k8s` |
| --- | --- | --- | --- |
| How the page opens it | `/stream/v1`, krm-stream's client | `/stream/v1`, krm-stream's client: the page cannot tell the two apart | `GET /k8s/...?watch=1`, read with `fetch` |
| Who opens the watch at the API server | The user, with their own token | krm-foyer's shared-watch identity, once per scope | The user, with their own token |
| Watches at the API server for N streams of one scope | N | 1 | N |
| Who decides what the user sees | The API server, on the user's own request | The API server, by a SubjectAccessReview about the user, at opening and every recheck | The API server, on the user's own request |
| When RBAC changes | The open watch carries on until it ends (Kubernetes does not end watches on RBAC changes) | The stream ends at the next recheck: within a minute at the defaults ([bound](#revocation)) | As per-user stream |
| What the page gets | Snapshot, changes, resync after a gap, reconnect with backoff, drafts kept | The same | Raw watch events; the page reconnects and resumes itself |
| In the audit log, the watch is by | The user | The shared-watch identity; the reviews name the user | The user |
| Configuration | None | A list of resources and an identity | None |

All three end when their session ends, and count against the [bounds](bounds.md).

## Choosing

**1. Is it a live view in a page?** Use a stream. krm-stream does the work every
Kubernetes client has to do (split events, resume from the last `resourceVersion`,
start again after a gap), and it does it once, tested, for every page. A native watch
leaves that work to page code. Use a native watch only for what already speaks the
Kubernetes watch protocol (a client library, an API explorer in a tab), or for a quick
look while debugging. krm-foyer keeps native watches working and bounded
([why](bounds.md#native-watches)), but optimizes nothing for them and offers no helper.

**2. Per-user or shared?** Share a resource when all of these hold:

- **Many users watch the same scope.** A scope is one resource in one namespace (or
  every namespace), optionally one name, optionally one label selector. A team's
  dashboard, a namespace everyone in a group follows, the hello example's notes:
  sharing turns N watches into one. If scopes are mostly personal (each user their own
  namespace), sharing saves little and costs a review per user; stay per-user.
- **You can name an identity that may list and watch the resource wherever users
  watch it.** Usually a ClusterRole on that one resource. That identity reads more than
  any one user, so keep it narrow, and never share Secrets or anything you would not
  give that identity cluster-wide.
- **A revocation bound of tens of seconds is acceptable.** A shared stream learns of an
  RBAC change at its next recheck. (A per-user stream does not learn of one at all until
  its watch ends, so this is rarely worse; but it is a number you choose.)
- **The audit log naming the reviewed user, rather than the watcher, is enough.** The
  watch is the shared identity's; the SubjectAccessReviews record who was asked about.

Otherwise stream per-user, which needs no configuration. You can switch a resource
either way later: only krm-foyer's flags change, never the page.

| Situation | Choose |
| --- | --- |
| A list many people watch in a shared namespace | Shared stream |
| Per-user or per-tenant namespaces | Per-user stream |
| Secrets, or anything sensitive cluster-wide | Per-user stream |
| One user with many tabs on the same list | Either; shared saves the extra watches |
| A tool that speaks the Kubernetes watch protocol | Native watch |

## Setting up shared watches

Shared watches are off until two flags are given:

```text
-shared-watch-resources=notes.hello.krm-foyer.example,configmaps
-shared-watch-token-file=/etc/krm-foyer/shared-watch/token
```

Resources are named as kubectl names them: `resource.group`, or `resource` for the core
group. Every other resource is streamed per-user, as before.

### The identity

The token file holds a service account's token. That account needs:

- `list` and `watch` on each shared resource, wherever users will watch it: usually a
  ClusterRole bound with a ClusterRoleBinding;
- `create` on `subjectaccessreviews` in `authorization.k8s.io`;
- nothing else.

The fixture binds `system:auth-delegator` for the reviews. That built-in role also
grants `create` on `tokenreviews`, which krm-foyer does not need. A deployment can use
a narrower ClusterRole containing only the SubjectAccessReview grant.

krm-foyer reads the file again as it changes, so a rotating token works. It never uses
the pod's own service account by itself: if you want that account to be the
shared-watch identity, point the flag at its projected token
(`/var/run/secrets/kubernetes.io/serviceaccount/token`) and give it only the grants
above. Otherwise mount a token of a separate account, as the
[e2e fixture](../test/e2e/cluster/foyer.yaml) does, where the pod's own account is
cluster-admin bait that must never be used.

### What happens for each stream

1. The browser opens `/stream/v1` for a shared resource, through the same gate as every
   stream: session, bounds, request rate.
2. krm-foyer asks the API server who the user is, with a SelfSubjectReview sent with
   the user's own token: the username, groups, UID and extras the API server makes of
   it. Nothing the browser sends, and no claim krm-foyer reads, decides this.
3. krm-foyer asks, as the shared-watch identity, whether that user may `list` and
   `watch` the scope (two SubjectAccessReviews). A denial wins over an allow; anything
   short of an allow is a refusal.
4. Only then is the stream served, from the scope's shared watch, which the first
   stream opened and the last stream out closes.
5. The question is asked again every `-shared-watch-recheck-interval` (30 seconds) and
   at every new snapshot. A no ends that user's stream with `FORBIDDEN`, and nobody
   else's.

A decision is reused for `-shared-watch-decision-ttl` (10 seconds) by the same user's
other streams of the same scope and by their rechecks: exactly the question the
reviews ask (the whole subject; the scope's group, version, resource, namespace and
name), nothing broader. The label selector is not part of that question, since RBAC
cannot grant by label, so streams that differ only in it share a decision. An error is
never reused.

### Revocation

A grant taken away ends a shared stream within:

| | Default | Flag |
| --- | --- | --- |
| The recheck interval | 30 seconds | `-shared-watch-recheck-interval` |
| plus the decision's lifetime | 10 seconds | `-shared-watch-decision-ttl` |
| plus one check | at most 10 seconds | |
| plus one write to the browser in progress | at most 10 seconds | `-stream-write-timeout` |
| **In all** | **60 seconds** | |

The last line is there because krm-stream delivers events and rechecks one at a time:
a recheck waits for the write in progress. Without a bound on writes, a browser that
stopped reading would hold its stream open, unchecked, until the 30-minute response
duration. With it, the write fails and the stream ends. Since 0.7.0 krm-stream refuses
timed rechecks without a write timeout, and its
[revocation budget](https://github.com/ConfigButler/krm-stream/blob/main/docs/auth.md#revocation-budget)
breaks down the same parts.

Logout and session expiry are not affected: they end every stream within the
session-check interval, as before. These access reviews use the subject captured by
the SelfSubjectReview when the stream opened. They do not refresh the OIDC token or
learn new issuer group membership; the RBAC bound is not an issuer-disablement bound.

### What the browser gets when something fails

| What happened | The browser receives |
| --- | --- |
| RBAC does not allow the user `list` and `watch` | `FORBIDDEN`, terminal, with the API server's reason |
| The API server does not take the user's token | `UNAUTHENTICATED`, terminal |
| The API server could not answer a review | `UPSTREAM_UNAVAILABLE`, not terminal, with its hint: the client retries |
| The API server refuses the shared-watch identity itself | `INTERNAL`, terminal, without the API server's message, which would name the identity. This is a configuration error: the log says `status_403` |

### Load on the API server

Per stream opened: one SelfSubjectReview. Then two SubjectAccessReviews per decision,
and decisions are made per **user and scope**, not per stream. While a user has
streams of a scope open, the API server is asked about that pair roughly:

- once per recheck interval, when all its streams recheck within one decision's
  lifetime of each other, as one page's do;
- up to once per decision lifetime, when its streams were opened far apart and recheck
  at scattered times.

So, **as a planning estimate,** the steady load is around `2 × pairs / recheck
interval` reviews a second, and up to `2 × pairs / decision lifetime`, where *pairs* is
the number of distinct (user, scope) combinations with a stream open. Exact counts
depend on timing: a decision's lifetime starts when its review finishes, so a recheck
can land just inside it (and reuse it) or just outside (and ask again), and a run can
come out below the first figure. The rehearsal, 200 users on one scope opening
together, measured 12.9 reviews a second ([the rehearsal](bounds.md#measured-the-rehearsal)).
The same 200 users on nine distinct scopes each are nine times as many pairs, so nine
times the load; reusing decisions cannot remove that, since each scope is its own
question.

What a review costs depends on the cluster's authorizers. With RBAC alone, it is an
in-memory evaluation, with no storage. With a webhook authorizer in the chain, it can be
a blocking HTTP call to that webhook, so the load lands there too, at the webhook's
latency; measure it with `krm_foyer_access_check_duration_seconds`. The shared-watch
identity's client sends at most `-shared-watch-qps` (100) requests a second, bursts
twice that (at least one request). Reviews, watch openings, fallback lists and retries
share one transport budget; SelfSubjectReviews use each user's separate client. The
API server's priority and fairness applies on top. Reviews that
cannot be sent in time show as `error` results and `UPSTREAM_UNAVAILABLE` streams,
never as streams served without a check.

How the two flags trade revocation time against load, as planning estimates for 200
users each on one scope (nine scopes each: multiply the load by nine):

| `-shared-watch-recheck-interval` / `-decision-ttl` | Revocation, at most | Reviews a second, together … scattered |
| --- | --- | --- |
| 30s / 10s (default) | 60 s | about 13 … up to 40 |
| 30s / 30s | 80 s | about 13 or fewer |
| 60s / 30s | 110 s | about 7 … up to 13 |
| 120s / 60s | 200 s | about 3 … up to 7 |

A lifetime as long as the interval caps the scattered case at the together figure, for
20 seconds more revocation time at the default interval. The defaults favour
revocation; relax them where the load matters more, and measure the result with
`krm_foyer_access_checks_total{source="api_server"}` rather than relying on the table.

#### Why not check only when something changes?

Kubernetes sends no notice that what a user may do has changed, so there is nothing to
wait for. One could watch RBAC's Roles and Bindings and recheck when they change, but:

- **RBAC is not all of authorization.** A webhook authorizer, the Node authorizer, or
  an external policy service can change an answer with no RBAC object to watch.
  Periodic reviews cover those changes for the captured subject. Issuer group changes
  also have no RBAC event, but require fresh identity resolution and a new token;
  repeating a SubjectAccessReview alone does not pick them up.
- **The shared-watch identity would read every Role and Binding** in the cluster: the
  whole access policy, a broader grant than it has now.
- **krm-stream rechecks on its own timer,** per stream; a host cannot ask it to
  recheck now.

The useful version is both: an RBAC watch that rechecks at once, making the common
revocation immediate, with a long periodic recheck (minutes) behind it, cutting the
load by as much. That needs krm-stream to let a host trigger rechecks, and is asked
for in [the shared-watch requests](investigations/krm-stream-shared-watches.md#ask-11-let-a-host-trigger-a-recheck).
Until then, the interval is the lever.

Each replica holds its own shared watches. With several replicas, a scope has at most
one watch per replica.

### Metrics

| Metric | What to watch |
| --- | --- |
| `krm_foyer_upstream_watches_open{identity="shared"}` | Watches the shared-watch identity holds open: one per scope in use. The bare name selects both series, `user` and `shared`; `sum()` them for the total |
| `krm_foyer_upstream_watches_open{identity="user"}` | Watches opened with a user's own token, one per per-user stream |
| `krm_foyer_shared_subscriptions_open` | Streams reading from a shared watch. Divided by the shared watches, the reuse |
| `krm_foyer_shared_overflows_total` | Streams that fell so far behind that they got a fresh snapshot from the cache instead |
| `krm_foyer_access_checks_total{source, result}` | Access decisions: from the API server or reused, allowed, denied or failed |
| `krm_foyer_access_check_duration_seconds` | How long a decision that asked the API server took. Near 10 seconds, rechecks start failing |
| `krm_foyer_subject_reviews_total{result}` | SelfSubjectReviews: resolved, refused, or failed |

How many streams one shared watch serves, as PromQL:

```text
krm_foyer_shared_subscriptions_open / ignoring(identity) krm_foyer_upstream_watches_open{identity="shared"}
```

Worth an alert: `krm_foyer_access_checks_total{result="error"}` rising (reviews failing:
streams are refused, which is safe, but users notice), and the check duration's p99
approaching 10 seconds.

### What sharing does not change

- **Writes** go through `/k8s` with the user's token, always. Nothing is ever written
  with the shared-watch identity.
- **Projections** are chosen per stream, as before.
- **Bounds** count each stream, shared or not: the stream limits per session and per
  replica bound the shared watches too, since a scope has a watch only while a stream
  reads it.

## Why sharing stays opt-in

The rehearsal turned 1800 watches into one, and it is tempting to share every
resource. krm-foyer does not, and does not plan to make it the default:

- **The identity would need everything.** Sharing every resource means one identity
  that may read everything any user may, and krm-foyer's reviews become the only thing
  between that identity's cache and each browser. Kept narrow, a mistake exposes one
  resource; kept broad, everything. krm-stream keeps sharing opt-in for the same
  reason.
- **The savings depend on overlap; the costs do not.** Sharing saves watches only where
  many streams watch the same scope. The reviews cost per user and scope wherever
  sharing is on, and on per-user scopes save nothing.
- **What every stream still costs stays.** Each stream still gets its own snapshot,
  connection and per-object work in krm-foyer and in the browser. The rehearsal's
  memory figures compare different snapshots, so they do not isolate what sharing
  saves there.
- **What is measured is consolidation.** The rehearsal shows many streams of one scope
  on one watch. It does not yet show varied scopes, streams opened at scattered times,
  sustained change, or the API server's CPU; measure those before sharing broadly.

Share the resources whose common scopes you know, with an identity narrowed to them:
in an application's own deployment, that can well be on by default.

## Native watches through `/k8s`

They work, they are bounded like any request, and they end with their session
([bounds](bounds.md#native-watches)). krm-foyer does not tell them apart from other
requests, measures nothing about them separately, and does not plan to: a page that
wants live state should use a stream. If one day a real application depends on native
watches at scale, measure them then.
