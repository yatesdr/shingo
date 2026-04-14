# Test Catalog

Regression and feature test cases for ShinGo Core, organized by subsystem. Each TC-N identifier maps to a Go test function with the same ID in its comment header. Run individual tests with:

```sh
go test -v ./<package> -run <TestFunctionName>
```

## Fleet Simulator (`dispatch/fleet_simulator_test.go`)

| ID | Description |
|----|-------------|
| TC-1 | Complex order blocks must include JackLoad/JackUnload bin tasks |
| TC-2 | Staged complex order — pre-wait blocks sent initially, post-wait blocks held |
| TC-3 | Simple retrieve — single pickup/dropoff dispatched to simulator |
| TC-4 | Simulator state mapping matches RDS adapter mapping |
| TC-5 | Fleet creation failure causes order to fail with no vendor order ID |

## Group Resolver (`dispatch/group_resolver_test.go`)

| ID | Description |
|----|-------------|
| TC-40a | FIFO mode — buried bin older than accessible triggers reshuffle |
| TC-40b | COST mode — oldest accessible returned, older buried bin ignored |
| TC-41 | Empty cart starvation — FindEmptyCompatibleBin is lane-unaware |

## Dispatch Integration (`dispatch/integration_test.go`)

| ID | Description |
|----|-------------|
| TC-41 | retrieve_empty for a buried empty triggers reshuffle instead of returning buried bin |
| TC-71 | Move order where source and destination are the same node must fail |

## Engine — Core Lifecycle (`engine/engine_test.go`)

| ID | Description |
|----|-------------|
| TC-15 | Full lifecycle — order submitted, dispatched, delivered, confirmed |
| TC-2 | Staged complex order release |
| TC-ClaimBin | Silent claim overwrite — second order claiming same bin |
| TC-21 | Only available bin is in quality hold — order fails cleanly |
| TC-23a | Operator tries to move a claimed bin via a second store order |
| TC-23b | Cancel in-flight store order — return order claims bin |
| TC-23c | Changeover with one bin already gone |
| TC-23d | Changeover while move-to-quality-hold is still in flight |
| TC-24 | Complex order bin poaching |
| TC-24b | Stale bin location after complex order completes |
| TC-24c | Phantom inventory — retrieve dispatched to empty node |
| TC-25 | Store order correctly claims staged bin at core node |
| TC-28 | Two lines request the same part at the same time |
| TC-30 | Failed order creates a return — does the return inherit the reservation? |
| TC-36 | Retrieve claim failure — queue instead of fail |
| TC-38 | Cancel delivered order must not create return order / receipt on cancelled order |
| TC-39 | TerminateOrder rejects terminal statuses |
| TC-80 | Orphaned bin claim after terminal order — reconciliation detects and sweep fixes |

## Engine — Concurrency (`engine/engine_concurrent_test.go`)

| ID | Description |
|----|-------------|
| TC-9 | Complex order with zero steps |
| TC-10 | Order references nonexistent delivery node |
| TC-12 | Order requests zero quantity |
| TC-37 | Staging sweep flips bin to available while still claimed |

## Engine — Complex Orders (`engine/engine_complex_test.go`)

| ID | Description |
|----|-------------|
| TC-42 | Complex order round-trip lifecycle (implicit, referenced in surrounding tests) |
| TC-47 | Empty post-wait release |
| TC-48 | Complex order redirect doesn't update StepsJSON |
| TC-49 | Ghost robot — claimComplexBins finds no bin |
| TC-50 | Concurrent complex orders targeting same node — double claim race |
| TC-55 | Sequential Backfill (Order B) — simplest, no wait |
| TC-56 | Sequential Removal (Order A) — wait/release lifecycle |
| TC-57 | Two-Robot Swap Resupply (Order A) |
| TC-58 | Two-Robot Swap Removal (Order B) |
| TC-59 | Staging + Deliver Separation |
| TC-60 | Single-Robot 9-Step Swap |
| TC-DW | Double-wait complex order — Phase 3 evacuate flow prerequisite |

## Engine — Compound Orders (`engine/engine_compound_test.go`)

| ID | Description |
|----|-------------|
| TC-44 | Compound order basic lifecycle (implicit, group header) |
| TC-45 | Compound order child failure handling |
| TC-46 | Compound order partial completion |
| TC-51 | AdvanceCompoundOrder skips failed children — premature completion |
| TC-52 | Lane lock contention — second reshuffle blocked |
| TC-53 | ApplyBinArrival status mapping for compound restock children |
| TC-54 | Staging TTL expiry during compound order execution |

## Engine — Vendor Status (`engine/wiring_vendor_status_test.go`)

| ID | Description |
|----|-------------|
| TC-VS-1 | RUNNING state updates order to in_transit and assigns robot ID |
| TC-VS-2 | Idempotent status — driving same state twice doesn't error |
| TC-VS-3 | FINISHED terminal state — order delivered, bin moved to dest |
| TC-VS-4 | FAILED terminal state — order failed, EventOrderFailed emitted |
| TC-VS-5 | STOPPED terminal state — order cancelled, EventOrderCancelled emitted |
| TC-VS-6 | Non-existent order — handleVendorStatusChange logs and returns gracefully |

## Engine — Vendor Robot Assignment (`engine/wiring_vendor_robot_test.go`)

| ID | Description |
|----|-------------|
| TC-90 | First robot assignment persists robot ID and sends waybill |
| TC-91 | Case D regression — subsequent event with empty RobotID does NOT clobber |
| TC-92 | Robot reassignment — event with different non-empty RobotID updates |
| TC-93 | Idempotent no-write — same status + same robot = no state change |
| TC-94 | Option C dedup — first robot assignment + status change = single UpdateOrderVendor |
| TC-95 | Idempotent path uses narrow UpdateOrderRobotID when robot changes without status change |

## Engine — Staging (`engine/wiring_staging_test.go`)

| ID | Description |
|----|-------------|
| TC-RS-1 | Normal lineside node (no parent) — staged=true |
| TC-RS-2 | Storage slot under a LANE parent — staged=false |
| TC-RS-3 | Node with non-LANE parent — staged=true (treated as lineside) |
| TC-RS-4 | Node with no parent ID — staged=true (lineside default) |

## Engine — Order Completion (`engine/wiring_completion_test.go`)

| ID | Description |
|----|-------------|
| TC-CO-1 | Normal receipt — bin already at destination (idempotent safety net) |
| TC-CO-2 | handleOrderCompleted with missing BinID — early return, no crash |
| TC-CO-3 | handleOrderCompleted with missing source/delivery nodes — early return |
| TC-CO-4 | handleOrderCompleted for non-existent order — log and return |
| TC-CO-5 | handleOrderCompleted safety net — bin NOT at dest yet |
| TC-CO-6 | handleOrderCompleted with retrieve_empty payload — staged=false override |
| TC-CO-7 | handleOrderCompleted with complex order + WaitIndex > 0 — staged=false override |

## Web Handlers — Bin Actions (`www/handlers_bins_test.go`)

| ID | Description |
|----|-------------|
| TC-70 | Moving a bin to its current node is physically impossible and must be rejected |

## Known Gaps

- **No HTTP-level tests for `/api/bins/request-transport`** — the transport endpoint has backend validation (same-node rejection) but no Go test. The `www` package currently only tests `executeBinAction` directly, not HTTP handlers. Adding httptest coverage would require new test infrastructure.
- **TC-71 covers dispatch-level same-node rejection** but there is no corresponding test for an edge station submitting a move order where both `source_node` and `delivery_node` resolve to the same concrete node through NGRP resolution.
