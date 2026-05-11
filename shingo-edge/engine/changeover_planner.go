package engine

import (
	"fmt"
	"strings"

	"shingo/protocol"
	"shingoedge/engine/changeover"
	"shingoedge/store/processes"
)

// BuildChangeoverPlan walks the per-node diffs and emits the order plan.
// Pure: no DB writes, no engine state mutated. The planner only inspects
// the claims, per-node config, and the active-pull snapshot to decide
// what orders are needed.
//
// fallbackAutoConfirm is the engine's web auto-confirm flag, threaded through
// because the retrieve fallback order honours it.
//
// activePullByCoreNode maps CoreNodeName → bool (true = currently the
// active-pull side of an A/B pair). Sequential changeover uses this to
// decide which physical side is "inactive" (swap first, line keeps
// running) vs "active" (swap second, gated by cutover click). Other
// modes ignore it. Empty map is fine — sequential then falls through
// to the documented convention (CoreNodeName=inactive,
// PairedCoreNode=active).
//
// Press-index different-bin-type detection runs as a separate post-
// processor (FanOutPressIndexDifferentBinType in changeover.go) before
// BuildChangeoverPlan sees the diffs; per-position diffs reach the
// planner with SwapMode = "press_position" and route through the
// dedicated case in BuildSwapChangeoverSteps / BuildEvacuateChangeoverSteps.
func BuildChangeoverPlan(diffs []ChangeoverNodeDiff, nodes []processes.Node, fallbackAutoConfirm bool, activePullByCoreNode map[string]bool) changeover.Plan {
	var actions []changeover.NodeAction
	for _, diff := range diffs {
		if diff.Situation == SituationUnchanged {
			continue
		}
		node := findNodeByCoreName(nodes, diff.CoreNodeName)
		if node == nil {
			continue
		}
		action := planNodeAction(diff, node, fallbackAutoConfirm, activePullByCoreNode)
		actions = append(actions, action)
	}
	return changeover.Plan{Actions: actions}
}

// directTripChangeoverMode reports whether a SwapMode dispatches a
// changeover via direct robot trips (no InboundStaging hop, no
// OutboundStaging park). Sequential is direct-trip by design (it
// mirrors steady-state's pickup-source → dropoff-line pattern); the
// per-position synthesized claim used by the press-index fan-out is
// also direct-trip. Modes outside this set use the staging hop and
// fall through to planFallbackStagingAction when their staging
// fields are missing.
func directTripChangeoverMode(swapMode protocol.SwapMode) bool {
	return swapMode == protocol.SwapModeSequential || swapMode == pressPositionSwapMode
}

// resolveSequentialActivePull computes inactive/active node names for a
// sequential A/B-paired claim using the active-pull snapshot. Tie-break:
// when neither side reports active=true (initial startup state, or both
// PLC bits low), use the convention CoreNodeName=inactive,
// PairedCoreNode=active. Unpaired claims return empty strings; the
// planner uses that as the misconfiguration signal.
func resolveSequentialActivePull(claim *processes.NodeClaim, activePull map[string]bool) (inactive, active string) {
	if claim == nil || claim.PairedCoreNode == "" {
		return "", ""
	}
	core := claim.CoreNodeName
	paired := claim.PairedCoreNode
	if activePull[paired] && !activePull[core] {
		return core, paired // CoreNodeName is inactive, PairedCoreNode is active
	}
	if activePull[core] && !activePull[paired] {
		return paired, core // PairedCoreNode is inactive, CoreNodeName is active
	}
	// Tie-break: both active or neither active. Convention.
	return core, paired
}

func planNodeAction(diff ChangeoverNodeDiff, node *processes.Node, fallbackAutoConfirm bool, activePullByCoreNode map[string]bool) changeover.NodeAction {
	action := changeover.NodeAction{
		NodeID:    node.ID,
		NodeName:  node.Name,
		Situation: string(diff.Situation),
	}

	switch diff.Situation {
	case SituationSwap:
		if diff.FromClaim == nil || diff.ToClaim == nil {
			action.Err = fmt.Errorf("swap requires both from and to claims")
			return action
		}
		// Direct-trip modes (sequential, per-position press-index) don't
		// use a staging hop, so they bypass the staging-fallback gate.
		// two_robot and two_robot_press_index don't use OutboundStaging
		// (evac goes straight to OutboundDestination), so they only
		// require InboundStaging. single_robot (and the default
		// fallthrough) use both InboundStaging and OutboundStaging.
		// KeepStaged is a single_robot/two_robot/two_robot_press_index
		// concept; sequential and per-position skip that gate too.
		if !directTripChangeoverMode(diff.FromClaim.SwapMode) {
			switch diff.FromClaim.SwapMode {
			case protocol.SwapModeTwoRobot, protocol.SwapModeTwoRobotPressIndex:
				if diff.ToClaim.InboundStaging == "" {
					return planFallbackStagingAction(action, diff.ToClaim, fallbackAutoConfirm)
				}
			default:
				if diff.ToClaim.InboundStaging == "" || diff.FromClaim.OutboundStaging == "" {
					return planFallbackStagingAction(action, diff.ToClaim, fallbackAutoConfirm)
				}
			}
			if diff.FromClaim.KeepStaged {
				return planKeepStagedAction(action, diff.FromClaim, diff.ToClaim)
			}
		}
		// Per-mode field validation runs BEFORE the builder so missing
		// fields surface as a clear, mode-specific diagnostic instead
		// of falling out of the builder's empty-dispatch path with a
		// generic "cannot build steps" message.
		if missing := requiredChangeoverFields(diff.FromClaim, diff.ToClaim); len(missing) > 0 {
			action.Err = fmt.Errorf("node %s: %s changeover requires %s",
				node.Name, diff.FromClaim.SwapMode, formatMissingFields(missing))
			return action
		}
		// For sequential, resolve inactive/active node names from the
		// active-pull snapshot. Other modes ignore them.
		var inactive, active string
		if diff.FromClaim.SwapMode == protocol.SwapModeSequential {
			inactive, active = resolveSequentialActivePull(diff.FromClaim, activePullByCoreNode)
		}
		disp := BuildSwapChangeoverSteps(diff.FromClaim, diff.ToClaim, inactive, active)
		// Empty StepsA is the builder's "I rejected this claim" signal.
		// Per-mode field validation above already catches the known
		// rejection cases with operator-readable diagnostics; this is
		// the last-resort message for an unanticipated rejection.
		if disp.StepsA == nil {
			action.Err = fmt.Errorf("cannot build swap steps for node %s (mode %q)", node.Name, diff.FromClaim.SwapMode)
			return action
		}
		assignDispatch(&action, diff.CoreNodeName, disp)
		action.NextState = "staging_requested"
		if diff.FromClaim.SwapMode == protocol.SwapModeSequential {
			action.LogTag = "swap_sequential"
		} else {
			action.LogTag = "swap"
		}

	case SituationEvacuate:
		if diff.FromClaim == nil || diff.ToClaim == nil {
			action.Err = fmt.Errorf("evacuate requires both from and to claims")
			return action
		}
		if !directTripChangeoverMode(diff.FromClaim.SwapMode) {
			switch diff.FromClaim.SwapMode {
			case protocol.SwapModeTwoRobot, protocol.SwapModeTwoRobotPressIndex:
				if diff.ToClaim.InboundStaging == "" {
					return planFallbackStagingAction(action, diff.ToClaim, fallbackAutoConfirm)
				}
			default:
				if diff.ToClaim.InboundStaging == "" || diff.FromClaim.OutboundStaging == "" {
					return planFallbackStagingAction(action, diff.ToClaim, fallbackAutoConfirm)
				}
			}
			if diff.FromClaim.KeepStaged {
				return planKeepStagedAction(action, diff.FromClaim, diff.ToClaim)
			}
		}
		// Per-mode field validation for evacuate.
		if missing := requiredChangeoverFields(diff.FromClaim, diff.ToClaim); len(missing) > 0 {
			action.Err = fmt.Errorf("node %s: %s evacuate-changeover requires %s",
				node.Name, diff.FromClaim.SwapMode, formatMissingFields(missing))
			return action
		}
		var einactive, eactive string
		if diff.FromClaim.SwapMode == protocol.SwapModeSequential {
			einactive, eactive = resolveSequentialActivePull(diff.FromClaim, activePullByCoreNode)
		}
		disp := BuildEvacuateChangeoverSteps(diff.FromClaim, diff.ToClaim, einactive, eactive)
		if disp.StepsA == nil {
			action.Err = fmt.Errorf("cannot build evacuate steps for node %s (mode %q)", node.Name, diff.FromClaim.SwapMode)
			return action
		}
		assignDispatch(&action, diff.CoreNodeName, disp)
		action.NextState = "staging_requested"
		if diff.FromClaim.SwapMode == protocol.SwapModeSequential {
			action.LogTag = "evacuate_sequential"
		} else {
			action.LogTag = "evacuate"
		}

	case SituationAdd:
		if diff.ToClaim == nil {
			action.Err = fmt.Errorf("add requires to claim")
			return action
		}
 		// SituationAdd: the node is empty (new in this style). No swap
 		// choreography needed — just deliver a bin directly to the node.
 		// Ignore InboundStaging / swap mode entirely; those are for swap
 		// coordination where a resident bin must be evacuated first.
 		action.SupplyOrder = &changeover.OrderSpec{
 			Retrieve: &changeover.RetrieveOrderSpec{
 				RetrieveEmpty: diff.ToClaim.Role == protocol.ClaimRoleProduce,
 				DeliveryNode:  diff.ToClaim.CoreNodeName,
 				StagingNode:   "",
 				LoadType:      "standard",
 				PayloadCode:   diff.ToClaim.PayloadCode,
 				AutoConfirm:   fallbackAutoConfirm,
 			},
 		}
 		action.NextState = "staging_requested"
 		action.LogTag = "add"
 		return action

	case SituationDrop:
		if diff.FromClaim == nil {
			action.Err = fmt.Errorf("drop requires from claim")
			return action
		}
		if diff.FromClaim.OutboundDestination == "" {
			// Drop must check OutboundDestination — that's what
			// BuildReleaseSteps actually consumes. The previous gate
			// keyed on OutboundStaging, which silently skipped real
			// misconfigured nodes (the ALN_002 silent-skip incident).
			// Fail loudly so the operator sees the misconfig and the
			// apply layer refuses the whole plan.
			action.Err = fmt.Errorf("node %s has no outbound destination configured for evacuate; cannot proceed", node.Name)
			return action
		}
		releaseSteps := BuildStagedReleaseSteps(diff.FromClaim)
		if releaseSteps == nil {
			return action
		}
		// AutoConfirm=false so the staged-release flow gates on the operator's
		// release click at the lineside. The operator confirms partial count
		// via the standard release-prompt dialog before the robot picks up.
		action.EvacOrder = complexSpecWithPayload("", diff.CoreNodeName, releaseSteps, false, diff.FromClaim.PayloadCode)
		action.LogTag = "drop"
		// EvacuateOnChangeover gates whether cutover waits for this drop.
		// When false, the bin can be retrieved at leisure; cutover does
		// not depend on it, so the task is terminal from plan time. The
		// drop order still runs (operator confirms partial count at the
		// lineside release prompt and the robot returns the bin), it just
		// isn't on the cutover critical path. When true, the operator
		// marked this node as needing tool-change-style evacuation before
		// cutover; the task stays at empty_requested until the bin
		// physically leaves the line (handled in the pickup hook).
		if diff.FromClaim.EvacuateOnChangeover {
			action.NextState = "empty_requested"
		} else {
			action.NextState = "line_cleared"
		}
	}

	return action
}

// planFallbackStagingAction mirrors createFallbackStagingOrder: a single
// staging or retrieve order on the A slot, advancing to staging_requested.
func planFallbackStagingAction(action changeover.NodeAction, toClaim *processes.NodeClaim, fallbackAutoConfirm bool) changeover.NodeAction {
	if toClaim.InboundStaging != "" {
		if steps := BuildStageSteps(toClaim); steps != nil {
			action.SupplyOrder = complexSpec(toClaim.InboundStaging, toClaim.CoreNodeName, steps, false)
			action.NextState = "staging_requested"
			action.LogTag = "fallback_staging"
			return action
		}
	}
	action.SupplyOrder = &changeover.OrderSpec{
		Retrieve: &changeover.RetrieveOrderSpec{
			RetrieveEmpty: toClaim.Role == protocol.ClaimRoleProduce,
			DeliveryNode:  toClaim.CoreNodeName,
			StagingNode:   "",
			LoadType:      "standard",
			PayloadCode:   toClaim.PayloadCode,
			AutoConfirm:   fallbackAutoConfirm,
		},
	}
	action.NextState = "staging_requested"
	action.LogTag = "fallback_retrieve"
	return action
}

// planKeepStagedAction mirrors createKeepStagedChangeoverOrders.
func planKeepStagedAction(action changeover.NodeAction, fromClaim, toClaim *processes.NodeClaim) changeover.NodeAction {
	switch fromClaim.SwapMode {
	case protocol.SwapModeTwoRobot, protocol.SwapModeTwoRobotPressIndex:
		deliverSteps := BuildKeepStagedDeliverSteps(toClaim)
		evacSteps := BuildKeepStagedEvacSteps(fromClaim)
		action.SupplyOrder = complexSpec(toClaim.InboundStaging, toClaim.CoreNodeName, deliverSteps, false)
		action.EvacOrder = complexSpec("", toClaim.CoreNodeName, evacSteps, true)
		action.NextState = "staging_requested"
		action.LogTag = "keep_staged_split"
	default:
		combinedSteps := BuildKeepStagedCombinedSteps(fromClaim, toClaim)
		evacSteps := BuildKeepStagedEvacSteps(fromClaim)
		action.SupplyOrder = complexSpec(toClaim.InboundStaging, toClaim.CoreNodeName, combinedSteps, false)
		action.EvacOrder = complexSpec("", toClaim.CoreNodeName, evacSteps, true)
		action.NextState = "staging_requested"
		action.LogTag = "keep_staged_combined"
	}
	return action
}

// assignDispatch wires a ChangeoverDispatch into NodeAction.SupplyOrder/EvacOrder.
// processNode is the line node both legs belong to (CoreNodeName); empty
// DeliveryNode for the evac order lets Core resolve from the steps.
func assignDispatch(action *changeover.NodeAction, processNode string, d ChangeoverDispatch) {
	if d.StepsA != nil {
		action.SupplyOrder = complexSpec(d.DeliveryNodeA, processNode, d.StepsA, d.AutoConfirmA)
	}
	if d.StepsB != nil {
		action.EvacOrder = complexSpec("", processNode, d.StepsB, d.AutoConfirmB)
	}
}

func complexSpec(deliveryNode, processNode string, steps []protocol.ComplexOrderStep, autoConfirm bool) *changeover.OrderSpec {
	return &changeover.OrderSpec{
		Complex: &changeover.ComplexOrderSpec{
			DeliveryNode: deliveryNode,
			ProcessNode:  processNode,
			Steps:        steps,
			AutoConfirm:  autoConfirm,
		},
	}
}

func complexSpecWithPayload(deliveryNode, processNode string, steps []protocol.ComplexOrderStep, autoConfirm bool, payloadCode string) *changeover.OrderSpec {
	return &changeover.OrderSpec{
		Complex: &changeover.ComplexOrderSpec{
			DeliveryNode: deliveryNode,
			ProcessNode:  processNode,
			Steps:        steps,
			AutoConfirm:  autoConfirm,
			PayloadCode:  payloadCode,
		},
	}
}

// missingField is one entry returned by requiredChangeoverFields when
// the from/to claim pair is missing a field the per-mode changeover
// builder will need.
type missingField struct {
	// Side names which claim is missing the field — "from" or "to" —
	// so the operator-visible diagnostic points at the right claim.
	Side string
	// Name is the user-facing field name (matches the claim editor
	// labels: "Outbound Destination", "Inbound Source", etc.). Picked
	// for diagnostic readability rather than struct field accuracy.
	Name string
}

func formatMissingFields(missing []missingField) string {
	if len(missing) == 0 {
		return ""
	}
	parts := make([]string, len(missing))
	for i, m := range missing {
		parts[i] = m.Side + "-claim " + m.Name
	}
	return strings.Join(parts, ", ")
}

// requiredChangeoverFields is the per-mode validation registry. Returns
// the list of fields that are missing on the from/to claim pair for the
// selected SwapMode. Empty slice means all required fields are
// populated and the builder will succeed.
//
// The registry mirrors what each per-mode builder actually consumes —
// not steady-state required fields. Steady-state validation lives in
// store/processes/claims.go.UpsertClaim and is independent.
//
// The function is pure: no DB access, no engine state. Callable from
// unit tests with constructed claims.
func requiredChangeoverFields(fromClaim, toClaim *processes.NodeClaim) []missingField {
	if fromClaim == nil || toClaim == nil {
		return nil
	}
	var missing []missingField
	switch fromClaim.SwapMode {
	case protocol.SwapModeSingleRobot:
		// buildSingleRobotChangeoverSwap: stage + line-side swap.
		// Needs InboundStaging on to-claim (stage destination),
		// OutboundStaging on from-claim (mid-swap park), and
		// OutboundDestination on from-claim (final old-bin home).
		if toClaim.InboundStaging == "" {
			missing = append(missing, missingField{Side: "to", Name: "Inbound Staging"})
		}
		if fromClaim.OutboundStaging == "" {
			missing = append(missing, missingField{Side: "from", Name: "Outbound Staging"})
		}
		if fromClaim.OutboundDestination == "" {
			missing = append(missing, missingField{Side: "from", Name: "Outbound Destination"})
		}
	case protocol.SwapModeTwoRobot:
		// buildTwoRobotChangeoverSwap: pre-stage + ready wait +
		// deliver / evac to destination. Same fields as single_robot
		// minus OutboundStaging (Order B goes straight to destination).
		if toClaim.InboundStaging == "" {
			missing = append(missing, missingField{Side: "to", Name: "Inbound Staging"})
		}
		if fromClaim.OutboundDestination == "" {
			missing = append(missing, missingField{Side: "from", Name: "Outbound Destination"})
		}
	case protocol.SwapModeTwoRobotPressIndex:
		// Same-bin-type press-index needs PairedCoreNode and
		// OutboundDestination. The different-bin-type case fans out
		// to per-position "press_position" claims before this
		// validation runs, so only same-bin-type press-index reaches
		// here. SecondPairedCoreNode is optional (3-pos vs 2-pos
		// signal).
		if fromClaim.PairedCoreNode == "" {
			missing = append(missing, missingField{Side: "from", Name: "Paired Core Node"})
		}
		if fromClaim.OutboundDestination == "" {
			missing = append(missing, missingField{Side: "from", Name: "Outbound Destination"})
		}
	case pressPositionSwapMode:
		// Synthesized per-position claim from the press-index different-
		// bin-type fan-out. Each position's order is either a full swap
		// or one half (evac-only or refill-only) routed via
		// SituationDrop/Add. The full-swap case needs OutboundDestination
		// (where the old bin goes) and InboundSource (where the new bin
		// comes from); the half cases delegate to the existing
		// SituationDrop / SituationAdd builders which validate their
		// own fields. Validate here against the full-swap shape since
		// SituationSwap reaches this case.
		if fromClaim.OutboundDestination == "" {
			missing = append(missing, missingField{Side: "from", Name: "Outbound Destination"})
		}
		if toClaim.InboundSource == "" {
			missing = append(missing, missingField{Side: "to", Name: "Inbound Source"})
		}
	case protocol.SwapModeSequential:
		// Direct trips, no staging hop. Needs PairedCoreNode (A/B
		// paired model), OutboundDestination (where evacuated bins
		// go), and InboundSource on to-claim (where new bins come
		// from).
		if fromClaim.PairedCoreNode == "" {
			missing = append(missing, missingField{Side: "from", Name: "Paired Core Node"})
		}
		if fromClaim.OutboundDestination == "" {
			missing = append(missing, missingField{Side: "from", Name: "Outbound Destination"})
		}
		if toClaim.InboundSource == "" {
			missing = append(missing, missingField{Side: "to", Name: "Inbound Source"})
		}
	case protocol.SwapModeManualSwap:
		// manual_swap nodes don't go through changeover (per Locked
		// Decision 4 — UI removal landed in 62ad397). Don't validate;
		// if a manual_swap claim somehow gets here the existing
		// dispatcher's fallthrough handles it.
	default:
		// "simple" or unrecognized — fall through to single_robot
		// pattern per existing dispatcher; share its required fields.
		if toClaim.InboundStaging == "" {
			missing = append(missing, missingField{Side: "to", Name: "Inbound Staging"})
		}
		if fromClaim.OutboundStaging == "" {
			missing = append(missing, missingField{Side: "from", Name: "Outbound Staging"})
		}
		if fromClaim.OutboundDestination == "" {
			missing = append(missing, missingField{Side: "from", Name: "Outbound Destination"})
		}
	}
	return missing
}
