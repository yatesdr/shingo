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

	// Dispatch is the shared swap-mode dispatch for sequential / single_robot
	// / two_robot / two_robot_press_index. Nil when SimpleMove is true.
	Dispatch *SwapDispatch
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
	if p.SimpleMove {
		return 1 + len(p.PrimePairedPositions)
	}
	if p.Dispatch == nil {
		return 0
	}
	n := 1 // StepsA
	if p.Dispatch.StepsB != nil {
		n++
	}
	return n
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
// with guardPositionSpokenFor. See the branch comment below.
//
// For two_robot_press_index downgrades, the planner also consults
// occupancy for PairedCoreNode and SecondPairedCoreNode and emits one
// prime delivery (PrimePairedPositions) per empty paired position so
// the next cycle has bins to cascade. Paired entries missing from the
// map default to occupied=true (safe — no prime emitted).
//
// autoConfirm is the merged claim.AutoConfirm || cfg.Web.AutoConfirm
// signal — surfaced as a parameter so the planner stays config-free.
//
// Validation errors are returned verbatim (no additional wrapping) so
// apply-time error surfaces stay diff-stable.
func BuildConsumePlan(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, quantity int64, occupancy map[string]bool, autoConfirm bool) (*ConsumePlan, error) {
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

	// Node-empty downgrade: Core reports no bin on the head position, so there
	// is nothing to swap out and the swap collapses to a plain delivery move
	// regardless of mode.
	//
	// THE MAP ANSWERS A NARROWER QUESTION THAN THIS BRANCH ASKS. It was read for
	// a long time as "nothing physically present to swap out", and the sentence
	// was believable because it is true in the case the downgrade was written
	// for: somebody pulled the carrier off by hand, and the position will still
	// be bare when a robot gets there. It is false during a swap. Between the
	// robot lifting the old carrier and setting the new one down, the position
	// genuinely holds no bin AND already has one on its way, and Core — honest
	// and instantaneous — correctly answers empty. Downgrading in that window
	// mints a second delivery into a position that is about to be occupied, and
	// the robot carrying it can never put it down (sim 2026-08-31, ALN_004:
	// four cells locked in one run).
	//
	// THIS FUNCTION CANNOT TELL THE TWO APART and must not try: the witness is
	// the cell's own in-flight orders, which live in the DB, and the planner is
	// pure. requestNodeFromClaim gates this outcome with guardPositionSpokenFor
	// before applying it. A caller that applies a downgraded plan without that
	// gate is reintroducing the race.
	headOccupied := isOccupied(occupancy, claim.CoreNodeName)
	if !headOccupied {
		if claim.InboundSource == "" {
			return nil, fmt.Errorf("node %s has no inbound source configured", node.Name)
		}
		plan.SimpleMove = true
		plan.SimpleSource = claim.InboundSource
		plan.SimpleDest = claim.CoreNodeName
		plan.DowngradedFromSwapMode = claim.SwapMode
		// Press-index cascade needs B (and C, on 3-position layouts) to
		// hold bins before the next swap cycle. Prime any empty paired
		// position from the same InboundSource. Partial-empty cases
		// where the head is full but a paired position is empty are
		// intentionally out of scope here — they don't trigger this
		// downgrade and need a separate decision (refuse vs. auto-prime).
		if claim.SwapMode == protocol.SwapModeTwoRobotPressIndex {
			if claim.PairedCoreNode != "" && !isOccupied(occupancy, claim.PairedCoreNode) {
				plan.PrimePairedPositions = append(plan.PrimePairedPositions,
					SimplePrime{Source: claim.InboundSource, Dest: claim.PairedCoreNode})
			}
			if claim.SecondPairedCoreNode != "" && !isOccupied(occupancy, claim.SecondPairedCoreNode) {
				plan.PrimePairedPositions = append(plan.PrimePairedPositions,
					SimplePrime{Source: claim.InboundSource, Dest: claim.SecondPairedCoreNode})
			}
		}
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
