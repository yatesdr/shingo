# Order-Building & Source Dispatch

**Source of truth:** `shingo-core/dispatch/source_finder.go`,
`shingo-core/dispatch/allocator.go`, `protocol/status.go`. This document is the
human-readable rendering; if the two diverge, the source wins.

Related: `[[reservations]]`, `[[storage-protections]]`, `[[order-state-machine/transitions]]`.

---

## Problem — two finders, drifting apart

Order intake (the planner) and the fulfillment scanner (the retry path) each had
their **own copy** of the logic that resolves "which bin fulfills this order."
That's not a style complaint — it was an active bug:

- A **dedicated-loader retrieve** scoped its source to a loader pool at intake
  (oldest-part-first, keep a partial buffer). On the scanner's retry, the inline
  copy skipped that scoping and fell through to a **plant-wide** scan — silently
  re-opening the bug the scoping was added to fix. Wrong source, not an error, so
  it surfaced as "partials not consumed oldest-first," never as an incident.
- A **group/lane-scoped empty retrieve** (added so a multi-supermarket plant
  doesn't pull empties from the wrong area) had its scope dropped on replay.

Both fired on the *same momentary-scarcity condition* that makes these orders
queue in the first place — so exposure was the normal path, not an exotic corner.

The deeper issue: source resolution lived in two places, so any future change
had to be made twice and could silently disagree.

## Solution — one SourceFinder behind both paths

`shingo-core/dispatch/source_finder.go` — `type SourceFinder` — is the single
source-finding engine. Both intake planning and scanner replay call it. It
runs a fixed **tier cascade** for one order + one intent and returns a closed
outcome — it never claims, transitions, or writes:

```
1. NGRP synthetic source  → resolver.Resolve, classified   (full intent)
2. dedicated-loader pool  → sourceFromDedicatedLoader       (Drain/Fill)
3. group/lane-scoped empty → FindEmptyCompatibleBinInGroup  (empty intent)
4. concrete-node candidate → ListBinsByNode                 (local/move)
5. plant-wide fallback    → FindSourceBinFIFO / FindEmptyCompatible
6. post-find buried check → IsSlotAccessible                (empty intent)
```

The **"no fall-through" edges are the load-bearing part** — the exact bugs the
collapse fixes:

- A **move** sources node-locally (tiers 1, 2, 4) and **never** falls through to
  the plant-wide scan.
- A **scoped empty retrieve** with no scoped empty **queues** rather than
  widening plant-wide.
- An **NGRP** capacity/buried error **queues (or reshuffles) scoped** rather than
  widening.

A drift in any of those edges re-opens a fixed bug, so they are kept exact and
the finder is shared rather than copied.

## Branch on data, not on order type

The dispatch control flow does **not** switch on `protocol.OrderType`
(`retrieve` / `retrieve_empty` / `store` / `move` / `complex`) to decide how to
source. It keys on the **sourcing intent** — a small data field stamped at
intake (`shingo-core/dispatch/order_provenance.go`):

| `SourceIntent` | Means | Order type that sets it |
|----------------|-------|-------------------------|
| `""` (Full) | a payload-matched full bin | `retrieve` |
| `"empty"` | an empty compatible carrier | `retrieve_empty` |
| `"local"` | the bin *at* a concrete source node (node-local) | `move` |

The finder reads `order.SourceIntent` to pick its tier path. This matters
because it keeps the simple/complex dispatch paths **shape-compatible**: a
complex order is a multi-need case over the same finder, not a parallel
machine. Branching on data (intent, `len(distinct needs)`, `hasWait`,
`SiblingOrderUUID`) rather than on `OrderType` is what makes the eventual
simple↔complex unification a migration rather than a rewrite.

## The acquiring set — `{queued, sourcing}`

The fulfillment scanner retries orders whose status is in the **acquiring**
set: `queued` and `sourcing`, exactly. `protocol/status.go`:

- `IsAcquiring(s)` — true for `queued`, `sourcing`.
- `AcquiringStatusSQLList()` — the SQL literal `('queued','sourcing')`, built by
  the same `buildStatusSQLList` factory that produces the terminal and
  pre-dispatch lists. One source of truth for "retryable state."

This set is **exactly** those two, not a loose "non-terminal":

- `tryFulfill`'s re-check and the complex dispatch entry guard use this set for
  *other* load-bearing reasons (catch a cancelled-mid-process order; block
  re-dispatching a reshuffling parent). Widening past the two would break those.
- A `sourcing` order holds its sources as **reservations** (`[[reservations]]`)
  and retries by **reconciling** those holds, not by re-shopping from scratch.

So the scan-set widening to `{queued, sourcing}` is what lets an incomplete
complex order make progress tick-by-tick: it sits in `sourcing` holding
revocable reservations, the Allocator reconciles on each tick, and it completes
the moment the missing source appears.

## Plan-shape intake

Orders are built **plan-shaped** at intake: the order's steps are resolved into
a concrete plan (sources, destinations, waits, siblings) *before* dispatch, and
the reserve/confirm legs in `[[reservations]]` key on that plan's distinct
needs rather than re-deriving them at dispatch time. A simple order is the
one-need case of the same machinery a multi-step complex order uses. This is
the structural decision that lets the reservation substrate serve both families
without forking.

## Out of scope

- The simple↔complex **dispatch-path unification** itself is a future phase;
  this work builds toward it (shared finder, intent-keyed dispatch,
  plan-shape primitives) but does not collapse the two paths into one.
- `OrderType` still exists and is carried on the order and emitted to the fleet;
  it's the *sourcing* decision that keys on intent data, not the order's
  identity.
