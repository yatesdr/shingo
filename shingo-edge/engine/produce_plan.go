package engine

import (
	"fmt"
	"time"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// ProducePlan describes everything FinalizeProduceNode will do for a given
// (node, runtime, claim) triple. Pure — no DB, fleet, or order-manager
// calls. Captures the produce-specific concerns (manifest the filled bin,
// reset the runtime UOP) on top of the shared swap dispatch.
//
// Build with BuildProducePlan. The corresponding Apply path lives in
// FinalizeProduceNode today; migrating it to consume a Plan is a follow-up.
type ProducePlan struct {
	// Manifest is the ingest order's manifest — currently always one entry,
	// kept as a slice for protocol shape consistency. ProducedAt is the
	// RFC3339 timestamp embedded on the ingest order.
	Manifest          []protocol.IngestManifestItem
	ProducedAtRFC3339 string
	AutoConfirmIngest bool

	// Dispatch is the shared swap-mode dispatch for sequential / single_robot
	// / two_robot / two_robot_press_index. Nil when SimpleOnly is true (no
	// complex orders, just the ingest).
	Dispatch *SwapDispatch

	// SimpleOnly is true for produce simple mode — only the ingest order is
	// dispatched, no complex orders. CycleMode in that case is "simple"
	// (Dispatch.CycleMode otherwise).
	SimpleOnly bool
}

// CycleMode returns the mode tag the apply caller surfaces in
// NodeOrderResult.CycleMode. "simple" for the ingest-only branch; the
// dispatch's CycleMode for every other mode.
func (p *ProducePlan) CycleMode() string {
	if p.SimpleOnly || p.Dispatch == nil {
		return "simple"
	}
	return p.Dispatch.CycleMode
}

// BuildProducePlan validates the (node, runtime, claim) triple and composes
// the produce-finalization plan for the claim's swap mode. Pure — no DB,
// fleet, or order-manager calls.
//
// now is the wall clock used for ProducedAt; tests inject a fixed value for
// determinism. autoConfirm is the e.cfg.Web.AutoConfirm signal — surfaced
// as a parameter so the planner stays config-free.
//
// Validation errors are returned verbatim (no additional wrapping) so
// apply-time error surfaces stay diff-stable.
func BuildProducePlan(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, autoConfirm bool, now time.Time) (*ProducePlan, error) {
	if claim == nil {
		return nil, fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.Role != protocol.ClaimRoleProduce {
		return nil, fmt.Errorf("node %s is not a produce node", node.Name)
	}
	if runtime.RemainingUOP <= 0 {
		return nil, fmt.Errorf("node %s has no parts to finalize", node.Name)
	}

	plan := &ProducePlan{
		Manifest: []protocol.IngestManifestItem{
			{
				PartNumber:  claim.PayloadCode,
				Quantity:    int64(runtime.RemainingUOP),
				Description: claim.PayloadCode,
			},
		},
		ProducedAtRFC3339: now.UTC().Format(time.RFC3339),
		AutoConfirmIngest: autoConfirm,
	}

	dispatch, err := BuildSwapDispatch(node, claim)
	if err != nil {
		return nil, err
	}
	if dispatch == nil {
		plan.SimpleOnly = true
	} else {
		plan.Dispatch = dispatch
	}
	return plan, nil
}
