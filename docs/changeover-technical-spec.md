# Changeover System — Technical Specification

This document details the complete changeover implementation across all three Shingo modules (Protocol, Core, Edge). It covers the parallel track architecture, every new and modified function, the robot orchestration process, integration with the existing material handling system, and known open issues.

---

## Architecture Overview

A changeover switches a production line from one job style to another. The system automates material staging, clearing, and swapping using two parallel tracks that converge when the operator presses Execute.

```
                 ┌─ STAGING TRACK (background, auto) ──────────────────┐
  Start ─────────┤                                                      ├── Execute → Running
                 └─ LINE TRACK (operator-driven) ──────────────────────┘
```

**Staging Track** runs immediately on changeover start. Robots fetch new-style bins from storage and deliver them to staging areas near the cell. Some robots are "dedicated" — they deliver and wait at staging for the swap.

**Line Track** is operator-driven. Production continues in Stopping phase until the operator signals "Parts Done." If tool access is needed, material is cleared and the operator signals "Tooling Done" after the physical die change.

**Convergence Gate** — the Execute button only appears when both tracks are complete. The operator must press it explicitly.

---

## Changeover Configuration

Set at start time on the changeover page:

| Field | Type | Description |
|-------|------|-------------|
| From Job Style | string | Currently running style |
| To Job Style | string | Target style (must have payloads pre-configured) |
| Tool Access | bool | When true, material is cleared after Parts Done for tooling cart access |
| Dedicated Robots | int | How many of the last staging robots stay at staging for the swap phase |

---

## Parallel Track State Machine

The changeover is NOT a linear state machine. The `Machine` struct tracks two independent boolean flags (`stagingDone`, `lineDone`) plus a line phase string (`linePhase`). A composite `DisplayPhase` function derives the UI-facing state from both tracks.

### States by Track

**Staging Track:**
| State | Description | Advance |
|-------|-------------|---------|
| Active | Robots delivering new-style bins to staging | Auto (all orders complete) |
| Done | All staging orders finished | — |

**Line Track:**
| Phase | Description | Advance |
|-------|-------------|---------|
| `stopping` | Auto-reorder disabled, operator still running parts | Manual: "Parts Done" button (or idle auto-detect) |
| `clearing` | Store orders removing old bins for tool access | Auto (all clearing orders complete) |
| `tooling` | Waiting for operator to finish physical die change | Manual: "Tooling Done" button |
| (done) | `lineDone = true` | — |

**Convergence:**
| Condition | Result |
|-----------|--------|
| `stagingDone && lineDone && !executeRequested` | UI shows "Ready — Execute Changeover" button |
| `stagingDone && lineDone && executeRequested` | Transitions to Executing |

### Display Phase Logic

```
DisplayPhase(linePhase, stagingDone, lineDone):
  if linePhase == "running"   → "running"
  if linePhase == "executing" → "executing"
  if stagingDone && lineDone  → "ready"
  else                        → linePhase (stopping, clearing, tooling)
```

---

## Scenario A — No Tool Access

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

## Scenario B — Tool Access Required

```
Time →

STAGING:  [Retrieve Bin A] [Retrieve Bin B] [Retrieve Bin C*] [Retrieve Bin D*]  ✓ Done

LINE:     [Stopping] → [Parts Done] → [Clearing: store old bins] → [Tooling: humans work] → [Tooling Done] ✓

GATE:     Both done → [Execute Changeover button]

EXECUTE:  Same as Scenario A — Core builds swap orders for dedicated robots

COMPLETE: Active job style updated, auto-reorder ON, running
```

---

## Dedicated Robot Process — In Depth

### The Problem

Without dedicated robots, pressing Execute would dispatch 4 new robots from wherever they are in the plant. They drive to staging, pick up bins, drive to lineside, swap — all while the cell sits idle. The whole point of staging was to have material ready. If the robots aren't ready too, the staging was half-wasted.

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

Core calls `ReleaseOrder(vendorOrderID, swapBlocks)` for each staged order. RDS resumes the robot with the new blocks. The robot executes the full swap chain without returning to the pool.

### How It Ties Into the Existing System

The dedicated robot mechanism reuses the same infrastructure as the normal material handling cycles:

| Concept | Normal Cycle | Changeover |
|---------|-------------|------------|
| Complex orders with wait | Used in sequential/two-robot/single-robot cycles | Used for dedicated staging robots |
| `order.release` / `HandleOrderRelease` | Operator presses RELEASE on canvas | Core releases via `ReleaseOrder` with swap blocks |
| `stepsToBlocks()` | Converts steps to RDS blocks | Same function builds swap blocks |
| `StagedOrderRequest.Vehicle` | Not used (fleet picks robot) | Used to target specific robot |
| `fleet.Backend.CreateStagedOrder` | Creates incremental RDS order | Same call, now with Vehicle field |
| Order completion tracking | Engine tracks via `EventOrderCompleted` | Same events feed changeover trackers |

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
| `ChangeoverConfig` | New struct: `ToolAccess bool`, `DedicatedRobots int` |
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
| `handleChangeoverClearing(evt)` | Creates store orders to clear old bins |
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
| `/api/changeover/start` | Now accepts `tool_access` (bool) and `dedicated_robots` (int) |

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
| Start form | Now includes Tool Access checkbox and Dedicated Robots field |
| Progress indicators | Per-phase completion counts (X of Y bins staged, etc.) |

**Rewritten: `www/static/js/pages/changeover.js`**

| Function | Purpose |
|----------|---------|
| `startChangeover()` | Sends `tool_access` and `dedicated_robots` with start request |
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

## Open Issues and Future Work

### 1. Idle Auto-Detect for Parts Done
The `Machine` struct has `idleSeconds` and `idleCallback` fields stubbed but not wired. The intent: if no PLC counter activity for N minutes, auto-signal parts done. Requires subscribing to counter events and tracking last delta timestamp per line.

### 2. Changeover Order Tracking Gap
When Core creates swap orders via `HandleChangeoverExecute`, the order IDs are returned in the ack. However, Edge doesn't currently populate the executing tracker with these IDs — the tracker was created empty before the ack arrives. The ack handler logs the orders but doesn't wire them into the tracker. Fix: on ack receipt, populate the tracker with the order UUIDs and match them to incoming `order.delivered` events.

### 3. Cancellation During Staging
If the operator cancels while staging orders are in flight, those orders continue executing (robots still deliver to staging). The cancel handler resets the machine state but doesn't abort the in-flight staging orders. Fix: on cancel, iterate the staging tracker's order IDs and call `AbortOrder` for each.

### 4. Cancellation During Executing
Similar to above — if cancelled during executing, swap orders continue. The dedicated robots keep running their swap chains. Fix: abort all tracked executing orders on cancel.

### 5. Restore on Restart
`Machine.Restore()` rebuilds state from the last changeover log entry, but it doesn't restore tracker state. If Edge restarts during staging or executing, the trackers are lost and the phase will never auto-advance. Fix: on restore, re-query active orders and rebuild trackers.

### 6. Two-Robot Cycle Mode in Changeover
The `buildSwapStepsProto` and `buildSwapStepsResolved` functions currently use the sequential pattern (old out → new in) regardless of cycle mode. The two-robot mode would need two separate robots per bin position, which conflicts with the multi-bin-per-robot dedicated robot model. Decision needed: should changeover always use sequential swap regardless of payload cycle mode?

### 7. Clearing Destination
Clearing orders use `CreateStoreOrder` which lets Core decide the destination. If the supermarket is full or the bins need to go to a specific return area, the operator has no way to specify this. May need a configurable clearing destination per payload.

### 8. Edge README Inaccuracy
The Edge README still says Stopping "cancels active orders." The actual behavior is that Stopping only disables auto-reorder and lets in-flight orders complete naturally. The operator keeps running parts. The README should be updated.

### 9. Wire Protocol Documentation
The `changeover.execute` and `changeover.execute_ack` subjects need to be added to `docs/wire-protocol.md` along with their payload schemas.

### 10. Changeover Log Schema
The changeover log stores `state` as a string. With the parallel track model, the logged "state" is the line phase, not a complete representation of both tracks. The staging track completion is logged as a detail on the current line phase. This is adequate for audit purposes but doesn't allow perfect state reconstruction from logs alone.
