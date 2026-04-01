# Concurrency & Stress

## Overview

Concurrency tests verify the system under contention. The primary concern is the TOCTOU (time-of-check-time-of-use) race between `FindSourceBinFIFO` and `ClaimBin` — two orders can both find the same bin before either claims it. The PostFindHook enables deterministic race injection by pausing between find and claim. `simulator.ParallelGroup` provides barrier-synchronized goroutine launches for stress testing. These tests confirm that contention produces correct outcomes: no double-claims, no permanent failures, excess orders queued for retry.

## Test files

- `engine/engine_concurrent_test.go` — all concurrency tests (TC-71a-e)

Run this domain's tests:

```bash
cd shingo-core
go test -v -run "TestConcurrent|TestRedirect|TestFulfillmentScanner" ./engine/ -timeout 120s
```

## Index

| TC | Description | Status |
|----|-------------|--------|
| TC-71a | Deterministic TOCTOU claim race (PostFindHook) | PASS |
| TC-71b | Dispatch stress — 20 concurrent orders, 10 bins | PASS |
| TC-71c | Redirect mid-transit — claim intact | PASS |
| TC-71d | Fulfillment scanner — queue to dispatch round-trip | PASS |
| TC-71e | SSE order list table refresh on order-update | PASS |

## Verified scenarios

### TC-71a: Deterministic TOCTOU claim race (PostFindHook) — PASS

**Scenario:** Two orders compete for the same bin. A PostFindHook is installed between `FindSourceBinFIFO` and `ClaimBin` to widen the TOCTOU race window. Goroutine 1 finds the bin, hits the hook, and pauses. Goroutine 2 starts, finds the same bin, claims it successfully. Goroutine 1 resumes, its `ClaimBin` fails with `claim_failed`. The test is 100% deterministic — the hook guarantees both goroutines enter the TOCTOU window simultaneously.

**Expected behavior:** One order dispatches with the bin claimed. The other is queued (not permanently failed) so the fulfillment scanner retries when a bin becomes available.

**Result:** PASS. The hook guarantees the race. One order dispatches, the other is queued with status `queued`. Neither order permanently fails.

**Test:** `engine/engine_concurrent_test.go` — `TestConcurrent_ClaimRaceDeterministic`

---

### TC-71b: Dispatch stress — 20 concurrent orders, 10 bins — PASS

**Scenario:** 20 orders fire simultaneously against 10 bins (2:1 contention ratio). `GOMAXPROCS` is set to `runtime.NumCPU()` to maximize real concurrency. Tests whether the claim-then-dispatch path produces any double-claims or permanent failures under pressure.

**Expected behavior:** Each bin is claimed by at most 1 order. No order permanently fails — excess orders should be queued.

**Result:** PASS. No double-claims detected. No orders permanently failed.

**Test:** `engine/engine_concurrent_test.go` — `TestConcurrent_DispatchStress`

---

### TC-71c: Redirect mid-transit — claim stays intact — PASS

**Scenario:** A retrieve order is dispatched and the robot reaches RUNNING (in_transit). The operator redirects the order to a different line node. The old vendor order should be cancelled in the fleet, a new one created, and the bin claim should remain intact.

**Expected behavior:** After redirect, the order's bin claim survives. A new vendor order is dispatched to the new destination. The claimed bin is still claimed by the same order.

**Result:** PASS. Bin claim intact after redirect. New vendor order dispatched to the second line.

**Test:** `engine/engine_concurrent_test.go` — `TestRedirect_MidTransit`

---

### TC-71d: Fulfillment scanner — queue to dispatch round-trip — PASS

**Scenario:** A retrieve order is submitted but no bins are available. The order is queued. Later, a compatible bin appears at a storage node. The fulfillment scanner runs and dispatches the queued order.

**Expected behavior:** The order starts as `queued`. After a bin appears and the scanner runs, the order transitions to `dispatched` with a bin claimed. Driving through the full lifecycle (RUNNING → FINISHED → receipt) should complete the order and move the bin to the destination.

**Result:** PASS. Full queue → scan → dispatch → deliver → confirm round-trip verified. Bin correctly at line node, claim released after completion.

**Test:** `engine/engine_concurrent_test.go` — `TestFulfillmentScanner_QueueToDispatch`

---

### TC-71e: SSE order list table refresh on order-update — PASS (Core UI)

**Scenario:** The Core orders page uses Server-Sent Events (SSE) for real-time updates. When an order's status changes (e.g., dispatched -> in_transit -> delivered -> confirmed), the server pushes an `order-update` event. The order list table is server-rendered HTML with no dynamic list-loading mechanism. Without a refresh mechanism, the table shows stale data until the operator manually reloads the page.

**Expected behavior:** When an SSE `order-update` event fires, the page should refresh to show the latest order statuses in the table.

**Result:** PASS. The `orders.js` `onOrderUpdate` handler (debounced at 2 seconds) already calls `location.reload()` on every SSE event. This is the simplest correct approach: the order list is server-rendered HTML, so a full page reload is the appropriate refresh mechanism. The handler also refreshes the order detail modal if the updated order is currently open.

**Why this works:** The server-rendered HTML table has no dynamic list loading function. Adding one would require significant refactoring (fetching HTML partials, diffing DOM, managing pagination state). `location.reload()` is the correct solution for server-rendered content. The 2-second debounce prevents rapid reloads during batch status changes.

**Note:** This was investigated as Bug 2 after observing that SSE events might not refresh the list. The investigation confirmed the existing implementation is correct -- `location.reload()` fires on every debounced `order-update` event.

**Test:** Manual verification via Core UI. `shingo-core/www/static/pages/orders.js` -- `onOrderUpdate` handler (line 249).
