# Core Dispatch

## Overview

Core dispatch translates order requests into fleet transport orders. It handles state mapping from fleet to Shingo statuses (CREATED→dispatched, RUNNING→in_transit, WAITING→staged, FINISHED→delivered), staged order release timing, bad input rejection, and the full dispatch-to-receipt lifecycle. This is the foundation — every other system builds on top of dispatch working correctly.

## Test files

- `dispatch/fleet_simulator_test.go` — outbound fleet instruction verification (TC-1, 3, 4, 5)
- `engine/engine_test.go` — engine-level lifecycle and staging tests (TC-2, 15)
- `engine/engine_test.go` — post-delivery cancel guard (TC-68)
- `engine/engine_concurrent_test.go` — malformed input handling (TC-9, 10, 12)
- `messaging/core_data_service_test.go` — node list sync (TC-69)

Run this domain's tests:

```bash
cd shingo-core
go test -v -run "TestSimulator|TestTC09|TestTC10|TestTC12|TestTC38|TestTC39|TestNodeListResponse|TestMaybeCreateReturnOrder" ./engine/ ./dispatch/ ./messaging/ -timeout 120s
```

## Index

| TC | Description | Status |
|----|-------------|--------|
| TC-1 | Robot drives to bin but doesn't jack it | PASS |
| TC-2 | Staged order release timing | PASS |
| TC-3 | Basic retrieve sends correct fleet instructions | PASS |
| TC-4 | Fleet state mapping (WAITING → staged, etc.) | PASS |
| TC-5 | Fleet down — no phantom return orders | PASS |
| TC-9 | Complex order with zero steps | PASS |
| TC-10 | Order references nonexistent delivery node | PASS |
| TC-12 | Order requests zero quantity | PASS |
| TC-15 | Full lifecycle dispatch → receipt → bin arrival | PASS |
| TC-14 | maybeCreateReturnOrder sends bin to wrong node | FIXED |
| TC-14b | HandleChildOrderFailure leaves in-flight siblings orphaned | FIXED |
| TC-68 | Post-delivery cancel: no return order, spurious return order | FIXED |
| TC-69 | Node list sync excludes NGRP node groups | FIXED |

## Bugs found and fixed

### TC-14: maybeCreateReturnOrder sends bin to wrong node

**Scenario:** A retrieve order is dispatched (storage → line). The robot starts moving (RUNNING), then fails (robot breakdown, obstacle). The system marks the order failed, unclaims the bin, and calls `maybeCreateReturnOrder` to send the bin back. The return order should route the bin back to its origin (the storage node where it still physically sits, since `ApplyBinArrival` never fired on the failed order).

**Expected behavior:** The return order's `SourceNode` should match the original order's `SourceNode` — the storage node where the bin started and never left. The return order delivers the bin back to the root parent of that source node so the group resolver can pick the best slot.

**Result:** BUG FOUND. `maybeCreateReturnOrder` set `SourceNode: order.DeliveryNode` instead of `SourceNode: order.SourceNode`. The return order told the robot to pick up from the line node (the destination the robot never reached) instead of the storage node (where the bin actually is). The robot would travel to an empty line node and fail.

**Root cause:** Line 384 of `engine/wiring.go` had a copy-paste error in the return order construction. The `DeliveryNode` field was correctly set to the root parent of the source node, but `SourceNode` was set to `order.DeliveryNode` instead of `order.SourceNode`. This only manifests when `SourceNode` and `DeliveryNode` differ (the common case) — round-trip orders where they happen to match masked the bug.

```go
// Before (broken): return order picks up from the destination the robot never reached
returnOrder := &store.Order{
    SourceNode:  order.DeliveryNode,  // WRONG — bin never left origin
    DeliveryNode: rootNode.Name,
    ...
}

// After (fixed): return order picks up from where the bin actually is
returnOrder := &store.Order{
    SourceNode:  order.SourceNode,    // CORRECT — bin is still at origin
    DeliveryNode: rootNode.Name,
    ...
}
```

**Production risk:** Any failed or cancelled in-flight order (robot breakdown, obstacle, emergency stop, operator cancel) creates a return order that sends a robot to the wrong location. The return order fails, and the bin stays at its real location but with no pending order to retrieve it. The bin effectively disappears from the dispatch system until manual intervention. This happens every time a robot fails mid-delivery — a regular occurrence on busy factory floors.

**Status:** Fixed. Return order now correctly uses `order.SourceNode`.

**Tests:** `engine/engine_test.go` — `TestMaybeCreateReturnOrder_SourceNode`; `engine/engine_complex_test.go` — `TestComplexOrder_CancelMidTransit`, `TestComplexOrder_FleetFailureMidTransit` (both strengthened with SourceNode assertions)

---

### TC-14b: HandleChildOrderFailure leaves in-flight siblings orphaned

**Scenario:** A compound reshuffle order has 3+ children executing sequentially. Child 2 fails (robot breakdown). `HandleChildOrderFailure` should cancel all remaining children (including child 3 which may already be in-flight with a fleet order and a robot moving) and fail the parent.

**Expected behavior:** All non-terminal children (pending, sourcing, dispatched, in_transit, staged) should have their fleet orders cancelled, their bins unclaimed, and their statuses set to cancelled. The parent should be marked failed. Lane lock released. No orphan robots, no stuck bins.

**Result:** BUG FOUND. `HandleChildOrderFailure` only cancelled children with status `StatusPending || StatusSourcing`. In-flight children (dispatched, in_transit, staged) were skipped. Their fleet orders remained active — orphan robots continued moving with no order tracking them. Their bin claims stayed in place — bins permanently stuck as `claimed_by` pointing to sibling orders that would never complete.

**Root cause:** The status filter in `HandleChildOrderFailure` was too narrow. It assumed only pending/sourcing children existed, but `AdvanceCompoundOrder` dispatches children one at a time — by the time a sibling fails, the next child may already be dispatched and in transit. The fix mirrors `cancelCompoundChildren` (the parent-initiated cancel path), which correctly cancels ALL non-terminal children by skipping only the three terminal states (cancelled, confirmed, failed) and using `lifecycle.CancelOrder` for everything else.

```go
// Before (broken): only cancels pending/sourcing, leaves in-flight siblings active
for _, child := range children {
    if child.Status != StatusPending && child.Status != StatusSourcing {
        continue  // SKIPS in-flight children!
    }
    ...
}

// After (fixed): cancels ALL non-terminal, using lifecycle.CancelOrder
cancelReason := fmt.Sprintf("sibling order %d failed during reshuffle", childOrderID)
for _, child := range children {
    if child.ID == childOrderID { continue }
    if child.Status == StatusCancelled || child.Status == StatusConfirmed || child.Status == StatusFailed {
        continue
    }
    d.lifecycle.CancelOrder(child, parent.StationID, cancelReason)
}
```

**Production risk:** Any compound reshuffle where a child fails after the next child has been dispatched. The orphan robot continues to a slot, potentially jacking a bin that no order tracks. The stuck bin claim prevents future orders from using that bin. Lane lock may not release if the orphan child never reaches a terminal state. Recovery requires manual database intervention. On a busy floor with multi-step reshuffles, this would cascade quickly.

**Status:** Fixed. `HandleChildOrderFailure` now uses `lifecycle.CancelOrder` for all non-terminal children.

**Tests:** `dispatch/reshuffle_test.go` — `TestHandleChildOrderFailure_InFlightSibling`; `engine/engine_compound_test.go` — `TestCompound_ChildFailureMidReshuffle_BlockerStranding`, `TestCompound_CancelParentWhileChildInFlight`

---

### TC-68: Post-delivery cancel: no return order, spurious return order

**Scenario:** A retrieve order for storage -> line is already dispatched and has "delivered" status in the UI. The admin user sees this and cancels the delivered order. This is a real operator workflow: an order shows as delivered but the operator didn't confirm receipt (maybe the bin never arrived), so they cancel it from the admin panel.

Three interrelated bugs fire in sequence:

**Bug C (root enabler):** `TerminateOrder` has NO status guard. Accepts orders in any status including delivered/confirmed/completed. Recovery path (`CancelStuckOrder`) correctly rejects terminal statuses, but nothing else does. UI hides Terminate for completed/cancelled/failed but NOT for delivered or confirmed.

**Bug A (cascade):** `maybeCreateReturnOrder` fires on `EventOrderCancelled`. By the time it reads the order from DB, `CancelOrder` already overwrote status to "cancelled" which passes the guard. Creates return order with `SourceNode=warehouse` (wrong -- bin is at lineside). Claims bin via `ClaimBin`. The `maybeCreateReturnOrder` would send a robot back to pick up a never-loaded bin.

**Bug B (cascade):** `ConfirmReceipt` only guard is `CompletedAt != nil`. Does NOT check status. Overwrites "cancelled" back to "confirmed", runs `CompleteOrder`, triggers `handleOrderCompleted` -> `ApplyBinArrival`.

**Expected behavior:** `ConfirmReceipt` on delivered orders should return a receipt rejection error. `CancelOrder` with status=cancelled should return a cancellation rejection. `TerminateOrder` should reject already-delivered statuses. `maybeCreateReturnOrder` should NOT create a return order when the order was delivered.

**Result:** BUG FOUND. All three bugs confirmed in a single test scenario. Terminating a delivered order succeeds (should reject), cancelling it creates a spurious return order (should not), and confirming receipt on the cancelled order overwrites status back to confirmed (should reject).

**Root cause:** Missing status guards at three independent points in the lifecycle. `TerminateOrder` was the root enabler -- without it, the cascade never starts.

```go
// Fix 1: TerminateOrder rejects terminal statuses
func (e *Engine) TerminateOrder(...) error {
    if order.Status == "delivered" || order.Status == "confirmed" ||
       order.Status == "cancelled" || order.Status == "completed" {
        return fmt.Errorf("cannot terminate order in %s status", order.Status)
    }
    ...
}

// Fix 2: CancelOrder passes previousStatus to event, maybeCreateReturnOrder checks it
func (e *Engine) CancelOrder(...) error {
    previousStatus := order.Status  // capture before overwrite
    ...
    e.bus.Emit(EventOrderCancelled, &OrderCancelledEvent{Order: order, PreviousStatus: previousStatus})
}

// Fix 3: ConfirmReceipt rejects non-deliverable statuses
func (d *Dispatcher) HandleOrderReceipt(...) error {
    if order.Status != "delivered" {
        return fmt.Errorf("cannot confirm receipt for order in %s status", order.Status)
    }
    ...
}
```

**Production risk:** Any time an admin cancels a delivered order (e.g., bin didn't actually arrive, wrong destination), the system creates a phantom return order sending a robot to the warehouse for a bin that's at lineside. If the admin also confirms receipt, the order completes with `ApplyBinArrival` overwriting the cancellation. Both paths corrupt inventory tracking. On a busy floor, this happens during shift transitions when operators reconcile discrepancies.

**Changeover interaction:** `CancelOrder` -> `CancelProcessChangeover` aborts in-flight orders on affected nodes. `AbortNodeOrders` aborts orders on affected nodes. Runtime state references are freed. Failures are logged, not blocking -- operator can retry manually.

**Status:** Fixed. `TerminateOrder` rejects already-delivered statuses. `CancelOrder` passes `order.Status` as `previousStatus` for `EmitOrderCancelled` to avoid spurious return orders. `ConfirmReceipt` rejects receipt on delivered/cancelled/confirmed statuses.

**Test:** `shingo-core/engine/engine_test.go` -- `TestTC38_CancelDeliveredOrder_NoReturnOrder`, `TestTC39_TerminateOrder_RejectsTerminalStatuses`

---

### TC-69: Node list sync excludes NGRP node groups

**Scenario:** Edge sends a node list request to Core ("Sync Nodes"). Core queries `ListNodesForStation` to get station-scoped nodes. The SQL includes NGRP (node group) parent nodes whose children are station-assigned, and `handleNodeListRequest` builds the response with dot notation for children (e.g., `STORAGE-G1.SLOT-1`). But the station-scoped path was excluding NGRP parent nodes because the original `ListNodesForStation` SQL filtered them out.

**Expected behavior:** When Edge syncs nodes, the response should include:
- Individual nodes assigned to the station
- NGRP parent nodes (with type `NGRP`) whose children are assigned to the station
- Children under NGRP parents with dot notation (`PARENT.CHILD`)

Edge has UI support for NGRP nodes (shows `(group)` suffix, expands children in dropdowns), but Core never sent them. NGRP nodes were missing from edge's node dropdown after sync.

**Result:** BUG FOUND. `ListNodesForStation` SQL returned individual nodes but excluded NGRP parent containers. The `handleNodeListRequest` station-scoped path built `NodeInfo` entries from the query results, but without NGRP parents in the result set, no group information was sent to edge. The global fallback path already returned NGRP parents with `parent_id IS NULL`, but the station-scoped path (the common case) missed them.

**Root cause:** The `ListNodesForStation` SQL had two conditions:
1. Direct station assignments: `n.id IN (SELECT node_id FROM node_stations WHERE station_id = $1)` -- this returns children but not their NGRP parents.
2. NGRP parent inclusion: `EXISTS (SELECT 1 FROM node_types nt WHERE nt.code = 'NGRP' AND ...)` -- this was missing, so NGRP parents were never included in station-scoped queries.

```sql
-- Before (broken): NGRP parents missing from station-scoped results
-- Only returned direct assignments, no group containers

-- After (fixed): includes NGRP parents whose children are station-assigned
OR (EXISTS (
    SELECT 1 FROM node_types nt WHERE nt.id = n.node_type_id AND nt.code = 'NGRP'
    AND EXISTS (
        SELECT 1 FROM nodes c JOIN node_stations cs ON cs.node_id = c.id
        WHERE c.parent_id = n.id AND cs.station_id = $1
    )
))
```

**Production risk:** When an operator hits "Sync Nodes" on edge, NGRP nodes (storage groups, supermarkets) never appear in the dropdown, even though edge's UI has `(group)` suffix support. Operators cannot select node groups for manual orders or configuration. This affects every edge station with NGRP-assigned storage areas -- which is the standard layout for warehouse supermarkets.

**Status:** Fixed. `ListNodesForStation` SQL now includes NGRP parents with station-assigned children. `handleNodeListRequest` correctly builds dot-notation names for both paths (station-scoped and global fallback).

**Test:** `shingo-core/messaging/core_data_service_test.go` -- `TestNodeListResponse_IncludesNodeGroups`, `TestNodeListResponse_GlobalPath_IncludesNodeGroups`

## Verified scenarios

### TC-1: Robot drives to a location but doesn't pick up the bin — PASS

**Scenario:** You send a complex order — pick up a bin from storage, drop it off at the line. The robot drives to the storage location. But what if the instructions don't tell the robot to actually jack the bin? The robot arrives, sits there, and drives away empty. The bin never moves.

**Expected behavior:** Every instruction block sent to the fleet must include a bin task — either JackLoad (pick up the bin) or JackUnload (set it down). The pickup block should be at the storage node, the dropoff block at the line node.

**Result:** PASS. Every block includes the correct bin task and location. This was a real bug found on the floor (2026-03-26) and has since been fixed. This test ensures it never comes back.

```go
// Every block the fleet receives must tell the robot what to do with the bin
for _, b := range blocks {
    if b.BinTask == "" {
        t.Errorf("block %q at %q has empty BinTask — robots would navigate without jacking",
            b.BlockID, b.Location)
    }
}
```

**Test:** `dispatch/fleet_simulator_test.go` — `TestSimulator_ComplexOrderBinTasks`

---

### TC-2: Robot is waiting at staging but the release doesn't go through — PASS

**Scenario:** A cycle order: robot picks up from storage, drops off at the line staging area, and waits for the operator. When the operator is done, they release the order and the robot continues — picks up from staging and returns to storage. But what if the release command gets rejected because the system doesn't realize the robot is actually waiting?

**Expected behavior:** When the robot reaches the waiting point, the system should update the order status to "staged." The release command should only be accepted when the order is in "staged" status. After release, the remaining instructions (pick up from staging, return to storage) should be appended to the robot's task.

**Result:** PASS after rewriting the test. The original test had a bug — it was sending the release while the order was still "dispatched" instead of "staged," so the release was silently rejected and the test passed without actually testing anything. The test now drives the robot through the full sequence: CREATED → RUNNING → WAITING (status becomes "staged") → release → RUNNING → FINISHED.

```go
// Robot reaches wait point — system sets status to "staged"
sim.DriveState(order.VendorOrderID, "WAITING")
// Release only works when status is "staged"
d.HandleOrderRelease(env, &protocol.OrderRelease{OrderUUID: "staged-tc2"})
// 2 pre-wait + 2 post-wait = 4 blocks total
assert(len(view.Blocks) == 4 && view.Complete == true)
```

**Test:** `engine/engine_test.go` — `TestSimulator_StagedComplexOrderRelease`

---

### TC-3: Basic retrieve order sends the right instructions to the fleet — PASS

**Scenario:** The simplest possible operation — request a bin from storage and deliver it to the production line. What if the system sends the wrong locations, or doesn't tell the robot to pick up and set down?

**Expected behavior:** The fleet should receive exactly 2 instruction blocks: JackLoad (pick up) at the storage node, JackUnload (set down) at the line node.

**Result:** PASS. The dispatch pipeline creates the correct transport order with the right block structure and locations.

**Test:** `dispatch/fleet_simulator_test.go` — `TestSimulator_SimpleRetrieveOrder`

---

### TC-4: System misinterprets what the robot is doing — PASS

**Scenario:** The fleet reports that a robot is in state "WAITING." The system needs to translate that into a Shingo status. What if the translation is wrong? If WAITING maps to the wrong status, the system won't know the robot is at the staging point, and operator releases will be rejected.

**Expected behavior:** Every fleet state must map to the correct Shingo status, matching exactly what the real RDS adapter does: CREATED → dispatched, RUNNING → in_transit, WAITING → staged, FINISHED → delivered, FAILED → failed, STOPPED → cancelled.

**Result:** PASS after fixing two mapping errors. The simulator was mapping WAITING to "waiting" instead of "staged," and was missing the TOBEDISPATCHED state entirely. Both were corrected. This directly affected TC-2 — with the wrong mapping, staged order releases would always be rejected.

**Test:** `dispatch/fleet_simulator_test.go` — `TestSimulator_StateMapping`

---

### TC-5: Fleet is down — system creates phantom return orders — PASS

**Scenario:** The fleet server is unreachable and rejects order creation. The system marks the order as failed. But what if the failure handler then tries to create a "return" order to send the bin back? The bin never left — the fleet never accepted the order — so a return order makes no sense and creates confusion about where bins actually are.

**Expected behavior:** When the fleet rejects order creation, the order should be marked as failed with no vendor order ID. No return order should be created.

**Result:** PASS. The order is correctly marked as failed, has no vendor order ID, and no phantom orders exist in the simulator.

**Test:** `dispatch/fleet_simulator_test.go` — `TestSimulator_FleetFailure_NoVendorOrderID`

---

### TC-15: Full order lifecycle from dispatch to bin arrival — PASS

**Scenario:** A retrieve order goes through its entire lifecycle: dispatch, robot picks up, robot drives to the line, robot arrives, the Edge station confirms the bin arrived, and the system updates inventory. What if any step in this chain breaks? The bin might not move in the database, the claim might not release, or the order might get stuck in "delivered" forever.

**Expected behavior:** The complete chain should work end-to-end: dispatched → in_transit → delivered → Edge confirms receipt → bin moves to destination node → claim released → order confirmed.

**Result:** PASS. The entire feedback loop works. This is the most important test because it exercises every layer of the system.

One important detail this test verifies: the robot arriving (FINISHED) does not automatically move the bin. The Edge station must confirm receipt first. This matches real production behavior — the operator confirms the bin is actually there before the system updates inventory.

```go
// 1. Dispatch order
d.HandleOrderRequest(env, &protocol.OrderRequest{...})
assert(order.Status == "dispatched")

// 2-3. Robot moves and arrives — bin has NOT moved yet
sim.DriveState(order.VendorOrderID, "RUNNING")   // → "in_transit"
sim.DriveState(order.VendorOrderID, "FINISHED")   // → "delivered"

// 4. Edge confirms receipt — NOW the bin moves
d.HandleOrderReceipt(env, &protocol.OrderReceipt{...})
assert(order.Status == "confirmed")

// 5. Verify: bin at destination, claim released
assert(*bin.NodeID == lineNode.ID)    // bin moved
assert(bin.ClaimedBy == nil)           // claim released
```

**Test:** `engine/engine_test.go` — `TestSimulator_FullLifecycle`

---

### TC-9: Complex order with zero steps — PASS

**Scenario:** Someone sends a complex order request with an empty steps array. The system should reject it gracefully — no panic, no fleet orders, no broken order record.

**Expected behavior:** The order is either rejected before being persisted (no database record) or created with a non-dispatched status. No robot should be dispatched.

**Result:** PASS. The handler rejected the order before persisting — `GetOrderByUUID` returned "not found." No fleet orders were created. The system handled the malformed input cleanly without crashing.

**Test:** `engine/engine_concurrent_test.go` — `TestTC09_ComplexOrderZeroSteps`

---

### TC-10: Order references nonexistent delivery node — PASS

**Scenario:** A retrieve order specifies a delivery node name that doesn't exist in the database (`"NOSUCH-NODE-XYZ"`). Should fail with a clear error, not create a partial order or dispatch a robot.

**Expected behavior:** The order is either rejected before persisting or created with a failed/queued status. No fleet transport order should be created.

**Result:** PASS. The lifecycle rejected the order before persisting — `GetOrderByUUID` returned "not found." No fleet orders were created.

**Test:** `engine/engine_concurrent_test.go` — `TestTC10_NonexistentDeliveryNode`

---

### TC-12: Order requests zero quantity — PASS

**Scenario:** Someone sends a retrieve order with `quantity=0`. Should be handled gracefully — no panic, no crash.

**Expected behavior:** The system processes the order without crashing. Whether it dispatches or rejects is secondary — the key is no panic.

**Result:** PASS. The system handled the zero-quantity order without panic. The order was created and processed through the normal pipeline.

**Test:** `engine/engine_concurrent_test.go` — `TestTC12_ZeroQuantity`
