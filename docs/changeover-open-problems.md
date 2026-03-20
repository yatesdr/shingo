# Changeover — Open Problems Tracker

This document tracks known problems with the changeover implementation, their status, and the decisions made for resolved ones. Use as reference when resuming development.

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

## Problem 3 — Cancel doesn't restore auto-reorder

**Status: OPEN**

**Problem:** When a changeover starts, `handleChangeoverStopping` disables `auto_reorder` for all old-style payloads. If the operator cancels, the machine resets to Running but auto-reorder stays disabled. The line resumes "running" but no automatic material replenishment triggers. Silent failure — operator won't notice until bins run empty.

**Fix needed:** Subscribe to `EventChangeoverCancelled` in the engine. On cancel, re-enable `auto_reorder` for the old-style payloads. Call `db.SetAutoReorderByJobStyle(fromStyleID, true)`. The `fromJobStyle` name is available in the `ChangeoverCancelledEvent`.

---

## Problem 4 — Cancel doesn't abort in-flight orders

**Status: OPEN**

**Problem:** If operator cancels during staging, robots keep delivering to staging areas. If cancelled during executing, swap robots keep swapping. The machine state says "running" but robots are still executing changeover work. Staging bins arrive with nobody tracking them. During executing, a robot might be mid-swap — old bin picked up, new bin not yet placed.

**Fix needed:** On cancel, iterate the active tracker's order IDs and call `orderMgr.AbortOrder()` for each. For staging tracker: abort all tracked staging orders. For clearing tracker: abort clearing store orders. For executing: abort swap orders (this one is trickier — aborting a mid-swap order could leave the cell in a bad state). May need a "safe cancel" that lets in-progress blocks finish but prevents the robot from starting the next bin.

**Design consideration:** Aborting dedicated robot orders mid-swap is dangerous. If Robot 3 has picked up old Bin A and is en route to outgoing, aborting would leave the robot holding a bin with nowhere to go. Safest approach: let the current bin swap complete, then cancel remaining bins. This requires block-level tracking, which is complex.

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

## Problem 7 — Old-style payload state cleanup

**Status: OPEN**

**Problem:** After changeover completes, old-style payload records still exist in the database with stale `remaining` counts and `status` values from the previous run. If someone later switches back to the old style, those payloads still have stale state. There's no reset or deactivation of old-style payloads.

**Fix needed:** On changeover completion, deactivate old-style payloads (set status to something like "inactive" or reset remaining to 0). On changeover start, when the target style's payloads are activated, reset their remaining to UOP capacity (fresh bins). This may tie into how payloads are linked to job styles — do payloads persist across changeovers or get recreated?

**Design consideration:** This depends on the broader payload lifecycle model. If the same job style runs again later, should its payloads retain their configuration (reorder point, cycle mode, node assignments) but reset operational state (remaining, status)? That's the likely answer — configuration persists, operational state resets.

---

## Summary

| # | Problem | Status | Blocking? |
|---|---------|--------|-----------|
| 1 | Dedicated staging orders never trigger staging done | **SOLVED** | Was blocker |
| 2 | Core-created swap orders invisible to Edge | **SOLVED** (design, impl pending) | Was blocker |
| 3 | Cancel doesn't restore auto-reorder | **OPEN** | Yes — silent failure |
| 4 | Cancel doesn't abort in-flight orders | **OPEN** | Yes — orphaned robots |
| 5 | No swap order visibility for operators | **OPEN** | No — enhancement |
| 6 | Executing tracker empty / hangs | **SOLVED** (via #2) | Was blocker |
| 7 | Old-style payload state cleanup | **OPEN** | No — stale data |

**Next session priorities:** Problem 3 (quick fix), Problem 4 (needs design for mid-swap safety), then finish Problem 2 implementation.
