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
	CycleMode string

	StepsA        []protocol.ComplexOrderStep
	DeliveryNodeA string // empty when Core resolves from steps
	AutoConfirmA  bool

	StepsB        []protocol.ComplexOrderStep
	AutoConfirmB  bool

	// RequiresActiveSwapGuard true when the apply caller must run
	// guardNoActiveSwap before dispatching. Set by modes that don't tolerate
	// overlapping swaps (two_robot, two_robot_press_index).
	RequiresActiveSwapGuard bool
}

// BuildSwapDispatch validates per-mode required fields and returns the
// dispatch for the four complex-order swap modes. Returns (nil, nil) for
// claim.SwapMode == "" / "simple" / any unrecognised value — the per-
// direction planner is expected to handle those (consume issues a bare
// move order; produce issues an ingest-only order). Pure — no DB or fleet
// calls.
//
// Per-mode field validation matches the inline switches in
// FinalizeProduceNode and requestNodeFromClaim verbatim, so error
// messages stay diff-stable across the refactor.
func BuildSwapDispatch(node *processes.Node, claim *processes.NodeClaim) (*SwapDispatch, error) {
	switch claim.SwapMode {
	case "sequential":
		return &SwapDispatch{
			CycleMode:    "sequential",
			StepsA:       BuildSequentialRemovalSteps(claim),
			AutoConfirmA: true,
		}, nil

	case "single_robot":
		if claim.InboundStaging == "" || claim.OutboundStaging == "" {
			return nil, fmt.Errorf("node %s: single-robot swap requires inbound and outbound staging nodes", node.Name)
		}
		return &SwapDispatch{
			CycleMode:     "single_robot",
			StepsA:        BuildSingleSwapSteps(claim),
			DeliveryNodeA: claim.CoreNodeName,
		}, nil

	case "two_robot":
		if claim.InboundStaging == "" {
			return nil, fmt.Errorf("node %s: two-robot swap requires inbound staging node", node.Name)
		}
		stepsA, stepsB := BuildTwoRobotSwapSteps(claim)
		return &SwapDispatch{
			CycleMode:               "two_robot",
			StepsA:                  stepsA,
			DeliveryNodeA:           claim.CoreNodeName,
			StepsB:                  stepsB,
			AutoConfirmB:            true,
			RequiresActiveSwapGuard: true,
		}, nil

	case "two_robot_press_index":
		if claim.PairedCoreNode == "" {
			return nil, fmt.Errorf("node %s: two_robot_press_index requires paired_core_node (back position)", node.Name)
		}
		if claim.OutboundDestination == "" {
			return nil, fmt.Errorf("node %s: two_robot_press_index requires outbound_destination", node.Name)
		}
		stepsR1, stepsR2 := BuildTwoRobotPressIndexSwapSteps(claim)
		return &SwapDispatch{
			CycleMode:               "two_robot_press_index",
			StepsA:                  stepsR1,
			DeliveryNodeA:           claim.CoreNodeName,
			StepsB:                  stepsR2,
			AutoConfirmB:            true,
			RequiresActiveSwapGuard: true,
		}, nil
	}
	return nil, nil
}
