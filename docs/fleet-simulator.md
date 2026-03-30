# Fleet Simulator

A testing tool for the Shingo warehouse dispatch system. The simulator replaces the real SEER RDS fleet server with a fake version that runs entirely in software, so you can test the full order lifecycle (dispatch, robot movement, bin arrival, claim release) without robots, RDS, Kafka, or Edge hardware.

This document is written for anyone who needs to understand what the simulator tests, what it found, and how to use it. You don't need to be a software developer to follow along.

---

## How it works

When Shingo Core sends an order to the fleet, it normally goes to the SEER RDS server which controls the physical robots. The simulator intercepts that communication and pretends to be RDS. It accepts orders, records what blocks and bin tasks were sent, and lets you manually advance the robot through its stages (created, running, waiting, finished).

The test database is a real Postgres database running inside Docker on your machine. Everything the dispatcher writes (orders, bin claims, status changes) goes into this real database, so the tests verify actual database behavior, not mocked-up responses.

```
Your test code  ──▶  Dispatcher (real production code)  ──▶  Simulator (fake fleet)
       │                        │                                    │
       │                        ▼                                    │
       │                 Real Postgres DB                            │
       │                  (via Docker)                               │
       ▼                                                             ▼
  Checks database                                            Records what the
  for correct state                                          "fleet" received
```

The dispatcher doesn't know it's not talking to real robots. The simulator just records what it gets. Your test code sets up the scenario, triggers the actions, and checks the results.

---

## Running the tests

Docker Desktop must be running. The tests spin up a temporary Postgres container automatically. First run pulls the image (one time), after that it's cached. The container is torn down when the test finishes.

Run all simulator tests:

```
cd shingo-core
go test -v -run "TestSimulator|TestClaimBin|TestTC[0-9]|TestConcurrent|TestBuriedBin|TestFulfillmentScanner|TestRedirect|TestComplexOrder|TestCompound|TestLaneLock" ./engine/ ./dispatch/ -timeout 120s
```

Run a specific test:

```
go test -v -run TestSimulator_FullLifecycle ./engine/ -timeout 60s
```

If Docker isn't running, database tests skip automatically (they won't fail your build). Pure logic tests like state mapping always run.

---

## How to read this document

This document has two kinds of test results, organized into separate sections:

**Bugs found and fixed** are regression tests. Each one documents a real bug that the simulator found in the codebase. The entry explains what went wrong, shows the root cause, and describes the fix. The test exists to make sure that specific bug never comes back. If a regression test fails, it means someone reintroduced a bug that was already fixed — that's a high-priority problem.

**Verified scenarios** are scenario tests. Each one explores a "what if" situation that could happen on the floor. The test passed, meaning the system handles that situation correctly today. These tests exist to catch future regressions — if one starts failing after a code change, it means something broke that was previously working.

Each test case entry follows the same format:

- **Scenario** describes the situation in plain language — what's happening on the floor and what could go wrong. You don't need to read code to understand the risk.
- **Expected behavior** explains what the system is supposed to do. This is the "right answer."
- **Result** tells you whether the test passed, and if a bug was found, what happened and how it was fixed.
- **Root cause** (bugs only) explains why the bug existed — what the code was doing wrong.
- **Production risk** (bugs only) explains who this affects on the floor and under what conditions.
- **Code snippets** (where included) show the key lines from the test or fix. Optional reading — the scenario and result tell the full story without them.
- **Test** at the bottom gives the file name and test function, so a developer can find and run the specific test.

The "Scenarios to test next" section at the end is a prioritized catalog of situations we haven't tested yet. These are written the same way (scenario + expected behavior) so they can be turned into tests as the project continues.

---

## Test case index

| TC | Description | Status | Section |
|----|-------------|--------|---------|
| TC-1 | Robot drives to bin but doesn't jack it | PASS | Verified |
| TC-2 | Staged order release timing | PASS | Verified |
| TC-3 | Basic retrieve sends correct fleet instructions | PASS | Verified |
| TC-4 | Fleet state mapping (WAITING → staged, etc.) | PASS | Verified |
| TC-5 | Fleet down — no phantom return orders | PASS | Verified |
| TC-6 | Cancel at exact moment fleet accepts | — | To test |
| TC-7 | Release before robot reaches wait point | — | To test |
| TC-8 | Double release command | — | To test |
| TC-9 | Complex order with zero steps | PASS | Verified |
| TC-10 | Order references nonexistent delivery node | PASS | Verified |
| TC-11 | Only bin at disabled storage node | — | To test |
| TC-12 | Order requests zero quantity | PASS | Verified |
| TC-15 | Full lifecycle dispatch → receipt → bin arrival | PASS | Verified |
| TC-16 | Fleet reports an unknown state | — | To test |
| TC-17 | Fleet reports the same state twice | — | To test |
| TC-18 | Fleet reports states out of order | — | To test |
| TC-19 | Robot completes a very short trip | — | To test |
| TC-20 | Two lines same assembly — staging isolation | — | To test |
| TC-21 | Quality-hold bin not dispatched | PASS | Verified |
| TC-22 | Only bin at maintenance node | — | To test |
| TC-23a | Second store order skips claimed bin | PASS | Verified |
| TC-23b | Cancel transfers claim to return order | PASS | Verified |
| TC-23c | Changeover with missing bin — ghost robot | FIXED | Bug found |
| TC-23d | Changeover while move-to-QH in flight | PASS | Verified |
| TC-24a | Complex order bin poaching during transit | FIXED | Bug found |
| TC-24b | Stale bin location after complex order | FIXED | Bug found |
| TC-24c | Phantom inventory from stale location | FIXED | Bug found |
| TC-25 | Staged bin at core node — store order claim | DISMISSED | Verified (correct behavior) |
| TC-26 | Operator cancels — bin reservation release | ~COVERED | See TC-23b (gap remains) |
| TC-27 | Redirect preserves bin reservation | COVERED | See Redirect mid-transit |
| TC-28 | Two lines request same part simultaneously | PASS | Verified |
| TC-29 | Cancel while robot in transit | — | To test |
| TC-30 | Fleet failure leaves bin claim dangling | FIXED | Bug found |
| TC-31 | Order finishes, freed bin picked up by waiting order | — | To test |
| TC-32 | Staging expiry vs active reservation | — | To test |
| TC-33 | Manual move of reserved bin | — | To test |
| TC-34 | Complex order dispatches to node with no bin | ~COVERED | See TC-49 (gap remains) |
| TC-35 | planMove dispatches robot with no bin | — | To test |
| TC-36 | Retrieve claim failure — queue instead of fail | FIXED | Bug found |
| TC-37 | Staging expiry strips status from claimed bin | FIXED | Bug found |
| TC-38 | Multi-pickup complex order leaves secondary bins stranded | — | To test |
| TC-39 | Cross-line poaching of producer empty bins | — | To test |
| TC-40a | FIFO mode — buried older than accessible triggers reshuffle | PASS | Verified |
| TC-40b | COST mode — oldest accessible returned, buried ignored | PASS | Verified |
| TC-41 | Empty cart starvation — no accessible empties | PASS | Verified |
| TC-42 | Complex order cancel mid-transit — auto-return with SourceNode | FIXED | Bug found |
| TC-43 | Complex order fleet failure mid-transit — auto-return SourceNode | FIXED | Bug found |
| TC-44 | Compound child failure mid-reshuffle — blocker stranding | PASS | Verified |
| TC-45 | Two-robot swap full lifecycle (5-step compound) | PASS | Verified |
| TC-46 | Cancel parent compound while child in-flight | FIXED | Bug found + Verified |
| TC-47 | Empty post-wait release — full lifecycle verification | PASS | Verified |
| TC-48 | Complex order redirect — StepsJSON stale after redirect | PASS | Verified (documents known issue) |
| TC-49 | Ghost robot — claimComplexBins finds no bin at pickup | PASS | Verified (documents behavior) |
| TC-50 | Concurrent complex orders same node — double claim race | PASS | Verified |
| TC-51 | AdvanceCompoundOrder skips failed children — premature completion | PASS | Verified (documents risk) |
| TC-52 | Lane lock contention — second reshuffle queued correctly | PASS | Verified |
| TC-53 | ApplyBinArrival status for compound restock children | PASS | Verified |
| TC-54 | Staging TTL expiry during compound order execution | PASS | Verified |
| — | maybeCreateReturnOrder sends bin to wrong node | FIXED | Bug found |
| — | HandleChildOrderFailure leaves in-flight siblings orphaned | FIXED | Bug found |
| TC-55 | Sequential Backfill — simplest complex order lifecycle | PASS | Verified |
| TC-56 | Sequential Removal — wait/release with post-wait bin claim | PASS | Verified |
| TC-57 | Two-Robot Swap Resupply — 2 pre-wait blocks, staging wait | PASS | Verified |
| TC-58 | Two-Robot Swap Removal — dropoff-first pre-wait, full lifecycle | PASS | Verified |
| TC-59 | Staging + Deliver separation — two independent orders | PASS | Verified |
| TC-60 | Single-robot 10-step swap — multi-bin junction table fix | PASS | Bug found + FIXED |
| — | ClaimBin silent overwrite | FIXED | Bug found |
| — | Deterministic TOCTOU claim race (PostFindHook) | PASS | Verified |
| — | Dispatch stress — 20 concurrent orders, 10 bins | PASS | Verified |
| — | Redirect mid-transit — claim intact | PASS | Verified |
| — | Fulfillment scanner — queue to dispatch round-trip | PASS | Verified |

---

## Bugs found and fixed

Each entry below documents a real bug found by the simulator. These are regression tests — they protect specific fixes from being accidentally undone.

---

### ClaimBin: Two orders claim the same bin — silent overwrite

**Scenario:** Two production lines both need the same part at roughly the same time. Both orders query for available bins and find the same one. The first order claims it. Then the second order claims the same bin. What happens to the first order's reservation?

**Expected behavior:** The second claim should be rejected. The bin is already reserved by the first order. The second order should get an error and look for a different bin.

**Result:** BUG FOUND. The second claim silently succeeded. The bin's reservation was overwritten from order 100 to order 200 with no error. The first order still thought it had a bin reserved, but that bin was now committed to the second order. When the first order's robot arrived, the bin could already be gone.

**Root cause:** The database query that claims a bin checked if the bin was locked, but never checked if it was already claimed by another order.

```sql
-- Before (broken): silently overwrites existing claims
UPDATE bins SET claimed_by=$1 WHERE id=$2 AND locked=false

-- After (fixed): rejects claim if bin is already reserved
UPDATE bins SET claimed_by=$1 WHERE id=$2 AND locked=false AND claimed_by IS NULL
```

**Production risk:** Under normal load, this race window is narrow because the system filters out already-claimed bins before attempting to claim. But with multiple lines requesting parts simultaneously, two orders can both find the same bin in the gap between one order's search and its claim. This is more likely during shift starts or when inventory is low.

**Status:** Fixed. The second claim now returns an error: `"bin 1 is locked, already claimed, or does not exist"`.

**Test:** `engine/engine_test.go` — `TestClaimBin_SilentOverwrite`

---

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

### TC-30: Fleet-reported failure leaves bin claim dangling

**Scenario:** A retrieve order is dispatched and the robot starts moving (RUNNING). The fleet reports the order as FAILED (robot broke down, obstacle, etc). The system marks the order as failed and tries to create a return order. But the original order's bin claim is never released. The return order can't claim the bin because it's still locked to the dead order.

**Expected behavior:** When the fleet reports a failure, the system should release the bin claim (same as it does for cancellation), then create the return order which re-claims the bin.

**Result:** BUG FOUND. The `handleVendorStatusChange` handler in wiring.go called `UnclaimOrderBins` for `StatusCancelled` (line 223) but not for `StatusFailed`. The cancellation path was correct; the failure path was missing the same cleanup step. The bin stayed claimed by the failed order, and `maybeCreateReturnOrder` couldn't re-claim it for the return.

**Root cause:** Asymmetry between the cancel and failure handlers in the same function. The cancel case had `UnclaimOrderBins` added at some point, but the failure case was never updated to match.

```go
// Before: failure case did NOT unclaim bins
case dispatch.StatusFailed:
    e.db.UpdateOrderStatus(order.ID, dispatch.StatusFailed, "fleet order failed")
    // missing: e.db.UnclaimOrderBins(order.ID)
    e.Events.Emit(Event{Type: EventOrderFailed, ...})

// After: failure case now unclaims, matching the cancel case
case dispatch.StatusFailed:
    e.db.UpdateOrderStatus(order.ID, dispatch.StatusFailed, "fleet order failed")
    e.db.UnclaimOrderBins(order.ID)  // ← ADDED
    e.Events.Emit(Event{Type: EventOrderFailed, ...})
```

**Production risk:** Any time a robot fails mid-delivery (breakdown, obstacle, emergency stop), the bin it was carrying becomes permanently stuck. No other order can claim it, and the auto-return mechanism can't work. The bin effectively disappears from the system's available inventory until someone manually intervenes in the database. This is most likely during peak hours when robots are more likely to encounter obstacles.

**Status:** Fixed. The failure handler now calls `UnclaimOrderBins` before emitting the failure event, matching the cancel handler's behavior.

**Test:** `engine/engine_test.go` — `TestTC30_FailedOrderReturnClaimTransfer`

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

### TC-25: Staged bin at core node — store order claim (DISMISSED)

Investigated whether `planStore`/`planMove` could "poach" a staged bin at a lineside core node. Originally flagged as a gap because these planners only check `claimed_by`, not `status`.

**Dismissed:** Physical constraint — a core node holds exactly one bin. After a retrieve delivers a bin (staged, unclaimed), the only bin at that node IS the bin the operator wants to act on. Store and move orders targeting a core node as source SHOULD claim the staged bin — that's how the operator releases it (store-back to storage, quality hold move, partial release). Filtering out staged bins would break these legitimate operator workflows.

The `staged` status correctly protects against `FindSourceBinFIFO` (retrieve orders don't pull from lineside), while remaining visible to `planStore`/`planMove` (operator-initiated releases). This is working as intended.

**Test:** `engine/engine_test.go` — `TestTC25_StoreOrderClaimsStagedBinAtCoreNode` (positive assertion that store order correctly claims staged bin)

---

### TC-36: Retrieve claim failure — order permanently failed instead of queued

**Scenario:** Two retrieve orders for the same payload fire nearly simultaneously. Both call `FindSourceBinFIFO` and find the same unclaimed bin. Order A calls `ClaimBin` first and succeeds. Order B calls `ClaimBin` and gets rejected — "bin is locked, already claimed, or does not exist." This is the classic TOCTOU (time-of-check-time-of-use) gap between the `SELECT` in `FindSourceBinFIFO` and the `UPDATE ... WHERE claimed_by IS NULL` in `ClaimBin`.

**Expected behavior:** The second order should be queued (status `queued`) so the fulfillment scanner retries when a bin becomes available — exactly like when `FindSourceBinFIFO` finds no bins at all. `claim_failed` is a transient condition: bins of the right payload DO exist, one was just claimed by a concurrent order.

**Result:** BUG FOUND. `planRetrieve` returned `planningError{Code: "claim_failed"}`, which `HandleOrderRequest` passed directly to `failOrder`. The order was permanently set to `StatusFailed` with an `order.error` message sent to Edge. The operator would see a failure and need to manually resubmit.

**Root cause:** `HandleOrderRequest` treated all `planningError` results the same — permanent failure. The distinction between permanent errors (`node_error`, `no_storage`) and transient errors (`claim_failed`) was not made at the dispatch level.

```go
// Before (broken): all planning errors permanently fail the order
result, planErr := d.planner.Plan(order, env, payloadCode)
if planErr != nil {
    d.failOrder(order, env, planErr.Code, planErr.Detail)
    return
}

// After (fixed): claim_failed is transient — queue for retry
if planErr.Code == "claim_failed" {
    d.queueOrder(order, env, payloadCode)
    return
}
d.failOrder(order, env, planErr.Code, planErr.Detail)
```

**Production risk:** Any time two production lines request the same part simultaneously, or a line and the fulfillment scanner compete for the same bin, one order dies permanently. More likely during shift starts when multiple lines kick off at once, or when the fulfillment scanner's retry coincides with a new order from Edge. The operator gets a failure notification, resubmits, and the second attempt usually succeeds — but the failed order leaves a confusing audit trail and the operator loses trust in the system.

**Status:** Fixed. `HandleOrderRequest` now checks for `planErr.Code == "claim_failed"` and calls `queueOrder` instead of `failOrder`. The fulfillment scanner retries on its next sweep. The same pattern exists in `planRetrieveEmpty` (empty bin retrieval) — both paths are covered by the fix in `HandleOrderRequest`.

**Test:** `engine/engine_test.go` — `TestTC36_RetrieveClaimFailure_QueueNotFail`

---

### TC-37: Staging sweep flips bin to available while still actively claimed

**Scenario:** A bin is delivered to a lineside node (status `staged`, `claimed_by=nil` via `ApplyBinArrival`). An operator creates a second order that claims the bin (`claimed_by` set to the new order's ID). The lineside node has a staging TTL. The TTL expires and the staging sweep runs. Does the sweep check for active claims before flipping the bin to `available`?

**Expected behavior:** The staging sweep should skip bins with active claims. A bin that is `claimed_by` a live order should remain `staged` regardless of TTL expiry. The claim protects the bin from being treated as generally available.

**Result:** BUG FOUND. `ReleaseExpiredStagedBins` used a SQL `WHERE` clause that checked `status='staged'`, `claimed_by IS NULL`, and `staged_expires_at < NOW()` — but the `claimed_by IS NULL` check was missing from the original query. The sweep flipped the bin to `available` while it was still claimed, creating a contradictory state (`status=available`, `claimed_by=123`).

**Root cause:** The original SQL was:
```sql
-- Before (broken): no claimed_by check
UPDATE bins SET status='available', ...
WHERE status='staged' AND staged_expires_at IS NOT NULL AND staged_expires_at < NOW()
```

**Fix:** Added `AND claimed_by IS NULL`:
```sql
-- After (fixed): respects active claims
UPDATE bins SET status='available', ...
WHERE status='staged' AND claimed_by IS NULL AND staged_expires_at IS NOT NULL AND staged_expires_at < NOW()
```

**Production risk:** A bin in the contradictory state (`available` but still `claimed`) would cause UI confusion — `NodeTileState` would show `Staged: false, Claimed: true`. Not a double-dispatch risk (the claim still protects), but misleading for operators and monitoring dashboards.

**Status:** Fixed. The staging sweep now checks `claimed_by IS NULL` before releasing expired bins.

**Test:** `engine/engine_concurrent_test.go` — `TestTC37_StagingExpiryVsActiveClaim`

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

### maybeCreateReturnOrder sends bin to wrong node

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

### HandleChildOrderFailure leaves in-flight siblings orphaned

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

---

## Verified scenarios

Each entry below is a scenario test that passed. The system handles these situations correctly. These tests guard against future regressions.

---

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

### TC-21: Only available bin is in quality hold — PASS

**Scenario:** A line requests a part. The only bin of that part in the warehouse is in quality hold (flagged for inspection). Should the system dispatch a held bin?

**Expected behavior:** The system should not dispatch a quality-hold bin. The order should be queued (not failed) so the fulfillment scanner can retry when inventory frees up.

**Result:** PASS. `FindSourceBinFIFO` correctly filters out bins with `status = 'quality_hold'`. The order is queued with no bin assigned, no robot dispatched. The bin remains untouched at its node.

**Test:** `engine/engine_test.go` — `TestTC21_QualityHoldBinNotDispatched`

---

### TC-23a: Second store order doesn't steal a claimed bin — PASS

**Scenario:** A bin at the line is claimed by an active store order (robot moving it to QH). The operator submits another store order at the same line. The system should skip the claimed bin and pick a different one.

**Expected behavior:** The second store order claims one of the other unclaimed bins. The first order's bin claim stays intact.

**Result:** PASS. The second order claimed a different bin. The first order's bin was correctly protected by its `claimed_by` value.

**Test:** `engine/engine_test.go` — `TestTC23a_MoveClaimedStagedBin`

---

### TC-23b: Cancel transfers claim to return order, second store picks different bin — PASS

**Scenario:** Operator cancels an in-flight store order. The system unclaims the bin, then auto-creates a return order that immediately re-claims it. The operator then submits another store order.

**Expected behavior:** The bin transfers from the cancelled order to the return order (never truly free). The subsequent store order claims a different bin.

**Result:** PASS. Bin claim correctly transferred from cancelled order 1 → return order 2. The third store order claimed a different bin and did not steal from the return order.

**Test:** `engine/engine_test.go` — `TestTC23b_CancelThenMoveBin`

---

### TC-23d: Changeover while move-to-quality-hold is still in flight — PASS

**Scenario:** Operator sends one bin to quality hold (robot in transit, bin claimed). Before that robot arrives, changeover begins with 2 more store orders. The changeover orders should skip the in-flight bin and only claim the 2 unclaimed bins.

**Expected behavior:** Each of the 3 orders claims a different bin. No double claims. After QH order completes, all bins are in a clean state.

**Result:** PASS. The changeover orders correctly skipped the in-flight bin and claimed the other 2. No overlapping claims detected.

**Test:** `engine/engine_test.go` — `TestTC23d_ChangeoverWhileMoveInFlight`

---

### TC-28: Two lines request the same part at the same time — PASS

**Scenario:** Line 1 and Line 2 both need PART-A. Two bins of PART-A sit in storage (one per storage node). Both retrieve orders fire back-to-back. Does each order get a different bin, or do they collide?

**Expected behavior:** Each order should claim a different bin. No double-assignment. Both robots should be dispatched to different storage locations.

**Result:** PASS. Order 1 claimed bin 1 at STORAGE-A1, order 2 claimed bin 2 at STORAGE-A2. The sequential dispatch path serializes correctly — the first `ClaimBin` completes before the second `FindSourceBinFIFO` runs, so the second query sees the first bin as claimed and returns the next available.

**Note:** This validates the sequential case (same goroutine). A true concurrent race (two goroutines dispatching simultaneously) could still hit a TOCTOU gap where `FindSourceBinFIFO` and `ClaimBin` target the same bin. In that case, `planRetrieve` returns `claim_failed` instead of retrying with a different bin. In production, orders arrive over the network and get serialized through the event bus, so the sequential test reflects real behavior.

**Test:** `engine/engine_test.go` — `TestTC28_ConcurrentRetrieveSamePart`

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

---

### Deterministic TOCTOU claim race (PostFindHook) — PASS

**Scenario:** Two orders compete for the same bin. A PostFindHook is installed between `FindSourceBinFIFO` and `ClaimBin` to widen the TOCTOU race window. Goroutine 1 finds the bin, hits the hook, and pauses. Goroutine 2 starts, finds the same bin, claims it successfully. Goroutine 1 resumes, its `ClaimBin` fails with `claim_failed`. The test is 100% deterministic — the hook guarantees both goroutines enter the TOCTOU window simultaneously.

**Expected behavior:** One order dispatches with the bin claimed. The other is queued (not permanently failed) so the fulfillment scanner retries when a bin becomes available.

**Result:** PASS. The hook guarantees the race. One order dispatches, the other is queued with status `queued`. Neither order permanently fails.

**Test:** `engine/engine_concurrent_test.go` — `TestConcurrent_ClaimRaceDeterministic`

---

### Dispatch stress — 20 concurrent orders, 10 bins — PASS

**Scenario:** 20 orders fire simultaneously against 10 bins (2:1 contention ratio). `GOMAXPROCS` is set to `runtime.NumCPU()` to maximize real concurrency. Tests whether the claim-then-dispatch path produces any double-claims or permanent failures under pressure.

**Expected behavior:** Each bin is claimed by at most 1 order. No order permanently fails — excess orders should be queued.

**Result:** PASS. No double-claims detected. No orders permanently failed.

**Test:** `engine/engine_concurrent_test.go` — `TestConcurrent_DispatchStress`

---

### Redirect mid-transit — claim stays intact — PASS

**Scenario:** A retrieve order is dispatched and the robot reaches RUNNING (in_transit). The operator redirects the order to a different line node. The old vendor order should be cancelled in the fleet, a new one created, and the bin claim should remain intact.

**Expected behavior:** After redirect, the order's bin claim survives. A new vendor order is dispatched to the new destination. The claimed bin is still claimed by the same order.

**Result:** PASS. Bin claim intact after redirect. New vendor order dispatched to the second line.

**Test:** `engine/engine_concurrent_test.go` — `TestRedirect_MidTransit`

---

### Fulfillment scanner — queue to dispatch round-trip — PASS

**Scenario:** A retrieve order is submitted but no bins are available. The order is queued. Later, a compatible bin appears at a storage node. The fulfillment scanner runs and dispatches the queued order.

**Expected behavior:** The order starts as `queued`. After a bin appears and the scanner runs, the order transitions to `dispatched` with a bin claimed. Driving through the full lifecycle (RUNNING → FINISHED → receipt) should complete the order and move the bin to the destination.

**Result:** PASS. Full queue → scan → dispatch → deliver → confirm round-trip verified. Bin correctly at line node, claim released after completion.

**Test:** `engine/engine_concurrent_test.go` — `TestFulfillmentScanner_QueueToDispatch`

---

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

## Verified scenarios — complex and compound orders

These tests target the complex order and compound reshuffle code paths — sequential removal, one-robot swap, and two-robot swap patterns. All 19 tests pass. TC-46 found and fixed a real bug (documented in the Bugs section above). TC-60 found and fixed two defects in multi-bin complex order handling via the order_bins junction table (documented in the Bugs section above).

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

**Scenario:** A complex order specifies a pickup at a node that has no bins matching the payload (or all bins are already claimed). `claimComplexBins` is best-effort — it logs a warning but lets the order dispatch with `BinID=nil`. The robot travels to the node and finds nothing.

**Expected behavior:** The order dispatches (ghost robot). When the fleet reports FAILED (robot can't find a bin), the order is marked failed. No auto-return is created because `maybeCreateReturnOrder` guards on `BinID != nil`. The failure is clean but wasteful — a robot trip was wasted.

**Why this matters:** In production, bin counts at nodes can change between order submission and dispatch. If a bin is grabbed by another order or manually removed, `claimComplexBins` silently fails and a ghost robot is dispatched. This should be considered for a pre-dispatch validation check.

**Result:** PASS. Ghost robot dispatches, fleet reports FAILED, order fails cleanly with no auto-return. Documents the behavior for future hardening.

**Test:** `engine/engine_complex_test.go` — `TestComplexOrder_GhostRobotNoBin`

---

### TC-50: Concurrent complex orders same node — double claim race

**Scenario:** Two complex orders are submitted simultaneously, both picking up from the same storage node that has only one available bin. `claimComplexBins` runs for both sequentially (same goroutine in current architecture). The first order claims the bin; the second finds no unclaimed bins.

**Expected behavior:** Exactly one order claims the bin. The other dispatches with `BinID=nil` (ghost robot). No double-claim occurs because `ClaimBin` uses an atomic update.

**Why this matters:** In production, concurrent retrieve requests targeting the same NGRP are common. The claim mechanism must be race-safe. If two orders both get `BinID` pointing to the same bin, both robots arrive expecting the same bin.

**Result:** PASS. First order claims the bin. Second dispatches with `BinID=nil`. No double-claim. Strengthened: `hasBin==0` (neither order claimed) is now a hard failure, not just a log message.

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

---

## Scenarios to test next

These scenarios haven't been tested yet. Each one describes something that could go wrong on the floor and what the system should do about it.

### Bin reservation problems (highest priority)

A reservation ("claim") bug means the system's record of which bins are committed to which orders doesn't match reality. These are the most dangerous because they can cause robots to arrive at empty locations or bins to become permanently stuck.

**TC-26: Operator cancels an order — does the bin reservation release?** Largely covered by TC-23b (cancel transfers claim to return order). Remaining gap: a standalone cancel with no return order — verify the claim is released without transfer. Low priority since the cancel handler calls `UnclaimOrderBins` unconditionally.

**TC-29: Operator cancels while the robot is in transit.** The robot is already moving with the bin. The operator cancels. The reservation should release cleanly even though the robot hasn't arrived yet. Partially covered by TC-23b (cancel with robot in flight), but TC-29 would test the cancel → return → re-claim chain with the robot at RUNNING state specifically.

**TC-32: Bin sits at staging too long — what happens to the reservation?** A bin has been at a staging area past its expiry time. The system releases the staging status. But if that bin was reserved by an active order, does the reservation also get cleaned up? Or is it left dangling?

**TC-33: Operator manually moves a reserved bin.** An operator requests a manual move on a bin that is reserved by an active order. Should the system block the move? Release the reservation? Allow both and hope for the best?

**TC-34: Complex order dispatches robot to node with no bin.** Partially covered by TC-49, which documents that `claimComplexBins` is best-effort and lets ghost robots dispatch. The remaining gap: the order should fail at the planning stage with a "no bin available" error (same as `planStore` does) rather than dispatching a ghost robot. Same class of bug as TC-23c, but in the complex order path rather than the store path. Production risk: ghost robot during changeover or manual operations.

**TC-35: planMove dispatches robot with no bin.** A move order targets a lineside node with no bins, and no `payloadCode` is specified. `planMove` skips the bin-finding loop entirely (empty node, no payload filter) and dispatches with `BinID=nil`. The order should fail with a "no available bin" error, matching `planStore`'s guard. Same ghost robot class as TC-23c and TC-34. Lower likelihood since move orders typically specify a payload, but the code path exists.

**TC-38: Multi-pickup complex order leaves secondary bins stranded.** **Fixed by TC-60.** The single-robot swap test (`TestComplexOrder_SingleRobotSwap`) exposed two defects: wrong destination and orphaned claim. Both fixed via `order_bins` junction table (migration v9) with per-step bin tracking. `resolvePerBinDestinations` simulates bin flow, `handleMultiBinCompleted` moves all bins atomically, and `DeleteOrderBins` cleans up on all paths. See TC-60 bug writeup for full details.

### Timing and race conditions

**TC-6: Operator cancels at the exact moment the fleet accepts the order.** The cancel and the fleet acceptance happen at nearly the same time. Does the system end up in a clean state, or does the order get stuck — the fleet thinks it's active, the system thinks it's cancelled?

**TC-7: Operator releases before the robot has arrived at the wait point.** The Edge sends a release command for a staged order, but the robot hasn't actually reached the waiting state yet. The release should be rejected — the robot isn't there to continue.

**TC-8: Operator accidentally sends the release command twice.** Edge sends the release twice in a row. The second release should be ignored. It should not append duplicate blocks to the robot's instructions.

### Bad input handling

**TC-11: Only available bin is at a disabled storage node.** The storage node is marked as disabled (out of service). The system should not dispatch from disabled nodes — it should report no inventory available rather than sending a robot to a node that's offline.

### Multi-line and inventory scenarios

**TC-20: Two lines run the same assembly — does one line steal from the other's staging?** Line 1 and Line 2 both assemble the same product. Line 1 has a bin staged and waiting. Line 2 requests the same part. Will the system try to pull from Line 1's staging area? It shouldn't — those bins are committed to active orders on Line 1.

**TC-22: The only available bin is in the maintenance area.** Similar to quality hold, but the bin is physically at a maintenance node. The system should skip it.

**TC-31: One order finishes and frees a bin — does the next order pick it up?** Order A completes and releases its bin. Order B has been waiting because there was no inventory. Does the fulfillment scanner detect the newly available bin and dispatch Order B automatically?

**TC-39: Cross-line poaching of producer empty bins.** Producer Line A clears a bin (manifest cleared, `payload_code = ''`, `status = 'available'`). The empty bin sits at Line A's lineside node. Before Line A's operator requests a replacement empty, Line B's auto-reorder for an empty bin fires. `FindEmptyCompatibleBin` finds Line A's empty bin (no node-type filter, no ownership check). Line B's order claims it. Robot takes Line A's empty to Line B. Line A is starved for empties. Empties at lineside nodes should be invisible to cross-line `retrieve_empty` orders. `FindEmptyCompatibleBin` should exclude bins at lineside/production nodes, or producer nodes should have an affinity model for their own empties. Production risk: producer starvation. A busy floor with multiple producer lines sharing compatible bin types will see this regularly during peak periods. The zone preference mitigates it partially but does not prevent cross-zone fallback poaching.

### Fleet behavior edge cases

**TC-16: Fleet reports an unknown state.** The fleet sends a state string that the system doesn't recognize. Should map to a safe default status, not crash the event pipeline.

**TC-17: Fleet reports the same state twice.** The fleet says the robot is RUNNING, then says RUNNING again. The system should treat this as a no-op — no duplicate events, no double database updates.

**TC-18: Fleet reports states out of order.** The fleet says FINISHED before it ever said RUNNING. This can happen if a status poll is missed. The system should handle it gracefully and end up in the correct final state.

**TC-19: Robot completes a very short trip.** The fleet goes through CREATED → RUNNING → FINISHED in rapid succession (robot was right next to the destination). All three state changes should be processed correctly despite the speed.

---

## Architecture reference

This section describes how the simulation harness is built, for anyone who needs to add new tests or modify the simulator.

### The simulation harness

The engine test harness (`newTestEngine`) creates a real Shingo Engine connected to the simulator instead of RDS. When you call `sim.DriveState()` to advance a simulated robot, the following chain fires automatically:

```
sim.DriveState("RUNNING")
    → Simulator emits OrderStatusChanged event
    → Engine's EventBus delivers event to handleVendorStatusChange
    → Handler updates order status in database
    → Handler notifies Edge (writes to outbox table, goes nowhere in tests)

sim.DriveState("FINISHED")
    → Same chain → status becomes "delivered"

dispatcher.HandleOrderReceipt(...)     (simulates Edge confirmation)
    → ConfirmReceipt → EventOrderCompleted
    → handleOrderCompleted → ApplyBinArrival
    → Bin moves to destination node in database
    → Bin claim released
```

The key detail: the robot finishing (FINISHED) does not move the bin. The Edge station must confirm receipt first. This matches real production behavior and prevents inventory from updating before a human confirms the bin actually arrived.

### Concurrency testing infrastructure

The simulator harness includes infrastructure for deterministic and statistical concurrency testing:

**PostFindHook** — A test-only synchronization point installed on the `PlanningService` between `FindSourceBinFIFO` and `ClaimBin`. When set via `Dispatcher.SetPostFindHook(fn)`, the hook fires inside `planRetrieve` and `planRetrieveEmpty` after finding a bin but before claiming it. Tests use the hook to widen the TOCTOU race window and guarantee both goroutines enter it simultaneously.

```go
d.SetPostFindHook(func() {
    // This runs between Find and Claim — widen the TOCTOU window
    signalChan <- struct{}{} // let the other goroutine start
    <-waitForOther           // wait until the other goroutine claims
})
```

**simulator.ParallelGroup** — A barrier-synchronized goroutine launcher. All goroutines wait on a channel barrier, then start simultaneously. Used for stress tests where N orders compete for M bins.

```go
simulator.ParallelGroup(20, func(i int) {
    d.HandleOrderRequest(env, &protocol.OrderRequest{...})
})
```

### Important technical constraints

**Receipt required for bin movement.** Tests that verify bin movement must call `HandleOrderReceipt` after driving to FINISHED. Without this step, the bin stays at its original location in the database.

**State changes fire events automatically.** When the simulator is wired into an Engine via `newTestEngine`, calling `DriveState` automatically fires events through the engine pipeline. Tests don't need to manually emit events.

**Each test gets a fresh database.** All tests share a single Postgres container (started once per process via `sync.Once`), but each test gets its own `CREATE DATABASE`. This gives full isolation without the overhead of 90+ containers. The shared infrastructure lives in `internal/testdb/`.

### Files

```
shingo-core/
├── internal/
│   └── testdb/
│       ├── testdb.go              # Open, SetupStandardData, CreateBinAtNode, Envelope
│       └── compound.go            # CompoundScenario, CompoundConfig, SetupCompound
├── engine/
│   ├── engine_test.go             # 16 foundational tests (harness helpers removed)
│   ├── engine_concurrent_test.go  # Concurrency + general tests (~400 lines)
│   ├── engine_compound_test.go    # 8 compound reshuffle tests (~650 lines)
│   └── engine_complex_test.go     # 12 complex order tests (~600 lines)
├── dispatch/
│   ├── dispatcher_test.go         # 18 tests, helpers removed (~550 lines)
│   ├── reshuffle_test.go          # 6 tests, setup uses testdb helpers (~350 lines)
│   ├── group_resolver_test.go     # 15 tests, helpers removed (~650 lines)
│   ├── integration_test.go        # 13 tests (~950 lines, unchanged)
│   └── fleet_simulator_test.go    # 5 tests (~315 lines, unchanged)
└── fleet/
    └── simulator/
        ├── simulator.go           # Fake fleet backend, TrackingBackend impl
        ├── transitions.go         # DriveState, DriveFullLifecycle, etc.
        ├── inspector.go           # GetOrder, HasOrder, FindOrderByLocation, etc.
        ├── options.go             # Fault injection (WithCreateFailure, WithPingFailure)
        └── concurrent.go          # ParallelGroup barrier launcher
```

| File | What it does |
|------|-------------|
| `internal/testdb/testdb.go` | Shared test infrastructure. Container reuse via `sync.Once`, per-test `CREATE DATABASE`, standard data setup, bin creation helpers. |
| `internal/testdb/compound.go` | Compound scenario builder. `SetupCompound` creates full NGRP → LANE → slots → shuffle → line → bins layout from a `CompoundConfig`. |
| `fleet/simulator/simulator.go` | The fake fleet backend. Stores orders and blocks in memory. Implements TrackingBackend so the Engine can wire it up automatically. |
| `fleet/simulator/transitions.go` | State transition helpers. DriveState, DriveFullLifecycle, DriveSimpleLifecycle, DriveToFailed, DriveToStopped. |
| `fleet/simulator/inspector.go` | Read-only query methods. GetOrder, GetOrderByIndex, OrderCount, BlocksForOrder, HasOrder, FindOrderByLocation. Used by tests to inspect what the "fleet" received. |
| `fleet/simulator/options.go` | Fault injection. WithCreateFailure (fleet rejects orders), WithPingFailure (fleet health check fails). |
| `fleet/simulator/concurrent.go` | Barrier-synchronized goroutine launcher (ParallelGroup). Used for concurrent dispatch stress tests. |
| `engine/engine_test.go` | Engine-level tests (regression and scenario). TC-15, TC-2, TC-21, TC-23 cluster, TC-24 cluster, TC-30, ClaimBin. Uses real Engine + real DB + simulator. |
| `engine/engine_concurrent_test.go` | Concurrency, malformed input, redirect, fulfillment scanner, and staging expiry tests. TC-09, TC-10, TC-12, claim race, dispatch stress, redirect, fulfillment scanner, TC-37. Uses PostFindHook for deterministic TOCTOU race reproduction. |
| `engine/engine_complex_test.go` | Complex order lifecycle + production cycle pattern tests. TC-42, TC-43, TC-47, TC-48, TC-49, TC-50, TC-55, TC-56, TC-57, TC-58, TC-59, TC-60. Strengthened with SourceNode, bin position, and lifecycle assertions. |
| `engine/engine_compound_test.go` | Compound reshuffle order tests. TC-40a, TC-44, TC-45, TC-46, TC-51, TC-52, TC-53, TC-54. |
| `dispatch/dispatcher_test.go` | Dispatcher-level tests. 18 tests, helper bodies replaced with thin wrappers to `testdb`. |
| `dispatch/reshuffle_test.go` | Reshuffle planning tests. 6 tests, setup uses `testdb` helpers. |
| `dispatch/group_resolver_test.go` | Group resolver tests. 15 tests, `createTestBinAtNode` wrapper to `testdb`. |
| `dispatch/integration_test.go` | Integration tests. 13 tests, unchanged. |
| `dispatch/fleet_simulator_test.go` | Dispatcher-level scenario tests (TC-1, TC-3, TC-4, TC-5). Tests the outbound path only (what gets sent to the fleet). |

---

## Future: Edge simulation

The current simulator replaces the fleet (RDS). A future addition would simulate the Edge station as well, allowing tests to verify the full round-trip: Core dispatches → robot moves → Edge detects arrival → Edge sends receipt → Core completes order.

Today, tests simulate Edge by manually calling `HandleOrderReceipt` on the dispatcher. A dedicated Edge simulator would make this more realistic by modeling the Edge's state machine (order tracking, receipt generation, staged order release timing).

This is not yet built. The current approach (manual receipt calls) is sufficient for testing Core's behavior. Edge simulation would be valuable when testing timing-dependent scenarios like "what happens if the Edge receipt arrives before the fleet reports FINISHED."
