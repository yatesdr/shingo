# Changeover System — Technical Specification

This document covers changeover design: clearing strategies, robot orchestration, dedicated robots, cross-module protocol changes, and open issues.

> **Architecture Note (2026-03-22):** This spec was written before the operator station redesign (`docs/edge-operator-station-redesign.md`). The v2 architecture replaces the flat line-wide changeover model with a hierarchical per-position model: ProcessChangeover → ChangeoverStationTask → ChangeoverNodeTask. The old `changeover/` package (types.go, machine.go) described in Cross-Module Changes below **does not exist** — the actual implementation lives in `engine/operator_stations.go` (orchestration), `store/changeovers_v2.go` (persistence), and `www/handlers_operator_stations_v2.go` (API).
>
> **What's still valid in this spec:** Clearing strategies (direct, sweep-to-stage), dedicated robot process, protocol messages (changeover.execute/ack), UOP remaining sync, and the general phasing concepts (staging, clearing, executing). What needs translation: all references to `changeover/machine.go`, line-wide state tracking, `operator-canvas/`, and the linear `Machine` struct should be read as applying to the per-position v2 model instead.

---

## Overview

A changeover switches a process from one job style to another. The system automates material staging, clearing, and swapping. Each operator station and position can transition at different times (rolling changeover).

**What exists today (v2 architecture):**
- Three-tier changeover tracking: ProcessChangeover → ChangeoverStationTask → ChangeoverNodeTask
- Per-position state progression: `unchanged → staging_requested → staged → empty_requested → line_cleared → release_requested → released → switched → verified`
- Start/cancel/advance APIs, per-position material staging/emptying/release actions
- Wiring integration linking order lifecycle events to changeover node task state
- Canvas-based operator station HMI with changeover context

**What still needs implementation:**
- Automated staging order creation (currently manual per-position)
- Clearing strategy execution (direct and sweep-to-stage as designed below)
- Dedicated robot process with Core-side protocol handling
- PLC counter integration with per-position remaining tracking
- Cancel flow with in-flight order redirection
- Crash recovery / state restoration

**The goal:** Connect the changeover orchestration to the material handling cycle system. When an operator initiates a changeover, the system automatically stages new-style bins per position, pauses automation, optionally clears old bins for tooling access, and executes the swap using the same cycle infrastructure that runs normal operations.

**The operator's role:** Initiate (select new style, press Start), signal readiness per station, and verify completion. The system handles material movement in between.

---

## Coding Conventions for Implementation

All changeover code must follow the established codebase patterns. These were audited across engine, store, API, and messaging layers on 2026-03-22.

### Error Handling — Three Tiers
1. **Critical path** (return error): State machine violations, validation, missing data, permission denials. Descriptive error messages.
2. **Operational side effects** (log and swallow): Runtime state updates, audit writes, health pings. Pattern: `_ = db.Update(...)` or `if err != nil { log.Printf(...) }` without returning.
3. **Data reads** (bubble up): Lookup failures return errors to caller. Nil checks guard dereferencing. Chain fails fast on missing data.

### Guard Clauses
- Every engine function opens with validation: entity exists → right state → no conflicting operation → then mutate.
- Use `ensureNodeTaskCanRequestOrder` pattern: check if order ID already linked AND whether that order is terminal before creating new orders.
- Multi-condition guards on completion (all conditions must hold, no else clause, unmatched = silent no-op).
- Idempotency: if already in target state, return nil (not error).

### State Machine
- Strict transition allowlists (`IsValidTransition`, `isNodeStateAtLeast`).
- Force transitions only for crash recovery reconciliation — log explicitly when used.
- No partial states; terminal states are final.

### Database
- **Edge (SQLite)**: Single connection (`SetMaxOpenConns(1)`), WAL mode, foreign keys on. Use `EnsureXxx` pattern for lazy row creation. `COALESCE` in JOINs. `sql.NullInt64/NullString` for scans. No ORM.
- **Core (Postgres)**: Explicit transactions with `defer tx.Rollback()` for multi-step operations (bin claims + order inserts). `RETURNING id` for inserts.
- All timestamps stored as `TEXT` (Edge) or `TIMESTAMP` (Core). Use `datetime('now')` in SQLite, `NOW()` in Postgres.

### Messaging
- All Kafka messages go through the **outbox** — persist first, drain async. Never direct-publish.
- Use existing `DataSender` (3 retries, exponential backoff) for protocol messages.
- Nil-safe `DebugLogFunc` for debug logging; `log.Printf` for operational errors.

### API Handlers
- Parse URL params with `parseID()`, return 400 on failure.
- Decode JSON with `json.NewDecoder(r.Body).Decode(&req)`, return 400 on failure.
- Validate required fields before engine calls.
- Use `writeError(w, status, msg)` for consistent `{"error": "msg"}` responses.
- Non-critical side effects (e.g., `TouchOperatorStation`) silently swallowed.

### SSE / Events
- Use `EventHub.Broadcast()` (non-blocking, drops if buffer full).
- Map engine events → SSE types in `sse.go`.
- Synchronous emission on publisher goroutine; handlers must not block.

### Thread Safety
- `sync.RWMutex` for shared config (getter: RLock + defensive copy; setter: Lock + rebuild + Unlock + emit).
- EventBus copies subscriber list under RLock before invoking callbacks.
- Respect `LaneLock` if creating orders that could conflict with reshuffles.

### Nil Safety
- Check all pointers before dereferencing. Use `sql.NullX` types for DB scans.
- Optional callbacks: `if h.callback != nil { h.callback(...) }`.
- `DebugLogFunc` is nil-safe by design.

### Prefer Simple Solutions
- Set one field at creation, check it at completion (complex order completion guard pattern).
- No new abstractions when a guard clause suffices.
- No protocol changes when a local field check works.

---

## Changeover Configuration

Set per changeover on the changeover menu (different job style transitions on the same process may need different settings):

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

> **v2 Note:** The sections below describe the original design targeting a standalone `changeover/` package. In the actual codebase, this functionality lives in: `engine/operator_stations.go` (orchestration), `store/changeovers_v2.go` (persistence), `www/handlers_operator_stations_v2.go` (API). The phase progression, clearing strategies, and protocol integration concepts still apply — they just operate on the per-position model (ProcessChangeover → StationTask → NodeTask) rather than a line-wide Machine struct.

**~~Revised~~ Original design: `changeover/types.go`** (not implemented — see v2 model above)

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

Changeover is the first case where a partially consumed bin re-enters storage and later comes back out. Normal cycles hide this gap because bins always start full and end empty. See Problem 7 in the "Open Issues" section below for full design.

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

## Implementation Plan

> **Last updated:** 2026-03-22
> **Architecture basis:** `docs/edge-operator-station-redesign.md` (v2 operator station model)

### Current State

The v2 operator station architecture provides the changeover foundation. The following is already implemented:

| Component | File | Status |
|-----------|------|--------|
| Three-tier changeover model (ProcessChangeover → StationTask → NodeTask) | `store/changeovers_v2.go` | Done |
| Per-position state progression (unchanged → ... → verified) | `engine/operator_stations.go` | Done |
| Start/cancel/advance orchestration | `engine/operator_stations.go` | Done |
| Per-position material operations (stage, empty, release, switch, verify) | `engine/operator_stations.go` | Done |
| Order events → changeover node task state updates | `engine/wiring.go` | Done |
| REST APIs for all changeover commands | `www/handlers_operator_stations_v2.go` | Done |
| Changeover dashboard page | `www/handlers_changeover.go` | Done |
| Operator station HMI with changeover context | `www/static/operator-station/hmi.js` | Done |
| Composite station views for HMI | `store/station_views.go` | Done |
| Runtime material state per position | `store/op_node_assignments.go` | Done |

### Remaining Slices

#### Slice 1: Auto-Reorder Suppression During Changeover

**Goal:** Prevent automatic material reorders while a changeover is active on a process.

**Changes:**
- `engine/wiring.go` — Add guard in the reorder handler: before dispatching a reorder, check if a non-completed/non-cancelled ProcessChangeover exists for the position's process. If yes, skip the reorder and log.
- `engine/operator_stations.go` — On changeover completion (`tryCompleteProcessChangeover`), emit an event so suppressed positions can re-evaluate their reorder state.

**Why first:** This is a safety guard that prevents conflicting orders during changeover. Low risk, high value.

#### Slice 2: Automated Staging Order Creation

**Goal:** When changeover starts, automatically create retrieve orders for each position's target-style material instead of requiring manual per-position staging.

**Changes:**
- `engine/operator_stations.go` — In `StartProcessChangeoverV2`, after creating all NodeTasks, iterate positions and for each one that has a target-style assignment with a different payload than the current: create a retrieve order for the new material to the position's staging area (staging_area_1 node binding). Update the NodeTask state to `staging_requested` and link the order ID.
- `store/changeovers_v2.go` — May need a method to bulk-update NodeTask states and order IDs.
- Wiring integration already handles: when the staging order completes/stages, the NodeTask advances to `staged`.

**Dependencies:** Requires positions to have `staging_area_1` node bindings configured and target-style assignments set up.

#### Slice 3: Clearing Strategy Execution

**Goal:** When operator signals parts done at a station, automatically clear old-style bins based on the configured clearing strategy.

**Changes:**
- `engine/operator_stations.go` — New functions: `executeClearingDirect(stationTask)` creates one store order per position (pickup lineside → dropoff outgoing_destination). `executeClearingSweep(stationTask, robotCount)` creates multi-bin complex order chains distributed across N robots to nearby clearing nodes, then Phase B background store orders.
- Clearing strategy config — Add `clearing_strategy`, `clearing_robots` fields to ProcessChangeover or a new changeover config table.
- NodeTask state progression: when clearing orders complete, advance from `empty_requested` → `line_cleared`.

**Design reference:** See "Clearing Strategies" section above for full direct vs. sweep-to-stage design.

#### Slice 4: UOP Remaining Sync

**Goal:** Ensure partial bins carry correct remaining count through storage and back.

**Changes:**
- `protocol/payloads.go` — Add `UOPRemaining int` and `BinLabel string` fields to `OrderDelivered` (both `json:",omitempty"`)
- `shingo-core/engine/wiring.go` — In `handleOrderDelivered`, look up `order.BinID` → `GetBin` → populate `UOPRemaining` and `BinLabel` before sending to Edge
- `shingo-edge/messaging/edge_handler.go` — Pass `UOPRemaining` through from delivered message to order record
- `shingo-edge/engine/wiring.go` — Modify slot/position reset to use delivered UOP when available instead of blindly resetting to catalog capacity

**Design reference:** See "UOP Remaining Sync" section above.

#### Slice 5: Protocol + Core Changeover Handler

**Goal:** Core-side handling of changeover execute requests — find staged robots, build swap blocks, release.

**Changes:**
- `protocol/types.go` — Add `SubjectChangeoverExecute`, `SubjectChangeoverExecuteAck` constants
- `protocol/payloads.go` — Add `ChangeoverSwapItem`, `ChangeoverExecuteRequest`, `ChangeoverExecuteAck`, `ChangeoverOrderInfo` structs
- `shingo-core/dispatch/changeover.go` — New file: `HandleChangeoverExecute`, `releaseWithSwapBlocks`, `createIndividualSwapOrder`, swap step builders
- `shingo-core/messaging/core_handler.go` — Add `SubjectChangeoverExecute` case in HandleData
- `shingo-core/store/orders.go` — `ListStagedOrdersByStation(stationID)` query
- `protocol/ingestor.go` — Register new message types

**Design reference:** See "Message Flow: Execute Phase" and "Cross-Module Changes" sections above.

#### Slice 6: Dedicated Robot Process

**Goal:** Last N staging robots stay at staging area (complex orders with wait), then Core appends swap blocks on execute.

**Changes:**
- `engine/operator_stations.go` — When creating staging orders, the last N orders are created as complex orders with a final `wait` step (robot parks at staging). Track these as "dedicated" in the NodeTask.
- `fleet/fleet.go` — Ensure `StagedOrderRequest.Vehicle` field exists for targeting specific robots
- `fleet/seerrds/adapter.go` — Map `Vehicle` field through to RDS API
- Core `dispatch/changeover.go` — On execute, find staged orders, distribute swap items across dedicated robots, append swap blocks, release

**Dependencies:** Slice 5 (protocol + Core handler) must be done first.

#### Slice 7: Cancel Flow

**Goal:** Cancel as redirect — operator selects what to changeover into next. In-flight changeover orders are aborted.

**Changes:**
- `engine/operator_stations.go` — Enhance `CancelProcessChangeoverV2` to:
  1. Accept a `next_style_id` parameter (same style = revert, different style = redirect)
  2. Abort all in-flight changeover orders (staging, clearing, swap) by iterating NodeTasks with linked order IDs
  3. If reverting to same style: return stranded bins to lineside positions
  4. If redirecting to new style: stranded bins continue to storage, start new changeover
- API — Update cancel endpoint to accept next_style_id

#### Slice 8: Crash Recovery

**Goal:** Restore changeover state from DB on restart, rebuild tracking from active orders.

**Changes:**
- `engine/operator_stations.go` or new `engine/changeover_restore.go` — On engine startup, query for non-completed ProcessChangeovers. For each: load StationTasks and NodeTasks, check linked order states, rebuild any tracking state.
- Handle edge cases: orders that completed while Edge was down (check order table states), orders that are still in-flight (resume tracking).

#### Slice 9: Enhanced Changeover Dashboard

**Goal:** Full monitoring UI showing per-station and per-position changeover progress.

**Changes:**
- `www/handlers_changeover.go` — Enhance template data with station/position progress details, clearing status, phase timings
- `www/templates/changeover.html` — Per-station progress cards, per-position state indicators, phase timeline
- SSE integration — Real-time updates as positions progress through changeover states

### Slice Dependencies

```
Slice 1 (auto-reorder suppression) ─── no dependencies, do first
Slice 2 (automated staging) ─── no dependencies
Slice 3 (clearing strategies) ─── no dependencies
Slice 4 (UOP sync) ─── no dependencies (protocol change)
Slice 5 (protocol + Core) ─── Slice 4 (uses UOP fields)
Slice 6 (dedicated robots) ─── Slice 5 (needs Core handler)
Slice 7 (cancel flow) ─── Slice 2 + 3 (needs orders to cancel)
Slice 8 (crash recovery) ─── Slice 2 + 3 + 6 (needs full order flow to restore)
Slice 9 (dashboard UI) ─── no hard dependencies, can start anytime
```

**Recommended order:** 1 → 2 → 4 → 3 → 5 → 6 → 7 → 8 → 9

Slice 1 is the quickest safety win. Slice 2 unlocks the automated changeover flow. Slice 4 is a protocol change that should go in early so Core can start populating UOP data. Slices 5-6 are the Core-side work. Slices 7-8 are hardening. Slice 9 is polish.

---

## Operator Screen Context Switch

> **v2 Note:** The legacy operator canvas (`operator-canvas/`) has been removed. Operator stations now use the HMI at `/operator-station/{stationID}/hmi` which fetches composite station views. The concepts below (layout push, sub-station registry) should be adapted to the v2 station model — the HMI already has changeover context built into its position cards.

When a changeover completes and `ActiveStyleID` switches on a process, operator station HMIs need to refresh their position cards to reflect the new style's assignments. The HMI fetches a composite station view that includes current assignments and runtime state, so a re-fetch after changeover completion updates the display.

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

See the "Open Issues" section below Problem 17 for the full problem statement.

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
The operator canvas needs a per-line toggle for auto-reorder. Decided (2026-03-21): render a button in the StatusBar element (right side, 160px wide). Three visual states: green ON, red OFF, grey disabled-during-changeover. During active changeover, toggle is greyed out and non-interactive — communicates to operators that auto-reorder is suppressed. Clicks POST to `/api/line/auto-reorder` which bulk-toggles all payloads on the line's active job style via `SetAutoReorderByJobStyle`. Updates propagate via SSE. See the "Open Issues" section below Problem 10 for full design.

### 11. Engine Changeover Handler Wiring (First Slice)
Three engine event handlers need to be wired as the first slice of changeover automation. Decided (2026-03-21, revised same day): new file `engine/changeover.go` with `handleChangeoverStarted` (cancel all active orders + emit changeover-active), `handleChangeoverCancelled` (emit changeover-active=false), `handleChangeoverCompleted` (emit changeover-active=false). Auto-reorder flags are NEVER modified by changeover — instead, a guard in `handlePayloadReorder` suppresses reorder dispatch while changeover is active. See the "Open Issues" section below Problem 11 for full implementation plan.

### 12. UOP Remaining Sync (Partial Bin State)
Changeover revealed a bin state sync gap: Edge tracks real-time consumption via PLC but never tells Core. Core's `bins.uop_remaining` is stale. When partial bins return to storage and are later re-retrieved via FIFO, Edge blindly resets remaining to full capacity. Decided (2026-03-21): add `RemainingUOP` to `ChangeoverSwapItem` (Edge → Core during swap), add `UOPRemaining` + `BinLabel` to `OrderDelivered` (Core → Edge on any delivery), modify `resetPayloadOnRetrieve` to use actual remaining. See the "Open Issues" section below Problem 7 and the "UOP Remaining Sync" section above for full design.

### Open Questions

1. ~~**Release strategy during Executing:** Release each station individually, or "release all" at once?~~ **Decided (2026-03-21):** Release all at once. Dedicated robots are already parked at staging — no reason to stagger. The fleet backend handles concurrent robot movement natively. Sequential release would just extend cell idle time.
2. ~~**Staging order failure:** Retry, skip and flag, or block changeover?~~ **Decided (2026-03-21):** Skip and flag. If 1 of N staging orders fails, the rest proceed. The failed payload is marked in the tracker and the operator is notified with the failure reason so operations can react (manually stage the bin, reassign, etc.). During Execute, that payload gets a standard individual swap order instead of using a dedicated robot. Blocking the entire changeover on a single staging failure is too conservative. Automatic retry is risky — if the failure is systemic (bin mismatch, node offline), retrying loops.
3. ~~**Swap failure during Executing:** Retry, continue with other stations, or pause?~~ **Decided (2026-03-21):** Other robots continue — swap failures are isolated per robot. The failed robot's swap is paused and the operator is notified. Tech/support responds to the robot failure using the existing order failure workflow (retry failed, force complete, or manual intervention). Each robot's swap chain is a sequential delivery — the failure handling method is already defined in the normal order lifecycle. The changeover doesn't pause globally for one robot's problem.
4. ~~**Payloads without staging nodes:** Deliver directly to lineside during Executing?~~ **Decided (2026-03-21):** Yes. If a payload has no staging node configured, the bin can't be pre-staged. During Execute, it gets a standard retrieve order (pickup from source → deliver to lineside). Slower than a dedicated robot swap but functional. The swap item carries `staging_node: ""` and Core creates a simple retrieve order instead of appending blocks to a staged order.
5. ~~**Operator canvas:** Show changeover status indicator?~~ **Decided (2026-03-21):** StatusBar gets auto-reorder toggle. Changeover banner already exists. Further operator visibility (Problem 5) is an enhancement.
6. ~~**New-style payload creation:** Must payloads exist before changeover starts, or auto-create from job style?~~ **Decided (2026-03-21):** Payloads must exist before changeover starts. No auto-creation. Payloads require engineering decisions — cycle mode, staging nodes, reorder points, outgoing destinations — that cannot be guessed. Job styles and their payloads are set up by an engineer on the Setup page before a changeover to that style is attempted. Problem 15 (target style validation) enforces this by rejecting changeover start if the target style has no payloads configured.
7. ~~**Counter reset:** Does `resetPayloadOnRetrieve` handle this, or does changeover need a separate reset?~~ **Decided (2026-03-21):** `resetPayloadOnRetrieve` is modified to use actual bin remaining from `OrderDelivered.UOPRemaining` when available, falling back to catalog capacity for full bins. No separate changeover reset needed — the delivery path handles both full and partial bins. See UOP Remaining Sync section.
8. ~~**Cancel during sweep_to_stage clearing:** Bins stranded at clearing nodes — return-to-lineside or let operator decide?~~ **Decided (2026-03-21):** Cancel is a redirect — operator must select the next style. If same style → bins return to lineside. If different style → bins continue to storage. Cancel API extended to accept `next_style`. See the "Open Issues" section below Problem 9 for full design.
