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
go test -v -run "TestSimulator|TestClaimBin" ./engine/ ./dispatch/ -timeout 120s
```

Run a specific test:

```
go test -v -run TestSimulator_FullLifecycle ./engine/ -timeout 60s
```

If Docker isn't running, database tests skip automatically (they won't fail your build). Pure logic tests like state mapping always run.

---

## Tested cases

These tests have been implemented, run, and verified. Each entry describes what was tested, what happened, and whether any bugs were found.

### TC-1: Complex order bin tasks

**What it tests:** When you send a complex order (pick up from storage, drop off at line), the system creates instructions ("blocks") for the robot. Each block must tell the robot to physically jack the bin up or down. Without this, the robot drives to the location, sits there, and drives away. No bin moves.

**What happened:** Test passes. Every block correctly includes either a JackLoad (pick up) or JackUnload (set down) instruction, and the locations match the requested pickup and dropoff nodes.

**Why it matters:** This was a real bug found on the factory floor (2026-03-26). The robots were navigating to locations without jacking bins. The test locks out this class of bug permanently — if anyone changes how blocks are generated, this test fails before the code reaches production.

**Key assertion:**

```go
for _, b := range blocks {
    if b.BinTask == "" {
        t.Errorf("block %q at %q has empty BinTask — robots would navigate without jacking",
            b.BlockID, b.Location)
    }
}
```

**Test location:** `dispatch/fleet_simulator_test.go` — `TestSimulator_ComplexOrderBinTasks`

---

### TC-2: Staged complex order release

**What it tests:** A complex order with a "wait" step in the middle. The robot picks up, drops off at a staging area, and waits for an operator. When the operator releases the order, the remaining instructions (pick up from staging, return to storage) are appended to the robot's task.

**What happened:** The original version of this test had a bug — it was trying to release the order while the system still thought it was "dispatched" instead of "staged." The release handler requires the order to be in "staged" status before it accepts a release command, so the release was silently rejected and the test passed without actually testing anything.

**What was fixed:** The test was rewritten to use the full engine pipeline. Now it properly drives the robot through CREATED → RUNNING → WAITING. When the robot reaches WAITING, the engine automatically updates the database status to "staged." Only then does the test send the release command. After release, it verifies all 4 blocks are present (2 pre-wait + 2 post-wait), then drives through RUNNING → FINISHED to complete the order.

**Why it matters:** Staged orders are how Shingo handles cycle operations where the robot has to wait for a human. If the release flow breaks, robots get stuck at staging areas indefinitely.

**The critical sequence:**

```go
// Drive robot to WAITING — engine sets DB status to "staged"
sim.DriveState(order.VendorOrderID, "WAITING")
order, _ = db.GetOrderByUUID("staged-tc2")
// Status must be "staged" before release will be accepted
assert(order.Status == "staged")

// Now Edge sends release — post-wait blocks get appended
d.HandleOrderRelease(env, &protocol.OrderRelease{OrderUUID: "staged-tc2"})

// 2 pre-wait + 2 post-wait = 4 blocks total
view := sim.GetOrder(order.VendorOrderID)
assert(len(view.Blocks) == 4)
assert(view.Complete == true)
```

**Test location:** `engine/engine_test.go` — `TestSimulator_StagedComplexOrderRelease`

---

### TC-3: Simple retrieve order

**What it tests:** The most basic operation — request a bin from storage, deliver it to the production line. Verifies the simulator receives exactly 2 blocks: JackLoad at the storage node and JackUnload at the line node.

**What happened:** Test passes. The dispatch pipeline correctly creates a transport order with the right block structure and locations.

**Why it matters:** If this breaks, nothing works. This is the baseline sanity check for the entire dispatch system.

**Test location:** `dispatch/fleet_simulator_test.go` — `TestSimulator_SimpleRetrieveOrder`

---

### TC-4: State mapping

**What it tests:** The simulator must translate fleet states (CREATED, RUNNING, WAITING, FINISHED, FAILED, STOPPED) into Shingo statuses (dispatched, in_transit, staged, delivered, failed, cancelled) exactly the same way the real RDS adapter does. If the mapping is wrong, the system thinks the robot is in a different state than it actually is.

**What happened:** Test passes after a fix. The original simulator was mapping WAITING to "waiting" instead of "staged," and was missing the TOBEDISPATCHED state entirely. Both were corrected to match the real RDS adapter.

**What was fixed:** Updated the simulator's state mapping so WAITING maps to "staged" (matching the real adapter) and added TOBEDISPATCHED as an alias for "dispatched."

**Why it matters:** A wrong state mapping means the entire event pipeline processes the order incorrectly. For example, the staged order release flow (TC-2) depends on WAITING mapping to "staged" — with the wrong mapping, releases would always be rejected.

**Test location:** `dispatch/fleet_simulator_test.go` — `TestSimulator_StateMapping`

---

### TC-5: Fleet creation failure

**What it tests:** When the fleet server is down and rejects an order, the system should fail the order cleanly. It should not create a "return" order for a bin that never actually left storage — the fleet never accepted the order, so there's nothing to return.

**What happened:** Test passes. The order is marked as failed, has no vendor order ID (confirming the fleet never accepted it), and no phantom orders exist in the simulator.

**Why it matters:** Without this guard, a fleet outage would cause the system to generate return orders for bins that never moved, creating confusion about where bins actually are.

**Test location:** `dispatch/fleet_simulator_test.go` — `TestSimulator_FleetFailure_NoVendorOrderID`

---

### TC-15: Full lifecycle (end-to-end)

**What it tests:** The complete journey of a retrieve order from start to finish: dispatch the order, robot picks up, robot is in transit, robot arrives, Edge station confirms receipt, bin moves in the database, and the claim on the bin is released so it's available for future orders.

This is the most important test because it exercises every layer of the system: Dispatcher, Engine, EventBus, fleet state tracking, bin movement, and claim management.

**What happened:** Test passes. The full chain works:

1. Order dispatched — robot task created in fleet, bin claimed, order status "dispatched"
2. Robot running — engine detects state change, updates order to "in_transit"
3. Robot finished — engine sets order to "delivered"
4. Edge confirms receipt — triggers bin arrival processing, bin moves to destination node, claim released
5. Final state: order "confirmed", bin at the correct node, no claim remaining

**Why it matters:** This proves the entire feedback loop works. Previous tests only checked the outbound path (Core → fleet). This test also checks the inbound path (fleet → Core → database updates). Most real bugs live in this feedback loop — bins not moving, claims not releasing, orders stuck in "delivered" forever.

**Key detail:** The robot arriving (FINISHED) does not automatically move the bin. The Edge station must send a receipt confirmation first. This matches real behavior — the Edge operator confirms the bin arrived before the system updates inventory. The test simulates this Edge confirmation step.

**The lifecycle chain:**

```go
// 1. Dispatch
d.HandleOrderRequest(env, &protocol.OrderRequest{...})
order, _ := db.GetOrderByUUID("lc-1")
assert(order.Status == "dispatched")

// 2. Robot running
sim.DriveState(order.VendorOrderID, "RUNNING")
assert(order.Status == "in_transit")

// 3. Robot finished — bin has NOT moved yet
sim.DriveState(order.VendorOrderID, "FINISHED")
assert(order.Status == "delivered")

// 4. Edge confirms receipt — NOW the bin moves
d.HandleOrderReceipt(env, &protocol.OrderReceipt{
    OrderUUID: "lc-1", ReceiptType: "confirmed", FinalCount: 1,
})
assert(order.Status == "confirmed")

// 5. Bin is at destination, claim released
bin, _ := db.GetBin(*order.BinID)
assert(*bin.NodeID == lineNode.ID)      // bin moved
assert(bin.ClaimedBy == nil)             // claim released
```

**Test location:** `engine/engine_test.go` — `TestSimulator_FullLifecycle`

---

### ClaimBin: Silent claim overwrite (BUG FOUND)

**What it tests:** When the system claims a bin for an order (reserving it so no other order can use it), a second order should not be able to steal that claim. The test has order 1 claim a bin, then order 2 attempts to claim the same bin.

**What happened:** BUG CONFIRMED. The second claim silently succeeded. Order 200 overwrote order 100's claim with no error. The bin's `claimed_by` field changed from 100 to 200 without anyone being told.

**Root cause:** The database query that claims a bin (`UPDATE bins SET claimed_by=$1 WHERE id=$2 AND locked=false`) checks if the bin is locked but does not check if it's already claimed. It should include `AND claimed_by IS NULL` so that claiming an already-claimed bin fails instead of silently overwriting.

**Risk in production:** If two orders are dispatched at nearly the same time and both need the same type of part, they could both find the same bin available and both try to claim it. The second claim silently steals the bin from the first order. The first order thinks it has a bin reserved, but the bin is actually committed to the second order. When the first order's robot arrives at the bin location, the bin may already be gone.

In practice, the query that finds available bins (`FindSourceBinFIFO`) does filter out already-claimed bins, so this race window is narrow — both orders would have to query for bins in the gap between the first order's query and its claim. But under load (multiple lines requesting parts simultaneously), this becomes a real possibility.

**The bug in code:**

```sql
-- Current (broken): allows overwrite
UPDATE bins SET claimed_by=$1 WHERE id=$2 AND locked=false

-- Fixed: rejects claim on already-claimed bin
UPDATE bins SET claimed_by=$1 WHERE id=$2 AND locked=false AND claimed_by IS NULL
```

**Test output:**

```
engine_test.go:377: BUG: ClaimBin(bin=1, order=200) succeeded
    — silently overwrote claim from order 100. claimed_by is now 200
--- FAIL: TestClaimBin_SilentOverwrite (2.58s)
```

**Status:** Bug confirmed by test. Fix pending (next commit).

**Test location:** `engine/engine_test.go` — `TestClaimBin_SilentOverwrite`

---

## Planned tests (not yet implemented)

These tests have been identified as valuable but haven't been written yet. They are listed roughly in priority order.

### Claim lifecycle tests (highest priority)

These test the bin reservation system, which is where the most dangerous bugs can hide. A claim bug means the system's inventory tracking doesn't match physical reality.

**TC-25: Claim released on order failure.** When a fleet order fails (robot breaks down, fleet rejects), the bin claim should be automatically released so the bin is available for retry. If the claim isn't released, the bin becomes "stuck" — the system thinks it's reserved but no robot is coming for it.

**TC-26: Claim released on order cancellation.** Same as TC-25 but triggered by an operator cancelling the order instead of a fleet failure.

**TC-27: Claim survives order redirect.** When an operator redirects a robot mid-delivery to a different destination, the bin claim should stay intact. The bin is still committed to this order — it's just going somewhere else.

**TC-28: Double dispatch same payload.** Two orders request the same part type simultaneously. Each should get a different bin. If both get the same bin, one order will arrive at an empty location.

**TC-29: Claim with concurrent cancel.** An order is dispatched and claims a bin. While the robot is in transit, the operator cancels. The claim must release cleanly even though the robot hasn't finished yet.

**TC-30: Return order inherits no claim.** When a failed order triggers an automatic return (sending the bin back to storage), the return order should not inherit the original order's bin claim. The original claim should already be released by the failure handler.

**TC-31: Claim overwrite guard.** Direct test of the ClaimBin SQL — verify that claiming an already-claimed bin fails. (This is the bug we confirmed above. The test exists but the fix is pending.)

**TC-32: Staged bin expiry releases claim.** When a bin sits at a staging area too long and the staged timer expires, the system releases the staging status. If there's a claim on that bin, does the claim also get cleaned up?

**TC-33: Manual move on claimed bin.** An operator requests a manual move on a bin that is already claimed by an active order. What happens? Should the system reject the move, release the claim, or allow both?

### Race condition and edge case tests

**TC-6: Cancel during fleet creation.** Operator cancels an order at the exact moment the fleet is accepting it. Does the system cleanly cancel, or does the order get stuck?

**TC-7: Release before robot arrives at wait point.** Edge sends a release for a staged order before the robot has actually reached the waiting state. The release should be rejected (robot isn't there yet).

**TC-8: Double release.** Edge sends the release command twice. The second release should be ignored gracefully, not append duplicate blocks.

### Input validation tests

**TC-9: Empty steps in complex order.** Complex order request with zero steps. Should fail with a clear error, not crash.

**TC-10: Unknown node name.** Order references a node that doesn't exist in the database. Should fail with "node not found."

**TC-11: Disabled source node.** The storage node containing the needed bin is marked as disabled. The system should not dispatch from disabled nodes.

**TC-12: Zero quantity order.** Order requests quantity zero. Should be rejected.

### Cross-line and inventory tests

**TC-20: Two lines, same assembly.** Two production lines both need the same part. Will Core try to pull from the other line's staging area? (It shouldn't — each line's staged bins belong to active orders.)

**TC-21: Only bin is in quality hold.** A line requests a part, but the only available bin is in quality hold status. The system should not dispatch a held bin — it should report no inventory available.

**TC-22: Only bin is in maintenance area.** Similar to TC-21 but the bin is physically at a maintenance node.

**TC-23: Manual move on staged bin.** An operator manually moves a bin that is currently staged (waiting at a staging area for an active order). What happens to the order?

**TC-24: Inventory freed by order completion.** Order A finishes and releases its bin. Order B was waiting for inventory. Does Order B's fulfillment scanner pick up the newly available bin?

### State and mapping tests

**TC-16: Unknown vendor state.** Fleet reports a state string that the simulator doesn't recognize. Should map to a safe default, not crash.

**TC-17: Duplicate state transition.** Fleet reports the same state twice (e.g., RUNNING → RUNNING). The system should treat this as a no-op and not emit duplicate events.

**TC-18: Out-of-order state transition.** Fleet reports FINISHED before RUNNING. Can happen if a poll is missed. The system should handle this gracefully.

**TC-19: Rapid state transitions.** Fleet goes through CREATED → RUNNING → FINISHED in rapid succession (robot completes a very short trip). All three state changes should be processed and the order should end up in the correct final state.

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
| `engine/engine_test.go` | Engine-level tests. TC-15 (full lifecycle), TC-2 (staged release), ClaimBin overwrite test. Uses real Engine + real DB + simulator. |
| `dispatch/fleet_simulator_test.go` | Dispatcher-level tests. TC-1, TC-3, TC-4, TC-5. Tests the outbound path only (what gets sent to the fleet). |

---

## Future: Edge simulation

The current simulator replaces the fleet (RDS). A future addition would simulate the Edge station as well, allowing tests to verify the full round-trip: Core dispatches → robot moves → Edge detects arrival → Edge sends receipt → Core completes order.

Today, tests simulate Edge by manually calling `HandleOrderReceipt` on the dispatcher. A dedicated Edge simulator would make this more realistic by modeling the Edge's state machine (order tracking, receipt generation, staged order release timing).

This is not yet built. The current approach (manual receipt calls) is sufficient for testing Core's behavior. Edge simulation would be valuable when testing timing-dependent scenarios like "what happens if the Edge receipt arrives before the fleet reports FINISHED."
