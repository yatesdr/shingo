# Order-Building & Source Dispatch

Verified against: 74c3eb77

**Source of truth:** `shingo-core/dispatch/builder.go`, `shingo-core/dispatch/source_finder.go`,
`shingo-core/dispatch/allocator.go`, `protocol/status.go`. This document is the
human-readable rendering; if the two diverge, the source wins.

Related: [reservations.md](reservations.md), [storage-protections.md](storage-protections.md),
[order-state-machine/transitions.md](order-state-machine/transitions.md).

---

## One SourceFinder behind both paths

Order intake (the planner) and the fulfillment scanner (the retry path) resolve
"which bin fulfills this order" through one engine, so a queued order's retry
cannot source differently from its intake (a loader-pool or group-scoped source
widening to a plant-wide scan on replay).

`shingo-core/dispatch/source_finder.go` — `type SourceFinder` — is that engine.
Both intake planning and scanner replay call it. It runs a fixed **tier
cascade** for one order + one intent and returns a closed outcome — it never
claims, transitions, or writes. The cascade, as `FindSource` documents it:

```
1. NGRP/LANE source        → resolver.Resolve, classified   (full intent)
2. dedicated-loader pool  → sourceFromDedicatedLoader        (Drain/Fill)
3. group/lane-scoped empty → FindEmptyCompatibleBinInGroup   (empty intent)
4. concrete-node candidate → ListBinsByNode    (move-shaped; a full naming a node)
5. plant-wide fallback    → FindSourceBinFIFO / FindEmptyCompatibleBin
6. post-find buried check → IsSlotAccessible                 (empty intent)
```

The **"no fall-through" edges are the load-bearing part**:

- A **move** sources node-locally (tiers 1, 2, 4) and **never** falls through to
  the plant-wide scan.
- A **scoped empty retrieve** with no scoped empty **queues** rather than
  widening plant-wide.
- An **NGRP** capacity/buried error **queues (or reshuffles) scoped** rather than
  widening.

A drift in any of those edges sends an order to the wrong source, silently (a
wrong source is not an error), so they are kept exact and the finder is shared
rather than copied. The `forbidigo` rules on the plant-wide finders
(`.golangci.yml`) keep every other caller out of tier 5.

## Branch on data, not on order type

The dispatch control flow does **not** switch on `protocol.OrderType`
(`retrieve` / `retrieve_empty` / `store` / `move` / `complex`) to decide how to
source. It keys on the **sourcing intent** — a small data field stamped once at
intake (`shingo-core/dispatch/order_provenance.go`):

| `SourceIntent` | Means | Set by |
|----------------|-------|--------|
| `""` (Full) | a payload-matched full bin | `retrieve` |
| `"empty"` | an empty compatible carrier | `retrieve_empty` |
| `"local"` | the bin *at* a concrete source node (node-local) | `move` |
| `"on_deck"` | the bin is already on the pinned robot's deck: nothing to source, and the plan is a single unload | the carried-bin recovery (`Need.SourceIntent`; no type maps to it) |

`SourceIntentForType` maps the three plain types; `on_deck` is the one intent
that changes the plan rather than the search, and the one that pins a vehicle.
The finder reads `order.SourceIntent` to pick its tier path. A height an empty
is wanted at is not an intent: it is the order's want (`orders.want`, ground
rule 4 in [ground-rules.md](ground-rules.md)).

Keying on data keeps the simple and complex dispatch paths **shape-compatible**:
a complex order is a multi-need case over the same finder, not a parallel
machine, and dispatch branches on intent, `len(distinct needs)`, `hasWait` and
`SiblingOrderUUID` rather than on `OrderType`. `OrderType` is still carried on
the order and emitted to the fleet; the `\.OrderType\b` forbidigo rule keeps
dispatch and fulfillment from routing on it.

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
- A `queued` order holds its sources as **reservations**
  ([reservations.md](reservations.md)) and retries by **reconciling** those
  holds, not by re-shopping from scratch.

So an incomplete complex order makes progress tick-by-tick: it sits in
`queued` holding revocable reservations, the Allocator reconciles on each tick,
and it is dispatched the moment the missing source appears.

`sourcing` is not in the set: no edge enters it, and it survives only on old
history rows ([order-lifecycle.md](order-lifecycle.md)).

## The one issue

This section is the canonical statement of how an order comes to exist and what
it holds from birth; [reservations.md](reservations.md) and
[ground-rules.md](ground-rules.md) link here.

An order comes to exist one way: **issued**, through `shingo-core/dispatch/builder.go`
(`IssueOrder` in a transaction of its own; `issue` on the caller's, for a compound's
legs). A door states a `Need` — the row's facts no plan decides, and what the order
holds from its issue — and the planner's `plan.Plan` rides beside it; the issue writes
the row through the single INSERT (`orders.Create`) and takes the holds in the same
transaction, or nothing is written:

| `Need` field | What the issue does with it |
|--------------|-----------------------------|
| `Cart` | the one carrier the order is for: the order's cart (its `order_bins` row, `Order.HoldFromBirth`) and `named_bin_id` are written with the INSERT, and its pending bin reservation is taken in the same transaction |
| `Window` | the drop when it is one carrier's at a time (a loader window, a storage slot), which **is** the delivery: a pending slot reservation, taken before the cart. See *An order bound to a window* below |
| `QualityHoldBy` | a station's containment move: the cart's hold marker |
| `Robot` | a carried-bin recovery's pin, in the INSERT |
| `SourceIntent` | the intent when the type does not decide it: the carried-bin recovery's `on_deck` |
| `Leg` | a compound's child: parent and turn. A dig leg's bin is written as its cart with the INSERT and held by its parent's lane, with no bin reservation of its own |
| `Steps`, `Complex`, `Sibling` | a multi-leg order's resolved steps, its coordinated facts, its paired order |
| `By` | who asked: `Person(name)` takes a stranger's pending hold on the cart out loud; the zero value (Core's own errand) is refused by it; nobody takes a confirmed hold |

**An order for one carrier holds it from its issue.** A person's bin move from
Core's pages, a station's move naming its bin, a window pull (the drain's, stage
2's), a carried-bin recovery and a home clear's leg each declare their cart
(`Need.Cart`). Its pending reservation is taken in the issue's own transaction,
and the hard claim follows at the hand, as for every order — no hard claim
before the dispatch step on any path. Who asked decides a stranger's pending
hold on the cart: a person's issue takes it out loud (`store.TakeSoftHolds`: the
holder's reservation and cart row go, and it is told; one that is itself an
order for that cart is ended by name), Core's own errand is refused by it, and
nobody takes a confirmed hold. A dig's leg is the one order for a carrier that
holds it by its parent's lane instead of a reservation (the exception is
described in [reservations.md](reservations.md)).

**An order bound to a window holds it from its issue** (`Need.Window`, a pending
slot reservation taken in the issue's transaction before the cart): a window
pull's move (`windowMove`); a Core ask, the loader loop's at the window it chose
and the level keeper's at the slot it pre-resolved (`AdmitCoreAsk`); a
station's move whose settled drop is one carrier's at a time
(`CreateInboundOrder`, the acquire's own rule); and the carried-bin recovery's
drop when it is one (`dispatch.OneCarrierAt`). A window another order holds
refuses Core's own errand with nothing written (`ErrWindowTaken`): the loader
loop names the window `window-reserved` and asks at its next one, the keeper
asks for its next slot, and the recovery is refused
(`CarriedBinNotRecoverable`). A station's move takes another order's pending
hold on its window out loud (the holder's row deleted and the holder told
`slot-taken`, naming the move); a confirmed hold, a robot's, refuses it
`claim_failed` naming the holder. A dig that outranks the holder takes the
window out loud too (the holder told `slot-taken`), and one that does not, does
not (`TestDig_TakesAnAsksWindowOnlyByRank`). The acquire's hold
(`holdPosition`) is owner-idempotent on a window the order already holds. Not
declared at the issue: a person's bin move's drop (held at its acquire, by the
acquire's rule), a home clear's (its parent delivers to the home and holds
nothing there) and every complex order's (the reserve pass holds storage
dropoffs and declared-exclusive ones, never a loader window).

The census of doors is `shingo-core/internal/testdb/issue_census.go` (`IssueDoors`):
what each door's order holds at the commit that issues it, held to the tree by
`TestCensus_Issuers` (every caller of the INSERT, every site on the walk up from it)
and to the database by each package's `TestIssueCensus_AtCommit`. A door that changes
what it holds flips its row there first.

## Plan-shape intake

Orders are built **plan-shaped** at intake: the order's steps are resolved into
a concrete plan (sources, destinations, waits, siblings) *before* dispatch, and
the reserve/confirm legs in [reservations.md](reservations.md) key on that
plan's distinct needs rather than re-deriving them at dispatch time. A simple
order is the one-need case of the same machinery a multi-step complex order
uses, which is what lets the reservation substrate serve both families without
forking. The simple and complex dispatch paths are still two paths over the
shared finder, intent-keyed sourcing and plan-shape primitives.
