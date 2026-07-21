// handler_bin_picked_up.go — Edge handler for SubjectBinPickedUp.
//
// HandleBinPickedUp processes Core's notification. It
// fires when Core observes that a robot has physically picked up a
// bin from the source location (driven by the rds.Poller's per-block
// FINISHED transition through wiring_block_completed.go on Core).
//
// SEND PARTIAL BACK is the motivating flow:
//
//  1. Operator clicks RELEASE PARTIAL on a bin that still has UOP
//     remaining. Edge marks the order in_transit, sets the bin's
//     manifest to the partial count, and fires the order to the
//     fleet.
//  2. The cell keeps cycling — PLC ticks continue against the
//     released bin's slot until the robot arrives. Each tick
//     attributes to the released bin (ActiveOrderID still points
//     at the partial-back order, BinID still points at that bin).
//  3. Robot arrives, grabs the bin. Core's rds.Poller sees the
//     pickup-block FINISH and publishes BinPickedUp to Edge.
//  4. Edge flushes the inventory delta accumulator for the released
//     bin so any in-flight ticks ship before the active claim
//     advances. The runtime's ActiveOrderID is then cleared so
//     subsequent ticks attribute to whatever lands next.
//
// SME-accepted small bias: if Edge crashes during the pickup window,
// a tick or two recorded after the physical pickup but before the
// flush may attribute to a bin that's no longer at the slot. Post-flip
// (commit 6d226d1) no reconciler exists to heal this; the miscount is
// bounded by the pickup-to-restart window and surfaces via
// FlushFailures if it ever causes a Core-side delta rejection.
package engine

import (
	"strings"

	"shingoedge/domain"
)

// HandleBinPickedUp processes a Core BinPickedUp notification.
// Best-effort — failures log and continue rather than rejecting the
// envelope. Post-flip there is no reconciler to heal silent failures;
// the FlushFailures gauge is the operational signal if attribution
// goes wrong.
//
// location is the Core node name where the pickup occurred (BinPickedUp.Location
// on the wire envelope). Used to gate the runtime-mutation branches below:
// the supply leg of a two_robot swap has one pickup at a remote source
// (supermarket) and one dropoff at our slot; the order ID matches but no
// slot pickup event ever fires. Without the gate, the supermarket
// pickup's BinPickedUp would clear active state at our slot.
func (e *Engine) HandleBinPickedUp(orderUUID string, binID int64, location string) {
	order, err := e.db.GetOrderByUUID(orderUUID)
	if err != nil || order == nil {
		// Unknown order — Edge may have GC'd a terminal order, or the
		// envelope is for a different station. Log and move on.
		e.logFn("bin_picked_up: order uuid=%s not found", orderUUID)
		return
	}

	// === Location gate (inverted; fails closed) ===
	//
	// LOAD-BEARING: this gate compares Core's ev.Location (from
	// wiring_block_completed.go:74, un-normalized RDS vendor string)
	// to Edge's ProcessNode.CoreNodeName (which IS trimmed on write
	// at store/processes/processes.go:219,259 via strings.TrimSpace).
	//
	// Trim policy at compare time:
	//   - TrimSpace(location)          — LOAD-BEARING. Core's RDS path
	//                                    doesn't normalize; admin save
	//                                    with stray whitespace on the
	//                                    vendor side would otherwise
	//                                    silently dead-code this gate.
	//   - TrimSpace(node.CoreNodeName) — defense-in-depth. Today Edge
	//                                    always trims on write; trim
	//                                    again here guards against any
	//                                    future write path that skips
	//                                    the canonical trim.
	//
	// Trim only — NOT case-fold. A case mismatch is a real config
	// error and the loud byte-mismatch is a useful failure, not
	// noise to mask.
	//
	// Failure modes treated as "couldn't verify, do nothing":
	//   - order.ProcessNodeID == nil  — kanban-only orders. F' Phase 2
	//     doesn't apply (no process node = no changeover task) and the
	//     downstream flush+clear shouldn't fire either. Fall through
	//     the gate by skipping the whole side-effecting block.
	//   - GetProcessNode error or nil — DB error or missing node. Treat
	//     as "not our slot" rather than fall open (the pre-1.1 bug
	//     class).
	//   - Empty location string       — Core didn't populate. Treat as
	//     "not our slot."
	//
	// If either side ever adds case-folding, prefix-stripping, or any
	// other transform, this gate can flip to "always reject" and the
	// flush + clear become silent dead code. Do not weaken or remove
	// this without replacing the proof end-to-end on the wire path.
	if order.ProcessNodeID == nil {
		// Kanban-only order — no slot to verify, nothing to flush /
		// clear via this handler. Logging would be noise.
		return
	}
	node, nerr := e.db.GetProcessNode(*order.ProcessNodeID)
	if nerr != nil || node == nil {
		e.logFn("bin_picked_up: order=%s — couldn't resolve process node id=%v (nerr=%v); skipping for safety",
			orderUUID, order.ProcessNodeID, nerr)
		return
	}
	if location == "" {
		e.logFn("bin_picked_up: order=%s node=%s — empty Location from Core; skipping for safety (Springfield-class regression path)",
			orderUUID, node.CoreNodeName)
		return
	}
	if strings.TrimSpace(location) != strings.TrimSpace(node.CoreNodeName) {
		e.logFn("bin_picked_up: order=%s pickup at location=%q vs CoreNodeName=%q (display name=%s) — ignoring, not our slot",
			orderUUID, location, node.CoreNodeName, node.Name)
		return
	}

	// === F' Phase 2 — deferred-supply release on evac pickup confirm ===
	//
	// Now gated; only fires at-our-slot.
	//
	// When the picked-up order is the evac leg of a changeover node
	// task (matched via task.OldMaterialReleaseOrderID = order.ID),
	// release the paired supply leg (task.NextMaterialOrderID) now
	// that the evac robot has the old bin and is moving away from the
	// slot. The evac order itself is still RUNNING (it has dropoff
	// blocks remaining at outbound) — we trigger on the pickup block's
	// FINISHED transition mid-order, NOT on evac order completion.
	// The pickup-block-done moment is the only one that matters: it
	// means the slot is physically clear for the supply robot to come
	// in. ReleaseChangeoverWait deliberately defers the supply leg at
	// click time precisely so this auto-release closes the loop
	// deterministically — no slot-collision race.
	//
	// Scoped to changeover paths only via the task lookup. Operator-
	// station two-robot paths (operator_stations.go, operator_produce,
	// operator_bin_ops) use SiblingOrderID for their own supply↔evac
	// pairing and continue to rely on operator-click release; we don't
	// touch them here.
	//
	// Runs before the inventoryDelta-nil early-return so the chain
	// works whether or not the delta reporter is wired (the chain is
	// orthogonal to delta flushing). releaseUnlessTerminal is
	// idempotent against terminal supply orders.
	if task, terr := e.db.GetChangeoverNodeTaskByEvacOrderID(order.ID); terr == nil && task != nil {
		if task.NextMaterialOrderID != nil {
			supplyDisp := ReleaseDisposition{CalledBy: "auto-evac-pickup"}
			// releaseIfReleasable, not releaseUnlessTerminal: nothing upstream
			// guarantees the supply leg has reached staged by the time the evac
			// robot lifts the bin. Releasing a queued/sourcing supply queues an
			// envelope Core refuses ("invalid_state") while the Edge row is
			// force-transitioned to in_transit — an Edge/Core divergence that a
			// later re-release cannot heal. Skipping is safe: the supply stages
			// on its own and the operator's next release-wait click fires it.
			if _, rerr := e.releaseIfReleasable(*task.NextMaterialOrderID, "deferred-supply-after-evac-pickup", supplyDisp); rerr != nil {
				e.logFn("bin_picked_up: deferred-supply release order %d for evac %s: %v", *task.NextMaterialOrderID, orderUUID, rerr)
			}
		}
		// Drop tasks with the evacuate marker stay non-terminal until the
		// line is physically clear — the operator opted in to "wait for
		// this node to be evacuated before cutover." Pickup is that
		// moment; the bin's onward trip to the supermarket is just a
		// logistics move and doesn't gate cutover. Advance the task to
		// line_cleared (terminal for drop, per IsNodeTaskStateTerminal)
		// so the cutover guard and downstream rollups unblock immediately
		// rather than waiting for order completion at the destination.
		// Non-evac drops were stamped terminal at plan time, so the
		// !terminal guard makes this a no-op for them.
		if task.Situation == "drop" && !domain.IsNodeTaskStateTerminal(task.State, task.Situation) {
			if err := e.db.UpdateChangeoverNodeTaskState(task.ID, domain.NodeTaskLineCleared); err != nil {
				e.logFn("bin_picked_up: advance drop task %d to line_cleared: %v", task.ID, err)
			}
		}
	}

	if e.inventoryDelta == nil {
		// Nothing to flush; reporter not wired (test contexts).
		return
	}

	// Flush the released-bin's accumulator: any deltas recorded
	// between RELEASE PARTIAL and BinPickedUp must ship before the
	// active claim advances or the runtime clears, otherwise
	// post-flush ticks attribute against a bin that's no longer at
	// the slot.
	if err := e.inventoryDelta.OnBinPickedUp(order.ProcessNodeID); err != nil {
		e.logFn("bin_picked_up: flush failed: %v", err)
	}

	// Advance: clear the runtime's ActiveOrderID for whichever node
	// the order was tied to so the next tick attribution lands cleanly
	// against the next claim. ProcessNodeID may be nil (pure-kanban
	// orders, generic moves); skip in that case.
	if order.ProcessNodeID != nil {
		runtime, err := e.db.GetProcessNodeRuntime(*order.ProcessNodeID)
		if err != nil || runtime == nil {
			return
		}
		// Clear the active-ORDER ref only if the active order is still the
		// one we just picked up — guards against a race where the next bin's
		// delivery already advanced the slot.
		if runtime.ActiveOrderID != nil && *runtime.ActiveOrderID == order.ID {
			if err := e.db.UpdateProcessNodeRuntimeOrders(*order.ProcessNodeID, nil, runtime.StagedOrderID); err != nil {
				e.logFn("bin_picked_up: clear active order node=%d: %v", *order.ProcessNodeID, err)
			}
		}

		// Clear the active-BIN pointer whenever the bin that just physically
		// left the slot is the one bound as active — gated on BIN IDENTITY,
		// independent of the active-order ref above. Two ways the old gating
		// (ActiveOrderID == order.ID) missed a departed bin, both of which let
		// PLC consume ticks keep charging a bin that was gone:
		//   1. Two-robot swap: the EVAC leg carries the old bin out, but the
		//      evac is usually the staged (not active) order, so its pickup
		//      never matched ActiveOrderID.
		//   2. Changeover abort (cancelProcessChangeover) nulls the active-order
		//      ref *before* the evac's pickup event lands, permanently disarming
		//      the clear.
		// Springfield 2026-06-02: ALN_003 RH→LH changeover aborted mid-swap;
		// bin 18 (RH) departed but active_bin_id stayed = 18, so consume ticks
		// drained bin 18 while it sat in the supermarket. Tracking the pointer
		// by physical bin identity makes it follow reality through aborts.
		if e.inventoryDelta != nil && runtime.ActiveBinID != nil && *runtime.ActiveBinID == binID {
			if err := e.inventoryDelta.ClearActiveBin(*order.ProcessNodeID); err != nil {
				e.logFn("bin_picked_up: clear active bin node=%d: %v", *order.ProcessNodeID, err)
			}
		}
	}

	e.logFn("bin_picked_up: flushed deltas + cleared active for order=%s bin=%d (status=%s)",
		orderUUID, binID, order.Status)

	// Home consolidation: if this was Order A of a ClearLoaderHome sequence,
	// the robot just cleared the home position. Fire Order B (buffer partial → home).
	e.homeConsolidationsMu.Lock()
	if c, ok := e.homeConsolidations[orderUUID]; ok {
		delete(e.homeConsolidations, orderUUID)
		e.homeConsolidationsMu.Unlock()
		e.dispatchBufferConsolidation(c)
		return
	}
	e.homeConsolidationsMu.Unlock()
}
