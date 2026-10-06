# Connections across a pod replacement

**Status (2026-10-06): the rehearsal's dial timeouts are diagnosed and fixed in the e2e
harness. A rollout that swallows no connection is parked:** live rollouts are not part of
the demo, and it is operator work the project does not need yet. This page records what
was measured, so the work can start from here rather than from scratch.
The roadmap keeps the item ("Rolling updates that refuse no connection").

## What failed

A fresh `task verify` run failed in the rehearsal's spec "with a watch of each user's own",
at `rehearsal_test.go:144`, while it opened 1,800 streams at once (200 identities, 9
each): `dial tcp 172.29.250.2:30443: i/o timeout`. The same spec passed when run alone.

## What it was

It was not krm-foyer refusing anything, and not a shortage of capacity. The fixture's
NodePort path takes a burst of 8,000 simultaneous connections with no loss (p99 162 ms).
The timeouts appear only after a krm-foyer pod has been replaced:

1. A connection is tracked on the node (conntrack) together with the pod kube-proxy sent
   it to. When that pod goes away, kube-proxy changes its rules for new connections, but
   it does not remove TCP entries already tracked. It does remove UDP ones. The entries
   live on until they time out, up to two minutes for `SYN_SENT`.
2. SYNs sent in the moment between the old pod's address going away and kube-proxy's
   rules following it are never answered. Their entries still point at the gone address.
3. A later connection from the same client address and port follows such an entry to
   the gone address. Nobody answers it or its retransmissions, so the dial times out
   after five seconds.

The suite is one client making thousands of connections a minute to one NodePort, so it
reuses source ports quickly. The rehearsal replaces krm-foyer twice per run, and other
specs replace it too, once with a forced delete. Whether a burst met stale entries
depended on what ran in the two minutes before it, which explains why the failure came
and went with spec order.

Evidence, from the fixture:

| Check | Result |
| --- | --- |
| 1,800 dials at once, steady state | 0 failures, repeatedly; 8,000 at once also 0 |
| After a `rollout restart` with traffic during it, 3 bursts of 1,800 | 16 to 28 `i/o timeout` per burst; 44 and 239 per three bursts in two later rounds |
| After a forced pod delete, the same | 75 to 97 per burst; 229 and 172 per three bursts |
| Stuck client sockets compared with the node's `conntrack -L` | Their source ports matched `SYN_SENT` entries DNATed to deleted pods, one of them deleted two minutes earlier |
| The same replacements, then the cleanup below, then the bursts | 0 timeouts in every round |

## The fix in the harness

After the suite replaces krm-foyer's pod, `fixture.forgetGonePods` deletes the node's
conntrack entries for krm-foyer's NodePort whose reply comes from a pod address that no
longer exists ([fixture_test.go](../../test/e2e/fixture_test.go), called from
`replaceFoyer`). This is the cleanup kube-proxy already does for UDP. It touches only
this fixture's node and only entries that can never be answered. The rehearsal's
workload, 200 identities × 9 streams opened at once, and its 5-second dial timeout are
unchanged.

## What is parked: a rollout that swallows no connection

The same mechanism affects a client connecting *during* a rollout, and a real client
behind one NAT address could meet it too. The pending spec
[foyer_rollout_test.go](../../test/e2e/foyer_rollout_test.go) dials continuously across a
`rollout restart` and accepts refusals, but no timeouts. What was tried, uncommitted:

- **A shutdown delay in krm-foyer.** On SIGTERM it would fail `/readyz` at once and keep
  serving for `-shutdown-delay`, with a 1-second readiness probe, so the endpoint stops
  serving before the address goes away. Measured after a `rollout restart`: the old
  endpoint stopped serving at 2.7 to 5.1 s, its NAT rule was gone at 4.0 to 5.8 s, and the
  pod itself was gone at 5.8 to 7.8 s without a delay, or 12 to 13 s with a 10-second
  delay. With the delay, a rollout still swallowed 0 to 2 connections in about one run
  in two. The cause of those was not found.
- **Probe design matters.** A probe that sends `Connection: close` makes krm-foyer close
  first, often with a reset. The client then reuses those ports at once, against the
  node's fresh `CLOSE` entries: every SYN of a burst right after that was delayed by one
  retransmission (1.09 s). The parked spec therefore only opens and closes TCP
  connections.
- **Two explanations were ruled out.** Entries in `LAST_ACK` expire before the client
  frees their ports. `TIME_WAIT` entries from connections the client closed are reopened
  correctly by conntrack.

The chart's `Recreate` strategy keeps one pod at a time, so a rollout always has a gap
in which new connections are refused. "Refuse no connection" needs two pods during an
update, which in turn needs the multi-replica design the roadmap lists first.

## When this comes back

- Start from the pending spec and the measurements above, on the fixture as it is.
- Decide first whether the goal is "refuse, never swallow" (one pod) or "refuse nothing"
  (two pods, after the multi-replica design).
- gitops-reverser's move to Kubernetes 1.37 ran its e2e on one server and one agent
  after a server restart left cluster DNS dead. That is a different failure, but the
  fixture's own server restart (when the authentication configuration changes) would
  benefit from the same layout.
