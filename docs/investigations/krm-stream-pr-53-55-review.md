# krm-stream PRs #53–#55: review asks

**Review, 2026-10-04.** For the agent working on krm-stream. Reviewed at:

| PR | Branch | Head |
|---|---|---|
| #53 | `docs/proposal-0009-stream-editor-separation` | `42c745c` |
| #54 | `feat/proposal-0010-gateway-api-cleanup` | `8bc56df` |
| #55 | `fix/stream-review-feedback` | `0921657` |

All three are green in CI. #55 addressed every point of the previous review round
(host-callback exceptions, the adopted-save ghost, Change 2 slimmed with deferral
triggers, an upgrade guide). The asks below are what remains. Verify each against the
current code before acting; if one does not hold, say why rather than forcing a change.

Asks 1–4 are fixes to the open PRs. Asks 5–7 are documentation and a follow-up
experiment, prompted by the question in
[sse-and-native-watches.md](sse-and-native-watches.md).

**2026-10-05 follow-up:** the [native connector request](krm-stream-native-connector-request.md)
expands ask 7 with recovery acceptance and a projection comparison example. Keep the
reference gateway in krm-stream and keep its SSE encoding. The findings and reviewed
heads above remain the historical review, not a fresh assessment of the PRs.

## 1. A late `adoptSaved` re-inserts a deleted object (#55)

The ghost fix in `9b092d6` covers the snapshot case: a response adopted after `reset`
no longer counts as membership, so `synced` prunes it. Its sibling is still open:

1. The stream is live (no snapshot in progress).
2. The host sends a save for UID `u`.
3. Someone deletes the object; the stream delivers `deleted` for `u`, and the store removes it.
4. The save response arrives; the host calls `adoptSaved(response)`.
5. `adoptSaved` finds no existing entry and calls `#upsert`, which inserts `u` again.

Nothing removes it until the next reconnect's snapshot, which on a healthy connection
may be hours away. The commit message's "nothing would ever remove it" applies here.

Change 2 removes `adoptSaved`, which ends this. Until then:

- Add a red-first test with the sequence above, asserting `ids()` does not contain `u`
  after step 5.
- Either make it pass (for example, `adoptSaved` refuses to insert a UID the store
  removed by a `deleted` event, or one it never held while no create is pending), or, if
  Change 2 lands in the same release, pin the current behavior with that test as a known
  limitation and point the `adoptSaved` doc comment at `captureReconciliation`.

## 2. An `onError` that throws on a terminal error ends as `closed` (#55)

In `connectResourceStream`, the `error` hook sets `terminal ||= true` and then calls
`onError` through `call()`. If `onError` throws, `fail()` aborts the controller, `run()`
takes the `if (controller.signal.aborted) break;` branch before checking `terminal`,
and the final published state is `closed`.

The doc comment says the stream ends `closed` "unless it had already ended `terminal` or
`exhausted`". For this path that is wrong: the server refused terminally, and a UI that
shows "signed out" or "access denied" from `state.status === "terminal"` misses it.

Either:

- **Preferred:** keep the server's verdict. Check `terminal` before the abort check (or
  let the final publish use `ended` when it is `terminal` or `exhausted`, and `closed`
  only otherwise), while `closed` still rejects with the host's exception.
- Or keep the behavior and correct the doc comment and the upgrade guide.

Acceptance: a test where the gateway sends a terminal `FORBIDDEN` and `onError` throws,
asserting the final state and that `closed` rejects with the thrown error.

## 3. A golden changed, and the error message names a Go field (#54)

Proposal 0010's acceptance says fixture bytes stay unchanged, but
`conformance/gen/sse/resourceversion-unorderable.sse` changed: the `unorderable` error
message now says `StreamConfig.Ordering` instead of `Gateway.Ordering`.

The message goes to the browser. Telling an end user the name of a Go configuration
field is a server-side hint on the wire. The design elsewhere keeps such detail in
`Diagnostics`.

- Make the wire message generic (for example "upstream served a resourceVersion that
  cannot be ordered") and put the `StreamConfig.Ordering = OrderingLenient` advice in
  the `Diagnostics` error only.
- Regenerate the golden once, and reword the acceptance line in proposal 0010 to "wire
  framing and event shapes unchanged; one message text changed deliberately", or
  similar.

## 4. Dismiss the two CodeQL alerts on #53

Both "File data in outbound network request" alerts (`e2e/wire.ts:102`,
`src/sse.ts:206`) trace a URL from a fixture file into `fetch`. That is the purpose of
the conformance harness, not a vulnerability. Dismiss them in code scanning as false
positives with that reason, then resolve the two review threads.

## 5. Rewrite `docs/why-a-gateway.md`

Its first argument is that native `EventSource` cannot read a Kubernetes watch or send a
bearer header. Since #53 the reference client does not use `EventSource`, so the
argument no longer supports the design; a reader who knows `fetch` streams will
discount the rest.

Rewrite the page around the gateway's concrete benefits:

- **Projection:** trimming `managedFields` and redacting Secrets before the browser.
- **Shared watches:** one upstream watch per scope, with timed reauthorization bounding
  revocation (Kubernetes does not end an open watch on an RBAC change).
- **Aggregated APIs** that refuse `sendInitialEvents` (observation F6): the gateway lists,
  then watches, and still frames a snapshot.
- **One error vocabulary** with terminal/retryable classification, the raw detail kept
  in `Diagnostics`.

Projection before delivery and shared upstream watches need server-side work.
List/watch fallback and error classification can also be provided by a native client.
Include existing `krm-spec/v1` suppression: per-user streams also save browser work
when status changes but the selected view does not.

And state plainly what does *not* need a gateway: credentials can stay on a host proxy
that attaches the user's token to a native watch, and the browser-side work (reader,
store, editor) is a client library. SSE is the framing chosen, not a requirement.

Also mention the one place the native watch is better: it resumes from the last
`resourceVersion`, while every v1 reconnect sends a full snapshot (spec: no
`Last-Event-ID` resume). Link the continuation work in proposal 0006 §4 as the answer.

## 6. Add the native-watch path to `docs/alternatives.md`

The table has no row for the most direct alternative: a backend proxy that attaches the
user's token, and the browser reading the native watch (Headlamp and krm-foyer's `/k8s`
work this way). Add it, with where krm-stream fits: the store and editor work on that
path too, and the gateway adds projection, sharing and the aggregated-API fallback.

## 7. Follow-up experiment: a native-watch connector

Not a fix, and not for these PRs. Since #53 the store's input is `ResourceStateEvent`,
so a connector can translate a native Kubernetes watch into it:

| Kubernetes | `ResourceStateEvent` |
|---|---|
| opening with `sendInitialEvents=true` | `reset` |
| `ADDED` / `MODIFIED` | `added` / `modified` |
| `DELETED` | `deleted` (identity from the last object) |
| `BOOKMARK` with `k8s.io/initial-events-end: "true"` | `synced` |
| 410 Gone, at opening or as an `ERROR` event | new connection with `reset` |
| end of response after a complete snapshot | resume from the last applied event or bookmark's `resourceVersion`, no `reset` |
| end before the initial snapshot completes | restart initialization; never resume from a partial snapshot |

The earlier sketch was removed after finding incomplete-snapshot and error-recovery
bugs. Use the [expanded request](krm-stream-native-connector-request.md) for acceptance:
a proposal (number assigned upstream) and executable example first, with the same
connector lifecycle, tested recovery and an explicit raw-view editing contract.
The gateway remains the choice for projection, suppression and sharing.

## Process

- Merge in stack order. After each squash, rebase the next PR onto `main`: CodeRabbit
  reviews only PRs based on `main`, so #54 and #55 have had no CodeRabbit review yet.
- Squash-merge keeps only the PR title, so #55's `fix(client)!` title is the only
  changelog line for the `adoptSaved` fix. That is acceptable, since all three ship in
  one release; mention the fix in the PR body so the release notes can carry it.
