# Watches: which kind to use

A page that shows live Kubernetes state can get it three ways through krm-foyer. This
page helps you choose one, then explains how to set up the one that needs setting up:
shared watches. In short: **use a stream for anything live in a page, share it when
many users watch the same thing, and treat native watches as a fallback.**

## The three kinds

| | Per-user stream | Shared stream | Native watch through `/k8s` |
| --- | --- | --- | --- |
| How the page opens it | `/stream/v1`, krm-stream's client | `/stream/v1`, krm-stream's client: the page cannot tell the two apart | `GET /k8s/...?watch=1`, read with `fetch` |
| Who opens the watch at the API server | The user, with their own token | krm-foyer's shared-watch identity, once per scope | The user, with their own token |
| Watches at the API server for N streams of one scope | N | 1 | N |
| Who decides what the user sees | The API server, on the user's own request | The API server, by a SubjectAccessReview about the user, at opening and every recheck | The API server, on the user's own request |
| When RBAC changes | The open watch carries on until it ends (Kubernetes does not end watches on RBAC changes) | The stream ends at the next recheck: 30 seconds by default, plus the decision's lifetime | As per-user stream |
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
- `create` on `subjectaccessreviews`, which the built-in `system:auth-delegator`
  ClusterRole grants;
- nothing else.

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
other streams of the same scope and by their rechecks: exactly the same question (the
whole subject and the whole scope), nothing broader. An error is never reused.

**Revocation bound:** a grant taken away ends a shared stream within the recheck
interval, plus the decision's lifetime, plus one check (at most 10 seconds): 50 seconds
at the defaults. Logout and session expiry are not affected: they end every stream
within the session-check interval, as before.

### What the browser gets when something fails

| What happened | The browser receives |
| --- | --- |
| RBAC does not allow the user `list` and `watch` | `FORBIDDEN`, terminal, with the API server's reason |
| The API server does not take the user's token | `UNAUTHENTICATED`, terminal |
| The API server could not answer a review | `UPSTREAM_UNAVAILABLE`, not terminal, with its hint: the client retries |
| The API server refuses the shared-watch identity itself | `INTERNAL`, terminal, without the API server's message, which would name the identity. This is a configuration error: the log says `status_403` |

### Load on the API server

Per stream opened: one SelfSubjectReview. Per user and scope: two SubjectAccessReviews
at opening, and two per recheck interval while any of their streams is open, fewer
when decisions are reused. The shared-watch identity's client allows 100 requests a
second, bursts of 200; the API server's priority and fairness applies on top. A
large cluster that rejects reviews shows it as `error` results and `UPSTREAM_UNAVAILABLE`
streams, not as streams served without a check. On one replica with 200 identities and 1800 streams of
one scope, that was one watch at the API server, 200 decisions at opening (1600 reused),
and 12.9 SubjectAccessReviews a second while the streams stayed open
([the rehearsal](bounds.md#measured-the-rehearsal)).

Each replica holds its own shared watches. With several replicas, a scope has at most
one watch per replica.

### Metrics

| Metric | What to watch |
| --- | --- |
| `krm_foyer_upstream_watches_open{identity="shared"}` | Watches the shared-watch identity holds open: one per scope in use |
| `krm_foyer_upstream_watches_open{identity="user"}` | Watches opened with a user's own token, one per per-user stream |
| `krm_foyer_shared_subscriptions_open` | Streams reading from a shared watch. Divided by the shared watches, the reuse |
| `krm_foyer_shared_overflows_total` | Streams that fell so far behind that they got a fresh snapshot from the cache instead |
| `krm_foyer_access_checks_total{source, result}` | Access decisions: from the API server or reused, allowed, denied or failed |
| `krm_foyer_access_check_duration_seconds` | How long a decision that asked the API server took. Near 10 seconds, rechecks start failing |
| `krm_foyer_subject_reviews_total{result}` | SelfSubjectReviews: resolved, refused, or failed |

How many streams one shared watch serves, as PromQL:

```text
krm_foyer_shared_subscriptions_open / krm_foyer_upstream_watches_open{identity="shared"}
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

## Native watches through `/k8s`

They work, they are bounded like any request, and they end with their session
([bounds](bounds.md#native-watches)). krm-foyer does not tell them apart from other
requests, measures nothing about them separately, and does not plan to: a page that
wants live state should use a stream. If one day a real application depends on native
watches at scale, measure them then.
