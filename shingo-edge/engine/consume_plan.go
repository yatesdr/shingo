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
// Build with BuildConsumePlan; apply with applyConsumePlan.
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
	SimpleMove               bool
	SimpleSource, SimpleDest string
	DowngradedFromSwapMode   protocol.SwapMode // empty unless this is the case-2 downgrade

	// PrimePairedPositions are additional simple deliveries emitted
	// alongside SimpleMove for the two_robot_press_index empty-station
	// downgrade. When the head node is empty AND the paired positions
	// (PairedCoreNode / SecondPairedCoreNode) are also empty, one prime
	// per empty paired position is added so the next swap cycle has bins
	// to cascade. Each entry produces one CreateMoveOrder. Order tracking
	// stays on the head node's runtime; primes are not sibling-linked.
	PrimePairedPositions []SimplePrime

	// SuppressSwap says this round creates the primes and nothing else: a press
	// whose line holds a bin and whose paired position is bare gets that
	// position's bin, and no swap until it is there (planBareLine). The produce
	// plan's flag of the same name, for the same decision. No primes at all is
	// the hold: every bare position already has a bin on its way.
	SuppressSwap bool

	// Dispatch is the shared swap-mode dispatch for sequential / single_robot
	// / two_robot / two_robot_press_index. Nil when SimpleMove is true.
	Dispatch *SwapDispatch

	// Spot is what a keep-staged claim's spot needs from this request
	// (planSpotForConsume); zero for every other claim. The apply creates it
	// after the swap legs.
	Spot spotPlan
}

// SimplePrime describes one fire-and-forget delivery move emitted as
// part of the press-index empty-station downgrade.
type SimplePrime struct {
	Source string
	Dest   string
}

// OrderCount is how many ORDER ROWS applying this plan will create.
//
// This is a demand episode's expected_orders, and getting the UNIT right is the
// point. RequestNodeMaterial(node.ID, 1) passes ONE BIN, not one order, and the
// plan expands that bin into a variable number of rows: 1 on the simple-move
// downgrade, 2 on a swap, plus one per primed paired position on a press-index
// downgrade. Copying the literal 1 into expected_orders crosses units and reads
// 2x or more too high — which makes a healthy swap episode look like waste and
// a genuinely bad one look ordinary.
//
// Taking it from the plan means the number is the SYSTEM'S OWN STATED INTENT,
// captured once, with no per-mode special-casing: a healthy episode is ratio
// 1.0 whatever choreography it used.
//
// The validation is 2026-07-21: 484 actual over 2 expected = 242x, which is
// futility.go's independently-derived "~242/h" and the fix commit's "242
// skipped + 242 cancelled".
func (p *ConsumePlan) OrderCount() int {
	if p == nil {
		return 0
	}
	if p.SuppressSwap {
		return len(p.PrimePairedPositions)
	}
	if p.SimpleMove {
		return 1 + len(p.PrimePairedPositions) + p.Spot.orders()
	}
	if p.Dispatch == nil {
		return 0
	}
	n := 1 // StepsA
	if p.Dispatch.StepsB != nil {
		n++
	}
	return n + p.Spot.orders()
}

// BuildConsumePlan validates the (node, runtime, claim) triple and
// composes the consume-request plan for the claim's swap mode. Pure — no
// DB, fleet, or order-manager calls.
//
// occupancy maps core node names to their telemetry-reported occupied
// state (from engine.claimOccupancy / FetchNodeBins). When the head
// node (claim.CoreNodeName) is reported empty, the planner downgrades
// any non-simple swap mode to a SimpleMove — matching the existing
// operator_stations.go behavior — so a manually removed bin doesn't
// strand the operator behind a swap that has nothing to swap out. That
// downgrade is a PROPOSAL, not a decision: telemetry alone cannot tell a
// bare position from one a robot is mid-swap on, so the caller gates it
// with gateLineRows. See the branch comment below.
//
// For two_robot_press_index, the planner also consults occupancy for
// PairedCoreNode and SecondPairedCoreNode and emits one prime delivery
// (PrimePairedPositions) per empty paired position so the next cycle has
// bins to cascade, alongside the head's delivery when the head is empty and
// instead of the swap when it is not (SuppressSwap). Paired entries missing
// from the map default to occupied=true (safe — no prime emitted); inbound
// names the paired positions a bin is already on its way to.
//
// autoConfirm is the merged claim.AutoConfirm || cfg.Web.AutoConfirm
// signal — surfaced as a parameter so the planner stays config-free.
//
// Validation errors are returned verbatim (no additional wrapping) so
// apply-time error surfaces stay diff-stable.
func BuildConsumePlan(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, quantity int64, occupancy, inbound map[string]bool, autoConfirm bool) (*ConsumePlan, error) {
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

	// Node-empty downgrade: a line with no bin gets the plain delivery the bare
	// line needs (planBareLine) instead of a swap with nothing to lift, in every
	// mode. requestNodeFromClaim gates it with gateLineRows before applying
	// it. A press whose head is full and a paired position is not gets
	// that position's bin and no swap, as a produce press does, gated with
	// guardPairedPrimes.
	bare, err := planBareLine(node, claim, occupancy, inbound)
	if err != nil {
		return nil, err
	}
	if bare != nil && bare.lineHeld() {
		plan.SuppressSwap = true
		plan.PrimePairedPositions = bare.primes
		return plan, nil
	}
	if bare != nil {
		plan.SimpleMove = true
		plan.SimpleSource = bare.source
		plan.SimpleDest = bare.dest
		plan.DowngradedFromSwapMode = claim.SwapMode
		plan.PrimePairedPositions = bare.primes
		return plan, nil
	}

	dispatch, err := BuildSwapDispatch(node, claim)
	if err != nil {
		return nil, err
	}
	if dispatch == nil {
		// Bare-move fallback: BuildSwapDispatch returned nil (an unrecognised or
		// unconfigured mode, or a legacy "simple" row with an occupied head).
		// Issue a single delivery move.
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

// isOccupied reads the occupancy map with the same missing-entry-means-
// occupied default the apply caller applies to Core telemetry failures.
// Keeps the downgrade trigger and the paired-prime check on identical
// semantics so a Core blip can't half-fire the downgrade.
func isOccupied(occupancy map[string]bool, coreNodeName string) bool {
	if coreNodeName == "" {
		return true
	}
	occ, ok := occupancy[coreNodeName]
	if !ok {
		return true
	}
	return occ
}
