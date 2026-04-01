# Complex & Compound Orders

## Overview

Complex orders handle multi-step robot instructions (pickup, dropoff, wait, swap) through StepsJSON → claimComplexBins → fleet dispatch. Compound reshuffles are multi-child orders for buried bin retrieval in supermarket NGRP lanes — the system detects blocked bins, plans a sequence of unbury/retrieve/restock children, and executes them sequentially with lane locking. The order_bins junction table tracks multi-pickup orders where a single complex order moves multiple bins.

## Test files

- `engine/engine_complex_test.go` — complex order lifecycle and production cycle patterns (TC-42–60)
- `engine/engine_compound_test.go` — compound reshuffle tests (TC-40a–54)
- `dispatch/group_resolver_test.go` — FIFO/COST resolution, buried bin detection
- `dispatch/integration_test.go` — reshuffle integration
- `dispatch/reshuffle_test.go` — reshuffle planning

Run this domain's tests:

```bash
cd shingo-core
go test -v -run "TestComplexOrder|TestCompound|TestBuriedBin|TestLaneLock|TestTC40|TestTC44|TestTC45|TestTC46|TestTC60|TestTC24" ./engine/ ./dispatch/ -timeout 120s
```

## Index

| TC | Description | Status |
|----|-------------|--------|
| TC-23c | Changeover with missing bin — ghost robot | FIXED |
| TC-24a | Complex order bin poaching during transit | FIXED |
| TC-24b | Stale bin location after complex order | FIXED |
| TC-24c | Phantom inventory from stale location | FIXED |
| TC-40a | FIFO mode — buried older than accessible triggers reshuffle | PASS |
| TC-40b | COST mode — oldest accessible returned, buried ignored | PASS |
| TC-41 | Empty cart starvation — no accessible empties | PASS |
| TC-42 | Complex order cancel mid-transit — auto-return with SourceNode | FIXED |
| TC-43 | Complex order fleet failure mid-transit — auto-return SourceNode | FIXED |
| TC-44 | Compound child failure mid-reshuffle — blocker stranding | PASS |
| TC-45 | Two-robot swap full lifecycle (5-step compound) | PASS |
| TC-46 | Cancel parent compound while child in-flight | FIXED |
| TC-47 | Empty post-wait release — full lifecycle verification | PASS |
| TC-48 | Complex order redirect — StepsJSON stale after redirect | PASS |
| TC-49 | Ghost robot — claimComplexBins finds no bin at pickup | FIXED |
| TC-50 | Concurrent complex orders same node — double claim race | FIXED |
| TC-51 | AdvanceCompoundOrder skips failed children — premature completion | PASS |
| TC-52 | Lane lock contention — second reshuffle queued correctly | PASS |
| TC-53 | ApplyBinArrival status for compound restock children | PASS |
| TC-54 | Staging TTL expiry during compound order execution | PASS |
| TC-55 | Sequential Backfill — simplest complex order lifecycle | PASS |
| TC-56 | Sequential Removal — wait/release with post-wait bin claim | PASS |
| TC-57 | Two-Robot Swap Resupply — 2 pre-wait blocks, staging wait | PASS |
| TC-58 | Two-Robot Swap Removal — dropoff-first pre-wait, full lifecycle | PASS |
| TC-59 | Staging + Deliver separation — two independent orders | PASS |
| TC-60 | Single-robot 10-step swap — multi-bin junction table fix | FIXED |

## Bugs found and fixed

### TC-23c: Changeover with one bin already gone — ghost robot dispatched

**Scenario:** A production line has 3 bins in operation. The operator moves one bin to quality hold (via a completed move order — the bin is physically gone from the line). Then the operator initiates changeover, which submits store orders to clear all bins from the line. But only 2 bins remain at the line node. What happens to the 3rd store order?

**Expected behavior:** The system should find only 2 unclaimed bins at the line. Two store orders should claim and dispatch normally. The 3rd store order should fail cleanly — "no available bin" — and not send a robot.

**Result:** BUG FOUND. The 3rd store order dispatched a real robot to the line with no bin assigned (`BinID=nil`). The fleet received a transport order (`sg-3-40cb32bb`) and would send a robot to LINE1-IN to pick up nothing. The order showed as "dispatched" with a valid vendor order ID but no bin.

**Root cause:** The store order planning function (`planStore`) searches for unclaimed bins at the source node. If the loop finds nothing, the code continued to dispatch without checking whether a bin was actually claimed. There was no guard after the bin-finding loop.

```go
// Before (broken): falls through to dispatch even if no bin was found
for _, bin := range bins {
    if bin.ClaimedBy == nil {
        // try to claim...
    }
}
// no check here — dispatches with BinID=nil

// After (fixed): fails the order if no bin is available
if order.BinID == nil {
    return nil, &planningError{Code: "no_bin",
        Detail: fmt.Sprintf("no available bin at %s", sourceNode.Name)}
}
```

**Production risk:** This happens during any changeover where the operator clears a line that isn't fully stocked, or when a bin was already moved away for quality or other reasons before changeover starts. The ghost robot arrives at the line, finds nothing, and occupies a fleet slot while the operator wonders why a robot showed up. On a busy floor with limited robots, that's a wasted trip during the time-critical changeover window.

**Status:** Fixed. The 3rd store order now fails with `"no available bin at LINE1-IN"` and no robot is dispatched.

**Test:** `engine/engine_test.go` — `TestTC23c_ChangeoverWithMissingBin`

---

### TC-24 cluster: Complex order bin claiming — three related bugs (FIXED)

Complex orders (`HandleComplexOrderRequest`) previously never called `ClaimBin` when dispatching. The bin's `claimed_by` field stayed NULL and its location never updated, even while a robot was physically carrying it. This produced three distinct bugs, all now fixed.

**TC-24a: Bin poaching during transit.** A complex order picks up a bin from storage. Robot is in transit (RUNNING). A store order targets the same storage node. Previously, `planStore` would find the bin with `claimed_by=NULL` and claim it for a second robot. Two orders would reference the same physical bin.

**TC-24b: Stale bin location after completion.** A complex order moves a bin from storage to line. Order completes (FINISHED). Previously, the bin was physically at the line but the database still showed it at storage because `ApplyBinArrival` was skipped when `order.BinID` was nil.

**TC-24c: Phantom inventory retrieval.** After TC-24b's stale location, a retrieve order asks for that payload. Previously, `FindSourceBinFIFO` would find the phantom bin (still listed at storage, unclaimed, status=available) and dispatch a robot to an empty storage slot.

**Root cause:** `HandleComplexOrderRequest` was designed to send multi-step robot instructions without pre-binding to a specific bin. The database layer was designed around pre-allocation — store/retrieve orders claim bins during planning. The two models didn't coexist safely. Confirmed via `git log -S "ClaimBin" -- dispatch/complex.go` that `ClaimBin` never existed in complex.go across the entire repository history prior to this fix.

**Fix:** Added `claimComplexBins()` in complex.go. After creating the order record but before dispatching to the fleet, the function iterates over pickup steps, finds the best unclaimed bin at each pickup node (filtering by payload code and status), and claims it. For single-pickup orders (the most common pattern), it sets `Order.BinID` which enables the standard completion flow: `ApplyBinArrival` moves the bin in the DB, and `maybeCreateReturnOrder` creates an auto-return on cancel/fail. Multi-pickup orders claim all pickup bins but only track the first via `Order.BinID` (a multi-bin junction table would be needed for full multi-pickup completion tracking). The claim is best-effort — if no bin is found at a pickup node, the order still dispatches.

**Follow-up fix — staged filter regression:** The initial `claimComplexBins` implementation copied the status filter from `FindSourceBinFIFO`, which excludes `staged` bins. That filter is correct for retrieve orders (searching storage slots where bins are `available`), but wrong for complex orders. Complex orders pick up from core nodes and staging lanes where bins are always `staged` (set by `ApplyBinArrival` for non-storage slots). Every sequential removal, swap, release, and restore order was dispatching with `BinID=nil` because `claimComplexBins` skipped the only bin at the node. Removed `staged` from the skip list. Empty bins at produce nodes are not affected — the produce finalize path (`CreateIngestStoreOrder`) bypasses `claimComplexBins` entirely by pre-setting `BinID`.

**Known limitation:** `Order.BinID` is a single `*int64`. Multi-pickup complex orders (uncommon) have all bins claimed and protected from poaching, but only the first bin gets the `ApplyBinArrival` treatment on completion. A future schema change (order_bins junction table) would fully address this.

**Tests:** `engine/engine_test.go` — `TestTC24_ComplexOrderBinPoaching`, `TestTC24b_StaleBinLocationAfterComplexOrder`, `TestTC24c_PhantomInventoryRetrieve`

---

### TC-46: Cancel parent compound order leaves orphan robots, stuck bins, and locked lane

**Scenario:** A reshuffle compound order is in progress — 3 children (unbury, retrieve, restock). Child 1 is dispatched and its robot is in transit (RUNNING). Child 2 is dispatched but waiting for fleet pickup. Child 3 is pending. The operator cancels the parent order directly.

**Expected behavior:** All children should be cancelled (including their fleet orders). Lane lock released. All bin claims freed.

**Result:** BUG FOUND. `HandleOrderCancel` called `lifecycle.CancelOrder` on the parent only. It never cascaded to children. Three problems:
1. In-flight children kept their fleet orders active — orphan robots continued moving with no order tracking them
2. Bin claims on children were never released — bins permanently stuck as `claimed_by` pointing to cancelled child orders
3. Lane lock was never released — no future reshuffles could run on that lane until server restart

**Root cause:** `HandleOrderCancel` was written for simple (non-compound) orders. It calls `lifecycle.CancelOrder` which cancels the fleet order, unclaims bins, and updates status — but only for the single order passed to it. There was no check for whether the cancelled order was a compound parent with children. The failure path (`HandleChildOrderFailure`) existed for child-initiated failures but was never called from the parent cancel path.

**Fix:** Added `cancelCompoundChildren` in `compound.go`. `HandleOrderCancel` now checks if the order being cancelled is a compound parent (`StatusReshuffling`). If so, it iterates all non-terminal children and calls `lifecycle.CancelOrder` on each one (cancelling their fleet orders and unclaiming their bins), then unlocks the lane, before cancelling the parent.

**Production risk:** Any time an operator cancels a reshuffle — lane jammed, wrong bin targeted, shift change — the entire NGRP lane becomes permanently unusable. Bins claimed by the orphaned children can't be retrieved by new orders. The only recovery is manual database intervention or a server restart to clear the in-memory lane lock. On a busy floor this would cascade: operators can't get parts from that lane, production stops, manual workarounds needed.

**Status:** Fixed.

**Test:** `engine/engine_compound_test.go` — `TestCompound_CancelParentWhileChildInFlight`

---

### TC-60: Single-robot 10-step swap — multi-bin junction table fix

**Scenario:** A single-robot swap moves two bins in one trip: new material from storage to the line, old material from the line to outbound destination. The 10-step sequence has two pickup steps at different nodes. `claimComplexBins` claims both bins (iterates all pickup steps), but `Order.BinID` is `*int64` — it can only track the first claimed bin (the new bin at storage). The second bin (old bin at line) is claimed but invisible to the completion path.

**Expected behavior:** New bin at lineNode. Old bin at outboundDest. Both unclaimed.

**Result:** BUG FOUND AND FIXED. Two defects resolved via `order_bins` junction table (migration v9):

1. **Wrong destination (fixed):** `resolvePerBinDestinations` now simulates bin flow through the step sequence to compute where each bin ends up. `handleMultiBinCompleted` moves all bins to their per-step destinations via `ApplyMultiBinArrival` (single atomic transaction).

2. **Orphaned claim (fixed):** `claimComplexBins` populates the `order_bins` junction table for multi-pickup orders. `DeleteOrderBins` added to all 5 `UnclaimOrderBins` call sites. Junction table cleaned up on success, cancel, and failure paths.

**Root cause:** `Order.BinID` is `*int64` (single bin). The completion path (`handleOrderCompleted` → `ApplyBinArrival`) processes exactly one bin. Multi-pickup orders need a junction table (`order_bins`) and per-step bin tracking.

**Production risk:** Any multi-pickup complex order (single-robot swap is the primary pattern) triggered both defects prior to the fix. Now resolved — the single-robot swap pattern is safe for production use.

**Status:** FIXED. Both defects resolved by `order_bins` junction table. 6 unit tests for `resolvePerBinDestinations` (swap, re-staging, ghost pickup, empty dropoff, same-node conflict). TC-60 passes with both bins at correct destinations and unclaimed.

**Test:** `engine/engine_complex_test.go` — `TestComplexOrder_SingleRobotSwap`

## Verified scenarios

### TC-40a: Buried bin reshuffle via engine pipeline — PASS

**Scenario:** An NGRP node group contains a LANE with 3 physical slots. A blocker bin sits at depth 1 (front, newer `loaded_at`). A target bin sits at depth 2 (buried, `loaded_at` 2 hours ago). FIFO retrieval targeting the NGRP detects the buried target as older than any accessible bin and triggers a compound reshuffle order.

**Expected behavior:** A compound order is created with 3 child steps: (1) unbury blocker → shuffle slot, (2) retrieve target → line node, (3) restock blocker → original slot. Each child dispatches through the fleet simulator and completes sequentially. The target bin arrives at the line node. The blocker is restocked. All claims released. Lane lock freed.

**Result:** PASS. The FIFO GroupResolver correctly detected the buried bin, `PlanReshuffle` generated the 3-step compound order, and the engine wiring drove each child through the simulator lifecycle. The compound order completed with the target bin at the line and the lane lock released.

**Test:** `engine/engine_compound_test.go` — `TestBuriedBin_ReshuffleViaEngine`

---

### TC-40b: COST mode — oldest accessible returned, buried ignored — PASS

**Scenario:** Same 3-lane NGRP layout as TC-40a (oldest bin buried at depth 3 behind two blockers, newer accessible bins at the front of other lanes). A retrieve order fires with `retrieve_algorithm = COST`.

**Expected behavior:** `resolveRetrieveCOST` should return the oldest accessible bin without scanning buried bins. No reshuffle triggered. The fleet should receive a direct retrieve to the accessible bin's storage slot. This validates the cost-optimized behavior that avoids unnecessary reshuffles.

**Result:** PASS. Two sub-tests verified: (1) when accessible bins exist, COST mode returns the oldest accessible bin (BIN-MID at T+1s) and ignores the buried older bin (BIN-OLD at T). No `BuriedError` raised. (2) When no accessible bins exist at all (only buried bins behind blockers), COST mode falls back to returning a `BuriedError` for the buried bin, which triggers a reshuffle — the same behavior as FIFO mode in this degenerate case.

**Tests:** `dispatch/group_resolver_test.go` — `TestTC40b_COSTIgnoresBuriedWhenAccessible`, `TestTC40b_COSTFallsToBuriedWhenNoAccessible`

---

### TC-41: Empty cart starvation — no accessible empties — PASS

**Scenario:** All accessible empty bins in an NGRP have been consumed through normal retrieves. Only buried empties remain (behind full bins at deeper lane depths). A press drops a full bin and needs an empty pickup. `FindEmptyCompatibleBin` looks for an empty bin, but every matching empty is physically unreachable.

**Expected behavior:** The system should detect that the empty is buried and trigger a reshuffle to unbury the shallowest one (fewest blockers), rather than dispatching a robot to an inaccessible slot.

**Result:** PASS. Two complementary tests verified: (1) Unit level (`group_resolver_test.go`): confirms the gap — `FindEmptyCompatibleBin` is lane-unaware and returns a buried empty, but `IsSlotAccessible` correctly reports it as unreachable. This documents the pre-fix behavior where a robot would be sent to a slot it can't physically access. (2) Integration level (`integration_test.go`): after the fix, the `retrieve_empty` path with buried empties creates a reshuffle compound order (status `reshuffling` with compound children) instead of dispatching directly to the unreachable slot.

**Tests:** `dispatch/group_resolver_test.go` — `TestTC41_EmptyStarvation_BuriedEmptiesUnreachable`; `dispatch/integration_test.go` — `TestTC41_RetrieveEmpty_BuriedEmptyTriggersReshuffle`

---

### TC-42: Complex order cancel mid-transit — auto-return with SourceNode

**Scenario:** A complex order (pickup → dropoff → wait → pickup → dropoff) is dispatched. The robot is RUNNING (in transit). The operator cancels the order. The bin was claimed via `claimComplexBins` and is physically on the robot.

**Expected behavior:** Order cancelled. Bin claim released. An auto-return order is created (via `maybeCreateReturnOrder`) to bring the bin back to its origin. The return order's `SourceNode` must match the original order's `SourceNode` — the storage node where the bin started, not the destination the robot never reached. The return order re-claims the bin. No bin is permanently stranded.

**Why this matters:** This test caught Bug 1 (maybeCreateReturnOrder wrong SourceNode). Before the fix, the return order's `SourceNode` was set to `order.DeliveryNode` (the destination), sending the return robot to the wrong location. Now asserts the SourceNode is correct.

**Result:** PASS. Auto-return order created with correct SourceNode. Bin claim transferred. No stranding.

**Test:** `engine/engine_complex_test.go` — `TestComplexOrder_CancelMidTransit`

---

### TC-43: Complex order fleet failure mid-transit — auto-return SourceNode

**Scenario:** A complex order (pickup → dropoff, no wait) is dispatched. Robot starts RUNNING, then fleet reports FAILED (breakdown, obstacle). The engine's failure handler fires.

**Expected behavior:** Order marked failed. Bin claim released (fixed in TC-30). Auto-return order created with correct `SourceNode` matching the original order's source. Same recovery as cancel, but triggered by fleet status rather than operator action.

**Why this matters:** This test directly exposed Bug 1 (maybeCreateReturnOrder wrong SourceNode). The original test used `t.Logf` — it observed the wrong SourceNode without failing. Strengthened to assert SourceNode correctness, which caught the bug.

**Result:** PASS. Failure handler correctly releases claim and creates auto-return with correct SourceNode.

**Test:** `engine/engine_complex_test.go` — `TestComplexOrder_FleetFailureMidTransit`

---

### TC-44: Compound child failure mid-reshuffle — blocker stranding

**Scenario:** A 3-step reshuffle is in progress (unbury blocker → retrieve target → restock blocker). Step 1 completes successfully — the blocker bin moves from its lane slot to the shuffle slot. Step 2 (retrieve target) fails — robot breaks down mid-transit.

**Expected behavior:** `HandleChildOrderFailure` cancels remaining children, fails the parent, and releases the lane lock. The blocker bin is now physically at the shuffle slot (moved by completed step 1), unclaimed, and accessible for manual recovery or a new reshuffle. The target bin remains at its original slot, unclaimed. No bins permanently stuck.

**Why this matters:** This is the most dangerous failure mode for reshuffles. After partial completion, bins are in temporary positions that aren't their normal home. If claims aren't released or the lane lock isn't freed, recovery is impossible without database intervention.

**Result:** PASS. Parent failed, lane lock released, all bins unclaimed and accessible for recovery. Assertions strengthened: blocker position verified after both step 1 completion and step 2 failure, confirming bin moved to shuffle slot and claim released correctly.

**Test:** `engine/engine_compound_test.go` — `TestCompound_ChildFailureMidReshuffle_BlockerStranding`

---

### TC-45: Two-robot swap full lifecycle (5-step compound)

**Scenario:** An NGRP lane has 3 bins: target at depth 3 (oldest), blocker-2 at depth 2, blocker-1 at depth 1 (newest). FIFO retrieval detects the buried target and triggers a 5-step compound reshuffle: (1) unbury blocker-1 → shuffle-1, (2) unbury blocker-2 → shuffle-2, (3) retrieve target → line, (4) restock blocker-2 → depth 2, (5) restock blocker-1 → depth 1.

**Expected behavior:** All 5 children created and dispatched sequentially via `AdvanceCompoundOrder`. Target arrives at line node. Both blockers restocked to their original positions (deepest-first restocking). All claims released. Lane lock freed. Parent order confirmed.

**Why this matters:** The existing reshuffle test (`TestBuriedBin_ReshuffleViaEngine`) only exercises 3 steps (1 blocker). This test validates the full two-robot swap pattern with 2 blockers and 5 sequential steps — the most complex compound order pattern used in production.

**Result:** PASS. All 5 children complete sequentially. Target at line, blockers restocked to exact original slots (asserted), both status=available, lane lock freed.

**Test:** `engine/engine_compound_test.go` — `TestCompound_TwoRobotSwap_FullLifecycle`

---

### TC-46: Cancel parent compound while child in-flight — FIXED

Bug found and fixed. Full writeup in the Bugs found and fixed section above.

**Test:** `engine/engine_compound_test.go` — `TestCompound_CancelParentWhileChildInFlight`

---

### TC-47: Empty post-wait release — full lifecycle verification

**Scenario:** A complex order has steps `[pickup, dropoff, wait]` with nothing after the wait. The order is dispatched, the robot delivers to lineside, and the order enters DWELLING/staged status. Edge sends an `OrderRelease`. `HandleOrderRelease` parses `StepsJSON`, calls `splitPostWait`, gets an empty post-wait slice, and calls `ReleaseOrder(vendorOrderID, nil)`.

**Expected behavior:** No panic, no error. The fleet receives a release with empty blocks, which signals completion. The order transitions to confirmed. The bin moves to the dropoff destination (lineNode) and is unclaimed.

**Why this matters:** The original test only checked that no panic occurred and never drove the fleet through completion. Strengthened to include full lifecycle (DriveState FINISHED + HandleOrderReceipt) so `ApplyBinArrival` fires and the bin actually moves. This catches cases where the empty release completes the fleet order but the bin never moves in the database.

**Result:** PASS. Empty release completes cleanly. Bin at lineNode, unclaimed, order confirmed.

**Test:** `engine/engine_complex_test.go` — `TestComplexOrder_EmptyPostWaitRelease`

---

### TC-48: Complex order redirect doesn't update StepsJSON

**Scenario:** A complex order with a wait phase (`pickup A → dropoff B → wait → pickup B → dropoff C`) is dispatched and enters staged/dwelling status. The operator sends a redirect changing the delivery from node C to node D. `HandleOrderRedirect` updates `DeliveryNode` in the DB via `PrepareRedirect`, but `StepsJSON` still contains `"dropoff C"` in the post-wait steps.

**Expected behavior (documenting bug):** When `HandleOrderRelease` fires, it reads `StepsJSON` and builds fleet blocks from the stored steps — which still reference node C. The fleet routes the robot to the old destination, not the redirected one.

**Why this matters:** In production, operators redirect orders when lineside demand shifts. If the redirect doesn't update `StepsJSON`, the post-wait phase sends the robot to the wrong node. This test documents the bug so it can be fixed before complex orders with wait+redirect are used.

**Result:** PASS. Test confirms StepsJSON is stale after redirect — documenting the known issue for future fix.

**Test:** `engine/engine_complex_test.go` — `TestComplexOrder_RedirectStaleStepsJSON`

---

### TC-49: Ghost robot — claimComplexBins finds no bin at pickup

**Scenario:** A complex order specifies a pickup at a node that has no bins matching the payload (or all bins are already claimed).

**Bug:** `claimComplexBins` was best-effort — it logged a warning but let the order dispatch with `BinID=nil`, sending a ghost robot to an empty node. Same class of bug as TC-23c but in the complex order path.

**Fix:** `claimComplexBins` now returns a `planningError{Code: "no_bin"}` when zero bins are claimed. `HandleComplexOrderRequest` calls `failOrder` and returns before dispatching to fleet. This aligns complex orders with `planStore`'s pre-dispatch validation.

**Result:** FIXED. Order fails at planning with status=failed, no vendor order created, no ghost robot.

**Test:** `engine/engine_complex_test.go` — `TestComplexOrder_GhostRobotNoBin`

---

### TC-50: Concurrent complex orders same node — double claim race

**Scenario:** Two complex orders are submitted sequentially, both picking up from the same storage node that has only one available bin. `claimComplexBins` runs for both sequentially (same goroutine in current architecture). The first order claims the bin; the second finds no unclaimed bins.

**Expected behavior:** First order claims the bin and dispatches. Second order fails at planning with "no_bin" error — no ghost robot dispatched. No double-claim occurs because `ClaimBin` uses an atomic update.

**Why this matters:** In production, concurrent retrieve requests targeting the same NGRP are common. The claim mechanism must be race-safe. If two orders both get `BinID` pointing to the same bin, both robots arrive expecting the same bin.

**Result:** FIXED (was PASS/observational). First order claims and dispatches. Second fails at planning with status=failed. No double-claim.

**Test:** `engine/engine_complex_test.go` — `TestComplexOrder_ConcurrentSameNodeDoubleClaimRace`

---

### TC-51: AdvanceCompoundOrder skips failed children — premature completion

**Scenario:** A 3-step compound order where child 2 has an invalid source node (empty string, simulating data corruption). `AdvanceCompoundOrder` dispatches child 1, which completes. When advancing to child 2, lines 77-98 in `compound.go` detect the missing source/delivery, mark child 2 as failed, and recursively call `AdvanceCompoundOrder`. This skips to child 3.

**Expected behavior (documenting risk):** If child 2 was the retrieve step and child 3 is the restock, the parent may "complete" even though the target bin was never retrieved. The parent reaches `StatusConfirmed` with a failed child — data inconsistency.

**Why this matters:** If a compound child's data is corrupted (race condition, DB error), the current code silently skips it and proceeds. The parent should probably fail if any child fails, but the current recursive skip behavior may mask the issue.

**Result:** PASS. Confirms the skip behavior exists — parent completes despite failed child. Documents the risk for future review.

**Test:** `engine/engine_compound_test.go` — `TestCompound_AdvanceSkipsFailedChild_PrematureCompletion`

---

### TC-52: Lane lock contention — second reshuffle blocked

**Scenario:** A retrieve order triggers a reshuffle on a lane. While the reshuffle is active (lane locked), a second retrieve order targets the same NGRP. The planning service detects the lane lock at line 212 of `planning_service.go` and returns a `lane_locked` `planningError`.

**Expected behavior:** The `lane_locked` error goes through `queueOrder`. The second order is marked QUEUED and will be retried by the fulfillment scanner when the lane unlocks. Verified correct — no operator resubmission needed.

**Why this matters:** In production, multiple operators may request bins from the same NGRP simultaneously. This test confirms the system handles it correctly — the second order waits for the reshuffle to complete rather than failing permanently.

**Result:** PASS. Second order queued correctly via `queueOrder`. No permanent failure.

**Test:** `engine/engine_compound_test.go` — `TestLaneLock_Contention_SecondReshuffleBlocked`

---

### TC-53: ApplyBinArrival status for compound restock children

**Scenario:** A compound restock child delivers a blocker bin back to its storage slot (a child node of a LANE). When `handleOrderCompleted` calls `ApplyBinArrival`, it checks if the destination is a storage slot (parent type LANE). If so, it sets `status='available'` instead of `staged`.

**Expected behavior:** After restock, the blocker bin at the storage slot should have `status='available'`, `claimed_by=NULL`, and be visible to `FindSourceBinFIFO` queries. If it were marked `staged`, it would be invisible to FIFO and effectively orphaned.

**Why this matters:** This is a critical correctness check for the restock phase of reshuffles. If `ApplyBinArrival` doesn't correctly detect the storage slot, restocked blockers become invisible bins — they exist in the DB but can't be retrieved.

**Result:** PASS. Bin status correctly set to `available` at storage slot. Visible to `FindSourceBinFIFO`. Strengthened: FIFO query now asserted to return exactly the restocked blocker bin (`fifoBin.ID == blockerBin.ID`), not just any available bin.

**Test:** `engine/engine_compound_test.go` — `TestCompound_RestockChild_BinStatusAvailable`

---

### TC-54: Staging TTL expiry during compound order execution

**Scenario:** During a compound reshuffle, child 1 (unbury blocker) completes and delivers the blocker to a shuffle slot (non-storage node). `ApplyBinArrival` marks it as `staged` with a TTL. If the reshuffle takes longer than the TTL (e.g., robot breakdown delays child 2), the staging sweep runs and flips the blocker bin to `available`.

**Expected behavior:** The restock child (child 3) should still work correctly even if the bin's status changed from `staged` to `available` due to TTL expiry. The bin is still physically at the shuffle slot and should still be claimable by the restock child.

**Why this matters:** Compound reshuffles can take several minutes (robot travel time, queuing). If the staging TTL is shorter than the reshuffle duration, the sweep silently changes bin status mid-operation. This test verifies the restock child is resilient to this status change.

**Result:** PASS. Restock child completes normally despite TTL-driven status change. Bin correctly restocked. Strengthened: sweep correctly flips bin status from staged to available (asserted), confirming the sweep ran and the compound completed despite mid-operation status change. Note: the blocker's claim is legitimately nil after child 1 completion (ApplyBinArrival releases it), NOT because the sweep stripped it.

**Test:** `engine/engine_compound_test.go` — `TestCompound_StagingTTLExpiryDuringReshuffle`

---

### TC-55: Sequential Backfill — simplest complex order lifecycle — PASS

**Scenario:** `pickup(storage) → dropoff(line)`. No wait. The simplest possible complex order — still goes through StepsJSON, claimComplexBins, stepsToBlocks. One bin at storage, delivered to line.

**Expected behavior:** Bin claimed at storage. 2 blocks (JackLoad/JackUnload), complete=true. After full lifecycle: bin at line, unclaimed.

**Result:** PASS. Full end-to-end lifecycle through the complex order path.

**Test:** `engine/engine_complex_test.go` — `TestComplexOrder_SequentialBackfill`

---

### TC-56: Sequential Removal — wait/release with post-wait bin claiming — PASS

**Scenario:** `dropoff(line) → wait → pickup(line) → dropoff(outboundDest)`. Robot navigates empty to line, waits for operator, picks up old bin, delivers to outbound destination. First step is dropoff — tests whether `claimComplexBins` finds bins at post-wait pickup steps.

**Expected behavior:** `claimComplexBins` iterates ALL steps including post-wait, finds the pickup at lineNode, claims oldBin. 1 pre-wait block, staged. After release: 3 blocks. After completion: bin at outboundDest.

**Why this matters:** Confirms bin claiming works for dropoff-first orders where the pickup is after the wait. The claim happens at dispatch time, protecting the bin for the entire order lifecycle.

**Result:** PASS.

**Test:** `engine/engine_complex_test.go` — `TestComplexOrder_SequentialRemoval`

---

### TC-57: Two-Robot Swap Resupply — 2 pre-wait blocks, staging wait — PASS

**Scenario:** `pickup(storage) → dropoff(inboundStaging) → wait → pickup(inboundStaging) → dropoff(line)`. Resupply robot picks up new material, stages at inbound staging, waits for line to clear, then delivers.

**Expected behavior:** Bin claimed at storage. 2 pre-wait blocks, staged. After release: 4 total blocks. After full lifecycle: bin at line, unclaimed.

**Result:** PASS. All 4 block locations and bin tasks verified in order.

**Test:** `engine/engine_complex_test.go` — `TestComplexOrder_TwoRobotSwap_Resupply`

---

### TC-58: Two-Robot Swap Removal — dropoff-first pre-wait, full lifecycle — PASS

**Scenario:** `dropoff(line) → wait → pickup(line) → dropoff(outboundDest)`. Same structure as sequential removal but in the two-robot context. Robot pre-positions at line, waits for operator, picks up old bin, delivers to outbound destination.

**Expected behavior:** 1 pre-wait block, staged. Bin claimed via post-wait pickup. After release: 3 blocks. After full lifecycle including receipt: bin at outboundDest, unclaimed.

**Result:** PASS. Full lifecycle including `HandleOrderReceipt` → `ApplyBinArrival`.

**Test:** `engine/engine_complex_test.go` — `TestComplexOrder_TwoRobotSwap_Removal`

---

### TC-59: Staging + Deliver separation — two independent orders — PASS

**Scenario:** Two independent orders. Stage: `pickup(storage) → dropoff(inboundStaging)`. Deliver: `pickup(inboundStaging) → dropoff(line)`. Stage completes first, bin at inboundStaging with status=staged. Deliver claims the staged bin and delivers to line.

**Expected behavior:** Stage order completes: bin at inboundStaging, unclaimed, bin status=staged. Deliver order claims the staged bin (`claimComplexBins` allows "staged" bins at production nodes). After deliver completes: bin at line, unclaimed.

**Why this matters:** Confirms `claimComplexBins` includes "staged" bins in its search — unlike `FindSourceBinFIFO` which excludes them. Staged bins at storage slots are invisible to retrieves, but staged bins at production/staging nodes must be visible to complex orders.

**Result:** PASS. Two independent vendor orders, bin flows correctly through both.

**Test:** `engine/engine_complex_test.go` — `TestComplexOrder_StagingAndDeliver`
