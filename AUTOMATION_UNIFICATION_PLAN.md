# Automation Unification Plan

## Summary

Replace the multi-toggle automation system (AutoReorder, AutoOrderEmpties, RetrieveEmpty, AutoRemoveEmpties, HotSwap) with a unified model: **CycleMode** + **Role**. One material handling cycle, one code path, applied equally to consume and produce payloads.

## Core Principle: Material Flow Is Circular

A bin sits at a station. It gets used (consume) or filled (produce). When it's done, two things happen: the outgoing bin leaves, a replacement arrives. This is one cycle regardless of role. The role only determines which direction the bins flow — full in / empty out, or empty in / full out. The cycle itself is the same.

Both consume and produce go through the same `RequestOrders` function. CycleMode (sequential, two-robot, single-robot) determines the strategy. Role determines the bin direction.

## Design Principles

1. **One cycle, one path.** Both consume and produce use `RequestOrders`. No separate code paths per role.
2. **Same trigger mechanism.** Both roles trigger on counter delta crossing the reorder point. Consume: remaining drops as parts are used. Produce: remaining drops as capacity fills up. Same `handleCounterDelta` code, same logic.
3. **Role determines direction.** Consume = full bins in, empties out. Produce = empties in, full bins out. Derived from `Role == "produce"` at order creation, not configured by operators.
4. **CycleMode determines strategy.** Sequential, Two Robot, or Single Robot. Applies equally to both roles.
5. **AutoReorder controls WHO triggers.** ON = system triggers when counter crosses reorder point. OFF = operator presses REQUEST button. The cycle itself is the same either way.
6. **Reset on delivery confirmation.** When the replacement bin is delivered and confirmed, remaining resets to UOP capacity. Same for both roles. Ingest completion (Core storing a full produce bin) is Core's concern — Edge does nothing on ingest completion because the cycle was already triggered by the counter.
7. **Operator interaction is confirmation.** REQUEST (trigger), RELEASE (confirm pickup), CONFIRM (acknowledge replacement). Not automation toggles.

## Unified Cycle Model

| | Consume | Produce |
|---|---|---|
| **Trigger** | Counter crosses reorder point (remaining drops — parts used) | Counter crosses reorder point (remaining drops — capacity filling up) |
| **Path** | `handleCounterDelta` → `RequestOrders` | `handleCounterDelta` → `RequestOrders` |
| **CycleMode applies?** | Yes | Yes |
| **Sequential** | Navigate → wait → pickup empty → dropoff → Order B delivers full | Navigate → wait → pickup full → dropoff → Order B delivers empty |
| **Two Robot** | Resupply stages full + removal stages at line → release → swap | Resupply stages empty + removal stages at line → release → swap |
| **Single Robot** | Full 10-step swap cycle | Full 10-step swap cycle |
| **Role's effect** | `retrieveEmpty = false` (get me a full bin) | `retrieveEmpty = true` (get me an empty bin) |
| **Reset point** | Replacement bin delivered → `resetPayloadOnRetrieve` | Replacement bin delivered → `resetPayloadOnRetrieve` |

## Cycle Modes

### Sequential (Default)
- Counter crosses reorder point → cycle triggers Order A
- Order A steps: `navigate(lineside) → wait → pickup(lineside) → dropoff(outgoing destination)`
- Robot drives to lineside (no cargo), enters WAITING state
- Operator sees STAGED — bin may still have parts being consumed or filled
- Operator presses RELEASE when bin is ready
- Robot picks up outgoing bin and takes it to outgoing destination
- Edge simultaneously creates Order B (retrieve replacement, deliver to lineside)
- Replacement delivered → remaining resets to UOP capacity → cycle complete

### Two Robot Hot-Swap
- Counter crosses reorder point → two concurrent orders created
- Robot 1 (Resupply): pickup replacement → stage → wait → pickup from staging → deliver to line
- Robot 2 (Removal): navigate to line → wait → pickup outgoing → deliver to outgoing destination
- Both robots position simultaneously
- Operator releases both at once
- Swap happens instantly
- Replacement delivered → remaining resets → cycle complete

### Single Robot Hot-Swap
- Counter crosses reorder point → one 10-step order
- Pre-stage replacement bin at staging, navigate to lineside, wait
- Operator releases
- Robot shuffles: outgoing to staging 2, replacement to lineside, outgoing to destination
- Replacement delivered → remaining resets → cycle complete

## Fields Removed (from UI and engine logic)

| Field | Was | Replaced By |
|-------|-----|-------------|
| `auto_order_empties` | Toggle produce cycle | Always on — both roles cycle through `RequestOrders` |
| `auto_remove_empties` | Toggle consume empty removal | CycleMode determines removal strategy |
| `retrieve_empty` (on payloads) | Config flag for bin type | Derived from `Role == "produce"` at order creation |
| `hot_swap` | Select hot-swap mode | Renamed to `cycle_mode` with sequential added |
| `manifest` | JSON manifest text | Core manages manifests — not an Edge concern |
| `multiplier` | Count-by multiplier | Always 1 — removed from calculation |
| `reorder_qty` | Quantity per reorder | Always 1 — one order = one bin |
| `production_units` | Bin capacity | UOP capacity from `payload_catalog` (synced from Core) |

## Fields Kept

| Field | Purpose |
|-------|---------|
| `auto_reorder` | WHO triggers: ON = system (counter), OFF = operator (REQUEST button) |
| `cycle_mode` | HOW the cycle executes: sequential, two_robot, single_robot |
| `role` | WHICH direction bins flow: consume or produce |
| `reorder_point` | WHEN the system triggers (if auto_reorder is ON) |
| `payload_code` | MANDATORY — links to Core catalog for UOP capacity lookup |
| `remaining` | Current stock level (operational state) |
| `status` | Current payload state (operational state) |

## Payload Config Form

1. **Payload Code** — mandatory, picker from Core catalog (auto-fills description + UOP)
2. **Description** — auto-filled, editable override
3. **Location (ALN)** — picker from Core approved nodes
4. **Role** — consume / produce
5. **Reorder Point** — threshold to trigger cycle
6. **Auto Reorder** — checkbox (ON = system triggers, OFF = operator REQUEST button)
7. **Cycle Mode** — sequential / two_robot / single_robot
8. **Node Config** (conditional on cycle mode) — Full Pickup Source, Staging 1, Staging 2, Outgoing Destination

## Operator Canvas Button States

Three operator interactions: REQUEST, RELEASE, CONFIRM.

### Auto Reorder ON (system-triggered)

| What operator sees | Button |
|---|---|
| Bin is fine, system monitoring | REQUEST (available as early override) |
| System triggered cycle, robot en route | ORDER IN PROGRESS (disabled) |
| Robot staged at station, waiting | RELEASE |
| Bin picked up, replacement on the way | ORDER IN PROGRESS (disabled) |
| New bin arrived at station | CONFIRM |
| Confirmed, remaining resets, cycle complete | Back to REQUEST |

### Auto Reorder OFF (operator-triggered)

| What operator sees | Button |
|---|---|
| Bin getting low, no order yet | REQUEST (operator must press to start) |
| Operator pressed REQUEST, robot en route | ORDER IN PROGRESS (disabled) |
| Robot staged at station, waiting | RELEASE |
| Bin picked up, replacement on the way | ORDER IN PROGRESS (disabled) |
| New bin arrived at station | CONFIRM |
| Confirmed, remaining resets, cycle complete | Back to REQUEST |

Same cycle, same steps. Only difference: who triggers the initial order.

## UOP Capacity — Single Source of Truth

UOP capacity is cached on Edge in the `payload_catalog` table (synced from Core via Kafka). No second copy on the payloads table.

```
resetPayloadOnRetrieve:
  bp := GetPayloadCatalogByCode(payload.PayloadCode)
  resetUnits = bp.UOPCapacity
```

PayloadCode is mandatory. The lookup always has a key. If the catalog hasn't synced yet, the reset logs an error and retries on the next delivery.

## What Does NOT Change

- **Core (shingo-core)** — zero changes. Core receives orders and processes them.
- **Protocol** — zero changes. `OrderRequest.RetrieveEmpty` stays. Edge derives the value from Role.
- **Order struct** — `retrieve_empty` stays on orders. Per-order runtime state, not config.
- **Order Manager** — `CreateRetrieveOrder` still takes `retrieveEmpty` parameter. Callers derive from Role.
- **Manual Orders page** — unchanged. Operators can still manually create any order type.
- **payload_catalog table** — unchanged. Already has `uop_capacity`. Single source of truth.

## Key Design Decisions

1. **PayloadCode is mandatory.** Links to Core's catalog. Required for UOP capacity lookup.
2. **UOP capacity lives in payload_catalog only.** Single cache maintained by Core's catalog sync.
3. **OrderRequestResult uses CycleMode string.** One field. Clients check `cycle_mode === "two_robot"`.
4. **Both roles use the same path.** `RequestOrders` handles consume and produce identically. Role only affects `retrieveEmpty` on the backfill order.
5. **Remaining resets on delivery confirmation.** Not on ingest completion. The cycle was triggered by the counter — the reset happens when the replacement bin arrives.
6. **OutgoingNode replaces EmptyDropNode.** "Outgoing" is accurate for both roles — the outgoing bin could be empty (consume) or full (produce).

## Future Work

### Cancel Order (Operator Canvas)
Operators need a way to cancel an in-progress order from the shop floor canvas. Currently the system handles `cancelled` as a status but the operator has no way to trigger it. The cancel API endpoint exists (`apiCancelOrder`). Implementation: long press on the ORDER IN PROGRESS button area reveals a cancel option. Prevents accidental cancellation while keeping the action accessible.

### Startup Recovery Scan (Both Roles)
`scanProducePayloads` currently handles initial provisioning for produce payloads at startup. A broader startup scan should detect and recover stuck payloads for both roles — e.g., payloads stuck in "replenishing" status with no active orders after a restart. This is a safety net, not part of the normal cycle flow.
