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
//       back to &0 if runtime UOP is non-positive, e.g. sentinel)
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

	// Drop CO release fast-path. A drop has no to-style claim by definition
	// (the new style abandons this node), so the normal toClaim-based
	// bookkeeping (UOP reset for the next bin, lineside bucket capture,
	// node-task state advance) doesn't apply — there's no next bin and no
	// new style claiming the slot. But the operator's disposition still
	// needs to flow to Core so the returning bin's manifest reflects the
	// partial count instead of arriving empty.
	//
	// Detect via the node task (situation="drop" with this order on
	// OldMaterialReleaseOrderID) and pass the disposition through directly.
	// Skipping the normal path avoids the toClaim-nil silent-drop at the
	// resolution fallback (plant incident 2026-05-11: ALN_002 drop bin
	// arrived empty at supermarket because resolveReleaseClaim returned nil
	// and the disposition was discarded; root cause was runtime ambiguity
	// in the fallback rather than a clean drop path).
	if dropTask, _ := e.db.GetChangeoverNodeTaskByEvacOrderID(order.ID); dropTask != nil && dropTask.Situation == "drop" {
		manifestUOP := computeReleaseRemainingUOP(disp, runtime)
		wireDisposition := buildProtocolDisposition(disp, runtime)
		e.logRelease("order=%d node=%s disposition=%q — drop release: passing manifest sync through, skipping toClaim-dependent bookkeeping",
			orderID, node.Name, string(disp.Mode))
		return e.orderMgr.ReleaseOrderWithDisposition(orderID, manifestUOP, wireDisposition, disp.CalledBy)
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
	// order being released is the supply (Order A) or the evac (Order B),
	// so it forwards the operator's chosen disposition either way. We
	// discriminate server-side via the durable sibling pointer set at
	// order-creation time — see isSupplyOrderInTwoRobotSwap.
	isSupply := e.isSupplyOrderInTwoRobotSwap(order, node, toClaim)

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

	// Compute the manifest-sync UOP from the disposition. Renamed from
	// `remainingUOP` to disambiguate from Manager.ReleaseOrder's parameter of
	// the same name; both flow to the same envelope field but live in
	// different scopes.
	manifestUOP := computeReleaseRemainingUOP(disp, runtime)

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
		var binID int64
		if order.BinID != nil {
			binID = *order.BinID
		}
		if _, err := e.inventoryDelta.CaptureToLineside(uop.CaptureEvent{
			NodeID:           node.ID,
			StyleID:          toClaim.StyleID,
			PairKey:          toClaim.PairedCoreNode,
			Disposition:      disp,
			BinID:            binID,
			PayloadCode:      order.PayloadCode,
			SuppressBinDelta: isSupply,
		}); err != nil {
			return err
		}
	}

	// Release-click runtime cache binding. The slot's accounting flips
	// to the *incoming* supply bin at the click moment, regardless of
	// swap mode or which leg of a pair is being released. Both legs of
	// a two-robot pair land here and write the same value (resolved via
	// the supply sibling), so the second call is an idempotent rewrite.
	//
	// Resolution: if this order delivers to the slot, it IS the supply
	// leg. Otherwise follow the sibling pointer to find the supply leg.
	// If no supply leg can be resolved (pure removal / abandoned slot),
	// cache zeroes — operator's click is "this slot is now empty"
	// pending whatever physical event clears it. Post-flip (6d226d1)
	// Edge is authoritative for the at-node count; subsequent ticks
	// emit signed deltas keeping Core in step. No reconciler.
	//
	// Failure mode: if the supply bin's id can't be reached at Core
	// (network blip), the cache is left untouched rather than
	// overwritten with a guessed value — better stale than wrong.
	supplyBinID := e.resolveReleaseSupplyBin(order, node.CoreNodeName)
	cacheValue := 0
	var cachedBinID *int64
	skipCacheWrite := false
	if supplyBinID != nil {
		uop, found, lookupErr := e.coreClient.BinByID(*supplyBinID)
		switch {
		case lookupErr != nil:
			// Core unreachable: leave cache untouched. Subsequent
			// PLC ticks will continue to attribute against the
			// current cached_bin_id; no reconciler exists to re-bind.
			e.logRelease("order=%d node=%s — supply bin %d uop lookup failed (Core unreachable): %v — cache untouched",
				orderID, node.Name, *supplyBinID, lookupErr)
			skipCacheWrite = true
		case !found:
			// Confirmed missing — operator clicked but the supply bin
			// is gone. Treat as no supply leg: cache=0, cached_bin_id=nil.
		default:
			cacheValue = uop
			cachedBinID = supplyBinID
		}
	}
	if !skipCacheWrite {
		// PrepareIncoming when we have a resolved supply bin;
		// ClearCache when both pointer and count fall back to nil/0
		// (no supply leg resolved — pure removal / abandoned slot).
		if e.inventoryDelta != nil {
			var err error
			if cachedBinID != nil {
				err = e.inventoryDelta.PrepareIncoming(node.ID, *cachedBinID, cacheValue)
			} else {
				err = e.inventoryDelta.ClearCache(node.ID)
			}
			if err != nil {
				e.logRelease("set runtime cache for node %s on release: %v", node.Name, err)
			}
		}
	}
	if nodeTask != nil {
		if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "released"); err != nil {
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
func computeReleaseRemainingUOP(disp ReleaseDisposition, runtime *processes.RuntimeState) *int {
	return uop.ComputeReleaseRemainingUOP(disp, runtime)
}

func buildProtocolDisposition(disp ReleaseDisposition, runtime *processes.RuntimeState) *protocol.UOPDisposition {
	return uop.BuildProtocolDisposition(disp, runtime)
}

// isSupplyOrderInTwoRobotSwap reports whether the given order is the
// supply leg (Order A) of a two-robot swap pair. Used by
// ReleaseOrderWithLineside to suppress manifest sync for Order A — the
// supply bin coming from the supermarket should never have its manifest
// cleared at release time, only the evac bin (Order B) at the line should.
//
// Identification uses durable order data only:
//
//   - order.SiblingOrderID must be non-nil (a swap pair was established
//     at order-creation time via LinkOrderSiblings).
//   - claim.SwapMode must be "two_robot" or "two_robot_press_index"
//     (the modes that have a supply/evac choreography).
//   - order.DeliveryNode must equal node.CoreNodeName (supply delivers AT
//     the slot; evac departs FROM it).
//
// Pre-2026-05-04, this function read runtime.ActiveOrderID /
// StagedOrderID to discriminate. That signal decayed: handler_bin_picked_up
// nulls ActiveOrderID when the supply bin leaves the supermarket, and
// any subsequent release of the supply order saw the guard return false
// → manifest cleared at Core → bin arrived empty at the slot → went
// negative on consume ticks. Plant incident on ALN_002 (bin 12 reached
// uop_remaining=-20). The sibling pointer is durable across that event.
//
// Returns false on any DB read error (defensive — better to allow the
// release than block it on a transient lookup failure).
func (e *Engine) isSupplyOrderInTwoRobotSwap(order *storeorders.Order, node *processes.Node, claim *processes.NodeClaim) bool {
	if order == nil || node == nil || claim == nil {
		return false
	}
	if claim.SwapMode != protocol.SwapModeTwoRobot && claim.SwapMode != protocol.SwapModeTwoRobotPressIndex {
		return false
	}
	if order.SiblingOrderID == nil {
		return false
	}
	return order.DeliveryNode == node.CoreNodeName
}

// resolveReleaseSupplyBin resolves the BinID of the supply leg for the
// release-click cache binding. The supply leg is whichever order in the
// swap pair delivers TO this slot (DeliveryNode == coreNodeName). Three
// cases:
//
//   - this order delivers to the slot → it is the supply, return its BinID
//   - this order doesn't, but its sibling does → return the sibling's BinID
//   - neither → return nil (no supply leg; cache flips to 0)
//
// Returns nil when the order or sibling has no BinID assigned yet
// (shouldn't normally happen at release-click time but guards against
// races with the dispatch path).
func (e *Engine) resolveReleaseSupplyBin(order *storeorders.Order, coreNodeName string) *int64 {
	if order == nil {
		return nil
	}
	if order.DeliveryNode == coreNodeName && order.BinID != nil {
		return order.BinID
	}
	if order.SiblingOrderID == nil {
		return nil
	}
	sibling, err := e.db.GetOrder(*order.SiblingOrderID)
	if err != nil {
		return nil
	}
	if sibling.DeliveryNode == coreNodeName && sibling.BinID != nil {
		return sibling.BinID
	}
	return nil
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
