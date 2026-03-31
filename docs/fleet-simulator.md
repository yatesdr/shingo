# Fleet Simulator

The fleet simulator is a test harness for the Shingo dispatch system. It replaces the SEER RDS fleet server with an in-memory stub, enabling end-to-end testing of the full order lifecycle — dispatch, robot movement, bin arrival, claim release — without robots, RDS, Kafka, or Edge hardware.

---

## How it works

In production, the dispatcher sends transport orders to the SEER RDS server, which controls physical robots. The simulator intercepts those calls and records what was sent. It accepts orders, stores the instruction blocks, and exposes methods to drive a simulated robot through its state transitions (`created → running → waiting → finished`).

The test database is a real Postgres instance running in Docker. Everything the dispatcher writes — orders, bin claims, status changes — goes into a real database, so tests verify actual database behavior rather than mocked responses.

```
Test code  ──▶  Dispatcher (real production code)  ──▶  Simulator (fake fleet)
     │                        │                                    │
     │                        ▼                                    │
     │                 Real Postgres DB                            │
     │                  (via Docker)                               │
     ▼                                                             ▼
Asserts database state                                    Records what the
                                                          "fleet" received
```

The dispatcher is unaware it is connected to a simulator rather than the real fleet. Test code sets up a scenario, drives the robot through state transitions, and asserts the resulting database state.

---

## Running the tests

Docker Desktop must be running. Tests spin up a temporary Postgres container automatically (image is pulled once, then cached). The container is torn down when the test process exits.

Run all simulator tests:

```
cd shingo-core
go test -v -run "TestSimulator|TestClaimBin|TestTC[0-9]|TestConcurrent|TestBuriedBin|TestFulfillmentScanner|TestRedirect|TestComplexOrder|TestCompound|TestLaneLock" ./engine/ ./dispatch/ -timeout 120s
```

Run a specific test:

```
go test -v -run TestSimulator_FullLifecycle ./engine/ -timeout 60s
```

If Docker is not running, database-dependent tests are skipped automatically — they will not fail the build. Pure logic tests (e.g., state mapping) always run.

---

## How to read this document

Test results are organized into two sections:

**Bugs found and fixed** — regression tests documenting real bugs discovered by the simulator. Each entry explains the failure scenario, root cause, and fix. These tests ensure the bug is not reintroduced. A failing regression test is high priority.

**Verified scenarios** — scenario tests that explore edge cases and failure modes the system should handle. These tests passed, confirming correct behavior today. They guard against future regressions.

Each test case follows this format:

- **Scenario** — the situation in plain language, with enough context to understand the risk without reading code.
- **Expected behavior** — what the system should do.
- **Result** — pass/fail, and for bugs, what went wrong and how it was fixed.
- **Root cause** (bugs only) — why the bug existed.
- **Production risk** (bugs only) — who this affects on the floor and under what conditions.
- **Code snippets** (optional) — key lines from the test or fix.
- **Test** — file name and function for locating and running the test.

The **Scenarios to test next** section is a prioritized backlog of untested situations, written in the same format so they can be turned into tests directly.

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
| TC-49 | Ghost robot — claimComplexBins finds no bin at pickup | FIXED | Bug found + Verified |
| TC-50 | Concurrent complex orders same node — double claim race | FIXED | Bug found + Verified |
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
| TC-61 | Queued order fulfilled after changeover starts — wrong payload | FIXED | Bug found (Edge) |
| — | ClaimBin silent overwrite | FIXED | Bug found |
| — | Deterministic TOCTOU claim race (PostFindHook) | PASS | Verified |
| — | Dispatch stress — 20 concurrent orders, 10 bins | PASS | Verified |
| — | Redirect mid-transit — claim intact | PASS | Verified |
| — | Fulfillment scanner — queue to dispatch round-trip | PASS | Verified |
| TC-62a | ClearForReuse nulls manifest and makes bin visible to FindEmpty | PASS | Verified |
| TC-62b | SyncUOP preserves manifest and payload while updating count | PASS | Verified |
| TC-62c | ClearAndClaim atomically clears manifest + claims in one UPDATE | PASS | Verified |
| TC-62d | ClearAndClaim rejects already-claimed bin | PASS | Verified |
| TC-62e | ClearAndClaim rejects locked bin | PASS | Verified |
| TC-62f | SyncUOPAndClaim updates count + claims atomically | PASS | Verified |
| TC-62g | SetForProduction sets manifest, payload, UOP | PASS | Verified |
| TC-62h | Confirm marks manifest as confirmed | PASS | Verified |
| TC-63a | ClaimForDispatch nil → plain claim (no manifest change) | PASS | Verified |
| TC-63b | ClaimForDispatch zero → ClearAndClaim (full depletion) | PASS | Verified |
| TC-63c | ClaimForDispatch positive → SyncUOPAndClaim (partial) | PASS | Verified |
| TC-64a | Full depletion (remainingUOP=0) clears manifest on dispatch | PASS | Verified |
| TC-64b | Partial consumption (remainingUOP=42) syncs UOP, preserves manifest | PASS | Verified |
| TC-64c | Concurrent retrieve_empty cannot steal bin during clear+claim | PASS | Verified |
| TC-64d | Concurrent ClaimForDispatch race — one ClearAndClaim, one SyncUOPAndClaim, exactly one wins | PASS | Verified |
| TC-65 | extractRemainingUOP: nil envelope, empty payload, missing field, zero, positive, malformed | PASS | Verified |
| TC-66a | Produce simple — FinalizeProduceNode creates ingest order, resets UOP to 0 | PASS | Verified (Edge) |
| TC-66b | Produce sequential — ingest + complex removal order created | PASS | Verified (Edge) |
| TC-66c | Produce single_robot — 10-step complex swap order created | PASS | Verified (Edge) |
| TC-66d | Produce two_robot — two coordinated complex orders, both tracked in runtime | PASS | Verified (Edge) |
| TC-66e | Produce finalize rejects zero UOP (nothing to finalize) | PASS | Verified (Edge) |
| TC-66f | Produce finalize rejects consume-role node | PASS | Verified (Edge) |
| TC-67a | Ingest completion resets produce UOP to 0 and clears order tracking | PASS | Verified (Edge) |
| TC-67b | Retrieve completion resets produce UOP to 0 (empty bin received) | PASS | Verified (Edge) |
| TC-67c | Retrieve completion resets consume UOP to capacity (full bin received) | PASS | Verified (Edge) |
| TC-67d | Counter delta increments produce UOP (counting UP) | PASS | Verified (Edge) |
| TC-67e | Counter delta decrements consume UOP (counting DOWN) | PASS | Verified (Edge) |
| TC-67f | Counter delta floors consume UOP at zero (never negative) | PASS | Verified (Edge) |
| TC-67g | Bin loader move completion resets runtime state | PASS | Verified (Edge) |
| TC-68 | Post-delivery cancel: no return order, spurious return order (Core) | FIXED | Bug found (Core) |
| TC-69 | Node list sync excludes NGRP node groups (Core) | FIXED | Bug found (Core) |
| TC-70 | Payload catalog sync prunes deleted entries (Edge) | FIXED | Bug found (Edge) |

---

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

### Changeover empty wiring: complex order not recognized as line clear

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

---

### TC-70: Payload catalog sync does not prune deleted entries

**Scenario:** Core sends a payload catalog sync to Edge. Edge upserts the received entries into its local SQLite database. Later, an admin deletes a payload from Core's catalog. On the next sync, Core sends the updated catalog (without the deleted entry). Edge upserts the entries it receives but never removes entries that Core no longer includes. The deleted payload remains in Edge's local catalog forever.

**Expected behavior:** After a catalog sync, Edge's local catalog should exactly match Core's catalog. Entries that were deleted from Core should be removed from Edge's local database.

**Result:** BUG FOUND. `HandlePayloadCatalog` in `shingo-edge/engine/engine.go` only called `UpsertPayloadCatalog` for each received entry. It never removed entries that were no longer in Core's response. Stale deleted payloads accumulated in Edge's local catalog indefinitely.

**Root cause:** Missing prune step after upsert. The handler collected entries from Core and upserted them, but had no mechanism to detect and remove entries that Core had deleted since the last sync.

```go
// Before (broken): only upserts, stale entries persist forever
func (e *Engine) HandlePayloadCatalog(entries []protocol.CatalogPayloadInfo) {
    for _, b := range entries {
        entry := &store.PayloadCatalogEntry{...}
        e.db.UpsertPayloadCatalog(entry)
    }
    // no prune step -- deleted entries stay in local DB
}

// After (fixed): upsert then prune stale entries
func (e *Engine) HandlePayloadCatalog(entries []protocol.CatalogPayloadInfo) {
    ids := make([]int64, 0, len(entries))
    for _, b := range entries {
        entry := &store.PayloadCatalogEntry{...}
        e.db.UpsertPayloadCatalog(entry)
        ids = append(ids, b.ID)
    }
    // prune entries not in core's active set
    if err := e.db.DeleteStalePayloadCatalogEntries(ids); err != nil {
        log.Printf("engine: prune stale payload catalog: %v", err)
    }
}
```

**New method:** `DeleteStalePayloadCatalogEntries(activeIDs []int64)` in `store/payload_catalog.go` deletes entries whose IDs are not in the active set. Safety guard: if `activeIDs` is empty, no entries are removed (prevents accidental full wipe on empty sync).

```sql
DELETE FROM payload_catalog WHERE id NOT IN (<activeIDs>)
```

**Production risk:** Low. Stale entries remain visible in Edge's payload dropdown after deletion from Core. An operator could attempt to use a deleted payload code, but the order would fail at Core (payload doesn't exist). The real risk is operator confusion: seeing payloads in the dropdown that no longer exist in Core. Over time, the dropdown accumulates outdated entries that operators must mentally filter.

**Status:** Fixed. `HandlePayloadCatalog` now collects active IDs during upsert and calls `DeleteStalePayloadCatalogEntries` afterward. Entries deleted from Core are pruned from Edge's local catalog on the next sync.

**Test:** `shingo-edge/engine/wiring_test.go` -- `TestHandlePayloadCatalog_PruneDeletedEntries`

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

### SSE order list table refresh on order-update — PASS (Core UI)

**Scenario:** The Core orders page uses Server-Sent Events (SSE) for real-time updates. When an order's status changes (e.g., dispatched -> in_transit -> delivered -> confirmed), the server pushes an `order-update` event. The order list table is server-rendered HTML with no dynamic list-loading mechanism. Without a refresh mechanism, the table shows stale data until the operator manually reloads the page.

**Expected behavior:** When an SSE `order-update` event fires, the page should refresh to show the latest order statuses in the table.

**Result:** PASS. The `orders.js` `onOrderUpdate` handler (debounced at 2 seconds) already calls `location.reload()` on every SSE event. This is the simplest correct approach: the order list is server-rendered HTML, so a full page reload is the appropriate refresh mechanism. The handler also refreshes the order detail modal if the updated order is currently open.

**Why this works:** The server-rendered HTML table has no dynamic list loading function. Adding one would require significant refactoring (fetching HTML partials, diffing DOM, managing pagination state). `location.reload()` is the correct solution for server-rendered content. The 2-second debounce prevents rapid reloads during batch status changes.

**Note:** This was investigated as Bug 2 after observing that SSE events might not refresh the list. The investigation confirmed the existing implementation is correct -- `location.reload()` fires on every debounced `order-update` event.

**Test:** Manual verification via Core UI. `shingo-core/www/static/pages/orders.js` -- `onOrderUpdate` handler (line 249).

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

---

## Scenarios to test next

These scenarios haven't been tested yet. Each one describes something that could go wrong on the floor and what the system should do about it.

### Bin reservation problems (highest priority)

A reservation ("claim") bug means the system's record of which bins are committed to which orders doesn't match reality. These are the most dangerous because they can cause robots to arrive at empty locations or bins to become permanently stuck.

**TC-26: Operator cancels an order — does the bin reservation release?** Largely covered by TC-23b (cancel transfers claim to return order). Remaining gap: a standalone cancel with no return order — verify the claim is released without transfer. Low priority since the cancel handler calls `UnclaimOrderBins` unconditionally.

**TC-29: Operator cancels while the robot is in transit.** The robot is already moving with the bin. The operator cancels. The reservation should release cleanly even though the robot hasn't arrived yet. Partially covered by TC-23b (cancel with robot in flight), but TC-29 would test the cancel → return → re-claim chain with the robot at RUNNING state specifically.

**TC-32: Bin sits at staging too long — what happens to the reservation?** A bin has been at a staging area past its expiry time. The system releases the staging status. But if that bin was reserved by an active order, does the reservation also get cleaned up? Or is it left dangling?

**TC-33: Operator manually moves a reserved bin.** An operator requests a manual move on a bin that is reserved by an active order. Should the system block the move? Release the reservation? Allow both and hope for the best?

**TC-34: Complex order dispatches robot to node with no bin.** **Fixed by TC-49.** `claimComplexBins` now returns a `planningError` when zero bins are claimed, failing the order before fleet dispatch. No ghost robot dispatched.

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

### Core/Edge sync and cross-component issues

**TC-68: Post-delivery cancel creates spurious return order.** **Fixed.** Three interrelated bugs: `TerminateOrder` had no status guard (accepted delivered orders), `maybeCreateReturnOrder` created phantom return orders on cancellation, and `ConfirmReceipt` overwrote cancelled status back to confirmed. All three fixed with status guards. See TC-68 in Bugs Found and Fixed for full details.

**TC-69: Node list sync excludes NGRP node groups.** **Fixed.** `ListNodesForStation` SQL now includes NGRP parents whose children are station-assigned. `handleNodeListRequest` builds dot-notation names for both station-scoped and global paths. Edge now receives node group containers in the Sync Nodes dropdown. See TC-69 in Bugs Found and Fixed.

**TC-70: Payload catalog sync does not prune deleted entries.** **Fixed.** `HandlePayloadCatalog` now calls `DeleteStalePayloadCatalogEntries` after upsert, removing entries that Core no longer includes. Stale deleted payloads no longer accumulate in Edge's local catalog. See TC-70 in Bugs Found and Fixed.

**SSE order list refresh on order-update.** **Verified.** Core's `orders.js` already calls `location.reload()` on debounced SSE `order-update` events. The server-rendered HTML table refreshes correctly. No code change needed. See SSE entry in Verified scenarios.

---

## Verified scenarios — bin lifecycle and produce nodes

These tests cover the BinManifestService, the remainingUOP protocol extension, and the produce node swap mode choreography. They verify the fixes for ghost bins (bins never cleared after consumption), partial UOP sync, and the new produce node automation.

Core tests use PostgreSQL 16 via testcontainers (same as the fleet simulator tests). Edge tests use SQLite in a temp directory — no Docker required.

---

### BinManifestService — centralized manifest mutations (TC-62)

**Scenario:** All bin manifest mutations (clear, sync UOP, set for production, confirm) now flow through a single `BinManifestService` instead of scattered `db.SetBinManifest` / `db.ClearBinManifest` calls. The service also provides atomic `ClearAndClaim` and `SyncUOPAndClaim` operations that close the TOCTOU race window between clearing a bin's manifest and claiming it for dispatch.

**Expected behavior:** Each operation mutates exactly the fields it should and nothing else. Atomic operations succeed or fail as a unit — no partial state. Claims are rejected if the bin is already claimed or locked.

**Result:** PASS. 12 unit tests in `service/bin_manifest_test.go`.

**Test:** `shingocore/service/bin_manifest_test.go` — `TestBinManifestService_*`

---

### ClaimForDispatch routing — remainingUOP protocol (TC-63, TC-64, TC-65)

**Scenario:** Edge sends `remaining_uop` on move orders to tell Core the bin's consumption state. Three cases: nil (legacy, no sync), zero (fully depleted — clear manifest), positive (partial consumption — sync UOP count). The dispatcher extracts this from the envelope and routes through `ClaimForDispatch`.

**Expected behavior:** nil → plain `ClaimBin` (no manifest change). Zero → `ClearAndClaim` (atomic clear + claim, bin becomes visible to `FindEmptyCompatibleBin`). Positive → `SyncUOPAndClaim` (UOP updated, manifest preserved, bin claimed).

**Result:** PASS. 4 integration tests in `dispatch/bin_lifecycle_test.go`, 7 unit tests for `extractRemainingUOP` in `dispatch/planning_test.go`.

**Bug found and fixed:** `DecrementBinUOP` was dead code — never called by any path. Removed. The new `SyncUOPAndClaim` replaces it with an atomic operation.

**Bug found and fixed:** `tryAutoRequestEmpty` was incorrectly called on produce node ingest completion. This function is bin_loader-only — it calls `RequestEmptyBin` which gates on `role == "bin_loader"`. Removed the call; produce nodes don't auto-request after ingest because simple mode still has the filled bin at the node, and swap modes already have complex orders in flight.

**Test:** `shingocore/dispatch/bin_lifecycle_test.go` — `TestFullDepletion_ClearsManifest`, `TestPartialConsumption_SyncsUOP`, `TestConcurrentRetrieveEmpty_GhostBin`; `shingocore/dispatch/planning_test.go` — `TestExtractRemainingUOP_*`

---

### Produce swap mode finalization (TC-66)

**Scenario:** Produce nodes fill empty bins. When the operator finalizes a bin (locks the UOP count), the system must manifest the bin at Core via an ingest order, then dispatch the appropriate swap choreography based on the claim's swap mode.

**Expected behavior:** All four swap modes create an ingest order first, then:

- **Simple** — bare ingest, no swap. Runtime UOP resets to 0.
- **Sequential** — ingest + complex removal order. Backfill auto-created by wiring when removal goes in_transit (same as consume).
- **Single_robot** — ingest + 10-step all-in-one complex swap order.
- **Two_robot** — ingest + two coordinated complex orders (OrderA for fetch-and-stage, OrderB for remove-filled). Both tracked in runtime.

Finalization is rejected if UOP is zero (nothing to finalize) or the node's role is not `produce`.

**Result:** PASS. 7 tests in `engine/produce_swap_test.go`.

**Test:** `shingoedge/engine/produce_swap_test.go` — `TestProduceSimple_FinalizeIngest`, `TestProduceSequential_RemovalThenBackfill`, `TestProduceSingleRobot_TenStepSwap`, `TestProduceTwoRobot_BothOrdersCreated`, `TestProduceFinalize_RejectsZeroUOP`, `TestProduceFinalize_RejectsConsumeNode`

---

### Edge wiring — event-driven state transitions (TC-67)

**Scenario:** The Edge engine's event handlers manage UOP tracking and order lifecycle state for process nodes. Different order types and node roles trigger different reset behavior.

**Expected behavior:**

- Ingest completion (produce): UOP → 0, order IDs cleared. No auto-request (bin still at node in simple mode; swap orders already in flight for other modes).
- Retrieve/complex completion (produce): UOP → 0 (empty bin received, starts counting from zero).
- Retrieve/complex completion (consume): UOP → capacity (full bin received).
- Counter delta (produce): UOP increments (counting UP toward capacity).
- Counter delta (consume): UOP decrements (counting DOWN from capacity), floored at zero.
- Bin loader move completion: UOP → 0, order tracking cleared, auto-request for next empty.

**Bug found and fixed:** Produce UOP reset was using `claim.UOPCapacity` for all roles. Produce nodes receiving an empty bin should reset to 0, not capacity. Fixed with `if claim.Role == "produce" { resetUOP = 0 }`.

**Result:** PASS. 7 tests in `engine/wiring_test.go`.

**Test:** `shingoedge/engine/wiring_test.go` — `TestWiring_*`

---

### TC-70: Payload catalog sync prunes deleted entries (Edge) — PASS

**Scenario:** Edge receives a payload catalog sync from Core. Core's response includes entries A and B. Edge upserts both. Later, Core deletes entry B. On the next sync, Core sends only entry A. Edge should upsert A and prune B from its local catalog.

**Expected behavior:** After sync, Edge's local catalog contains exactly the entries Core sent. Entries deleted from Core are removed from Edge's local database.

**Result:** PASS. `HandlePayloadCatalog` in `shingo-edge/engine/engine.go` collects active IDs during upsert and calls `DeleteStalePayloadCatalogEntries` after all entries are processed. The `DeleteStalePayloadCatalogEntries` method deletes entries whose IDs are not in the active set, with a safety guard that skips pruning if the active set is empty (prevents accidental full wipe).

**Bug found and fixed:** Before the fix, `HandlePayloadCatalog` only upserted entries without pruning. Stale deleted payloads accumulated in Edge's local catalog indefinitely. See TC-70 in Bugs Found and Fixed for full details.

**Test:** `shingo-edge/engine/wiring_test.go` — `TestHandlePayloadCatalog_PruneDeletedEntries`

---

### TC-69: Node list sync includes NGRP node groups (Core) — PASS

**Scenario:** Edge sends a node list request to Core for station `STATION-1`. The station has individual nodes and NGRP (node group) parents whose children are assigned to the station. Core's `ListNodesForStation` should return both individual nodes and NGRP group containers, so Edge can display them in the node dropdown with `(group)` suffix support.

**Expected behavior:** The node list response includes NGRP parent nodes (e.g., `STORAGE-G1` with type `NGRP`) alongside their children with dot notation (e.g., `STORAGE-G1.SLOT-1`). Both the station-scoped path (common) and the global fallback path return NGRP nodes.

**Result:** PASS. `ListNodesForStation` SQL now includes an `OR` clause that adds NGRP parents whose children are station-assigned. `handleNodeListRequest` builds the response with dot notation for children in both paths. Two tests verify: station-scoped path (`TestNodeListResponse_IncludesNodeGroups`) and global fallback path (`TestNodeListResponse_GlobalPath_IncludesNodeGroups`).

**Bug found and fixed:** Before the fix, `ListNodesForStation` excluded NGRP parent containers from station-scoped results. Edge never received node group information, so NGRP nodes were missing from the dropdown. See TC-69 in Bugs Found and Fixed for full details.

**Test:** `shingo-core/messaging/core_data_service_test.go` — `TestNodeListResponse_IncludesNodeGroups`, `TestNodeListResponse_GlobalPath_IncludesNodeGroups`

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
│       ├── compound.go            # CompoundScenario, CompoundConfig, SetupCompound
│       └── mock_backend.go        # MockBackend (NewFailingBackend/NewSuccessBackend), MockTrackingBackend
├── engine/
│   ├── engine_test.go             # 16 foundational tests (harness helpers removed)
│   ├── engine_concurrent_test.go  # Concurrency + general tests (~400 lines)
│   ├── engine_compound_test.go    # 8 compound reshuffle tests (uses SetupCompound for setup)
│   └── engine_complex_test.go     # 12 complex order tests (~600 lines)
├── dispatch/
│   ├── dispatcher_test.go         # 18 tests, uses testdb.NewFailingBackend
│   ├── reshuffle_test.go          # 6 tests, uses testdb.NewSuccessBackend/NewFailingBackend
│   ├── group_resolver_test.go     # 15 tests, helpers removed (~650 lines)
│   ├── integration_test.go        # 13 tests, uses testdb.NewTrackingBackend/CreateBinAtNode
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
| `internal/testdb/mock_backend.go` | Shared mock fleet backends. `NewFailingBackend()` (all ops error), `NewSuccessBackend()` (records orders), `NewTrackingBackend()` (satisfies `fleet.TrackingBackend`). Eliminates duplicate mock implementations across test files. |
| `internal/testdb/compound.go` | Compound scenario builder. `SetupCompound` creates full NGRP → LANE → slots → shuffle → line → bins layout from a `CompoundConfig`. |
| `fleet/simulator/simulator.go` | The fake fleet backend. Stores orders and blocks in memory. Implements TrackingBackend so the Engine can wire it up automatically. |
| `fleet/simulator/transitions.go` | State transition helpers. DriveState, DriveFullLifecycle, DriveSimpleLifecycle, DriveToFailed, DriveToStopped. |
| `fleet/simulator/inspector.go` | Read-only query methods. GetOrder, GetOrderByIndex, OrderCount, BlocksForOrder, HasOrder, FindOrderByLocation. Used by tests to inspect what the "fleet" received. |
| `fleet/simulator/options.go` | Fault injection. WithCreateFailure (fleet rejects orders), WithPingFailure (fleet health check fails). |
| `fleet/simulator/concurrent.go` | Barrier-synchronized goroutine launcher (ParallelGroup). Used for concurrent dispatch stress tests. |
| `engine/engine_test.go` | Engine-level tests (regression and scenario). TC-15, TC-2, TC-21, TC-23 cluster, TC-24 cluster, TC-30, ClaimBin. Uses real Engine + real DB + simulator. |
| `engine/engine_concurrent_test.go` | Concurrency, malformed input, redirect, fulfillment scanner, and staging expiry tests. TC-09, TC-10, TC-12, claim race, dispatch stress, redirect, fulfillment scanner, TC-37. Uses PostFindHook for deterministic TOCTOU race reproduction. |
| `engine/engine_complex_test.go` | Complex order lifecycle + production cycle pattern tests. TC-42, TC-43, TC-47, TC-48, TC-49, TC-50, TC-55, TC-56, TC-57, TC-58, TC-59, TC-60. Strengthened with SourceNode, bin position, and lifecycle assertions. |
| `engine/engine_compound_test.go` | Compound reshuffle order tests. TC-40a, TC-44, TC-45, TC-46, TC-51, TC-52, TC-53, TC-54. All 8 tests use `testdb.SetupCompound()` for setup, reducing ~400 lines of boilerplate. |
| `dispatch/dispatcher_test.go` | Dispatcher-level tests. 18 tests, uses shared `testdb.NewFailingBackend()` instead of inline mock. |
| `dispatch/reshuffle_test.go` | Reshuffle planning tests. 6 tests, uses `testdb.NewSuccessBackend()` and `testdb.NewFailingBackend()`. |
| `dispatch/group_resolver_test.go` | Group resolver tests. 15 tests, `createTestBinAtNode` wrapper to `testdb`. |
| `dispatch/integration_test.go` | Integration tests. 13 tests, uses `testdb.NewTrackingBackend()` and `testdb.CreateBinAtNode()` for bin setup. |
| `dispatch/fleet_simulator_test.go` | Dispatcher-level scenario tests (TC-1, TC-3, TC-4, TC-5). Tests the outbound path only (what gets sent to the fleet). |
| `dispatch/bin_lifecycle_test.go` | Bin lifecycle integration tests (TC-64a–c). Full depletion, partial consumption, concurrent retrieve_empty race. |
| `dispatch/planning_test.go` | `extractRemainingUOP` unit tests (TC-65). Nil, empty, missing, zero, positive, malformed JSON. |
| `service/bin_manifest_test.go` | BinManifestService unit tests (TC-62a–h). All manifest mutations, atomic clear+claim, lock rejection. |
| `messaging/core_data_service_test.go` | Core data service tests (TC-69). Node list response includes NGRP node groups for station-scoped and global paths. Uses testcontainers Postgres. |

Edge test files (in `shingo-edge/`, SQLite-based — no Docker required):

| File | What it does |
|------|-------------|
| `engine/produce_swap_test.go` | Produce swap mode tests (TC-66a–f). All four swap modes, rejection guards. Includes `seedProduceNode` and `testEngine` helpers. |
| `engine/wiring_test.go` | Edge wiring event handler tests (TC-67a–g, TC-70). Ingest/retrieve completion, counter delta, UOP reset, bin_loader move completion, payload catalog prune. |
| `engine/changeover_test.go` | Changeover automation tests (TC-70a–g). Wiring lifecycle, auto-staging, order failure/retry, cancel mid-staging. Includes `seedChangeoverScenario` and `startChangeover` helpers. |

---

## Verified scenarios — changeover automation

These tests cover the changeover automation pipeline: event-driven wiring that advances node task states on order completion, auto-staging at changeover start, and cancel/error handling. Edge tests use SQLite in a temp directory — no Docker required.

Run all changeover tests:

```
cd shingo-edge
go test -v -run "TestChangeover" ./engine/ -timeout 60s
```

---

### TC-70a: Staging order completion advances node task — PASS

**Scenario:** A changeover is started and a position is staged (material ordered to inbound staging). The staging order completes. The wiring should advance the node task from `staging_requested` to `staged`.

**Expected behavior:** `handleNodeOrderCompleted` matches the completed order against the node task's `NextMaterialOrderID`, recognizes the delivery node matches the to-claim's `InboundStaging`, and updates the state to `staged`.

**Result:** PASS. The node task advances to `staged` on staging order completion.

**Test:** `engine/changeover_test.go` — `TestChangeover_StagingCompletion`

---

### TC-70b: Empty order completion advances node task — PASS

**Scenario:** During changeover, a position has been staged and the operator empties it (removes old material). The empty order completes. The wiring should advance the node task from `empty_requested` to `line_cleared`.

**Expected behavior:** `handleNodeOrderCompleted` matches the completed order against the node task's `OldMaterialReleaseOrderID` and advances state to `line_cleared`.

**Result:** PASS (after fixing the `TypeMove`-only check — see "Changeover empty wiring" in Bugs Found). The wiring now accepts both `TypeMove` and `TypeComplex` for line-clear matching.

**Test:** `engine/changeover_test.go` — `TestChangeover_EmptyCompletion`

---

### TC-70c: Release order completion advances node task — PASS

**Scenario:** During changeover, a position has been cleared and the staged material is released into production. The release order completes. The wiring should advance the node task to `released`.

**Expected behavior:** `handleNodeOrderCompleted` matches the completed order against the node task's `NextMaterialOrderID` (now used for release), updates runtime to the target claim, and advances state to `released`.

**Result:** PASS. The node task advances to `released` and `tryCompleteProcessChangeover` fires.

**Test:** `engine/changeover_test.go` — `TestChangeover_ReleaseCompletion`

---

### TC-70d: Full changeover lifecycle — PASS

**Scenario:** A complete changeover from start to finish: auto-stage (Phase 2), drive staging order to completion, empty, drive empty order to completion, release, drive release order to completion, switch node to target, cutover production. This tests the entire happy path.

**Expected behavior:** After all steps, the changeover should be completed, the process should be at `active_production`, and the active style should be the target style.

**Result:** PASS. The full lifecycle completes cleanly. Auto-staging fires at changeover start, all wiring transitions work, `CompleteProcessProductionCutover` sets the active style and completes the changeover.

**Test:** `engine/changeover_test.go` — `TestChangeover_FullLifecycle`

---

### TC-70e: Order failure marks error, retry recovers — PASS

**Scenario:** During changeover, a staging order fails (bin unavailable, Core error, etc.). The wiring should mark the node task as `error`. The operator retries staging. The retry should succeed because the failed order is terminal.

**Expected behavior:** On failure: node task state becomes `error`. On retry: a new staging order is created (the old one is terminal so `ensureNodeTaskCanRequestOrder` allows it), and completing the new order recovers the state to `staged`.

**Result:** PASS. Error state is set on failure. Retry creates a new order and completes normally.

**Test:** `engine/changeover_test.go` — `TestChangeover_OrderFailure`

---

### TC-70f: Cancel mid-staging aborts orders — PASS

**Scenario:** A changeover is started and auto-staging creates an in-flight order. The operator cancels the changeover before staging completes. All in-flight orders should be aborted, all node tasks should be cancelled, and the process should revert to `active_production`.

**Expected behavior:** `CancelProcessChangeover` aborts linked orders on all node tasks, marks tasks as `cancelled`, and resets production state.

**Result:** PASS. The staging order is aborted (terminal status), node tasks are cancelled, and the changeover record is marked `cancelled`. Production state reverts to `active_production`.

**Test:** `engine/changeover_test.go` — `TestChangeover_CancelMidStaging`

---

### TC-70g: Auto-staging fires on changeover start (Phase 2) — PASS

**Scenario:** A changeover is started on a process with a swap position (different payload between from-style and to-style). In Phase 2, `StartProcessChangeover` should automatically call `StageNodeChangeoverMaterial` for all swap/add positions instead of requiring the operator to click "Stage" per position.

**Expected behavior:** After `StartProcessChangeover` returns, the node task should already be at `staging_requested` (not `swap_required`) with a `NextMaterialOrderID` linked.

**Result:** PASS. The auto-staging loop iterates diffs, filters for swap/add situations, resolves process node IDs, and calls existing `StageNodeChangeoverMaterial`. Failures are logged, not blocking — operator can retry manually.

**Test:** `engine/changeover_test.go` — `TestChangeover_AutoStaging`

---

## Future: Edge simulation

The curre