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

Core maintains a `demand_registry` table mapping payload codes to loader/unloader station addresses. It is derived from the Core-owned `bin_loaders` aggregate (`BuildDemandRegistryFromAggregate`, run by the seed / `migrateloaders`):

```
demand_registry
  station_id    TEXT     -- Edge station ID
  core_node_name TEXT   -- delivery node dot-name
  role          TEXT     -- "produce" (loader) or "consume" (unloader)
  payload_code  TEXT     -- allowed payload
```

`ClaimSync` (the Edge→Core push of `style_node_claims`) is **retired**: Core owns loader config via the `bin_loaders` aggregate and derives `demand_registry` from it, syncing loader config down to the Edge through the node-list sync. `Engine.SendClaimSync` is a no-op, kept only so its call sites don't change.

### Event Flow

1. Bin moves at a storage node (BinUpdatedEvent, action "moved").
2. `handleKanbanDemand` checks whether the bin left or arrived at a storage slot:
   - **Bin left storage** (supply decreased): send `DemandSignal` with role "produce" to matching loader stations.
   - **Bin arrived at storage** (supply increased): send `DemandSignal` with role "consume" to matching unloader stations.
3. Storage slot detection: `isStorageSlot` checks if the node's parent has `NodeTypeCode == "LANE"`.
4. `sendDemandSignals` looks up the demand registry by payload code and role, sends a `DemandSignal` envelope to each matching Edge station.
5. Edge's demand-signal handler routes by role to the reservation seam — produce → `MaybeCreateLoaderEmptyIn` (L1 empty-in), consume → `MaybeCreateUnloaderFullIn` (U1 full-in) — which resolves the loader from the Core aggregate and creates orders for payloads in deficit, deduped by the never-2N seam.

### Deduplication

The reservation seam (`reserveLoaderBins`, below) counts in-flight orders across the loader's delivery set before firing, so a payload that already has a non-terminal order in flight creates nothing. Core's `UNIQUE(edge_uuid)` constraint catches any remaining duplicates from at-least-once delivery.

### Startup Sweep

On Edge startup, after registration ack, the auto-push sweeps `SweepPushLoaders` / `SweepPushUnloaders` offer every auto loader/unloader's payloads to the reservation seam, picking up demand that arrived while offline. (`SendClaimSync` is retired to a no-op — see Demand Registry above.)

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

### Reservation Seam (the never-2N guarantee)

Every loader empty-in (an L1 `retrieve_empty`) and every unloader full-in (a U1 retrieve of a full bin) is created through **one** chokepoint, `engine.reserveLoaderBins`. It owns the count→fire decision so a demand signal (Kafka), an operator request (HTTP), and the push sweep can never both pass the in-flight count and both fire. The invariant: **one demand of N → exactly N bins in flight across the loader's delivery cluster, never 2N** — in either direction (a `retrieveEmpty` parameter selects which direction's in-flight orders the budget counts).

How it works:

- **Per-loader mutex, keyed map, NO transaction.** `loaderResv` is a `sync.Map[loaderID]*sync.Mutex`; the reservation holds the loader's mutex across the count and the create. Two *different* loaders never block each other. There is deliberately **no surrounding DB transaction** — its atomicity is the mutex, not DB isolation.
  - *Why no tx (monotonicity):* the only operation that *raises* a loader's in-flight empty count is the create the seam guards; every other mutation (completion, cancellation, failure) only *lowers* it. Serialising the up-writers therefore makes the count monotone-safe without isolation.
  - *Why no tx (unsoundness):* `CreateRetrieveOrder` is not transaction-pure — it enqueues to Core and fires a synchronous `EmitOrderCreated` mid-write. A surrounding tx could roll back the DB rows while those side effects already happened, manufacturing the Core/Edge divergence it was meant to prevent.
- **One set query.** In-flight is counted across the loader's whole delivery-node set in a single `ListActiveOrdersByDeliveryNodeSet` (one snapshot), giving both the per-payload dedup and the loader-capacity cap.
- **The Loader owns the reservation shape.** `reserveLoaderBins` takes a `*domain.Loader`; the delivery-node set and the budget come from `loader.ReservationTarget(member, payload, multiWindow)`, which encodes the per-layout semantics so the seam stays layout-agnostic: a dedicated position maps to its one independent slot (budget 1); a shared loader funnels to its anchor (budget 1) **unless** multi-window is enabled, in which case it spreads to its windows (budget = `SlotCount`). The seam keys its mutex on `loader.ID()`.
- **Multi-window delivery (flag-gated).** With config `loaders_multi_window` on, a shared loader's bins spread **one per free window** — the seam computes the windows with none in flight and assigns each new order to a distinct one (round-robin), so a demand of N at an N-window loader fires exactly N, one per window, never two at the same window. Default OFF: a shared loader funnels to its anchor. The never-2N budget is per-loader (keyed on `loader.ID()`), so it is not fragmented by spreading.
- **One physical check the seam does NOT subsume.** The seam counts in-flight *orders*, not parked *bins*. The loader side relies purely on the order count because its `want` is demand-netted by the threshold monitor; the unloader's full-in is event-driven (`want=1`), so it keeps a physical "is a full already parked at the window?" check (`unloaderHasUsableFullPresent`) ahead of the seam.
- **Fails closed.** A count read error fires nothing; the next signal retries.

**Re-entrancy rule (MUST be honoured by every event-bus subscriber):** `reserveLoaderBins` calls its `fire` closure *while the loader's mutex is held*, and `CreateRetrieveOrder` dispatches `EmitOrderCreated` **synchronously** on the in-process bus (`eventbus.Emit` runs subscribers inline). **No `EventOrderCreated` (or any order-event) subscriber may synchronously call back into the reservation seam for the same loader** — `sync.Mutex` is non-reentrant and it would self-deadlock. A subscriber acting on a *different* loader is fine. If a subscriber ever needs to re-enter the same loader, split reserve-from-fire (end the lock after the DB insert; enqueue/emit after release). Guarded by `TestReserveLoaderEmpties_EmitDuringReservation_NoDeadlock`.

Callers routed through the seam — **loader side:** `tryCreateL1` (threshold + side-cycle), `RequestEmptyBin` (operator, manual_swap), and `maybeStageLoaderEmpty`/`MaybePushLoader` (via `tryCreateL1`). **Unloader side:** `createUnloaderFullInViaSeam`, reached from the consume `DemandSignal` / line-evac (`MaybeCreateUnloaderFullIn`) and the auto-push sweep (`MaybePushUnloader`/`SweepPushUnloaders`).

---

## LoaderStore (config resolution)

"Which loader serves this payload / contains this node, and what is its budget" is resolved through one consumer-defined interface, `engine.LoaderStore` (defined in the engine package, not next to the store — idiomatic Go, and it keeps the engine from importing store internals):

```go
type LoaderStore interface {
    LoaderForPayload(payload PayloadCode, role LoaderRole, activeOnly bool) (*domain.Loader, error)
    LoaderAt(coreNode NodeID, role LoaderRole) (*domain.Loader, error)
    Loaders(role LoaderRole) ([]*domain.Loader, error)
}
```

One implementation, `aggregateLoaderStore` — it projects the Core-owned cache into validated `*domain.Loader`s (via the domain constructors) and holds them as an **immutable in-memory snapshot**, swapped atomically (`atomic.Pointer`) on each node-list sync (`SetCoreLoaders` → `Refresh`). Resolution reads the snapshot, never the DB, so a torn multi-statement read of the cache is impossible and a DB flicker during a sync only keeps the last-known-good snapshot.

**Error contract (fail closed).** Every lookup returns `(*domain.Loader, error)`:

| Result | Meaning | Caller |
|---|---|---|
| `(loader, nil)` | resolved | use it |
| `(nil, ErrLoaderNotFound)` | a clean miss | may take its fallback (e.g. payload-first-match) |
| `(nil, other error)` | a real failure (DB read, malformed config) | **fail closed** — must NOT fall open, or a flicker reroutes demand to the wrong loader |

Callers branch with `errors.Is(err, ErrLoaderNotFound)`. This closes the prior bug where `resolveCoreLoaderForPayload` returned `nil` for both a miss and a DB error and the caller fell open into payload-first-match on a transient flicker.

**Consumed by both the loader and unloader paths.** `findLoaderForDemand` (DemandSignal) and `HandleLoopBelowThreshold` (C-push) resolve a `*domain.Loader` through the store and pass it to the seam; `refillLoaderForPayload` reads `loader.MinStockFor(payload)`. The unloader full-in resolves the same way — `MaybeCreateUnloaderFullIn` / `MaybePushUnloader` resolve a consume `*domain.Loader` through the store and route through the seam. The `manualSwapNode {node, claim}` shim is no longer the **unit of resolution**; it survives only as the projection the loader push/board enumerate (`manualSwapNodesFromCore`, built from the same aggregate), with `loaderFromManualSwapClaim` turning a resolved node into a single-window `Loader` for those paths.

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

## Transitional Preload Mode

A bridge for loaders whose payloads don't all have supermarket slots yet (the
manual tugger isn't fully eliminated), so UOP-threshold replenishment can't own
them. Design rationale lives in `transitional-bin-loader-plan-v2.md` at the
GitHub root; this section is the as-built summary.

**The flag.** A loader is marked transitional by membership in the Edge-only
`transitional_loaders` table, keyed by `core_node_name` (1:1 with the physical
loader — a loader shared across processes/styles has many claim rows but one
core node, so the flag is loader-wide). It is **not** plumbed through ClaimSync;
Core's threshold monitor already idles for a loader with no configured
threshold. `isTransitionalLoader(coreNodeName)` reads it, failing open
(non-transitional) on a DB error.

**What it changes.** For a transitional loader the market-accounting automatic
L1 paths are suppressed — both legacy bin-count (`refillLoaderForPayload`) and
UOP-threshold C-push (`HandleLoopBelowThreshold`) short-circuit in the single
`tryCreateL1` chokepoint (allowlist gate: `L1Source.suppressedByTransitional`).
Empties instead flow via `MaybePushLoader`, the loader-side mirror of
`MaybePushUnloader`: when the window is free it opportunistically stages one
empty. The staged empty is **payload-agnostic** — a generic carrier with no
payload tag, since an opportunistic stage has no payload-specific demand behind
it; the operator binds the real payload at load. Triggered on L2/clear
completion and a startup sweep. (Single-carrier assumption: a blank order
sources any compatible empty, which is correct only when the loader uses one
carrier type — `OrderRequest` carries no bin-type field, so `payload_code` is
the only carrier proxy on the wire. A multi-carrier loader would need a bin-type
field added before it could request a generic empty by carrier.)

**The board.** The HMI gains a PRELOAD / ACTIVE-ONLY toggle. ACTIVE-ONLY shows
only what the running styles need; PRELOAD shows the full covered list and
enables manual requests (the formalized `canRequestHere` path). The card sets
come from the multi-process view-model union (`active_style_payloads` /
`all_style_payloads`), which spans **every** active process sharing the loader —
so an operator at a loader feeding two cells sees both cells' payloads. A
transitional loader defaults to PRELOAD (no meaningful active-demand mode);
PRELOAD is shown with a distinct violet header treatment (not amber/orange,
which mean release/changeover).

**Routing.** Both automatic L1 paths resolve the loader by the signal's
`CoreNodeName`, not by first payload match, so a payload loaded at two separate
loaders routes to the one the signal names.

**Supermarket browse/manipulate panel** (PRELOAD-mode reach into the loader's
`InboundSource` / `OutboundDestination` markets, with a direction-aware
server-side move guard) is specified in the plan and **not yet implemented**.

**Deprecation.** Add supermarket space, clear the `transitional_loaders` row,
calibrate thresholds — the loader returns to C-push automatically. The preload
board stays available as a manual override.
