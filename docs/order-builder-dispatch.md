# Order-Building & Source Dispatch

**Source of truth:** `shingo-core/dispatch/builder.go`, `shingo-core/dispatch/source_finder.go`,
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

## The acquiring set — `{pending, queued}`

Every order is issued `pending`. **Acquiring** is the act between `pending` and
`dispatched` — asking for the order's whole hand and taking it in one
transaction — never a stored status. An order whose hand is not whole parks in
`queued` with its cause on the row. The fulfillment scanner asks orders whose
status is in the **acquiring** set: `pending` and `queued`, exactly.
`protocol/status.go`:

- `IsAcquiring(s)` — true for `pending`, `queued`.
- `IsWaiting(s)` — true for `queued` alone: a cause on the order describes a
  wait going on now. Readers that tell, keep or show a wait ask this, never
  `IsAcquiring`.
- `AcquiringStatusSQLList()` — the SQL literal `('pending','queued')`, built by
  the same `buildStatusSQLList` factory that produces the terminal and
  pre-dispatch lists. One source of truth for "may still be asked."
  `ListAcquiring` also requires `parent_order_id IS NULL`: a compound child is
  dispatched by the compound machinery, never the scanner.

This set is **exactly** those two, not a loose "non-terminal":

- `tryFulfill`'s re-check and the complex dispatch entry guard use this set for
  *other* load-bearing reasons (catch a cancelled-mid-process order; block
  re-dispatching a reshuffling parent). Widening past the two would break those.
- A `queued` order holds its sources as **reservations** (`[[reservations]]`)
  and retries by **reconciling** those holds, not by re-shopping from scratch.

So an incomplete complex order makes progress tick-by-tick: it sits in
`queued` holding revocable reservations, the Allocator reconciles on each tick,
and it is dispatched the moment the missing source appears.

`sourcing` was in this set before C1, as a stored status; it is history now
(no edge enters it; see `[[order-lifecycle]]`).

## The one issue

An order comes to exist one way: **issued**, through `shingo-core/dispatch/builder.go`
(`IssueOrder` in a transaction of its own; `issue` on the caller's, for a compound's
legs). A door states a `Need` — the row's facts no plan decides, and what the order
holds from its issue — and the planner's `plan.Plan` rides beside it; the issue writes
the row through the single INSERT (`orders.Create`) and takes the holds in the same
transaction, or nothing is written:

| `Need` field | What the issue does with it |
|--------------|-----------------------------|
| `Cart` | the one carrier the order is for: a pending bin row, `bin_id` and `named_bin_id` stamped in the INSERT |
| `Window` | a loader window that **is** the delivery: a pending slot row, before the cart; another order's refuses the issue |
| `QualityHoldBy` | a station's containment move: the cart's hold marker |
| `Robot` | a carried-bin recovery's pin, in the INSERT |
| `Leg` | a compound's child: parent and turn; a dig leg's bin stamped and held by its parent's lane, with no bin row |
| `Steps`, `Complex`, `Sibling` | a multi-leg order's resolved steps, its coordinated facts, its paired order |
| `By` | who asked: `Person(name)` takes a stranger's pending hold on the cart out loud; the zero value (Core's own errand) is refused by it; nobody takes a confirmed hold |

The census of doors is `shingo-core/internal/testdb/issue_census.go` (`IssueDoors`):
what each door's order holds at the commit that issues it, held to the tree by
`TestCensus_Issuers` (every caller of the INSERT, every site on the walk up from it)
and to the database by each package's `TestIssueCensus_AtCommit`. A door that changes
what it holds flips its row there first.

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
