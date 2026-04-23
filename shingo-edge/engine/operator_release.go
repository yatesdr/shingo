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

	"shingoedge/store"
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
		return e.orderMgr.ReleaseOrder(orderID, nil)
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
		return e.orderMgr.ReleaseOrder(orderID, nil)
	}

	// Produce nodes don't use lineside buckets — skip capture, skip UOP
	// reset (produce resets on ingest completion, not release). Pass
	// nil remaining_uop so Core leaves the produce bin's manifest alone.
	if toClaim.Role == "produce" {
		return e.orderMgr.ReleaseOrder(orderID, nil)
	}

	// Compute remaining_uop from the disposition. Capture the runtime UOP
	// BEFORE the SetProcessNodeRuntime reset below — otherwise the reset
	// would clobber the operator's intent.
	remainingUOP := computeReleaseRemainingUOP(disp, runtime)

	// Capture lineside buckets (conditional on disposition) and always
	// deactivate other styles on this node.
	if err := e.captureLinesideOnRelease(node, toClaim, disp); err != nil {
		return err
	}

	claimID := toClaim.ID
	if err := e.db.SetProcessNodeRuntime(node.ID, &claimID, toClaim.UOPCapacity); err != nil {
		return fmt.Errorf("reset runtime on release for node %d: %w", node.ID, err)
	}
	if nodeTask != nil {
		if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "released"); err != nil {
			log.Printf("release: update node task %d to released: %v", nodeTask.ID, err)
		}
	}

	return e.orderMgr.ReleaseOrder(orderID, remainingUOP)
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
func computeReleaseRemainingUOP(disp ReleaseDisposition, runtime *store.ProcessNodeRuntimeState) *int {
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

// resolveReleaseClaim returns the claim whose capacity the release
// should reset UOP against, plus the changeover node task when one is
// active. For a changeover the target is the to-style claim; otherwise
// it's the currently-active claim on the node.
func (e *Engine) resolveReleaseClaim(node *store.ProcessNode, runtime *store.ProcessNodeRuntimeState) (*store.StyleNodeClaim, *store.ChangeoverNodeTask) {
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
func (e *Engine) captureLinesideOnRelease(node *store.ProcessNode, toClaim *store.StyleNodeClaim, disp ReleaseDisposition) error {
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
