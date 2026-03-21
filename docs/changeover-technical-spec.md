# Changeover System — Technical Specification

This document is the single source of truth for the changeover system design across all three Shingo modules (Protocol, Core, Edge). It covers the architecture, clearing strategies, robot orchestration, cross-module implementation details, and open issues.

For tracked bugs and design decisions, see [changeover-open-problems.md](changeover-open-problems.md).

---

## Overview

A changeover switches a production line from one job style to another. The system automates material staging, clearing, and swapping using two parallel tracks that converge when the operator presses Execute.

**What exists today:** The changeover state machine is built and functional but uses an old linear model (7 states, manual advance, no automation). The parallel-track architecture described here is fully designed but not yet coded.

**The goal:** Connect the changeover state machine to the material handling cycle system. When an operator initiates a changeover, the system automatically stages new-style bins, pauses automation, optionally clears old bins for tooling access, and executes the swap using the same cycle infrastructure that runs normal operations.

**The operator's role:** Initiate (select new style, press Start), signal readiness (Parts Done, Tooling Done), and execute (press Execute). The system handles everything in between.

---

## Changeover Configuration

Set per changeover on the changeover menu (different job style transitions on the same line may need different settings):

| Field | Type | Description |
|-------|------|-------------|
| From Job Style | string | Currently running style |
| To Job Style | string | Target style (must have payloads pre-configured) |
| Tool Access | bool | When true, material is cleared after Parts Done for tooling cart access |
| Clearing Strategy | string | `direct` or `sweep_to_stage` — how old bins are cleared from lineside (only when tool access = true) |
| Clearing Robots | int | How many robots assigned to clearing chains (sweep_to_stage only; direct uses one robot per bin) |
| Dedicated Robots | int | How many of the last staging robots stay at staging for the swap phase |

---

## Parallel Track Architecture

```
                 ┌─ STAGING TRACK (background, auto) ──────────────────┐
  Start ─────────┤                                                      ├── Execute → Running
                 └─ LINE TRACK (operator-driven) ──────────────────────┘
```

**Staging Track** runs immediately on changeover start. Robots fetch new-style bins from storage and deliver them to staging areas near the cell. The last N robots are "dedicated" — they deliver and wait at staging for the swap.

**Line Track** is operator-driven. Production continues in the Stopping phase until the operator signals "Parts Done." If tool access is needed, material is cleared and the operator signals "Tooling Done" after the physical die change.

**Convergence Gate** — the Execute button only appears when both tracks are complete. The operator must press it explicitly.

### State Machine

The changeover is NOT a linear state machine. The `Machine` struct tracks two independent boolean flags (`stagingDone`, `lineDone`) plus a line phase string (`linePhase`). A composite `DisplayPhase` function derives the UI-facing state from both tracks.

**Staging Track:**
| State | Description | Advance |
|-------|-------------|---------|
| Active | Robots delivering new-style bins to staging | Auto (all orders complete) |
| Done | All staging orders finished | — |

**Line Track:**
| Phase | Description | Advance |
|-------|-------------|---------|
| `stopping` | Auto-reorder disabled, operator still running parts | Manual: "Parts Done" button (or idle auto-detect) |
| `clearing` | Removing old bins from lineside for tool access | Auto (all clearing orders complete) |
| `tooling` | Waiting for operator to finish physical die change | Manual: "Tooling Done" button |
| (done) | `lineDone = true` | — |

**Convergence:**
| Condition | Result |
|-----------|--------|
| `stagingDone && lineDone && !executeRequested` | UI shows "Ready — Execute Changeover" button |
| `stagingDone && lineDone && executeRequested` | Transitions to Executing |

**Display Phase Logic:**
```
DisplayPhase(linePhase, stagingDone, lineDone):
  if linePhase == "running"   → "running"
  if linePhase == "executing" → "executing"
  if stagingDone && lineDone  → "ready"
  else                        → linePhase (stopping, clearing, tooling)
```

### What the System Does at Each Phase

**Stopping** — Operator starts changeover (selects new job style, clicks Start):
1. Set `auto_reorder = false` for all payloads on this line's active job style
2. Cancel all active orders (retrieve, complex) for payloads on this line
3. Wait for cancelled orders to reach terminal state
4. Staging track begins simultaneously (see Staging below)
5. Line track: operator keeps running parts until Parts Done

**Staging** — Runs in parallel with the line track from the moment changeover starts:
1. Look up the new job style's payloads
2. For each payload, create a retrieve order to deliver the new-style bin to its staging node
3. Last N orders are "dedicated" — dispatched as complex orders with `complete: false` (robot waits at staging)
4. Track completions; staging done when all orders complete or reach `staged` status

**Parts Done** — Operator signals they've finished running parts:
- If `tool_access = false`: line done immediately
- If `tool_access = true`: advance to Clearing

**Clearing** — Old bins removed from lineside (see Clearing Strategies below):
- Orders dispatched based on `clearing_strategy`
- Clearing done when all clearing orders complete
- Auto-advance to Tooling

**Tooling** — Waiting for operator to finish physical die change:
- Lineside is clear, operator has full access
- Operator presses "Tooling Done" when finished
- For `sweep_to_stage`: Phase B store orders dispatched in background
- Line done

**Executing** — Operator presses Execute (convergence gate open):
- Core releases dedicated robots with swap blocks appended
- Dedicated robots execute multi-bin swap chains
- Non-dedicated bins get new swap orders
- Auto-advance to Running when all swaps complete

**Running (Changeover Complete):**
1. Update the production line's `ActiveJobStyleID` to the new job style
2. Set `auto_reorder = true` for all new-style payloads
3. Reset remaining counts based on new UOP capacities
4. Emit `ChangeoverCompleted` event
5. Normal operations resume

---

## Scenarios

### Scenario A — No Tool Access

```
Time →

STAGING:  [Retrieve Bin A] [Retrieve Bin B] [Retrieve Bin C*] [Retrieve Bin D*]  ✓ Done
                                                                    (* = dedicated, wait at staging)

LINE:     [Stopping — operator runs parts] → [Parts Done] ✓ Done

GATE:     Both done → [Execute Changeover button]

EXECUTE:  Core releases dedicated robots with swap blocks appended
          Robot C: pickup old₁ → outgoing → pickup C → lineside → pickup old₂ → outgoing → pickup D → lineside
          Robot D: (similar pattern for remaining bins)

COMPLETE: Active job style updated, auto-reorder ON, running
```

### Scenario B — Tool Access, Direct Clearing

```
Time →

STAGING:  [Retrieve Bin A] [Retrieve Bin B] [Retrieve Bin C*] [Retrieve Bin D*]  ✓ Done

LINE:     [Stopping] → [Parts Done] → [Clearing: store orders, 1 per bin] → all complete
          → [Tooling: humans work] → [Tooling Done] ✓

GATE:     Both done → [Execute Changeover button]

EXECUTE:  Same as Scenario A

COMPLETE: Active job style updated, auto-reorder ON, running
```

### Scenario C — Tool Access, Sweep-to-Stage Clearing

```
Time →

STAGING:  [Retrieve Bin A] [Retrieve Bin B] [Retrieve Bin C*] [Retrieve Bin D*]  ✓ Done

LINE:     [Stopping] → [Parts Done] → [Clearing: sweep chains to clearing nodes] → all complete
          → [Tooling: humans work, lineside fully empty] → [Tooling Done] ✓
          → (Phase B: background store orders move clearing nodes → outgoing)

GATE:     Both done → [Execute Changeover button]

EXECUTE:  Same as Scenario A

COMPLETE: Active job style updated, auto-reorder ON, running
```

---

## Clearing Strategies

When `tool_access = true`, old-style bins must be physically removed from lineside before operators can access the die area with tooling carts. The clearing strategy is configured per changeover on the changeover menu.

### Strategy 1: Direct (`direct`)

Each old-style bin at lineside gets its own store order:

```
pickup(lineside) → dropoff(outgoing/market)
```

- One robot per bin, all clearing in parallel
- Clearing tracker marks done when all store orders complete
- Simple — reuses existing store order infrastructure
- Uses more robots; best when fleet has capacity and outgoing destinations are nearby

### Strategy 2: Sweep-to-Stage (`sweep_to_stage`)

Bins are cleared in two phases:

**Phase A — Sweep (blocks changeover):**
1-2 complex orders with multi-bin pickup/dropoff chains:

```
Robot 1: pickup(bin1@lineside) → dropoff(clearing_node1) → pickup(bin2@lineside) → dropoff(clearing_node2) → ...
Robot 2: pickup(bin3@lineside) → dropoff(clearing_node3) → pickup(bin4@lineside) → dropoff(clearing_node4) → ...
```

- `clearing_robots` config determines how many robots sweep (bins distributed evenly)
- No wait steps — dispatched as complete complex orders, robot runs the full chain immediately
- Short hops to nearby clearing nodes = fast lineside clearance
- Clearing tracker marks done when all Phase A orders complete

**Phase B — Store (background, does NOT block changeover):**
After operator presses Tooling Done, separate store orders are dispatched:

```
store(clearing_node1 → outgoing)
store(clearing_node2 → outgoing)
...
```

- Fire-and-forget: runs in background after line done
- Does not block the convergence gate or Execute
- Bins eventually reach final outgoing destination

**Best for:** lines where robots are scarce, clearing nodes are close to the cell, or die changes take significant time.

### Node Configuration (Engineers)

Clearing nodes are configured per payload in Edge setup:
- **Clearing Node**: nearby staging location where old bins are parked during sweep-to-stage
- **Outgoing Destination**: final destination for cleared bins (already exists on payload config)

For the `direct` strategy, only the outgoing destination is needed.

---

## Dedicated Robot Process

### The Problem

Without dedicated robots, pressing Execute would dispatch new robots from wherever they are in the plant. They drive to staging, pick up bins, drive to lineside, swap — all while the cell sits idle. The staging was half-wasted if the robots aren't ready too.

### The Solution

The last N staging robots are told to stay at the staging area after delivery. When Execute fires, Core appends swap work to their existing orders and releases them. They're already there — no drive time.

### Step-by-Step Robot Flow

**Example: 4 bins, 2 dedicated robots**

**During Staging:**

| Robot | Order Type | Steps | After Delivery |
|-------|-----------|-------|----------------|
| Robot 1 | Simple retrieve (fire-and-forget) | pickup Bin A from supermarket → dropoff at Staging 1 | Leaves, returns to pool |
| Robot 2 | Simple retrieve (fire-and-forget) | pickup Bin B from supermarket → dropoff at Staging 2 | Leaves, returns to pool |
| Robot 3 | Complex order with wait | pickup Bin C from supermarket → dropoff at Staging 3 → **WAIT** | Parks at Staging 3 (status: staged) |
| Robot 4 | Complex order with wait | pickup Bin D from supermarket → dropoff at Staging 4 → **WAIT** | Parks at Staging 4 (status: staged) |

**On Execute:**

Edge sends `changeover.execute` to Core. Core finds the 2 staged orders for this station. Core distributes 4 swap items across 2 robots:

| Robot | Assigned Bins | Swap Blocks Appended |
|-------|--------------|---------------------|
| Robot 3 | Bin A, Bin C | pickup old₁ from Line₁ → dropoff outgoing → pickup Bin A from Staging₁ → dropoff Line₁ → pickup old₃ from Line₃ → dropoff outgoing → pickup Bin C from Staging₃ → dropoff Line₃ |
| Robot 4 | Bin B, Bin D | pickup old₂ from Line₂ → dropoff outgoing → pickup Bin B from Staging₂ → dropoff Line₂ → pickup old₄ from Line₄ → dropoff outgoing → pickup Bin D from Staging₄ → dropoff Line₄ |

Core calls `ReleaseOrder(vendorOrderID, swapBlocks)` for each staged order. RDS resumes the robot with the new blocks.

### Connection to Existing Cycle System

The changeover reuses the same infrastructure as normal material handling:

| Concept | Normal Cycle | Changeover |
|---------|-------------|------------|
| Complex orders with wait | Used in sequential/two-robot/single-robot cycles | Used for dedicated staging robots |
| `order.release` / `HandleOrderRelease` | Operator presses RELEASE on canvas | Core releases via `ReleaseOrder` with swap blocks |
| `stepsToBlocks()` | Converts steps to RDS blocks | Same function builds swap blocks |
| `StagedOrderRequest.Vehicle` | Not used (fleet picks robot) | Used to target specific robot |
| `fleet.Backend.CreateStagedOrder` | Creates incremental RDS order | Same call, now with Vehicle field |
| Order completion tracking | Engine tracks via `EventOrderCompleted` | Same events feed changeover trackers |
| `RequestOrders(payloadID, 1)` | Counter crosses reorder point → one station | Changeover execute → all stations simultaneously |

---

## Cross-Module Changes

### Protocol Package (`protocol/`)

**New constants** in `types.go`:

| Constant | Value | Description |
|----------|-------|-------------|
| `SubjectChangeoverExecute` | `"changeover.execute"` | Edge → Core: execute the changeover swap |
| `SubjectChangeoverExecuteAck` | `"changeover.execute_ack"` | Core → Edge: swap orders created |

**New structs** in `payloads.go`:

| Struct | Direction | Purpose |
|--------|-----------|---------|
| `ChangeoverSwapItem` | Edge → Core | One bin position to swap (payload code, lineside, staging, outgoing, cycle mode, role) |
| `ChangeoverExecuteRequest` | Edge → Core | Line ID, station, dedicated robot count, list of swap items |
| `ChangeoverExecuteAck` | Core → Edge | Order count, per-order info (order ID, UUID, robot ID, payload codes) |
| `ChangeoverOrderInfo` | Core → Edge | Details of one swap order (which robot, which payloads) |
| `ChangeoverRobot` | Internal (Core) | Robot ID + current node (used in Core's robot lookup) |

### Shingo Core (`shingo-core/`)

**New file: `dispatch/changeover.go`**

| Function | Purpose |
|----------|---------|
| `HandleChangeoverExecute(env, req)` | Main entry point. Finds staged robots, distributes swap items, dispatches |
| `releaseWithSwapBlocks(env, req, stagedOrders)` | Appends swap blocks to staged orders and releases them |
| `createIndividualSwapOrder(env, req, item)` | Fallback: creates one swap order per bin (no dedicated robot) |
| `buildSwapStepsProto(item)` | Builds protocol step chain for one bin swap |
| `buildSwapStepsResolved(item)` | Builds resolved steps for appending to existing order |
| `pickupStagingOrSource(item)` | Returns pickup step from staging or source |
| `dropoffStep(node, nodeGroup)` | Returns dropoff step with node or group |

**Modified: `fleet/fleet.go`**

| Change | Detail |
|--------|--------|
| `StagedOrderRequest.Vehicle` | New `string` field. When set, targets a specific robot by ID. |

**Modified: `fleet/seerrds/adapter.go`**

| Change | Detail |
|--------|--------|
| `CreateStagedOrder` | Maps `req.Vehicle` → `rdsReq.Vehicle` in the RDS API call |

**Modified: `messaging/core_handler.go`**

| Change | Detail |
|--------|--------|
| `HandleData` switch | Added `SubjectChangeoverExecute` case |
| `handleChangeoverExecute` | New method: unmarshals request, calls dispatcher, sends ack reply |

**New query: `store/orders.go`**

| Function | Purpose |
|----------|---------|
| `ListStagedOrdersByStation(stationID)` | Returns orders with status `staged` for a station. Used to find dedicated robots. |

### Shingo Edge (`shingo-edge/`)

**Revised: `changeover/types.go`**

| Change | Detail |
|--------|--------|
| Phase constants | `PhaseRunning`, `PhaseStopping`, `PhaseClearing`, `PhaseTooling`, `PhaseExecuting` |
| Track constants | `TrackStaging`, `TrackLine` |
| `ChangeoverConfig` | New struct: `ToolAccess bool`, `ClearingStrategy string`, `ClearingRobots int`, `DedicatedRobots int` |
| `DisplayPhase()` | Derives UI-facing phase from both track states |

**Rewritten: `changeover/machine.go`**

| Method | Purpose |
|--------|---------|
| `Start(from, to, operator, cfg)` | Begins changeover with config. Both tracks start. |
| `MarkStagingDone()` | Staging track complete. Checks convergence. |
| `PartsDone(operator)` | Operator signals parts done. If tool access: → Clearing. Else: line done. |
| `MarkClearingDone()` | Clearing complete → Tooling phase. |
| `ToolingDone(operator)` | Operator signals tooling done → line done. Checks convergence. |
| `Execute(operator)` | Operator presses Execute. Sets gate flag, triggers if both tracks done. |
| `triggerExecuting()` | Internal: transitions to Executing if all conditions met. |
| `checkConvergence()` | Internal: checks if both tracks done, triggers if execute already requested. |
| `MarkExecutingDone(detail)` | All swap orders complete → Running. |
| `Advance(operator)` | Generic advance: maps to PartsDone, ToolingDone, or Execute. |
| `Cancel(operator)` | Aborts changeover, resets all state. |
| `SupervisorOverride(operator)` | Forces both tracks done, triggers convergence. |
| `Info()` | Returns (from, to, displayPhase, active, stagingDone, lineDone). |
| `Config()` | Returns the `ChangeoverConfig`. |
| `ToJobStyle()` / `FromJobStyle()` | Thread-safe getters. |

**New file: `engine/changeover.go`**

| Function | Purpose |
|----------|---------|
| `handleChangeoverStateChanged(evt)` | Main dispatcher: routes on new state |
| `handleChangeoverStopping(evt)` | Disables auto-reorder for old-style payloads |
| `handleChangeoverStagingStart(evt)` | Creates staging orders (fire-and-forget + dedicated with wait) |
| `handleChangeoverClearing(evt)` | Creates clearing orders based on strategy |
| `buildDirectClearingOrders(lineID, payloads)` | Strategy `direct`: one store order per old-style bin at lineside |
| `buildSweepClearingOrders(lineID, payloads, robotCount)` | Strategy `sweep_to_stage`: multi-bin complex order chains distributed across N robots |
| `handleToolingDone(lineID)` | On Tooling Done: marks line done; for sweep_to_stage, dispatches Phase B store orders in background |
| `handleChangeoverExecuting(evt)` | Builds swap items, sends `changeover.execute` to Core |
| `HandleChangeoverExecuteAck(ack)` | Processes Core's response |
| `handleChangeoverComplete(evt)` | Updates active job style, enables auto-reorder |
| `onStagingDone(lineID, phase, failed)` | Callback: marks staging track done |
| `onClearingDone(lineID, phase, failed)` | Callback: marks clearing done |
| `onExecutingDone(lineID, phase, failed)` | Callback: marks executing done |
| `ChangeoverProgress(lineID, phase)` | Returns (completed, total) for UI progress |
| `ValidateChangeoverStart(toJobStyleName)` | Checks target style has payloads configured |

**Tracker infrastructure** (also in `engine/changeover.go`):

| Component | Purpose |
|-----------|---------|
| `changeoverTracker` | Tracks in-flight order IDs for a phase, calls onDone when all complete |
| `trackers` map | Global registry keyed by `lineID:phase`, supports concurrent staging + clearing |
| `notifyChangeoverTrackers()` | Called on every order completion/failure, updates all active trackers |

**Modified: `engine/wiring.go`**

| Change | Detail |
|--------|--------|
| Added subscription | `EventChangeoverStateChanged` → `handleChangeoverStateChanged` |
| Added subscription | `EventOrderCompleted` → `notifyChangeoverTrackers` (success) |
| Added subscription | `EventOrderFailed` → `notifyChangeoverTrackers` (failure) |

**Modified: `messaging/edge_handler.go`**

| Change | Detail |
|--------|--------|
| `onChangeoverAck` callback | New field on `EdgeHandler` |
| `SetChangeoverAckHandler()` | Setter for the callback |
| `HandleData` switch | Added `SubjectChangeoverExecuteAck` case |

**Modified: `cmd/shingoedge/main.go`**

| Change | Detail |
|--------|--------|
| Wiring | `edgeHandler.SetChangeoverAckHandler(eng.HandleChangeoverExecuteAck)` |

**New API endpoints** in `www/handlers_api_config.go`:

| Endpoint | Method | Handler | Purpose |
|----------|--------|---------|---------|
| `/api/changeover/parts-done` | POST | `apiChangeoverPartsDone` | Operator signals parts are done |
| `/api/changeover/tooling-done` | POST | `apiChangeoverToolingDone` | Operator signals tooling is done |
| `/api/changeover/override` | POST | `apiChangeoverOverride` | Supervisor forces past current phase |

**Modified API** in `www/handlers_api_config.go`:

| Endpoint | Change |
|----------|--------|
| `/api/changeover/start` | Now accepts `tool_access` (bool), `clearing_strategy` (string), `clearing_robots` (int), and `dedicated_robots` (int) |

**Modified: `www/handlers_changeover.go`**

| Change | Detail |
|--------|--------|
| `handleChangeover` | Updated for new `Info()` return signature (6 values instead of 4) |
| Template data | Passes staging/clearing/executing progress, tool access flag, staging/line done booleans |

**New DB query** in `store/payloads.go`:

| Function | Purpose |
|----------|---------|
| `SetAutoReorderByJobStyle(jobStyleID, autoReorder)` | Batch toggle auto-reorder for all payloads in a job style |

**Rewritten: `www/templates/changeover.html`**

| Change | Detail |
|--------|--------|
| Two-column track display | Staging and Line tracks shown side-by-side |
| Contextual buttons | Parts Done (stopping), Tooling Done (tooling), Execute (ready) |
| Supervisor override card | Always visible during active changeover |
| Start form | Tool Access checkbox, Clearing Strategy dropdown, Clearing Robots field, Dedicated Robots field |
| Progress indicators | Per-phase completion counts (X of Y bins staged, etc.) |

**Rewritten: `www/static/js/pages/changeover.js`**

| Function | Purpose |
|----------|---------|
| `startChangeover()` | Sends config fields with start request |
| `partsDone()` | Calls `/api/changeover/parts-done` |
| `toolingDone()` | Calls `/api/changeover/tooling-done` |
| `executeChangeover()` | Calls `/api/changeover/advance` (which maps to Execute) |
| `supervisorOverride()` | Calls `/api/changeover/override` |

**Modified: `www/static/operator-canvas/display.js`**

| Change | Detail |
|--------|--------|
| `changeoverState` variable | Tracks active changeover for canvas rendering |
| SSE handler | Updates changeover state on `changeover-update` events instead of reloading |
| `renderFrame` calls | Pass `changeoverState` parameter |

**Modified: `www/static/operator-canvas/render.js`**

| Change | Detail |
|--------|--------|
| `renderFrame` signature | Added `changeoverState` parameter |
| `drawChangeoverBanner()` | New function: renders status banner at bottom of canvas (blue = active, green = ready) |

---

## Message Flow: Execute Phase

```
Edge                          Kafka                         Core
 |                              |                              |
 | (operator presses Execute)  |                              |
 |                              |                              |
 |-- data: changeover.execute ->|-- shingo.orders ----------->|
 |   { line_id, station_id,    |                              |
 |     dedicated_robots: 2,    |                              |
 |     swap_items: [...] }     |                              |
 |                              |                              |
 |                              |     Core finds staged orders |
 |                              |     for this station         |
 |                              |                              |
 |                              |     Distributes 4 items      |
 |                              |     across 2 robots          |
 |                              |                              |
 |                              |     Appends swap blocks      |
 |                              |     to each staged order     |
 |                              |                              |
 |                              |     ReleaseOrder(blocks)     |
 |                              |     for each robot           |
 |                              |                              |
 |<- data: changeover.execute_ack|<- shingo.dispatch --------- |
 |   { order_count: 2,         |                              |
 |     orders: [...] }         |                              |
 |                              |                              |
 |                              |     Robots execute swap      |
 |                              |     blocks (normal fleet     |
 |                              |     polling tracks progress) |
 |                              |                              |
 |<- order.delivered ----------|<- shingo.dispatch ---------- |
 |<- order.delivered ----------|<- shingo.dispatch ---------- |
 |                              |                              |
 | (all swap orders complete)  |                              |
 | → MarkExecutingDone()       |                              |
 | → handleChangeoverComplete()|                              |
 | → SetActiveJobStyle()       |                              |
 | → SetAutoReorderByJobStyle()|                              |
```

---

## Implementation Order

1. Revise state machine (changeover/types.go) — parallel tracks, ChangeoverConfig
2. Rewrite machine (changeover/machine.go) — dual-track with convergence gate
3. Add new DB queries for batch payload operations
4. Wire changeover events in engine (engine/wiring.go subscriptions)
5. Implement Stopping logic (engine/changeover.go — pause + cancel active orders)
6. Implement Staging logic (staging orders + dedicated robot wait + tracker)
7. Implement Clearing logic — both strategies:
   a. `direct`: store orders per bin, track completion
   b. `sweep_to_stage`: Phase A complex chains, Phase B background store orders
8. Implement Executing logic (changeover.execute message, Core handler, swap block building)
9. Implement completion logic (update job style, resume auto-reorder, reset payloads)
10. Update changeover UI (parallel track display, strategy selection, contextual buttons)
11. Update operator canvas for changeover awareness
12. Testing: full changeover cycle end-to-end (all scenarios)

---

## Open Issues

### 1. Idle Auto-Detect for Parts Done
The `Machine` struct has `idleSeconds` and `idleCallback` fields stubbed but not wired. The intent: if no PLC counter activity for N minutes, auto-signal parts done. Requires subscribing to counter events and tracking last delta timestamp per line.

### 2. Changeover Order Tracking Gap
When Core creates swap orders via `HandleChangeoverExecute`, the order IDs are returned in the ack. However, Edge doesn't currently populate the executing tracker with these IDs — the tracker was created empty before the ack arrives. Fix: on ack receipt, populate the tracker with the order UUIDs.

### 3. Cancellation During Staging
If the operator cancels while staging orders are in flight, those orders continue executing. Fix: on cancel, iterate the staging tracker's order IDs and call `AbortOrder` for each.

### 4. Cancellation During Executing
Similar to above — if cancelled during executing, swap orders continue. Dedicated robots keep running swap chains. Fix: abort all tracked executing orders on cancel. Aborting mid-swap is dangerous (robot holding a bin with nowhere to go) — may need to let current bin complete, then cancel remaining.

### 5. Restore on Restart
`Machine.Restore()` rebuilds state from the last changeover log entry, but doesn't restore tracker state. Fix: on restore, re-query active orders and rebuild trackers.

### 6. Two-Robot Cycle Mode in Changeover
The swap step builders currently use the sequential pattern regardless of cycle mode. Two-robot mode would need two robots per bin position, conflicting with the multi-bin-per-robot dedicated model. Decision needed: should changeover always use sequential swap?

### 7. Edge README Inaccuracy
The Edge README still says Stopping "cancels active orders." Actual behavior: Stopping only disables auto-reorder and lets in-flight orders complete naturally.

### 8. Wire Protocol Documentation
The `changeover.execute` and `changeover.execute_ack` subjects need to be added to `docs/wire-protocol.md`.

### 9. Changeover Log Schema
The changeover log stores `state` as a string representing the line phase, not both tracks. Adequate for audit but doesn't allow perfect state reconstruction from logs alone.

### Open Questions

1. **Release strategy during Executing:** Release each station individually, or "release all" at once?
2. **Staging order failure:** Retry, skip and flag, or block changeover?
3. **Swap failure during Executing:** Retry, continue with other stations, or pause?
4. **Payloads without staging nodes:** Deliver directly to lineside during Executing?
5. **Operator canvas:** Show changeover status indicator?
6. **New-style payload creation:** Must payloads exist before changeover starts, or auto-create from job style?
7. **Counter reset:** Does `resetPayloadOnRetrieve` handle this, or does changeover need a separate reset?
8. **Cancel during sweep_to_stage clearing:** Bins stranded at clearing nodes — return-to-lineside or let operator decide?
