# Changelog

## 2026-03-23 — Delivery Cycle Modes: Sequential, Single Robot, Two Robot

Adds source/destination routing to `style_node_claims`, fixes single-robot and two-robot step sequences, and introduces sequential mode.

### New Fields on `style_node_claims`

Four columns for source/destination routing, separate from staging areas:

```
InboundSourceNode → InboundStaging → CoreNodeName → OutboundStaging → OutboundSourceNode
  (where from)       (temp park)      (lineside)     (temp park)       (where to)
```

| Field | Purpose |
|-------|---------|
| `inbound_source_node` | Explicit pickup node for new material |
| `inbound_source_node_group` | Node group for new material (Core resolves via FIFO/FAVL) |
| `outbound_source_node` | Explicit dropoff node for old material |
| `outbound_source_node_group` | Node group for old material (Core resolves) |

Node = specific slot (Shingo doesn't control storage). Node group = Core picks best slot. Blank = Core global fallback by payloadCode. Fully backward compatible.

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

- `buildStep` helper uses 3-tier resolution: explicit node → node group → empty (global fallback)
- `BuildDeliverSteps` / `BuildReleaseSteps` use source routing instead of staging fields for pickup/dropoff
- Sequential backfill wired via `EventOrderStatusChanged` → `handleSequentialBackfill` in `engine/wiring.go`
- UI: "Sequential" added to swap mode dropdown, source/destination fields added to claim modal

### Files Changed

| File | Change |
|------|--------|
| `store/schema.go` | 4 ALTER TABLE migrations for source routing columns |
| `store/style_node_claims.go` | Struct + SQL updated for 4 new fields |
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
