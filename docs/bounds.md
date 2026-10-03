# Bounds

What krm-foyer limits on its own, why, and what it deliberately leaves alone. The
[design](design.md#access) lists the bounds as requirements; this document explains each
choice, records what was left out and why, and says how to read the metrics that show
how close real traffic comes to each bound.

A *bound* is something krm-foyer limits, such as how long a response may stay open. Its
*limit* is the configured value, such as 30 minutes.

**Decisions (2026-10-02):**

- **Native watches stay open, and are not a special case.** Every bound applies to every
  request through `/k8s`, so krm-foyer never has to tell a watch from any other request.
  Live views in applications belong to krm-stream ([roadmap step 5](roadmap.md#order-of-work));
  the native watch stays available underneath it. See [native watches](#native-watches).
- **Five bounds**, each configurable with a documented default: the request rate per
  session, concurrent requests per session and per replica, how long a response may
  stay open, and the bytes of a response, counted decoded. On top of them, every open
  response ends when its session ends.
- **A response cut short is aborted, never ended cleanly**, so a part of an answer can
  never pass for the whole of it. See [cutting a response short](#cutting-a-response-short).
- **A response too large gets a 502 only when its size is known before it starts.**
  Otherwise the answer has begun, and it is cut short. See [response bytes](#response-bytes).
- **No bound on page size.** It would not reliably bound what a list costs. See
  [left out](#left-out-a-bound-on-page-size).
- **Streams are bounded on their own** (2026-10-02): how many a session, and a replica,
  may have open, counted apart from requests through `/k8s`. Opening one draws on the
  same request rate, and the response duration and the session check hold it like any
  other response. See [streams](#streams).
- **Metrics come from the Prometheus client**, on a listener of their own, labelled only
  by fixed sets of values. OpenTelemetry is kept for tracing, if that is ever added. See
  [metrics](#metrics).

## What a bound is for

krm-foyer is for applications that talk to Kubernetes from a browser, so the bounds are
shaped by what a browser does: what a page sends when it loads, what a tab keeps open,
and what goes wrong in page code. They protect krm-foyer and the API server from load
that no person generates: a retry loop, a leaked watch, a component that turns every
render into a request, a compressed answer that expands a thousandfold.

They are not three other things:

- **Not an access rule.** A bound never changes what a request may do, only how much and
  how long. [Kubernetes alone decides](design.md#access) what is allowed.
- **Not a limit on export.** A user allowed to list a namespace can retrieve all of it
  through repeated pages, and should be able to.
- **Not a defense against a determined user.** The per-session bounds are per session,
  and a new session costs one login. A signed-in user who wants to cause more load can
  open more sessions, or sign in at the issuer and call the API server directly. Load
  from a user is the API server's to limit: [API Priority and
  Fairness](https://kubernetes.io/docs/concepts/cluster-administration/flow-control/)
  charges every request to the user who made it, whichever client sent it.

**Every bound is per replica.** krm-foyer keeps no shared counters. With one replica, as
today, that is the whole deployment. Once there are several
([step 6](roadmap.md#order-of-work)), a session whose requests reach N replicas gets up
to N times its per-session limits, and the per-replica limits are what hold.

The per-replica bounds on concurrent requests are the ones that protect krm-foyer itself,
however many sessions there are. The byte bound limits one response, not the total:
krm-foyer streams, so a response costs it the same memory whatever its size, and the
number of responses in flight is what the concurrency bound caps.

## Native watches

A native watch is a `GET` that the API server answers with a stream of events. A browser
can read one: `fetch` exposes a response body as a stream, so page code can read events
as they arrive. The code must then do what every Kubernetes client does: split the
stream into events, reconnect when it ends, and resume from the last `resourceVersion`
without losing or repeating a change.

krm-stream does that work once, for every application. It is the path krm-foyer
recommends for live views ([step 5](roadmap.md#order-of-work)), and the hello example
moves to it. So the question was whether native watches through `/k8s` should stay at
all. **They stay**, for three reasons:

- **Refusing them would be an access rule.** RBAC already says who may watch what.
  krm-foyer refusing a verb that RBAC allows is exactly the second set of rules the
  design avoids. krm-foyer blocks as little as possible.
- **They work today**, against the real API server, and a spec proves it.
- **They cost almost nothing extra.** What they need, an end when the session ends and
  cancellation at the API server, every long response needs too: a followed log, or a
  slow list open at logout. krm-stream's user-authenticated watches will need the same.

What is not offered: a watch client in the [browser helper](frontend.md), or an example
that reads native watches. That would be a second, lesser krm-stream.

**krm-foyer does not tell watches apart.** It was going to, and two measurements against
the real API server (k3s in the e2e fixture, 2026-10-02) showed why it should not:

- **The response does not say.** A JSON watch is served as plain `application/json`;
  only the other encodings get `;stream=watch`. That is in Kubernetes'
  [watch handler](https://github.com/kubernetes/apiserver/blob/master/pkg/endpoints/handlers/watch.go).
- **The request has many spellings.** The API server takes the first `watch` value after
  Go's query parsing: anything but `0` or `false`, in any case, is a watch, including an
  empty value, `watch=yes` and a bare `watch`. `%77atch=1` is a watch, a pair with a
  semicolon is dropped, and the deprecated `/watch/` paths are watches too. A `GET` of
  one named object ignores `watch`. Matching all of that is a second parser of exactly
  the kind krm-foyer [avoids for paths](design.md#access).

So the bounds are written not to need it. Concurrency counts every request in flight,
the duration and the session check apply to every response, and a response cut short is
aborted whatever it is. A watch is simply a request that stays open.

krm-stream's streams are bounded on their own: see [streams](#streams).

## The bounds

| Bound | Flag | Default | When it is reached |
| --- | --- | --- | --- |
| Request rate per session | `-session-request-rate`, `-session-request-burst` | 20 a second, bursts of 100 | 429 `RequestRateExceeded` with `Retry-After` |
| Concurrent requests per session | `-max-session-concurrent-requests` | 64 | 429 `TooManyConcurrentRequests` |
| Concurrent requests per replica | `-max-concurrent-requests` | 2000 | 429 `TooManyConcurrentRequests` |
| Streams open per session | `-max-session-streams` | 32 | 429 `TooManyStreams` |
| Streams open per replica | `-max-streams` | 2000 | 429 `TooManyStreams` |
| Response duration | `-max-response-duration` | 30 minutes | The response is [cut short](#cutting-a-response-short) |
| Response bytes, decoded | `-max-response-bytes` | 32 MiB | 502 `ResponseTooLarge` if known in advance; otherwise [cut short](#cutting-a-response-short) |
| Session check | `-session-check-interval` | 5 seconds | An open response whose session has ended is [cut short](#the-session-check) |

Why these defaults:

- **20 requests a second, bursts of 100.** The burst covers a cold page load, discovery
  included, which can send dozens of requests at once. Twenty a second, sustained, is
  far above what a person clicking generates, and a tight loop meets the bound within
  seconds.
- **64 concurrent requests per session.** Over HTTP/1.1 a browser opens at most six
  connections to one host, so it never gets near this. Over HTTP/2, a cold page load can
  have dozens of requests in flight for a moment. Sixty-four leaves room for that next
  to a dozen open watches. A page that leaks watches meets the limit, and from then on
  every request of that session is refused, which makes the leak hard to miss.
- **2000 concurrent requests per replica.** Each one holds a connection or HTTP/2 stream
  to the browser and another to the API server. This is still a capacity hypothesis:
  [the rehearsal](#measured-the-rehearsal) measured streams, not native watches through
  `/k8s`, which pass through a lighter path. Measure them before relying on it.
- **32 streams per session.** A page shows a handful of live lists, and one session is
  every tab of the application in that browser. Thirty-two leaves room for several tabs
  with several streams each, and a page that leaks streams meets it.
- **2000 streams per replica, provisionally.** In [the rehearsal](#measured-the-rehearsal),
  1800 streams of one small note each took about 250 MiB of resident memory on one
  replica, so 2000 like them would take about 300 MiB. That holds for that workload only:
  a stream's cost grows with its snapshot, its objects and how often they change. Measure
  your own scopes before sizing a replica by it.
- **30 minutes per response.** Ordinary requests finish in seconds; the API server gives
  them 60 at most. What stays open is a watch or a followed log. Thirty minutes is the
  shortest time the API server itself keeps a watch open when the client names no
  `timeoutSeconds` (it picks between `--min-request-timeout`, 1800 seconds by default,
  and twice that). Clients that name a shorter `timeoutSeconds`, as client-go does
  (five to ten minutes), never meet this bound. The limit must be below the session's
  idle timeout, which krm-foyer checks at startup: a reconnect counts as use, so a tab
  that only watches stays signed in.
- **32 MiB per response.** That fits an unpaginated list of a few thousand ordinary
  objects. gzip expands at most about a thousandfold, so a compressed bomb is stopped
  after some 32 KiB of what the API server sent.
- **A session check every 5 seconds.** Logout and expiry cut open responses short within
  that time, or twice it at worst (see [the session check](#the-session-check)).

### How a bound answers

A refusal is an [interruption](design.md#interruptions), like every other answer
krm-foyer gives instead of the API server's. It carries the `Krm-Foyer-Interruption`
header, and its `Status` names the bound in `details.causes`: one cause with reason
`BoundReached`, the bound as the metrics name it in `field` (such as
`session_concurrent_requests`), and its limit in `message`.

That header is what tells krm-foyer's 429 from the API server's. The API server sends
429s of its own, from Priority and Fairness, and they pass through unchanged and without
the header, since the proxy passes no upstream header outside its allowlist.

The header says that krm-foyer answered. It does not, on its own, say that Kubernetes
never saw the request; that depends on the reason, and the
[interruptions table](design.md#interruptions) says it for each:

- **`RequestRateExceeded`, `TooManyConcurrentRequests` and `TooManyStreams`** are decided
  before anything is sent. The request did not reach Kubernetes, so code may send it again, a change
  included.
- **`ResponseTooLarge`** is decided on the API server's answer. The request reached
  Kubernetes and may have taken effect. Code may repeat a `GET`, but must never resend
  a change on this answer without finding out first whether it happened.

`Retry-After` comes only where waiting is known to help: for the request rate it is the
time until the session may send its next request, rounded up to whole seconds. A
concurrency slot frees when another request ends, and nobody knows when that will be, so
`TooManyConcurrentRequests` carries no `Retry-After` rather than a number made up.

### Cutting a response short

A response can end in five ways:

| Cause | What the browser sees |
| --- | --- |
| The API server ends it: the answer is complete, or a watch reached its `timeoutSeconds` | The end of the response |
| The browser goes away | Nothing; nobody is left |
| The response duration is reached | An aborted response |
| The response-byte bound is reached | An aborted response |
| Its session ends: logout, idle, absolute or token expiry, or a session store that cannot answer | An aborted response, within the [session check](#the-session-check) |

When krm-foyer cuts a response short, it does two things:

- **It cancels its request to the API server**, so the API server releases the watch, or
  stops the work, at once rather than at its own timeout.
- **It aborts the response to the browser.** Over HTTP/2 the stream is reset; over
  HTTP/1.1 the connection is closed before the final chunk. The browser sees a network
  error, never a clean end.

A clean end would be friendlier to a watch, but krm-foyer does not know which responses
are watches, and for anything else a clean end would make the first part of an answer
look like the whole of it. For a watch the abort makes no difference: Kubernetes clients
resume after a broken connection exactly as after an end, from the last
`resourceVersion` they saw. If the session has ended, the resumed request gets the 401
[interruption](design.md#interruptions), and the application signs in again.

Both steps must work while the response is blocked. A read from the API server is
unblocked by cancelling its request. A write to a browser that has stopped reading is
unblocked by a write deadline, so a stalled tab cannot keep a response open past its
bound.

### The session check

Every open response asks, once per interval, whether its session is still live. The
question reads the session without counting as use: only requests renew the idle timeout,
so a reconnecting watch does and an open one does not.

The check is given at most one interval to answer. A store that does not answer in time,
or answers with an error, counts as a session that has ended: fail closed, as the
[session lifecycle](design.md#session-lifecycle) requires. So a response is cut short
within one interval of its session ending, plus at most one more for a check that hangs:
5 seconds normally, 10 at worst, with the defaults.

Each open response reads the store once per interval: nothing for the memory store, and
some 400 reads a second at the per-replica limit of 2000 once the store is shared
([step 6](roadmap.md#order-of-work)).

### Response bytes

The bound counts decoded bytes. krm-foyer asks the API server for gzip and
[decodes it](design.md#upstream-responses), so the bound applies to what the browser
receives, and a small compressed body cannot expand past it.

The design first said a response over the bound gets a 502. That can only be true before
the response starts, and usually it has started already. krm-foyer streams: it passes on
the status and headers as soon as they arrive, then the body as it comes. The API server
compresses large answers, so their decoded size is unknown until every byte has been
counted, and by then the 200 is on its way to the browser.

The exact rule, for a limit of *L* bytes:

- **A response of up to *L* decoded bytes passes**, whether its length was known in
  advance or not.
- **Known in advance:** a response whose `Content-Length` is more than *L* gets a 502
  `ResponseTooLarge` before any of it is sent.
- **Counted as it streams:** when byte *L* + 1 arrives, it is not forwarded, and the
  response is [cut short](#cutting-a-response-short).
- **A `HEAD` response is never too large:** it has no body, and its `Content-Length`
  describes the `GET`.

The tests send *L* − 1, *L* and *L* + 1 bytes, both with a known length and gzip-encoded.

Other ways to keep the 502, and why not:

- **Buffer the response until it is complete or over the limit.** Then the status could
  still change. But memory becomes the limit times the requests in flight, 32 MiB a
  hundred times over is more than 3 GiB, and a bound meant to protect krm-foyer would
  become the easiest way to exhaust it.
- **Ask the API server not to compress.** Large lists would then cross the network
  several times larger, and a chunked response still has no length in advance.
- **Report the overflow in a trailer.** `fetch` does not expose trailers, so no browser
  code would see it.

Revisit when the held-back page of the [interruptions](design.md#interruptions) table
learns to show a response as escaped text. It needs a small buffer for that page anyway,
and a 502 for small bodies could share it.

## Streams

A stream on `/stream/v1` is krm-stream's: a snapshot of a resource, then its changes, for
as long as the page keeps it open ([design](design.md#streams-and-editing)). krm-foyer
counts streams apart from requests, and counts the watches they hold at the API server
apart again:

- **Streams have limits of their own,** per session and per replica. A page that keeps a
  few streams open should not lose its room for ordinary requests, and a request
  should not be refused because live views are open. Past a limit, a stream is a 429
  `TooManyStreams` interruption, and krm-stream's browser client tries again later.
- **Opening a stream is a request** for the request rate: one budget per session for
  `/k8s` and `/stream`, so a page that reconnects in a loop meets the rate bound. A
  stream that fails in a way that may pass closes, and krm-stream's browser client opens
  it again after a wait, so its retries are requests too, and meet the same bound.
- **The response duration and the session check** hold a stream as they hold every
  response: it is cut short at 30 minutes, and krm-stream's client opens it again with a
  fresh snapshot, which renews the session's idle timeout; and it is cut short within
  the session-check interval of its session ending.
- **The response-byte bound does not apply.** It bounds one answer, and a stream is not
  one: it is many snapshots and changes over half an hour. The size of a snapshot is
  the size of a list, which the API server answers the same way through `/k8s`.
- **Upstream watches are counted, not bounded.** A per-user stream holds one watch at the
  API server, opened as its user; a [shared](watches.md) stream holds none of its own,
  and its scope's shared watch stays open only while some stream reads it. So there are
  never more upstream watches than streams, and the stream limits bound them. A bound of
  their own was planned for shared watches, and left out (2026-10-03) for that reason:
  it could only be reached by first reaching a stream limit. They are counted apart, by
  whose identity opened them (`krm_foyer_upstream_watches_open{identity}`), because with
  sharing the two counts differ: the reuse is their ratio.

What is not bounded yet: a tab that stops reading. A native watch through `/k8s` holds
its slot then until the response duration ends it, and a stream does the same.
krm-stream can bound each write instead (`WriteTimeout`), which would free the slot
within seconds; that needs its own test, of a tab that stops reading while a stream
fills the buffers between, and comes later.

## Measured: the rehearsal

The rehearsal ([rehearsal_test.go](../test/e2e/rehearsal_test.go)) is part of the e2e
suite and runs with every `task verify`. It restarts krm-foyer, signs in 200 identities
through Dex's login form, opens nine streams for each, 1800 on one replica, near the
2000 it allows by default, and changes one note they all watch. Then every identity
signs out. It fails unless every stream had the change within 10 seconds, every stream
was aborted within the session-check interval of its logout, and the streams, the
watches at the API server and the goroutines all went back to where they were. It
prints what it measured with its report.

Three runs on 2026-10-02, against the e2e fixture (k3s 1.36.4 in k3d, krm-stream 0.4.0,
one replica, the suite and the cluster on one devcontainer host):

| Measured | Result |
| --- | --- |
| Opening 1800 streams at once, until every snapshot was complete | 0.7 to 1.1 seconds |
| One change reaching all 1800, from `kubectl patch` starting | under 110 ms, the slowest; the spread from first to slowest under 50 ms |
| Logout to stream aborted, the slowest of 1800 | 4.1 to 4.5 seconds, within the 5-second check |
| Goroutines per open stream | 6 |
| Resident memory | 30 MiB idle, 34 MiB with 200 sessions, 235 to 267 MiB with 1800 streams: about 110 to 130 KiB per stream, for this workload |
| Watches at the API server | one per stream, 1800, all released at logout |

One run each on 2026-10-03, after shared watches came (krm-stream 0.6.0, otherwise the
same fixture), now that the rehearsal runs twice: streams of ConfigMaps, each a watch
of its user's own, and streams of shared notes, all on one watch:

| Measured | Per-user (ConfigMaps) | Shared (notes) |
| --- | --- | --- |
| Opening 1800 streams at once, until every snapshot was complete | 0.6 seconds | 2.2 seconds: a SelfSubjectReview for each stream, and the access checks, come first |
| One change reaching all 1800, the slowest | 84 ms | 47 ms |
| Logout to stream aborted, the slowest | 4.4 seconds | 1.6 seconds |
| Goroutines per open stream | 6 | 5 |
| Resident memory with 1800 streams | 326 MiB, about 164 KiB per stream | 220 MiB, about 103 KiB per stream |
| Watches at the API server | 1800 | 1 |
| Access checks while opening | none | 200 asked the API server (two SubjectAccessReviews each), 1600 reused |
| Access checks while held for 31 seconds | none | 200 asked, 12.9 reviews a second; 1600 reused |

With sharing, the API server holds one watch instead of 1800, and is asked about each
identity once per recheck rather than about each stream: 12.9 reviews a second for 200
identities at the 30-second default, the rate krm-stream's own estimate gives. The
ConfigMap snapshot holds two objects (the namespace's `kube-root-ca.crt` beside the
one created), the note snapshot one, so the memory columns are not a like-for-like
comparison of the two paths.

Every stream watches the same scope: one small object. So the memory figures describe
that workload, not a stream in general. krm-stream keeps per-object bookkeeping for each
stream, and its list-then-watch fallback holds a whole snapshot while it lists, so
larger scopes, larger objects and frequent changes cost more, possibly much more.
Resident memory is measured in a fresh process, where it only grows over a run this
short; the garbage collector made smaller measurements too noisy to use. What the
rehearsal does not cover: representative scopes and objects, sustained change, native
watches through `/k8s`, many replicas, many scopes per shared resource, a slow or
distant API server, and the browser's side. A real cluster's
numbers belong next to these when someone measures them.

## Left out: a bound on page size

A Kubernetes list takes `limit`, the most items to return in one page; the API server
then answers with a `continue` token for the next page. A bound on page size would refuse
a `limit` above some maximum. It was in the design and the roadmap until 2026-10-02.

It would not reliably bound what a list costs:

- **Only a `limit` the client sends can be checked.** A list without one returns every
  item, so a client that wants everything at once leaves `limit` out, and the bound
  never applies.
- **Adding a `limit` where there is none would break clients silently.** A client that
  sends no `limit` does not expect a `continue` token; given a partial list, it shows a
  partial list, and nobody finds out. Rewriting the request also breaks the rule that
  krm-foyer passes requests on as they came, or answers itself with an
  [interruption](design.md#interruptions).
- **What a large list costs krm-foyer and the browser is its bytes**, and the
  response-byte bound holds those, with or without `limit`.
- **The API server already charges for it.** Priority and Fairness estimates the work of
  a list from its `limit` and the number of objects, and charges it to the user.

Telling a list from a single object, so as to require a `limit` on lists, would mean
parsing resource paths the way the API server does: a second parser, which krm-foyer
avoids everywhere else.

Revisit when an adopter shows a list that the byte bound lets through and that hurts, or
when an [application scope](application-scope.md) brings resource-path parsing for
reasons of its own. Built then, it should refuse a `limit` above the maximum with a 400,
not a 429: waiting does not make the request smaller.

## Metrics

**Decision (2026-10-02): the Prometheus client, scraped.** krm-foyer records its metrics
with the Prometheus Go client and serves them on a listener of its own
(`-metrics-listen`, `:9090` by default, `/metrics` only). The metrics are never served on
the origin, where any page could read them. **Keeping the port to the monitoring system
is the deployment's job:** a NetworkPolicy that admits only the monitoring system to
it. The Helm chart will ship one ([roadmap](roadmap.md#deployment)); until then, a
deployment adds its own. The e2e fixture has none, and reaches the port as admin
through the API server.

The other candidate was the OpenTelemetry metrics API, exported through OpenTelemetry's
Prometheus exporter, as gitops-reverser does. It instruments the same way and scrapes the
same way. It was not chosen:

- **It is the Prometheus client plus a layer.** The exporter is built on the Prometheus
  client, so OpenTelemetry comes on top of it: 21 modules in the build, against 10 for
  the client alone, for the same scrape.
- **Its names are translated.** The exporter turns dots into underscores, appends unit
  suffixes and `_total`, and adds scope labels and a `target_info` series unless told
  not to. With the client, the name in the code is the name in the scrape, and in the
  alerts people build on it.
- **What it would buy is not needed.** Its one advantage is export to more than one
  backend, and a scrape already reaches them all: an OpenTelemetry Collector scrapes
  `/metrics` with its Prometheus receiver. Nobody has to run a collector for krm-foyer,
  and a collector that is down never touches a request.

What is left out on purpose:

- **No automatic HTTP instrumentation.** Middleware of that kind labels requests by
  route, and under `/k8s` the path names namespaces and objects: unbounded label values,
  and a record of what each user looks at, kept outside the audit log. krm-foyer's
  metrics are labelled only by bound, cause and interruption reason, fixed sets known in
  advance. Details of a single request belong in its log line.
- **No tracing yet.** Tracing is where OpenTelemetry is the right choice, and it would
  come with its tracing SDK only, not its metrics. But the API server accepts a W3C
  `traceparent`, and forwarding the browser's would let a page write into the cluster's
  traces. That changes the request header allowlist, and needs a decision of its own.
- **Names, labels and buckets are an interface.** People build alerts on them, so a test
  reads `/metrics` and fails when one changes, and changing one is a breaking change in
  the changelog. Go runtime and process metrics (`go_goroutines` shows a leaked
  response) come with the client.

### What is measured

Names as scraped:

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `krm_foyer_bound_limit` | gauge | `bound` | The configured limit of each bound, in its own unit (below) |
| `krm_foyer_bound_usage_ratio` | histogram | `bound` | How much of its bound each request or response let through used, from 0 to 1 |
| `krm_foyer_bound_reached_total` | counter | `bound` | Requests refused, and responses cut short, because a bound was reached |
| `krm_foyer_requests_in_flight` | gauge | | Requests through `/k8s` open now, watches included |
| `krm_foyer_streams_open` | gauge | | Streams through `/stream` open now |
| `krm_foyer_upstream_watches_open` | gauge | `identity` | Watches open at the API server now: `user`, one per per-user stream, or `shared`, one per scope in use by [shared](watches.md) streams |
| `krm_foyer_shared_subscriptions_open` | gauge | | Streams reading from a shared watch now |
| `krm_foyer_shared_overflows_total` | counter | | Streams that fell behind their shared watch and got a fresh snapshot from its cache |
| `krm_foyer_access_checks_total` | counter | `source`, `result` | Access decisions for shared streams: `api_server` or `cache`, and `allowed`, `denied` or `error` |
| `krm_foyer_access_check_duration_seconds` | histogram | | How long a decision that asked the API server took (two SubjectAccessReviews) |
| `krm_foyer_subject_reviews_total` | counter | `result` | SelfSubjectReviews before a shared stream: `resolved`, `refused` or `error` |
| `krm_foyer_responses_cut_short_total` | counter | `cause` | Responses krm-foyer cut short, by why |
| `krm_foyer_interruptions_total` | counter | `reason` | Answers krm-foyer gave instead of the API server's, by the reason in the `Krm-Foyer-Interruption` header |

The `bound` label, and when usage is measured:

| `bound` | Limit, in | Usage is measured | as |
| --- | --- | --- | --- |
| `session_request_rate` | requests a second | (limit only: the rate the burst refills at) | |
| `session_request_burst` | requests | at every request let through | the share of the session's burst spent, this request included |
| `session_concurrent_requests` | requests | at every request let through, as it starts | the session's requests in flight, this one included, over the limit |
| `concurrent_requests` | requests | at every request let through, as it starts | the replica's requests in flight, this one included, over the limit |
| `session_streams` | streams | at every stream let through, as it opens | the session's streams open, this one included, over the limit |
| `streams` | streams | at every stream let through, as it opens | the replica's streams open, this one included, over the limit |
| `response_duration` | seconds | when a response ends | how long it was open, over the limit |
| `response_bytes` | bytes | when a response ends | its decoded bytes, over the limit |

A refusal for the request rate is counted under `session_request_burst`: a request is
refused when the session's burst is spent, and the burst refills at the rate.

The `cause` label of `krm_foyer_responses_cut_short_total` is `response_duration`,
`response_bytes` or `session_ended`, the rows of
[cutting a response short](#cutting-a-response-short) that are krm-foyer's doing. The
first two are also counted in `krm_foyer_bound_reached_total`; this counter puts them
next to the one cause that is not a bound.

The usage ratio is the point of these metrics. It shows how close real traffic comes to
each bound, in one unit for all of them, and it stays meaningful when a limit changes.
Multiply by `krm_foyer_bound_limit` for absolute numbers. The buckets are 0.1, 0.25, 0.5,
0.75, 0.9 and 1.

A value of 1 means the whole bound was used, not that anything was refused: the last
concurrency slot taken, the last of the burst spent, or a response cut short at its
limit. Refusals are not observed in the ratio at all. Only
`krm_foyer_bound_reached_total` says that a bound refused a request or cut a response
short.

### Reading them

Which bounds were reached today, and how often:

```promql
sum by (bound) (increase(krm_foyer_bound_reached_total[1d]))
```

The 99th percentile, over every request let through, of how much of its session's
concurrency limit was in use when it started. It describes requests, not sessions: it
does not single out the busiest session, or give any session's peak.

```promql
histogram_quantile(0.99,
  sum by (le) (rate(krm_foyer_bound_usage_ratio_bucket{bound="session_concurrent_requests"}[1d])))
```

The share of responses that used more than 90% of the byte bound:

```promql
1 - sum(rate(krm_foyer_bound_usage_ratio_bucket{bound="response_bytes", le="0.9"}[1d]))
  / sum(rate(krm_foyer_bound_usage_ratio_count{bound="response_bytes"}[1d]))
```

Why krm-foyer cuts responses short:

```promql
sum by (cause) (rate(krm_foyer_responses_cut_short_total[1h]))
```

Rules of thumb for tuning:

- **Raise a limit when real users reach it.** Look at the bound's refusals together with
  its top bucket: a busy 0.9 to 1 bucket with refusals means the limit is in the way of
  ordinary use.
- **Lower a limit when the 99th percentile stays far below it** for weeks: the room
  above it serves only runaway pages.
- **Expect `session_ended` at logout and at expiry**, one for each response the tab had
  open. Idle expiry cannot cause it, since reconnects renew the session; the absolute
  timeout and the token's expiry can. Far more of them than sessions ending means
  something else: a session store that does not answer the check in time.
