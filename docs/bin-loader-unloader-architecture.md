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

The bin-count `DemandSignal` route this section used to describe is gone (2026-08; see
Event Flow below). What remains Core-side is the demand registry and the threshold
signal — the UOP C-push path.

### Demand Registry

Core maintains a `demand_registry` table mapping payload codes to loader/unloader station addresses. It is derived from the Core-owned `bin_loaders` aggregate (`BuildDemandRegistryFromAggregate`, run by the seed / `migrateloaders`):

```
demand_registry
  station_id    TEXT     -- Edge station ID
  core_node_name TEXT   -- delivery node dot-name
  role          TEXT     -- "produce" (loader) or "consume" (unloader)
  payload_code  TEXT     -- allowed payload
```

`ClaimSync` (the Edge→Core push of `style_node_claims`) is **deleted**: Core owns loader config via the `bin_loaders` aggregate and derives `demand_registry` from it, syncing loader config down to the Edge through the node-list sync. Both halves are gone — `Engine.SendClaimSync` on the Edge and `HandleClaimSync` / `SubjectClaimSync` on the Core. (The plant-claims snapshot is a separate subject, `plant.claims`; it never flowed through `ClaimSync` and is unaffected.)

### Event Flow

**The bin-count DemandSignal route was removed entirely (2026-08).** Core no longer
emits `demand.signal` and no handler exists on Edge. The removal closed the kanban-eval
TODO with plant evidence: the produce leg emitted ~14 signals/day at Core, 100%
discarded on arrival by design; the consume leg had zero `demand_registry` rows at
either plant. Nothing downstream of either emission ever ran.

What remains, per side:

- **Produce (loader L1)**: Core's threshold monitor creates the orders directly (no wire
  signal — `LoopBelowThresholdSignal`/`HandleLoopBelowThreshold` were deleted 2026-08-02
  with the Edge's sizing half), plus the operator push. No bin-count trigger exists.
- **Consume (unloader U1)**: `MaybeCreateUnloaderFullIn` routes a U1 full-in through the
  reservation seam, fired by operator release and the auto-push sweep — no bin-arrival
  trigger exists.

`isStorageSlot` (formerly the storage-detection half of the trigger, step 3 below in
older versions of this doc) survives in `wiring_staging.go` with live callers unrelated
to demand.

### Deduplication

The reservation seam (`withLoaderBudget`, below) counts in-flight orders across the loader's delivery set before firing, so a payload that already has a non-terminal order in flight creates nothing. Core's `UNIQUE(edge_uuid)` constraint catches any remaining duplicates from at-least-once delivery.

### Startup Sweep

On Edge startup, after registration ack, the auto-push sweeps `SweepPushLoaders` / `SweepPushUnloaders` offer every auto loader/unloader's payloads to the reservation seam, picking up demand that arrived while offline. (`SendClaimSync` is deleted — see Demand Registry above.)

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

The seam runs in NO transaction, deliberately: its correctness is the per-loader mutex plus count monotonicity, not database isolation. Wrapping it in one would manufacture the Core/Edge divergence it exists to prevent, because the create it guards enqueues to Core and emits synchronously mid-write. The reasoning is written at the seam itself.

### Reservation Seam (the never-2N guarantee)

Every loader empty-in (an L1 `retrieve_empty`) and every unloader full-in (a U1 retrieve of a full bin) is created through **one** chokepoint, `engine.withLoaderBudget`. It owns the count→fire decision so the threshold signal (Kafka), an operator request (HTTP), and the push sweep can never both pass the in-flight count and both fire. The invariant: **one demand of N → exactly N bins in flight across the loader's delivery cluster, never 2N** — in either direction (a `retrieveEmpty` parameter selects which direction's in-flight orders the budget counts).

How it works:

- **Per-loader mutex, keyed map, NO transaction.** `loaderResv` is a `sync.Map[loaderID]*sync.Mutex`; the reservation holds the loader's mutex across the count and the create. Two *different* loaders never block each other. There is deliberately **no surrounding DB transaction** — its atomicity is the mutex, not DB isolation.
  - *Why no tx (monotonicity):* the only operation that *raises* a loader's in-flight empty count is the create the seam guards; every other mutation (completion, cancellation, failure) only *lowers* it. Serialising the up-writers therefore makes the count monotone-safe without isolation.
  - *Why no tx (unsoundness):* `CreateRetrieveOrder` is not transaction-pure — it enqueues to Core and fires a synchronous `EmitOrderCreated` mid-write. A surrounding tx could roll back the DB rows while those side effects already happened, manufacturing the Core/Edge divergence it was meant to prevent.
- **One set query.** In-flight is counted across the loader's whole delivery-node set in a single `ListActiveOrdersByDeliveryNodeSet` (one snapshot), giving both the per-payload dedup and the loader-capacity cap.
- **The Loader owns the reservation shape.** `withLoaderBudget` takes a `*domain.Loader`; the delivery-node set and the budget come from `loader.ReservationTarget(member, payload, multiWindow)`, which encodes the per-layout semantics so the seam stays layout-agnostic: a dedicated position maps to its one independent slot (budget 1); a shared loader funnels to its anchor (budget 1) **unless** multi-window is enabled, in which case it spreads to its windows (budget = `SlotCount`). The seam keys its mutex on `loader.ID()`.
- **Multi-window delivery (per-loader, Core-owned).** Multi-window is per-loader on the Core `bin_loaders.funnel_windows` field; the edge `loaders_multi_window` key is deprecated and only acts as a plant-wide OFF brake. With it on, a shared loader's bins spread **one per free window** — the seam computes the windows with none in flight and assigns each new order to a distinct one (round-robin), so a demand of N at an N-window loader fires exactly N, one per window, never two at the same window; with it off, the loader funnels to the first window with budget 1. The never-2N budget is per-loader (keyed on `loader.ID()`), so it is not fragmented by spreading.
- **One physical check the seam does NOT subsume.** The seam counts in-flight *orders*, not parked *bins*. The loader side relies purely on the order count because its `want` is demand-netted by the threshold monitor; the unloader's full-in is event-driven (`want=1`), so it keeps a physical "is a full already parked at the window?" check (`unloaderHasUsableFullPresent`) ahead of the seam.
- **Fails closed.** A count read error fires nothing; the next signal retries.

**Re-entrancy rule (MUST be honoured by every event-bus subscriber):** `withLoaderBudget` calls its `fire` closure *while the loader's mutex is held*, and `CreateRetrieveOrder` dispatches `EmitOrderCreated` **synchronously** on the in-process bus (`eventbus.Emit` runs subscribers inline). **No `EventOrderCreated` (or any order-event) subscriber may synchronously call back into the reservation seam for the same loader** — `sync.Mutex` is non-reentrant and it would self-deadlock. A subscriber acting on a *different* loader is fine. If a subscriber ever needs to re-enter the same loader, split reserve-from-fire (end the lock after the DB insert; enqueue/emit after release). Guarded by `TestWithLoaderBudget_EmitDuringReservation_NoDeadlock`.

Callers routed through the seam — **loader side:** `RequestEmptyBin` and `RequestFullBin` (operator, manual_swap), `maybeStageLoaderEmpty`/`MaybePushLoader` (the operator push), and `CreateRetrieveForAPI` (the HTTP order API). (`fireThresholdL1` is gone — deleted with the Edge's half of loader replenishment, 2026-08-02; the decision it carried is Core's.) **Unloader side:** `createUnloaderFullInViaSeam`, reached from produce-role lineside release (`MaybeCreateUnloaderFullIn`) and the auto-push sweep (`MaybePushUnloader`/`SweepPushUnloaders`).

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

**Consumed by the unloader paths.** The unloader full-in resolves a `*domain.Loader` through the store and passes it to the seam — `MaybeCreateUnloaderFullIn` / `MaybePushUnloader` resolve a consume `*domain.Loader` and route through the seam. There is no loader-side threshold resolver on the Edge any more (`HandleLoopBelowThreshold` is deleted — Core's threshold monitor creates its orders directly, so it never needs the Edge aggregate), and the legacy DemandSignal resolver and its bin-count minimum-stock read are retired too. The `manualSwapNode {node, claim}` shim is no longer the unit of resolution; every remaining path resolves a `*domain.Loader` from the aggregate.

---

## Edge State Tracking

Standard consume/produce nodes use `ActiveOrderID`/`StagedOrderID` on `ProcessNodeRuntimeState` (serial, one order at a time). Bin loader/unloader nodes skip these slots entirely and query the orders table directly.

`CanAcceptOrders` has role-based branching: for `bin_loader` role, it queries the orders table and allows multiple non-terminal orders. Changeover check still applies.

No schema changes to `ProcessNodeRuntimeState`. The serial model for consume/produce is untouched.

---

## Protocol

### Loader config: `NodeListResponse.Loaders` (Core → Edge)

Loader configuration is **Core-owned** and rides the node-list sync — there is no
dedicated loader-config message. `node.list_response` carries a `Loaders []LoaderInfo`
slice alongside the topology (`BuildLoaderInfos`), and the Edge swaps it into its
`core_loaders` cache / `aggregateLoaderStore` snapshot atomically with the node list.
This replaced the Edge→Core `ClaimSync` push below.

### ClaimSync — DELETED

`claim.sync` (the Edge→Core push of `style_node_claims` with a per-node `mode`/payload
set) authored loader config before the Core-owned refactor. It is **deleted**:
`Engine.SendClaimSync` on the Edge, `HandleClaimSync` and `SubjectClaimSync` on the
Core, and the `ClaimSync`/`ClaimSyncEntry` payload types are all gone, along with the
per-style edge loader checkboxes / `style_node_claims.mode` authoring path.
Core now owns the `bin_loaders` aggregate and derives `demand_registry` from it (see
Demand Registry), syncing config down on the node list.

(The plant-claims snapshot — `SubjectPlantClaims` → `HandlePlantClaims` — is a separate
subject and was never part of `ClaimSync`. An earlier version of this section kept the
Core handler alive on the grounds that "Edge still publishes a plant-claims snapshot";
that conflated the two subjects, and the handler was unreachable the whole time.)

### DemandSignal — REMOVED

The `demand.signal` subject, its payload struct, and both halves of its handling
were deleted in 2026-08 (Core's `sendDemandSignals`/`handleKanbanDemand`, Edge's
handler registration). There is no wire message and no code path; see Event Flow
above for what replaced each side.

### Existing (Unchanged)

- `OrderRequest` with `RetrieveEmpty` bool -- same protocol, just more orders queued per node.
- `LoadBin`, `ClearBin` HTTP endpoints unchanged.
- `BinUpdatedEvent` actions unchanged.

---

## Implementation Files

As-built after the Core-owned loader refactor. Loader config lives in the Core
`bin_loaders` aggregate; the Edge consumes a read-only projection. The retired
ClaimSync / `style_node_claims.mode` / edge-checkbox authoring path is gone.

### Core (owns the loader config)

| File | Role |
|------|------|
| `store/loaders/loaders.go`, `store/loaders.go` | The `bin_loaders` aggregate: `bin_loaders` (identity, role, layout, replenishment, flow dests) + `bin_loader_homes` (windows / dedicated positions, `UNIQUE(position_node_id)`) + `bin_loader_payloads` (shared-window payload set). CRUD + `GroupIntoLoaders` (migration derive) + `DeleteLoader` (soft-archive `archived_at`). |
| `service/loader_service.go` | Loader CRUD service (Create/Update/Delete/SetHome/SetPayload). `rederive` rebuilds `demand_registry` from the aggregate for the union of registry stations + registered edges and nudges the threshold monitor. |
| `store/loaders_sync.go` | `BuildLoaderInfos` (loader config for the node-list sync), `BuildDemandRegistryFromAggregate`, `DemandRegistryStations`. |
| `store/demand_registry.go`, `store/demands/demands.go` | `demand_registry` table + `SyncDemandRegistry` (diff/upsert). |
| `engine/wiring_staging.go` | `isStorageSlot` (parent LANE/NGRP + loader-home check) — moved here from wiring_kanban.go when the kanban demand-signal path was deleted; `resolveNodeStaging` (arrival staging). (`handleKanbanDemand`/`sendDemandSignals` deleted — see DemandSignal REMOVED above.) |
| `messaging/core_data_service.go` | Node-list response carries `Loaders` (`BuildLoaderInfos`); **seeds `demand_registry` from the aggregate on edge (re)connect**, then `thresholdMonitor.Resync`. (`HandleClaimSync` deleted.) |
| `store/migrations.go` | `bin_loaders` aggregate schema (v34–v40); `UNIQUE` on `orders.edge_uuid`. |
| `cmd/migrateloaders` | One-time per-plant migration: derive the aggregate from the legacy edge `style_node_claims` + seed `demand_registry`. |
| `www/handlers_loader.go`, `www/static/pages/loaders.js` | Core loader admin UI (create, layout, windows/positions, payload checklist + batch save, replenishment). |
| `dispatch/` | One-robot-at-a-time + bin-occupied guards in the fulfillment scanner. |

### Edge (consumes the Core projection)

| File | Role |
|------|------|
| `store/core_loaders.go`, `engine/loader_store.go` | `core_loaders` cache + `aggregateLoaderStore` — an immutable in-memory snapshot of the Core loader config, swapped atomically on each node-list sync. |
| `engine/core_loaders.go` | `SetCoreLoaders` / `Refresh` — ingest `NodeListResponse.Loaders` into the cache. |
| `engine/operator_demand_loader.go`, `operator_demand_unloader.go` | The operator push / release-triggered paths; the `withLoaderBudget` never-2N seam; `funnel_windows` on the Core loader row (multi-window is per-loader on that field — the edge `loaders_multi_window` key is deprecated and only acts as a plant-wide OFF brake). (DemandSignal handling deleted — see above.) |
| `domain/loader.go` | The `Loader` type, layouts (`shared_window` / `dedicated_positions`), `SlotCount`, `ReservationTarget`. |
| `service/station_service.go` | `BuildView` resolves a node's parent loader + windows from the aggregate for the operator HMI. |
| `messaging/edge_handler.go` | Node-list handler feeds `SetCoreLoaders`. (`onDemandSignal` callback deleted with the demand-signal route.) |
| `config/config.go` | `LoadersMultiWindow` (`loaders_multi_window`) — DEPRECATED, plant-wide OFF brake only. |
| `www/static/operator-station/*` | Demand-queue payload board + per-window state. |
| `engine/*` (retired) | `SendClaimSync` deleted; no `processes.js` loader-mode selector; `transitional_loaders` → `operator_driven_loaders` flag. |

### Protocol

| File | Role |
|------|------|
| `protocol/payloads.go` | `LoaderInfo` (carried on `NodeListResponse.Loaders`). (`DemandSignal` and `ClaimSync`/`ClaimSyncEntry` both deleted — see their sections above.) |
| `protocol/types.go` | No loader-replenishment subjects remain. (`SubjectClaimSync`, `SubjectDemandSignal`, and `SubjectLoopBelowThreshold` all deleted — the threshold signal went with the Edge's half of replenishment, 2026-08-02.) |

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

**Edge restart:** the auto-push sweeps run after registration ack, offering every auto loader/unloader's payloads to the seam to catch demand that arrived while offline.

**Concurrent pushes:** the seam's per-loader mutex serializes count-and-fire. Core's UNIQUE(edge_uuid) catches any remaining duplicates.

**No bins ever available:** Order stays queued indefinitely. Operator sees QUEUED status, can cancel.

**Future modes:** Hand loading stations and decanter stations could reuse the same infrastructure (demand queue, payload cards, hard rejection) with different cycle behaviors. The mode field accommodates future values like `hand_load` or `decanter`.

## Operator-Driven Loaders (formerly "transitional preload")

A loader whose replenishment is **operator-paced** rather than UOP-threshold-paced:
the operator decides what to stage from a preload board instead of the system
auto-firing on a kanban threshold. Originally a bridge for loaders whose payloads
don't all have supermarket slots yet (the manual tugger isn't fully eliminated).
Design rationale in `transitional-bin-loader-plan-v2.md` at the GitHub root; this is
the as-built summary. ("Transitional" was renamed "operator-driven"; the per-bin-count
`min_stock` floor is retired — replenishment is `{operator, threshold}`.)

**How it's set.** Replenishment is a field on the Core `bin_loaders` aggregate —
`operator` (this mode) or `threshold` (UOP-auto). Core owns it, so it rides the
node-list sync into the Edge cache; there is no separate edge flag to author. (The
legacy Edge-only `operator_driven_loaders` table — renamed from `transitional_loaders`
— was dropped; Core's replenishment field is the only source.)

**What it changes.** For an operator-driven loader the market-accounting automatic L1
path is suppressed — the UOP-threshold C-push is Core-owned and checks the
operator-driven flag on the Core aggregate, so it never orders for an
operator-driven loader. Empties instead flow via
`MaybePushLoader`, the loader-side mirror of `MaybePushUnloader`: when a window is free
it opportunistically stages one empty. The staged empty is **payload-agnostic** — a
generic carrier with no payload tag, since an opportunistic stage has no
payload-specific demand behind it; the operator binds the real payload at load.
Triggered on L2/clear completion and a startup sweep. (Single-carrier assumption: a
blank order sources any compatible empty, correct only when the loader uses one carrier
type — `OrderRequest` carries no bin-type field, so `payload_code` is the only carrier
proxy on the wire. The blank-order path is still type-blind; the quota tables
(`core_loader_window_bin_types` / `core_loader_quotas`) and the source finder already
speak bin type — the gap is the OrderRequest wire field.)

**The board.** The HMI gains a PRELOAD / ACTIVE-ONLY toggle. ACTIVE-ONLY shows only
what the running styles need; PRELOAD shows the full covered list and enables manual
requests (the `canRequestHere` path). The card sets come from the multi-process
view-model union (`active_style_payloads` / `all_style_payloads`), spanning **every**
active process sharing the loader — so an operator at a loader feeding two cells sees
both cells' payloads. An operator-driven loader defaults to PRELOAD; PRELOAD shows a
distinct violet header (not amber/orange, which mean release/changeover).

**Routing.** ~~The automatic L1 paths resolve the loader by the demand signal's owning
loader (the Edge maps the signal's `CoreNodeName` to its `loader_key`), not by first
payload match, so a payload loaded at two separate loaders routes to the one the signal
names.~~ *(The signal this described is gone — 2026-08. The threshold signal names its
loader directly from the threshold binding; the operator push and manual request paths
resolve from the node the operator is at.)*

**Supermarket browse/manipulate panel** (PRELOAD-mode reach into the loader's
`InboundSource` / `OutboundDestination` markets, with a direction-aware server-side
move guard) is specified in the plan and **not yet implemented**.

**Deprecation.** Add supermarket space, switch the loader's replenishment to
`threshold`, calibrate thresholds — it returns to UOP C-push automatically. The preload
board stays available as a manual override.
