# Changeover — Problems & Integration Analysis

This document tracks known problems with the changeover implementation, their status, and the decisions made for resolved ones. It also captures integration points across the system, overlap risks between changeover and existing subsystems, and patterns transferable to other Shingo systems. Use as reference when resuming development.

---

## Problem 1 — Dedicated staging orders never trigger "staging done"

**Status: SOLVED**

**Problem:** The staging tracker marks orders done on `OrderCompleted` and `OrderFailed` events. But dedicated robot staging orders (complex orders with a wait step) transition to `staged` status when the robot parks — they never reach `completed` during the staging phase. The staging track hangs forever when dedicated robots > 0.

**Decision:** Treat `staged` status as "done" for the staging tracker. Added a subscription to `EventOrderStatusChanged` in `engine/wiring.go` — when any order transitions to `staged`, it calls `notifyChangeoverTrackers` with success. Fire-and-forget orders complete normally (trigger via `OrderCompleted`), dedicated orders reach `staged` (trigger via `OrderStatusChanged`). Both paths mark the order as done in the tracker.

**Implementation:** `shingo-edge/engine/wiring.go` — new event subscription for `EventOrderStatusChanged` checking `NewStatus == "staged"`.

---

## Problem 2 — Core-created swap orders invisible to Edge

**Status: SOLVED (design decided, implementation pending)**

**Problem:** During Execute, Edge sends `changeover.execute` to Core. The original implementation had Core creating swap orders directly — those orders exist in Core's database but not Edge's. When Core sends `order.delivered` messages back, Edge can't find the orders locally. The executing tracker on Edge is empty and never fires completion. Changeover hangs in Executing forever.

**Decision:** Edge creates ALL orders (following the existing architecture — Edge always creates locally first). The flow:

- **Dedicated robots:** Edge already created staging orders during the Staging phase using `CreateComplexOrder` with `complete: false` (no explicit wait step needed — RDS natively enters WAITING when blocks run out on an incomplete order). On Execute, Core matches staged orders to swap items by staging node, then calls RDS `/addBlocks` with the swap blocks and `complete: true`. The robot immediately starts executing swap blocks. Edge tracks the original staging order IDs through normal order lifecycle.

- **Non-dedicated bins:** Edge creates new swap orders during Execute using `CreateComplexOrder` — standard Edge → Core → RDS flow.

- **Executing tracker:** Populated with dedicated staging order IDs (already known from staging phase) plus new swap order IDs (created at execute time). All tracked through normal `OrderCompleted` events.

- **Robot-to-line matching:** Core matches staged orders to swap items by staging node (from the `ChangeoverExecuteRequest`). Each swap item carries a `staging_node` field. Core finds the staged order that delivered to that node. Two changeovers on different lines won't collide because they use different staging nodes.

**Pending implementation:**
1. Remove explicit `wait` step from dedicated staging orders — use `complete: false` pattern instead (RDS WAITING state)
2. Update Core's `HandleChangeoverExecute` to use `/addBlocks` on staged orders matched by staging node
3. Edge's `handleChangeoverExecuting` needs to populate executing tracker with dedicated staging order IDs + new swap order IDs
4. Need to add `CompleteFlag` or similar to the complex order protocol so Core knows to dispatch with `complete: false`

---

## Problem 3 — Auto-reorder state during changeover

**Status: DESIGN DECIDED (revised 2026-03-21)**

**Problem:** Original framing was about restoring auto-reorder on cancel. Deeper issue: the system should never silently toggle auto-reorder. It's an explicit operator decision and their responsibility.

**Decision (revised):** The changeover system **never modifies auto-reorder flags**. The per-payload `auto_reorder` boolean in the DB stays exactly as the operator set it — before, during, and after changeover. No bulk enable, no bulk disable, no snapshot/restore.

Instead, the changeover **suppresses reorder behavior** while active:
- A guard in `handlePayloadReorder` (`engine/wiring.go`) checks if a changeover is active on the payload's line. If yes, skip the reorder — don't create orders. The flag stays ON in the DB but the behavior is suppressed.
- When changeover completes (or is cancelled and the line resumes), the guard lifts. Whatever auto-reorder flags the operator configured take effect immediately.
- On completion with a new job style: the new style's payloads use their pre-configured auto-reorder flags. If an operator set a payload to auto-reorder OFF, completion doesn't override that.

**Canvas presentation:**
- During active changeover, the StatusBar auto-reorder toggle shows as **disabled/greyed out** — visually communicates "not active right now" regardless of the underlying flag value
- The operator canvas is an HMI — operators need to see at a glance that auto-reorder is not actionable during changeover
- After changeover ends (complete or cancel → resume), the toggle returns to its normal interactive state reflecting the actual per-payload flags

**What this changes from the earlier design:**
- `handleChangeoverStarted` NO LONGER calls `SetAutoReorderByJobStyle(false)` — flags untouched
- `handleChangeoverCompleted` NO LONGER calls `SetAutoReorderByJobStyle(true)` — flags untouched
- `SetAutoReorderByJobStyle` is still needed for the canvas toggle (per-line bulk toggle, see Problem 10) but NOT called by changeover lifecycle
- No snapshot/restore logic needed

**Implementation:**
- Add guard to `handlePayloadReorder` in `engine/wiring.go`: check `e.ChangeoverMachine(lineID).IsActive()` (or equivalent) before dispatching reorder
- Canvas StatusBar toggle: when changeover is active on the line, render greyed out with text like "AUTO-REORDER (CHANGEOVER)" or similar disabled appearance
- `handleChangeoverCancelled` and `handleChangeoverCompleted`: no auto-reorder logic at all

---

## Problem 4 — Cancel doesn't abort in-flight orders

**Status: PARTIALLY ADDRESSED (changeover start cancels; cancel-during-phases still open)**

**Problem:** If operator cancels during staging, robots keep delivering to staging areas. If cancelled during executing, swap robots keep swapping. The machine state says "running" but robots are still executing changeover work. Staging bins arrive with nobody tracking them. During executing, a robot might be mid-swap — old bin picked up, new bin not yet placed.

**Decided — order cancellation on changeover start:** When a changeover is initiated (`handleChangeoverStarted`), ALL active orders on the line are cancelled immediately. This uses `AbortOrder(orderID)` for each order returned by `ListActiveOrdersByLine(lineID)`. `AbortOrder` handles both local state transition (→ cancelled) and sending `order.cancel` to Core via Kafka. All orders are cancelled regardless of status — pending ones locally, submitted/in-transit/staged ones via Kafka cancel to Core. Errors are logged but don't block the loop (a failed order abort shouldn't prevent the rest from being cancelled).

**Cancellation system notes (discovered during planning):**
- `AbortOrder` exists at `orders/manager.go:364` — transitions to cancelled + enqueues `order.cancel` to Kafka outbox
- Works for individual orders only; the changeover handler loops through `ListActiveOrdersByLine` results
- `AbortOrder` reason is hardcoded to `"aborted by operator"` — should be changed to `"changeover initiated"` for traceability
- `ListActiveOrdersByLine` (`store/orders.go:85`) excludes `confirmed` and `cancelled` but NOT `failed` — may try to abort a failed order, which returns a harmless "already terminal" error
- Known gap: after cancellation, payload status stays "replenishing" — no cleanup handler exists yet. Not in scope for this slice but should be addressed.
- No bulk cancel exists — each order gets its own `AbortOrder` call and Kafka message. Typical line has <20 active orders, so this is fine.

**Still open — cancel during later phases:** Aborting orders during staging/clearing/executing phases (after changeover start) still needs the tracker-based approach described below. Aborting dedicated robot orders mid-swap is dangerous — if Robot 3 has picked up old Bin A and is en route to outgoing, aborting would leave the robot holding a bin with nowhere to go. Safest approach: let the current bin swap complete, then cancel remaining bins. This requires block-level tracking, which is complex.

---

## Problem 5 — No swap order visibility for operators

**Status: OPEN**

**Problem:** The operator canvas shows a changeover banner, but there's no visibility into which robots are dedicated, what bins they're handling, or their progress through the multi-bin swap chain. If a robot fails mid-chain, the operator has no way to know which bin is where or which step failed.

**Fix needed:** The changeover page and/or operator canvas should show per-robot swap progress during the Executing phase. Something like: "Robot AMR-03: Swapping Bin A (2 of 4 bins) — picking up old bin." This requires tracking block-level progress from Core's fleet poller.

**Design consideration:** This is an enhancement, not a blocker. The system functions without it — the operator just has less visibility. Can be deferred to a later iteration.

---

## Problem 6 — Executing tracker empty / hangs forever

**Status: SOLVED (via Problem 2 decision)**

**Problem:** The executing tracker was created empty in `handleChangeoverExecuting`. No order IDs were added because the orders were created by Core, not Edge. An empty tracker never fires `onExecutingDone` because `checkDone` is only called from `markComplete`/`markFailed`, which require orders in the map.

**Decision:** Solved by the Problem 2 approach — Edge creates all orders. The executing tracker is populated with the dedicated staging order IDs (reused from staging) plus new swap order IDs created at execute time. Normal order completion events drive the tracker to completion.

---

## Problem 7 — Partial bin state: UOP remaining sync between Edge and Core

**Status: DESIGN DECIDED**

**Problem (reframed):** The original framing was about "old-style payload cleanup" — deactivating stale payload records. But the real issue is deeper: old bins at lineside are **partials** with production value. When a changeover swaps them out and returns them to storage, the bin's actual remaining UOP must travel with it. When that job style runs again later, FIFO pulls the oldest bins first — the partials — and the payload remaining on Edge must reflect the actual bin contents, not a blind reset to full capacity.

This is a **bin state synchronization gap** that changeover brings to light. Edge tracks real-time consumption via PLC counter deltas. Core tracks bin location but never receives updated remaining. The two views of the bin diverge as soon as the first PLC tick fires.

**The gap has two directions:**

### Direction 1: Edge → Core (partial bin leaving lineside)

When a swap order picks up a partial bin from lineside and stores it, Core's `bins.uop_remaining` is stale (still set to full capacity from initial delivery). Core needs the actual remaining so the bin is stored with correct state.

**Decision:** Two mechanisms, one changeover-specific and one general:

**A. Changeover swap items carry remaining:**
- Add `RemainingUOP int` field to `ChangeoverSwapItem` (`protocol/payloads.go`)
- Edge populates from `payload.Remaining` when building swap items in `handleChangeoverExecuting`
- Core's `HandleChangeoverExecute` (`dispatch/changeover.go`) updates `bins.uop_remaining` via `db.RecordBinCount(binID, remainingUOP, "changeover")` or similar before releasing the swap order
- The bin now carries correct remaining when it's stored at outgoing/supermarket

**B. General store orders (future):**
- Any store order that removes a bin from lineside should also communicate remaining
- Can extend `OrderReceipt.FinalCount` semantics or add a field to store order requests
- Not required for changeover MVP but should be addressed — manual store of a partial bin has the same gap

### Direction 2: Core → Edge (partial bin arriving at lineside)

When a partial bin is delivered to lineside (FIFO pulled the oldest bin, which is a partial from a previous changeover), Edge's `resetPayloadOnRetrieve` currently sets remaining to UOP capacity from the catalog. This is wrong for partial bins.

**Decision:** Include actual bin remaining in the delivery notification:

- Add `UOPRemaining int` and `BinLabel string` to `OrderDelivered` (`protocol/payloads.go:114-119`)
- Core populates from `bin.UOPRemaining` when sending the delivered message — the bin is already available via `order.BinID` in `handleOrderDelivered` (`shingo-core/engine/wiring.go:183-203`)
- Edge receives the remaining in `HandleOrderDelivered` (`messaging/edge_handler.go:147-153`)
- Edge stores the delivered UOP remaining on the order record (new column or in-memory)
- `resetPayloadOnRetrieve` (`engine/wiring.go:218-242`) uses the delivered remaining when available; falls back to catalog UOP capacity for full/fresh bins (when `UOPRemaining == 0` or not provided, treat as full)

**This fixes the general case too** — any delivery of a partial bin (not just changeover) correctly sets the payload remaining.

### FIFO ordering

FIFO retrieval sorts by `loaded_at` timestamp. A partial bin returned to storage retains its original `loaded_at` from when it was first loaded. It's the oldest bin, so FIFO naturally pulls it first. **No changes needed for retrieval ordering** — the existing FIFO algorithm already does the right thing. The fix is purely about carrying the correct remaining count.

### Old-style payload records on Edge

The old-style payload records (Edge DB) don't need special cleanup. When the style isn't active, auto-reorder is off and no orders are created — the records are inert. When the style runs again:
- Fresh bins delivered: `resetPayloadOnRetrieve` fires with the actual bin remaining (from the `OrderDelivered` message)
- Partial bins delivered: same path, but remaining is < capacity
- Configuration (reorder point, cycle mode, node assignments) persists across changeovers — only operational state (remaining, status) changes on delivery

### Implementation details

**Protocol changes** (`protocol/payloads.go`):
```
ChangeoverSwapItem:
  + RemainingUOP int `json:"remaining_uop"` — current UOP remaining at the lineside position

OrderDelivered:
  + UOPRemaining int    `json:"uop_remaining,omitempty"` — actual bin contents
  + BinLabel     string `json:"bin_label,omitempty"`      — bin identifier for operator reference
```

**Core changes:**
- `HandleChangeoverExecute` (`dispatch/changeover.go`): Before releasing swap orders, look up the bin at the lineside node for each swap item and call `db.RecordBinCount(binID, item.RemainingUOP, "changeover-swap")` to update `bins.uop_remaining`
- `handleOrderDelivered` (`engine/wiring.go:183-203`): Look up `order.BinID` → `GetBin` → populate `OrderDelivered.UOPRemaining` and `BinLabel` before sending to Edge

**Edge changes:**
- `handleChangeoverExecuting` (`engine/changeover.go`): When building swap items, set `RemainingUOP` from the current `payload.Remaining` for each old-style payload
- `HandleOrderDelivered` (`messaging/edge_handler.go:147-153`): Pass `UOPRemaining` through to the order manager
- Order record or manager needs to carry the delivered UOP remaining from the delivered message to `handleOrderCompleted`
- `resetPayloadOnRetrieve` (`engine/wiring.go:218-242`): If the delivered message included `UOPRemaining > 0`, use that instead of catalog capacity. If `UOPRemaining == 0` or not set (backward compatibility), fall back to catalog capacity (bin is full or message is from an older Core)

**Existing code that already works:**
- `bins.uop_remaining` column exists (`shingo-core/store/schema_sqlite.go:120`)
- `RecordBinCount(binID, actualUOP, actor)` exists (`shingo-core/store/bins.go:357-361`)
- `GetBin(binID)` returns `UOPRemaining` field (`shingo-core/store/bins.go:111-114`)
- `order.BinID` is set during dispatch — available at delivery time
- FIFO sorts by `loaded_at` — already correct for partial bin retrieval order

---

## Problem 8 — Clearing strategy: sweep_to_stage Phase B timing

**Status: DESIGN DECIDED**

**Problem:** In the `sweep_to_stage` clearing strategy, Phase A moves old bins from lineside to clearing nodes. Phase B creates store orders to move bins from clearing nodes to outgoing. When should Phase B orders be dispatched?

**Decision:** Phase B store orders are dispatched when the operator presses "Tooling Done." They run in the background and do NOT block the changeover gate. The bins sit at clearing nodes during the entire tooling phase (which is fine — clearing nodes are designated staging areas). After Tooling Done, the line track marks done, and Phase B orders are fire-and-forget.

**Rationale:** Clearing nodes are near the cell, not in the way of anything. There's no urgency to move bins to outgoing — the important thing is that lineside is clear for tooling. Moving bins to outgoing can happen while the swap is being set up or executed.

---

## Problem 9 — Cancel during clearing phase (and general cancel → redirect flow)

**Status: DESIGN DECIDED**

**Problem:** If the operator cancels during the clearing phase, clearing orders are in-flight. For `direct` strategy, store orders are moving bins to outgoing — those can be aborted and bins stay at lineside (no harm). For `sweep_to_stage`, bins may already be at clearing nodes — aborting leaves bins stranded at clearing nodes, not at lineside and not at outgoing. Need a recovery path.

**Decision:** Cancel is never just "stop" — it's "stop and redirect." The operator's next choice determines what happens to stranded bins. The cancel flow prompts the operator to select what they're changing over into next:

**Cancel → pick different style (C):** Operator went A → B, cancels, picks C.
- Old bins (A) at clearing nodes are no longer needed at lineside
- Continue them to storage: dispatch Phase B store orders (clearing node → outgoing) for any bins already at clearing nodes
- Same as the normal Phase B fire-and-forget path — bins eventually reach outgoing
- New changeover A → C starts immediately

**Cancel → pick same style (A):** Operator went A → B, cancels, picks A (resume).
- Bins at clearing nodes need to return to lineside
- Dispatch return-to-lineside orders: reverse the sweep (clearing node → lineside) for each bin at a clearing node
- Abort any still-in-flight Phase A orders (bins not yet moved stay at lineside, fine)
- Line resumes running with original style A

**Cancel UI flow:**
1. Operator presses Cancel
2. UI prompts: "Select next job style" — dropdown includes all styles including the current from-style
3. If selected style ≠ from-style → old bins continue to storage, new changeover starts
4. If selected style = from-style → bins return to lineside, changeover fully reverts

**Per-strategy behavior on cancel:**

| Strategy | Bins in transit | Bins at clearing nodes | Bins still at lineside |
|----------|----------------|----------------------|----------------------|
| `direct` | Let in-flight store orders complete (bin ends at outgoing — fine either way) | N/A (direct has no clearing nodes) | Stay at lineside |
| `sweep_to_stage` (→ different style) | Let in-flight Phase A complete | Dispatch Phase B store orders (→ outgoing) | Stay at lineside (will be cleared by next changeover) |
| `sweep_to_stage` (→ same style) | Abort in-flight Phase A orders | Dispatch return orders (→ lineside) | Stay at lineside |

**Auto-reorder on cancel:** Not modified. Auto-reorder flags stay as-is throughout changeover (see Problem 3 revised design). The `handlePayloadReorder` guard lifts when changeover ends, and existing per-payload flags take effect.

**Implementation notes:**
- Cancel API needs to accept the next style selection: `POST /api/changeover/cancel` with `{"line_id": int, "next_style": string}`
- If `next_style` = from-style → dispatch return orders + cancel changeover
- If `next_style` ≠ from-style → dispatch Phase B for stranded bins + cancel current changeover + start new changeover (A → next_style)
- The changeover machine's `Cancel()` method may need to be extended or a new `CancelAndRedirect()` method added
- Return-to-lineside orders are simple store orders (clearing node → lineside node, one per stranded bin) — tracked by a temporary tracker or fire-and-forget

---

## Problem 10 — Per-line auto-reorder toggle on operator canvas

**Status: DESIGN DECIDED (implementation pending)**

**Problem:** Operators have no way to manually control auto-reorder from the shop floor. After a cancelled changeover, auto-reorder stays disabled (intentional — see Problem 3) but operators need a way to re-enable it without accessing the admin Material page. More generally, auto-reorder toggle should be accessible from the operator canvas for any situation.

**Decision:** Add a toggle button to the StatusBar element on the operator canvas. The StatusBar already renders at the bottom of every canvas screen showing line name, style name, and connection status. The toggle goes on the right side.

**Design details:**

- **Visual:** 160px wide button in the StatusBar. Three states:
  - **ON (normal):** Green (`#2E7D32`), text "AUTO-REORDER ON" — interactive, clickable
  - **OFF (normal):** Red (`#C62828`), text "AUTO-REORDER OFF" — interactive, clickable
  - **Disabled (changeover active):** Grey (`#555`), text "AUTO-REORDER (CHANGEOVER)" or similar — non-interactive, greyed out. Communicates that auto-reorder is suppressed by changeover regardless of underlying flag values. This is an HMI — operators need to see at a glance that it's not actionable.
- **Scope:** Per-line toggle — affects all payloads on the line's active job style. Uses `SetAutoReorderByJobStyle(activeJobStyleID, enabled)` (new bulk DB method)
- **Hit testing:** Store button bounds in `cfg._autoReorderBtnBounds` during render, same pattern as `hitButton` for OrderCombo action buttons (`render.js:413`)
- **Click handler:** In `display.js`, add check in `onCanvasClick` before the existing ordercombo guard: if hit is statusbar and button hit, call `toggleLineAutoReorder(shape)` which POSTs to `/api/line/auto-reorder`
- **API endpoint:** `POST /api/line/auto-reorder` with `{"line_id": int, "enabled": bool}` — public (no admin auth), looks up line's `ActiveJobStyleID` (`*int64` on `ProductionLine` struct), calls `SetAutoReorderByJobStyle`, emits `EventAutoReorderChanged`
- **SSE flow:** New `EventAutoReorderChanged` event type → SSE event `"auto-reorder-update"` → display.js listener updates `live.auto_reorder` on statusbar shapes matching the line ID
- **Initial state:** Defaults to `true` on canvas load until first SSE event. Same pattern as other live data fields — acceptable brief default.
- **Cursor:** `onCanvasMove` updated to show pointer cursor on the toggle button

**Canvas code references:**
- `drawStatusBar`: `render.js:344-365` — add toggle rendering here
- `hitButton` pattern: `render.js:413-418` — model for `hitStatusBarButton`
- `onCanvasClick`: `display.js:340-371` — add statusbar check
- `onCanvasMove`: `display.js:373-377` — add pointer cursor
- `roundRect` helper: `render.js:383` — for toggle button shape
- StatusBar config has `lineId` field (`shapes.js:38`) — used to determine which line to toggle

---

## Problem 11 — Changeover engine handlers not wired (first slice)

**Status: DESIGN DECIDED (revised 2026-03-21, implementation pending)**

**Problem:** The changeover state machine emits events (`EventChangeoverStarted`, `EventChangeoverCancelled`, `EventChangeoverCompleted`) but no engine handlers respond to them. Nothing actually happens when a changeover starts — no order cancellation, no reorder suppression.

**Decision:** Implement the first slice of changeover engine automation. This is the foundation that all other changeover automation builds on.

**Key design principle (revised):** The changeover system **never modifies auto-reorder flags**. Instead, it suppresses reorder behavior via a guard in `handlePayloadReorder`. See Problem 3 for rationale.

**Implementation plan:**

1. **New DB method** — `store/payloads.go`: `SetAutoReorderByJobStyle(jobStyleID int64, autoReorder bool) (int64, error)`. Single SQL: `UPDATE payloads SET auto_reorder=?, updated_at=datetime('now') WHERE job_style_id=?`. Returns rows affected. **Note:** This is used by the canvas toggle (Problem 10), NOT by changeover lifecycle handlers.

2. **Reorder suppression guard** — `engine/wiring.go`: In `handlePayloadReorder`, before dispatching `RequestOrders`, check if a changeover is active on the payload's line via `e.ChangeoverMachine(lineID).IsActive()`. If active, log and skip — don't create orders. The auto-reorder flag stays as-is in the DB.

3. **New file** — `engine/changeover.go`: Three methods on `*Engine`:
   - `handleChangeoverStarted(evt)`: Cancel all active orders on the line. `ListActiveOrdersByLine(lineID)` (`store/orders.go:85`) and loop `AbortOrder(o.ID)` (`orders/manager.go:364`) for each, logging errors but continuing. Change abort reason to `"changeover initiated"` for traceability. Emit `EventChangeoverActive` so canvas knows to grey out the toggle.
   - `handleChangeoverCancelled(evt)`: Emit `EventChangeoverActive` (active=false) so canvas un-greys the toggle. No auto-reorder changes.
   - `handleChangeoverCompleted(evt)`: Emit `EventChangeoverActive` (active=false) so canvas un-greys the toggle. No auto-reorder changes.

4. **New event type** — `engine/events.go`: `EventChangeoverActive` with struct `ChangeoverActiveEvent{LineID int64, Active bool}`. Used by the canvas to show/hide the disabled state on the auto-reorder toggle.

5. **Wire events** — `engine/wiring.go`: Add 3 subscriptions at end of `wireEventHandlers()` (after line 55) + the reorder guard modification. Same `SubscribeTypes` pattern as existing handlers.

6. **SSE wiring** — `www/sse.go`: Add cases for `EventChangeoverActive` → `"changeover-active"` and `EventAutoReorderChanged` → `"auto-reorder-update"` in `SetupEngineListeners`.

7. **API endpoint** — `www/handlers_api_config.go`: `apiToggleLineAutoReorder` handler (for canvas toggle, not changeover). Accepts `{"line_id": int, "enabled": bool}`. Looks up line's `ActiveJobStyleID`, calls `SetAutoReorderByJobStyle`. Emits `EventAutoReorderChanged`. Returns `{"status":"ok", "affected": N, "enabled": bool}`. Route: `POST /api/line/auto-reorder` in public API section of `www/router.go`.

8. **Canvas** — StatusBar toggle with three visual states (ON/OFF/disabled-during-changeover). See Problem 10.

**Implementation order:**
1. `store/payloads.go` — `SetAutoReorderByJobStyle` (for canvas toggle)
2. `engine/events.go` — `EventChangeoverActive` + `EventAutoReorderChanged` + structs
3. `engine/changeover.go` — new file with 3 handlers
4. `engine/wiring.go` — wire 3 subscriptions + reorder suppression guard
5. `www/sse.go` — changeover-active and auto-reorder SSE events
6. `www/handlers_api_config.go` + `www/router.go` — toggle API
7. `render.js` — StatusBar button with 3 visual states + hit test (Problem 10)
8. `display.js` — click handler (disabled during changeover), SSE listeners for both events (Problem 10)

---

## Problem 12 — Counter delta after Parts Done may corrupt UOP snapshot

**Status: OPEN**

**Problem:** `handleCounterDelta` in `engine/wiring.go` decrements `payload.remaining` on every PLC tick. During the Stopping phase this is correct — the operator is still running parts. But after Parts Done, the old-style payloads' `remaining` values become the source of truth for `ChangeoverSwapItem.RemainingUOP`. Stray PLC ticks after Parts Done (buffer flush, delayed counter reads, electrical noise) would decrement remaining further, making the swap item's UOP count inaccurate. The bin arrives in storage with the wrong remaining, and when FIFO retrieves it later, Edge sets the wrong capacity.

**Recommendation:** Snapshot `payload.remaining` for all old-style payloads at Parts Done time. Store the snapshot in the changeover machine or tracker state. Use the snapshot — not the live value — when building swap items at execute time. This avoids modifying the counter delta handler and keeps the fix local to the changeover path.

**Alternative:** Add a guard in `handleCounterDelta` that skips decrement for old-style payloads after Parts Done. Simpler but couples the counter handler to changeover state.

---

## Problem 13 — Phase B store order failure is invisible

**Status: OPEN**

**Problem:** In `sweep_to_stage`, Phase B store orders are dispatched fire-and-forget after Tooling Done. If a Phase B order fails (robot can't reach outgoing, fleet error, bin mismatch), the bin sits at the clearing node permanently. No tracker monitors these orders, no operator notification fires, and the stranded bin has no visibility in the UI. Over time, clearing nodes accumulate orphaned bins.

**Recommendation:** Add a lightweight failure listener for Phase B orders. Subscribe to `EventOrderFailed` and check if the failed order's pickup node is a clearing node associated with an active (or recently completed) changeover. On match, emit an operator notification — anomaly entry on the changeover page or a toast on the canvas. The bin can stay at the clearing node for manual recovery, but the operator needs to know.

**Scope:** Enhancement, not blocker. Phase B failure is unlikely under normal conditions and the bin is physically accessible at the clearing node.

---

## Problem 14 — Changeover restore doesn't rebuild tracker state

**Status: OPEN**

**Problem:** `Machine.Restore()` rebuilds changeover state from the last log entry, but tracker state (which orders are in-flight for staging/clearing/executing) is not persisted. If Edge crashes during the Staging phase with 3 of 5 staging orders complete, on restart the machine restores to the correct phase but the tracker is empty. Those 3 completed orders won't re-emit completion events. The tracker never reaches "done" and the changeover hangs forever.

**Recommendation:** On restore, derive tracker state from the order table rather than persisting tracker state to disk:
1. Query `ListActiveOrdersByLine(lineID)` to find non-terminal orders
2. Filter to orders created after the changeover started (compare `created_at` to changeover log timestamp)
3. Rebuild the tracker with only those order IDs
4. Immediately call `checkDone()` — if all orders are already terminal, advance the phase

This is robust because the order table is the authoritative record. No new persistence needed.

**Scope:** Blocker for production reliability. A crash during any tracked phase leaves the changeover stuck without this fix.

---

## Problem 15 — No validation that target job style has payloads

**Status: OPEN**

**Problem:** `apiChangeoverStart` accepts any target job style without checking that it has payloads configured in Edge's database. The spec mentions `ValidateChangeoverStart(toJobStyleName)` in the `engine/changeover.go` design but it's not implemented. Starting a changeover to a style with no payloads produces an empty staging tracker (immediately fires "done"), an empty swap item list (execute is a no-op), and the production line switches to a style that can't auto-reorder — a silent misconfiguration.

**Recommendation:** Add validation in `apiChangeoverStart` before calling `Machine.Start()`:
1. Look up the target job style by name → get `jobStyleID`
2. Query `ListPayloadsByJobStyle(jobStyleID)`
3. If empty, return HTTP 400 with `{"error": "Target style has no payloads configured"}`

**Scope:** Small fix, should be included in the first slice. Prevents operator confusion.

---

## Problem 16 — Concurrent changeovers sharing fleet resources

**Status: OPEN (low priority)**

**Problem:** Multiple production lines can run changeovers simultaneously, each creating staging, clearing, and swap orders. If two lines share robots or staging areas, concurrent changeovers could overwhelm the fleet backend or create conflicting robot assignments. The tracker infrastructure correctly isolates per-line state via `lineID:phase` keys, but the fleet backend sees all orders together.

**Recommendation:** Low risk in practice — lines typically have separate staging areas and the fleet backend (RDS) handles robot contention natively. No code change needed for MVP. If problems arise at scale, consider:
- Adding a warning in changeover start validation if another line is mid-changeover using overlapping staging nodes
- A changeover scheduler that serializes changeovers across lines sharing staging resources

**Scope:** Not a blocker. Monitor during first multi-line deployments.

---

## Problem 17 — Operator canvas reload on changeover + sub-station management

**Status: OPEN**

**Problem:** The operator canvas uses the auto-generated path (`/operator/cell/{lineID}`) — it queries `ActiveJobStyleID`, loads that style's payloads, and builds a layout via `generateDefaultLayout()`. This is already per-line and per-style, which is correct. On changeover, the SSE `changeover-update` event triggers a full `location.reload()`, which picks up the new style's payloads. This works but has two issues:

1. **Blunt reload:** The full page reload drops SSE connections, resets all canvas state, and causes a visible flash. On a shop floor HMI this is disruptive. A targeted layout swap via SSE would be smoother.

2. **No sub-station concept:** There's no model for physical operator terminals. Screens are just browser tabs pointed at URLs. Nobody knows which terminal is showing what line, and there's no way to manage them (e.g., "push Line 1 canvas to Terminal 3" or "which terminals are online?").

**Note on designed screens:** The codebase contains a drag-and-drop designer (`designer.js`, `operator-designer.html`), saved screen storage (`operator_screens` table), and a display route (`/operator/display/{screenID}`). This is scaffolding carried over from andon v4 — it's not the intended architecture. The auto-generated path is the real system. The designer code should be evaluated for removal or clearly marked as unused. It should not be extended.

**Design direction:**

**1. Replace page reload with SSE layout push:**
- `handleChangeoverCompleted` already updates `ActiveJobStyleID`
- After that update, build the new layout via `generateDefaultLayout` and send it as a `screen-change` SSE event
- `display.js` receives the event, replaces its shapes array and rebuilds its index maps, and the existing render loop picks up the new shapes on the next frame
- No `location.reload()` — the page stays up, SSE connection stays open, no flash
- Fall back to full reload if the SSE push fails

**2. Sub-station registry (future):**
- New table: `sub_stations {id, name, line_id, display_type, last_seen_at}`
- A sub-station is a physical operator terminal associated with a process (line)
- Sub-stations self-register on SSE connect (client sends line ID in query param, server tracks connection)
- Enables: admin visibility into which terminals are online, push-to-terminal features

**3. Per-style layout overrides (future, optional):**
- The auto-generator already handles different payload counts per style (1-row vs 2-row grid, card sizing)
- If finer control is ever needed, store layout overrides keyed by `(line_id, job_style_id)` that patch the auto-generated base — don't build a separate designer
- Not needed for MVP

**Operator transparency:** The StatusBar already renders `"{lineName} — {styleName}"`. When the layout updates via SSE, the style name updates with it. The operator sees the cards change and the style name change in the same frame.

**Implementation:**
- `engine/changeover.go`: in `handleChangeoverCompleted`, after `SetActiveJobStyle`, call `generateDefaultLayout` for the new style and emit the layout via `EventScreenChange`
- `www/sse.go`: broadcast `screen-change` SSE event with layout JSON
- `display.js`: handle `screen-change` — replace shapes, rebuild index maps (replaces current `location.reload()` handler)
- Sub-station table + self-registration (separate, not required for the layout push)

**Cleanup — remove designed-screen scaffolding:** The following are andon v4 carry-over and not part of the intended architecture. Remove:
- `www/static/operator-canvas/designer.js`
- `www/templates/operator-designer.html`
- `www/templates/operator-home.html`
- `store/operator_screens.go` (and `operator_screens` table from schema)
- `handleOperatorDesigner`, `handleOperatorDisplay`, `handleOperatorScreenList` handlers
- `apiCreateOperatorScreen`, `apiListOperatorScreens`, `apiGetOperatorScreenLayout`, `apiSaveOperatorScreenLayout` API handlers
- Associated routes in `www/router.go`: `/operator/designer`, `/operator/display/{id}`, `/operator` (home), `/api/operator-screens/*`

**Scope:** SSE layout push is a quality-of-life improvement. Sub-station registry is for multi-terminal deployments. Neither blocks changeover MVP.

---

## Summary

| # | Problem | Status | Blocking? |
|---|---------|--------|-----------|
| 1 | Dedicated staging orders never trigger staging done | **SOLVED** | Was blocker |
| 2 | Core-created swap orders invisible to Edge | **SOLVED** (design, impl pending) | Was blocker |
| 3 | Auto-reorder during changeover | **DESIGN DECIDED** (revised) | No — guard-based suppression, flags untouched |
| 4 | Cancel doesn't abort in-flight orders | **PARTIALLY ADDRESSED** | Start-time cancel decided; mid-phase cancel open |
| 5 | No swap order visibility for operators | **OPEN** | No — enhancement |
| 6 | Executing tracker empty / hangs | **SOLVED** (via #2) | Was blocker |
| 7 | Partial bin UOP remaining sync (Edge ↔ Core) | **DESIGN DECIDED** | Yes — incorrect bin state on re-delivery |
| 8 | Sweep_to_stage Phase B timing | **DESIGN DECIDED** | No — Phase B is background |
| 9 | Cancel → redirect flow (clearing phase + general) | **DESIGN DECIDED** | Yes — stranded bins need recovery |
| 10 | Per-line auto-reorder toggle on canvas | **DESIGN DECIDED** (impl pending) | No — operator UX |
| 11 | Engine handlers not wired (first slice) | **DESIGN DECIDED** (revised, impl pending) | Yes — foundation for all automation |
| 12 | Counter delta after Parts Done corrupts UOP snapshot | **OPEN** | Yes — incorrect bin state in storage |
| 13 | Phase B store order failure is invisible | **OPEN** | No — enhancement |
| 14 | Changeover restore doesn't rebuild tracker state | **OPEN** | Yes — crash during tracked phase hangs forever |
| 15 | No validation that target style has payloads | **OPEN** | No — but causes silent misconfiguration |
| 16 | Concurrent changeovers sharing fleet resources | **OPEN** (low priority) | No — fleet handles contention natively |
| 17 | Operator screens not bound to process/style | **OPEN** | No — auto-generated path works; designed screens stale after changeover |

**Next session priorities:** Problem 11 (engine handler wiring — the first slice of real automation), which also delivers Problems 3 (reorder guard), 4 (start-time cancellation), and 10 (canvas toggle). Problem 15 (target style validation) should be included in the first slice — small fix, prevents confusion. Then Problem 7 (UOP remaining sync). Then finish Problem 2 implementation. Problem 12 (counter delta snapshot) should be addressed alongside Problem 7 since both affect UOP accuracy. Problem 14 (restore + tracker) is needed before production deployment. Cancel → redirect flow (Problem 9) and mid-phase cancellation (remainder of Problem 4) integrate with clearing strategy implementation (Problem 8). Problem 17 (screen-per-style binding) should be addressed before production deployments with designed screens. Problems 13 and 16 are enhancements for later.

---

## Overlap Risks

Areas where changeover touches the same concern as an existing system through two different mechanisms. These aren't bugs — they're interaction points that need awareness during implementation to avoid conflicting behavior.

### Overlap 1: Auto-reorder — suppression guard vs. canvas toggle

Two independent mechanisms affect auto-reorder behavior:

- **Suppression guard** (Problem 3): `handlePayloadReorder` checks `ChangeoverMachine(lineID).IsActive()` and skips reorder dispatch. Per-payload `auto_reorder` flags in the DB are **never modified** by changeover.
- **Canvas toggle** (Problem 10): Operator clicks the StatusBar button → `POST /api/line/auto-reorder` → `SetAutoReorderByJobStyle(jobStyleID, enabled)` → **actually flips** `auto_reorder` boolean per payload in the DB.

**Risk:** If the canvas toggle is clicked OFF before a changeover starts, then a changeover runs and completes, the guard lifts — but the flags are still OFF from the operator's earlier toggle. This is correct behavior (the operator explicitly turned it off), but could be confusing if the operator forgot they toggled it.

**Mitigation:** The toggle is greyed out during changeover (Problem 10, disabled state). After changeover completes, the toggle shows its actual DB state (OFF in this case). No code fix needed — the visual state is accurate. Operators need to understand the toggle is their explicit control and changeover doesn't touch it.

### Overlap 2: Order cancellation — start-time vs. mid-phase

Problem 4 describes two cancellation contexts:
- **Start-time** (decided): `handleChangeoverStarted` loops `ListActiveOrdersByLine` + `AbortOrder` for each. Abort reason: `"changeover initiated"`.
- **Mid-phase** (open): If operator cancels during staging/clearing/executing, the changeover's own orders (staging, clearing, swap) also need aborting.

**Risk:** Both contexts use `ListActiveOrdersByLine` + `AbortOrder`, but they need different abort reasons (`"changeover initiated"` vs. `"changeover cancelled"`) for traceability. More critically, aborting a dedicated robot mid-swap (bin picked up, not yet placed) is dangerous — the robot has nowhere to put the bin.

**Mitigation:** Start-time cancel is safe (those are pre-changeover orders). Mid-phase cancel needs phase-aware logic: let the current block complete before aborting remaining blocks. Use distinct abort reasons per context. The mid-phase cancel design is deferred (Problem 4 "still open") — when implemented, it must account for block-level safety.

### Overlap 3: UOP remaining sync — changeover-specific vs. general

Problem 7 introduces two sync mechanisms:
- **`ChangeoverSwapItem.RemainingUOP`** (changeover-specific, Edge → Core): Carries actual remaining when swap items are built during execute.
- **`OrderDelivered.UOPRemaining`** (general, Core → Edge): Carries actual remaining on any delivery, so `resetPayloadOnRetrieve` uses the real count instead of catalog capacity.

**Risk:** The general `OrderDelivered` path fixes the return trip for any delivery. But the outbound sync (`RemainingUOP` on swap items) only fires during changeover execution. A manual store order — operator manually stores a partial bin — has the same Edge → Core gap: Core's `bins.uop_remaining` stays stale.

**Mitigation:** Problem 7 Section B ("General store orders — future") explicitly calls this out. Not a blocker for changeover MVP. Follow-up: add `RemainingUOP` to regular store order payloads so any bin leaving lineside carries its actual count to Core.

---

## Integration Map

Nine integration points where changeover connects to existing systems. Use this to understand the full surface area before implementing.

| # | Integration Point | Direction | Existing System | What Changeover Does | Status |
|---|---|---|---|---|---|
| 1 | **Order Lifecycle** | Edge + Core | `orders/manager.go`, `dispatch/dispatcher.go` | Cancel active orders on start; create staging/clearing/swap orders; track completion via `EventOrderCompleted` | Designed, not wired |
| 2 | **Auto-Reorder Suppression** | Edge | `engine/wiring.go` → `handlePayloadReorder` | Guard checks `ChangeoverMachine(lineID).IsActive()` before dispatching reorder | Designed, not implemented |
| 3 | **Kafka Data Channel** | Edge → Core → Edge | `messaging/edge_handler.go`, `messaging/core_handler.go` | `changeover.execute` / `changeover.execute_ack` subjects through existing `HandleData` switch | Protocol types don't exist yet |
| 4 | **Complex / Staged Orders** | Core dispatch | `dispatch/complex.go`, `fleet/fleet.go` | Dedicated robots use `CreateStagedOrder` with `complete: false`; `ReleaseOrder` appends swap blocks | Infrastructure exists; changeover-specific logic doesn't |
| 5 | **UOP Remaining Sync** | Both directions | `protocol/payloads.go` → `OrderDelivered`; `engine/wiring.go` → `resetPayloadOnRetrieve` | Edge → Core via swap item `RemainingUOP`; Core → Edge via `OrderDelivered.UOPRemaining` | Neither field exists in protocol yet |
| 6 | **SSE / Operator Canvas** | Edge → Browser | `www/sse.go`, `render.js`, `display.js` | `EventChangeoverActive` for toggle greying; `EventAutoReorderChanged` for toggle state | Events not defined yet |
| 7 | **Payload Catalog & Reset** | Edge | `engine/wiring.go` → `resetPayloadOnRetrieve` | Must use delivered UOP remaining instead of blindly resetting to catalog capacity | Not modified yet |
| 8 | **Production Line ActiveJobStyleID** | Edge store | `store/production_lines` table | Updated on changeover completion; drives which payloads are active for auto-reorder | Logic designed, not coded |
| 9 | **Operator Screen Context Switch** | Edge → Browser | `store/operator_screens.go`, `display.js` | On completion, push the new style's designed screen to connected terminals via `EventScreenChange` SSE; sub-stations hot-swap layout | Not implemented (Problem 17) |

### Architecture Note: State Machine Model Gap

The current implementation uses a **7-state linear machine** (running → stopping → counting_out → storing → delivering → counting_in → ready → running). The spec describes a **parallel-track architecture** (staging track + line track + convergence gate). Every integration point above is designed against the parallel model. The first slice (Problem 11) wires handlers to the existing linear events — acceptable tech debt, but plan for re-wiring when the parallel model lands (items 14–15 in the tech spec's implementation order).

---

## Transferable Benefits

Patterns emerging from changeover design that solve general problems across Shingo. These should inform future system work beyond changeover.

### 1. Suppression Guard Pattern → Any "Hold" Mode

The guard in `handlePayloadReorder` — check a runtime condition before dispatching, without modifying persistent configuration — is a general pattern for any mode where behavior should be temporarily suspended.

**How it works:** Don't modify flags (keep operator intent intact). Suppress behavior via a runtime check. Lift the check when the condition clears. Flags resume their configured effect immediately.

**Applicable to:**
- **Line maintenance windows** — suppress auto-reorder during planned maintenance without touching payload configs
- **Shift changes** — hold all dispatch during handoff, resume without reconfiguration
- **Quality holds** — suppress ordering for a specific payload pending investigation
- **Manual override periods** — operator takes manual control, system defers

**Future consideration:** Extract into a general `LineMode` or `DispatchGuard` that checks multiple conditions (changeover active, maintenance active, manual hold, etc.) rather than hardcoding changeover-specific checks in `handlePayloadReorder`.

### 2. UOP Remaining on OrderDelivered → System-Wide Inventory Accuracy

The `OrderDelivered.UOPRemaining` field fixes the general case for any delivery of a partial bin — not just changeover. This is the first time Shingo syncs real-time consumption data from Edge back through Core to a re-delivered bin.

**Applicable to:**
- **Quality hold → return:** Bin flagged for quality, pulled to inspection, returned to storage, later re-delivered. Correct remaining travels with it.
- **Manual store → re-retrieve:** Operator manually stores a half-used bin. Next shift retrieves it and gets the actual remaining, not full capacity.
- **Inventory reporting:** Core's `bins.uop_remaining` becomes accurate, enabling consumption rate analytics and predictive staging.

**Priority:** Consider implementing the `OrderDelivered.UOPRemaining` field early — it improves system-wide accuracy independent of changeover.

### 3. Parallel Track + Convergence Gate → Multi-Concern Orchestration

The staging track + line track + convergence gate pattern cleanly coordinates independent async processes with different completion criteria (automatic vs. operator-driven).

**Applicable to:**
- **Shift handoff:** Material staging for next shift (automatic, background) + current shift wind-down (operator-driven). Gate: both complete → switch shift.
- **Quality containment:** Quarantine track (isolate suspect bins, automatic) + production track (switch to known-good material, operator confirms). Gate: both complete → resume.
- **Batch changeover:** Current batch completion track + next batch material staging track. Gate: both complete → start next batch.

### 4. Order Group Tracker → General Fan-In Monitoring

The `changeoverTracker` — register N order IDs, call `onDone` when all reach terminal state — is a reusable fan-in construct.

**Applicable to:**
- **Batch delivery tracking:** Deliver N bins to a staging area, notify when all arrive.
- **Reshuffle group monitoring:** Track compound order child moves (currently uses a different mechanism in `dispatch/compound.go`).
- **Shift-end material return:** Track all store orders issued at shift end, notify when warehouse is restocked.

**Recommendation:** When implementing the tracker, make it generic (accept any callback, any set of order IDs, any completion predicate) rather than changeover-specific.

### 5. Cancel = Redirect → General Recovery Pattern

The insight that "cancel is never just stop — it's stop and redirect" applies to any abort scenario where physical material is in motion.

**Applicable to:**
- **Cancelled retrieve orders:** Bin is in-transit. Where does it go? Current behavior: robot completes delivery (bin arrives at a node nobody expected). Better: explicit redirect to storage or return to source.
- **Failed reshuffles:** Bins split between lane and shuffle row. Current behavior: lane locked, operator alert. Could benefit from a "recover reshuffle" flow that offers redirect options.
- **Quality diversion:** Mid-delivery quality flag. Redirect to quarantine node instead of original destination.
