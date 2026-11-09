# The engine test harness

Verified against: ae059495

How Core's engine tests drive a real Engine against the fleet simulator, for anyone who needs to add a test
or modify the simulator.

## The simulation harness

The engine test harness (`newTestEngine`, in `shingo-core/engine`'s docker-tagged tests) creates a real
Shingo Engine on its own Postgres database, connected to the simulator (`simulator.New`) instead of RDS.
When you call `sim.DriveState()` to advance a simulated robot, the following chain fires automatically:

```
sim.DriveState(vendorOrderID, "RUNNING")
    → Simulator emits an OrderStatusChanged event
    → Engine's EventBus delivers it to handleVendorStatusChange
    → Handler writes the order's new status
    → Handler notifies Edge (writes to the outbox table, goes nowhere in tests)

sim.DriveState(vendorOrderID, "FINISHED")
    → Same chain → status becomes "delivered"
    → handleOrderDelivered → applyBinArrivalForOrder
    → Each of the order's carts is set down at its last drop, its claim released
    → EventOrderCompleted → handleOrderCompleted (finds nothing left to move)

dispatcher.HandleOrderReceipt(...)     (simulates Edge confirmation)
    → ConfirmReceipt → status becomes "confirmed" → EventOrderCompleted
    → handleOrderCompleted: advances a compound parent; re-applies any arrival still owed
```

The key detail: the robot finishing (FINISHED) is what moves the bin, so the next order sees the occupancy
the floor has; the Edge's receipt confirms the order and moves nothing that already landed
(`TestRegression_BinMovesOnDelivered`, `TestRegression_CompletionIdempotentAfterDelivery`). A compound
order's robot-internal children confirm themselves on delivery, since no operator stands at their
destination.

A running sim stack is measured by `shingo-core/cmd/soakstat`, not by this harness
([plants/README.md](../../plants/README.md), "Tooling"); its ledger's columns are documented at the top of
`shingo-core/cmd/soakstat/ledger.go`.

## Concurrency testing infrastructure

Concurrency tests verify the system under contention. The primary concern is the window between finding a
source bin and acquiring it — two orders can both find the same bin before either holds it. The fulfillment
scanner is the single place an order acquires its source (`(*Scanner).fulfill` in `shingo-core/fulfillment`,
one scan at a time), and a race there must re-queue the loser, never drop it. These tests confirm that
contention produces correct outcomes: no double-claims, no permanent failures, excess orders queued for
retry.

The simulator harness includes infrastructure for deterministic and statistical concurrency testing:

**PostFindHook** — A test-only synchronization point the fulfillment scanner fires between its find and
its acquire. When set via `Dispatcher.SetPostFindHook(fn)`, the hook runs after a bin is found and before
it is reserved. Tests use it to make a claim race deterministic: the hook takes the bin for another order,
and the test asserts the order re-queues (`TestPhase0_DispositionTriad_ClaimFailed` in
`shingo-core/dispatch`).

```go
d.SetPostFindHook(func() {
    // This runs between Find and the acquire — widen the race window
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

Run this domain's tests:

```bash
cd shingo-core
go test -tags docker -count=1 -run "TestConcurrent|TestRedirect|TestFulfillmentScanner|TestPhase0_DispositionTriad" ./engine/ ./dispatch/ -timeout 120s
```

## Important technical constraints

**The bin moves on FINISHED.** A test that checks where a bin is after delivery reads it after driving to
FINISHED; `HandleOrderReceipt` is what confirms the order and advances a compound parent.

**State changes fire events automatically.** When the simulator is wired into an Engine via `newTestEngine`, calling `DriveState` automatically fires events through the engine pipeline. Tests don't need to manually emit events.

**Each test gets a fresh database.** A test process uses one Postgres server — its own container, or the
one `scripts/gate.sh` starts and names in `$SHINGO_TEST_PG` — and builds one migrated template database;
`testdb.Open` clones the template for each test (`CREATE DATABASE … TEMPLATE`), so every test is isolated
without re-running the migrations. `testdb.OpenShared` gives every test that passes the same key (a test
file's, `testdb.FileKey`) one database. The shared infrastructure lives in `shingo-core/internal/testdb`.

## Where the pieces are

- `shingo-core/fleet/simulator` — the fake fleet backend: the transitions (`DriveState`,
  `DriveFullLifecycle`, `DriveToFailed`), the read-only inspector (`FindOrderByLocation` and its
  siblings), fault injection (`WithCreateFailure`), and `ParallelGroup`.
- `shingo-core/internal/testdb` — the database (`testdb.Open`), the fixtures (`testdb.SetupStandardData`,
  `testdb.CreateBinAtNode`, the compound layout `testdb.SetupCompound`), and the mock fleet backends
  (`testdb.NewFailingBackend`, `testdb.NewSuccessBackend`, `testdb.NewTrackingBackend`).

The Edge's engine tests (`shingo-edge/engine`) run on SQLite and need no Docker. `go test -list` names
every test in a package.
