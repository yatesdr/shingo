package dispatch

import (
	"fmt"
	"log"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/dispatch/binresolver"
	"shingocore/fleet"
	"shingocore/fleet/seerrds"
	"shingocore/store/orders"
	"shingocore/store/reservations"
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
func (d *Dispatcher) resolveComplexSteps(steps []protocol.ComplexOrderStep, payloadCode string, asker reservations.DigAsker) ([]resolvedStep, error) {
	var resolved []resolvedStep
	for i, step := range steps {
		// MG3-4: where this leg's carrier is GOING, for the pickup that has not
		// happened yet. See resolveStepNode.
		nextDrop := nextDropoffNode(steps, i)
		switch step.Action {
		case protocol.ActionPickup, protocol.ActionDropoff:
			// Blank dropoff = deferred destination (placeForDedicatedLoader resolves it
			// after intake). Pass it through unchanged, same as reResolveComplexSteps.
			if step.Action == protocol.ActionDropoff && step.Node == "" {
				resolved = append(resolved, resolvedStep{Action: protocol.ActionDropoff, Empty: step.Empty,
					PayloadCode: step.PayloadCode, ExclusiveSlot: step.ExclusiveSlot})
				continue
			}
			nodeName, group, err := d.resolveStepNode(step, payloadCode, asker, nextDrop)
			if err != nil {
				return nil, fmt.Errorf("step %d: %w", i, err)
			}
			resolved = append(resolved, resolvedStep{Action: step.Action, Node: nodeName, Group: group, Empty: step.Empty,
				PayloadCode: step.PayloadCode, ExclusiveSlot: step.ExclusiveSlot})
		case protocol.ActionWait:
			// Wait may optionally include a node (drive-to-and-hold).
			// If present, resolve it; otherwise it's a bare wait (split point only).
			// THE AUTHOR'S KIND IS CARRIED, NOT RE-DERIVED. This is a
			// translation from the wire type to Core's, not a new author: the
			// station stamped who owns its wait when it built the plan, and
			// dropping the stamp here would put the guessing back exactly where
			// it was — the fence would see an unowned wait and read it as the
			// station's by the drain-window default, which is right today by
			// accident and wrong the moment that window closes.
			if step.Node != "" {
				nodeName, group, err := d.resolveStepNode(step, payloadCode, asker, nextDrop)
				if err != nil {
					return nil, fmt.Errorf("step %d: %w", i, err)
				}
				resolved = append(resolved, resolvedStep{
					Action: protocol.ActionWait, Node: nodeName, Group: group, WaitKind: step.WaitKind,
				})
			} else {
				resolved = append(resolved, resolvedStep{Action: protocol.ActionWait, WaitKind: step.WaitKind})
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
func (d *Dispatcher) reResolveComplexSteps(steps []resolvedStep, payloadCode string, asker reservations.DigAsker) (newSteps []resolvedStep, changed bool, err error) {
	newSteps = make([]resolvedStep, 0, len(steps))
	for i, step := range steps {
		if step.Node == "" {
			newSteps = append(newSteps, step)
			continue
		}
		if step.WaitKind == WaitKindLane {
			// A LANE WAIT IS EXEMPT, EXPLICITLY. Its node is an RDS map point,
			// not a Core node — deliberately, so a point that never holds a bin
			// stays out of the node graph (PropLaneGatePoint). So the lookup
			// below cannot find it and it would ride the "node vanished —
			// unrecoverable" arm: the right OUTCOME (passed through untouched)
			// reached by a branch that means something else.
			//
			// That matters beyond tidiness. That arm is where a genuinely
			// missing node lands, and a reader debugging one should not have to
			// know that every gated plan takes it too. Naming the case also
			// makes it fail loudly if a plant ever configures a gate point that
			// HAPPENS to collide with a Core node name — without this, such a
			// wait would be looked up, possibly found to be an NGRP, and
			// re-resolved into a storage slot, silently re-aiming the gate.
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
		ps := protocol.ComplexOrderStep{Action: step.Action, Node: step.Node, Empty: step.Empty,
			PayloadCode: step.PayloadCode, ExclusiveSlot: step.ExclusiveSlot}
		newName, group, resolveErr := d.resolveStepNode(ps, payloadCode, asker, "")
		if resolveErr != nil {
			return steps, false, fmt.Errorf("step %d: %w", i, resolveErr)
		}
		if newName != step.Node {
			changed = true
		}
		newSteps = append(newSteps, resolvedStep{Action: step.Action, Node: newName, Group: group, Empty: step.Empty,
			PayloadCode:   step.PayloadCode,
			ExclusiveSlot: step.ExclusiveSlot})
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
		out = append(out, resolvedStep{Action: s.Action, Node: s.Node, Empty: s.Empty,
			PayloadCode: s.PayloadCode, ExclusiveSlot: s.ExclusiveSlot})
	}
	return out
}

// resolveStepNode resolves a single step's node. If the node is a synthetic
// group (NGRP), it is automatically resolved via the group resolver. If the
// node is concrete, it is returned directly. If no node is provided, the
// global fallback resolves via payload code.
//
// Every source-finding path in this function now routes through the shared
// dispatch.SourceFinder (C(i)): the last inline copies of the plant-wide bin
// finders are gone, and the forbidigo carve-out that marked them
// (exclusions rule #7, complex_steps.go arm) is deleted with them. The
// Reshuffle→blind-dispatch mappings below preserve pre-fold behaviour
// byte-for-byte; surfacing the burial is a C(ii) decision.
// stepPayload is the payload THIS step's bin selection resolves against.
//
// A leg may name its own, and exactly one kind does: the refill leg of a
// changeover swap. That order carries the FROM-style payload because its
// opening pickup has to find the bin physically on the line, while the carrier
// it fetches has to suit the style arriving. Marking the leg Empty drops the
// full-bin content match but not bin-type compatibility, which resolves against
// whatever payload reaches PayloadBinTypeAdvisoryClause — so before the step
// could say, the press was handed a carrier of the type it was leaving
// (sim 2026-08-24, N1-c).
//
// Everything else says nothing and gets the order's payload, which is what it
// has always got.
func stepPayload(step protocol.ComplexOrderStep, orderPayload string) string {
	if step.PayloadCode != "" {
		return step.PayloadCode
	}
	return orderPayload
}

func (d *Dispatcher) resolveStepNode(step protocol.ComplexOrderStep, orderPayload string,
	asker reservations.DigAsker, nextDropoff string) (string, string, error) {
	payloadCode := stepPayload(step, orderPayload)
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
				// Through the shared finder (tier 3 runs the same
				// FindEmptyCompatibleBinInGroup this used to call inline, with
				// excludeID=0 because the need carries no DeliveryNode).
				res := d.finder.FindSourceForNeed(SourceNeed{
					SourceNode:  step.Node,
					PayloadCode: payloadCode,
					ProcessNode: nextDropoff,
					Intent:      IntentEmpty,
					Asker:       asker,
				})
				switch res.Outcome {
				case OutcomeFound:
					return res.Node.Name, step.Node, nil
				case OutcomeReshuffle:
					// Pre-fold behaviour, preserved BYTE-FOR-BYTE for C(i): the
					// inline call never checked slot accessibility, so a buried
					// empty was dispatched at blind. The finder's tier-6 check
					// now SEES the burial; acting on it (reshuffle instead of a
					// robot fault at an unreachable slot) is a C(ii) decision,
					// not a ratchet side effect.
					return res.Buried.Slot.Name, step.Node, nil
				default:
					// Wait and Structural both collapse to the resolution error
					// this caller has always returned. Known string delta, DB-
					// error path only: the inline code distinguished a db error
					// ("cannot resolve empty in group") from no-bin; the finder
					// folds both into Wait.
					return "", "", fmt.Errorf("no empty carrier in group %s", step.Node)
				}
			}
			// Per STEP, not per order: one complex order's steps go both ways,
			// which is why the resolver takes a direction rather than the
			// order's type.
			mode := binresolver.ResolveModeRetrieve
			if step.Action == protocol.ActionDropoff {
				mode = binresolver.ResolveModeStore
			}
			result, err := d.resolver.Resolve(node, mode, payloadCode, nil, asker, nil)
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
			// Through the shared finder: a need with NO source node and
			// NodeLocal=false skips tiers 1-4 and lands exactly on the tier-5
			// plant-wide calls this used to make inline (preferZone "" and
			// excludeID 0 because the need carries no DeliveryNode).
			intent := IntentFull
			if step.Empty {
				intent = IntentEmpty
			}
			res := d.finder.FindSourceForNeed(SourceNeed{
				PayloadCode: payloadCode,
				Intent:      intent,
				Asker:       asker,
				ProcessNode: nextDropoff,
			})
			switch res.Outcome {
			case OutcomeFound:
				d.dbg("resolveStepNode: global fallback pickup → %s (bin %d)", res.Node.Name, res.Bin.ID)
				return res.Node.Name, "", nil
			case OutcomeReshuffle:
				// Pre-fold blind dispatch preserved — see the NGRP branch above.
				return res.Buried.Slot.Name, "", nil
			default:
				if step.Empty {
					return "", "", fmt.Errorf("no empty carrier for payload %q", payloadCode)
				}
				return "", "", fmt.Errorf("no source bin for payload %q", payloadCode)
			}
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
// swapLegHeld and deadIsEvac, both of which deadlocked or mis-read
// press-index because a leg can end somewhere other than where its bin ends.
// Role comes from the steps: see legTakesLineBin (swap_leg_role.go).
//
// The INVARIANT it silently relies on: every leg the Edge builds ends on a
// DROPOFF, so "last actionable" and "last dropoff" coincide and the routing patch
// above rewrites a dropoff to itself on the happy path. A builder that ended a leg
// on a pickup would make this return a pickup node and the patch would re-aim the
// robot's final drop at it. Edge pins that invariant directly —
// TestSwapBuilders_EveryLegEndsOnADropoff (material_orders_invariant_test.go).
//
// AND THE GATE RE-BIND IS THE ONE WRITER THAT BREAKS IT. It sets delivery_node to
// the LANE ENTRY, which on a swap is not the last dropoff — so for a rebound order
// the patch would rewrite the final leg to somewhere it does not belong, and did,
// for a week (PLAN §R.5, D1). That is why appendGateTail no longer applies the
// patch at all; the plan is already correct there, patched by carried index. The
// happy-path-self-rewrite argument above holds for the plain path only.
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

// waitAt returns the wait step an order with this wait_index is PARKED AT, and
// whether there is one.
//
// ── IT MUST COUNT WAITS EXACTLY AS splitSegment DOES ──────────────────────
//
// That is why it lives here rather than beside its caller. splitSegment skips
// past (waitIndex+1) waits to find what to release NEXT; this returns the wait
// it stopped at — the one whose tail has not gone out. Same enumeration, one
// step apart, so the two must agree on what counts as a wait or the predicate
// built on this would name a different wait than the release built on that.
//
// BOTH SPELLINGS COUNT. A bare wait (no node) emits no RDS block, but it is
// still a split point and splitSegment still counts it, so a plan whose
// operator wait is bare must not shift the numbering here. Block-EMISSION and
// wait-COUNTING are different questions and only the first cares about Node.
//
// ok=false when waitIndex is past the last wait, which is the released state:
// the shared append helper advances wait_index only after the fleet accepted
// the segment, so an order that has consumed its final wait indexes past the
// end. That is the same condition splitSegment reports as a nil segment.
func waitAt(steps []resolvedStep, waitIndex int) (resolvedStep, bool) {
	if waitIndex < 0 {
		return resolvedStep{}, false
	}
	seen := 0
	for _, s := range steps {
		if s.Action != protocol.ActionWait {
			continue
		}
		if seen == waitIndex {
			return s, true
		}
		seen++
	}
	return resolvedStep{}, false
}

// mintVendorOrderID is the ONE place an order-backed fleet order id is made.
//
// It exists next to mintBlockID because the two are one mechanism: every block id
// this system emits is "<vendorOrderID>-b<n>", so whatever makes a vendor id
// unique makes every block id under it unique too. THE ORDER ID IS WHAT DOES
// THAT. Two different orders cannot produce the same vendor id, so they cannot
// produce the same block id, whatever their block numbering does.
//
// That property was already true and held by CONVENTION — the same format string
// written out at four call sites. Nothing was broken; a fifth site written from
// memory is what this forecloses. Note what is NOT being claimed: a cross-order
// collision was not reachable before this, and this is not a fix for one.
//
// Why it matters that block ids are globally distinct rather than merely distinct
// within an order: RDS exposes /orderDetailsByBlockId/{id} and
// /blockDetailsById/{id} (rds/orders.go), which resolve a parent order from a
// bare block id. Those only make sense over a fleet-wide id space, so a
// cross-order duplicate would make a global lookup return the WRONG order rather
// than fail. Neither endpoint has a production caller today; the vendor's
// behaviour on such a duplicate is unverified and nothing here should be read as
// a statement about it.
//
// The uuid fragment is disambiguation for order REUSE (a retried order id across
// a database reset), not the uniqueness argument. Uniqueness is order.ID.
//
// Two other vendor ids are deliberately NOT minted here: the fleet test command
// (fleet/seerrds) and the send-to-node handler (www/handlers_robots.go). Neither
// has an order row, both carry their own prefix, and forcing them through a
// helper that requires an order id would mean inventing one.
func mintVendorOrderID(orderID int64) string {
	return fmt.Sprintf("%s%d-%s", VendorIDPrefix, orderID, uuid.New().String()[:8])
}

// mintBlockID hand-authors a block ID unique within a vendor order. Uniqueness
// is SEER's only contract on it (a duplicate is rejected 50001); every consumer
// — poller, telemetry, block-completion, the differential harness — treats it as
// opaque (D80e / F4c V0 finding 3). The default order-side shape is
// "<vendorOrderID>-b<n>"; the advanced-load-sequence expansion suffixes "-<k>" on
// the same base so the N same-location blocks stay distinct without renumbering
// the rest of the order.
//
// ⚠ THIS FUNCTION MUST STAY DETERMINISTIC GIVEN (vendorOrderID, n). Do not add a
// uuid, a timestamp or a counter to it. The determinism is load-bearing and it
// looks like the opposite of what it is:
//
// Two paths can append the same tail to one order concurrently — the valve and
// the lane-gate evaluator. Both are serialized and both reload before appending
// (lane_gate_dispatch.go), but that guard is Core's own. SEER's rejection of a
// duplicate blockId within an order is the backstop UNDER it, and it only works
// because two racing appends of the same tail compute the SAME id. Give them
// different ids and both are accepted: the robot gets the tail twice, which is a
// silent double dispatch rather than one success and one logged rejection.
//
// So "make collisions impossible" (mintVendorOrderID, across orders) and "keep
// collisions happening" (here, within an order) are not in tension. They are
// about different scopes, and this one is a safety net that only catches
// something because it is dumb.
func mintBlockID(vendorOrderID string, n int) string {
	return fmt.Sprintf("%s-b%d", vendorOrderID, n)
}

// stepsToBlocks converts resolved steps to fleet OrderBlocks. blockOffset shifts
// the block numbering so that post-wait blocks don't collide with pre-wait block
// IDs already submitted to RDS.
//
// loadSeq is the advanced load sequence (F4c): when non-empty, the LOAD leg —
// the first pickup step — is emitted as one same-location block per named binTask
// in the sequence (in order), replacing the single default JackLoad block. This
// is the only place the wire order gains extra blocks. Every other step (the
// delivery, waits) and every non-configured order (loadSeq nil) is byte-identical
// to before: the non-expanded path keeps the exact "<vendorOrderID>-b<offset+i+1>"
// id it always had, so unchanged orders serialize identically.
func stepsToBlocks(vendorOrderID string, steps []resolvedStep, blockOffset int, loadSeq []string) []fleet.OrderBlock {
	var blocks []fleet.OrderBlock
	loadExpanded := false
	for i, s := range steps {
		if s.Action == protocol.ActionWait && s.Node == "" {
			// Bare wait (no node) is a split point only — not an RDS block.
			continue
		}
		base := blockOffset + i + 1
		// Expand the load leg (first pickup) for a configured payload. All four
		// blocks carry the SAME location (the source); only the binTask differs,
		// matching the vendor's working same-location Postman order.
		if len(loadSeq) > 0 && !loadExpanded && s.Action == protocol.ActionPickup {
			loadExpanded = true
			for k, task := range loadSeq {
				id := mintBlockID(vendorOrderID, base)
				if k > 0 {
					id = fmt.Sprintf("%s-%d", id, k)
				}
				blocks = append(blocks, fleet.OrderBlock{
					BlockID:  id,
					Location: s.Node,
					BinTask:  task,
				})
			}
			continue
		}
		blocks = append(blocks, fleet.OrderBlock{
			BlockID:  mintBlockID(vendorOrderID, base),
			Location: s.Node,
			BinTask:  seerrds.BinTaskForAction(s.Action),
		})
	}
	return blocks
}

// widenSupplyPickups is C(ii)'s behavior change: complex SUPPLY pickups see the
// loader POOL instead of one hard-coded node.
//
// The incident shape (Springfield 74379, orders 2429/2431/2433): a supply
// pickup at an empty pinned home while the ONLY bin of that payload sat one
// slot over in the same loader's buffer. The allocator's findAvailableForNeed
// reads exactly one node, so the order terminal-skipped "no bin at any source
// node", the swap-peer unwound the partner evac, and — because the skip is
// terminal — autoreorder saw a clear lane and re-fired the pair every ~50s
// (282 skips in one day at SPR on 2026-07-21). This function is both halves of
// the fix: the WIDENING (pool hit → rewrite the step to the bin's real node)
// and, in its caller, the DISPOSITION change (pool dry → hold with a scoped
// queue reason instead of a terminal skip).
//
// Gate, per locked design: pickup ∧ !Empty ∧ !isRemovalPickup.
//   - Empty pickups keep their existing resolution paths untouched.
//   - Removal/evac pickups stay NODE-BOUND and payload-agnostic: an evac must
//     clear whatever sits at the line, never be re-routed to a pool node —
//     filtering an evac by anything is what wedged SPR ALN_006 (2026-07).
//     Orders with a blank ProcessNode degrade isRemovalPickup to
//     "never removal"; the live population with that shape is supply-shaped
//     (Springfield census 2026-07-21), and for a line-node pickup the finder's
//     tier 4 resolves the resident bin AT that node — same name, no rewrite —
//     so the degradation is inert there too. Pinned by the fixture tests.
//
// Anchor = step.Group if set, else step.Node — RE-DERIVED per tick from the
// persisted anchor, so a widened step stays re-derivable after restarts and
// the pool choice can move as bins move. Synthetic nodes are skipped (NGRP
// resolution is reResolveComplexSteps' job, which runs before this).
//
// The Group stamp on a rewrite is what keeps the reservation reconcile sane:
// matchHeldToNeed keys held reservations by the step's CURRENT node, so the
// rewrite must persist (the caller's changed-steps machinery) or every tick
// re-derives from the stale node and thrashes the hold.
//
// Returns the (possibly rewritten) steps, whether anything changed — set
// EXPLICITLY on a Group-only stamp, because the caller's persist trigger keys
// on the changed flag and a name-equality check alone cannot see a Group write
// — and the first non-Found SourceResult for a widened need (nil when every
// widened pickup resolved). The caller owns the disposition of that hold.
func (d *Dispatcher) widenSupplyPickups(order *orders.Order, steps []resolvedStep) ([]resolvedStep, bool, *SourceResult) {
	if d.finder == nil {
		return steps, false, nil
	}
	// Owner-aware guard: a `sourcing` order HOLDS partial reservations across
	// scanner retries by design (reserveComplexPlan's landmine doc). The
	// finder is owner-blind — it would reject the order's own held bin as
	// unavailable and park the order on the exact resource it already holds,
	// a self-park it could never exit. So needs covered by an own hold are the
	// reconcile's property and widening must not touch them. If the holds
	// can't be read, skip widening entirely this tick (the pre-widening
	// behavior) rather than proceed blind.
	rows, rerr := d.db.ListReservationsByOrder(order.ID)
	if rerr != nil {
		log.Printf("dispatch: widen order %d: list own reservations: %v (skipping widening this tick)", order.ID, rerr)
		return steps, false, nil
	}
	var held []*heldReservation
	if len(rows) > 0 {
		held = d.allocator.resolveHeldReservations(rows)
	}
	out := make([]resolvedStep, len(steps))
	copy(out, steps)
	changed := false
	for i := range out {
		step := out[i]
		if step.Action == protocol.ActionWait && step.WaitKind != WaitKindLane {
			// Widening stops at the first OPERATOR wait. Post-wait steps execute
			// after an Edge release, against a FUTURE world state — the classic
			// single-order swap's post-wait line pickup removes a bin that
			// arrives only once THIS order's pre-wait blocks deliver it.
			// Judging those steps against current pools would park orders
			// whose conditions resolve mid-flight. The release path owns
			// them, mirroring the block split the fleet dispatch itself
			// makes at the wait.
			//
			// A LANE WAIT IS NOT SUCH A BOUNDARY, and the distinction is the
			// whole reason the kind is on the step. What an operator wait
			// separates is two states of the WORLD: the station reports
			// something that has not happened yet. A lane wait separates two
			// states of one LANE, and everything after it works the same pools,
			// against the same inventory, as everything before it — Core is
			// merely deciding when the corridor is free.
			//
			// Stopping at one would silently un-widen every supply pickup after
			// the lane, which is the SPR-74379 failure this function exists to
			// fix (a supply pickup at an empty pinned home while the only bin of
			// that payload sat one slot over): the order terminal-skips "no bin
			// at any source node", the swap peer unwinds its partner, and
			// autoreorder re-fires the pair every ~50s. The splice inserts lane
			// waits into plans that previously had none, so without this line
			// the fix would have quietly stopped covering them.
			break
		}
		if step.Action != protocol.ActionPickup || step.Empty || isRemovalPickup(step, order.ProcessNode) {
			continue
		}
		if hb := matchHeldToNeed(held, step); hb != nil {
			// Same (node, empty-status) key the reserve reconcile matches on;
			// consume the hold so a second identical need can't ride the same
			// reservation past the finder.
			hb.used = true

			// WINDOW 4. This skip is why a complex order could wait forever on a
			// bin it can no longer reach.
			//
			// The skip itself is right and stays: the finder is owner-blind, so
			// re-resolving a need the order already holds would reject its own bin
			// and park it on the resource it is holding — a self-park with no exit.
			// But "I hold it" and "I can still get it" are different questions, and
			// only the first was being asked. A store buries the held bin: the
			// burial guard permits it (hard claims only, by ruling — a soft hold is
			// a plan), nothing on the complex path re-asks reachability, and no dig
			// is wired to a complex need the way window 2 wired the plain path. The
			// swap waits on a bin behind a wall.
			//
			// So ask the second question, and only when the first says yes.
			buried, err := d.heldNeedUnreachable(hb)
			if err != nil {
				d.dbg("widen: order %d could not check reachability of held bin %d (%v) — keeping the hold",
					order.ID, hb.binID, err)
				continue
			}
			if buried == nil {
				continue // still reachable: nothing has changed for this need
			}
			rewritten, res := d.recalcBuriedNeed(order, hb, buried, anchorFor(step), &out[i])
			if res != nil {
				return out, changed, res
			}
			changed = changed || rewritten
			continue
		}
		anchor := anchorFor(step)
		if anchor == "" {
			continue
		}
		if n, err := d.db.GetNodeByDotName(anchor); err != nil || n == nil || n.IsSynthetic {
			continue // unknown or synthetic anchor — not this seam's job
		}
		res := d.finder.FindSourceForNeed(SourceNeed{
			SourceNode:   anchor,
			PayloadCode:  order.PayloadCode,
			DeliveryNode: order.DeliveryNode,
			Intent:       IntentFull,
			NodeLocal:    true,
			Asker:        digAskerFor(order),
		})
		switch MapFinderOutcome(res) {
		case OutcomeFound:
			if res.Node.Name != step.Node {
				d.dbg("widen: order %d supply pickup %s → %s (pool hit, anchor %s)",
					order.ID, step.Node, res.Node.Name, anchor)
				out[i].Node = res.Node.Name
				out[i].Group = anchor
				changed = true
			} else if out[i].Group != anchor && step.Group != "" {
				// Anchor came from a prior rewrite's Group and the pool now
				// resolves back to a node matching the current name — keep the
				// stamp coherent, and set changed EXPLICITLY: the persist
				// trigger cannot see a Group-only write through name equality.
				out[i].Group = anchor
				changed = true
			}
		default:
			// Wait / Reshuffle / Structural — the caller disposes. First
			// blocked need wins; steps rewritten so far still persist.
			resCopy := res
			return out, changed, &resCopy
		}
	}
	return out, changed, nil
}

// anchorFor is where a supply pickup shops: its persisted Group stamp when it has
// one (the pool it was widened within), else the node it names.
func anchorFor(step resolvedStep) string {
	if step.Group != "" {
		return step.Group
	}
	return step.Node
}

// heldNeedUnreachable asks whether a bin this order already holds can still be
// GOT — the second question the own-hold skip never asked.
//
// Returns nil when the bin is reachable, or when the hold is not the kind of
// thing this question applies to (a slot row, a stray whose node could not be
// resolved, a bin outside any lane). Returns a BuriedError describing the dig
// that would clear it otherwise.
//
// A READ THAT FAILS RETURNS AN ERROR, and the caller keeps the hold. The opposite
// default would release a live reservation on a database hiccup and send a swap
// shopping for a bin it already had.
func (d *Dispatcher) heldNeedUnreachable(hb *heldReservation) (*BuriedError, error) {
	if hb.kind == reservations.KindSlot || hb.binID == 0 || hb.nodeID == 0 {
		return nil, nil
	}
	lane, err := d.db.LaneForNode(hb.nodeID)
	if err != nil {
		return nil, fmt.Errorf("resolve lane for held bin %d: %w", hb.binID, err)
	}
	if lane == nil {
		return nil, nil // not in a lane: nothing can be in front of it
	}
	blockers, err := findBuriedBlockers(d.db, hb.nodeID)
	if err != nil {
		return nil, fmt.Errorf("blockers in front of held bin %d: %w", hb.binID, err)
	}
	if len(blockers) == 0 {
		return nil, nil
	}
	bin, err := d.db.GetBin(hb.binID)
	if err != nil || bin == nil {
		return nil, fmt.Errorf("reload held bin %d: %w", hb.binID, err)
	}
	slot, err := d.db.GetNode(hb.nodeID)
	if err != nil || slot == nil {
		return nil, fmt.Errorf("reload slot %d for held bin %d: %w", hb.nodeID, hb.binID, err)
	}
	return &BuriedError{Bin: bin, Slot: slot, LaneID: lane.ID}, nil
}

// recalcBuriedNeed is the reserve treated as what it is — a plan.
//
// The order holds a bin it can no longer reach. The reservation says otherwise,
// so the reservation is wrong, and the cheapest correction is usually not a dig:
// for a fungible need, another bin of the same payload sitting in the open is
// better than excavating this one. So the hold is RELEASED and the need
// re-resolved, in that order, because the finder is owner-blind and would
// otherwise refuse to see anything while this order still holds something.
//
// Returns (rewritten, nil) when a substitute was found and the step now points at
// it. Returns (false, reshuffle result) when there is no substitute — a named
// bin, or nothing else of the payload — which the caller routes to the same
// buried-bin planner the plain path uses; the complex parent resumes after the
// dig and re-resolves, exactly as it does for a burial found at intake.
// (false, nil) means nothing could be done safely and the hold stands.
//
// THE RELEASE IS NOT UNDONE on the dig arm, deliberately. A reservation held
// across an excavation is a promise about a bin the dig may relocate, which is
// the stale bookkeeping the steal contract exists to stop. The parent re-resolves
// when it resumes; if something else takes the bin meanwhile, that is the reserve
// recalculating, which is what a reserve is for.
func (d *Dispatcher) recalcBuriedNeed(order *orders.Order, hb *heldReservation, buried *BuriedError, anchor string, step *resolvedStep) (rewritten bool, hold *SourceResult) {
	log.Printf("dispatch: complex order %d holds bin %d at %s, which is now buried — releasing the "+
		"hold and re-resolving the need (a reserve is a plan, and this one is out of date)",
		order.ID, hb.binID, hb.nodeName)

	if err := d.db.ReleaseReservation(order.ID, hb.binID); err != nil {
		// Keep the hold rather than proceed half-released: re-resolving while the
		// row still stands would let the order reserve a SECOND bin for one need.
		d.dbg("widen: order %d could not release its hold on buried bin %d (%v) — keeping it",
			order.ID, hb.binID, err)
		return false, nil
	}

	// THE ANCHOR MAY BE A GROUP, and here that is the point rather than something
	// to skip. The widen loop above skips synthetic anchors because NGRP
	// resolution is reResolveComplexSteps' job; this is a different question —
	// "is there another bin that would do" — and for a need anchored on a
	// supermarket the answer is the whole group. The finder handles both shapes:
	// its tier 1 resolves a synthetic NGRP through the group resolver, which
	// prefers ACCESSIBLE bins and only reports a burial when every candidate is
	// walled; a concrete anchor takes the node-local tier. NodeLocal keeps the
	// plant-wide scan unreachable either way — a swap shops its own supermarket,
	// not the whole plant.
	if anchor != "" {
		res := d.finder.FindSourceForNeed(SourceNeed{
			SourceNode:   anchor,
			PayloadCode:  order.PayloadCode,
			DeliveryNode: order.DeliveryNode,
			Intent:       IntentFull,
			NodeLocal:    true,
			Asker:        digAskerFor(order),
		})
		switch MapFinderOutcome(res) {
		case OutcomeFound:
			// A substitute only counts if it is a DIFFERENT bin and actually
			// reachable — re-resolving to the same buried bin, or to another one
			// behind the same wall, is the recalculation congratulating itself. The
			// group resolver already prefers accessible bins; the check also covers
			// the node-local tier, whose post-find reachability arm is empty-intent
			// only.
			if res.Bin != nil && res.Node != nil && res.Bin.ID != hb.binID {
				if reachable, rerr := d.db.IsSlotAccessible(res.Node.ID); rerr == nil && reachable {
					log.Printf("dispatch: complex order %d re-resolved its buried need to bin %d at %s "+
						"— no dig needed", order.ID, res.Bin.ID, res.Node.Name)
					step.Node = res.Node.Name
					step.Group = anchor
					return true, nil
				}
			}
		case OutcomeReshuffle:
			// Every candidate in the anchor is buried, and the finder has already
			// worked out which one is cheapest to dig. Prefer its answer to ours:
			// it looked at the whole group, this function only at the bin the order
			// happened to be holding.
			if res.Buried != nil {
				resCopy := res
				return false, &resCopy
			}
		}
	}

	// No substitute: the bin this order needs is the buried one, so dig for it.
	return false, &SourceResult{Outcome: OutcomeReshuffle, Buried: buried}
}

// nextDropoffNode names where the carrier a leg is picking up is GOING — the
// first dropoff at or after this step that names a node.
//
// ── MG3-4: OUTFLOW TYPING, AND WHY THE DESTINATION IS THE INPUT ─────────────
//
// A press empty pull has no payload to reason from: it is asking for an empty
// carrier, and "empty" says nothing about which type. The only fact available at
// selection time is the position it is going TO, and that position's effective
// bin types say what fits there.
//
// This threads the destination in. The finder decides what to do with it —
// exactly one effective type at the position means that is the want; zero or
// many leaves today's behaviour untouched, because "this slot fits anything" and
// "nobody has said" are both answered correctly by not narrowing.
//
// IT IS ALSO THE FENCE'S INPUT. A supported press reaches its own maintained
// group through the supports list, keyed on this same node. Before MG3-4 a
// complex leg carried no process identity at all, so every press-index swap was
// an outsider at every group — including the one built to serve it.
//
// INERT ON EMPTY CONFIG, deliberately. A plant with no per-position bin types
// and no maintained groups threads a node name that changes no decision. That is
// why it lands regardless of what the census says about how many positions are
// typed today: the wire has to exist before the config is worth populating.
//
// The FIRST dropoff at or after the step, not the last: a multi-leg plan drops
// at several places, and the one that matters for a pickup is where that
// carrier is set down next.
func nextDropoffNode(steps []protocol.ComplexOrderStep, from int) string {
	for i := from; i < len(steps); i++ {
		if steps[i].Action == protocol.ActionDropoff && steps[i].Node != "" {
			return steps[i].Node
		}
	}
	return ""
}
