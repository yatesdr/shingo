package dispatch

import (
	"fmt"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/fleet/seerrds"
)

// resolveComplexSteps validates and resolves all steps, returning concrete node names.
//
// Pure function — does NOT side-effect sendError on failure. Callers
// decide how to surface the error (intake may queue on capacity
// errors via classifyResolutionError; the scanner replay path
// re-runs resolution per tick).
//
// Error shape: "step N: <reason>" — index info preserved so callers
// don't need to re-format. The wrapped reason from resolveStepNode
// preserves the original resolver substring so
// classifyResolutionError's ResolutionCapacity branch can match.
func (d *Dispatcher) resolveComplexSteps(steps []protocol.ComplexOrderStep, payloadCode string) ([]resolvedStep, error) {
	var resolved []resolvedStep
	for i, step := range steps {
		switch step.Action {
		case protocol.ActionPickup, protocol.ActionDropoff:
			// Blank dropoff = deferred destination (placeForDedicatedLoader resolves it
			// after intake). Pass it through unchanged, same as reResolveComplexSteps.
			if step.Action == protocol.ActionDropoff && step.Node == "" {
				resolved = append(resolved, resolvedStep{Action: protocol.ActionDropoff, Empty: step.Empty})
				continue
			}
			nodeName, group, err := d.resolveStepNode(step, payloadCode)
			if err != nil {
				return nil, fmt.Errorf("step %d: %w", i, err)
			}
			resolved = append(resolved, resolvedStep{Action: step.Action, Node: nodeName, Group: group, Empty: step.Empty})
		case protocol.ActionWait:
			// Wait may optionally include a node (drive-to-and-hold).
			// If present, resolve it; otherwise it's a bare wait (split point only).
			if step.Node != "" {
				nodeName, group, err := d.resolveStepNode(step, payloadCode)
				if err != nil {
					return nil, fmt.Errorf("step %d: %w", i, err)
				}
				resolved = append(resolved, resolvedStep{Action: protocol.ActionWait, Node: nodeName, Group: group})
			} else {
				resolved = append(resolved, resolvedStep{Action: protocol.ActionWait})
			}
		default:
			return nil, fmt.Errorf("step %d: unknown step action: %s", i, step.Action)
		}
	}
	return resolved, nil
}

// reResolveComplexSteps walks an already-resolved step list and
// re-resolves any step whose node still references a synthetic NGRP.
// Used by DispatchPreparedComplex on the scanner replay path: when
// intake queued an order because the NGRP was saturated, the original
// NGRP names sit in stepsJSON until a later tick succeeds.
//
// Returns:
//   - newSteps: resolution-applied step list (concrete child names
//     where NGRP resolution succeeded).
//   - changed: true if any step's Node value changed from the input.
//     The dispatcher persists the new stepsJSON when changed=true so
//     subsequent ticks don't redo the resolution work and so claim
//     proceeds against the locked-in children.
//   - err: the first resolution error encountered. Caller distinguishes
//     capacity (queue, retry next tick), buried (replay reshuffle), and
//     other errors (fail) via classifyResolutionError.
func (d *Dispatcher) reResolveComplexSteps(steps []resolvedStep, payloadCode string) (newSteps []resolvedStep, changed bool, err error) {
	newSteps = make([]resolvedStep, 0, len(steps))
	for i, step := range steps {
		if step.Node == "" {
			newSteps = append(newSteps, step)
			continue
		}
		node, lookupErr := d.db.GetNodeByDotName(step.Node)
		if lookupErr != nil || node == nil {
			// Node vanished from Core — unrecoverable. Fall through to
			// the original step; the claim path will surface a usable
			// error.
			newSteps = append(newSteps, step)
			continue
		}
		if !(node.IsSynthetic && node.NodeTypeCode == protocol.NodeClassNGRP) {
			// Already a concrete node — no re-resolution needed.
			newSteps = append(newSteps, step)
			continue
		}
		// Step still references an NGRP; re-attempt resolution. Carry Empty so
		// the produce empty-leg distinction survives replay re-resolution.
		ps := protocol.ComplexOrderStep{Action: step.Action, Node: step.Node, Empty: step.Empty}
		newName, group, resolveErr := d.resolveStepNode(ps, payloadCode)
		if resolveErr != nil {
			return steps, false, fmt.Errorf("step %d: %w", i, resolveErr)
		}
		if newName != step.Node {
			changed = true
		}
		newSteps = append(newSteps, resolvedStep{Action: step.Action, Node: newName, Group: group, Empty: step.Empty})
	}
	return newSteps, changed, nil
}

// stepsAsResolved performs a 1:1 field copy from the wire-protocol
// step shape to the dispatcher's resolvedStep shape, preserving
// whatever Node names the caller provided (NGRP or concrete). Used by
// HandleComplexOrderRequest when intake resolution fails with a
// capacity error — the original NGRP-bearing steps are preserved so
// DispatchPreparedComplex can re-attempt resolution on each replay.
func stepsAsResolved(steps []protocol.ComplexOrderStep) []resolvedStep {
	out := make([]resolvedStep, 0, len(steps))
	for _, s := range steps {
		out = append(out, resolvedStep{Action: s.Action, Node: s.Node, Empty: s.Empty})
	}
	return out
}

// resolveStepNode resolves a single step's node. If the node is a synthetic
// group (NGRP), it is automatically resolved via the group resolver. If the
// node is concrete, it is returned directly. If no node is provided, the
// global fallback resolves via payload code.
//
// TODO: route this through dispatch.SourceFinder. This is the last
// inline copy of the plant-wide bin finders (FindEmptyCompatibleBinInGroup /
// FindEmptyCompatibleBin / FindSourceBinFIFO below) — the simple planners and the
// scanner already collapsed onto SourceFinder. It is a visible,
// forbidigo-carved-out exception (exclusions.rules #7) until the complex-path
// unification folds complexPickups through the finder.
func (d *Dispatcher) resolveStepNode(step protocol.ComplexOrderStep, payloadCode string) (string, string, error) {
	if step.Node != "" {
		node, err := d.db.GetNodeByDotName(step.Node)
		if err != nil {
			return "", "", fmt.Errorf("node %q not found", step.Node)
		}
		// Auto-detect group nodes and resolve to a concrete slot.
		if node.IsSynthetic && node.NodeTypeCode == protocol.NodeClassNGRP && d.resolver != nil {
			// Empty pickup leg (produce node's "bring an empty to fill"):
			// resolve to a slot holding an EMPTY compatible carrier, not a
			// payload-matching full. Mirrors planRetrieveEmpty's source-group
			// branch, which also bypasses the full-retrieve resolver for
			// empties. (Pre-fix every complex pickup resolved as
			// OrderTypeRetrieve — a full — which delivered a full to produce
			// nodes; step.Empty now carries the distinction the old comment
			// here said it would need.)
			if step.Action == protocol.ActionPickup && step.Empty {
				bin, err := d.db.FindEmptyCompatibleBinInGroup(payloadCode, node.ID, 0)
				if err != nil {
					return "", "", fmt.Errorf("cannot resolve empty in group %s: %w", step.Node, err)
				}
				if bin == nil || bin.NodeID == nil {
					return "", "", fmt.Errorf("no empty carrier in group %s", step.Node)
				}
				slot, err := d.db.GetNode(*bin.NodeID)
				if err != nil || slot == nil {
					return "", "", fmt.Errorf("resolve node for empty bin %d in group %s: %w", bin.ID, step.Node, err)
				}
				return slot.Name, step.Node, nil
			}
			orderType := OrderTypeRetrieve
			if step.Action == protocol.ActionDropoff {
				orderType = OrderTypeStore
			}
			result, err := d.resolver.Resolve(node, orderType, payloadCode, nil)
			if err != nil {
				return "", "", fmt.Errorf("cannot resolve group %s: %w", step.Node, err)
			}
			return result.Node.Name, step.Node, nil
		}
		return node.Name, "", nil
	}
	// Global fallback: when Edge sends no node, resolve the pickup SOURCE using
	// the payload code (FindSourceBinFIFO / FindEmptyCompatibleBin). Blank
	// dropoffs are deferred and short-circuited by the callers, so they never
	// reach here — there is no dropoff fallback.
	if payloadCode != "" {
		switch step.Action {
		case protocol.ActionPickup:
			// Global fallback resolver: no order-level destination context here
			// (we are picking the source), so no node to exclude. Pass 0.
			// Empty leg sources an empty carrier (FindEmptyCompatibleBin),
			// matching the NGRP empty branch above; otherwise a full (FIFO).
			if step.Empty {
				bin, err := d.db.FindEmptyCompatibleBin(payloadCode, "", 0)
				if err != nil {
					return "", "", fmt.Errorf("no empty carrier for payload %q: %w", payloadCode, err)
				}
				if bin == nil || bin.NodeID == nil {
					return "", "", fmt.Errorf("no empty carrier for payload %q", payloadCode)
				}
				node, err := d.db.GetNode(*bin.NodeID)
				if err != nil || node == nil {
					return "", "", fmt.Errorf("resolve node for empty bin %d: %w", bin.ID, err)
				}
				d.dbg("resolveStepNode: global fallback empty pickup → %s (bin %d)", node.Name, bin.ID)
				return node.Name, "", nil
			}
			bin, err := d.db.FindSourceBinFIFO(payloadCode, 0)
			if err != nil {
				return "", "", fmt.Errorf("no source bin for payload %q: %w", payloadCode, err)
			}
			node, err := d.db.GetNode(*bin.NodeID)
			if err != nil {
				return "", "", fmt.Errorf("resolve node for source bin %d: %w", bin.ID, err)
			}
			d.dbg("resolveStepNode: global fallback pickup → %s (bin %d)", node.Name, bin.ID)
			return node.Name, "", nil
		}
	}
	return "", "", fmt.Errorf("step requires either node or payload_code for resolution")
}

// extractEndpoints returns the pickup (first actionable) and delivery (last
// actionable) nodes. "Actionable" means pickup or dropoff — a wait is skipped.
//
// This is where Core's order.DeliveryNode comes from. Edge does NOT send one:
// ComplexOrderRequest has no delivery-node field, so Core derives it here at
// intake and again on re-resolve. Core's delivery_node and the Edge's column of
// the same name are independent values; do not reason from one about the other.
//
// It is load-bearing for ROBOT ROUTING, not just display: patchRedirectSegments
// (complex_release.go) rewrites the final segment's last dropoff to
// order.DeliveryNode so a redirect issued while the order was staged actually
// reaches the robot. Redefining what this returns re-aims a robot.
//
// It is NOT a leg's role, and two dispatch predicates used to think it was —
// swapRemovalLegHeld and deadIsEvac, both of which deadlocked or mis-read
// press-index because a leg can end somewhere other than where its bin ends.
// Role comes from the steps: see legTakesLineBin (swap_leg_role.go).
//
// The INVARIANT it silently relies on: every leg the Edge builds ends on a
// DROPOFF, so "last actionable" and "last dropoff" coincide and the routing patch
// above rewrites a dropoff to itself on the happy path. A builder that ended a leg
// on a pickup would make this return a pickup node and the patch would re-aim the
// robot's final drop at it. Edge pins that invariant directly —
// TestSwapBuilders_EveryLegEndsOnADropoff (material_orders_invariant_test.go).
func extractEndpoints(steps []resolvedStep) (pickup, delivery string) {
	for _, s := range steps {
		if s.Action == protocol.ActionPickup || s.Action == protocol.ActionDropoff {
			if pickup == "" {
				pickup = s.Node
			}
			delivery = s.Node
		}
	}
	return
}

// splitAtWait returns steps up to and including the first "wait" and whether a
// wait was found. A wait-with-node produces an RDS block (BinTask=Wait) and is
// included in preWait so the robot receives the "drive to node" instruction
// before the order is staged. A bare wait (no node) is a pure split marker and
// is excluded from preWait (no block emitted).
func splitAtWait(steps []resolvedStep) (preWait []resolvedStep, hasWait bool) {
	for i, s := range steps {
		if s.Action == protocol.ActionWait {
			if s.Node != "" {
				// Wait-with-node: include it (becomes a Wait block), split after.
				return steps[:i+1], true
			}
			// Bare wait: split before (no block for this step).
			return steps[:i], true
		}
	}
	return steps, false
}

// splitSegment extracts the next segment of steps to release for a given
// waitIndex. It skips past the first (waitIndex+1) wait actions, then returns
// steps up to the next wait (or end of list). Returns the segment, whether
// more waits remain after it, and the block offset (total steps that produce
// RDS blocks before this segment) for correct block ID numbering.
//
// Wait-with-node steps produce RDS blocks (BinTask=Wait) and count toward the
// offset. Bare waits (no node) are pure split markers and do not produce blocks.
//
// Example for steps: [pickup, dropoff, wait(node), pickup, dropoff, wait, pickup, dropoff]
//
//	waitIndex=0 → segment=[pickup, dropoff] after wait₀, moreWaits=true, offset=3
//	waitIndex=1 → segment=[pickup, dropoff] after wait₁, moreWaits=false, offset=5+1
func splitSegment(steps []resolvedStep, waitIndex int) (segment []resolvedStep, moreWaits bool, blockOffset int) {
	// Find the start: skip past (waitIndex+1) wait actions.
	// waitIndex=0 means we want steps after the 1st wait.
	waitsSeen := 0
	startIdx := 0
	found := false
	for i, s := range steps {
		if s.Action == protocol.ActionWait {
			waitsSeen++
			if waitsSeen == waitIndex+1 {
				startIdx = i + 1
				found = true
				break
			}
		}
	}

	// Guard: if waitIndex exceeds the number of waits in the step list,
	// return an empty segment. This prevents a stale or duplicate release
	// from silently replaying the entire order.
	if !found {
		return nil, false, 0
	}

	// Count steps before startIdx that produce RDS blocks.
	// pickup/dropoff always produce blocks. wait-with-node produces a block
	// (BinTask=Wait). Bare waits (no node) produce no block.
	blockOffset = 0
	for i := 0; i < startIdx; i++ {
		if steps[i].Action != protocol.ActionWait || steps[i].Node != "" {
			blockOffset++
		}
	}

	// Find the end: next wait after startIdx, or end of steps.
	// A wait-with-node is included in the segment (it produces an RDS block);
	// the split happens after it. A bare wait ends the segment before it.
	endIdx := len(steps)
	for i := startIdx; i < len(steps); i++ {
		if steps[i].Action == protocol.ActionWait {
			if steps[i].Node != "" {
				// Wait-with-node: include it in segment, split after.
				endIdx = i + 1
			} else {
				// Bare wait: split before.
				endIdx = i
			}
			moreWaits = true
			break
		}
	}

	segment = steps[startIdx:endIdx]
	return
}

// stepsToBlocks converts resolved steps to fleet OrderBlocks.
// blockOffset shifts the block numbering so that post-wait blocks don't
// collide with pre-wait block IDs already submitted to RDS.
func stepsToBlocks(vendorOrderID string, steps []resolvedStep, blockOffset int) []fleet.OrderBlock {
	var blocks []fleet.OrderBlock
	for i, s := range steps {
		if s.Action == protocol.ActionWait && s.Node == "" {
			// Bare wait (no node) is a split point only — not an RDS block.
			continue
		}
		binTask := seerrds.BinTaskForAction(s.Action)
		blocks = append(blocks, fleet.OrderBlock{
			BlockID:  fmt.Sprintf("%s-b%d", vendorOrderID, blockOffset+i+1),
			Location: s.Node,
			BinTask:  binTask,
		})
	}
	return blocks
}
