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
	// BuildProducePlan errors on a claim with no swap. Nil on a vacant line's
	// plan (SimpleMove), and when a finalizing request with nothing counted
	// drops the swap and keeps the chain (requestProduceSwapFor); the apply's
	// swap arm runs only on a non-nil Dispatch.
	Dispatch *SwapDispatch

	// Chain is what the dependent positions of a press need before its swap can
	// index from them (ChainReadiness): the clears and the primes, created ahead
	// of the swap, or alongside the empty to a vacant line
	// (createClearsThenPrimes).
	Chain Readiness

	// SimpleMove is the empty-line plan (planVacantLine): Core reports no bin on the
	// line, so there is nothing to lift and the swap collapses to one plain
	// order bringing an empty from SimpleSource. FromSpot says SimpleSource is
	// the keep-staged spot, whose standing spare is moved rather than retrieved.
	// Mutually exclusive with Dispatch.
	SimpleMove   bool
	SimpleSource string
	FromSpot     bool
}

// OrderCount is how many ORDER ROWS applying this plan will create — the
// evacuate direction's expected_orders, and the exact mirror of
// ConsumePlan.OrderCount. See that method for why the unit matters.
func (p *ProducePlan) OrderCount() int {
	if p == nil {
		return 0
	}
	// One row per clear and per prime, then the line's delivery or the swap's
	// legs. Same unit discipline as ConsumePlan.OrderCount — see that method.
	n := p.Chain.Count()
	switch {
	case p.SimpleMove:
		n++
	case p.Dispatch != nil:
		n++ // StepsA
		if p.Dispatch.StepsB != nil {
			n++
		}
	}
	return n
}

// BuildProducePlan validates the (node, runtime, claim) triple and composes
// the produce-finalization plan for the claim's swap mode. Pure — no DB,
// fleet, or order-manager calls.
//
// occupancy maps core node names to their telemetry-reported occupied state
// (from engine.claimOccupancy, the bins table), same source and same
// missing-entry-means-occupied reading as the consume side. deck is what the
// request read of the dependent positions (ChainReadiness): what stands on
// each and whether a carrier is already on its way, so a second request while
// the first prime is still travelling adds nothing.
//
// Validation errors are returned verbatim (no additional wrapping) so
// apply-time error surfaces stay diff-stable.
//
// The plan carries no manifest: the departing bin is finalized at the
// operator's RELEASE (finalizeDepartingProduce), not at the call for parts.
func BuildProducePlan(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, occupancy map[string]bool, deck Deck) (*ProducePlan, error) {
	if claim == nil {
		return nil, fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.Role != protocol.ClaimRoleProduce {
		return nil, fmt.Errorf("node %s is not a produce node", node.Name)
	}

	// A VACANT POSITION GETS A BIN, in every mode and for both roles
	// (planVacantLine): a line with no bin gets an empty, as the consume side's
	// gets a full, instead of a swap: every swap opens by lifting the line's bin,
	// and with none there it does nothing useful (a single-robot lift holds at
	// Core for good, a two-robot or sequential removal is skipped and a
	// sequential backfill is never made). A press's dependent positions get
	// their chain (a clear of a wrong carrier, then a prime) beside the line's
	// empty, or ahead of the swap when the line holds a bin. The caller gates a
	// vacant line's delivery (gateLineRows).
	bare, err := planVacantLine(node, claim, occupancy, deck)
	if err != nil {
		return nil, err
	}
	if bare != nil && bare.dest != "" {
		return &ProducePlan{SimpleMove: true, SimpleSource: bare.source, Chain: bare.chain}, nil
	}

	// NO COUNT HERE. Whether a request may ask with nothing counted is the
	// question of the request that finalizes a filled bin (requestProduceSwapFor),
	// not of what the line needs.
	plan := &ProducePlan{}
	if bare != nil {
		plan.Chain = bare.chain
	}

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
