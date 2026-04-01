# Changeover Automation & A/B Cycling

## Overview

Changeover automation handles production line tooling changes (style A → style B). Edge wiring advances node task states on order completion: `staging_requested → staged → line_cleared → released`. Auto-staging (Phase 2) fires at changeover start for all swap/add positions. Keep-staged modes handle pre-staged bins with either a single robot (combined) or two robots (split). A/B cycling manages paired consume nodes with `active_pull` switching — only the active node decrements UOP, the inactive holds staged material as buffer.

Edge tests use SQLite in a temp directory — no Docker required.

## Test files

- `engine/changeover_test.go` — full changeover lifecycle, auto-staging, error/retry, cancel, keep-staged (TC-74, 76, 61, 73)
- `engine/wiring_test.go` — A/B cycling and FlipABNode (TC-72)
- `engine/operator_stations_test.go` — order acceptance guards (TC-61)

Run this domain's tests:

```bash
cd shingo-edge
go test -v -run "TestChangeover|TestWiring_ABCycling|TestWiring_FlipABNode|TestCanAcceptOrders" ./engine/ -timeout 60s
```

## Index

| TC | Description | Status |
|----|-------------|--------|
| TC-61 | Queued order fulfilled after changeover starts — wrong payload | FIXED |
| TC-73 | Changeover empty wiring — complex order not recognized as line clear | FIXED |
| TC-74a | Staging order completion advances node task | PASS |
| TC-74b | Empty order completion advances node task | PASS |
| TC-74c | Release order completion advances node task | PASS |
| TC-74d | Full changeover lifecycle | PASS |
| TC-74e | Order failure marks error, retry recovers | PASS |
| TC-74f | Cancel mid-staging aborts orders | PASS |
| TC-74g | Auto-staging fires on changeover start (Phase 2) | PASS |
| TC-76a | Keep-staged combined — single robot clears old + stages new | PASS |
| TC-76b | Keep-staged split — two robots, deliver + evacuate in parallel | PASS |
| TC-76c | Order B before Order A — defensive, documents wiring behavior | PASS |
| TC-72a | A/B cycling — active node decrements, inactive skipped | PASS |
| TC-72b | A/B cycling — FlipABNode switches active_pull correctly | PASS |
| TC-72c | A/B cycling — fallthrough when both nodes inactive | PASS |
| TC-72d | A/B cycling — unpaired node always decrements (backward compat) | PASS |
| TC-72e | FlipABNode rejects unpaired node | PASS |

## Bugs found and fixed

### TC-61: Queued order fulfilled after changeover starts — wrong payload delivered to evacuating node

**Scenario:** A production line runs Style A with a consume node at LINE1-IN. A retrieve order for PART-A (Style A's payload) is submitted, but no bins are available in storage — the order goes to `queued`. The operator then initiates changeover from Style A to Style B. Edge sets `production_state = changeover_active` and begins evacuating old material from LINE1-IN. Later, a PART-A bin is returned to storage by an unrelated order. The fulfillment scanner runs on Core, finds the PART-A bin, and fulfills the queued order — dispatching a robot to deliver old-payload material to a node that is mid-evacuation.

**Expected behavior:** The queued order should be cancelled when changeover starts on its delivery node, so the fulfillment scanner never dispatches old-payload material to a mid-evacuation node.

**Result:** BUG FOUND AND FIXED. The fulfillment scanner has no awareness of changeover state. `ListQueuedOrders()` returns all queued orders with no station/process/changeover filter. `tryFulfill` checks `status == queued` and `CountInFlightOrdersByDeliveryNode` (which explicitly excludes `queued` from its count), then calls `FindSourceBinFIFO` — none of these check production state. Edge's auto-reorder guard (`wiring.go:80-84`) blocks *new* order creation during changeover, but the queued order was created *before* changeover started and bypasses that guard entirely.

**Root cause:** `StartProcessChangeover` created the changeover record and changeover node tasks but never cancelled pre-existing orders on affected nodes. `CancelProcessChangeover` correctly aborted changeover-managed orders on cancellation, but the start path had no equivalent cleanup.

```go
// CancelProcessChangeover (already correct): aborts orders on cancel
for _, task := range nodeTasks {
    for _, orderID := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
        if orderID == nil { continue }
        order, _ := e.db.GetOrder(*orderID)
        if !orders.IsTerminal(order.Status) {
            e.orderMgr.AbortOrder(order.ID)
        }
    }
}

// StartProcessChangeover (was missing): no abort at start
// After creating changeover record, returned without cleaning up
```

**Fix:** Added order abortion to `StartProcessChangeover` (`shingo-edge/engine/operator_stations.go`). After creating the changeover record, the function iterates all process nodes, checks runtime state for active/staged order references, and aborts non-terminal orders via `orderMgr.AbortOrder`. The abort enqueues an `OrderCancel` to Core before the local transition, guaranteeing Core receives the cancellation. Runtime order references are cleared. This mirrors `CancelProcessChangeover`'s abort pattern but runs at start instead of cancellation.

**Note:** This fix is on Edge, not Core. The fleet simulator tests Core dispatch behavior. A simulator test for this scenario would require Edge simulation (documented in the "Future: Edge simulation" section). The fix prevents the problem at the source — Edge cancels the stale queued order before Core's fulfillment scanner can dispatch it.

**Production risk:** Any changeover on a line with queued orders. More likely when inventory is low (orders queue because nothing is available) and a changeover is initiated (shift change, product mix change). Three failure modes without the fix: (1) wrong material delivered to a node that just changed over — potential quality incident; (2) stale delivery node — `ApplyBinArrival` places a PART-A bin at a node now expecting PART-B; (3) wasted robot trip during the changeover window when robots are needed for evacuation/restock.

**Status:** Fixed. `StartProcessChangeover` now aborts pre-existing orders on affected nodes before returning.

**Test:** `shingo-edge/engine/operator_stations_test.go` — `TestCanAcceptOrders` (7 subtests: no orders, no runtime, active order, staged order, terminal order, changeover active, changeover completed). `shingo-edge/engine/operator_stations.go` — `StartProcessChangeover` uses `AbortNodeOrders`; auto-reorder/auto-relief guards in `wiring.go` use `CanAcceptOrders`.

---

### TC-73: Changeover empty wiring — complex order not recognized as line clear

**Scenario:** During changeover, the operator clicks "Empty" on a position whose from-claim has outbound staging configured. `EmptyNodeForToolChange` creates a complex order (pickup from production node, dropoff at outbound staging) and sets the node task to `empty_requested`. The complex order completes successfully. The wiring should advance the node task to `line_cleared`.

**Expected behavior:** `handleNodeOrderCompleted` matches the completed order against the node task's `OldMaterialReleaseOrderID`, recognizes it as a line-clear completion, and advances the state to `line_cleared`.

**Result:** BUG FOUND. The node task stayed at `empty_requested` forever. The wiring never matched the completed order because `handleNodeOrderCompleted` checked `order.OrderType == orders.TypeMove` for the line-clear path. `EmptyNodeForToolChange` creates a `complex` order when the from-claim has outbound staging (using `BuildReleaseSteps`), not a `move` order. The `TypeMove` check rejected it.

**Root cause:** The line-clear match in `handleNodeOrderCompleted` (`wiring.go` line 149) was too restrictive. It only accepted move orders, but `EmptyNodeForToolChange` has two paths: (1) claim-based release via `CreateComplexOrder` when `OutboundStaging` is configured (produces `complex` type), and (2) fallback via `ReleaseNodeEmpty`/`ReleaseNodePartial` (produces `move` type). The wiring only handled path 2.

```go
// Before (broken): only matches move orders
if nodeTask != nil && nodeTask.OldMaterialReleaseOrderID != nil && *nodeTask.OldMaterialReleaseOrderID == order.ID &&
    order.OrderType == orders.TypeMove {

// After (fixed): matches both move and complex orders
if nodeTask != nil && nodeTask.OldMaterialReleaseOrderID != nil && *nodeTask.OldMaterialReleaseOrderID == order.ID &&
    (order.OrderType == orders.TypeMove || order.OrderType == orders.TypeComplex) {
```

**Production risk:** Any changeover where a from-claim has `outbound_staging` configured. The node task gets stuck at `empty_requested` permanently. The operator sees the empty order complete in the kanbans view but the changeover page never advances past "emptying." In a manual workflow, the operator can work around it (the material is physically gone). In an automated workflow (Phase 3), this would block the convergence gate indefinitely — the system would never auto-release or auto-switch, halting the entire changeover.

**Status:** Fixed. The line-clear match now accepts both `TypeMove` and `TypeComplex`.

**Test:** `shingo-edge/engine/changeover_test.go` — `TestChangeover_EmptyCompletion`

---

## Verified scenarios

### TC-74a: Staging order completion advances node task — PASS

**Scenario:** A changeover is started and a position is staged (material ordered to inbound staging). The staging order completes. The wiring should advance the node task from `staging_requested` to `staged`.

**Expected behavior:** `handleNodeOrderCompleted` matches the completed order against the node task's `NextMaterialOrderID`, recognizes the delivery node matches the to-claim's `InboundStaging`, and updates the state to `staged`.

**Result:** PASS. The node task advances to `staged` on staging order completion.

**Test:** `engine/changeover_test.go` — `TestChangeover_StagingCompletion`

---

### TC-74b: Empty order completion advances node task — PASS

**Scenario:** During changeover, a position has been staged and the operator empties it (removes old material). The empty order completes. The wiring should advance the node task from `empty_requested` to `line_cleared`.

**Expected behavior:** `handleNodeOrderCompleted` matches the completed order against the node task's `OldMaterialReleaseOrderID` and advances state to `line_cleared`.

**Result:** PASS (after fixing the `TypeMove`-only check — see "Changeover empty wiring" in Bugs Found). The wiring now accepts both `TypeMove` and `TypeComplex` for line-clear matching.

**Test:** `engine/changeover_test.go` — `TestChangeover_EmptyCompletion`

---

### TC-74c: Release order completion advances node task — PASS

**Scenario:** During changeover, a position has been cleared and the staged material is released into production. The release order completes. The wiring should advance the node task to `released`.

**Expected behavior:** `handleNodeOrderCompleted` matches the completed order against the node task's `NextMaterialOrderID` (now used for release), updates runtime to the target claim, and advances state to `released`.

**Result:** PASS. The node task advances to `released` and `tryCompleteProcessChangeover` fires.

**Test:** `engine/changeover_test.go` — `TestChangeover_ReleaseCompletion`

---

### TC-74d: Full changeover lifecycle — PASS

**Scenario:** A complete changeover from start to finish: auto-stage (Phase 2), drive staging order to completion, empty, drive empty order to completion, release, drive release order to completion, switch node to target, cutover production. This tests the entire happy path.

**Expected behavior:** After all steps, the changeover should be completed, the process should be at `active_production`, and the active style should be the target style.

**Result:** PASS. The full lifecycle completes cleanly. Auto-staging fires at changeover start, all wiring transitions work, `CompleteProcessProductionCutover` sets the active style and completes the changeover.

**Test:** `engine/changeover_test.go` — `TestChangeover_FullLifecycle`

---

### TC-74e: Order failure marks error, retry recovers — PASS

**Scenario:** During changeover, a staging order fails (bin unavailable, Core error, etc.). The wiring should mark the node task as `error`. The operator retries staging. The retry should succeed because the failed order is terminal.

**Expected behavior:** On failure: node task state becomes `error`. On retry: a new staging order is created (the old one is terminal so `ensureNodeTaskCanRequestOrder` allows it), and completing the new order recovers the state to `staged`.

**Result:** PASS. Error state is set on failure. Retry creates a new order and completes normally.

**Test:** `engine/changeover_test.go` — `TestChangeover_OrderFailure`

---

### TC-74f: Cancel mid-staging aborts orders — PASS

**Scenario:** A changeover is started and auto-staging creates an in-flight order. The operator cancels the changeover before staging completes. All in-flight orders should be aborted, all node tasks should be cancelled, and the process should revert to `active_production`.

**Expected behavior:** `CancelProcessChangeover` aborts linked orders on all node tasks, marks tasks as `cancelled`, and resets production state.

**Result:** PASS. The staging order is aborted (terminal status), node tasks are cancelled, and the changeover record is marked `cancelled`. Production state reverts to `active_production`.

**Test:** `engine/changeover_test.go` — `TestChangeover_CancelMidStaging`

---

### TC-74g: Auto-staging fires on changeover start (Phase 2) — PASS

**Scenario:** A changeover is started on a process with a swap position (different payload between from-style and to-style). In Phase 2, `StartProcessChangeover` should automatically call `StageNodeChangeoverMaterial` for all swap/add positions instead of requiring the operator to click "Stage" per position.

**Expected behavior:** After `StartProcessChangeover` returns, the node task should already be at `staging_requested` (not `swap_required`) with a `NextMaterialOrderID` linked.

**Result:** PASS. The auto-staging loop iterates diffs, filters for swap/add situations, resolves process node IDs, and calls existing `StageNodeChangeoverMaterial`. Failures are logged, not blocking — operator can retry manually.

**Test:** `engine/changeover_test.go` — `TestChangeover_AutoStaging`

---

### TC-76a: Keep-staged combined — single robot clears old + stages new — PASS

**Scenario:** From-claim has `KeepStaged=true` and `SwapMode="simple"` (default). The staging area has a pre-staged bin from the old style that must be cleared during changeover. In combined mode, a single robot handles clearing the old staged bin, picking the new material, and delivering it — all in one complex order (Order A). A separate Order B handles evacuating old material from the lineside node.

**Expected behavior:** `createChangeoverOrders` detects `diff.FromClaim.KeepStaged == true`, routes to `createKeepStagedChangeoverOrders`, hits the `default` (combined) branch. Two complex orders created: Order A with `BuildKeepStagedCombinedSteps` (clear + pick + deliver), Order B with `BuildKeepStagedEvacSteps` (evacuate old).

**Result:** PASS. Combined path creates both orders correctly with proper step sequences.

**Test:** `engine/changeover_test.go` — `TestChangeover_KeepStagedCombined`

---

### TC-76b: Keep-staged split — two robots handle deliver + evacuate — PASS

**Scenario:** Same as TC-76a but `SwapMode="two_robot"`. Two robots work in parallel: one delivers new material via the staging area, the other evacuates old material.

**Expected behavior:** `createKeepStagedChangeoverOrders` hits the `"two_robot"` branch. Order A uses `BuildKeepStagedDeliverSteps` (deliver new material), Order B uses `BuildKeepStagedEvacSteps` (evacuate old).

**Result:** PASS. Split path creates parallel orders with correct step builders.

**Test:** `engine/changeover_test.go` — `TestChangeover_KeepStagedSplit`

---

### TC-76c: Order B before Order A — defensive test — PASS

**Scenario:** Defensive test documenting what happens if Order B (swap/evacuate) completes before Order A (staging). This can happen if the robot for Order B finishes faster than the staging robot.

**Expected behavior:** The wiring in `handleNodeOrderCompleted` sets the changeover node task to "released" when Order B completes, regardless of Order A's state. This is the current behavior — documented, not necessarily ideal, but safe because the operator still controls the final cutover.

**Result:** PASS. Wiring correctly handles out-of-order completion. The node task transitions to "released" on Order B completion.

**Test:** `engine/changeover_test.go` — `TestChangeover_OrderBBeforeOrderA`

---

### TC-72a: A/B cycling — active node decrements, inactive skipped — PASS

**Scenario:** Two consume nodes (A and B) paired via `PairedCoreNode`. Node A has `active_pull=true`, Node B has `active_pull=false`. A counter delta event fires for the process.

**Expected behavior:** `handleCounterDelta` checks `claim.PairedCoreNode != "" && !runtime.ActivePull` — Node A passes (active, decrements), Node B is skipped (inactive, holds staged material).

**Result:** PASS. Delta of 5 decrements Node A from 80→75, Node B stays at 80.

**Test:** `engine/wiring_test.go` — `TestWiring_ABCycling_ActiveNodeDecrements`

---

### TC-72b: FlipABNode switches active_pull correctly — PASS

**Scenario:** Operator calls `FlipABNode(nodeBID)` to switch active pull from A to B, then `FlipABNode(nodeAID)` to switch back.

**Expected behavior:** `FlipABNode` sets `active_pull=true` on the target node and `active_pull=false` on the partner. Validates that the node has a `PairedCoreNode` set. Also triggers auto-reorder on the depleted partner if UOP is at or below reorder point.

**Result:** PASS. Both directions flip correctly. Unpaired nodes are rejected with an error.

**Test:** `engine/wiring_test.go` — `TestWiring_FlipABNode_SwitchesActivePull`

---

### TC-72c: A/B cycling — fallthrough when both nodes inactive — PASS

**Scenario:** Edge case where both paired nodes have `active_pull=false` (shouldn't happen in normal operation, but defensive). A counter delta fires.

**Expected behavior:** The fallthrough safety net in `handleCounterDelta` detects that no paired consume node was decremented, and decrements the first inactive paired node found. This is the "count to lineside storage" case — production hasn't stopped, so the delta must go somewhere.

**Result:** PASS. Total remaining across both nodes = 153 (160 - 7), confirming exactly one node was decremented.

**Test:** `engine/wiring_test.go` — `TestWiring_ABCycling_FallthroughBothInactive`

---

### TC-72d: Unpaired node always decrements (backward compatibility) — PASS

**Scenario:** A regular consume node without `PairedCoreNode` set. Verifies that the A/B cycling guard doesn't affect unpaired nodes.

**Expected behavior:** The guard `claim.PairedCoreNode != "" && !runtime.ActivePull` evaluates to false when `PairedCoreNode=""`, so the node always decrements regardless of `active_pull` value.

**Result:** PASS. Unpaired node decrements from 60→56 with delta of 4.

**Test:** `engine/wiring_test.go` — `TestWiring_ABCycling_UnpairedNodeAlwaysDecrements`

---

### TC-72e: FlipABNode rejects unpaired node — PASS

**Scenario:** `FlipABNode` called on a node that has no `PairedCoreNode` set.

**Expected behavior:** Returns an error "node X is not part of an A/B pair".

**Result:** PASS. Error returned, no state changed.

**Test:** `engine/wiring_test.go` — `TestWiring_FlipABNode_RejectsUnpairedNode`
