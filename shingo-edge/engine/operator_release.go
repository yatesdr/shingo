// operator_release.go — unified release path for staged orders.
//
// ReleaseOrderWithLineside performs the operator's "I'm done, push it"
// click atomically with capture of any parts the operator pulled to
// lineside during the swap, the UOP reset, the changeover-task state
// transition, and the bin manifest sync at Core (via the OrderRelease
// envelope's RemainingUOP field for RELEASE EMPTY / SEND PARTIAL BACK,
// or via the BinUOPDelta(capture_reduction) stream for PULL PARTS
// LINESIDE).
//
// Disposition modes
// -----------------
//
//   - DispositionCaptureLineside: operator declares the line emptied the bin.
//     Captures any pulled-to-lineside parts as buckets, and tells Core to
//     clear the bin's manifest (remaining_uop = 0). Default for the existing
//     "NOTHING PULLED" / per-part flows.
//   - DispositionSendPartialBack: operator wants the partially-consumed bin
//     returned to the supermarket as-is. No bucket capture, and Core syncs
//     the bin's UOP to the runtime's RemainingUOP at click time (manifest
//     preserved). Used by the "SEND PARTIAL BACK" button.
//   - "" (empty / zero-value disposition): no manifest action — Core gets nil
//     for remaining_uop and leaves the bin alone. Used by Order A in two-
//     robot swaps (the supply order has no outgoing line bin) and by every
//     fallback path that currently calls orderMgr.ReleaseOrder directly.
//     Preserves pre-disposition behavior so older clients/paths don't
//     accidentally start clearing manifests.
//
// Before this handler existed, the UOP reset fired when Order B (or A,
// depending on swap mode) *completed* — i.e. when the bots dropped the
// empty bin back at the AMR supermarket. That meant the node counter
// lied about station state during the entire "bots home" leg, and any
// parts the operator had already run off at lineside weren't tracked
// anywhere. Reset-on-release closes both gaps.
//
// The completion handlers in wiring.go still perform the UOP reset +
// "released" state transition as a safety net for paths that never hit
// this release handler (e.g. changeover restore, future AutoConfirm
// edge cases). Those handlers check nodeTask.State first — if the
// release handler already ran, they no-op.
package engine

import (
	"fmt"

	"shingo/protocol"
	"shingoedge/domain"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
	"shingoedge/uop"
)

// Backward-compat re-exports for the release disposition types.
// The canonical home is shingoedge/uop (Phase 2 move). Engine callers
// can keep using engine.ReleaseDisposition etc. until Phase 3a finishes
// pushing the verb interfaces to all callers; new code should reference
// uop.ReleaseDisposition directly.
type (
	ReleaseDisposition     = uop.ReleaseDisposition
	ReleaseDispositionMode = uop.ReleaseDispositionMode
)

const (
	DispositionCaptureLineside  = uop.DispositionCaptureLineside
	DispositionSendPartialBack  = uop.DispositionSendPartialBack
	DispositionReleaseUnderpack = uop.DispositionReleaseUnderpack
)

// ReleaseOrderWithLineside performs the operator's release click:
//
//  1. Resolves the order's process node + active claim. Orders without a
//     process node (pure kanban, generic moves) skip the lineside path
//     entirely and fall through to a plain release.
//  2. Computes the remaining_uop value for Core from the disposition:
//     - DispositionCaptureLineside → &0 (mark bin empty)
//     - DispositionSendPartialBack → &runtime.RemainingUOPCached (partial; falls
//     back to &0 if runtime UOP is non-positive, e.g. sentinel)
//     - "" (empty Mode) → nil (no manifest change)
//  3. Captures any pulled-to-lineside parts (capture_lineside only) and
//     deactivates buckets for other styles on this node (always — release
//     click is the "this style is now active here" point, regardless of
//     disposition).
//  4. Resets RemainingUOP to the target claim's UOPCapacity for the next bin
//     and points runtime.ActiveClaimID at the target claim.
//  5. Advances the changeover node task state to "released" if applicable.
//  6. Calls orderMgr.ReleaseOrder with the computed remaining_uop, which
//     embeds it in the OrderRelease envelope. Core's HandleOrderRelease
//     then routes through BinManifestService.SyncOrClearForReleased before
//     dispatching the fleet.
//
// For orders that don't match a process node (produce-only, generic kanban,
// etc.) this falls back to a plain orderMgr.ReleaseOrder with nil
// remaining_uop — apiReleaseOrder can keep calling this method for every
// release without special-casing, and Core's manifest stays untouched on
// those legacy paths.
func (e *Engine) ReleaseOrderWithLineside(orderID int64, disp ReleaseDisposition) error {
	order, err := e.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("get order %d: %w", orderID, err)
	}

	// Orders without a process node (pure kanban, generic moves) skip
	// the lineside path entirely. No disposition mapping — Core gets
	// nil remaining_uop and leaves the bin alone (legacy behavior).
	//
	// This is an intentional skip, not a bug — but make it observable.
	// A "the prompt didn't fire" investigation that lands here without a
	// log line has nothing to grep for. See the cleanup-2026-04-27
	// synthesis for the four sites this pattern was added at.
	if order.ProcessNodeID == nil {
		e.logRelease("order=%d disposition=%q — skipping manifest sync: no_process_node",
			orderID, string(disp.Mode))
		return e.orderMgr.ReleaseOrder(orderID, nil, disp.CalledBy)
	}

	node, err := e.db.GetProcessNode(*order.ProcessNodeID)
	if err != nil {
		return fmt.Errorf("get process node %d: %w", *order.ProcessNodeID, err)
	}
	runtime, err := e.db.EnsureProcessNodeRuntime(node.ID)
	if err != nil {
		return fmt.Errorf("ensure runtime for node %d: %w", node.ID, err)
	}

	if dropTask, _ := e.db.GetChangeoverNodeTaskByEvacOrderID(order.ID); dropTask != nil && dropTask.Situation == "drop" {
		return e.releaseOrderDropFastPath(orderID, node, runtime, disp)
	}

	// Resolve the target claim: if a changeover is active, use the
	// to-style claim; otherwise use the currently-active claim.
	toClaim, nodeTask := e.resolveReleaseClaim(node, runtime)
	if toClaim == nil {
		// No claim to reset against — still want the release to go out.
		// Skip the disposition mapping; this covers config drift / ingest-only
		// nodes where Core has no opinion on the bin manifest anyway.
		//
		// Diagnostic: this nil-return silently drops the operator's disposition.
		// Surface it so a recurrence is visible in logs instead of producing the
		// invisible "remaining_uop=<nil>" symptom downstream. Was a candidate
		// failure mode for the ALN_002 incident class; see bug-fix-review-plan.md
		// item 1.3.
		// Print "<nil>" rather than 0 when the runtime slot is unset — at 2am
		// in the Debug Log UI, "ActiveClaimID=0" is ambiguous (could be a
		// real ID, could be unset); "ActiveClaimID=<nil>" is unmistakable.
		activeClaimStr := "<nil>"
		if runtime != nil && runtime.ActiveClaimID != nil {
			activeClaimStr = fmt.Sprintf("%d", *runtime.ActiveClaimID)
		}
		e.logRelease("order %d on node %s — toClaim is nil (runtime.ActiveClaimID=%s), skipping manifest sync; disposition %q dropped",
			orderID, node.Name, activeClaimStr, string(disp.Mode))
		return e.orderMgr.ReleaseOrder(orderID, nil, disp.CalledBy)
	}

	// Two-robot supply-order detection. The per-order release path
	// (apiReleaseOrder, /api/orders/{id}/release) doesn't know whether the
	// order being released is the supply or the evac leg, so it forwards the
	// operator's chosen disposition either way. We discriminate server-side
	// from the leg's steps — see isSupplyOrderInTwoRobotSwap.
	//
	// Refuse the release if the leg cannot be classified: the operator retries,
	// which is recoverable. Guessing is not — guess "evac" and Core wipes the
	// manifest of a bin that is about to feed the line (ALN_002).
	isSupply, err := e.isSupplyOrderInTwoRobotSwap(order, node, toClaim)
	if err != nil {
		e.logRelease("order=%d node=%s disposition=%q — refusing release: %v",
			orderID, node.Name, string(disp.Mode), err)
		return err
	}

	// Side-cycle trigger (U1 only): fires when the operator declares a
	// produce-side bin full (capture_lineside) on the line side of a
	// swap (supply orders suppressed by isSupply). A SEND PARTIAL BACK
	// release explicitly returns the bin to the supermarket — no new
	// full needs to land at the unloader.
	//
	// L1 (consume-side empty-in) used to fire here too. Removed when
	// Core's wiring_kanban DemandSignal pipeline became the single
	// trigger source for L1: every release that empties a bin moves it
	// in Core, Core observes the move at storage, fires DemandSignal
	// to Edge, Edge fires L1 with current supply count. The release-
	// driven path created timing weirdness (release evaluated before
	// Core's bin state settled) and partial coverage (non-release bin
	// movements didn't fire). U1 stays release-driven because it's
	// genuinely tied to the release event ("operator just finished a
	// full bin"); the trigger isn't a count threshold, it's the act of
	// finishing.
	if !isSupply && disp.Mode == DispositionCaptureLineside && toClaim.Role == protocol.ClaimRoleProduce {
		e.MaybeCreateUnloaderFullIn(toClaim.PayloadCode)
	}

	// Produce nodes don't use lineside buckets — skip capture, skip UOP
	// reset (produce resets on ingest completion, not release). Pass
	// nil remaining_uop so Core leaves the produce bin's manifest alone.
	//
	// Intentional skip; logged for investigation breadcrumbs (see the
	// no_process_node site above for the rationale).
	if toClaim.Role == protocol.ClaimRoleProduce {
		e.logRelease("order=%d node=%s disposition=%q — skipping manifest sync: produce_role",
			orderID, node.Name, string(disp.Mode))
		return e.orderMgr.ReleaseOrder(orderID, nil, disp.CalledBy)
	}

	return e.releaseOrderWithFullLineside(order, node, runtime, toClaim, nodeTask, disp, isSupply)
}

// releaseOrderDropFastPath handles the drop-CO release shape. A drop has
// no to-style claim by definition (the new style abandons this node), so
// the normal toClaim-based bookkeeping (UOP reset for the next bin,
// lineside bucket capture, node-task state advance) doesn't apply —
// there's no next bin and no new style claiming the slot. But the
// operator's disposition still needs to flow to Core so the returning
// bin's manifest reflects the partial count instead of arriving empty.
//
// Detect via the node task (situation="drop" with this order on
// OldMaterialReleaseOrderID) and pass the disposition through directly.
// Skipping the normal path avoids the toClaim-nil silent-drop at the
// resolution fallback (plant incident 2026-05-11: ALN_002 drop bin
// arrived empty at supermarket because resolveReleaseClaim returned nil
// and the disposition was discarded; root cause was runtime ambiguity
// in the fallback rather than a clean drop path).
func (e *Engine) releaseOrderDropFastPath(orderID int64, node *processes.Node, runtime *processes.RuntimeState, disp ReleaseDisposition) error {
	// Drop fast path doesn't run CaptureToLineside (no to-style claim
	// to fill buckets against), so resolvedBinID=0 is correct: any
	// PULL PARTS LINESIDE shape here falls through to the legacy &0
	// wipe rather than a delta-driven write.
	manifestUOP := computeReleaseRemainingUOP(disp, runtime, 0)
	wireDisposition := buildProtocolDisposition(disp, runtime)
	e.logRelease("order=%d node=%s disposition=%q — drop release: passing manifest sync through, skipping toClaim-dependent bookkeeping",
		orderID, node.Name, string(disp.Mode))
	return e.orderMgr.ReleaseOrderWithDisposition(orderID, manifestUOP, wireDisposition, disp.CalledBy)
}

// releaseOrderWithFullLineside is the main lineside release path. Runs
// after the early-exit branches in ReleaseOrderWithLineside have ruled
// themselves out (process node present, not a drop CO, toClaim resolved,
// not a produce node). Owns: supply-bin guard, lineside bucket capture +
// paired bin delta, release-click runtime cache binding, node-task state
// transition to "released", outbox flush, final OrderRelease emit.
func (e *Engine) releaseOrderWithFullLineside(order *storeorders.Order, node *processes.Node, runtime *processes.RuntimeState, toClaim *processes.NodeClaim, nodeTask *processes.NodeTask, disp ReleaseDisposition, isSupply bool) error {
	orderID := order.ID

	// Resolve the bin id used by the capture_reduction emit and the
	// manifest-sync fallback. order.BinID is typically populated by
	// Core's OrderDelivered reply, but REP / complex orders whose
	// reply didn't carry binID (and any path that lost the binding
	// before release) land here with nil. For PULL PARTS LINESIDE we
	// ask Core which bin is currently sitting at this slot so the
	// capture_reduction delta lands on the right row; if Core can't
	// resolve, resolvedBinID stays 0 and ComputeReleaseRemainingUOP's
	// legacy-&0 fallback fires so the bin doesn't return to
	// marshalling with its original UOP intact.
	// See lineside-buckets-investigation-2026-05-18.md.
	var resolvedBinID int64
	if order.BinID != nil {
		resolvedBinID = *order.BinID
	} else if disp.Mode == uop.DispositionCaptureLineside && len(disp.LinesideCapture) > 0 && e.coreClient.Available() {
		bin, _, err := e.coreClient.BinAtLineside(node.CoreNodeName)
		switch {
		case err != nil:
			e.logRelease("order=%d node=%s — BinAtLineside resolve failed (%v); capture_reduction will skip and manifest-sync fallback will fire",
				orderID, node.Name, err)
		case bin != nil:
			resolvedBinID = bin.BinID
		}
		// bin == nil with no error: Core confirmed the slot is empty;
		// fall through with resolvedBinID=0 so the legacy &0 wipe
		// fires.
	}

	// Compute the manifest-sync UOP from the disposition. Renamed from
	// `remainingUOP` to disambiguate from Manager.ReleaseOrder's parameter of
	// the same name; both flow to the same envelope field but live in
	// different scopes.
	manifestUOP := computeReleaseRemainingUOP(disp, runtime, resolvedBinID)

	// Phase 0b — protocol Disposition rides alongside the legacy
	// RemainingUOP pointer. Core uses RemainingUOP for the manifest sync
	// (unchanged behavior); Disposition's CountSuggested /
	// CapturesSuggested feed the override audit. Built before the
	// supply-bin guard so isSupply paths still ship the disposition for
	// audit completeness even when the manifest sync is suppressed.
	wireDisposition := buildProtocolDisposition(disp, runtime)

	// Two-robot supply-bin protection (Bug A guard, ALN_002 plant test
	// 2026-04-23): if this is the supply order, skip the manifest sync.
	// Order A's bin is the freshly-loaded supply bin from the supermarket,
	// and clearing its manifest right before delivery destroys the payload
	// data the line is about to need. The consolidated ReleaseStagedOrders
	// path already does this by passing ReleaseDisposition{} for Order A;
	// this guard is the safety net when the per-order path runs instead.
	if manifestUOP != nil && isSupply {
		// Routed through e.logRelease so unit tests can capture via the
		// debug-log ring buffer (subsystem="release"), and so operators
		// see the breadcrumb in the browser debug-log UI without SSH.
		// disp.Mode is included so a future investigation can see which
		// operator-declared disposition was overridden by the
		// supply-bin guard.
		e.logRelease("order=%d node=%s disposition=%q — skipping manifest sync: supply_bin_guard (two-robot swap)",
			orderID, node.Name, string(disp.Mode))
		manifestUOP = nil
	}

	// Capture lineside buckets (conditional on disposition), always
	// deactivate other styles on this node, and emit the paired bin
	// capture_reduction delta when capture happened. The verb owns the
	// atomic emit-pair (bucket fills + bin reduction) so the magnitude
	// of capture_reduction always matches the sum of capture_fill
	// emissions.
	//
	// PayloadCode comes from the order, not the claim. The bin being
	// captured-from is the order's bin (delivered or evac); its payload
	// is what the order recorded at create-time. toClaim.PayloadCode is
	// the target-style template, which differs from the bin's payload
	// during changeover or reassignment scenarios — passing it would
	// trip Core's payload-mismatch validation in
	// inventory_delta_service.ApplyBinUOPDelta and the delta would be
	// silently rejected.
	//
	// SuppressBinDelta gates the capture_reduction emit on the supply
	// leg (Order A in two-robot swaps): a fresh supply bin had nothing
	// pulled from it, so applying capture_reduction would corrupt the
	// authoritative count. Same isSupply gate the manifestUOP
	// suppression uses.
	//
	// IMPORTANT: capture_reduction rides ALONGSIDE the legacy
	// RemainingUOP=&0 send (which Core's existing manifest-sync still
	// applies). The legacy send and the delta both target the same
	// authoritative bins.uop_remaining; net result is correct because
	// the legacy send wipes-then-the-delta-reduces and "no manifest =
	// no bin" semantics make the resulting count consistent. The
	// dual-write removal is a future cleanup item.
	if e.inventoryDelta != nil {
		// Epoch resolution: if the resolved bin matches the runtime's
		// active bin, lift the epoch from runtime; otherwise pass 0
		// and let Core's stale-epoch warn surface the drift. The
		// resolved-but-doesn't-match case is rare (Core's bin pointer
		// changed while the release was being processed) and a hard
		// drop is preferable to attribution against the wrong epoch.
		var binEpoch int64
		if runtime != nil && runtime.ActiveBinID != nil && *runtime.ActiveBinID == resolvedBinID {
			binEpoch = runtime.ActiveBinEpoch
		}
		if _, err := e.inventoryDelta.CaptureToLineside(uop.CaptureEvent{
			NodeID:           node.ID,
			StyleID:          toClaim.StyleID,
			PairKey:          toClaim.PairedCoreNode,
			CoreNodeName:     node.CoreNodeName,
			Disposition:      disp,
			BinID:            resolvedBinID,
			PayloadCode:      order.PayloadCode,
			BinEpoch:         binEpoch,
			SuppressBinDelta: isSupply,
		}); err != nil {
			return err
		}
	}

	// Release-click finalizes the OLD bin's local count to match the
	// disposition we just told Core (manifestUOP): RELEASE EMPTY -> 0,
	// SEND PARTIAL BACK -> the preserved partial count. It does NOT
	// pre-load the incoming bin — under hold-and-replay the new bin's
	// count+epoch arrive on its OrderDelivered envelope, and any ticks
	// during the pickup->delivery gap are held (pending_uop_delta) and
	// replayed onto it. nil manifestUOP (supply leg / no disposition)
	// leaves the cache tracking the physical bin until pickup/delivery.
	if manifestUOP != nil {
		if err := e.db.UpdateProcessNodeUOP(node.ID, *manifestUOP); err != nil {
			e.logRelease("finalize old-bin cache for node %s on release: %v", node.Name, err)
		}
	}
	if nodeTask != nil {
		if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, domain.NodeTaskReleased); err != nil {
			e.logRelease("update node task %d to released: %v", nodeTask.ID, err)
		}
	}

	// Flush boundary: drain any accumulated bin/bucket deltas for this
	// scope to the outbox before shipping the OrderRelease envelope.
	// Once enqueued, Kafka delivery semantics + Core's inventory_delta_dedup
	// handle the rest; we trust the bus rather than abort the release on
	// in-flight state.
	if e.inventoryDelta != nil {
		e.inventoryDelta.Flush()
	}

	if err := e.orderMgr.ReleaseOrderWithDisposition(orderID, manifestUOP, wireDisposition, disp.CalledBy); err != nil {
		return err
	}

	return nil
}

// computeReleaseRemainingUOP / buildProtocolDisposition moved to
// shingoedge/uop in Phase 2. These thin shims preserve the existing
// call sites in this file until Phase 3a inlines them at the
// Capturer.CaptureToLineside verb boundary.
func computeReleaseRemainingUOP(disp ReleaseDisposition, runtime *processes.RuntimeState, resolvedBinID int64) *int {
	return uop.ComputeReleaseRemainingUOP(disp, runtime, resolvedBinID)
}

func buildProtocolDisposition(disp ReleaseDisposition, runtime *processes.RuntimeState) *protocol.UOPDisposition {
	return uop.BuildProtocolDisposition(disp, runtime)
}

// isSupplyOrderInTwoRobotSwap reports whether the given order is the
// supply leg of a two-robot swap pair. Used by ReleaseOrderWithLineside to
// suppress manifest sync for the supply leg — the fresh bin coming from the
// supermarket should never have its manifest cleared at release time, only
// the evac bin at the line should.
//
// Identification uses durable order data only:
//
//   - order.SiblingOrderID must be non-nil (a swap pair was established
//     at order-creation time via LinkOrderSiblings).
//   - claim.SwapMode must be "two_robot" or "two_robot_press_index"
//     (the modes that have a supply/evac choreography).
//   - the leg must PLACE A BIN AT this node — legPlacesBinAt: a dropoff at the
//     node with no LATER pickup from it. The supply leg leaves a bin on the
//     slot; the evac leg takes one off.
//
// The last test reads the order's steps, NOT order.DeliveryNode. That field
// cannot answer the question and never could:
//
//   - press-index R1 (the evac — it lifts the spent bin OFF the press) stored
//     the press as its delivery node, so it was misread as the SUPPLY leg;
//   - press-index R2 (the real supply — it sets a bin ON the press) is
//     auto-confirmed, and dispatchComplexLeg blanks delivery_node for
//     auto-confirm legs, so no value in that column could ever name it.
//
// Nor is "where does the leg END?" the right question — that was the previous
// fix and it is still wrong for a 3-position press-index R2, which sets a bin on
// the press mid-sequence and then carries on to re-index the next position. Ask
// where the BIN comes to rest, not the robot. See legPlacesBinAt.
//
// Pre-2026-05-04, this function read runtime.ActiveOrderID /
// StagedOrderID to discriminate. That signal decayed: handler_bin_picked_up
// nulls ActiveOrderID when the supply bin leaves the supermarket, and
// any subsequent release of the supply order saw the guard return false
// → manifest cleared at Core → bin arrived empty at the slot → went
// negative on consume ticks. Plant incident on ALN_002 (bin 12 reached
// uop_remaining=-20). The sibling pointer is durable across that event.
//
// A steps-read failure is returned, not swallowed. It used to return false —
// "better to allow the release than block it" — which was doubly wrong: this
// function does not gate the release (it only selects a disposition, at the
// three call sites below), and false means EVAC, which is the branch that wipes
// the manifest. An unclassifiable leg is refused instead. The trade is real but
// one-sided: erring toward supply costs a U1 unloader-full signal, while erring
// toward evac empties a bin the line is about to need — and that is the
// incident class (ALN_002).
func (e *Engine) isSupplyOrderInTwoRobotSwap(order *storeorders.Order, node *processes.Node, claim *processes.NodeClaim) (bool, error) {
	if order == nil || node == nil || claim == nil {
		return false, nil
	}
	if !claim.SwapMode.IsTwoRobot() {
		return false, nil
	}
	if order.SiblingOrderID == nil {
		return false, nil
	}
	stepsJSON, err := e.db.GetOrderStepsJSON(order.ID)
	if err != nil {
		return false, fmt.Errorf("supply-leg check: order %d: load steps: %w", order.ID, err)
	}
	placesBin, err := legPlacesBinAtJSON(stepsJSON, node.CoreNodeName)
	if err != nil {
		return false, fmt.Errorf("supply-leg check: order %d: %w", order.ID, err)
	}
	return placesBin, nil
}

// resolveReleaseClaim returns the claim whose capacity the release
// should reset UOP against, plus the changeover node task when one is
// active. For a changeover the target is the to-style claim; otherwise
// it's the currently-active claim on the node.
func (e *Engine) resolveReleaseClaim(node *processes.Node, runtime *processes.RuntimeState) (*processes.NodeClaim, *processes.NodeTask) {
	if changeover, err := e.db.GetActiveProcessChangeover(node.ProcessID); err == nil {
		if toClaim, err := e.db.GetStyleNodeClaimByNode(changeover.ToStyleID, node.CoreNodeName); err == nil {
			if task, err := e.db.GetChangeoverNodeTaskByNode(changeover.ID, node.ID); err == nil {
				return toClaim, task
			}
			return toClaim, nil
		}
	}
	if runtime.ActiveClaimID == nil {
		return nil, nil
	}
	claim, err := e.db.GetStyleNodeClaim(*runtime.ActiveClaimID)
	if err != nil {
		return nil, nil
	}
	return claim, nil
}

// captureLinesideOnRelease moved into uop.Mutator.CaptureToLineside
// (Phase 3a wedge 10). The verb owns the atomic bucket-fill + bin-
// reduction emit pair; engine resolves order/claim/sibling context
// and passes it in via uop.CaptureEvent.
