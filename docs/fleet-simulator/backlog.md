# Scenarios to Test Next

Prioritized backlog of untested situations. Each item is tagged with the domain file it would land in when implemented. Write tests in the same format as existing TCs (Scenario / Expected behavior / Result).

---

## Bin reservation problems (highest priority)
> **Domain:** `bin-reservation.md`

A reservation ("claim") bug means the system's record of which bins are committed to which orders doesn't match reality. These are the most dangerous because they can cause robots to arrive at empty locations or bins to become permanently stuck.

**TC-26: Operator cancels an order — does the bin reservation release?** Largely covered by TC-23b (cancel transfers claim to return order). Remaining gap: a standalone cancel with no return order — verify the claim is released without transfer. Low priority since the cancel handler calls `UnclaimOrderBins` unconditionally.

**TC-27: Redirect preserves bin reservation.** COVERED — see TC-71c (in concurrency.md).

**TC-29: Operator cancels while the robot is in transit.** The robot is already moving with the bin. The operator cancels. The reservation should release cleanly even though the robot hasn't arrived yet. Partially covered by TC-23b (cancel with robot in flight), but TC-29 would test the cancel → return → re-claim chain with the robot at RUNNING state specifically.

**TC-32: Bin sits at staging too long — what happens to the reservation?** A bin has been at a staging area past its expiry time. The system releases the staging status. But if that bin was reserved by an active order, does the reservation also get cleaned up? Or is it left dangling?

**TC-33: Operator manually moves a reserved bin.** An operator requests a manual move on a bin that is reserved by an active order. Should the system block the move? Release the reservation? Allow both and hope for the best?

**TC-35: planMove dispatches robot with no bin.** A move order targets a lineside node with no bins, and no `payloadCode` is specified. `planMove` skips the bin-finding loop entirely (empty node, no payload filter) and dispatches with `BinID=nil`. The order should fail with a "no available bin" error, matching `planStore`'s guard. Same ghost robot class as TC-23c and TC-34. Lower likelihood since move orders typically specify a payload, but the code path exists.

**TC-39: Cross-line poaching of producer empty bins.** Producer Line A clears a bin (manifest cleared, `payload_code = ''`, `status = 'available'`). The empty bin sits at Line A's lineside node. Before Line A's operator requests a replacement empty, Line B's auto-reorder for an empty bin fires. `FindEmptyCompatibleBin` finds Line A's empty bin (no node-type filter, no ownership check). Line B's order claims it. Robot takes Line A's empty to Line B. Line A is starved for empties. Empties at lineside nodes should be invisible to cross-line `retrieve_empty` orders. `FindEmptyCompatibleBin` should exclude bins at lineside/production nodes, or producer nodes should have an affinity model for their own empties. Production risk: producer starvation. A busy floor with multiple producer lines sharing compatible bin types will see this regularly during peak periods. The zone preference mitigates it partially but does not prevent cross-zone fallback poaching.

---

## Timing and race conditions
> **Domain:** `core-dispatch.md`

**TC-6: Operator cancels at the exact moment the fleet accepts the order.** The cancel and the fleet acceptance happen at nearly the same time. Does the system end up in a clean state, or does the order get stuck — the fleet thinks it's active, the system thinks it's cancelled?

**TC-7: Operator releases before the robot has arrived at the wait point.** The Edge sends a release command for a staged order, but the robot hasn't actually reached the waiting state yet. The release should be rejected — the robot isn't there to continue.

**TC-8: Operator accidentally sends the release command twice.** Edge sends the release twice in a row. The second release should be ignored. It should not append duplicate blocks to the robot's instructions.

---

## Bad input handling
> **Domain:** `core-dispatch.md`

**TC-11: Only available bin is at a disabled storage node.** The storage node is marked as disabled (out of service). The system should not dispatch from disabled nodes — it should report no inventory available rather than sending a robot to a node that's offline.

---

## Multi-line and inventory scenarios
> **Domain:** `bin-reservation.md`

**TC-20: Two lines run the same assembly — does one line steal from the other's staging?** Line 1 and Line 2 both assemble the same product. Line 1 has a bin staged and waiting. Line 2 requests the same part. Will the system try to pull from Line 1's staging area? It shouldn't — those bins are committed to active orders on Line 1.

**TC-22: The only available bin is in the maintenance area.** Similar to quality hold, but the bin is physically at a maintenance node. The system should skip it.

**TC-31: One order finishes and frees a bin — does the next order pick it up?** Order A completes and releases its bin. Order B has been waiting because there was no inventory. Does the fulfillment scanner detect the newly available bin and dispatch Order B automatically?

---

## Fleet behavior edge cases
> **Domain:** `core-dispatch.md`

**TC-16: Fleet reports an unknown state.** The fleet sends a state string that the system doesn't recognize. Should map to a safe default status, not crash the event pipeline.

**TC-17: Fleet reports the same state twice.** COVERED - see TC-93 (in core-dispatch.md). Robot ID idempotent no-write test covers same-status-same-robot deduplication. TC-VS-2 covers status-only idempotency.

**TC-18: Fleet reports states out of order.** The fleet says FINISHED before it ever said RUNNING. This can happen if a status poll is missed. The system should handle it gracefully and end up in the correct final state.

**TC-19: Robot completes a very short trip.** The fleet goes through CREATED → RUNNING → FINISHED in rapid succession (robot was right next to the destination). All three state changes should be processed correctly despite the speed.
