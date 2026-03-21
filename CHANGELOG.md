# Changelog

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
