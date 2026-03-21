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
1. Cancel ALL active orders on this line — loops `ListActiveOrdersByLine(lineID)` and calls `AbortOrder(orderID)` for each. `AbortOrder` handles both local state transition (→ cancelled) and sending `order.cancel` to Core via Kafka. All statuses are cancelled (pending, submitted, acknowledged, in_transit, staged). Errors are logged per-order but don't block the loop. Abort reason: `"changeover initiated"`.
2. Auto-reorder is **suppressed** (not disabled) — a guard in `handlePayloadReorder` skips reorder dispatch while changeover is active on the line. Per-payload `auto_reorder` flags in the DB are never modified by the changeover system.
3. Emit `EventChangeoverActive{LineID, Active: true}` so the operator canvas StatusBar toggle greys out
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
2. Emit `EventChangeoverActive{LineID, Active: false}` — canvas un-greys the auto-reorder toggle
3. Auto-reorder suppression guard lifts — the new style's payloads use their pre-configured per-payload auto-reorder flags. The changeover system never modifies these flags.
4. Emit `ChangeoverCompleted` event
5. Normal operations resume — payloads with `auto_reorder = true` will trigger reorders on PLC counter crossings as usual

**Cancelled** — Operator cancels an in-progress changeover:
- Cancel is a redirect, not just a stop. The operator must select what to change over into next (see Problem 9 in open-problems.md):
  - **Different style (C):** Stranded bins continue to storage. New changeover A → C starts immediately.
  - **Same style (A):** Stranded bins return to lineside. Line resumes running style A.
- Auto-reorder flags are never modified — the suppression guard simply lifts when changeover ends
- Emit `EventChangeoverActive{LineID, Active: false}` — canvas un-greys the auto-reorder toggle

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
| `ChangeoverSwapItem` | Edge → Core | One bin position to swap (payload code, lineside, staging, outgoing, cycle mode, role, **remaining_uop** — actual UOP remaining at this position from Edge's PLC-decremented count) |
| `ChangeoverExecuteRequest` | Edge → Core | Line ID, station, dedicated robot count, list of swap items |
| `ChangeoverExecuteAck` | Core → Edge | Order count, per-order info (order ID, UUID, robot ID, payload codes) |
| `ChangeoverOrderInfo` | Core → Edge | Details of one swap order (which robot, which payloads) |
| `ChangeoverRobot` | Internal (Core) | Robot ID + current node (used in Core's robot lookup) |

**Modified existing structs** in `payloads.go` (UOP remaining sync — see Problem 7 in open-problems.md):

| Struct | Change | Purpose |
|--------|--------|---------|
| `OrderDelivered` | Add `UOPRemaining int` and `BinLabel string` fields (`omitempty`) | Core → Edge: actual bin contents on delivery, so Edge doesn't blindly reset to full capacity |

### Shingo Core (`shingo-core/`)

**Modified: `engine/wiring.go`**

| Change | Detail |
|--------|--------|
| `handleOrderDelivered` | Look up `order.BinID` → `GetBin` → populate `OrderDelivered.UOPRemaining` and `BinLabel` before sending to Edge |

**New file: `dispatch/changeover.go`**

| Function | Purpose |
|----------|---------|
| `HandleChangeoverExecute(env, req)` | Main entry point. Finds staged robots, distributes swap items, **updates `bins.uop_remaining` from each swap item's `RemainingUOP` before releasing**, dispatches |
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
| `/api/line/auto-reorder` | POST | `apiToggleLineAutoReorder` | Per-line bulk auto-reorder toggle (public, no admin auth). Accepts `{"line_id": int, "enabled": bool}`. Looks up line's `ActiveJobStyleID`, calls `SetAutoReorderByJobStyle`, emits `EventAutoReorderChanged`. Returns `{"status":"ok", "affected": N, "enabled": bool}` |

**Modified API** in `www/handlers_api_config.go`:

| Endpoint | Change |
|----------|--------|
| `/api/changeover/start` | Now accepts `tool_access` (bool), `clearing_strategy` (string), `clearing_robots` (int), and `dedicated_robots` (int) |

**Modified: `www/handlers_changeover.go`**

| Change | Detail |
|--------|--------|
| `handleChangeover` | Updated for new `Info()` return signature (6 values instead of 4) |
| Template data | Passes staging/clearing/executing progress, tool access flag, staging/line done booleans |

**New DB queries** in `store/payloads.go`:

| Function | Purpose |
|----------|---------|
| `SetAutoReorderByJobStyle(jobStyleID, autoReorder)` | Batch toggle auto-reorder for all payloads in a job style. Returns `(int64, error)` — rows affected. Single `UPDATE payloads SET auto_reorder=?, updated_at=datetime('now') WHERE job_style_id=?` |

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

## UOP Remaining Sync (Partial Bin State)

Changeover is the first case where a partially consumed bin re-enters storage and later comes back out. Normal cycles hide this gap because bins always start full and end empty. See Problem 7 in `changeover-open-problems.md` for full design.

### The Gap

Edge tracks real-time consumption via PLC counter deltas (`payload.remaining`). Core tracks bin location but never receives the updated remaining (`bins.uop_remaining` stays at whatever was set on initial load). The two views diverge as soon as the first PLC tick fires.

### Round-Trip Fix

**Edge → Core (bin leaving lineside during swap):**
```
Edge builds ChangeoverSwapItem:
  payload_code: "WIDGET-A"
  lineside_node: "LINE1.ST3"
  staging_node: "STAGE.3"
  remaining_uop: 30          ← NEW: from payload.Remaining (PLC-decremented)

Core receives changeover.execute:
  For each swap item:
    1. Find bin at lineside_node
    2. db.RecordBinCount(binID, item.RemainingUOP, "changeover-swap")
    3. Now bin carries correct remaining when stored
```

**Core → Edge (bin arriving at lineside, any delivery):**
```
Core sends order.delivered:
  order_uuid: "abc-123"
  delivered_at: "2026-03-21T14:00:00Z"
  uop_remaining: 30          ← NEW: from bin.UOPRemaining (looked up via order.BinID)
  bin_label: "BIN-0042"      ← NEW: for operator reference

Edge receives order.delivered:
  Stores uop_remaining on order record
  When handleOrderCompleted fires:
    resetPayloadOnRetrieve uses delivered uop_remaining instead of catalog capacity
    If uop_remaining == 0 or not provided: fall back to catalog capacity (backward compat)
```

**FIFO retrieval ordering:** Partial bins retain their original `loaded_at` timestamp from when they were first loaded. They're the oldest bins in storage. FIFO naturally pulls them first. No changes needed to retrieval algorithms.

### What This Fixes Beyond Changeover

The `OrderDelivered.UOPRemaining` field fixes the general case: any delivery of a partial bin (manual store → re-retrieve, quality hold → return, etc.) correctly sets Edge's payload remaining. The changeover-specific `ChangeoverSwapItem.RemainingUOP` field handles the outbound sync during swaps.

---

## Implementation Order

**First slice (designed 2026-03-21, revised same day — see Problem 11 in open-problems.md):**
1. `store/payloads.go` — add `SetAutoReorderByJobStyle` bulk method (for canvas toggle, NOT used by changeover lifecycle)
2. `engine/events.go` — add `EventChangeoverActive` + `EventAutoReorderChanged` constants and structs
3. `engine/changeover.go` — NEW FILE: `handleChangeoverStarted` (cancel all active orders + emit changeover-active), `handleChangeoverCancelled` (emit changeover-active=false), `handleChangeoverCompleted` (emit changeover-active=false)
4. `engine/wiring.go` — wire 3 changeover event subscriptions + add reorder suppression guard in `handlePayloadReorder` (skip if changeover active on the line)
5. `www/sse.go` — add `EventChangeoverActive` → `"changeover-active"` + `EventAutoReorderChanged` → `"auto-reorder-update"` SSE cases
6. `www/handlers_api_config.go` + `www/router.go` — `POST /api/line/auto-reorder` toggle endpoint
7. `www/static/operator-canvas/render.js` — StatusBar auto-reorder toggle with 3 visual states (ON/OFF/disabled-during-changeover) + `hitStatusBarButton` export
8. `www/static/operator-canvas/display.js` — click handler (disabled during changeover), SSE listeners for both events, cursor update

**Second slice: UOP remaining sync (designed 2026-03-21 — see Problem 7 in open-problems.md):**
9. `protocol/payloads.go` — add `RemainingUOP` to `ChangeoverSwapItem`, add `UOPRemaining` + `BinLabel` to `OrderDelivered`
10. `shingo-core/engine/wiring.go` — modify `handleOrderDelivered` to look up bin UOP and populate `OrderDelivered`
11. `shingo-edge/messaging/edge_handler.go` — pass `UOPRemaining` through from delivered message
12. `shingo-edge/engine/wiring.go` — modify `resetPayloadOnRetrieve` to use delivered UOP when available
13. `shingo-core/dispatch/changeover.go` — in `HandleChangeoverExecute`, update `bins.uop_remaining` from swap item before releasing

**Remaining (after first and second slices):**
14. Revise state machine (changeover/types.go) — parallel tracks, ChangeoverConfig
15. Rewrite machine (changeover/machine.go) — dual-track with convergence gate
16. Implement Staging logic (staging orders + dedicated robot wait + tracker)
17. Implement Clearing logic — both strategies:
    a. `direct`: store orders per bin, track completion
    b. `sweep_to_stage`: Phase A complex chains, Phase B background store orders
18. Implement Executing logic (changeover.execute message, Core handler, swap block building)
19. Implement completion logic (update job style)
20. Update changeover UI (parallel track display, strategy selection, contextual buttons)
21. Testing: full changeover cycle end-to-end (all scenarios)

---

## Operator Screen Context Switch

When a changeover completes and `ActiveJobStyleID` switches, operator terminals need to display the correct screen for the new style. The operator canvas uses the auto-generated path (`/operator/cell/{lineID}`), which builds a layout from the active style's payloads via `generateDefaultLayout()`. Today it handles changeover via a full `location.reload()` on the `changeover-update` SSE event. This works but is disruptive on shop floor HMIs.

**Note:** The codebase contains a drag-and-drop designer and saved-screen system (`operator_screens` table, `/operator/designer`, `/operator/display/{id}`) carried over from andon v4. This is scaffolding — the auto-generated path is the intended architecture. The designer code should not be extended for changeover integration.

### Layout Push on Changeover (replaces page reload)

In `handleChangeoverCompleted`, after updating `ActiveJobStyleID`:
1. Load the new style's payloads via `ListPayloadsByJobStyle(newJobStyleID)`
2. Build a new layout via `generateDefaultLayout(db, line, payloads)`
3. Emit `EventScreenChange{LineID, Layout}`
4. SSE broadcasts `screen-change` to connected canvas clients watching that line
5. `display.js` receives the event, replaces its shapes array, rebuilds its index maps (`payloadShapeMap`, `lineShapeMap`, `orderShapeMap`), and the existing render loop picks up the new shapes on the next frame — no page reload, no dropped SSE connection
6. Fall back to full `location.reload()` if the SSE push fails

### Sub-Station Registry (future enhancement)

```sql
CREATE TABLE sub_stations (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL,
    line_id      INTEGER NOT NULL REFERENCES production_lines(id),
    display_type TEXT NOT NULL DEFAULT 'canvas',
    last_seen_at TEXT,
    created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);
```

Sub-stations are physical operator terminals (tablets, HMI panels) associated with a process. They self-register on SSE connect by passing line ID in the query param. Enables: admin visibility into which terminals are online, future push-to-terminal features.

### Operator Transparency

The StatusBar already renders `"{lineName} — {styleName}"`. When the layout updates via SSE, the style name updates with it. Manual override is URL-based — the operator navigates to `/operator/cell/{differentLineID}` and the layout push only fires for the line the terminal is watching.

See `changeover-open-problems.md` Problem 17 for the full problem statement.

---

## Open Issues

### 1. Idle Auto-Detect for Parts Done
The `Machine` struct has `idleSeconds` and `idleCallback` fields stubbed but not wired. The intent: if no PLC counter activity for N minutes, auto-signal parts done. Requires subscribing to counter events and tracking last delta timestamp per line.

### 2. Changeover Order Tracking Gap
When Core creates swap orders via `HandleChangeoverExecute`, the order IDs are returned in the ack. However, Edge doesn't currently populate the executing tracker with these IDs — the tracker was created empty before the ack arrives. Fix: on ack receipt, populate the tracker with the order UUIDs.

### 3. Cancellation During Staging
If the operator cancels while staging orders are in flight, those orders continue executing. Fix: on cancel, iterate the staging tracker's order IDs and call `AbortOrder` for each. Note: changeover start already cancels all active orders on the line (decided 2026-03-21) — this issue is about orders created *by the changeover itself* (staging orders) that need to be aborted if the operator cancels mid-changeover.

### 4. Cancellation During Executing
Similar to above — if cancelled during executing, swap orders continue. Dedicated robots keep running swap chains. Fix: abort all tracked executing orders on cancel. Aborting mid-swap is dangerous (robot holding a bin with nowhere to go) — may need to let current bin complete, then cancel remaining.

### 5. Restore on Restart
`Machine.Restore()` rebuilds state from the last changeover log entry, but doesn't restore tracker state. Fix: on restore, re-query active orders and rebuild trackers.

### 6. Two-Robot Cycle Mode in Changeover
The swap step builders currently use the sequential pattern regardless of cycle mode. Two-robot mode would need two robots per bin position, conflicting with the multi-bin-per-robot dedicated model. Decision needed: should changeover always use sequential swap?

### 7. Edge README Inaccuracy
~~The Edge README still says Stopping "cancels active orders." Actual behavior: Stopping only disables auto-reorder and lets in-flight orders complete naturally.~~ **Resolved (2026-03-21):** The design now cancels all active orders on changeover start. The README description is correct.

### 8. Wire Protocol Documentation
The `changeover.execute` and `changeover.execute_ack` subjects need to be added to `docs/wire-protocol.md`.

### 9. Changeover Log Schema
The changeover log stores `state` as a string representing the line phase, not both tracks. Adequate for audit but doesn't allow perfect state reconstruction from logs alone.

### 10. Per-Line Auto-Reorder Toggle on Operator Canvas
The operator canvas needs a per-line toggle for auto-reorder. Decided (2026-03-21): render a button in the StatusBar element (right side, 160px wide). Three visual states: green ON, red OFF, grey disabled-during-changeover. During active changeover, toggle is greyed out and non-interactive — communicates to operators that auto-reorder is suppressed. Clicks POST to `/api/line/auto-reorder` which bulk-toggles all payloads on the line's active job style via `SetAutoReorderByJobStyle`. Updates propagate via SSE. See `changeover-open-problems.md` Problem 10 for full design.

### 11. Engine Changeover Handler Wiring (First Slice)
Three engine event handlers need to be wired as the first slice of changeover automation. Decided (2026-03-21, revised same day): new file `engine/changeover.go` with `handleChangeoverStarted` (cancel all active orders + emit changeover-active), `handleChangeoverCancelled` (emit changeover-active=false), `handleChangeoverCompleted` (emit changeover-active=false). Auto-reorder flags are NEVER modified by changeover — instead, a guard in `handlePayloadReorder` suppresses reorder dispatch while changeover is active. See `changeover-open-problems.md` Problem 11 for full implementation plan.

### 12. UOP Remaining Sync (Partial Bin State)
Changeover revealed a bin state sync gap: Edge tracks real-time consumption via PLC but never tells Core. Core's `bins.uop_remaining` is stale. When partial bins return to storage and are later re-retrieved via FIFO, Edge blindly resets remaining to full capacity. Decided (2026-03-21): add `RemainingUOP` to `ChangeoverSwapItem` (Edge → Core during swap), add `UOPRemaining` + `BinLabel` to `OrderDelivered` (Core → Edge on any delivery), modify `resetPayloadOnRetrieve` to use actual remaining. See `changeover-open-problems.md` Problem 7 and the "UOP Remaining Sync" section above for full design.

### Open Questions

1. ~~**Release strategy during Executing:** Release each station individually, or "release all" at once?~~ **Decided (2026-03-21):** Release all at once. Dedicated robots are already parked at staging — no reason to stagger. The fleet backend handles concurrent robot movement natively. Sequential release would just extend cell idle time.
2. ~~**Staging order failure:** Retry, skip and flag, or block changeover?~~ **Decided (2026-03-21):** Skip and flag. If 1 of N staging orders fails, the rest proceed. The failed payload is marked in the tracker and the operator is notified with the failure reason so operations can react (manually stage the bin, reassign, etc.). During Execute, that payload gets a standard individual swap order instead of using a dedicated robot. Blocking the entire changeover on a single staging failure is too conservative. Automatic retry is risky — if the failure is systemic (bin mismatch, node offline), retrying loops.
3. ~~**Swap failure during Executing:** Retry, continue with other stations, or pause?~~ **Decided (2026-03-21):** Other robots continue — swap failures are isolated per robot. The failed robot's swap is paused and the operator is notified. Tech/support responds to the robot failure using the existing order failure workflow (retry failed, force complete, or manual intervention). Each robot's swap chain is a sequential delivery — the failure handling method is already defined in the normal order lifecycle. The changeover doesn't pause globally for one robot's problem.
4. ~~**Payloads without staging nodes:** Deliver directly to lineside during Executing?~~ **Decided (2026-03-21):** Yes. If a payload has no staging node configured, the bin can't be pre-staged. During Execute, it gets a standard retrieve order (pickup from source → deliver to lineside). Slower than a dedicated robot swap but functional. The swap item carries `staging_node: ""` and Core creates a simple retrieve order instead of appending blocks to a staged order.
5. ~~**Operator canvas:** Show changeover status indicator?~~ **Decided (2026-03-21):** StatusBar gets auto-reorder toggle. Changeover banner already exists. Further operator visibility (Problem 5) is an enhancement.
6. ~~**New-style payload creation:** Must payloads exist before changeover starts, or auto-create from job style?~~ **Decided (2026-03-21):** Payloads must exist before changeover starts. No auto-creation. Payloads require engineering decisions — cycle mode, staging nodes, reorder points, outgoing destinations — that cannot be guessed. Job styles and their payloads are set up by an engineer on the Setup page before a changeover to that style is attempted. Problem 15 (target style validation) enforces this by rejecting changeover start if the target style has no payloads configured.
7. ~~**Counter reset:** Does `resetPayloadOnRetrieve` handle this, or does changeover need a separate reset?~~ **Decided (2026-03-21):** `resetPayloadOnRetrieve` is modified to use actual bin remaining from `OrderDelivered.UOPRemaining` when available, falling back to catalog capacity for full bins. No separate changeover reset needed — the delivery path handles both full and partial bins. See UOP Remaining Sync section.
8. ~~**Cancel during sweep_to_stage clearing:** Bins stranded at clearing nodes — return-to-lineside or let operator decide?~~ **Decided (2026-03-21):** Cancel is a redirect — operator must select the next style. If same style → bins return to lineside. If different style → bins continue to storage. Cancel API extended to accept `next_style`. See `changeover-open-problems.md` Problem 9 for full design.
