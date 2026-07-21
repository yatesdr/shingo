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
	Dispatch *SwapDispatch
}

// BuildProducePlan validates the (node, runtime, claim) triple and composes
// the produce-finalization plan for the claim's swap mode. Pure — no DB,
// fleet, or order-manager calls.
//
// now is the wall clock used for ProducedAt; tests inject a fixed value for
// determinism.
//
// Validation errors are returned verbatim (no additional wrapping) so
// apply-time error surfaces stay diff-stable.
func BuildProducePlan(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, now time.Time) (*ProducePlan, error) {
	if claim == nil {
		return nil, fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.Role != protocol.ClaimRoleProduce {
		return nil, fmt.Errorf("node %s is not a produce node", node.Name)
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
