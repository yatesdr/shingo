# Queued Order Fulfillment

## Problem

When Core can't immediately find a bin to fulfill an order (empty or loaded), it fails the order. Edge sees the failure, the operator sees an error, and must manually retry. With multiple nodes competing for scarce bins, this creates races, noise, and operator frustration.

## Solution

Core holds unfulfillable orders in a `queued` state instead of failing them. When inventory becomes available, Core fulfills the oldest matching queued order automatically -- FIFO, no races, no operator intervention.

## Scope

All order types, not just retrieve_empty. A retrieve for payload "NF1-Knockdown" that finds no source bins queues the same way. This makes queuing a first-class part of the order lifecycle, not a bin_loader special case.

## Order Lifecycle

```
Current:   pending -> sourcing -> [found] -> dispatched -> in_transit -> delivered -> confirmed
                               -> [not found] -> FAILED

Proposed:  pending -> sourcing -> [found] -> dispatched -> in_transit -> delivered -> confirmed
                               -> [not found] -> QUEUED -> [bin available] -> sourcing -> dispatched -> ...
                               -> [not found] -> QUEUED -> [cancelled by operator] -> cancelled
```

## New Status: `queued`

- Protocol: `StatusQueued = "queued"` in `protocol/types.go`
- Meaning: Core accepted the order, cannot fulfill it now, will fulfill it when inventory is available
- Persistence: stored in orders table, survives Core restarts

## Core Changes

### 1. Planning Service -- Queue Instead of Fail

File: `dispatch/planning_service.go`

`planRetrieve` and `planRetrieveEmpty` return `PlanningResult{Queued: true}` instead of `planningError` when no bin is found.

Applies to:
- `planRetrieveEmpty` -- no empty compatible bin found
- `planRetrieve` -- no source bin found (FIFO search fails, NGRP resolver fails)

Does NOT apply to:
- Node not found errors (bad delivery/pickup node -- config errors, should still fail)
- Claim failures (race condition on bin claim)

### 2. Dispatcher -- Handle Queued Result

File: `dispatch/dispatcher.go`

`queueOrder` method:
1. `db.UpdateOrderStatus(order.ID, "queued", "awaiting inventory")`
2. `db.UpdateOrderPayloadCode(order.ID, payloadCode)` -- persist for scanner
3. Send `OrderUpdate{Status: "queued", Detail: "awaiting inventory"}` reply to Edge
4. Emit `EventOrderQueued` for SSE/logging

### 3. Payload Code on Orders (New Column)

File: `store/orders.go`

- Add `payload_code TEXT NOT NULL DEFAULT ''` column to orders table
- Migration: `ALTER TABLE orders ADD COLUMN payload_code`
- Persist at order creation in `CreateInboundOrder`

### 4. Fulfillment Scanner

File: `engine/fulfillment_scanner.go` (new)

**Trigger points** (event-driven):
- `EventBinUpdated` (action: "moved" to storage, "cleared") -- bin became available
- `EventOrderCompleted` -- completing an order unclaims a bin
- `EventOrderCancelled` -- unclaimed bin
- `EventOrderFailed` -- unclaimed bin

**Safety sweep:** Every 60 seconds, full scan. Catches anything events missed.

**Startup:** Run once on Core start to pick up queued orders from before shutdown.

**Concurrency:** Mutex-guarded. Only one scan at a time. Events during scan are coalesced.

**Algorithm:**
```
1. List all queued orders, sorted by created_at ASC (FIFO)
2. For each queued order:
   a. Get payloadCode from order
   b. Determine order type:
      - retrieve_empty: FindEmptyCompatibleBin(payloadCode, preferZone)
      - retrieve: FindSourceBinFIFO(payloadCode) or resolve via NGRP if pickup node set
   c. If no bin found: skip (stays queued)
   d. ClaimBin(bin.ID, order.ID): if fails (race), skip
   e. Update order: bin_id, pickup_node, status -> "sourcing"
   f. Dispatch to fleet via DispatchDirect
   g. Send OrderAck + OrderWaybill to Edge
   h. If fleet dispatch fails: unclaim bin, set back to queued
3. Return count fulfilled
```

**Node vacancy check:** Before fulfilling, verify the delivery node doesn't already have an active non-queued delivery in flight.

### 5. Reply to Edge on Queue

Core sends `OrderUpdate{Status: "queued", Detail: "awaiting inventory"}`.

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

When `tryAutoRequestEmpty` fires and Core queues:
- Edge sees order transition to `queued`
- Order stays on node runtime (`ActiveOrderID`)
- No retry loop -- order is alive, Core fulfills when possible
- If operator cancels, runtime clears, next vacancy trigger retries

## Edge Cases and Recovery

**Core restart:** Scanner runs on startup, picks up all queued orders.

**Edge restart:** Startup reconciliation asks Core for current status, gets `queued`, updates local state.

**Concurrent scans:** Mutex prevents races. Events coalesced.

**Two orders, one bin:** FIFO -- oldest wins. `ClaimBin` is atomic.

**Order cancelled mid-fulfill:** Scanner checks status before dispatch. Claim released if cancelled.

**Delivery node occupied:** Scanner skips, order stays queued. Next scan after delivery completes picks it up.

**Fleet dispatch fails:** Unclaim bin, set back to queued. Next scan retries.

**No bins ever available:** Order stays queued indefinitely. Operator sees "Awaiting Stock", can cancel.

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
