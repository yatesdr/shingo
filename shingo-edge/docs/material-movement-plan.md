# Material Movement Plan

## Overview

A unified material movement system that handles both routine replenishment and changeovers using the same underlying order construction patterns.

## Core Principle

The order construction patterns are the same for routine replenishment and changeovers. A replenishment is a single-node material movement. A changeover is the same movement executed across all affected nodes, gated by phases. Build the patterns once, use them everywhere.

## Schema Changes

Add to `style_node_claims`:
- `staging_node TEXT` — where inbound material stages before delivery
- `release_node TEXT` — where outbound material goes on release
- `evacuate_on_changeover INTEGER` — force evacuation even if payload matches between styles

## Order Construction Patterns

Four reusable functions in `engine/material_orders.go`, returning `[]protocol.ComplexOrderStep`:

### 1. buildDeliverSteps(claim)
Pickup from storage/group → dropoff at core_node_name. First fill or post-evacuation reload.

### 2. buildReleaseSteps(claim)
Pickup from core_node_name → dropoff at release_node. Evacuate or remove finished goods.

### 3. buildSingleSwapSteps(claim)
One robot does the full sequence:
1. Pickup new from storage → dropoff at staging_node
2. Wait (for release signal)
3. Pickup old from core_node_name → dropoff at release_node
4. Pickup new from staging_node → dropoff at core_node_name

### 4. buildTwoRobotSwapSteps(claim)
Returns TWO order step lists (two separate orders edge coordinates):
- Order A: pickup new → dropoff at staging_node → wait → pickup from staging_node → dropoff at core_node_name
- Order B: pickup old from core_node_name → dropoff at release_node

Edge coordinates: Order B clears the node, then releases Order A to deliver.

## Routine Replenishment

### Consume Nodes
- Counter delta → decrement remaining_uop on runtime state
- At reorder_point with auto_reorder=true → construct order using claim's swap pattern
- With auto_reorder=false → set material_status="reorder_needed", operator triggers manually

### Produce Nodes
- Counter delta → increment remaining_uop
- At uop_capacity (bin full) → trigger relief: release full bin, deliver empty bin
- Same auto vs manual modes

Both use the same 4 order patterns. The only difference is what triggers them.

## Changeover

### Claim Diff

When changeover is initiated (from-style → to-style), diff the two styles' claims at each physical node:

| From Claim | To Claim | Situation | Work |
|---|---|---|---|
| Payload A | Payload A, no evacuate | `unchanged` | Nothing |
| Payload A | Payload A, evacuate=true | `evacuate` | Release → reload |
| Payload A | Payload B | `swap` | Stage new → release old → deliver new |
| Payload A | (none) | `drop` | Release old |
| (none) | Payload B | `add` | Deliver new |

### Per-Node Task Progression

```
stage → runout → release → evacuate → load → complete
```

Not every node goes through every state:
- `unchanged` → immediately `complete`
- `add` → skips to `load`
- `drop` → ends at `release`
- `swap` → full progression
- `evacuate` → release → evacuate → load → complete

### Phase Gates

The process's changeover flow (configurable) gates when nodes can advance:
- `runout` — all consume nodes run down old material
- `tool_change` — ALL nodes must be evacuated before proceeding (for tool access)
- `release` — staged material released into production nodes
- `cutover` — production officially switches to new style
- `verify` — operator confirms everything is correct

Within a phase, each node progresses independently (rolling changeover). Operators advance phases from the changeover tab or operator panels.

### Order Construction

Each changeover node task uses the same buildDeliverSteps/buildReleaseSteps/buildSingleSwapSteps/buildTwoRobotSwapSteps functions. The changeover engine calls them at the right time based on phase and node task state.

## Implementation Sequence

### Step 1: Schema + Claim Fields + UI
Add staging_node, release_node, evacuate_on_changeover to style_node_claims. Update claim modal with new fields.

### Step 2: Extract Order Patterns
Create engine/material_orders.go with 4 step-builder functions. Refactor existing requestNodeSingleRobot/TwoRobot/Sequential to call them.

### Step 3: Counter-Driven UOP Decrement + Auto-Reorder
Wire EventCounterDelta to decrement remaining_uop. Trigger auto-reorder at threshold.

### Step 4: Produce Node Relief
New RequestNodeRelief function + counter-driven auto-relief at capacity.

### Step 5: Changeover Claim-Diff + Node Task Generation
Rewrite changeover initiation to diff style_node_claims and generate node tasks with correct movement patterns.

### Step 6: Changeover Tab UI
Refine the changeover tab to show node task states and allow per-node phase advancement.

## What Exists vs What's New

### Reuse as-is:
- ComplexOrderStep protocol, CreateComplexOrder() manager, order.release signal
- Changeover state machine (phases, station tasks, node tasks)
- Configurable changeover flow per process
- handleNodeOrderCompleted wiring for order lifecycle
- All store CRUD

### Extract from existing:
- Step-building logic from requestNodeSingleRobot/TwoRobot/Sequential → pure functions

### New code:
- 3 columns on style_node_claims
- engine/material_orders.go — 4 step-builder functions
- Counter-driven UOP decrement + auto-reorder trigger in wiring
- Produce node relief function
- Changeover initiation rewrite to diff claims
- UI updates
