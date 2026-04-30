package engine

import (
	"fmt"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// ConsumePlan describes everything requestNodeFromClaim will do for a
// given (node, runtime, claim) triple. Pure — no DB, fleet, or order-
// manager calls. Captures the consume-specific concerns (the simple-move
// inbound delivery and the node-empty downgrade) on top of the shared
// swap dispatch.
//
// Build with BuildConsumePlan. The corresponding Apply path lives in
// requestNodeFromClaim today; migrating it to consume a Plan is a
// follow-up.
type ConsumePlan struct {
	// Quantity is the operator-requested quantity for the order(s); plumbed
	// through unchanged from RequestNodeMaterial.
	Quantity int64
	// AutoConfirm is the merged claim+config auto-confirm signal for the
	// SimpleMove path (claim.AutoConfirm || cfg.Web.AutoConfirm). Unused
	// for Dispatch — those modes drive their own per-leg auto-confirm.
	AutoConfirm bool

	// SimpleMove is true when the apply caller should issue a single
	// CreateMoveOrder from SimpleSource → SimpleDest. Covers two cases:
	// (1) the claim's swap mode is "" / "simple" (default branch), or
	// (2) the swap mode would otherwise dispatch a swap but the node was
	//     telemetry-reported empty so the swap is downgraded to a delivery.
	// Mutually exclusive with Dispatch.
	SimpleMove                            bool
	SimpleSource, SimpleDest              string
	DowngradedFromSwapMode                string // empty unless this is the case-2 downgrade

	// Dispatch is the shared swap-mode dispatch for sequential / single_robot
	// / two_robot / two_robot_press_index. Nil when SimpleMove is true.
	Dispatch *SwapDispatch
}

// CycleMode returns the mode tag the apply caller surfaces in
// NodeOrderResult.CycleMode. "simple" for the move branch (including the
// node-empty downgrade — matches today's behavior at operator_stations.go);
// the dispatch's CycleMode otherwise.
func (p *ConsumePlan) CycleMode() string {
	if p.SimpleMove || p.Dispatch == nil {
		return "simple"
	}
	return p.Dispatch.CycleMode
}

// BuildConsumePlan validates the (node, runtime, claim) triple and
// composes the consume-request plan for the claim's swap mode. Pure — no
// DB, fleet, or order-manager calls.
//
// nodeOccupied is the result of the apply caller's pre-check against
// Core's telemetry (engine.nodeIsOccupied). When false, the planner
// downgrades any non-simple swap mode to a SimpleMove — matching the
// existing operator_stations.go behavior — so a manually removed bin
// doesn't strand the operator behind a swap that has nothing to swap out.
//
// autoConfirm is the merged claim.AutoConfirm || cfg.Web.AutoConfirm
// signal — surfaced as a parameter so the planner stays config-free.
//
// Validation errors are returned verbatim (no additional wrapping) so
// apply-time error surfaces stay diff-stable.
func BuildConsumePlan(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, quantity int64, nodeOccupied bool, autoConfirm bool) (*ConsumePlan, error) {
	if claim == nil {
		return nil, fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.Role != protocol.ClaimRoleConsume {
		return nil, fmt.Errorf("node %s is not a consume node", node.Name)
	}
	if quantity < 1 {
		quantity = 1
	}

	plan := &ConsumePlan{
		Quantity:    quantity,
		AutoConfirm: autoConfirm,
	}

	// Node-empty downgrade: nothing physically present to swap out, so
	// any non-simple mode collapses to a delivery move.
	if claim.SwapMode != "simple" && claim.SwapMode != "" && !nodeOccupied {
		if claim.InboundSource == "" {
			return nil, fmt.Errorf("node %s has no inbound source configured", node.Name)
		}
		plan.SimpleMove = true
		plan.SimpleSource = claim.InboundSource
		plan.SimpleDest = claim.CoreNodeName
		plan.DowngradedFromSwapMode = claim.SwapMode
		return plan, nil
	}

	dispatch, err := BuildSwapDispatch(node, claim)
	if err != nil {
		return nil, err
	}
	if dispatch == nil {
		// Default ("" / "simple" / unrecognised) branch — issue a bare move.
		if claim.InboundSource == "" {
			return nil, fmt.Errorf("node %s has no inbound source configured", node.Name)
		}
		plan.SimpleMove = true
		plan.SimpleSource = claim.InboundSource
		plan.SimpleDest = claim.CoreNodeName
		return plan, nil
	}
	plan.Dispatch = dispatch
	return plan, nil
}
