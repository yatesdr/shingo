package engine

import (
	"fmt"
	"time"

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
	// Manifest is the ingest order's manifest — currently always one entry,
	// kept as a slice for protocol shape consistency. ProducedAt is the
	// RFC3339 timestamp embedded on the ingest order.
	Manifest          []protocol.IngestManifestItem
	ProducedAtRFC3339 string

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
	if p.Dispatch == nil {
		return 0
	}
	n := 1 // StepsA
	if p.Dispatch.StepsB != nil {
		n++
	}
	return n
}

// BuildProducePlan validates the (node, runtime, claim) triple and composes
// the produce-finalization plan for the claim's swap mode. Pure — no DB,
// fleet, or order-manager calls.
//
// now is the wall clock used for ProducedAt; tests inject a fixed value for
// determinism.
//
// occupancy maps core node names to their telemetry-reported occupied state
// (from engine.claimOccupancy / FetchNodeBins), same source and same
// missing-entry-means-occupied reading as the consume side. primedPositions
// marks paired positions that already have a non-terminal empty inbound, so a
// second request while the first prime is still travelling adds nothing.
//
// Validation errors are returned verbatim (no additional wrapping) so
// apply-time error surfaces stay diff-stable.
func BuildProducePlan(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, now time.Time, occupancy map[string]bool, primedPositions map[string]bool) (*ProducePlan, error) {
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
	// primedPositions gates the ORDER, not the suppression. A position that is
	// still physically bare cannot be indexed from, whether or not the empty
	// filling it is already on its way — so the swap stays suppressed for as
	// long as the position reads empty, and only the duplicate order is
	// skipped. Suppressing the order and releasing the swap together would
	// hand the second click of a double-tap exactly the un-sourceable swap
	// this branch exists to prevent.
	if claim.SwapMode == protocol.SwapModeTwoRobotPressIndex && isOccupied(occupancy, claim.CoreNodeName) {
		var bare, needsPrime []string
		for _, pos := range []string{claim.PairedCoreNode, claim.SecondPairedCoreNode} {
			if pos == "" || isOccupied(occupancy, pos) {
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

	if runtime.RemainingUOPCached <= 0 {
		return nil, fmt.Errorf("node %s has no parts to finalize", node.Name)
	}

	plan := &ProducePlan{
		Manifest: []protocol.IngestManifestItem{
			{
				PartNumber:  claim.PayloadCode,
				Quantity:    int64(runtime.RemainingUOPCached),
				Description: claim.PayloadCode,
			},
		},
		ProducedAtRFC3339: now.UTC().Format(time.RFC3339),
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
