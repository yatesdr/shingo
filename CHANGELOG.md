# Changelog

## 2026-03-25 — Universal Node Naming Alignment

### Transport Order Rename: pickup_node → source_node

Aligns transport order vocabulary with Derek's architecture (`OrderAck.SourceNode` precedent). Renames `pickup_node` / `PickupNode` to `source_node` / `SourceNode` across the entire codebase — protocol payloads, database schemas, Go structs, handlers, dispatch/planning logic, UI, and documentation.

- **Protocol**: `OrderRequest`, `OrderStorageWaybill`, `OrderIngestRequest`, `OrderStatusSnapshot` all use `source_node` (wire-breaking change — edge and core must deploy together)
- **Database migrations**: SQLite `ALTER TABLE orders RENAME COLUMN pickup_node TO source_node`; PostgreSQL via `migrateRenames()` for both `orders` and `mission_telemetry`
- **Store layer**: `Order.SourceNode`, `MissionTelemetry.SourceNode`, `UpdateOrderSourceNode()`
- **Dispatch/Engine**: planning service, fulfillment scanner, compound/complex orders, wiring, recovery — all updated
- **API handlers**: JSON tags and request structs on both edge and core

### Complex Order Test Form: Consistent Field Names

Renames complex order test handler fields to match style/cell config vocabulary:

- `FullPickup` → `InboundSource` (`full_pickup` → `inbound_source`)
- `StagingNode` → `InboundStaging` (`staging_node` → `inbound_staging`)
- `StagingNode2` → `OutboundStaging` (`staging_node_2` → `outbound_staging`)
- `OutgoingDest` → `OutboundDestination` (`outgoing_dest` → `outbound_destination`)

### UI Label Updates

- "Pickup Node" → "Source Node" across all order forms and detail views
- "Full Source" → "Inbound Source", "Staging Area 1/2" → "Inbound/Outbound Staging"
- "Outgoing Destination" → "Outbound Destination"
- "Production Node" → "Core Node" (edge manual order form)

## 2026-03-24 — Queued Order Fulfillment

### Queued Orders

Orders that cannot be immediately fulfilled (no source bin, no empty bin available) are now **queued** instead of failed. Core holds them in a `queued` status and automatically fulfills them FIFO when matching inventory becomes available. This eliminates race conditions when multiple nodes compete for scarce bins and removes the need for operators to manually retry failed orders.

**New status:** `queued` — first-class member of the order lifecycle. Applies to all retrieve and retrieve_empty orders, not just bin_loader nodes.

```
pending → sourcing → [found] → dispatched → in_transit → delivered → confirmed
                   → [not found] → queued → [bin available] → dispatched → ...
                   → [not found] → queued → [cancelled] → cancelled
```

### Fulfillment Scanner (Core)

Event-driven scanner monitors queued orders and matches them to available inventory:

- **Triggers:** bin arrival at storage, manifest clear, order completion/cancellation/failure (any event that frees a bin)
- **Safety sweep:** 60-second periodic scan catches anything events missed
- **Startup recovery:** scans queued orders on Core restart
- **FIFO fairness:** oldest queued order for a matching payload gets fulfilled first
- **Atomic claims:** `ClaimBin` prevents races between concurrent fulfillment attempts
- **Node vacancy guard:** skips fulfillment if the delivery node already has an in-flight delivery
- **Fleet failure recovery:** re-queues the order if fleet dispatch fails (transient, not permanent failure)
- **Mutex-guarded:** only one scan runs at a time, events coalesced during scan

### Payload Code Persistence

`payload_code` column added to Core's orders table (migration v8). Persisted at order creation so the fulfillment scanner can match queued orders to compatible bins without re-resolving from the original request.

### Edge Visibility

- `StatusQueued` with valid transitions: `submitted → queued`, `queued → acknowledged/cancelled/failed`
- Edge handler routes `OrderUpdate` with `status=queued` to proper status transition
- Startup reconciliation handles `queued` status from Core
- **Operator station:** bin_loader tiles show "AWAITING STOCK" in amber when a queued order is active
- **Material page:** queued orders show in the orders column with queued status badge

### Core Visibility

- SSE `order-update` event with `type: "queued"` for live dashboard refresh
- Amber CSS badge (`badge-queued`) in both light and dark themes
- Queued orders visible in active orders list

### Cancellation

Operators can cancel queued orders from Edge. Core's existing cancel flow works — no vendor order to cancel, no bin to unclaim, status transitions to cancelled cleanly.

## 2026-03-24 — Bin Loader Nodes, Core Telemetry API, NodeGroup Removal

### Bin Loader Role

New `bin_loader` claim role for nodes where forklifts load untracked material into existing bins. Operators select a payload from the claim's allowed list, confirm the manifest (from Core's payload template), set UOP count, and submit. The bin's manifest is set on Core via direct HTTP — no Kafka, immediate feedback.

- **Allowed payload codes** on style_node_claims — multi-select in claim modal, restricts which payloads a loader accepts
- **Load Bin** action on operator station and material page — payload picker, manifest from template, editable UOP
- **Clear Bin** action — reset a mis-loaded bin to empty
- **Move after load** — if outbound destination is configured, a move order auto-dispatches the loaded bin to storage
- **Claim modal field gating** — bin_loader hides swap mode, staging, inbound source, reorder, changeover fields
- **NGRP bulk claim creation** — selecting a group node expands to create claims for all direct physical children

### Core Telemetry API

New lightweight HTTP endpoints for Edge to fetch real-time state from Core, replacing Kafka for synchronous operations:

| Endpoint | Purpose |
|----------|---------|
| `GET /api/telemetry/node-bins` | Bin state per node (label, type, payload, UOP, manifest, confirmed) |
| `GET /api/telemetry/payload/{code}/manifest` | Payload manifest template + UOP capacity |
| `GET /api/telemetry/node/{name}/children` | Physical children of an NGRP node |
| `POST /api/telemetry/bin-load` | Set manifest on bin at node (was Kafka `bin.load`) |
| `POST /api/telemetry/bin-clear` | Clear bin manifest at node |

Edge `CoreClient` (`engine/core_client.go`) makes on-demand HTTP calls with 3s timeout. Graceful degradation — views render without bin data if Core is unreachable. Core API URL configured in Edge settings page.

### Bin State Visibility

- **Operator station tiles** show bin label (bold), loaded payload code, and EMPTY/LOADED/NO BIN status
- **Material page** shows bin label, payload from Core, and actual UOP count for bin_loader nodes
- **View Contents** modal on material page shows full bin manifest (part numbers, quantities), bin type, and confirmation status
- **Core nodes page** refreshes via SSE on bin-load/clear events; inventory display enriched with bin type, contents, UOP, and lock/claim badges

### NodeGroup Removal

Removed `NodeGroup` field from wire protocol `ComplexOrderStep`. Core auto-detects NGRP nodes via `IsSynthetic + NodeTypeCode` and resolves them — same pattern simple orders already used. Collapsed 4 edge claim source columns (`inbound_source_node`, `inbound_source_node_group`, `outbound_source_node`, `outbound_source_node_group`) into 2 (`inbound_source`, `outbound_source`).

### Code Quality

- Removed `enrichSingleViewBinState` wrapper (inlined at call site)
- `FetchNodeBins` error handling made consistent with other read methods (silent degradation)
- `slices.Contains` replaces hand-rolled loop in `LoadBin`
- Dead self-assignment removed from `SwitchNodeToTarget`
- Node children endpoint uses `GetNodeByDotName` for dot-notation consistency
- `bin.load` Kafka artifacts fully removed: `TypeBinLoad`, `BinLoadRequest`, `BinLoadAck`, `HandleBinLoad` from protocol, dispatcher, and core handler

## 2026-03-23 — Delivery Cycle Modes: Sequential, Single Robot, Two Robot

Adds source/destination routing to `style_node_claims`, fixes single-robot and two-robot step sequences, and introduces sequential mode.

### New Fields on `style_node_claims`

Two columns for source/destination routing, separate from staging areas:

```
InboundSource → InboundStaging → CoreNodeName → OutboundStaging → OutboundSource
 (where from)     (temp park)      (lineside)     (temp park)       (where to)
```

| Field | Purpose |
|-------|---------|
| `inbound_source` | Pickup node or group for new material (Core auto-detects groups) |
| `outbound_source` | Dropoff node or group for old material (Core auto-detects groups) |

Can be a specific node or a node group — Core auto-detects NGRP nodes and resolves via the group resolver. Blank = Core global fallback by payloadCode. Fully backward compatible.

### Step Sequences

#### Sequential — two robots, staggered dispatch

```
Order A (Robot 1 — removal):             Order B (Robot 2 — backfill):
┌─────────────────────────────────┐      ┌─────────────────────────────────┐
│ 1. dropoff(CoreNodeName)        │      │ 1. pickup(InboundSource)        │
│ 2. wait                         │      │ 2. dropoff(CoreNodeName)        │
│ 3. pickup(CoreNodeName)         │      └─────────────────────────────────┘
│ 4. dropoff(OutboundSource)      │────────▶ Order B auto-created when
└─────────────────────────────────┘        Order A goes "in_transit"
```

Order A delivery_node = "" (removal, no UOP reset). Order B delivery_node = CoreNodeName (backfill, resets UOP).

#### Single Robot — 10-step swap (was 7)

```
 1. pickup(InboundSource)          — pick new from source
 2. dropoff(InboundStaging)        — park new at inbound staging
 3. dropoff(CoreNodeName)          — pre-position at lineside
 4. wait                           — operator releases
 5. pickup(CoreNodeName)           — pick up old from line
 6. dropoff(OutboundStaging)       — quick-park old nearby
 7. pickup(InboundStaging)         — grab new from staging
 8. dropoff(CoreNodeName)          — deliver new to line
 9. pickup(OutboundStaging)        — grab old from staging
10. dropoff(OutboundSource)        — deliver old to final dest.
```

#### Two Robot — parallel swap

```
Order A (resupply):                      Order B (removal):
┌─────────────────────────────────┐     ┌─────────────────────────────────┐
│ 1. pickup(InboundSource)        │     │ 1. dropoff(CoreNodeName)        │
│ 2. dropoff(InboundStaging)      │     │ 2. wait                         │
│ 3. wait                         │     │ 3. pickup(CoreNodeName)         │
│ 4. pickup(InboundStaging)       │     │ 4. dropoff(OutboundSource)      │
│ 5. dropoff(CoreNodeName)        │     └─────────────────────────────────┘
└─────────────────────────────────┘
```

Two-robot validation now only requires InboundStaging (not OutboundStaging) — removal robot goes direct to OutboundSource.

### Other Changes

- `buildStep` helper sends node name; Core auto-detects groups (no `node_group` on wire protocol)
- `BuildDeliverSteps` / `BuildReleaseSteps` use source routing instead of staging fields for pickup/dropoff
- Sequential backfill wired via `EventOrderStatusChanged` → `handleSequentialBackfill` in `engine/wiring.go`
- UI: "Sequential" added to swap mode dropdown, source/destination fields added to claim modal
- `NodeGroup` field removed from `ComplexOrderStep` wire protocol — Core auto-detects NGRP nodes

### Files Changed

| File | Change |
|------|--------|
| `store/schema.go` | Migrations for source routing columns (collapsed to `inbound_source`, `outbound_source`) |
| `store/style_node_claims.go` | Struct + SQL updated for 2 source fields |
| `engine/material_orders.go` | Step builders rewritten: buildStep helper, 10-step single, source routing on two-robot, sequential builders added |
| `engine/operator_stations.go` | Sequential case added to `requestNodeFromClaim`, two-robot validation relaxed |
| `engine/wiring.go` | `EventOrderStatusChanged` subscription + `handleSequentialBackfill` handler |
| `www/templates/processes.html` | Sequential in dropdown, source/destination fieldset in claim modal |
| `www/static/js/pages/processes.js` | Source fields wired in edit/save/display, validation updated |

## 2026-03-21 — Lifecycle, Messaging, and Recovery Hardening

### Core Reliability

- **Durable inbound dedupe:** Core now persists inbound message IDs and suppresses replayed mutating commands before they reach dispatch.
- **Completion hardening:** Delivery receipts fail closed, duplicate receipts are ignored, and completion-side state changes are safer and more atomic.
- **Outbox consistency:** Core control/data replies now use the same durable outbox-backed delivery model as dispatch replies.
- **Reconciliation:** Core now detects completion drift, stale claims, stuck orders, expired staged bins, stale edges, dead letters, and outbox backlog age.
- **Recovery actions:** Safe audited repair actions were added for completion drift, stale terminal claims, staged-bin release, dead-letter replay, and stuck-order cancellation.

### Edge Reliability

- **Confirm fail-closed:** Edge no longer transitions an order to `confirmed` if the delivery receipt cannot be durably enqueued.
- **Startup reconciliation:** Edge requests authoritative order status from Core on startup and after re-registration so local state can be corrected after restart or disconnect.
- **Diagnostics:** Edge diagnostics now expose reconciliation anomalies, dead-letter outbox messages, and replay/sync actions.
- **Messaging split clarified:** Order mutations use durable outbox delivery, while operational data traffic uses an explicit direct-send path with retry.

### Architecture Cleanup

- **Core lifecycle extraction:** Order creation, cancel, receipt, redirect, ingest/store setup, and reply transport were moved behind explicit lifecycle and reply services.
- **Core planner registry:** Dispatch planning is now routed through registered order-type planners instead of a hardcoded `switch`, making new order types much less threaded to add.
- **Core data handler extraction:** Data-subject handling was split out of `CoreHandler`, leaving the transport layer thinner.
- **Edge lifecycle extraction:** Edge order transitions and Core-status reconciliation moved behind a lifecycle service, while envelope creation/enqueue moved into a dedicated order sender.
- **Messaging consistency:** Edge heartbeats and Core-sync requests now use a shared data sender instead of separate ad hoc publish flows.

### Observability and Diagnostics

- **Dashboard/health visibility:** Reconciliation summary and severity are now surfaced in Core dashboard, diagnostics, and health endpoints.
- **Recovery history:** Recovery actions are recorded so operator/admin repairs are auditable.
- **Diagnostics UI expansion:** Core diagnostics now includes reconciliation anomalies, dead letters, replay actions, and recovery workflows.

## 2026-03-21 — Edge Production Hardening & Domain Rename

### Breaking Changes

**Domain model rename** — Edge types, DB tables, API routes, and UI labels have been renamed to align with actual usage:

| Old | New | Rationale |
|-----|-----|-----------|
| `Payload` (store) | `MaterialSlot` | Edge's "payload" was a per-line slot config, not a template. Core owns the template (`PayloadCatalog`). |
| `ProductionLine` | `Process` | UI already said "Process". Matches terminology doc. |
| `JobStyle` | `Style` | UI already said "Style". Matches terminology doc. |
| `LocationNode` | `Node` | Redundant name. Matches Core's `Node`. |
| `Resupply` / `Removal` | `PrimaryOrder` / `SecondaryOrder` | Mode-neutral naming for `OrderRequestResult`. |

**DB migration** is automatic — `ALTER TABLE RENAME` runs on startup for existing databases.

**API routes renamed:**
- `/api/payloads/*` → `/api/material-slots/*`
- `/api/lines/*` → `/api/processes/*`
- `/api/job-styles/*` → `/api/styles/*`
- `/api/location-nodes/*` → `/api/nodes/*`

**Query param:** `?line=` → `?process=`

### Production Reliability Fixes

- **Cancel safety:** Cancel message enqueued before local transition. Prevents robot continuing on a locally-cancelled order.
- **Envelope failure handling:** Orders stay in `pending` if envelope fails to build or enqueue. Prevents stuck `submitted` orders that Core never receives.
- **Two-robot half-cycle prevention:** If removal order fails after resupply succeeds, resupply is automatically cancelled.
- **Replenishing deadlock fix:** Payload slot reset from `replenishing` to `active` if order creation fails, so auto-reorder can re-trigger.
- **Changeover durability:** DB write happens before in-memory state change. Errors propagate to HTTP response instead of being swallowed.
- **Store order waybill:** Enqueued before status transition. Failure returns error to operator.
- **Redirect order:** Envelope enqueued before local DB update. Failure returns error.
- **Production reporter:** Accumulated deltas restored on outbox enqueue failure (no silent data loss).
- **Heartbeat retry:** Periodic heartbeats now use 3-attempt retry with backoff (matches startup behavior).
- **Dead-letter logging:** Outbox dead-lettered messages logged at ERROR level with debug trace.

### Code Quality

- Domain constants for slot statuses (`SlotActive`, `SlotEmpty`, `SlotReplenishing`), roles, cycle modes, and dispatch reply types — eliminates scattered string literals.
- Debug logging added to all critical paths: order completion, order failure, slot reorder, auto-confirm, payload metadata lookup, envelope failures.
- `CountActiveOrders` error handling fixed (was silently returning 0).
- DB index added on `orders.payload_id`.
- `Multiplier` field removed (was always 1).
- `ManageReportingPointTag` flattened from nested conditionals to linear flow.

### UI Changes

- **Nav restructured:** 3 public tabs (Status, Orders, Changeover) + Admin dropdown (Setup, Production, Manual Order, Operator, Messages, Logs).
- **Auth gating:** Production, Manual Order, and Operator pages moved behind admin login. Operator display/cell views remain public (shop floor monitors).
- **Login/Logout** link added to nav bar.
- **Labels cleaned up:** "LSL Node" → "Location", "UOP Total" → "Capacity", "Reorder Pt" → "Reorder At", "Define Payloads" → "Material Slots", "Location Node" → "Node", removed "ALN or PLN" jargon.
