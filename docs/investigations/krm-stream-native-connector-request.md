# Requests: native watches, projected views and save progress

**Draft for the krm-stream team, 2026-10-05. Not submitted upstream.**

Suggested issue title: **feat(client): add a native Kubernetes watch connector with
the shared frontend lifecycle**

The [save-progress follow-up](#follow-up-3-reduce-save-interruptions-from-suppressed-updates)
below is a separate request to evaluate an existing deferred item. It can proceed
independently of the native connector.

## Problem and direction

krm-foyer already provides cookie-authenticated native watches through `/k8s` and
hosts krm-stream's projected streams on `/stream/v1`. A page that wants native
objects currently has to supply its own list/watch recovery. We want that page to
reuse krm-stream's live-resource and editing primitives, while pages needing
projection, redaction, suppression or watch sharing keep the gateway path.

Keep the reference Go gateway in krm-stream. krm-foyer is the integration point for
login, sessions, native API access, gateway hosting and bounds. Keep the gateway's
SSE encoding; replacing it is not needed to support native watches.

This expands [ask 7 of the earlier review](krm-stream-pr-53-55-review.md#7-follow-up-experiment-a-native-watch-connector).
It is a follow-up to the connector/store separation in PR #53, not an additional
requirement for merging that PR. As of this request, #53–#55 are open; foyer pins
0.7.0. Select a proposal number upstream rather than reserving one here.

## What exists, and what is missing

Verified against foyer's pinned gateway 0.7.0 (`project.go`, `stream.go`, `event.go`)
and the [implemented view design](https://github.com/ConfigButler/krm-stream/blob/a8281c58ac6adcb0b59a4f66ac87bc21828d1e08/docs/proposals/0004-views-and-bytes.md):

- Scope selects resource, namespace, name and optional labels.
- `krm-full/v1` omits Secret values, with paths and change revisions in `redacted`.
- `krm-spec/v1` also omits status and suppresses status-only changes.
- `krm-raw/v1` retains Secret values but still strips `managedFields` and last-applied
  configuration. It is not native passthrough.
- Suppression compares projected content excluding RV plus redaction records. A
  hidden Secret rotation still emits an update. Complete snapshots retain all members.

These features are valuable today. The missing pieces are a supported native
connector and a frontend example that makes the choices and their costs clear.

## Deliverable 1: a native connector

Provide a browser connector for a native Kubernetes collection URL through a host
proxy, with a caller-supplied fetch compatible with same-origin cookies. It must not
require Node polyfills, browser Kubernetes tokens, new foyer routes or service-account
credentials. The host continues to own login and routing.

Reuse the connector lifecycle introduced by #53: observable connection state,
subscription/disposal, completion, bounded retries and terminal errors. Feed the
store/editor through its event interface; do not create a second store or convert
native frames into SSE just to parse them again. Keep these distinctions explicit:

- Initial synchronization, live state, stale state during reconnect, access refusal
  and explicit closure are observable to the page.
- A transport checkpoint is separate from an object's edit precondition. Native
  objects retain their fields and versions; resource versions are opaque cursors.
- Raw objects have no gateway redaction revisions. Do not invent a projection name
  or infer missing fields are redacted. Specify how the store/editor represents the
  native view, including machinery fields that should not become accidental edits.
- The application chooses native or gateway access explicitly. Never retry a refused
  or unavailable projected stream through a native route. Changing the source/view
  must not merge incompatible snapshot, draft or redaction state silently.
- Save receipts, watch observations and later local typing retain the existing
  reconciliation guards. A no-op or superseded save cannot wait forever for an exact
  watch echo. Dirty drafts, in-flight writes and domain progress are separate concepts.

Start with a proposal and executable example, then expose the connector when the
contract and recovery tests pass. API names are for the krm-stream team to choose.

### Recovery acceptance

| Scenario | Required result |
| --- | --- |
| Initial streaming list, including an empty collection | `reset`, initial members, then `synced` only at the initial-end bookmark |
| EOF or read failure before that bookmark | Restart initialization; never resume using an individual initial object's RV as proof of a complete collection |
| Ordinary disconnect after initialization | Resume from the last applied event or bookmark, including progress before a thrown body-read error |
| HTTP or in-stream 410 | Start a fresh snapshot; retain old state as stale until completion, then prune missed deletes |
| API explicitly lacks streaming-list support | Complete a list, including pagination, then watch from the list's RV; do not misclassify auth or transient failures as unsupported features |
| HTTP or in-stream 401/403 | Terminal for this connection; no retry loop or transport fallback; later login may start another connection |
| 429/5xx, network failure, repeated short EOF | Bounded backoff and retry budget, honoring retry hints; no hot loop on a quiet or broken stream |
| Close while reading or sleeping | Cancel the request and backoff, release the reader, and deliver no later updates |
| Chunked UTF-8/JSON, malformed or truncated frame | Parse complete events across chunks; never treat partial data as a completed snapshot |
| Delete and recreate with the same name | New UID has distinct identity; a late response cannot resurrect the deleted object |
| Object enters or leaves a label-selected scope | Store membership follows the watch, including removal without deleting the upstream object |

Test retries that fail again, expired list continuations, and scope changes as well
as the happy path. Resume checkpoints must advance only after successful event
application; callback failure must follow the shared connector's documented behavior.
Do not assume periodic bookmarks: an idle native watch still needs recovery.

## Deliverable 2: show the value of choosing a view

Use the same small frontend and workload to demonstrate native, `krm-full/v1` and
`krm-spec/v1` access. Reuse rendering and editing code where the view permits it;
show differences explicitly rather than claiming identical payloads or guarantees.

| Operation | Native | Full | Spec |
| --- | --- | --- | --- |
| Initial load / spec edit | Original object | Projected object | Projected object without status |
| Status-only update | Update delivered | Update delivered | No object event |
| Managed-fields-only update | Update delivered | No object event | No object event |
| Rotate a test Secret value | Value delivered to an authorized reader | Value absent; redaction revision changes | Value absent; redaction revision changes |
| Reconnect after a delete | Resume or resnapshot, according to retained history | Fresh snapshot prunes absent UID | Fresh snapshot prunes absent UID |

Use synthetic Secret values and assert their absence from full/spec transcripts,
including after reconnect. Include an unauthorized caller on both routes and verify
that neither a fallback nor a shared identity grants access. In foyer, an authorized
user can separately read the Secret through native access: this is view selection,
not an additional field-permission system.

Document the scope/projection/suppression distinction with executable request
examples. There is no arbitrary browser-defined field subscription, general gateway
field selector or built-in status-only view today. Broader projections should follow
a concrete use case and their own read/write contract.

Measure bytes delivered, object events, store notifications/renders, reconnect
snapshot size, upstream watches and guarded-save conflicts under identical workloads.
Report controller churn and object counts. Suppression reduces downstream work, not
the changes arriving at the gateway. Resumable native watches can save snapshot work;
they do not have zero reconnect cost.

## Follow-up 3: reduce save interruptions from suppressed updates

Suggested separate issue title: **feat(client): reduce save interruptions caused by
suppressed resource updates**

### Upstream already explains and tracks this

**Checked 2026-10-05:** main at `a8281c58ac6adcb0b59a4f66ac87bc21828d1e08`,
the open PR stack through #59 at `b5cc77953bc58acf873fc7999c8c263af77be1d2`,
and the open issue/PR lists. The problem is well expressed; a version-only event is
already considered and explicitly deferred, not an overlooked feature.

| Source | What is already covered |
| --- | --- |
| [Saving guide: quiet streams](https://github.com/ConfigButler/krm-stream/blob/a8281c58ac6adcb0b59a4f66ac87bc21828d1e08/docs/saving.md#why-a-quiet-stream-can-still-reject-a-save) | The exact sequence: status advances RV, spec projection stays quiet, the save gets 409. It distinguishes stale version from a field conflict and warns that sustained invisible churn can prevent save progress. |
| [Proposal 0005: version delivery options](https://github.com/ConfigButler/krm-stream/blob/a8281c58ac6adcb0b59a4f66ac87bc21828d1e08/docs/proposals/0005-kubernetes-stream-and-save-semantics.md#version-delivery-options) | Compares complete-object version updates, version-only events, write tickets and alternate patch strategies. Current choice: retain existing emissions and explicit save outcomes; prefer existing full event shapes if measurements later justify version delivery. |
| [Proposal 0005: host write strategies](https://github.com/ConfigButler/krm-stream/blob/a8281c58ac6adcb0b59a4f66ac87bc21828d1e08/docs/proposals/0005-kubernetes-stream-and-save-semantics.md#host-write-strategies) | Future automatic retries must preserve the submitted intent, compare relevant base values, protect whole-array replacements and exclude typing after Save. Having no store conflicts is insufficient by itself. |
| [Proposal 0006: verification and scope](https://github.com/ConfigButler/krm-stream/blob/a8281c58ac6adcb0b59a4f66ac87bc21828d1e08/docs/proposals/0006-stream-and-save-implementation-plan.md#verification-and-scope) | Explicitly defers version-only events and automatic conflict-free retries until a concrete use case and measurements justify them. Priority 4 already requests save-409 measurements, including the share with no field conflicts. |
| [PR #58](https://github.com/ConfigButler/krm-stream/pull/58), head `2268916cf8e08650de1468d5832e61b125de2391` | Adds real-API composition tests for suppressed status changes, bookkeeping-only changes, guarded reconciliation and a fresh deliberate save. It remains open; these changes are not foyer's pinned 0.7.0 integration. |

The [test source](https://github.com/ConfigButler/krm-stream/blob/2268916cf8e08650de1468d5832e61b125de2391/gateway/kube/composition_e2e_test.go#L573)
asserts `PATCH 409` then `GET 200`, a `version-stale` outcome without field conflicts,
and a subsequent deliberate `PATCH 204` once churn has stopped. The PR reports local
real-cluster results; this audit inspected the code and reported evidence, and did
not rerun that upstream suite. Its description says the dedicated real-API workflow
is not yet pushed, so green general checks are not proof of that workflow running.

The open issue list returned no entries, and no listed open PR implements batched
version-only delivery or automatic conflict-free retry. The item is on the deferred
design list, not a scheduled implementation. The supported save flow remains guarded
reconciliation followed by another deliberate Save.

### The adopter request

For a person editing spec through `krm-spec/v1`, controller status updates should not
repeatedly require a second click on Save when none of the relevant editable values
changed. Keep low browser traffic and genuine conflict protection. This is a concrete
adopter use case for revisiting proposal 0006's deferral; we have not yet measured its
frequency in a representative foyer workload.

Please record an evaluation item under that existing plan, reusing PR #58's fixtures.
Compare the current deliberate-save baseline with:

1. **An optional bounded save-recovery policy.** Refresh and reconcile under the
   existing guards, then determine whether the originally submitted intent is still
   valid. A retry must not include later typing or silently accept a changed array,
   dependency or UID. Preserve explicit review for real disagreements. Document
   retry exhaustion and distinguish definite 409 rejection from an unknown write
   outcome; do not automatically replay an ambiguous write.
2. **Optional coalesced version delivery.** Retain the latest eligible object version
   per subscriber and UID, and deliver bounded periodic batches instead of a message
   per suppressed update. Compare small version-only records against batched complete
   projected updates, since proposal 0005 currently prefers the existing event shapes.

For a version-only record, require proof that the last delivered authoritative
projected content, excluding RV but including redaction records, still represents
that UID at the newer version. The draft is not the comparison baseline. The record
must be bound to the current view/snapshot and applied in order: a pending batch may
not overtake a visible change, resurrect a deleted object, roll a version backward or
cross a reconnect. Clear or supersede pending records when those transitions occur.
A collection bookmark cannot supply an individual object's edit version.

Advance the store's authoritative version without resetting drafts or causing
content-only subscribers to re-render. Capture each future save's version and patch
together; never mutate an already captured intent merely because a batch arrived.
State which writes may rely on this guarantee, including dependencies outside the
visible view. Periodic delivery can reduce staleness but cannot prevent a change
between the batch and PATCH, so recovery is still required.

### Evidence and decision requested

Use the same objects, subscriber count, edit workload and status-update rates for all
variants. Include quiet periods, bursts and sustained churn; success after churn stops
alone does not establish a usable editing experience during churn. Measure:

- stale-version 409s, field conflicts, extra Save clicks and save completion latency;
- successful saves and bounded failures while churn continues;
- GET/PATCH attempts, downstream bytes/events, browser notifications and renders;
- pending-batch memory and delivery work as object and subscriber counts grow.

Cover later typing, changed array members, real concurrent spec edits, deletion and
UID replacement, reset/reconnect with a pending batch, expired sessions, denied access
and hidden Secret rotations. A redaction revision change still needs an ordinary
visible update; it must not disappear into a version-only advance.

Keep `krm-spec/v1`'s existing silence for status-only updates by default. Any delivery
mode needs an explicit compatibility decision for old consumers and a declared
opt-in contract; do not silently add wakeups to the current projection. The outcome
can be to retain the baseline if costs outweigh measured UX benefit. Document the
decision, measurements and adoption guidance, rather than committing to a new wire
event or a universal automatic-save policy in advance.

In foyer, the hello example currently recovers a 409 by reopening the stream and
asking for another Save. Adopting a guarded projected read would be a separate host
integration improvement: the upstream example's custom read/save endpoints are not
provided by foyer's transparent `/k8s` proxy, and a raw GET cannot be fed into a
projected editor without honoring its view and reconciliation contract.

## Follow-up 4: meet frontend stores through optional adapters

Suggested separate issue title: **feat(examples): demonstrate Pinia and TanStack DB integration**

The [frontend integration investigation](frontend-integrations-pinia-tanstack-db.md)
recommends two small examples that can start with the existing gateway connector:

- A Pinia setup store using the existing Vue composable, with a shared collection,
  selected-resource editor, field bindings through krm-stream's edit methods, save
  state and subscription cleanup. Introduce a global plugin only if common client
  injection or configuration warrants it.
- A read-only TanStack DB Collection adapter for live filtering, sorting and joins
  over authoritative resource rows. Keep drafts/conflicts and guarded saves in
  krm-stream initially; do not have both systems independently reconcile one edit.

Evaluate a public observation API for affected UIDs, committed resource changes and
completed snapshots. The current coarse store subscription can force adapters to
scan/copy a collection on every notification. Prove the need with the examples and
measure update work before expanding the core API. Preserve full field removal,
snapshot completeness, missed-delete recovery, redaction metadata and connection
state; local query predicates do not automatically reduce network subscriptions.

Keep dependencies optional. Foyer can demonstrate the adapters under its existing
session, projection and RBAC boundaries. This is proposed work, not an implemented
adapter or a requirement for merging the connector changes. The linked investigation
records source revisions, ownership choices and acceptance scenarios.

## Relation to upstream continuation

[PR #59](https://github.com/ConfigButler/krm-stream/pull/59) proposes resuming the
gateway's Kubernetes watch while keeping its browser stream open. This request is
about a browser connector resuming its own native watch through a proxy. The two
can share test scenarios, but have different checkpoint owners and authorization
lifecycles. Neither introduces browser resume into the gateway's SSE protocol.

## Adoption in krm-foyer

The server paths already exist. After the native connector is released, foyer can
upgrade its dependency and add an example exercising both paths under the same
session, logout/expiry and RBAC tests. Until then the watch guide recommends the
existing gateway integration for new pages and marks the native connector as requested.
This documentation change implements neither the connector nor a dependency upgrade.
