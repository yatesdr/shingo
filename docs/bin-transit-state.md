# Bin Transit State

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

### Queue Reason

Free-text column on `orders` describing why an order is sitting in
`queued` status. Set by the capacity gate when an order is held back;
cleared on successful dispatch. Surfaced in order-status responses
and the operator HMI's "IN QUEUE" label.

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
navigation) and routes pickup-shaped blocks through `MoveToTransit`.

Idempotent against vendor retries: a `MoveToTransit` on a bin already
at `_TRANSIT` is a no-op. The poller suppresses duplicate emits via a
sticky-state diff.

### Multi-Pickup Orders

Complex orders with multiple pickups (single-robot swap, two-robot
swap, press-index) populate the `order_bins` junction table at intake.
Per-pickup transit transitions look up the right bin via
`(order_id, step_index, NodeName)`; single-pickup orders fall back to
`order.BinID`.

---

## Dropoff Capacity Gate

`dispatch.CheckDropoffCapacity(deliveryNode, excludeOrderID) → (blocked, reason)`
is the single predicate every planner-mediated dispatch path runs
before transitioning an order out of `queued`.

```
empty deliveryNode             → not blocked
lookup error (typo'd name)     → not blocked (clearer error at dispatch)
synthetic LANE / _TRANSIT      → not blocked (lane planners gate; transit is
                                              never a real dropoff)
synthetic NGRP                 → walk children; blocked iff every enabled
                                  non-synthetic child is occupied or has
                                  in-flight order inbound
concrete node, bin present     → blocked: "destination N occupied (X bin(s))"
concrete node, in-flight bound → blocked: "destination N has X in-flight order(s) inbound"
concrete node, free            → not blocked
```

`excludeOrderID` excludes the calling order's own row from the
in-flight count. Planner callers pass `order.ID` (the order's status is
`pending` or `sourcing` during planning, both of which the count would
otherwise tally against). Scanner callers pass 0 since `queued` is
already excluded from the count.

The gate is consulted at:

- `Dispatcher.HandleComplexOrderRequest` — fresh complex-order intake
- `PlanningService.planRetrieve` — simple retrieves (and `retrieve_empty`
  transitively)
- `PlanningService.planMove` — manual moves, side-cycle L2/U2,
  auto-return when re-enabled, NGRP-return moves
- `Scanner.tryFulfill` — every queued-order replay

NGRP saturation: the gate walks children rather than deferring to the
resolver. The resolver only picks free children at dispatch and fails
on saturation; queueing-on-saturation requires gating before resolution.

---

## Status-First Queueing

Complex orders are created in `queued` status by intake, not `pending`.
The fulfillment scanner is the single sync point that runs the capacity
gate and transitions queued → sourcing → dispatched.

```
HandleComplexOrderRequest:                  Scanner.tryFulfill:
  resolve steps                                (scan-mu serialized)
  create order (status=queued)                 capacity gate
  ack edge                                     ├─ blocked: set queue_reason, leave queued
  emit EventOrderQueued ───────synchronous───▶ └─ green: DispatchPreparedComplex
                                                         claim bins
                                                         status → sourcing → dispatched
                                                         ship blocks to fleet
```

`scan-mu` makes the queued → sourcing transition the single point of
serialization. Two concurrent intakes for the same dropoff can't both
pass the gate: the first transitions to `sourcing` (counted by the
in-flight tally), the second sees in-flight=1 and re-queues.

Simple retrieves and moves use the existing planner-returns-`Queued`
path — they aren't routed through the scanner because their
single-pickup shape doesn't need the same serialization (the
self-exclusion in the in-flight count is enough).

---

## Queue Retrigger Events

The fulfillment scanner subscribes to events that signal capacity may
have changed:

| Event | Source | Effect |
|---|---|---|
| `EventBinUpdated` | bin moves, status changes | retrigger scan |
| `EventOrderCompleted` | order confirmed | retrigger scan |
| `EventOrderCancelled` | order cancelled | retrigger scan |
| `EventOrderFailed` | order failed | retrigger scan |
| `EventBinEnteredTransit` | per-pickup signal | retrigger scan (source slot freed) |
| `EventOrderQueued` | fresh complex intake | synchronous scan |

Slot vacancy from any of these unblocks queued orders without waiting
for the original order to fully complete. The pickup-fires-source-free
path matters in production: a downstream order can dispatch as soon as
the upstream robot leaves its source, not when the upstream order
delivers.

---

## Operator Surfaces

### HMI duplicate prevention

When any non-staged/non-delivered active order exists for a process
node, the operator HMI's swap/finalize button renders disabled. The
label tracks status:

- `IN QUEUE` — order held by capacity gate
- `ROBOT IN TRANSIT` — order dispatched, robot moving
- `WAITING FOR OTHER ROBOT` — two-robot swap, peer not yet in position

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
| `bins.anomaly_at TIMESTAMPTZ NULL` | Optional sort/observability for anomaly view |
| `orders.queue_reason TEXT NOT NULL DEFAULT ''` | Why an order is in queued status |
| `nodes` row `name='_TRANSIT', is_synthetic=true` | The transit lane |

Migrations v15 + v16 handle existing databases idempotently.

---

## Predicate Inventory

A few load-bearing predicates that govern this surface:

```
"this order still owns the bin"     bin.claimed_by == order.id
                                    (was: bin.NodeID == sourceNode.ID — broke under transit)

"bin is in flight"                  bin.NodeID == _TRANSIT_id

"bin can be re-claimed by another"  bin.claimed_by IS NULL AND
                                    bin's node has is_synthetic=false

"anomaly state"                     bin.NodeID == _TRANSIT_id AND
                                    bin.claimed_by IS NULL

"slot is free for new dispatch"     CountBinsByNode(slot) == 0 AND
                                    CountInFlightByDeliveryNode(slot) == 0
                                    (excluding caller's own order)
```

The teleport-bug guard at `engine/wiring_completion.go` and the release-
time fallback in `dispatch/complex.go` both use the claim-based
predicate; the node-based predicate is only correct for pre-transit
semantics and shouldn't be reintroduced.
