package engine

import (
	"fmt"

	"shingo/protocol"
	"shingoedge/engine/changeover"
	"shingoedge/store/processes"
)

// BuildChangeoverPlan walks the per-node diffs and emits the order plan.
// Pure: no DB writes, no engine state mutated. The planner only inspects the
// claims and the per-node config to decide what orders are needed.
//
// fallbackAutoConfirm is the engine's web auto-confirm flag, threaded through
// because the retrieve fallback order honours it.
func BuildChangeoverPlan(diffs []ChangeoverNodeDiff, nodes []processes.Node, fallbackAutoConfirm bool) changeover.Plan {
	var actions []changeover.NodeAction
	for _, diff := range diffs {
		if diff.Situation == SituationUnchanged {
			continue
		}
		node := findNodeByCoreName(nodes, diff.CoreNodeName)
		if node == nil {
			continue
		}
		action := planNodeAction(diff, node, fallbackAutoConfirm)
		actions = append(actions, action)
	}
	return changeover.Plan{Actions: actions}
}

func planNodeAction(diff ChangeoverNodeDiff, node *processes.Node, fallbackAutoConfirm bool) changeover.NodeAction {
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
		if diff.ToClaim.InboundStaging == "" || diff.FromClaim.OutboundStaging == "" {
			return planFallbackStagingAction(action, diff.ToClaim, fallbackAutoConfirm)
		}
		if diff.FromClaim.KeepStaged {
			return planKeepStagedAction(action, diff.FromClaim, diff.ToClaim)
		}
		stageSteps := BuildStageSteps(diff.ToClaim)
		if stageSteps == nil {
			action.Err = fmt.Errorf("cannot build staging steps for node %s", node.Name)
			return action
		}
		swapSteps := BuildSwapChangeoverSteps(diff.FromClaim, diff.ToClaim)
		action.OrderA = complexSpec(diff.ToClaim.InboundStaging, stageSteps, false)
		action.OrderB = complexSpec("", swapSteps, true)
		action.NextState = "staging_requested"
		action.LogTag = "swap"

	case SituationEvacuate:
		if diff.FromClaim == nil || diff.ToClaim == nil {
			action.Err = fmt.Errorf("evacuate requires both from and to claims")
			return action
		}
		if diff.ToClaim.InboundStaging == "" || diff.FromClaim.OutboundStaging == "" {
			return planFallbackStagingAction(action, diff.ToClaim, fallbackAutoConfirm)
		}
		if diff.FromClaim.KeepStaged {
			return planKeepStagedAction(action, diff.FromClaim, diff.ToClaim)
		}
		stageSteps := BuildStageSteps(diff.ToClaim)
		if stageSteps == nil {
			action.Err = fmt.Errorf("cannot build staging steps for node %s", node.Name)
			return action
		}
		evacSteps := BuildEvacuateChangeoverSteps(diff.FromClaim, diff.ToClaim)
		action.OrderA = complexSpec(diff.ToClaim.InboundStaging, stageSteps, false)
		action.OrderB = complexSpec("", evacSteps, true)
		action.NextState = "staging_requested"
		action.LogTag = "evacuate"

	case SituationAdd:
		if diff.ToClaim == nil {
			action.Err = fmt.Errorf("add requires to claim")
			return action
		}
		return planFallbackStagingAction(action, diff.ToClaim, fallbackAutoConfirm)

	case SituationDrop:
		if diff.FromClaim == nil {
			action.Err = fmt.Errorf("drop requires from claim")
			return action
		}
		if diff.FromClaim.OutboundStaging == "" {
			// No outbound staging — operator must handle manually. Skip silently
			// (matches pre-refactor behaviour: createChangeoverOrders returned nil).
			return action
		}
		releaseSteps := BuildReleaseSteps(diff.FromClaim)
		if releaseSteps == nil {
			return action
		}
		action.OrderB = complexSpec("", releaseSteps, true)
		action.NextState = "empty_requested"
		action.LogTag = "drop"
	}

	return action
}

// planFallbackStagingAction mirrors createFallbackStagingOrder: a single
// staging or retrieve order on the A slot, advancing to staging_requested.
func planFallbackStagingAction(action changeover.NodeAction, toClaim *processes.NodeClaim, fallbackAutoConfirm bool) changeover.NodeAction {
	if toClaim.InboundStaging != "" {
		if steps := BuildStageSteps(toClaim); steps != nil {
			action.OrderA = complexSpec(toClaim.InboundStaging, steps, false)
			action.NextState = "staging_requested"
			action.LogTag = "fallback_staging"
			return action
		}
	}
	action.OrderA = &changeover.OrderSpec{
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
	case "two_robot", "two_robot_press_index":
		deliverSteps := BuildKeepStagedDeliverSteps(toClaim)
		evacSteps := BuildKeepStagedEvacSteps(fromClaim)
		action.OrderA = complexSpec(toClaim.InboundStaging, deliverSteps, false)
		action.OrderB = complexSpec("", evacSteps, true)
		action.NextState = "staging_requested"
		action.LogTag = "keep_staged_split"
	default:
		combinedSteps := BuildKeepStagedCombinedSteps(fromClaim, toClaim)
		evacSteps := BuildKeepStagedEvacSteps(fromClaim)
		action.OrderA = complexSpec(toClaim.InboundStaging, combinedSteps, false)
		action.OrderB = complexSpec("", evacSteps, true)
		action.NextState = "staging_requested"
		action.LogTag = "keep_staged_combined"
	}
	return action
}

func complexSpec(deliveryNode string, steps []protocol.ComplexOrderStep, autoConfirm bool) *changeover.OrderSpec {
	return &changeover.OrderSpec{
		Complex: &changeover.ComplexOrderSpec{
			DeliveryNode: deliveryNode,
			Steps:        steps,
			AutoConfirm:  autoConfirm,
		},
	}
}
