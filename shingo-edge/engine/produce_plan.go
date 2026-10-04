package engine

import (
	"fmt"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// ProducePlan describes everything RequestProduceSwap will do for a given
// (node, runtime, claim) triple. Pure — no DB, fleet, or order-manager
// calls. Captures the produce-specific concerns (manifest the filled bin,
// reset the runtime UOP) on top of the shared swap dispatch.
//
// Build with BuildProducePlan; apply with applyProducePlan.
type ProducePlan struct {
	// Dispatch is the shared swap-mode dispatch for sequential / single_robot /
	// two_robot / two_robot_press_index. Produce always has a swap mode now, so
	// Dispatch is always set — BuildProducePlan errors on a claim with no swap.
	// Nil ONLY on a primes-only plan (SuppressSwap); every other path sets it.
	Dispatch *SwapDispatch

	// PrimePairedPositions are the fire-and-forget empty deliveries that fill
	// a two_robot_press_index cell's bare paired position(s) before any swap
	// is minted, or alongside the empty to a bare line (SimpleMove). Same type
	// and shape as the consume side's downgrade primes (consume_plan.go) — one
	// CreateRetrieveOrder each.
	PrimePairedPositions []SimplePrime

	// SuppressSwap says this round mints the primes and NOTHING else: no
	// manifest, no dispatch, no runtime-slot write. An explicit flag rather
	// than a nil Dispatch because applyProducePlan dereferences Dispatch
	// unconditionally, and a nil there is a panic rather than a branch.
	SuppressSwap bool

	// SimpleMove is the empty-line plan (planBareLine): Core reports no bin on the
	// line, so there is nothing to lift and the swap collapses to one plain
	// order bringing an empty from SimpleSource. FromSpot says SimpleSource is
	// the keep-staged spot, whose standing spare is moved rather than retrieved.
	// Mutually exclusive with Dispatch and SuppressSwap.
	SimpleMove   bool
	SimpleSource string
	FromSpot     bool

	// Spot is what a keep-staged claim's spot needs from this request
	// (planSpotForProduce); zero for every other claim.
	Spot spotPlan
}

// OrderCount is how many ORDER ROWS applying this plan will create — the
// evacuate direction's expected_orders, and the exact mirror of
// ConsumePlan.OrderCount. See that method for why the unit matters.
func (p *ProducePlan) OrderCount() int {
	if p == nil {
		return 0
	}
	// A primes-only round creates exactly one order row per prime and no swap
	// legs. Same unit discipline as ConsumePlan.OrderCount — see that method.
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

// BuildProducePlan validates the (node, runtime, claim) triple and composes
// the produce-finalization plan for the claim's swap mode. Pure — no DB,
// fleet, or order-manager calls.
//
// occupancy maps core node names to their telemetry-reported occupied state
// (from engine.claimOccupancy / FetchNodeBins), same source and same
// missing-entry-means-occupied reading as the consume side. inbound marks
// paired positions that already have a bin on its way, so a second request
// while the first prime is still travelling adds nothing.
//
// Validation errors are returned verbatim (no additional wrapping) so
// apply-time error surfaces stay diff-stable.
//
// The plan carries no manifest: the departing bin is finalized at the
// operator's RELEASE (finalizeDepartingProduce), not at the call for parts.
func BuildProducePlan(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, occupancy, inbound map[string]bool) (*ProducePlan, error) {
	if claim == nil {
		return nil, fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.Role != protocol.ClaimRoleProduce {
		return nil, fmt.Errorf("node %s is not a produce node", node.Name)
	}

	// A BARE POSITION GETS A BIN, in every mode and for both roles
	// (planBareLine): a line with no bin gets an empty, as the consume side's
	// gets a full, plus one for each bare paired position of a press; a press
	// whose line holds a bin and whose paired position is bare gets that
	// position's empty and no swap. Every swap opens by lifting the line's bin,
	// and with none there it does nothing useful: a single-robot lift holds at
	// Core for good, a two-robot or sequential removal is skipped by Core and a
	// sequential backfill is never made, and a press's index leg holds at a bare
	// paired position. The caller gates this plan (positionWorkedBy,
	// guardPairedPrimes).
	bare, err := planBareLine(node, claim, occupancy, inbound)
	if err != nil {
		return nil, err
	}
	if bare != nil && bare.lineHeld() {
		return &ProducePlan{SuppressSwap: true, PrimePairedPositions: bare.primes}, nil
	}
	if bare != nil {
		return &ProducePlan{SimpleMove: true, SimpleSource: bare.source, PrimePairedPositions: bare.primes}, nil
	}

	// NO COUNT HERE. Whether a request may ask with nothing counted is the
	// question of the request that finalizes a filled bin (requestProduceSwapFor),
	// not of what the line needs.
	plan := &ProducePlan{}

	dispatch, err := BuildSwapDispatch(node, claim)
	if err != nil {
		return nil, err
	}
	if dispatch == nil {
		// Produce is always a swap now — simple-mode produce (bare ingest, no
		// swap) was retired. A nil dispatch means a legacy claim with no swap
		// mode configured; fail loud rather than mint a bare manifest cycle.
		return nil, fmt.Errorf("node %s: produce requires a swap mode (simple produce retired)", node.Name)
	}
	plan.Dispatch = dispatch
	return plan, nil
}
