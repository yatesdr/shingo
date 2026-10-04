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
	// is minted. Same type and same shape as the consume side's downgrade
	// primes (consume_plan.go) — one CreateRetrieveOrder each.
	PrimePairedPositions []SimplePrime

	// SuppressSwap says this round mints the primes and NOTHING else: no
	// manifest, no dispatch, no runtime-slot write. An explicit flag rather
	// than a nil Dispatch because applyProducePlan dereferences Dispatch
	// unconditionally, and a nil there is a panic rather than a branch.
	SuppressSwap bool

	// SimpleMove is the single-robot empty-line plan: Core reports no bin on the
	// line, so there is nothing to lift and the swap collapses to one plain
	// order bringing an empty from SimpleSource. FromSpot says SimpleSource is
	// the keep-staged spot, whose standing spare is moved rather than retrieved.
	// Mutually exclusive with Dispatch and SuppressSwap.
	SimpleMove             bool
	SimpleSource           string
	FromSpot               bool
	DowngradedFromSwapMode protocol.SwapMode

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
		return 1 + p.Spot.orders()
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
// missing-entry-means-occupied reading as the consume side. primedPositions
// marks paired positions that already have a non-terminal empty inbound, so a
// second request while the first prime is still travelling adds nothing.
//
// Validation errors are returned verbatim (no additional wrapping) so
// apply-time error surfaces stay diff-stable.
//
// The plan carries no manifest: the departing bin is finalized at the
// operator's RELEASE (finalizeDepartingProduce), not at the call for parts.
func BuildProducePlan(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, occupancy map[string]bool, primedPositions map[string]bool) (*ProducePlan, error) {
	if claim == nil {
		return nil, fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.Role != protocol.ClaimRoleProduce {
		return nil, fmt.Errorf("node %s is not a produce node", node.Name)
	}

	// PARTIAL-EMPTY PRIME, deliberately ABOVE the UOP guard.
	//
	// A press-index cell with the head occupied and a paired position bare
	// mints a swap whose index leg has nothing to source: R2 is sent to move a
	// bin that is not there, and the cycle wedges. Prime the bare position(s)
	// instead and mint no swap; the next request runs the swap against a full
	// cell.
	//
	// The guard below it refuses a cell with no parts counted, and that is the
	// wrong answer HERE: a cold press reads RemainingUOPCached == 0 — at
	// Springfield the counter tag is not wired at all, so it reads 0 always —
	// and a cold press with a bare paired position is exactly the cell that
	// needs priming. Ordering these the other way makes the fix unreachable on
	// the plant it was written for.
	//
	// THE UNWIRED COUNTER HAS A SECOND READER, and this is the place a person
	// looking at RemainingUOPCached will be standing. binDrainedAtCoreNode asks
	// the same field whether a press position has been drained, and the
	// reuse-compatible-bins shortcut skips a press-index swap when it says yes.
	// At a press whose counter is not wired that predicate answers "drained" for
	// every position. It gates nothing at Springfield today — ReuseCompatibleBins
	// has no plantspec key and no fixture sets it, so it takes an operator
	// flipping the Edge column AND running a changeover — but enabling that flag
	// before wiring the counter would skip swaps that need to happen. Wire the
	// counter first.
	// primedPositions gates the ORDER, not the suppression. A position that is
	// still physically bare cannot be indexed from, whether or not the empty
	// filling it is already on its way — so the swap stays suppressed for as
	// long as the position reads empty, and only the duplicate order is
	// skipped. Suppressing the order and releasing the swap together would
	// hand the second click of a double-tap exactly the un-sourceable swap
	// this branch exists to prevent.
	if claim.SwapMode == protocol.SwapModeTwoRobotPressIndex && isOccupied(occupancy, claim.CoreNodeName) {
		var bare, needsPrime []string
		for _, pos := range claim.ExtensionPositions() {
			if isOccupied(occupancy, pos) {
				continue
			}
			bare = append(bare, pos)
			if !primedPositions[pos] {
				needsPrime = append(needsPrime, pos)
			}
		}
		if len(bare) > 0 {
			if len(needsPrime) > 0 && claim.InboundSource == "" {
				return nil, fmt.Errorf("node %s has no inbound source configured", node.Name)
			}
			plan := &ProducePlan{SuppressSwap: true}
			for _, pos := range needsPrime {
				plan.PrimePairedPositions = append(plan.PrimePairedPositions,
					SimplePrime{Source: claim.InboundSource, Dest: pos})
			}
			// len(PrimePairedPositions) == 0 here is the HOLD: every bare
			// position already has an empty inbound, so this round mints
			// nothing and waits for it to land. The caller turns that into an
			// operator-legible refusal.
			return plan, nil
		}
	}

	// AN EMPTY SINGLE-ROBOT LINE GETS AN EMPTY, the produce twin of the consume
	// side's node-empty downgrade. That swap opens by lifting the line's bin, and
	// with none there its lift holds at Core for good, so a line a cancel or a
	// person left bare could never be filled by a request. Above the count guard
	// for the press prime's reason: a bare line has nothing to finalize, and the
	// count it carries says nothing about what the line needs now.
	//
	// SINGLE-ROBOT ONLY. A two-robot request on an empty line is left as it is.
	// The same caveat as the consume downgrade holds: Core's empty also reads true
	// mid-swap, so the caller gates this plan with guardPositionSpokenFor.
	if claim.SwapMode == protocol.SwapModeSingleRobot && !isOccupied(occupancy, claim.CoreNodeName) {
		if claim.InboundSource == "" {
			return nil, fmt.Errorf("node %s has no inbound source configured", node.Name)
		}
		return &ProducePlan{SimpleMove: true, SimpleSource: claim.InboundSource,
			DowngradedFromSwapMode: claim.SwapMode}, nil
	}

	if runtime.RemainingUOPCached <= 0 {
		return nil, fmt.Errorf("node %s has no parts to finalize", node.Name)
	}

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
