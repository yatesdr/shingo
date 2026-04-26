// operator_release.go — Phase 3 of the lineside inventory plan.
//
// ReleaseOrderWithLineside is the unified release path for staged orders. It
// performs the operator's "I'm done, push it" click atomically with capture
// of any parts the operator pulled to lineside during the swap, the UOP
// reset, the changeover-task state transition, and the bin manifest sync at
// Core (via the OrderRelease envelope's RemainingUOP field).
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
	"log"

	"shingoedge/store/processes"
)

// ReleaseDispositionMode controls how a release action interacts with the
// bin's manifest at Core. See operator_release.go for a full discussion.
type ReleaseDispositionMode string

const (
	// DispositionCaptureLineside is the default operator-confirmed-empty
	// disposition: any pulled parts are captured as lineside buckets and
	// Core clears the bin's manifest (remaining_uop=0).
	DispositionCaptureLineside ReleaseDispositionMode = "capture_lineside"
	// DispositionSendPartialBack returns the partially-consumed bin to the
	// supermarket with its current UOP count. No bucket capture; Core syncs
	// uop_remaining and preserves the manifest.
	DispositionSendPartialBack ReleaseDispositionMode = "send_partial_back"
)

// ReleaseDisposition carries the operator's release-time intent from the HTTP
// handler down through ReleaseOrderWithLineside to the order manager. The
// zero value (Mode == "", LinesideCapture == nil, CalledBy == "") is the
// backward-compat default — no manifest change at Core.
type ReleaseDisposition struct {
	Mode            ReleaseDispositionMode
	LinesideCapture map[string]int // qty per part — only valid when Mode == DispositionCaptureLineside
	CalledBy        string         // operator identity for audit
}

// ReleaseOrderWithLineside performs the operator's release click:
//
//  1. Resolves the order's process node + active claim. Orders without a
//     process node (pure kanban, generic moves) skip the lineside path
//     entirely and fall through to a plain release.
//  2. Computes the remaining_uop value for Core from the disposition:
//     - DispositionCaptureLineside → &0 (mark bin empty)
//     - DispositionSendPartialBack → &runtime.RemainingUOP (partial; falls
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
	if order.ProcessNodeID == nil {
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
		e.logFn("release: order %d on node %s — toClaim is nil (runtime.ActiveClaimID=%s), skipping manifest sync; disposition %q dropped",
			orderID, node.Name, activeClaimStr, string(disp.Mode))
		return e.orderMgr.ReleaseOrder(orderID, nil, disp.CalledBy)
	}

	// Produce nodes don't use lineside buckets — skip capture, skip UOP
	// reset (produce resets on ingest completion, not release). Pass
	// nil remaining_uop so Core leaves the produce bin's manifest alone.
	if toClaim.Role == "produce" {
		return e.orderMgr.ReleaseOrder(orderID, nil, disp.CalledBy)
	}

	// Compute the manifest-sync UOP from the disposition. Capture the
	// runtime UOP BEFORE the SetProcessNodeRuntime reset below — otherwise
	// the reset would clobber the operator's intent. Renamed from
	// `remainingUOP` to disambiguate from Manager.ReleaseOrder's parameter
	// of the same name; both flow to the same envelope field but live in
	// different scopes.
	manifestUOP := computeReleaseRemainingUOP(disp, runtime)

	// Two-robot supply-order detection. The per-order release path
	// (apiReleaseOrder, /api/orders/{id}/release) doesn't know whether the
	// order being released is the supply (Order A) or the evac (Order B),
	// so it forwards the operator's chosen disposition either way. We have
	// to discriminate server-side based on which order is which in the
	// runtime's order slots.
	isSupply := e.isSupplyOrderInActiveTwoRobotSwap(node.ID, orderID)

	// Two-robot supply-bin protection (Bug A guard, ALN_002 plant test
	// 2026-04-23): if this is the supply order, skip the manifest sync.
	// Order A's bin is the freshly-loaded supply bin from the supermarket,
	// and clearing its manifest right before delivery destroys the payload
	// data the line is about to need. The consolidated ReleaseStagedOrders
	// path already does this by passing ReleaseDisposition{} for Order A;
	// this guard is the safety net when the per-order path runs instead.
	if manifestUOP != nil && isSupply {
		log.Printf("release: order %d is the supply order in a two-robot swap on node %s — skipping manifest sync to protect supply bin",
			orderID, node.Name)
		manifestUOP = nil
	}

	// Capture lineside buckets (conditional on disposition) and always
	// deactivate other styles on this node.
	if err := e.captureLinesideOnRelease(node, toClaim, disp); err != nil {
		return err
	}

	// Two-robot runtime-reset protection (Bug B guard, same plant incident):
	// in the per-order release path, if Order A (the supply) is released
	// before Order B (the evac), Order A's release would normally call
	// SetProcessNodeRuntime to reset RemainingUOP to capacity. That reset
	// CLOBBERS the runtime UOP that Order B's subsequent release needs to
	// read for the SEND PARTIAL BACK disposition. Result: Edge sends
	// remaining_uop=capacity (e.g. 1200) for Order B's evac, Core's
	// SyncOrClearForReleased writes that bogus value to the bin row,
	// manifest stays loaded with full UOP, bin lands at OutboundDestination
	// looking like a fresh full bin — exact ALN_002 → SMN_003 symptom for
	// the partial-back case.
	//
	// The runtime reset's purpose is "prepare the line node's UOP tracking
	// for the new bin's cycle." That's properly Order B's responsibility:
	// Order B is what evacuates the old bin and signals the cycle turnover.
	// Order A delivers the new bin, but until B clears, the old bin's UOP
	// state is what matters. Skip the reset on Order A's release; Order B's
	// release (or the consolidated ReleaseStagedOrders path, which does B
	// then A) will perform it correctly.
	if !isSupply {
		claimID := toClaim.ID
		if err := e.db.SetProcessNodeRuntime(node.ID, &claimID, toClaim.UOPCapacity); err != nil {
			return fmt.Errorf("reset runtime on release for node %d: %w", node.ID, err)
		}
	} else {
		log.Printf("release: order %d is the supply order in a two-robot swap on node %s — skipping runtime UOP reset (Order B's release owns the reset)",
			orderID, node.Name)
	}
	if nodeTask != nil {
		if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "released"); err != nil {
			log.Printf("release: update node task %d to released: %v", nodeTask.ID, err)
		}
	}

	return e.orderMgr.ReleaseOrder(orderID, manifestUOP, disp.CalledBy)
}

// computeReleaseRemainingUOP turns the operator's declared disposition into
// the *int that gets sent on the OrderRelease envelope.
//
// The empty-Mode case returns nil deliberately: it preserves the
// pre-disposition behavior for callers that don't opt in (Order A in
// two-robot swaps, fallback paths, older HTTP clients posting bare bodies).
// Without this gate, any zero-value ReleaseDisposition would silently
// start sending remaining_uop=0 and clearing manifests Core has never
// touched on release before — including Order A's freshly-loaded supply bin.
//
// SendPartialBack falls back to &0 when runtime.RemainingUOP is non-positive
// (-1 sentinel, never set, etc.) — there's nothing meaningful to preserve in
// that case, so we may as well declare empty.
func computeReleaseRemainingUOP(disp ReleaseDisposition, runtime *processes.RuntimeState) *int {
	switch disp.Mode {
	case DispositionCaptureLineside:
		zero := 0
		return &zero
	case DispositionSendPartialBack:
		if runtime != nil && runtime.RemainingUOP > 0 {
			v := runtime.RemainingUOP
			return &v
		}
		// Non-positive runtime UOP: nothing to preserve, fall through to empty.
		zero := 0
		return &zero
	default:
		// "" / unknown mode → backward-compat: no manifest action.
		return nil
	}
}

// isSupplyOrderInActiveTwoRobotSwap reports whether the given order is the
// supply order (Order A) in a currently-staged two-robot swap on the given
// node. Used by ReleaseOrderWithLineside to suppress the manifest sync for
// Order A — the supply bin coming from the supermarket should never have
// its manifest cleared at release time, only the evac bin (Order B) at the
// line should.
//
// Identification: in a two-robot swap, the runtime tracks both order IDs
// via UpdateProcessNodeRuntimeOrders(nodeID, &orderA.ID, &orderB.ID). The
// first slot (ActiveOrderID) is Order A (supply); the second
// (StagedOrderID) is Order B (evac). The claim's SwapMode must be
// "two_robot". All three signals must be present — we don't want this
// guard firing on non-two-robot orders that happen to share a node ID.
//
// Returns false on any DB read error (defensive — better to allow the
// release than block it on a transient lookup failure).
func (e *Engine) isSupplyOrderInActiveTwoRobotSwap(nodeID, orderID int64) bool {
	runtime, err := e.db.GetProcessNodeRuntime(nodeID)
	if err != nil || runtime == nil {
		return false
	}
	if runtime.ActiveOrderID == nil || runtime.StagedOrderID == nil {
		return false
	}
	if *runtime.ActiveOrderID != orderID {
		// The order being released isn't the supply slot — either it's
		// the evac order (Order B in StagedOrderID) or an unrelated order
		// that just shares the node. Manifest sync is fine for the evac
		// order; that's the path designed to clear the line bin.
		return false
	}
	if runtime.ActiveClaimID == nil {
		return false
	}
	claim, err := e.db.GetStyleNodeClaim(*runtime.ActiveClaimID)
	if err != nil || claim == nil {
		return false
	}
	return claim.SwapMode == "two_robot"
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

// captureLinesideOnRelease records any parts the operator pulled to
// lineside (only when the disposition is capture_lineside) and always
// deactivates buckets for other styles on this node. The deactivation
// fires on every disposition because the release click is the "this
// node is running this style now" moment regardless of where the bin
// is heading.
//
// Note: count for the SEND PARTIAL BACK disposition is captured at
// release-click time, not at robot-pickup time. For two-robot swaps,
// the evacuation robot picks up the old bin moments after release —
// any further consumption between click and pickup isn't tracked. The
// operator's intent at click time is the source of truth.
func (e *Engine) captureLinesideOnRelease(node *processes.Node, toClaim *processes.NodeClaim, disp ReleaseDisposition) error {
	pairKey := toClaim.PairedCoreNode

	// Only capture parts when disposition is capture_lineside. Other
	// dispositions (send_partial_back, empty/default) skip the bucket loop
	// entirely.
	if disp.Mode == DispositionCaptureLineside {
		for part, qty := range disp.LinesideCapture {
			if qty <= 0 || part == "" {
				continue
			}
			if _, err := e.db.CaptureLinesideBucket(node.ID, pairKey, toClaim.StyleID, part, qty); err != nil {
				return fmt.Errorf("capture lineside bucket (node=%d style=%d part=%s): %w",
					node.ID, toClaim.StyleID, part, err)
			}
		}
	}

	// Always deactivate other styles on this node.
	if err := e.db.DeactivateOtherLinesideStyles(node.ID, toClaim.StyleID); err != nil {
		return fmt.Errorf("deactivate other lineside styles on node %d: %w", node.ID, err)
	}
	return nil
}
