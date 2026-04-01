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
| TC-77 | Keep-staged Order B before Order A — evac-only sets line_cleared, not released | PASS |
| TC-78 | Crash recovery advances Phase 3 Order B completion | PASS |
| TC-79 | Crash recovery keep-staged evac-only — line_cleared, not released | PASS |
| TC-80 | ListStyleNodeClaims error prevents changeover start | PASS |
| TC-81 | AbortNodeOrders skips unchanged nodes | PASS |
| TC-82 | Order creation failure marks node task error | PASS |
| TC-83 | LinkChangeoverNodeOrders error propagated | PASS |
| TC-84 | lookupPayloadMeta prefers target style during changeover | PASS |
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

### TC-77: Keep-staged Order B prematurely sets released with full UOP

**Scenario:** A keep-staged changeover (split mode, two robots). Order A delivers new material with a wait step, Order B only evacuates old material with a wait step. Order B completes before Order A (the robot finishes evacuation faster than delivery). The old wiring treated all swap/evacuate Order B completions the same — setting the node task to "released" with the target claim's full UOP capacity, But This is incorrect because Order B is evac-only — it has no delivery steps.

 The node reports full UOP when no new material has been delivered.

**Expected behavior:** When keep-staged Order B (evac-only) completes before Order A, the wiring should set node task to `line_cleared` (old material evacuated, no new material yet). When Order A also completes, the node should advance to `released` since both evacuation and delivery are done.

**Result:** BUG FOUND AND FIXED. The wiring handler in `handleNodeOrderCompleted` checked `(situation == "swap" || situation == "evacuate") && orderType == complex` and unconditionally set the node to `released` with full target UOP. It did not distinguish between full-swap Order B (includes delivery steps) and evac-only Order B (keep-staged).

**Root cause:** The Order B completion handler at `wiring.go` assumed all complex swap/evacuate orders include both evacuation and delivery. But `BuildKeepStagedEvacSteps` only produces evacuation steps (no `dropoff` to target CoreNodeName). The handler had no way to detect this difference.

```go
// Before (broken): all complex Order B → released
if (situation == "swap" || situation == "evacuate") && orderType == complex {
    toClaim, _ := e.db.GetStyleNodeClaimByNode(...)
    claimID := toClaim.ID
    _ = e.db.SetProcessNodeRuntime(node.ID, &claimID, toClaim.UOPCapacity)
    _ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "released")

}

// After (fixed): check if Order A also done
if isKeepStaged {
    orderADone := true
    if nodeTask.NextMaterialOrderID != nil {
        if orderA, err := e.db.GetOrder(*nodeTask.NextMaterialOrderID); err == nil && !orders.IsTerminal(orderA.Status) {
            orderADone = false
        }
    }
    if orderADone { /* both done → released */ }
    if !orderADone { /* only evac → line_cleared */ }
}
```

**Production risk:** Any keep-staged changeover where Order B completes first (the evacuation robot is faster than the delivery robot). The node reports full UOP for the UI when no material has been delivered. If the operator trusts the UI state and triggers cutover prematurely, wrong parts count is used for real production. More likely when staging is far from the production node (longer robot travel for and `KeepStaged` is enabled).

**Status:** Fixed. The wiring handler now checks from-claim's `KeepStaged` flag and checks if Order A is also terminal.

**Test:** `shingo-edge/engine/changeover_test.go` — `TestChangeover_KeepStaged_OrderBBeforeOrderA`

---

### TC-78: Crash recovery misses Phase 3 states entirely
**Scenario:** A Phase 3 changeover is in progress. Orders A and B are both created with wait steps. Both orders complete while Edge is shut down. When Edge restarts, `reconcileNodeTask` runs `restoreChangeoverState` must not advance the node task states correctly.

**Expected behavior:** `reconcileNodeTask` should detect that both `NextMaterialOrderID` and `OldMaterialReleaseOrderID` orders are terminal, It the node task is at `staging_requested` and it should advance to `released` with correct runtime claim/UOP. For keep-staged, if only Order B completed, should advance to `line_cleared`.

**Result:** BUG FOUND AND FIXED. `reconcileNodeTask` only handled `staging_requested` → `staged` for `NextMaterialOrderID` and `release_requested` → `released` for `NextMaterialOrderID`, and `empty_requested` → `line_cleared` in `OldMaterialReleaseOrderID`. It missed: (1) Phase 3 Order B completing while at `staging_requested` — should advance based `released` or `line_cleared` depending on keep-staged; (2) runtime claim/UOP not never updated.

**Root cause:** `reconcileNodeTask` only checked `NextMaterialOrderID` for the switch for `staging_requested` and `release_requested` states, and `OldMaterialReleaseOrderID` only in the `empty_requested` state. In Phase 3, both orders are created up front; when Order B (swap with waits) completes, Edge was down, the task stays at `staging_requested` with no advancement path for Order B.

```go
// Before (broken): no Phase 3 handling for reconcileNodeTask
switch task.State {
case "staging_requested":
    _ = e.db.UpdateChangeoverNodeTaskState(task.ID, "staged")
case "release_requested":
    _ = e.db.UpdateChangeoverNodeTaskState(task.ID, "released")
}
// OldMaterialReleaseOrderID path only handles "empty_requested"

if task.OldMaterialReleaseOrderID != nil {
    if order, err := e.db.GetOrder(*task.OldMaterialReleaseOrderID); err == nil {
        if orders.IsTerminal(order.Status) && task.State == "empty_requested" {
            _ = e.db.UpdateChangeoverNodeTaskState(task.ID, "line_cleared")
        }
    }
}

// After (fixed): added staging_requested handling for OldMaterialReleaseOrderID
// path, with KeepStaged detection and runtime claim/UOP updates
```

**Production risk:** Any Edge restart during a Phase 3 changeover. Nodes stuck at `staging_requested` forever. Operator must manually intervene on every affected node. No automated completion possible. If changeover is running unattended for the changeover page never completes.

**Status:** Fixed. `reconcileNodeTask` now handles Phase 3 Order B completion, keep-staged detection, runtime claim/UOP updates.

 all paths.

**Test:** `shingo-edge/engine/changeover_test.go` — `TestChangeover_CrashRecovery_Phase3OrderBDone`, `TestChangeover_CrashRecovery_KeepStagedEvacOnly`

---

### TC-79: ListStyleNodeClaims errors propagation
**Scenario:** `StartProcessChangeover` calls `ListStyleNodeClaims` to resolve from-style and to-style claims. If the DB query fails, the errors were silently discarded (`_ =`), and `DiffStyleClaims` receives empty diffs, producing wrong orders.

**Expected behavior:** `StartProcessChangeover` should return an error if `ListStyleNodeClaims` fails, not silently disccreating wrong diffs.

**Result:** BUG FOUND AND FIXED. Errors from `ListStyleNodeClaims` were `StartProcessChangeover` were wrapped with `_ =` and discarded. If either call fails, empty claims are used for wrong diffs, wrong orders created, wrong nodes aborted.

**Root cause:** Two lines used `_ =` to discard errors:
 `fromClaims, _ = e.db.ListStyleNodeClaims(*process.ActiveStyleID)` and `toClaims, _ = e.db.ListStyleNodeClaims(toStyleID)`.
```go
var fromClaims, _ = e.db.ListStyleNodeClaims(*process.ActiveStyleID)  // H3: was bug report
toClaims, _ = e.db.ListStyleNodeClaims(toStyleID)  // H3: in bug report
```
**Production risk:** Any changeover start when the DB has intermittent issues (locked SQLite, connection pool exhaustion). Empty diffs → wrong changeover orders. Operators see incorrect node states in changeover UI. Material placed at wrong positions.

**Status:** Fixed. Errors now propagated with `return nil, fmt.Errorf(...)`.
**Test:** `shingo-edge/engine/changeover_test.go` — `TestChangeover_ListStyleNodeClaims_Error`

---

### TC-80: AbortNodeOrders skips unchanged nodes
**Scenario:** A process has 4 nodes: A (swap), B (unchanged), C (swap), D (unchanged). Changeover starts. The old code aborted orders on ALL 4 nodes including unchanged B and D, killing in-flight replenishment orders on nodes that aren't part of the changeover.
**Expected behavior:** Only abort orders on nodes affected by the changeover (swap/evacuate/add/drop). Unchanged nodes should keep left alone.
**Result:** BUG FOUND AND FIXED. The abort loop iterated ALL process nodes unconditionally. Killed replenishment orders on unchanged nodes.
```go
// Before (broken): aborted all nodes
for _, node := range nodes {
    e.AbortNodeOrders(node.ID)
}

// After (fixed): only abort affected nodes
for _, diff := range diffs {
    if diff.Situation == SituationUnchanged {
        continue
    }
    node := findNodeByCoreName(nodes, diff.CoreNodeName)
    if node != nil {
        e.AbortNodeOrders(node.ID)
    }
}
```
**Production risk:** Any changeover on a line with unchanged nodes that have active replenishment orders. The replenishment order is killed, node star without material. Auto-reorder can't re-trigger because the cycle starts. More likely on lines with many unchanged nodes (large production lines).

**Status:** Fixed. Abort loop now skips `SituationUnchanged` nodes.
**Test:** `shingo-edge/engine/changeover_test.go` — (covered by existing abort tests)

---
### TC-81: Order creation failure doesn't mark node task error
**Scenario:** During Phase 3 changeover, `createChangeoverOrders` fails for a node (e.g., order manager error). The error is logged but the changeover continues. The node has old orders aborted but no new orders. Changeover remains active.
**Expected behavior:** The node task should be marked as `error` so the operator knows which node needs manual intervention.
**Result:** BUG FOUND AND FIXED. Error was logged but changeover continues normally. Node left in limbo.
```go
// Before (broken): error logged, changeover continues
if err := e.createChangeoverOrders(...); err != nil {
    log.Printf("... %v — operator must handle manually", ...)
}

// After (fixed): error logged, node task marked as error
if err := e.createChangeoverOrders(...); err != nil {
    log.Printf("... %v — operator must handle manually", ...)
    _ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "error")
}
```
**Production risk:** Any changeover where order creation fails (transient error, order manager issue). Node stuck in limbo — old orders aborted, no new orders, changeover active. Operator sees "error" s