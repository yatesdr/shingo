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
go test -v -run "TestSimulator|TestClaimBin|TestTC[2-4][0-9]" ./engine/ ./dispatch/ -timeout 120s
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

**Written but not yet run** are tests that have been coded but haven't been executed yet. They explore specific scenarios and may find new bugs when run.

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
| TC-9 | Complex order with zero steps | — | To test |
| TC-10 | Order references nonexistent node | — | To test |
| TC-11 | Only bin at disabled storage node | — | To test |
| TC-12 | Order requests zero quantity | — | To test |
| TC-15 | Full lifecycle dispatch → receipt → bin arrival | PASS | Verified |
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
| TC-25 | Staged bin at core node — store order claim | DISMISSED | Investigated, correct behavior |
| TC-28 | Two lines request same part simultaneously | PASS | Verified |
| TC-29 | Cancel while robot in transit | — | To test |
| TC-30 | Fleet failure leaves bin claim dangling | FIXED | Bug found |
| TC-31 | Order finishes, freed bin picked up by waiting order | — | To test |
| TC-32 | Staging expiry vs active reservation | — | To test |
| TC-33 | Manual move of reserved bin | — | To test |
| TC-34 | Complex order dispatches to node with no bin | — | To test |
| TC-35 | planMove dispatches robot with no bin | — | To test |
| TC-36 | Retrieve claim failure leaves order stranded | — | To test |
| TC-37 | Staging expiry strips status from claimed bin | — | To test |
| TC-38 | Multi-pickup complex order leaves secondary bins stranded | — | To test |
| TC-39 | Cross-line poaching of producer empty bins | — | To test |
| TC-40a | FIFO mode — buried older than accessible triggers reshuffle (Cube #7) | — | To test |
| TC-40b | COST mode — oldest accessible returned, buried ignored (Cube #7) | — | To test |
| TC-41 | Empty cart starvation — no accessible empties (Cube #6) | — | To test |
| — | ClaimBin silent overwrite | FIXED | Bug found |

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

## Written but not yet run

These tests have been coded and are ready to execute. They may find new bugs when run.

---

## Scenarios to test next

These scenarios haven't been tested yet. Each one describes something that could go wrong on the floor and what the system should do about it.

### Bin reservation problems (highest priority)

A reservation ("claim") bug means the system's record of which bins are committed to which orders doesn't match reality. These are the most dangerous because they can cause robots to arrive at empty locations or bins to become permanently stuck.

**Robot breaks down mid-delivery — does the bin reservation release?** A robot is carrying a bin and breaks down. The fleet marks the order as failed. The bin claim should be automatically released so the system knows that bin is available for a new attempt. If it doesn't release, the system thinks a robot is still coming for it and no one else can use it.

**TC-26: Operator cancels an order — does the bin reservation release?** Same as above, but the operator cancels instead of the robot failing. The bin was reserved but the order is no longer happening. The reservation must release.

**TC-27: Operator redirects a robot — does the bin reservation survive?** A robot is mid-delivery and the operator redirects it to a different destination. The bin is still on the robot — the reservation should stay intact. Only the destination changes.

**TC-29: Operator cancels while the robot is in transit.** The robot is already moving with the bin. The operator cancels. The reservation should release cleanly even though the robot hasn't arrived yet.

**TC-32: Bin sits at staging too long — what happens to the reservation?** A bin has been at a staging area past its expiry time. The system releases the staging status. But if that bin was reserved by an active order, does the reservation also get cleaned up? Or is it left dangling?

**TC-37: Staging expiry strips status from actively-claimed bin.** A bin is delivered to a lineside node (status `staged`, `claimed_by` set on the delivery receipt order). The node has a staging TTL of 5 minutes. 5 minutes pass. `ReleaseExpiredStagedBins` flips status to `available` without checking `claimed_by`. The bin is now `available` but still claimed — a contradictory state. Staging expiry should either skip bins with active claims, or the claim check should prevent downstream confusion. The bin should remain `staged` while actively claimed. Production risk: UI display confusion — `NodeTileState` would show `Staged: false, Claimed: true`. Not a double-dispatch risk (claim still protects), but could mislead operators or monitoring dashboards.

**TC-33: Operator manually moves a reserved bin.** An operator requests a manual move on a bin that is reserved by an active order. Should the system block the move? Release the reservation? Allow both and hope for the best?

**TC-34: Complex order dispatches robot to node with no bin.** A complex order (sequential removal) targets a lineside node where the bin was already moved by a prior manual move order. `claimComplexBins` finds no unclaimed bin, but the order still dispatches to the fleet. Robot arrives at an empty node. The order should fail at the planning stage with a "no bin available" error, same as `planStore` does. No robot should be dispatched. Same class of bug as TC-23c, but in the complex order path rather than the store path. Production risk: ghost robot during changeover or manual operations.

**TC-35: planMove dispatches robot with no bin.** A move order targets a lineside node with no bins, and no `payloadCode` is specified. `planMove` skips the bin-finding loop entirely (empty node, no payload filter) and dispatches with `BinID=nil`. The order should fail with a "no available bin" error, matching `planStore`'s guard. Same ghost robot class as TC-23c and TC-34. Lower likelihood since move orders typically specify a payload, but the code path exists.

**TC-36: Retrieve claim failure leaves order stranded in sourcing.** Two orders target the same bin. Order A claims it. Order B's `ClaimBin` fails with `claim_failed`. The order stays in `sourcing` status indefinitely — no event triggers the fulfillment scanner to retry. The order should be queued (status `queued`) so the fulfillment scanner retries when a bin becomes available. Alternatively, `claim_failed` should trigger a re-attempt with the next available bin. Production risk: order stuck in sourcing forever, invisible to operators until they notice a bin never arrives, requires manual database intervention to clear.

**TC-38: Multi-pickup complex order leaves secondary bins stranded.** A complex order has two pickup steps at different nodes. Both bins are claimed via `claimComplexBins`. Order completes. `ApplyBinArrival` moves the first bin (tracked by `Order.BinID`). The second bin stays claimed at its original node — never moved, never unclaimed. All claimed bins should be moved and unclaimed on order completion. A junction table (`order_bins`) would be needed to track multiple bin associations. Production risk: uncommon today (multi-pickup orders are rare), but the stranded bin becomes invisible to the system. No other order can claim it. It's permanently reserved by a completed order until manual database intervention.

### NGRP lane behavior (from Shingo Cube observations)

Scenarios first observed in the Shingo Cube simulation.

**TC-40a: FIFO mode — buried bin older than accessible triggers reshuffle.** A 3-lane NGRP has bins with staggered `loaded_at` timestamps. The oldest bin is buried at depth 2 in Lane 3 behind a blocker. Newer accessible bins sit at the front of Lanes 1 and 2. A retrieve order fires with `retrieve_algorithm = FIFO`. Expected behavior: `resolveRetrieveFIFO` compares the oldest accessible bin's timestamp against the buried bin, finds the buried bin is older, and returns a `BuriedError` triggering a reshuffle. The fleet should receive a compound reshuffle order, not a direct retrieve. Production risk: without this, FIFO drift accumulates silently — parts with shelf-life or lot-tracking requirements get served out of order.

**TC-40b: COST mode — oldest accessible returned, older buried bin ignored.** Same 3-lane NGRP layout as TC-40a (oldest bin buried, newer bins accessible). A retrieve order fires with `retrieve_algorithm = COST`. Expected behavior: `resolveRetrieveCOST` returns the oldest accessible bin without scanning buried bins. No reshuffle triggered. The fleet should receive a direct retrieve to the accessible bin's storage slot. This validates the preserved cost-optimized behavior under its new name.

**TC-41: Empty cart starvation — all accessible empties consumed, buried empties unreachable.** LKND storage scatters empties across lanes where they get buried behind full bins. All accessible empties are consumed through normal retrieves. A press drops a full bin and needs an empty pickup. `FindEmptyCompatibleBin` returns nil — no accessible empty exists. Expected behavior: the system detects buried empties exist and triggers a reshuffle to unbury the shallowest one (fewest blockers). Production risk: press starvation. The empties are physically in the grid but unreachable. Manual intervention is the only current recovery.

### Timing and race conditions

**TC-6: Operator cancels at the exact moment the fleet accepts the order.** The cancel and the fleet acceptance happen at nearly the same time. Does the system end up in a clean state, or does the order get stuck — the fleet thinks it's active, the system thinks it's cancelled?

**TC-7: Operator releases before the robot has arrived at the wait point.** The Edge sends a release command for a staged order, but the robot hasn't actually reached the waiting state yet. The release should be rejected — the robot isn't there to continue.

**TC-8: Operator accidentally sends the release command twice.** Edge sends the release twice in a row. The second release should be ignored. It should not append duplicate blocks to the robot's instructions.

### Bad input handling

**TC-9: Complex order with zero steps.** Someone sends an order request with no steps. The system should fail with a clear error, not crash or create a broken order.

**TC-10: Order references a node that doesn't exist.** The order specifies a pickup or delivery node name that isn't in the database. Should fail with "node not found" instead of creating a partial order.

**TC-11: Only available bin is at a disabled storage node.** The storage node is marked as disabled (out of service). The system should not dispatch from disabled nodes — it should report no inventory available rather than sending a robot to a node that's offline.

**TC-12: Order requests zero quantity.** Someone sends an order for 0 bins. Should be rejected as invalid.

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

### Important technical constraints

**Receipt required for bin movement.** Tests that verify bin movement must call `HandleOrderReceipt` after driving to FINISHED. Without this step, the bin stays at its original location in the database.

**State changes fire events automatically.** When the simulator is wired into an Engine via `newTestEngine`, calling `DriveState` automatically fires events through the engine pipeline. Tests don't need to manually emit events.

**Each test gets a fresh database.** Every test function spins up its own Postgres container, so tests cannot interfere with each other. Container startup takes about 1-2 seconds.

### Files

| File | What it does |
|------|-------------|
| `fleet/simulator/simulator.go` | The fake fleet backend. Stores orders and blocks in memory. Implements TrackingBackend so the Engine can wire it up automatically. |
| `fleet/simulator/transitions.go` | State transition helpers. DriveState, DriveFullLifecycle, DriveSimpleLifecycle, DriveToFailed, DriveToStopped. |
| `fleet/simulator/inspector.go` | Read-only query methods. GetOrder, GetOrderByIndex, OrderCount, BlocksForOrder. Used by tests to inspect what the "fleet" received. |
| `fleet/simulator/options.go` | Fault injection. WithCreateFailure (fleet rejects orders), WithPingFailure (fleet health check fails). |
| `engine/engine_test.go` | Engine-level tests (regression and scenario). TC-15, TC-2, TC-21, TC-23 cluster, TC-24 cluster, TC-30, ClaimBin. Uses real Engine + real DB + simulator. |
| `dispatch/fleet_simulator_test.go` | Dispatcher-level tests (scenario). TC-1, TC-3, TC-4, TC-5. Tests the outbound path only (what gets sent to the fleet). |

---

## Future: Edge simulation

The current simulator replaces the fleet (RDS). A future addition would simulate the Edge station as well, allowing tests to verify the full round-trip: Core dispatches → robot moves → Edge detects arrival → Edge sends receipt → Core completes order.

Today, tests simulate Edge by manually calling `HandleOrderReceipt` on the dispatcher. A dedicated Edge simulator would make this more realistic by modeling the Edge's state machine (order tracking, receipt generation, staged order release timing).

This is not yet built. The current approach (manual receipt calls) is sufficient for testing Core's behavior. Edge simulation would be valuable when testing timing-dependent scenarios like "what happens if the Edge receipt arrives before the fleet reports FINISHED."
