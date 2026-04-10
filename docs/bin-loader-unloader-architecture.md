# Bin Loader / Unloader

This document covers the bin loader and unloader workflow for manual material staging at operator stations. Loaders bring material into the AMR system; unloaders remove finished goods.

---

## Concepts

### Loader vs Unloader

A bin_loader claim has a **mode** field: `loader` (default) or `unloader`. Each node has exactly one mode, set at claim configuration time.

- **Loader (produce):** Forklift operator swaps empty bins for full bins that enter the AMR system. Orders use `retrieveEmpty=true` -- the system delivers an empty bin, the operator loads it, and the full bin moves to storage.
- **Unloader (consume):** Forklift operator removes finished goods from the AMR system. Orders use `retrieveEmpty=false` -- the system delivers a full FG bin, the operator unloads it, and the empty bin returns to storage.

Single role (`bin_loader`) with mode avoids duplicating role checks. Mode flips `retrieveEmpty` and determines HMI layout.

### Kanban-Driven Demand

There is no manual configuration of target levels or fixed ratios. Downstream consumption drives upstream replenishment:

- **Loader signal:** A weld cell empties a bin (consumes last part, bin returns to storage empty). The empty bin returning IS the kanban card. This triggers a loader order for that payload.
- **Unloader signal:** A weld cell produces a full FG bin and it goes to storage. The full bin arriving IS the kanban card. This triggers an unloader order.

If weld cells burn through Part ABC fast, more empties return, more loader orders get created. If Part DEF slows down, fewer empties, fewer orders. Self-balancing.

### Multi-Order Queue

Bin loader/unloader nodes allow multiple queued orders simultaneously. This replaces the single-order constraint used by standard consume/produce nodes.

Core's fulfillment scanner already iterates queued orders FIFO, skips ones it can't fulfill (no stock) or can't deliver (node busy), and dispatches the first one it can. No Core queue logic changes needed.

If Part A is stuck (no empty bins available), Parts B and C keep flowing. The scanner skips A and dispatches B or C.

---

## Operator Workflow

Loader and unloader operators are forklift (HILO) drivers. The physical workflow:

**Loader cycle:**
1. Robot delivers empty bin to loader node. Auto-confirm fires.
2. Operator sees demand queue on HMI -- taps the payload card for the material they have stock for.
3. Forklift pulls empty off the AMR, brings a full bin of the selected payload (same bin type, not same physical bin).
4. Operator confirms load on HMI. Edge calls LoadBin, Core sets manifest.
5. If OutboundDestination is configured, Edge creates a move order. Robot takes the full bin to storage.
6. Node vacant -- scanner dispatches the next fulfillable order.

**Unloader cycle:**
1. Robot delivers full FG bin to unloader node. Auto-confirm fires.
2. Operator sees which payload arrived. Taps the card.
3. Forklift removes full bin, takes product to staging, brings back an empty of the same type.
4. Operator confirms unload (CLEAR BIN). Core clears manifest, UOP resets to 0.
5. Edge creates move order for the empty bin back to storage.
6. Node vacant -- scanner dispatches next full FG bin from queue.

**Auto-confirm is mandatory** for bin_loader claims. Robots auto-confirm delivery -- the operator's load/unload action IS the acknowledgement. Enforced in claim config: when role is `bin_loader`, `auto_confirm` is always true.

**RDS dispatch sequencing:** RDS will not send a second robot to a node while one is still there. The multi-order queue means the next order is queued and ready in Core, so the moment the first robot clears, Core immediately dispatches.

**Skipping payloads:** If the operator has no stock for the requested payload, they skip it and load a different payload that has demand. The skipped demand stays in the queue.

---

## HMI Design

### Payload Board (Single-Node Station)

When the station has a single `manual_swap` node, the HMI shows a full-screen payload board. Each configured payload is shown as a large card:

- **Cards with active demand** are colored and ordered by queue position (1 = oldest). Statuses: DELIVERED (bin at node, tappable), IN TRANSIT (robot en route), QUEUED (waiting for robot).
- **Cards with no demand** are greyed out and non-actionable. The operator cannot load a payload without demand -- this protects storage capacity.
- **Bin info** is shown in the header bar: which bin is at the node, its type, and UOP state.

Tapping a DELIVERED card opens an inline confirm bar with quantity and lot fields.

### Node Grid (Multi-Node Station)

When the station has multiple nodes, the HMI shows a grid of node tiles. Tapping a node opens a modal overlay with the same demand card layout as the payload board.

### Data Source

The demand queue comes from `ListActiveOrdersByProcessNode` -- orders in non-terminal status (queued, acknowledged, in_transit, delivered) for this node. Each order has a payload code. The HMI groups by payload, shows status, and orders by creation time.

The full list of allowed payloads comes from the claim's `allowed_payload_codes`. Any payload in that list without a matching active order is shown greyed out.

---

## Kanban Demand Wiring (Core-Side)

Core is the natural home for the demand signal. `BinUpdatedEvent` already fires on bin movements. Core has system-wide bin visibility.

### Demand Registry

Core maintains a `demand_registry` table mapping payload codes to loader/unloader station addresses. Edge syncs this via `ClaimSync` messages sent after registration:

```
demand_registry
  station_id    TEXT     -- Edge station ID
  core_node_name TEXT   -- delivery node dot-name
  role          TEXT     -- "produce" (loader) or "consume" (unloader)
  payload_code  TEXT     -- allowed payload
```

`ClaimSync` is sent on Edge startup (after registration ack) and whenever claim config changes. Core upserts the registry on receipt.

### Event Flow

1. Bin moves at a storage node (BinUpdatedEvent, action "moved").
2. `handleKanbanDemand` checks whether the bin left or arrived at a storage slot:
   - **Bin left storage** (supply decreased): send `DemandSignal` with role "produce" to matching loader stations.
   - **Bin arrived at storage** (supply increased): send `DemandSignal` with role "consume" to matching unloader stations.
3. Storage slot detection: `isStorageSlot` checks if the node's parent has `NodeTypeCode == "LANE"`.
4. `sendDemandSignals` looks up the demand registry by payload code and role, sends a `DemandSignal` envelope to each matching Edge station.
5. Edge `HandleDemandSignal` finds the matching `manual_swap` node by `CoreNodeName`, calls `tryAutoRequest` to create orders for payloads that don't already have pending orders.

### Deduplication

`tryAutoRequest` queries existing orders before creating new ones. Only creates orders for payloads where no non-terminal order exists. Core's `UNIQUE(edge_uuid)` constraint catches any remaining duplicates from at-least-once delivery.

### Startup Sweep

On Edge startup, `SendClaimSync` runs after registration ack (not during `eng.Start()`, because `sendFn` isn't wired yet at that point). Edge also runs `tryAutoRequest` for all bin_loader nodes to pick up demand that arrived while offline.

---

## Safety Guards

### One Robot at a Time

`CountInFlightOrdersByDeliveryNode` -- before dispatching, the scanner checks if there's already a non-queued, non-terminal order targeting this delivery node. If yes, skip. Prevents two robots heading to the same node.

### Bin-Occupied Guard

In `tryFulfill`, after the in-flight check: resolve delivery node name via `GetNodeByDotName`, call `CountBinsByNode(nodeID)`. If count > 0, skip. A bin is physically at the node -- the operator hasn't cleared it yet.

This prevents dispatching while the operator is still working:
1. Order A delivered, operator unloads, confirms. Order A is terminal (excluded from in-flight count).
2. Scanner sees 0 in-flight, but the empty bin is still physically on the node (hasn't moved out yet).
3. Bin-occupied guard catches this and skips.

### Payload Validation (Hard Rejection)

HMI only shows payloads with active demand as actionable. Server-side enforcement in `LoadBin`: after the allowed-payload check, query active orders for the node and verify at least one non-terminal order exists. DB-down returns an error rather than a false rejection.

### Edge SQLite Transaction Safety

`tryAutoRequest` wraps check+create in a `BEGIN IMMEDIATE` transaction. Serializes concurrent access at the SQLite level, preventing duplicate orders when two order completions fire simultaneously.

---

## Edge State Tracking

Standard consume/produce nodes use `ActiveOrderID`/`StagedOrderID` on `ProcessNodeRuntimeState` (serial, one order at a time). Bin loader/unloader nodes skip these slots entirely and query the orders table directly.

`CanAcceptOrders` has role-based branching: for `bin_loader` role, it queries the orders table and allows multiple non-terminal orders. Changeover check still applies.

No schema changes to `ProcessNodeRuntimeState`. The serial model for consume/produce is untouched.

---

## Protocol

### ClaimSync (Edge to Core)

Subject: `claim.sync`

```go
type ClaimSync struct {
    StationID string           `json:"station_id"`
    Claims    []ClaimSyncEntry `json:"claims"`
}

type ClaimSyncEntry struct {
    CoreNodeName        string   `json:"core_node_name"`
    Role                string   `json:"role"`
    AllowedPayloadCodes []string `json:"allowed_payload_codes"`
    OutboundDestination string   `json:"outbound_destination,omitempty"`
}
```

### DemandSignal (Core to Edge)

Subject: `demand.signal`

```go
type DemandSignal struct {
    CoreNodeName string `json:"core_node_name"`
    PayloadCode  string `json:"payload_code"`
    Role         string `json:"role"`
    Reason       string `json:"reason"`
}
```

### Existing (Unchanged)

- `OrderRequest` with `RetrieveEmpty` bool -- same protocol, just more orders queued per node.
- `LoadBin`, `ClearBin` HTTP endpoints unchanged.
- `BinUpdatedEvent` actions unchanged.

---

## Implementation Files

### Core

| File | Change |
|------|--------|
| `engine/wiring.go` | `handleKanbanDemand` subscription on BinUpdatedEvent. `isStorageSlot` (parent LANE check). `sendDemandSignals` (demand registry lookup, DemandSignal envelope to Edge). |
| `messaging/core_data_service.go` | `HandleClaimSync` -- upserts demand_registry on receipt. `SendDataToEdge` for DemandSignal delivery. |
| `store/schema_postgres.go` | `demand_registry` table. `UNIQUE INDEX` on `orders.edge_uuid`. |
| `store/demand_registry.go` | `UpsertDemandRegistry`, `LookupDemandRegistry` queries. |
| `store/orders.go` | `ON CONFLICT (edge_uuid) DO NOTHING` in CreateOrder. |
| `dispatch/fulfillment_scanner.go` | Bin-occupied guard in `tryFulfill` (~8-10 lines). |

### Edge

| File | Change |
|------|--------|
| `engine/operator_stations.go` | `HandleDemandSignal` (finds matching node, calls tryAutoRequest). `SendClaimSync` (builds ClaimSyncEntry list from active claims). `RequestFullBin` (mirrors RequestEmptyBin with retrieveEmpty=false). `tryAutoRequest` updated for multi-payload demand with BEGIN IMMEDIATE. |
| `engine/wiring.go` | TypeRetrieve handler for bin_loader (UOP=0, clears runtime). TypeMove completion triggers tryAutoRequest. |
| `cmd/shingoedge/main.go` | `SendClaimSync` wired to registration ack callback. `SetDemandSignalHandler` wired to engine. |
| `messaging/edge_handler.go` | `onDemandSignal` callback, `SetDemandSignalHandler`, SubjectDemandSignal case in HandleData. |
| `store/style_node_claims.go` | `mode` column (loader/unloader), `AllowedPayloads()` method. |
| `store/schema.go` | Migration for mode column on style_node_claims. |
| `store/orders.go` | `payload_code` column on orders. `ListActiveOrdersByProcessNode` query. |
| `www/static/operator-station/operator.js` | Demand queue cards, payload board view, status labels (DELIVERED / IN TRANSIT / QUEUED / NO DEMAND). |
| `www/static/js/pages/processes.js` | Mode selector (loader/unloader) on claim config form. |
| `www/templates/processes.html` | Mode dropdown in claim form. |

### Protocol

| File | Change |
|------|--------|
| `protocol/types.go` | `SubjectClaimSync`, `SubjectDemandSignal` constants. |
| `protocol/payloads.go` | `ClaimSync`, `ClaimSyncEntry`, `DemandSignal` structs. |

---

## What Doesn't Change

- **Core queue logic:** FulfillmentScanner already iterates queued orders and skips unfulfillable ones.
- **Core bin tracking:** ApplyBinArrival, ClearBinManifest, MoveBin all work as-is.
- **Core dispatch:** `planRetrieve` and `planRetrieveEmpty` already handle both directions.
- **Consume/produce nodes:** Serial flow with ActiveOrderID/StagedOrderID is unchanged.
- **Fleet integration:** No changes to robot dispatching or status mapping.

---

## Bin Identity Model

The system tracks bin slots, not individual physical bins by serial number. When a forklift operator swaps a bin, the system tracks "there is a bin at this node" (occupied) or "there is no bin" (vacant). The manifest is associated with the bin record in Core's `bins` table, but the physical identity is the bin's label, not a tracked serial.

When the operator loads DEF into the bin at SMN_001, the system sets the manifest on whatever bin record Core has for that node. The operator could have swapped the physical container -- the system doesn't know or care. What matters is the manifest (payload + UOP count) and the location.

---

## Edge Cases

**Operator has no stock for requested payload:** Skip it, load a different payload with demand. The skipped payload's demand stays in the queue.

**Core restart:** Fulfillment scanner runs on startup, picks up all queued orders from before shutdown.

**Edge restart:** `SendClaimSync` runs after registration ack. `tryAutoRequest` runs for all bin_loader nodes to catch missed demand.

**Concurrent tryAutoRequest:** BEGIN IMMEDIATE serializes at SQLite level. Core UNIQUE(edge_uuid) catches any remaining duplicates.

**No bins ever available:** Order stays queued indefinitely. Operator sees QUEUED status, can cancel.

**Future modes:** Hand loading stations and decanter stations could reuse the same infrastructure (demand queue, payload cards, hard rejection) with different cycle behaviors. The mode field accommodates future values like `hand_load` or `decanter`.
