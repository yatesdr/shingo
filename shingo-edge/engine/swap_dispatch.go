package engine

import (
	"fmt"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// SwapDispatch describes the per-mode complex-order dispatch shape — what
// step lists, in what arity, with what flags. Direction-agnostic: produce
// and consume both consume this for sequential / single_robot / two_robot /
// two_robot_press_index. Modes that produce no complex orders (produce
// simple = ingest only; consume simple = bare move) bypass this entirely
// and the per-direction planner handles them.
//
// The robot doesn't care whether the bin is filling or emptying; the
// choreography is the same. SwapDispatch enforces that by being the single
// source of truth for "given this swap mode, which step lists at which
// arity with which flags?" — the produce and consume planners both call
// BuildSwapDispatch instead of duplicating the switch.
type SwapDispatch struct {
	CycleMode protocol.SwapMode

	// ProcessNode is the line node both legs belong to (= claim.CoreNodeName).
	// Threaded into ComplexOrderRequest.ProcessNode so Core can pick the
	// line bin for order.BinID at claim time and at release-time fallback.
	ProcessNode string

	StepsA        []protocol.ComplexOrderStep
	DeliveryNodeA string // empty when Core resolves from steps
	AutoConfirmA  bool

	StepsB       []protocol.ComplexOrderStep
	AutoConfirmB bool

	// RequiresActiveSwapGuard true when the apply caller must run
	// guardNoActiveSwap before dispatching. Set by modes that don't tolerate
	// overlapping swaps (two_robot, two_robot_press_index).
	RequiresActiveSwapGuard bool
}

// BuildSwapDispatch validates per-mode required fields and returns the
// dispatch for the four complex-order swap modes. Returns (nil, nil) for
// claim.SwapMode == "simple" / any unrecognised value — the per-
// direction planner is expected to handle those (consume issues a bare
// move order; produce issues an ingest-only order). Pure — no DB or fleet
// calls.
//
// Per-mode field validation matches the inline switches in
// FinalizeProduceNode and requestNodeFromClaim verbatim, so error
// messages stay diff-stable across the refactor.
//
// After building the per-mode steps, a produce claim's inbound-source pickup
// (the "fetch a fresh bin from the supermarket" leg) is marked Empty so Core
// sources and claims an EMPTY carrier to fill — the store dual of a consume
// node's full retrieve, and the same intent the simple-retrieve path already
// carries via RetrieveEmpty (changeover_planner). Without it the complex-order
// path delivers a full bin to the press.
func BuildSwapDispatch(node *processes.Node, claim *processes.NodeClaim) (*SwapDispatch, error) {
	disp, err := buildSwapDispatch(node, claim)
	if err != nil || disp == nil {
		return disp, err
	}
	if claim.Role == protocol.ClaimRoleProduce && claim.InboundSource != "" {
		markInboundEmpty(disp.StepsA, claim.InboundSource)
		markInboundEmpty(disp.StepsB, claim.InboundSource)
	}
	return disp, nil
}

// markInboundEmpty flags every pickup at inboundSource as an empty leg. The
// inbound-source pickup is the only leg that fetches a fresh carrier from the
// supermarket; the other pickups move bins already in the swap, so they keep
// their contents and must not be flagged.
func markInboundEmpty(steps []protocol.ComplexOrderStep, inboundSource string) {
	for i := range steps {
		if steps[i].Action == "pickup" && steps[i].Node == inboundSource {
			steps[i].Empty = true
		}
	}
}

func buildSwapDispatch(node *processes.Node, claim *processes.NodeClaim) (*SwapDispatch, error) {
	switch claim.SwapMode {
	case protocol.SwapModeSequential:
		return &SwapDispatch{
			CycleMode:    protocol.SwapModeSequential,
			ProcessNode:  claim.CoreNodeName,
			StepsA:       BuildSequentialRemovalSteps(claim),
			AutoConfirmA: true,
		}, nil

	case protocol.SwapModeSingleRobot:
		if claim.InboundStaging == "" || claim.OutboundStaging == "" {
			return nil, fmt.Errorf("node %s: single-robot swap requires inbound and outbound staging nodes", node.Name)
		}
		return &SwapDispatch{
			CycleMode:     protocol.SwapModeSingleRobot,
			ProcessNode:   claim.CoreNodeName,
			StepsA:        BuildSingleSwapSteps(claim),
			DeliveryNodeA: claim.CoreNodeName,
		}, nil

	case protocol.SwapModeTwoRobot:
		if claim.InboundStaging == "" {
			return nil, fmt.Errorf("node %s: two-robot swap requires inbound staging node", node.Name)
		}
		stepsA, stepsB := BuildTwoRobotSwapSteps(claim)
		return &SwapDispatch{
			CycleMode:               protocol.SwapModeTwoRobot,
			ProcessNode:             claim.CoreNodeName,
			StepsA:                  stepsA,
			DeliveryNodeA:           claim.CoreNodeName,
			StepsB:                  stepsB,
			AutoConfirmB:            true,
			RequiresActiveSwapGuard: true,
		}, nil

	case protocol.SwapModeTwoRobotPressIndex:
		if claim.PairedCoreNode == "" {
			return nil, fmt.Errorf("node %s: two_robot_press_index requires paired_core_node (back position)", node.Name)
		}
		if claim.OutboundDestination == "" {
			return nil, fmt.Errorf("node %s: two_robot_press_index requires outbound_destination", node.Name)
		}
		stepsR1, stepsR2 := BuildTwoRobotPressIndexSwapSteps(claim)
		return &SwapDispatch{
			CycleMode:               protocol.SwapModeTwoRobotPressIndex,
			ProcessNode:             claim.CoreNodeName,
			StepsA:                  stepsR1,
			DeliveryNodeA:           claim.CoreNodeName,
			StepsB:                  stepsR2,
			AutoConfirmB:            true,
			RequiresActiveSwapGuard: true,
		}, nil
	}
	return nil, nil
}
