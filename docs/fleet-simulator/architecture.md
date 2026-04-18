# Architecture Reference

How the simulation harness is built, for anyone who needs to add new tests or modify the simulator.

## The simulation harness

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

## Concurrency testing infrastructure

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

## Important technical constraints

**Receipt required for bin movement.** Tests that verify bin movement must call `HandleOrderReceipt` after driving to FINISHED. Without this step, the bin stays at its original location in the database.

**State changes fire events automatically.** When the simulator is wired into an Engine via `newTestEngine`, calling `DriveState` automatically fires events through the engine pipeline. Tests don't need to manually emit events.

**Each test gets a fresh database.** All tests share a single Postgres container (started once per process via `sync.Once`), but each test gets its own `CREATE DATABASE`. This gives full isolation without the overhead of 90+ containers. The shared infrastructure lives in `internal/testdb/`.

## Files

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
│   ├── end_to_end_test.go         # 13 tests, uses testdb.NewTrackingBackend/CreateBinAtNode
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
| `dispatch/end_to_end_test.go` | End-to-end dispatcher tests. 13 tests, uses `testdb.NewTrackingBackend()` and `testdb.CreateBinAtNode()` for bin setup. Drives the dispatcher through complete retrieve/move/store/cancel/redirect/synthetic/reshuffle lifecycles. |
| `dispatch/fleet_simulator_test.go` | Dispatcher-level scenario tests (TC-1, TC-3, TC-4, TC-5). Tests the outbound path only (what gets sent to the fleet). |
| `dispatch/bin_lifecycle_test.go` | Bin lifecycle integration tests (TC-64a–c). Full depletion, partial consumption, concurrent retrieve_empty race. |
| `dispatch/planning_test.go` | `extractRemainingUOP` unit tests (TC-65). Nil, empty, missing, zero, positive, malformed JSON. |
| `service/bin_manifest_test.go` | BinManifestService unit tests (TC-62a–h). All manifest mutations, atomic clear+claim, lock rejection. |
| `messaging/core_data_service_test.go` | Core data service tests (TC-69). Node list response includes NGRP node groups for station-scoped and global paths. Uses testcontainers Postgres. |

Edge test files (in `shingo-edge/`, SQLite-based — no Docker required):

| File | What it does |
|------|-------------|
| `engine/produce_swap_test.go` | Produce swap mode tests (TC-66a–f). All four swap modes, rejection guards. Includes `seedProduceNode` and `testEngine` helpers. |
| `engine/wiring_test.go` | Edge wiring event handler tests (TC-67a–g, TC-70, TC-72a–e). Ingest/retrieve completion, counter delta, UOP reset, bin_loader move completion, payload catalog prune, A/B cycling. |
| `engine/changeover_test.go` | Changeover automation tests (TC-74a–g, TC-76a–c). Wiring lifecycle, auto-staging, order failure/retry, cancel mid-staging, keep-staged. Includes `seedChangeoverScenario` and `startChangeover` helpers. |
