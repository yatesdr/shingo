# Queued Order Fulfillment

## Problem

When Core can't immediately find a bin to fulfill an order (empty or loaded), it fails the order. Edge sees the failure, the operator sees an error, and must manually retry. With multiple nodes competing for scarce bins, this creates races, noise, and operator frustration.

## Solution

Core holds unfulfillable orders in a `queued` state instead of failing them. When inventory becomes available, Core fulfills the oldest matching queued order automatically -- FIFO, no races, no operator intervention.

## Scope

All order types, not just retrieve_empty. A retrieve for payload "NF1-Knockdown" that finds no source bins queues the same way. This makes queuing a first-class part of the order lifecycle, not a bin_loader special case.

## Order Lifecycle

```
pending -> (acquiring) -> [whole hand] -> dispatched -> in_transit -> delivered -> confirmed
                       -> [not whole]  -> QUEUED (with a cause) -> [hand whole] -> dispatched -> ...
                                                                -> [cancelled by operator] -> cancelled
dispatched -> [fleet refused the create] -> QUEUED (fleet_unavailable) -> ...
```

Every order is issued `pending`. **Acquiring** is the act between `pending` and
`dispatched` — asking for the whole hand — never a stored status. When the hand
is whole the order goes `pending → dispatched` in one transaction; otherwise it
parks `pending → queued`, with its cause written on the row first. `queued` is
the one place an order rests short of the fleet, and it always carries a cause.
(`sourcing` was a status before C1; it is history now — see
`[[order-lifecycle]]`.)

`pending` and `queued` together form the **acquiring** set — the orders the
fulfillment scanner may ask. `IsAcquiring(s)` / `AcquiringStatusSQLList()` in
`protocol/status.go` define it; the scanner's re-check and the complex dispatch
entry guard both gate on it. `IsWaiting(s)` is `queued` alone. See
`[[order-builder-dispatch]]`.

## New Status: `queued`

- Protocol: `StatusQueued = "queued"` in `protocol/status.go`
- Meaning: Core asked for the order's whole hand and could not have it; it waits, with the cause on its row (`queue_code`, `queue_reason`), and is asked again
- Always carries a cause: `LifecycleService.transition` refuses `→ queued` on a row with an empty code (`WaitWithoutCause`)
- Persistence: stored in orders table, survives Core restarts

## Core Changes

### 1. Planning Service -- Queue Instead of Fail

File: `dispatch/planning_service.go`

`planRetrieve` and `planRetrieveEmpty` return `PlanningResult{Queued: true}` instead of `planningError` when no bin is found.

Applies to:
- `planRetrieveEmpty` -- no empty compatible bin found
- `planRetrieve` -- no source bin found (FIFO search fails, NGRP resolver fails)

Does NOT apply to:
- Node not found errors (bad delivery/source node -- config errors, should still fail)
- Claim failures (race condition on bin claim)

### 2. Doors -- Issue, Apply, One Acquire

Files: `dispatch/door.go`, `fulfillment/scanner.go`

Each door (`HandleOrderRequest`, complex intake, loader replenishment) holds the
scanner's lock across the order's issue, its apply and one acquire, so a periodic
pass never acquires a half-applied row (a carried-bin recovery holds none: it is
issued whole, its robot pinned in the row, and dispatched in its own call):

1. Issue: the INSERT writes `pending`.
2. Apply: the door's own writes (remaining UOP, resolved destination).
3. One acquire (`AcquireLocked`): the order is dispatched, or parked
   `pending → queued` under the cause its arm wrote, and its station is told
   once with that cause.

A door whose decision is already a wait (dropoff gate, finder wait, unresolved
group) parks the order itself (`ParkAndTell`) instead of asking again. The
station is not told `queued` for an order that never waited, and a door never
emits `EventOrderQueued`.

### 3. Payload Code on Orders (New Column)

File: `store/orders.go`

- Add `payload_code TEXT NOT NULL DEFAULT ''` column to orders table
- Migration: `ALTER TABLE orders ADD COLUMN payload_code`
- Persist at order creation in `CreateInboundOrder`

### 4. Fulfillment Scanner

File: `fulfillment/scanner.go`

**Trigger points** (event-driven):
- `EventBinUpdated` (action: "moved" to storage, "cleared") -- bin became available
- `EventOrderCompleted` -- completing an order releases a bin's reservation
- `EventOrderCancelled` -- reservation released
- `EventOrderFailed` -- reservation released
- `EventOrderSkipped` -- reservation released
- `EventBinEnteredTransit` -- a move committed, freeing upstream state
- plus the synchronous `EventOrderQueued` trigger, emitted by a caller outside any door that parked an order (the doors acquire their own order instead)

**Safety sweep:** Every 60 seconds, full scan. Catches anything events missed.

**Startup:** Run once on Core start to pick up queued orders from before shutdown.

**Concurrency:** Mutex-guarded (`scan-mu`). Only one scan at a time. Events during scan are coalesced.

**How fulfillment works (reservation reconcile, not atomic claim):**
Source resolution is **not** inlined in the scanner. Both intake planning and
scanner replay call the shared `dispatch.SourceFinder` (`source_finder.go`),
which runs the same tier cascade for both paths so replay cannot drift from
intake (see `[[order-builder-dispatch]]`). The actual hold on a source is a
**reservation**, set via `Acquire` at plan time and `Confirm` at dispatch time;
a `queued` order retries by **reconciling** its existing reservations
(keep held, release moved, acquire newly needed) rather than re-claiming from
scratch. See `[[reservations]]`.

```
1. List acquiring orders ({pending, queued}, top-level only), sorted by priority DESC, created_at ASC
2. For each order:
   a. Resolve source via SourceFinder (shared with intake)
   b. Allocator.reconcile: keep/release/acquire reservations vs the plan's needs
   c. If a source can't be secured: park (`pending → queued` under the cause the arm wrote; a `queued` order stays); retry next tick
   d. Confirm reservations + hard-claim via the reservation-guarded seatbelt (ConfirmForDispatch)
   e. Dispatch to fleet
   f. If fleet dispatch fails: take the claims off, demote the reservations to pending, keep the bin pointer, park `dispatched → queued` under `fleet_unavailable` or the ngrp carve-out (DemoteAfterFleetRefusal); the next tick re-confirms
3. Return count fulfilled
```

**Node vacancy check:** Before fulfilling, the scanner's dropoff gate verifies that the delivery node holds no bin and that no other order delivering there holds a claimed bin (`orders.InFlightForDropoffSQL` — a holder, not a status). An order that has only reserved, or is parked, counts for nothing; the gate runs again before every dispatch attempt, so the first order to claim goes and the others wait their turn (`TestDropoffGate_TwoShoppersForOneExclusiveDropoffOneGoes`, `TestFleetRecovery_TwoDemotedOrdersToOneLineOnlyOneGoes`).

### 5. Reply to Edge on Queue

A park sends one `OrderUpdate{Status: "queued"}` carrying the reason (`ParkAndTell` / `NotifyWaitChanged`); a later change of cause goes through the wait-change notify. An order the door dispatched is never told `queued`.

When fulfilled and dispatched, Edge receives the normal `order.ack` -> `order.waybill` -> `order.update(in_transit)` -> `order.delivered` flow.

### 6. Cancellation

Edge sends `order.cancel`. Core's existing `HandleOrderCancel` works:
- No vendor order ID -> skip fleet cancel
- No claimed bin -> skip unclaim
- Status -> cancelled, reply sent

### 7. SSE Events

- `EventOrderQueued` -> broadcast `order-update` with `type: "queued"`
- Existing `EventOrderStatusChanged` handles queued -> dispatched transition

### 8. Core Orders UI

- "queued" status badge: amber, "Awaiting Stock"
- Queued orders in active orders list
- Admin can cancel from orders page

## Edge Changes

### 1. New Status Constant

`StatusQueued = protocol.StatusQueued` in `orders/types.go`

Valid transitions:
```
StatusSubmitted -> StatusQueued     (Core couldn't find a bin)
StatusQueued -> StatusAcknowledged  (Core found a bin, dispatching)
StatusQueued -> StatusCancelled     (operator cancelled)
StatusQueued -> StatusFailed        (Core gave up)
```

### 2. Handle order.update with Queued Status

`messaging/edge_handler.go`: route `OrderUpdate` with `status=queued` to a proper status transition.

### 3. Operator Experience

| Node State | What Operator Sees | Actions Available |
|---|---|---|
| No bin, no order | "No bin" | Request Empty |
| No bin, queued order | "Awaiting Stock" (amber) | Cancel |
| No bin, order in transit | "Incoming" (blue) | -- |
| Bin present, empty | "EMPTY" | Load Bin |
| Bin present, loaded | Payload code shown | View, Load Bin, Clear Bin |

### 4. Auto-request Interaction

When an empty request fires and Core queues (the trigger named here, `tryAutoRequestEmpty`, is retired — the surviving one is the operator push):
- Edge sees order transition to `queued`
- Order stays on node runtime (`ActiveOrderID`)
- No retry loop -- order is alive, Core fulfills when possible
- If operator cancels, runtime clears, next vacancy trigger retries

## Edge Cases and Recovery

**Core restart:** Scanner runs on startup, picks up all queued orders.

**Edge restart:** Startup reconciliation asks Core for current status, gets `queued`, updates local state.

**Concurrent scans:** Mutex prevents races. Events coalesced.

**Two orders, one bin:** FIFO -- oldest wins. The loser loses the `Acquire` race
(`ErrReservationConflict` from the partial unique index) and requeues; the
scanner retries it next tick.

**Order cancelled mid-fulfill:** Scanner checks status before dispatch. Claim released if cancelled.

**Delivery node occupied:** Scanner skips, order stays queued. Next scan after delivery completes picks it up.

**Fleet dispatch fails:** Claims off, reservations demoted to pending, bin pointer kept, `dispatched → queued` under `fleet_unavailable`. Next scan retries.

**No bins ever available:** Order stays queued indefinitely. Operator sees "Awaiting Stock", can cancel.

## Why an order is waiting, and what will end it

The sections above describe the original queued-order mechanism. The lane
campaign generalized it: a queued order is one instance of a **machine-owned
wait**, and every such wait now has to declare how it ends.

### The doctrine

Every machine-owned wait has:

1. a named **event releaser** — the thing that, when it happens, ends this wait;
2. a periodic **floor** that re-evaluates it, for when that event does not fire;
3. a **record** when the floor is what freed it, rather than the event.

It is a table rather than prose because prose cannot be asserted. The defect that
motivated it was not a missing idea — the evaluator's own documentation said "a
dropped event costs only latency until the next firing." It was that nothing
checked whether a next firing could exist. Every individual comment was
defensible; nothing connected them.

### Where it lives

`shingo-core/dispatch/queue_cause.go` holds the `QueueCause` constants — the
named reason an order is waiting, written on its row. This is **Core vocabulary
and never crosses the wire**, which is what makes the values safe to rename in
bulk but not safe to re-spell casually: they are already written on rows in a
plant's orders table and are grouped by in forensic queries.

`shingo-core/dispatch/queue_releasers.go` holds two tables:

- `causeReleasers` — for each cause, the populations an order carrying it can be
  sitting in, and `what` should end the wait.
- `waitPopulations` — for each population, its owner, its re-driver, the events
  it is released by, and its floor.

Three totality tests keep them honest:

| Test | Asserts |
|---|---|
| `TestEveryQueueCauseHasAReleaser` | `causeReleasers` is total over the `QueueCause` constants |
| `TestEveryWaitPopulationHasBothPaths` | every population carries both an event set and a floor |
| `TestDeclaredReleaserEventsAreSubscribed` | the declared events are really subscribed to that population's re-driver |

### The floor-release histogram is a worklist

The floor records the cause an order was carrying when the **floor** — rather
than an event — freed it. `what` is the sentence that record prints: it says what
*should* have ended the wait, so the record reads as "this event did not fire"
rather than "something was slow."

So the histogram of floor releases grouped by cause is a **ranked worklist of
missing emitters**. That is the artifact an emitter hunt runs on, and it is the
main reason to look at this data at all.

### Honest entries only — the current findings

A cause whose row cannot be written truthfully carries a `finding` instead of a
plausible sentence. Four exist today, and they are not all defects:

| Cause | Finding | Is it a defect? |
|---|---|---|
| `CauseFleetRefusedCreate` | No event exists — nothing emits "the fleet became willing", so the floor is the only thing that re-asks | **No.** Absence-class, and the floor is the intended answer |
| `CauseHeldBinMissing` | No event releases it, and that is correct rather than a gap | **No.** Reasoned absence |
| `CauseLaneEntryError` | *(resolved 2026-08-17)* declared and never set — dead vocabulary; deleted | Was a defect, now gone |
| `CauseLaneLockRace` | *(resolved 2026-08-17)* shared the string `"lock-race"` with `CauseBinLockRace` and had no writer; deleted, leaving the value meaning exactly one fact | Was a defect, now gone |

Read the findings before trusting a histogram. Two of them say "this wait has no
event and that is fine"; a reader who assumes every cause has an event releaser
would draw the wrong conclusion from either.

## Implementation Sequence

1. Protocol -- add `StatusQueued`
2. Core store -- `payload_code` column, migration, new queries
3. Core dispatch -- modify planning service, `queueOrder`, `SendUpdate`
4. Core engine -- `FulfillmentScanner`, event wiring, startup scan
5. Core SSE/UI -- queued event broadcast, amber badge
6. Edge orders -- `StatusQueued`, valid transitions
7. Edge messaging -- handle `order.update` with queued status
8. Edge UI -- "Awaiting Stock" display

## Risk Assessment

**Low risk:** Protocol (additive), Edge status handling, SSE/UI.

**Medium risk:** Fulfillment scanner -- mitigated by mutex, atomic claims, existing DispatchDirect, startup recovery.

**Highest risk:** `planRetrieve` change for non-empty bins. Only queue on `no_source`/`no_empty_bin`, not config errors (`node_error`, `claim_failed`).
