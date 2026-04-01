# Bin Reservation & Claiming

## Overview

Bin claiming protects bins from being dispatched by multiple orders simultaneously. The system uses atomic SQL updates (`claimed_by IS NULL` guard) to reserve bins during planning. The staging sweep (`ReleaseExpiredStagedBins`) flips bins from `staged` to `available` after TTL expiry. Bugs in this area cause double-dispatches (two robots sent to the same bin), phantom robots (dispatched with no bin), and permanently stuck inventory (claims never released).

## Test files

- `engine/engine_test.go` — claiming, staging, quality hold (TC-13, 21, 23 cluster, 25, 28, 30, 36, 37)
- `engine/engine_concurrent_test.go` — staging expiry vs active claim (TC-37)

Run this domain's tests:

```bash
cd shingo-core
go test -v -run "TestClaimBin|TestTC21|TestTC23|TestTC25|TestTC28|TestTC30|TestTC36|TestTC37" ./engine/ -timeout 60s
```

## Index

| TC | Description | Status |
|----|-------------|--------|
| TC-13 | ClaimBin silent overwrite | FIXED |
| TC-21 | Quality-hold bin not dispatched | PASS |
| TC-23a | Second store order skips claimed bin | PASS |
| TC-23b | Cancel transfers claim to return order | PASS |
| TC-23d | Changeover while move-to-QH in flight | PASS |
| TC-25 | Staged bin at core node — store order claim | DISMISSED |
| TC-28 | Two lines request same part simultaneously | PASS |
| TC-30 | Fleet failure leaves bin claim dangling | FIXED |
| TC-36 | Retrieve claim failure — queue instead of fail | FIXED |
| TC-37 | Staging expiry strips status from claimed bin | FIXED |

## Bugs found and fixed

### TC-13: ClaimBin — two orders claim the same bin — silent overwrite

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

## Verified scenarios

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

### TC-25: Staged bin at core node — store order claim (DISMISSED)

Investigated whether `planStore`/`planMove` could "poach" a staged bin at a lineside core node. Originally flagged as a gap because these planners only check `claimed_by`, not `status`.

**Dismissed:** Physical constraint — a core node holds exactly one bin. After a retrieve delivers a bin (staged, unclaimed), the only bin at that node IS the bin the operator wants to act on. Store and move orders targeting a core node as source SHOULD claim the staged bin — that's how the operator releases it (store-back to storage, quality hold move, partial release). Filtering out staged bins would break these legitimate operator workflows.

The `staged` status correctly protects against `FindSourceBinFIFO` (retrieve orders don't pull from lineside), while remaining visible to `planStore`/`planMove` (operator-initiated releases). This is working as intended.

**Test:** `engine/engine_test.go` — `TestTC25_StoreOrderClaimsStagedBinAtCoreNode` (positive assertion that store order correctly claims staged bin)

---

### TC-28: Two lines request the same part at the same time — PASS

**Scenario:** Line 1 and Line 2 both need PART-A. Two bins of PART-A sit in storage (one per storage node). Both retrieve orders fire back-to-back. Does each order get a different bin, or do they collide?

**Expected behavior:** Each order should claim a different bin. No double-assignment. Both robots should be dispatched to different storage locations.

**Result:** PASS. Order 1 claimed bin 1 at STORAGE-A1, order 2 claimed bin 2 at STORAGE-A2. The sequential dispatch path serializes correctly — the first `ClaimBin` completes before the second `FindSourceBinFIFO` runs, so the second query sees the first bin as claimed and returns the next available.

**Note:** This validates the sequential case (same goroutine). A true concurrent race (two goroutines dispatching simultaneously) could still hit a TOCTOU gap where `FindSourceBinFIFO` and `ClaimBin` target the same bin. In that case, `planRetrieve` returns `claim_failed` instead of retrying with a different bin. In production, orders arrive over the network and get serialized through the event bus, so the sequential test reflects real behavior.

**Test:** `engine/engine_test.go` — `TestTC28_ConcurrentRetrieveSamePart`
