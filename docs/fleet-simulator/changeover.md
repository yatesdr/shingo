# Changeover Automation & A/B Cycling

## Overview

Changeover automation handles production line tooling changes (style A → style B). Edge wiring advances node task states on order completion: `staging_requested → staged → line_cleared → released`. Auto-staging (Phase 2) fires at changeover start for all swap/add positions. Keep-staged modes handle pre-staged bins with either a single robot (combined) or two robots (split). A/B cycling manages paired consume nodes with `active_pull` switching — only the active node decrements UOP, the inactive holds staged material as buffer.

Edge tests use SQLite in a temp directory — no Docker required.

## Test files

- `engine/changeover_test.go` — full changeover lifecycle, auto-staging, error/retry, cancel, keep-staged (TC-74, 76, 61, 73)
- `engine/wiring_test.go` — A/B cycling and FlipABNode (TC-72)
- `engine/operator_stations_test.go` — order acceptance guards (TC-61)
- `engine/changeover_diff_test.go` — DiffStyleClaims pure function unit tests (TC-86 through TC-89)
- `engine/step_builders_test.go` — Build*Steps pure function unit tests (TC-90)

Run this domain's tests:

```bash
cd shingo-edge
go test -v -run "TestChangeover|TestWiring_ABCycling|TestWiring_FlipABNode|TestCanAcceptOrders|TestDiffStyleClaims|TestBuildSwapChangeover|TestBuildEvacuateChangeover|TestBuildKeepStaged|TestBuildStage|TestBuildRelease|TestBuildRestore" ./engine/ -timeout 60s
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
| TC-85 | A/B cycling UI — paired node field hidden for produce role | FIXED |
| TC-86 | DiffStyleClaims double-`__empty__` returns Drop instead of Unchanged | FIXED |
| TC-87a | DiffStyleClaims — swap (different payload) | PASS |
| TC-87b | DiffStyleClaims — evacuate (same payload + evacuate flag) | PASS |
| TC-87c | DiffStyleClaims — unchanged (same payload, no evacuate) | PASS |
| TC-87d | DiffStyleClaims — add (node only in to-style) | PASS |
| TC-87e | DiffStyleClaims — drop (node only in from-style) | PASS |
| TC-87f | DiffStyleClaims — to `__empty__` (explicitly clear node) | PASS |
| TC-87g | DiffStyleClaims — from `__empty__` (was empty, needs material) | PASS |
| TC-87h | DiffStyleClaims — new node with `__empty__` (stays unchanged) | PASS |
| TC-87i | DiffStyleClaims — both `__empty__` (nothing to do) | PASS |
| TC-87j | DiffStyleClaims — changeover role from-side (forces evacuate) | PASS |
| TC-87k | DiffStyleClaims — changeover role to-side (forces evacuate) | PASS |
| TC-87l | DiffStyleClaims — changeover role both sides (always evacuates) | PASS |
| TC-87m | DiffStyleClaims — role change with same payload → swap | PASS |
| TC-87n | DiffStyleClaims — evacuate flag ignored when payload differs | PASS |
| TC-88 | DiffStyleClaims — multi-node mixed situations in one call | PASS |
| TC-89 | DiffStyleClaims — nil from-claims (first-ever changeover) | PASS |
| TC-90a | BuildSwapChangeoverSteps — 8 steps, 1 wait, correct nodes | PASS |
| TC-90b | BuildEvacuateChangeoverSteps — 9 steps, 2 waits, correct nodes | PASS |
| TC-90c | BuildKeepStagedDeliverSteps — 5 steps, 1 wait | PASS |
| TC-90d | BuildKeepStagedEvacSteps — 4 steps, 1 wait | PASS |
| TC-90e | BuildKeepStagedCombinedSteps — 7 steps, 1 wait, clear-then-stage | PASS |
| TC-90f | BuildStageSteps — source → staging route | PASS |
| TC-90g | BuildStageSteps — no InboundStaging → nil | PASS |
| TC-90h | BuildReleaseSteps — core → destination | PASS |
| TC-90i | BuildReleaseSteps — missing OutboundDestination (payload fallback) | PASS |
| TC-90j | BuildRestoreSteps — outbound staging → core | PASS |
| TC-90k | BuildRestoreSteps — no OutboundStaging → nil | PASS |
| TC-90l | BuildStageSteps — missing InboundSource (payload fallback) | PASS |
| TC-90m | BuildKeepStagedDeliverSteps — missing InboundSource (payload fallback) | PASS |
| TC-91 | SituationDrop lifecycle — evacuation-only order, no Order A | PASS |
| TC-92 | Multi-node changeover — 4 nodes with swap/unchanged/drop/add | PASS |
| TC-93 | Changeover-only role — evacuate and restore | PASS |
| TC-94 | Double changeover — complete one, start another, clean state | PASS |
| TC-95 | No claim changes — all unchanged, effective no-op changeover | PASS |
| TC-96 | Cancel mid-release — Order B in transit gets aborted | PASS |
| TC-97 | Order A fails — staging failure → error → retry succeeds | PASS |
| TC-98 | Order B fails — swap order failure → error → manual recovery | PASS |
| TC-99 | Partial completion — 2 of 3 nodes done, 1 errors, cutover blocked | PASS |
| TC-100 | Cutover completion — switch + cutover sets active style | PASS |
| TC-101 | Keep-staged + evacuate — both flags on same claim | PASS |
| TC-102 | Keep-staged from → non-keep-staged to — from-claim drives handler | PASS |
| TC-103 | Keep-staged missing staging config — falls back to simple staging | PASS |
| TC-104 | A/B flip during changeover — flip succeeds, orders blocked | PASS |
| TC-105 | A/B produce pair — active increments, inactive skipped | FIXED |
| TC-106 | A/B flip + immediate delta — race-free sequencing | PASS |
| TC-107 | A/B pairs across styles — unpaired after changeover, both decrement | PASS |
| TC-108 | A/B asymmetric pair — fallthrough hits inactive node | PASS |

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
**Production risk:** Any changeover where order creation fails (transient error, order manager issue). Node stuck in limbo — old orders aborted, no new orders, changeover active. Operator sees no progress and has no indication of what went wrong.

**Status:** Fixed. Node task set to `error` state on order creation failure.

**Test:** `shingo-edge/engine/changeover_test.go` — `TestChangeover_OrderFailure`

---

### TC-85: A/B cycling UI — paired node field hidden for produce role

**Scenario:** An operator configures a produce node with an A/B pair on the processes page (`/processes` → Node Claims → Edit Claim). The A/B Node Cycling fieldset is not visible when the role is set to "produce" — only "consume" shows it. Produce nodes that alternate (e.g., two output stations) cannot be configured as A/B pairs through the UI.

**Expected behavior:** The A/B Node Cycling fieldset should be visible for both consume and produce roles. Only bin_loader and changeover roles should hide it.

**Result:** BUG FOUND AND FIXED. The `toggleClaimsAddPayload()` function in `processes.js` checked `role === 'consume'` to show the fieldset. Changed to `role === 'consume' || role === 'produce'`.

**Root cause:** Initial implementation assumed A/B cycling was consume-only (alternating pull points). But produce nodes can also alternate (e.g., two output conveyors taking turns).

```js
// Before (broken): only consume
var isConsume = role === 'consume';
document.getElementById('claims-ab-fieldset').style.display = isConsume ? '' : 'none';

// After (fixed): consume and produce
var showAB = role === 'consume' || role === 'produce';
document.getElementById('claims-ab-fieldset').style.display = showAB ? '' : 'none';
```

**Production risk:** Low — configuration only. No A/B produce pairs could be set up through the UI, but the backend already supported them. Workaround was direct API calls.

**Status:** Fixed in `processes.js` and `processes.html` helper text updated.

**Test:** Manual UI verification — change role dropdown in claim modal, verify A/B fieldset visibility.

---

### TC-86: DiffStyleClaims double-`__empty__` returns SituationDrop instead of SituationUnchanged

**Scenario:** Both the from-style and to-style declare a node with `PayloadCode = "__empty__"`. The from-style says "this node was empty"; the to-style says "this node should be empty." Nothing changes — the node was empty and stays empty.

**Expected behavior:** `DiffStyleClaims` should return `SituationUnchanged` for this node — no robot action needed.

**Result:** BUG FOUND AND FIXED. The function returned `SituationDrop`. The switch statement checks `to.PayloadCode == "__empty__"` (case 3) before considering whether `from` is also `__empty__`. This triggered a spurious "drop" action: the system would try to evacuate material from a node that is already empty.

**Root cause:** The `to.PayloadCode == "__empty__"` guard at `changeover.go:77` didn't check if `from` was also `__empty__`. It unconditionally classified the node as "explicitly clear the node" without considering that there's nothing to clear.

```go
// Before (broken): both empty → Drop (spurious evacuation)
case to != nil && to.PayloadCode == "__empty__":
    situation = SituationDrop // explicitly clear the node

// After (fixed): both empty → Unchanged (nothing to do)
case to != nil && to.PayloadCode == "__empty__":
    if from != nil && from.PayloadCode == "__empty__" {
        situation = SituationUnchanged // both empty → nothing to do
    } else {
        situation = SituationDrop // explicitly clear the node
    }
```

**Production risk:** Any changeover where both styles declare the same node as `__empty__`. The system would create a spurious evacuation order for an empty node. The robot arrives, finds nothing to pick up, and either fails or wastes time. On a tightly choreographed changeover (many nodes, limited robots), a wasted robot trip delays the entire changeover.

**Status:** Fixed. `DiffStyleClaims` now checks if from is also `__empty__` before classifying as Drop.

**Test:** `shingo-edge/engine/changeover_diff_test.go` — `TestDiffStyleClaims_BothEmpty`

---

### TC-87: DiffStyleClaims — single-node situation classification

**Scenario:** `DiffStyleClaims` is the brain of the changeover system — it classifies each node's transition between two styles into one of five situations (swap, evacuate, unchanged, add, drop). Every branch of its switch statement must produce the correct classification for the correct reason.

**Expected behavior:** Each situation is classified correctly based on payload codes, roles, the evacuate flag, and the changeover-only role.

**Result:** PASS. All 14 branches tested and classified correctly:

| Sub | Test | From | To | Expected |
|-----|------|------|----|----------|
| a | Swap | PART-A, consume | PART-B, consume | swap |
| b | Evacuate | PART-A, consume | PART-A, consume + evacuate_on_changeover | evacuate |
| c | Unchanged | PART-A, consume | PART-A, consume | unchanged |
| d | Add | (absent) | PART-A, consume | add |
| e | Drop | PART-A, consume | (absent) | drop |
| f | To empty | PART-A, consume | `__empty__`, consume | drop |
| g | From empty | `__empty__`, consume | PART-A, consume | add |
| h | New empty | (absent) | `__empty__`, consume | unchanged |
| i | Both empty | `__empty__`, consume | `__empty__`, consume | unchanged |
| j | CO role from | PART-A, changeover | PART-A, consume | evacuate |
| k | CO role to | PART-A, consume | PART-A, changeover | evacuate |
| l | CO both | PART-A, changeover | PART-A, changeover | evacuate |
| m | Role change | PART-A, consume | PART-A, produce | swap |
| n | Evac flag ignored | PART-A, consume | PART-B, consume + evacuate_on_changeover | swap |

**Test:** `shingo-edge/engine/changeover_diff_test.go` — `TestDiffStyleClaims_Swap`, `TestDiffStyleClaims_Evacuate`, `TestDiffStyleClaims_Unchanged`, `TestDiffStyleClaims_Add`, `TestDiffStyleClaims_Drop`, `TestDiffStyleClaims_ToEmpty`, `TestDiffStyleClaims_FromEmpty`, `TestDiffStyleClaims_ToEmptyNewNode`, `TestDiffStyleClaims_BothEmpty`, `TestDiffStyleClaims_ChangeoverRole_FromSide`, `TestDiffStyleClaims_ChangeoverRole_ToSide`, `TestDiffStyleClaims_ChangeoverRoleOverridesNoEvacuate`, `TestDiffStyleClaims_RoleChange`, `TestDiffStyleClaims_EvacuateFlagIgnoredOnPayloadChange`

---

### TC-88: DiffStyleClaims — multi-node mixed situations in one call

**Scenario:** A production line has 6 nodes transitioning between styles: one swap (different payload), one unchanged (same payload, no evacuate), one evacuate (same payload + evacuate flag), one drop (only in from-style), one add (only in to-style), and one changeover-only node (forces evacuate). All nodes are processed in a single `DiffStyleClaims` call.

**Expected behavior:** All 6 nodes are correctly classified with their distinct situations in one invocation. Claim pointers (FromClaim, ToClaim) are correctly set to nil for add/drop situations.

**Result:** PASS. All 6 nodes classified correctly. FromClaim is nil for add, ToClaim is nil for drop.

**Test:** `shingo-edge/engine/changeover_diff_test.go` — `TestDiffStyleClaims_MultiNode`

---

### TC-89: DiffStyleClaims — nil from-claims (first-ever changeover)

**Scenario:** A production line has never had a style activated (`ActiveStyleID` is nil). The first-ever changeover passes `fromClaims = nil` to `DiffStyleClaims`. Every node in the target style should be classified as `SituationAdd` (or `SituationUnchanged` for `__empty__` nodes).

**Expected behavior:** All nodes classified as Add. FromClaim is nil for all diffs. No panic from nil slice.

**Result:** PASS. Both nodes classified as Add with nil FromClaim.

**Test:** `shingo-edge/engine/changeover_diff_test.go` — `TestDiffStyleClaims_NilFromClaims`

---

### TC-90: Step builder unit tests — exact pickup/dropoff/wait sequences

**Scenario:** The `Build*Steps` functions are pure functions that construct robot order step sequences from `StyleNodeClaim` routing config. They are the building blocks for every complex order in the system — swap changeovers, evacuate changeovers, keep-staged flows, staging, release, and restore. A wrong step sequence sends the robot to the wrong node or misses a wait gate.

**Expected behavior:** Each builder returns the exact step sequence with the correct number of wait steps, correct node names, and correct action order. Edge cases (missing routing fields) produce valid steps with payload-based fallback (empty Node field) or nil (feature unavailable).

**Result:** PASS. All 13 builder variants verified:

| Sub | Builder | Steps | Waits | Key check |
|-----|---------|-------|-------|-----------|
| a | BuildSwapChangeoverSteps | 8 | 1 | pre-position → wait → evac → park → grab new → deliver → grab old → final |
| b | BuildEvacuateChangeoverSteps | 9 | 2 | same as swap + extra wait for tooling |
| c | BuildKeepStagedDeliverSteps | 5 | 1 | source → stage → wait → grab → deliver |
| d | BuildKeepStagedEvacSteps | 4 | 1 | pre-position → wait → evac → final (no staging hop) |
| e | BuildKeepStagedCombinedSteps | 7 | 1 | clear old staged → return to source → grab new → stage → wait → grab → deliver |
| f | BuildStageSteps | 2 | 0 | source → inbound staging |
| g | BuildStageSteps (no staging) | nil | — | InboundStaging empty → cannot pre-stage |
| h | BuildReleaseSteps | 2 | 0 | core node → outbound destination |
| i | BuildReleaseSteps (no dest) | 2 | 0 | OutboundDestination empty → dropoff with no Node (payload fallback) |
| j | BuildRestoreSteps | 2 | 0 | outbound staging → core node |
| k | BuildRestoreSteps (no staging) | nil | — | OutboundStaging empty → nothing to restore |
| l | BuildStageSteps (no source) | 2 | 0 | InboundSource empty → pickup with no Node (payload fallback) |
| m | BuildKeepStagedDeliverSteps (no source) | 5 | 1 | first pickup has empty Node (payload fallback) |

**Test:** `shingo-edge/engine/step_builders_test.go` — `TestBuildSwapChangeoverSteps`, `TestBuildEvacuateChangeoverSteps`, `TestBuildKeepStagedDeliverSteps`, `TestBuildKeepStagedEvacSteps`, `TestBuildKeepStagedCombinedSteps`, `TestBuildStageSteps`, `TestBuildStageSteps_NoInboundStaging`, `TestBuildReleaseSteps`, `TestBuildReleaseSteps_MissingDestination`, `TestBuildRestoreSteps`, `TestBuildRestoreSteps_NoOutboundStaging`, `TestBuildStageSteps_MissingInboundSource`, `TestBuildKeepStagedDeliverSteps_MissingInboundSource`

---

### TC-91: SituationDrop lifecycle — evacuation-only order, no Order A

**Scenario:** A production line transitions from Style A to Style B. The from-style claims a consume node (DROP-NODE) with outbound staging configured, but the to-style does NOT claim that node. `DiffStyleClaims` classifies it as SituationDrop — the node must be evacuated but needs no new material.

**Expected behavior:** The changeover creates only Order B (evacuation via complex release steps). No Order A (staging) is created. When Order B completes, the node task advances to `line_cleared` (no new material to release).

**Result:** PASS. Order B is a complex order (release steps). No Order A. Completion advances to `line_cleared`.

**Test:** `shingo-edge/engine/changeover_flow_test.go` — `TestChangeoverFlow_SituationDrop`

---

### TC-92: Multi-node changeover — 4 nodes with swap/unchanged/drop/add

**Scenario:** A production line with 4 nodes transitions between styles. NODE-SWAP gets a different payload (swap), NODE-UNCHANGED keeps the same payload (unchanged), NODE-DROP only exists in from-style (drop), NODE-ADD only exists in to-style (add).

**Expected behavior:** All 4 situations classified correctly. Unchanged gets no orders. Swap gets both Order A and Order B. Drop gets only Order B. Add gets only Order A. Completing both orders on swap node advances it to `released`. Switching the swap node does not auto-complete the changeover (drop/add still pending).

**Result:** PASS. All situations correct. Changeover stays active after swap switch because other nodes are incomplete.

**Test:** `shingo-edge/engine/changeover_flow_test.go` — `TestChangeoverFlow_MultiNode`

---

### TC-93: Changeover-only role — evacuate and restore

**Scenario:** A node has `role="changeover"` in both from-style and to-style. Both claims have full staging config (inbound, outbound, source, destination). `DiffStyleClaims` classifies it as SituationEvacuate (changeover role always forces evacuate). The changeover creates both Order A (staging) and Order B (evacuate with wait steps).

**Expected behavior:** Order A completion → `staged`. Order B completion (evacuate with 2 waits) → `released`. The changeover role forces evacuation even though payload is identical.

**Result:** PASS. Evacuate situation forced by changeover role. Both orders created and state machine advances correctly.

**Test:** `shingo-edge/engine/changeover_flow_test.go` — `TestChangeoverFlow_ChangeoverRole`

---

### TC-94: Double changeover — complete one, start another, clean state

**Scenario:** Complete a full changeover (orders → switch → cutover), then immediately start a second changeover to a third style. Verify the second changeover gets a new record, new node tasks, and doesn't inherit stale state from the first.

**Expected behavior:** First changeover completes cleanly. Second changeover creates a new changeover record (different ID), new node task with correct situation, and `production_state = changeover_active`.

**Result:** PASS. Second changeover is independent. No state leakage from first changeover.

**Test:** `shingo-edge/engine/changeover_flow_test.go` — `TestChangeoverFlow_DoubleChangeover`

---

### TC-95: No claim changes — all unchanged, effective no-op changeover

**Scenario:** Both from-style and to-style have identical claims on the same node (same payload, same role, same everything). The changeover should be an effective no-op.

**Expected behavior:** `DiffStyleClaims` returns SituationUnchanged. No orders created. Cutover still works — sets active style and returns to `active_production`.

**Result:** PASS. Unchanged situation, no orders, cutover completes normally.

**Test:** `shingo-edge/engine/changeover_flow_test.go` — `TestChangeoverFlow_NoClaimChanges`

---

### TC-96: Cancel mid-release — Order B in transit gets aborted

**Scenario:** A Phase 3 changeover is in progress. Order A completes (node staged). Order B is submitted, acknowledged, and goes `in_transit` (robot physically executing). The operator cancels the changeover.

**Expected behavior:** Order B is aborted (terminal status). Active changeover is cleared. Production state returns to `active_production`.

**Result:** PASS. In-transit Order B correctly aborted on cancel. Changeover cleaned up.

**Test:** `shingo-edge/engine/changeover_flow_test.go` — `TestChangeoverFlow_CancelMidRelease`

---

### TC-97: Order A fails — staging failure → error → retry succeeds

**Scenario:** Phase 3 changeover starts. Order A (staging) fails. The node task should go to `error`. The operator retries staging via `StageNodeChangeoverMaterial`. The retry order completes successfully.

**Expected behavior:** Order A failure → node task state = `error`. Retry creates a new staging order. Retry completion → node task state = `staged`.

**Result:** PASS. Error state set on failure, retry succeeds and advances to staged.

**Test:** `shingo-edge/engine/changeover_flow_test.go` — `TestChangeoverFlow_OrderAFails`

---

### TC-98: Order B fails — swap order failure → error → manual recovery

**Scenario:** Phase 3 changeover. Order A completes (staged). Order B (swap with wait steps) fails. The node task goes to `error`. The operator uses manual `EmptyNodeForToolChange` to evacuate old material.

**Expected behavior:** Order B failure → node task `error`. Manual empty creates a complex order. Its completion advances the node to `released` (wiring handler matches complex order on swap situation).

**Result:** PASS. Manual empty after failure advances to `released` because wiring treats complex order completion on swap situation as full transition.

**Test:** `shingo-edge/engine/changeover_flow_test.go` — `TestChangeoverFlow_OrderBFails`

---

### TC-99: Partial completion — 2 of 3 nodes done, 1 errors, cutover blocked

**Scenario:** A 3-node changeover. Nodes A and B complete fully (Order A + Order B + switch). Node C's Order B fails (error). The changeover should remain active because node C is not done.

**Expected behavior:** Nodes A and B switch successfully. Node C enters `error` state. Changeover stays active. `tryCompleteProcessChangeover` is blocked because not all nodes are released/switched.

**Result:** PASS. Changeover stays active with partial completion. Node C error prevents auto-completion.

**Test:** `shingo-edge/engine/changeover_flow_test.go` — `TestChangeoverFlow_PartialCompletion`

---

### TC-100: Cutover completion — switch + cutover sets active style

**Scenario:** Complete a full Phase 3 changeover: Order A → staged, Order B → released, switch node, then cutover. Verify all final state: active style set, target style cleared, production state restored, runtime claim/UOP updated.

**Expected behavior:** After cutover: `ActiveStyleID = toStyleID`, `TargetStyleID = nil`, `ProductionState = active_production`, runtime `ActiveClaimID = toClaim.ID`, `RemainingUOP = toClaim.UOPCapacity`.

**Result:** PASS. All state transitions correct.

**Test:** `shingo-edge/engine/changeover_flow_test.go` — `TestChangeoverFlow_CutoverCompletion`

---

### TC-101: Keep-staged + evacuate — both flags on same claim

**Scenario:** A from-claim has both `KeepStaged = true` and `EvacuateOnChangeover = true` with matching payloads across styles. `DiffStyleClaims` classifies it as SituationEvacuate. The keep-staged handler creates both Order A (combined keep-staged delivery) and Order B (evacuation).

**Expected behavior:** Same payload + EvacuateOnChangeover → SituationEvacuate. KeepStaged flag triggers keep-staged order handler. Order A completion → `staged`. Order B completion (with Order A done) → `released`.

**Result:** PASS. Both flags coexist correctly. State machine advances through staged → released.

**Test:** `shingo-edge/engine/changeover_flow_test.go` — `TestChangeoverFlow_KeepStagedWithEvacuate`

---

### TC-102: Keep-staged from → non-keep-staged to — from-claim drives handler

**Scenario:** The from-style claim has `KeepStaged = true` with full staging config. The to-style claim does NOT have KeepStaged. Different payloads (PART-OLD vs PART-NEW) → SituationSwap. The from-claim's KeepStaged flag drives the order handler.

**Expected behavior:** SituationSwap with keep-staged handler (from from-claim). Both Order A and Order B created despite to-style not having KeepStaged.

**Result:** PASS. From-claim's KeepStaged drives the handler. Both orders created.

**Test:** `shingo-edge/engine/changeover_flow_test.go` — `TestChangeoverFlow_KeepStagedToNoKeep`

---

### TC-103: Keep-staged missing staging config — falls back to simple staging

**Scenario:** A claim has `KeepStaged = true` but `InboundStaging` is empty (no staging node configured). The system cannot pre-stage material without a staging node.

**Expected behavior:** Falls back to simple staging/retrieve order (Order A only). No Order B — Phase 3 requires InboundStaging to create the full swap order pair.

**Result:** PASS. Order A created (fallback), no Order B. System degrades gracefully when staging config is missing.

**Test:** `shingo-edge/engine/changeover_flow_test.go` — `TestChangeoverFlow_KeepStagedMissingStaging`

---

### TC-104: A/B flip during changeover — flip succeeds, orders blocked

**Scenario:** An A/B consume pair is in production. A changeover starts on the process. The operator flips A/B (FlipABNode) during the active changeover.

**Expected behavior:** FlipABNode succeeds (it doesn't check changeover state). ActivePull switches correctly. But `CanAcceptOrders` returns false with reason "changeover in progress", blocking auto-reorder on the newly active node.

**Result:** PASS. Flip works during changeover. New orders blocked by CanAcceptOrders guard.

**Test:** `shingo-edge/engine/wiring_test.go` — `TestWiring_ABFlip_DuringChangeover`

---

### TC-105: A/B produce pair — active increments, inactive skipped

**Scenario:** Two produce nodes in an A/B pair. Node A has `ActivePull = true`, Node B has `ActivePull = false`. A counter delta fires.

**Expected behavior:** Only Node A (active) should increment UOP. Node B (inactive) should be skipped — its production is held in buffer.

**Result:** BUG FOUND AND FIXED. Both produce nodes incremented regardless of ActivePull. The `handleCounterDelta` produce case (`wiring.go:111`) did not check `PairedCoreNode` or `ActivePull`, unlike the consume case which had the A/B guard at line 82.

**Root cause:** The produce branch in `handleCounterDelta` was missing the A/B cycling guard that the consume branch already had. Initial implementation only added A/B logic for consume nodes; produce nodes were overlooked.

```go
// Before (broken): all produce nodes increment
case "produce":
    newRemaining := runtime.RemainingUOP + int(delta.Delta)
    _ = e.db.UpdateProcessNodeUOP(node.ID, newRemaining)

// After (fixed): check A/B pairing like consume does
case "produce":
    if claim.PairedCoreNode != "" && !runtime.ActivePull {
        continue
    }
    newRemaining := runtime.RemainingUOP + int(delta.Delta)
    _ = e.db.UpdateProcessNodeUOP(node.ID, newRemaining)
```

**Production risk:** Any produce node configured in an A/B pair. Both nodes count production simultaneously, doubling the reported output. Auto-relief triggers on the wrong node (inactive one might hit capacity prematurely). UOP tracking becomes unreliable — operators see wrong counts on the production page.

**Status:** Fixed. Produce branch now checks `PairedCoreNode` and `ActivePull` before incrementing.

**Test:** `shingo-edge/engine/wiring_test.go` — `TestWiring_ABProducePair_ActiveIncrements`

---

### TC-106: A/B flip + immediate counter delta — race-free sequencing

**Scenario:** An A/B consume pair with Node A active. Flip to Node B, then immediately fire a counter delta. Verify the delta hits Node B (not Node A) — no race between flip and delta processing.

**Expected behavior:** After flip, Node B is active. Counter delta decrements Node B. Node A unchanged.

**Result:** PASS. Flip is synchronous (DB update), delta reads current state. No race condition.

**Test:** `shingo-edge/engine/wiring_test.go` — `TestWiring_ABFlip_ImmediateDelta`

---

### TC-107: A/B pairs across styles — unpaired after changeover, both decrement

**Scenario:** An A/B consume pair under Style 1 (paired via PairedCoreNode). Changeover to Style 2 which has NO PairedCoreNode on either node. After cutover, both nodes are unpaired. A counter delta fires.

**Expected behavior:** Both nodes decrement independently (unpaired behavior). Total delta = 2 × individual delta. Pairing is the mechanism that prevents double-counting; without it, both nodes count.

**Result:** PASS. Total delta = 6 (each of 2 nodes decrements by 3 independently). Documents that pairing must be configured in the target style to maintain A/B behavior across changeovers.

**Test:** `shingo-edge/engine/wiring_test.go` — `TestWiring_ABPairsAcrossStyles`

---

### TC-108: A/B asymmetric pair — fallthrough hits inactive node

**Scenario:** Node A names Node B as partner (`PairedCoreNode: "ASYM-B"`), but Node B does NOT name Node A (`PairedCoreNode: ""`). Node A is paired, Node B is unpaired.

**Expected behavior:** With Node A active and Node B inactive: Node A decrements (active paired), Node B decrements (unpaired always decrements). After flip (A inactive, B active): Node B decrements (unpaired), Node A ALSO decrements via fallthrough — because Node B never sets `pairedConsumeHandled` (it's unpaired), so the fallthrough safety net fires on inactive Node A.

**Result:** PASS. Documents the asymmetric A/B edge case: when only one side of the pair is configured, the fallthrough logic double-counts. This is expected behavior — A/B pairing requires symmetric configuration.

**Test:** `shingo-edge/engine/wiring_test.go` — `TestWiring_AB_AsymmetricPair`