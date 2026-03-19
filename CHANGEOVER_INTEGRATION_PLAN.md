# Changeover Integration Plan

## Summary

Connect the existing changeover state machine to the unified material handling cycle system. When an operator initiates a changeover, the system automatically stages new-style bins, pauses automation, and executes the swap using the same cycle infrastructure that runs normal operations.

## What Exists Today

The changeover state machine is built and functional:
- 7 linear states: Running → Stopping → CountingOut → Storing → Delivering → CountingIn → Ready → Running
- Start / Advance / Cancel API endpoints
- UI with visual step progression
- SSE event broadcasting
- Persistence and restart recovery
- Operator canvas listens and reloads on changeover events

What's missing: the state machine tracks state but doesn't DO anything. State transitions are manual (operator clicks advance). No orders are created, no payloads are modified, no automation is paused or resumed. The states themselves don't match the actual changeover workflow.

## The Changeover Flow

1. **Operator initiates changeover** — selects new job style on the changeover page
2. **System pauses automation** — auto reorder OFF for all payloads on the line, cancel any in-transit orders for old style
3. **System stages new bins** — creates retrieve orders to deliver new-style bins to staging areas near each station
4. **All new bins staged** — system detects all staging orders are complete, signals ready
5. **Operator executes changeover** — presses a button, system runs the swap across all stations simultaneously. Old bins picked up, new bins placed from staging. This is the hot-swap cycle running in batch.
6. **Swap complete** — system updates active job style, turns auto reorder back ON, normal operations resume

The operator's role: initiate (step 1), execute (step 5). The system handles everything in between.

## Revised State Machine

The current states (CountingOut, Storing, Delivering, CountingIn) don't match this flow. Proposed revision:

```
Running → Stopping → Staging → Staged → Executing → Running
```

| State | What Happens | Who Advances |
|-------|-------------|--------------|
| **Running** | Normal operations | Operator starts changeover |
| **Stopping** | Auto reorder OFF. Cancel active orders for old style payloads on this line. | System auto-advances when all orders cancelled/completed |
| **Staging** | Create retrieve orders to deliver new-style bins to staging areas. | System auto-advances when all staging orders delivered |
| **Staged** | All new bins in position at staging. Waiting for operator. | Operator presses "Execute Changeover" |
| **Executing** | System runs hot-swap cycles across all stations — old bins picked up, new bins placed from staging. | System auto-advances when all swaps complete |
| **Running** | Active job style updated. Auto reorder ON. Normal cycle operations resume. | — |

Three of the five transitions are automatic (system detects completion). Two are operator-initiated (start changeover, execute changeover).

## What the System Does at Each State

### Stopping
**Trigger:** Operator starts changeover (selects new job style, clicks Start)

**Actions:**
1. Set `auto_reorder = false` for ALL payloads on this line's active job style
2. Cancel all active orders (retrieve, complex) for payloads on this line
3. Wait for cancelled orders to reach terminal state (confirmed/cancelled/failed)
4. Auto-advance to Staging when all orders are done

**Engine implementation:** Subscribe to `EventChangeoverStateChanged`. When new state is `Stopping`:
- Query all payloads for the line's active job style
- Set `auto_reorder = false` on each
- Cancel each active order via `orderMgr.AbortOrder()`
- Track order completion events; when all are terminal, advance the machine

### Staging
**Trigger:** System auto-advances from Stopping

**Actions:**
1. Look up the new job style's payloads (the "to" style)
2. For each payload, create a retrieve order to deliver the new-style bin to its staging node
3. Track each staging delivery
4. Auto-advance to Staged when all deliveries complete

**Key question:** Where do the new-style bins go? Each payload has a `StagingNode` configured. The staging order delivers to that node. If no staging node is configured, deliver directly to lineside (no staging step — the swap happens immediately on execute).

**Engine implementation:** When state advances to `Staging`:
- Query payloads for the new job style
- For each payload with a StagingNode: `orderMgr.CreateRetrieveOrder(retrieveEmpty based on role, deliveryNode = payload.StagingNode)`
- For each payload without a StagingNode: flag for direct delivery during Executing
- Track completions; when all staging orders are delivered, advance to Staged

### Staged
**Trigger:** System auto-advances from Staging

**Actions:** None — waiting for operator. All new-style bins are staged and ready.

**UI:** The changeover page shows "All bins staged. Ready to execute changeover." with a big "Execute Changeover" button.

**Operator canvas:** Could show a changeover status indicator — "CHANGEOVER READY" or similar.

### Executing
**Trigger:** Operator clicks "Execute Changeover"

**Actions:**
1. For each station on the line, run the swap:
   - Pick up old-style bin from lineside
   - Place new-style bin from staging to lineside
2. This is a batch of hot-swap operations running simultaneously
3. Track all swap completions
4. Auto-advance to Running when all swaps complete

**How swaps work:** For each station, the system creates a complex order:
- `pickup(lineside)` — remove old bin
- `dropoff(outgoing destination)` — take old bin to storage/return
- (If new bin is at staging) `pickup(staging)` → `dropoff(lineside)` — place new bin

Or if using the two-robot pattern: one robot picks up old, another places new, simultaneously.

**The CycleMode from the NEW style's payload config determines the swap strategy.** Sequential, two-robot, or single-robot — the same infrastructure, just triggered by changeover instead of reorder point.

**Engine implementation:** When state advances to `Executing`:
- For each new-style payload: call `RequestOrders(payloadID, 1)` — this creates the appropriate cycle based on CycleMode
- The cycle handles the swap (navigate, wait, pickup, deliver)
- Wait for operator to RELEASE each station (or release all at once?)
- Track completions; when all orders complete, advance to Running

### Running (Changeover Complete)
**Trigger:** System auto-advances from Executing

**Actions:**
1. Update the production line's `ActiveJobStyleID` to the new job style
2. Set `auto_reorder = true` for all new-style payloads on this line
3. Reset remaining counts based on new UOP capacities
4. Emit `ChangeoverCompleted` event
5. Normal operations resume — counter deltas now apply to new-style payloads

## Connection to Cycle System

The changeover reuses the same infrastructure:

| Normal Operation | Changeover |
|-----------------|------------|
| Counter crosses reorder point → `RequestOrders` | Changeover execute → `RequestOrders` for each station |
| One station at a time | All stations simultaneously |
| CycleMode determines strategy | Same CycleMode from new payload config |
| Operator RELEASE per station | Operator RELEASE all at once (or per station) |
| Reset on delivery | Reset on delivery |

The difference: normal operations trigger one station when its counter crosses the threshold. Changeover triggers ALL stations at once as a coordinated batch.

## Files That Need to Change

### changeover/types.go
- Revise states: `Running → Stopping → Staging → Staged → Executing → Running`
- Update `stateOrder`, `NextState`, `StateIndex`

### engine/wiring.go
- Subscribe to `EventChangeoverStateChanged`
- Add `handleChangeoverStateChange(event)` handler
- Implement logic for each state: Stopping (pause + cancel), Staging (create orders), Executing (run swaps)
- Track order completions during changeover (need to distinguish changeover orders from normal cycle orders)

### engine/engine.go
- May need a "changeover context" to track which orders belong to the current changeover
- Need to know when all changeover orders are complete to auto-advance

### store/payloads.go
- Need a query: `ListPayloadsByJobStyleAndLine(jobStyleID, lineID)` — get all payloads for the new style on this line
- May need a batch `UpdateAutoReorderForLine(lineID, enabled)` — toggle auto reorder for all payloads on a line

### www/templates/changeover.html
- Update step names to match new states
- Add "Execute Changeover" button for Staged state
- Show staging progress (X of Y bins staged)

### www/static/js/pages/changeover.js
- Update state-to-step mapping
- Add staging progress display
- Update button labels per state

## Open Questions

1. **Release strategy during Executing:** Does the operator release each station individually, or is there a "release all" that fires all swaps at once? A "release all" is cleaner for changeover — one button, everything swaps.

2. **What if a staging order fails?** The bin can't be staged. Options: retry, skip that station and flag it, or block the changeover until resolved.

3. **What if a swap fails during Executing?** One station's robot can't complete the swap. Options: retry, continue with other stations, or pause the changeover.

4. **Payloads without staging nodes:** For sequential mode payloads that don't have staging configured, the new bin goes directly to lineside during Executing. No pre-staging step.

5. **Changeover and the operator canvas:** Should the canvas show changeover status? "CHANGEOVER IN PROGRESS" instead of the normal cycle display?

6. **How are new-style payloads created?** If the new job style has different payload configurations (different parts, different locations), those payloads need to exist in the system before the changeover starts. Is this a setup step, or do payloads auto-create from the job style definition?

7. **Counter reset:** When the new style starts, remaining should be at UOP capacity (fresh bins). Does `resetPayloadOnRetrieve` handle this, or does the changeover need a separate reset?

## Implementation Order

1. Revise state machine states (types.go)
2. Add new DB queries for batch payload operations
3. Wire changeover events in engine (subscribe + handlers)
4. Implement Stopping logic (pause + cancel)
5. Implement Staging logic (create staging orders + track completion)
6. Implement Executing logic (batch swaps + track completion)
7. Implement completion logic (update job style, resume auto reorder)
8. Update changeover UI (new states, progress, execute button)
9. Update operator canvas for changeover awareness
10. Testing: full changeover cycle end-to-end
