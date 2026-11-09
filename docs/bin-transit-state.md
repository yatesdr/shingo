# Bin Transit State

Verified against: b9210bce

This document covers how Shingo tracks bins while they're physically in
flight, and how dropoff capacity gates dispatch.

---

## Terminology

### Transit Node

A synthetic node named `_TRANSIT` (one global row, `is_synthetic=true`).
Bins occupy this node while a robot is carrying them — between the
moment a pickup block FINISHES and the moment a dropoff block FINISHES.

The synthetic flag is load-bearing: every bin-finding query in Core
(`FindSourceFIFO`, `FindEmptyCompatible`, lane finders, NGRP resolvers)
already filters `claimed_by IS NULL AND is_synthetic = false`. A bin in
transit therefore can't be re-claimed by another order, without any
predicate changes in those queries.

### Carrier Node

A per-robot synthetic node named `_ROBOT:<vehicle>` (created lazily on first
use). A bin sits here when its order terminated while the robot was still
holding it on the deck — the bin is not lost, it is on that robot.

Synthetic for the same reason `_TRANSIT` is: the finders already exclude
synthetic nodes, so a bin riding a deck cannot be re-claimed or sourced from,
and it is never offered as a destination. It is deliberately NOT `_TRANSIT`,
because `_TRANSIT` plus no claim is the anomaly definition below, and a bin
whose location is known exactly is not an anomaly.

A bin leaves a carrier node when that robot's jack reports unloaded. There is no
jack-unload event to subscribe to — the jack is sampled state — so this is a
watch on the poll Core already makes every two seconds.

The reading that matters is the FIRST one after the deck empties, and it is
frozen: a robot drives on within seconds, and the station it reports decays as
it goes, so a later reading names wherever the robot went next rather than where
it set the bin down. The frozen reading is then resolved — first as a Core node
name, then through the scene's `GeneralLocation` rows, which carry the mapping
from the point a robot reports (`AP102`) to the station it serves (`SMN_007`).

A set-down the watch saw at a station is an ARRIVAL: the robot put the bin
there, which it could not have done onto a bin really standing there. So it is
placed the way a delivery is, and a stale record at that node is evicted to
`_TRANSIT` and surfaced, not obeyed.

Once the deck reads empty the bin is not on it, whatever else is true. Anything
the watch will not place goes back to `_TRANSIT` as an anomaly that says WHY,
and is never left recorded on a deck seen empty: left there, every later lift
and set-down of that robot's other work would read as this bin's. The reasons
need different responses: the point is not a station (and the note names what it
is — a charge point, a park point, an aisle); the destination will not take this
bin's type; Core restarted after the unload, so nobody watched it; Core was
running but heard nothing from that robot across the drop; or the drop was
watched but could not be placed before the observation aged out. The last three
never place, by design; a person finds the bin and moves it on the bins page.

### Anomaly

A bin where `node_id = _TRANSIT AND claimed_by IS NULL`. This state is
binary (no TTL) and emerges naturally:

| Order outcome | claimed_by | node_id | State |
|---|---|---|---|
| In flight, healthy | order.id | _TRANSIT | Normal — bin owned, in transit |
| Delivery succeeded | NULL (cleared by ApplyArrival) | dest | Normal — bin landed |
| Order failed/cancelled | NULL (cleared by atomic) | _TRANSIT | **Anomaly** — bin orphaned in flight |

`bins.anomaly_at` is an optional sort key for the operator's anomaly
view; the binary signal is what fires alerts.

`bins.anomaly_note` (v96) is where the robot carrying the bin last was —
coordinates, station names, and what its deck reported. Most of recovering a
stranded bin is the FINDING, and this turns the search into a map pin. Free
text, for a human; nothing queries it.

### Drop-point inference

When an order terminates with its bin still at `_TRANSIT`, Core asks the robot
where it went, rather than waiting for an operator to find it.

The watch starts at the pickup, not at the end. While the order runs, the same
two-second poll records the order's deck going loaded and the first at-rest
empty reading after that, frozen as above; it places nothing, because the
order's own arrival does. A later loaded reading forgets the set-down — a swap's
supply sets its carrier down at staging and lifts it again. So when the order
ends without arriving, Core already knows where the deck emptied, even if the
robot has since driven on and reads busy with another task: a bin set back on
its slot before a cancel is placed there, not left in transit. Outcomes, in
order:

| Robot's deck at the end | Set-down seen during the order | Outcome |
|---|---|---|
| loaded (`jack_state` 1) | — | bin moves to `_ROBOT:<vehicle>`; any earlier set-down is forgotten |
| empty | yes, at a station that resolves | bin placed there, `system:inferred`; a stale record there is evicted |
| empty | yes, anywhere else | anomaly, with where it was set down in `anomaly_note`; the sweep retries the resolution |
| empty (`jack_state` 3) | no; the station resolves and the node is free | bin placed there, `system:inferred` |
| anything else | no | anomaly, with the position in `anomaly_note` |

On the later sweep a loaded deck is this bin only if the watch never saw it set
down: by then the robot may be working another order, and a deck loaded then is
carrying that order's bin.

A deck MID-TRAVEL (`jack_state` 0 or 2) is not an answer: a bin halfway down is
neither on the robot nor at the station, so the inference declines rather than
making a placement an operator would have to undo. A placement from one reading
of a parked robot (no set-down seen) only suggests where the bin went, so it
declines a node Core shows occupied.

The terminal-event hook is the fast path; `ReconciliationService.Loop` re-runs
the whole decision over every stranded bin on its sweep (`strandedBinSweep`),
which is the guarantee, and which also clears a bin stranded while nothing was
watching.

### Reservation layer

`claimed_by` is the **confirmed-claim mirror** of a reservation, not the
whole ownership story. A bin a not-yet-dispatched order intends to pick up is held by a
**reservation** (a soft, revocable row) *before* `claimed_by` is set; the claim
is written atomically with the reservation's `pending → confirmed` flip. So:

- While an order is `pending`/`queued`, its sources are held as `pending`
  reservations — `claimed_by` may still be NULL, but the bin is not free.
- `claimed_by` and the reservation move together (one transaction); they never
  drift apart. Clearing `claimed_by` releases the reservation in the same write.
- The capacity gate and in-flight tally described below are the *destination*
  side; source ownership is the reservation substrate. See
  [reservations.md](reservations.md) for the reserve→claim→confirm lifecycle.

### Queue reason, code and cause

Three columns on `orders` say why an order is sitting in `queued`:
`queue_reason` (the operator sentence), `queue_code` (the wire vocabulary,
`protocol.QueueCode`) and `queue_cause` (Core's engineer vocabulary,
`dispatch.QueueCause`, which never crosses the wire). Every park writes them —
the capacity gate's verdict is one writer among many — and `Park` refuses
`→ queued` with no code (`WaitWithoutCause`). A transition into a moving status
clears all three in the same write as the status. The reason surfaces in
order-status responses and the operator HMI's "IN QUEUE" label; what ends each
cause's wait is [queue-releasers.md](queue-releasers.md).

---

## Bin Lifecycle

```
Source slot   pickup        dropoff       Dest slot
   bin   ───────FINISHED──▶ _TRANSIT ────FINISHED──▶ bin
                            (claimed_by                (claimed_by
                             preserved)                 cleared)
```

`bin.NodeID` transitions to `_TRANSIT` on the first per-block FINISHED
event for a pickup block. `bin.claimed_by` stays set throughout transit
so the order still owns the bin — clearing the claim is what creates the
anomaly state on the failure path.

### Per-Pickup Signal Source

The RDS poller reads `OrderDetail.Blocks[i].State` on every poll. When
a block transitions to FINISHED while the parent order is still
RUNNING, the poller fires `EventBlockCompleted`. The engine handler
classifies the block by `BinTask` (Load / Unload / Wait / Script /
navigation) and routes pickup-shaped blocks through the bin service's lift
(`BinService.Lift`), which moves the bin to `_TRANSIT` and writes the Edge's
`BinPickedUp` notice in the same transaction; the handler then emits
`EventBinEnteredTransit`.

Idempotent against vendor retries: a lift of a bin already at `_TRANSIT` moves
nothing. The poller suppresses duplicate emits via a sticky-state diff.

### Which cart a pickup lifts

An order's carts are its `order_bins` rows, one per lift: the cart, the plan
step that picks it up and the plan step that sets it down
(`domain.Cart`), written where the order comes to hold the cart: the issue, the
scanner's soft-acquire, an allocation (`ReplaceOrderBins`). A pickup block lifts
the order's cart whose pick step happens at the
block's location (`resolvePickupBinRow`, reading the step's node through
`domain.StepNode`) and that still stands there. A pickup at a node where the plan
grew a pickup after its rows were written lifts the one cart the order has
claimed there and writes that lift's row (`RecordAppendedLift`). A block no row
names lifts nothing: the cart moves to its drop at whole-order FINISHED.

---

## Dropoff Capacity Gate

`dispatch.CheckDropoffCapacity(db, deliveryNode, excludeOrderID) → (blocked, CapacityBlock)`
is the single predicate every dispatch path that places a bin runs before it
sends the order. The gate itself is `plan.DropoffCapacity`; a blocked verdict
carries the cause (`CapacityBlock.Cause`) and the sentence's params, and the
caller parks through the shared formatter with that cause, never a tag of its
own.

```
empty deliveryNode             → not blocked
no such node (typo'd name)     → not blocked (clearer error at dispatch)
node lookup errored            → blocked: capacity-check-failed
synthetic LANE / _TRANSIT      → not blocked (lane planners gate; transit is
                                              never a real dropoff)
synthetic NGRP                 → walk children; blocked iff every enabled
                                  non-synthetic child is occupied or has
                                  an in-flight order inbound (ngrp-full),
                                  or the group is at its level (ngrp-at-level)
concrete node, bin present     → blocked: dropoff-occupied
concrete node, in-flight bound → blocked: dropoff-inflight
concrete node, free            → not blocked
```

`excludeOrderID` excludes the calling order's own row from the
in-flight count, which counts orders holding a claimed bin
(`orders.InFlightForDropoffSQL`). Callers dispatching an order pass its
`order.ID`, so an order re-checking after it has claimed does not count itself.
Callers with no order of their own to exclude pass 0: the preview route.

The gate is consulted at:

- `PlanningService.decideTransport` — the plain family's planner (retrieve,
  retrieve_empty, move)
- `Scanner.fulfill` — every acquire of a plain order, queued replays included
- complex dispatch (`complex_dispatch.go`) — the delivery node when it is a
  storage dropoff (`isStorageDropoff`), and each declared-exclusive dropoff step
- the loader placements (`loader_place.go`) and placement by contents
  (`place_by_contents.go`)
- `Dispatcher.PreviewDropoffCapacity` — the read-only preview, with 0

NGRP saturation: the gate walks children rather than deferring to the
resolver. The resolver only picks free children at dispatch and fails
on saturation; queueing-on-saturation requires gating before resolution.

A dig's shuffle walk does not ask this gate: it asks `nodes.SlotTakeableSQL`,
which also reads slot reservations.

### Two intakes for one dropoff

The ladder an order climbs (`pending` → `dispatched`, or `pending` → `queued`
with a cause) and the doors that hold the scanner's lock across their issue and
one acquire are [order-lifecycle.md](order-lifecycle.md). What makes the gate
safe under that lock: the in-flight tally counts an order that HOLDS a claimed
bin bound for the dropoff (`orders.InFlightForDropoffSQL` — a holder, not a
status), so of two concurrent intakes for one dropoff the first to claim is
counted and the second sees in-flight=1 and waits.

### What re-asks a queued order

The events that re-run the fulfillment scanner for a waiting order, and the
periodic floor behind them, are the `acquiring` row of the wait-population
table in [queue-releasers.md](queue-releasers.md) (generated from
`waitPopulations`). `EventBinEnteredTransit` is among them: a downstream order
can dispatch as soon as the upstream robot lifts its bin off the source, not
when the upstream order delivers.

---

## Operator Surfaces

### HMI duplicate prevention

While a leg works a cell, the station card (`cellCardAction`) offers no new
request; its disabled label says why:

- `IN QUEUE: <reason>` — Core holds the order, with its queue reason
- `ROBOT IN TRANSIT` — order dispatched, robot moving
- `WAITING FOR OTHER ROBOT` — a pair released as one, not yet releasable: the
  label says how many legs are parked, names the leg to watch, and lists every
  distinct cause across the pair (`pairWaitingLabel`)

### Anomaly recovery

The operator dashboard lists bins parked at `_TRANSIT` with no live
claim. Recovery is "I found this bin physically; it's at node X" —
moves the bin to the chosen physical node, clears `anomaly_at`, records
the action in `recovery_actions` with the operator's identity.

Validation: the recovery destination must be physical (not synthetic,
not `_TRANSIT`) and currently empty. No partial-occupancy override.

---

## API Surface

```
GET  /api/dispatch/preview-capacity?node=NAME    → {blocked, reason, delivery_node}
GET  /api/dispatch/anomalies                     → [{...bin row...}, ...]
POST /api/dispatch/clear-anomaly                 → {bin_id, to_node_id}
```

`order.queue_reason` is exposed on standard order-status endpoints
(`/api/orders`, `/api/orders/detail`).

---

## Schema

| Column | Purpose |
|---|---|
| `bins.anomaly_at TIMESTAMPTZ NULL` | Optional sort/observability for anomaly view (v15) |
| `bins.anomaly_note TEXT` | Where the robot carrying a stranded bin last was (v96) |
| `orders.queue_reason TEXT NOT NULL DEFAULT ''` | The operator sentence for why an order is queued (v16) |
| `orders.queue_code`, `orders.queue_cause` | The wait's wire code and Core cause, NULL when the order is not waiting |
| `order_bins (order_id, bin_id, pick_step, drop_step)` | An order's carts, one row per lift |
| `nodes` row `name='_TRANSIT', is_synthetic=true` | The transit lane |

---

## Predicate Inventory

A few load-bearing predicates that govern this surface:

```
"this order still owns the bin"     bin.claimed_by == order.id
                                    Note: a pending/queued order may own the bin as a
                                    pending RESERVATION before claimed_by is set.

"bin is in flight"                  bin.NodeID == _TRANSIT_id

"bin can be re-claimed by another"  bin.claimed_by IS NULL AND
                                    no active reservation holds it AND
                                    bin's node has is_synthetic=false

"anomaly state"                     bin.NodeID == _TRANSIT_id AND
                                    bin.claimed_by IS NULL

"slot is free for new dispatch"     CountBinsByNode(slot) == 0 AND
                                    CountInFlightByDeliveryNode(slot) == 0
                                    (excluding caller's own order)
```

Ownership is the claim, never the bin's node: a bin in transit is on
`_TRANSIT`, not at its source, so a node-based ownership test would read every
carried bin as lost.
