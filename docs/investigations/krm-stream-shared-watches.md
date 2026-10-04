# krm-stream feedback from krm-foyer: shared watches

**Historical findings with two deferred requests.** Asks 9, 10 and the documentation
part of 13 were addressed in krm-stream 0.7.0, which krm-foyer now uses. Asks 11 and 12
remain deferred. Current setup and guarantees are in [watches](../watches.md).

The original investigation checked krm-stream `5ca19ed` (gateway 0.6.0) on 2026-10-04,
by reading the source and reproducing the failures in krm-foyer tests. The problem
and workaround descriptions below are that historical record; in particular, the
lazy-open workaround under ask 9 has been removed.

## Status (2026-10-04): asks 9, 10 and 13 are in krm-stream 0.7.0

krm-stream took ask 9, the "require a write timeout" form of ask 10, and the documentation
part of ask 13 ([its proposal 0008](https://github.com/ConfigButler/krm-stream/blob/main/docs/proposals/0008-shared-watch-hardening.md)),
and krm-foyer moved to 0.7.0:

- **Ask 9:** krm-foyer's upstream backend opens the shared watch in `Watch` again; the
  workaround that opened it on the first `Next` is gone. The test that reproduced the
  problem stays, and fails against 0.6.0.
- **Ask 10:** nothing changed in krm-foyer, which always set a write timeout. A test now
  checks that every route in front of a stream lets it flush and set write deadlines.
- **Ask 13:** the decision cache's key cites krm-stream's documented review attributes; the
  test that fails if a review starts carrying a selector stays, since the key is
  krm-foyer's to get right.

Asks 11 and 12 are deferred, with what would reopen them recorded in the proposal.
krm-foyer keeps rechecking on the timer and reusing decisions in its own authorizer.

## The asks at a glance

| # | Ask | Status | Current integration |
| --- | --- | --- | --- |
| 9 | Open a shared watch without holding the backend-wide lock, and cancel an opening nobody waits for | Fixed in 0.7.0 | Opens normally in `Watch`; the regression test remains |
| 10 | Make timed rechecks independent of a blocked write, or require a write timeout with them | Fixed in 0.7.0 | Sets `WriteTimeout` on every stream, and counts it in its revocation bound |
| 11 | Let a host trigger a recheck | Deferred | Rechecks on the timer only |
| 12 | Recheck once per principal and scope, not once per subscriber | Deferred | Reuses decisions for a short time in its own authorizer |
| 13 | Say which attributes the SubjectAccessReview asks about | Documented in 0.7.0 | Keys its decisions on them, with a test that fails if they change |

## Ask 9: open a shared watch without holding the backend-wide lock

**The problem.** `SharedBackend.Watch` holds `b.mu`, the lock every scope takes, while
`startScope` calls `b.upstream.Watch`, a network round trip to the API server. The
context it passes is `context.WithCancel(context.Background())`, and nothing cancels it
until the scope exists. So one API server slow to answer one opening:

- **holds up every other scope:** a stream of an unrelated namespace waits for the lock,
  and never reaches the API server;
- **cannot be abandoned:** the browser that asked for it leaving cancels its own request,
  but not the opening, so its stream slot stays taken, and so does every other waiting
  stream's.

krm-foyer reproduced both: an API server that never answers a watch of one namespace,
not even with headers, held up a stream of another namespace until the test timed out,
and the first stream's slot stayed taken after its browser left. A client-go watch has
no response-header timeout of its own, so the wait is unbounded.

**What we suggest.** Open the upstream outside `b.mu`: record an in-flight entry for the
scope under the lock, so later subscribers of the same scope wait on it, then open
without the lock. Give the opening a context that its waiting subscribers can cancel
together: when the last one leaves before the watch opened, cancel the opening and
drop the entry. The backoff bookkeeping can stay under the lock.

**Historical workaround (removed in 0.7.0).** Its upstream backend's `Watch` returns a watcher at
once, and opens the real watch on the first `Next`, with the context `Watch` was given.
`pump` calls `Next` without the lock, and `leave` cancels that context when the last
subscriber goes, so the opening is the scope's alone and ends with it. An opening
failure arrives through `Next` and `die`, which already records an
`UPSTREAM_UNAVAILABLE` for the backoff. This works, but it relies on the order of calls
inside `SharedBackend`, which is not a contract.

## Ask 10: keep timed rechecks from waiting on a blocked write

**The problem.** Timed checks and delivery share one gate per subscriber (docs/auth.md:
"Timed checks run per subscriber and pause that subscriber's object delivery"). A write
to a browser that stopped reading blocks once the buffers between are full, and the
recheck waits behind it. `ReauthorizationTimeout` does not cover that wait. With
`WriteTimeout` zero, the default, the write blocks until something else ends the
request, so a revoked grant is never applied to that stream. krm-foyer reproduced it: a
stream whose reader stopped, its buffers filled, its grant revoked, stayed open with no
further reviews, past a bound of about 10 seconds.

docs/auth.md does say the bound "assumes ... sinks do not block indefinitely", and the
sharedstream example sets `WriteTimeout`. But nothing ties the two together, and a host
that sets `ReauthorizationInterval` alone gets an interval that does not hold.

**What we suggest,** either of:

- **Refuse the combination:** `Handler` panics on `ReauthorizationInterval > 0` with
  `WriteTimeout == 0`, as it does for other unsafe options, or defaults `WriteTimeout`
  then; or
- **Decouple them:** run timed checks apart from delivery, and on a denial end the
  request (close the connection), whatever the write in progress is doing.

Either way, document the whole revocation budget in one place: interval, plus the check
timeout, plus one write timeout, plus cleanup, and whatever caching the host adds.

**What krm-foyer does meanwhile.** It sets `WriteTimeout` (10 seconds, configurable) on
every stream, shared or not, and its documented bound adds it in. A test stops reading,
fills the buffers, revokes the grant, and sees the stream end within the bound.

## Ask 11: let a host trigger a recheck

**The problem.** Rechecks are timed, per subscriber, and only the interval can be
tuned. A shorter interval revokes sooner and costs more reviews; a longer one the
reverse. A host that learns of a change sooner (it watches RBAC's Roles and Bindings, a
webhook authorizer's policy, or its own session store) has no way to apply it before
the next tick.

**What we suggest.** An API on the gateway to recheck now, for every subscriber or for
those matching a principal or scope, for example `Gateway.Reauthorize(func(Principal,
Scope) bool)`, through the same path as a timed check, so a denial ends only those
streams. A host could then pair an event-driven recheck (immediate for RBAC changes)
with a long periodic one as a safety net for what it cannot watch, such as group
membership from the issuer. For krm-foyer that would make the common revocation
immediate and cut the periodic review load several times over.

**What krm-foyer does meanwhile.** Rechecks on the timer; its decision guide explains
the trade-off between the interval and the load ([watches](../watches.md#load-on-the-api-server)).

## Ask 12: recheck once per principal and scope

**The problem.** Each subscriber has its own recheck timer. A user with nine tabs on
**one** scope is nine timers, each asking the same two questions, and the timers drift
apart as the tabs open at different times. Without reuse, the reviews scale with
streams rather than with users and scopes: 200 users with nine tabs each on one scope
cost about 120 reviews a second at a 30-second interval, where 13 would do.

That multiplier is what this ask removes. It is different from nine **different**
scopes: 200 users each on nine scopes are nine questions per user, about 120 reviews a
second however checks are coalesced. Only a longer interval cuts that, which ask 11
would make safe.

**What we suggest,** either of:

- one timed check per (principal, scope), whose answer applies to every subscriber of
  that pair; or
- a decision cache in `kube`, keyed on exactly the attributes a review sends, that a
  host can wrap `SubjectAccessReviewAuthorizer` with, with a lifetime the host chooses
  and counted in the revocation budget. Every shared host needs one, and getting the
  key wrong is a disclosure, so it belongs next to the authorizer.

**What krm-foyer does meanwhile.** Its own authorizer wraps the SubjectAccessReview
authorizer with such a cache: a decision is reused for 10 seconds for exactly the same
subject and asked attributes, two checks at once wait for one answer, and an error is
never kept. A fuzz test checks that the cache never changes an answer. Reuse still
depends on the timers falling within one lifetime of each other, which ask 12's first
form would make unnecessary; and as a lifetime starts when its review finishes,
whether a timer lands inside it is a matter of milliseconds, so the counts can only be
planned roughly.

## Ask 13: say which attributes the review asks about

**The problem.** A host that caches decisions has to know exactly which parts of a scope
the review asks about. Today `review` sends the group, version, resource, namespace and
name, and not the label selector (Kubernetes' `ResourceAttributes` has a `LabelSelector`
since 1.31, unused here). That is reasonable, since RBAC cannot grant by label, but it is
written down nowhere, and a later version that sends selectors would make a cache keyed
without them wrong.

**What we suggest.** Document the attributes on `SubjectAccessReviewAuthorizer`, and
mention a change to them in the changelog as a breaking change for hosts that cache. An
exported function returning the `ResourceAttributes` for a scope and verb would let a
host key on exactly what is asked.

**What krm-foyer does meanwhile.** Keys on those five attributes and the subject, and a
test fails if a review it observes carries a selector.

## What we did not ask for

- **A response-header timeout in the Kubernetes backend.** Ask 9 is the real fix; with
  it, a slow opening costs only its own scope's streams, which their browsers can
  abandon.
- **Sharing by default.** We agree with keeping `SharedBackend` opt-in: an identity that
  may read everything, and the review load, are not costs every host should carry
  unasked. krm-foyer keeps it opt-in too ([why](../watches.md#why-sharing-stays-opt-in)).
