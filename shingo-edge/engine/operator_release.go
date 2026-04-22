// operator_release.go — Phase 3 of the lineside inventory plan.
//
// ReleaseOrderWithLineside is the new unified release path for staged
// orders: it performs the operator's "I'm done, push it" click atomically
// with capture of any parts the operator pulled to lineside during the
// swap, the UOP reset, and the changeover-task state transition.
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

// ReleaseOrderWithLineside performs the operator's release click:
//
//  1. Captures any parts the operator pulled to lineside during the swap.
//     qtyByPart maps part_number → qty. Zero-qty and missing entries are
//     no-ops — an empty map means "nothing was pulled to lineside."
//  2. Deactivates any active buckets on the node that belong to a
//     different style (keeps the one-active-style-per-node rule).
//  3. Resets RemainingUOP to the target claim's UOPCapacity and points
//     runtime.ActiveClaimID at the target claim.
//  4. If this release is part of a changeover, advances the node task
//     state to "released".
//  5. Calls orderMgr.ReleaseOrder to dispatch the bots.
//
// All five happen before the release is dispatched so the counter never
// lies and lineside inventory is correctly modeled from tick zero.
//
// For orders that don't match a process node (produce-only, generic
// kanban, etc.) this falls back to a plain orderMgr.ReleaseOrder —
// apiReleaseOrder can keep calling this method for every release
// without special-casing.
func (e *Engine) ReleaseOrderWithLineside(orderID int64, qtyByPart map[string]int) error {
	order, err := e.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("get order %d: %w", orderID, err)
	}

	// Orders without a process node (pure kanban, generic moves) skip
	// the lineside path entirely.
	if order.ProcessNodeID == nil {
		return e.orderMgr.ReleaseOrder(orderID)
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
		// This covers edge cases (config drift, ingest-only nodes).
		return e.orderMgr.ReleaseOrder(orderID)
	}

	// Produce nodes don't use lineside buckets — skip capture, skip UOP
	// reset (produce resets on ingest completion, not release). Just
	// pass through to the release.
	if toClaim.Role == "produce" {
		return e.orderMgr.ReleaseOrder(orderID)
	}

	// Capture lineside buckets and advance state.
	if err := e.captureLinesideOnRelease(node, toClaim, qtyByPart); err != nil {
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

	return e.orderMgr.ReleaseOrder(orderID)
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
// lineside and flips stranded buckets for other styles to inactive.
// qtyByPart may be nil or empty — in that case only the deactivation
// step runs (the operator confirmed "nothing to capture").
func (e *Engine) captureLinesideOnRelease(node *store.ProcessNode, toClaim *store.StyleNodeClaim, qtyByPart map[string]int) error {
	pairKey := toClaim.PairedCoreNode

	for part, qty := range qtyByPart {
		if qty <= 0 || part == "" {
			continue
		}
		if _, err := e.db.CaptureLinesideBucket(node.ID, pairKey, toClaim.StyleID, part, qty); err != nil {
			return fmt.Errorf("capture lineside bucket (node=%d style=%d part=%s): %w",
				node.ID, toClaim.StyleID, part, err)
		}
	}

	// Deactivate any active buckets on this node for other styles, even
	// if nothing was captured this time — the release click is the
	// point where "this node is running this style now" becomes true.
	if err := e.db.DeactivateOtherLinesideStyles(node.ID, toClaim.StyleID); err != nil {
		return fmt.Errorf("deactivate other lineside styles on node %d: %w", node.ID, err)
	}
	return nil
}
