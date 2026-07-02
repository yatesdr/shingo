package dispatch

import (
	"fmt"

	"shingo/protocol"
	"shingocore/dispatch/binresolver"
	"shingocore/store/bins"
)

// ComplexPlan describes everything a complex order will do, derived purely
// from already-resolved steps plus the bin candidates available at each
// pickup node. Doing the planning before any side-effect lets the operator
// preview the order, lets tests cover the planning math without a DB, and
// gives one inspectable structure to log for incident postmortems.
//
// Build with BuildComplexPlan, then apply with ApplyComplexPlan, which
// re-walks the resolved steps over live bin state to claim each pickup and
// write the durable order/bin links. DispatchPreparedComplex runs both and
// ships the resulting fleet blocks.
type ComplexPlan struct {
	// SourceNode is the first actionable (pickup/dropoff) step's node;
	// DeliveryNode is the last. Match the values stored on the order row.
	SourceNode   string
	DeliveryNode string

	// ResolvedSteps is the full step sequence after node resolution. The
	// dispatcher persists this as StepsJSON so HandleOrderRelease can replay
	// per-segment.
	ResolvedSteps []resolvedStep

	// BinClaims is one entry per pickup step that selected a bin. The first
	// entry is the order's primary bin (the one written to Order.BinID).
	// Empty when no pickup step found a usable bin — the caller should fail
	// the order with code "no_bin" in that case, matching today's behavior.
	BinClaims []PlannedBinClaim

	// PerBinDestinations maps binID → final node name from simulating the
	// step sequence (resolvePerBinDestinations). Empty when len(BinClaims)
	// <= 1; multi-pickup orders need it to populate the order_bins junction
	// table at apply time.
	PerBinDestinations map[int64]string

	// PreWaitSteps is the segment that becomes the initial fleet block list.
	// HasWait reports whether the step list contains a split point: if true
	// the order ships staged (complete=false) and waits for HandleOrderRelease
	// to emit the rest; if false everything ships at once with complete=true.
	PreWaitSteps []resolvedStep
	HasWait      bool

	// Skips records pickup steps that did not claim a bin, with the same
	// per-step reason strings the inline path emits today. Surfaces the
	// silent-claim-failure path (the ALN_002 → SMN_003 incident class) so
	// callers can log diagnostics up front rather than relying on the
	// release-time fallback to be the only signal.
	Skips []pickupSkip
}

// PlannedBinClaim is the bin selection for one pickup step. The claim has
// not yet been written to the DB at this point — Apply will call
// ClaimForDispatch with these values.
type PlannedBinClaim struct {
	StepIndex int
	NodeName  string
	BinID     int64
	BinLabel  string

	// IsProcessNode marks the pickup that occurs at the order's source node.
	// At apply time only this bin's claim receives the operator's
	// RemainingUOP signal; storage pickups use a plain (nil) claim.
	IsProcessNode bool
}

// BuildComplexPlan composes a complex-order plan from already-resolved steps
// and the bin candidates available at each pickup node. Pure — no DB, fleet,
// or lifecycle calls.
//
// binsByNode is keyed by the dot-name of every pickup node referenced in
// steps; entries may be empty (the node resolved but had no bins) and that
// produces a "no bins at node" skip. Nodes that failed to resolve upstream
// should be omitted from binsByNode entirely; the caller is expected to
// surface those errors separately (they are not the planner's concern).
//
// processNode is the order's source node — the outgoing bin at that node is
// the one that receives the operator's RemainingUOP signal at apply time.
//
// Bin selection mirrors the live claim path (ApplyComplexPlan): for each
// pickup step, walk the candidate bins and take the first one where
// BinUnavailableReason returns "". A step with no usable candidate adds an
// entry to plan.Skips with the same per-bin reject summary the inline path
// produces.
func BuildComplexPlan(steps []resolvedStep, binsByNode map[string][]*bins.Bin, payloadCode, processNode string) *ComplexPlan {
	plan := &ComplexPlan{
		ResolvedSteps: steps,
	}
	plan.SourceNode, plan.DeliveryNode = extractEndpoints(steps)

	for i, s := range steps {
		if s.Action != protocol.ActionPickup {
			continue
		}
		candidates, ok := binsByNode[s.Node]
		if !ok || len(candidates) == 0 {
			plan.Skips = append(plan.Skips, pickupSkip{
				stepIndex: i,
				nodeName:  s.Node,
				reason:    "no bins at node",
			})
			continue
		}
		// Empty pickup leg (produce node's "bring an empty to fill"): claim an
		// EMPTY carrier, not a payload-matching full, so the plan models the same
		// selection the live claim path (ApplyComplexPlan) makes. Without this
		// filter the planner would predict a payload-matching full at an empty
		// leg and mispredict the bin on every refill order.
		claimPayload := payloadCode
		if s.Empty {
			candidates = emptyBinsOnly(candidates)
			claimPayload = ""
			if len(candidates) == 0 {
				plan.Skips = append(plan.Skips, pickupSkip{
					stepIndex: i,
					nodeName:  s.Node,
					reason:    "no empty carrier at node for empty pickup leg",
				})
				continue
			}
		}
		claim, reject := selectClaim(candidates, claimPayload)
		if claim == nil {
			plan.Skips = append(plan.Skips, pickupSkip{
				stepIndex: i,
				nodeName:  s.Node,
				reason: fmt.Sprintf("no candidate among %d bin(s); rejects: [%s]",
					len(candidates), joinRejects(reject)),
			})
			continue
		}
		plan.BinClaims = append(plan.BinClaims, PlannedBinClaim{
			StepIndex:     i,
			NodeName:      s.Node,
			BinID:         claim.ID,
			BinLabel:      claim.Label,
			IsProcessNode: s.Node == processNode,
		})
	}

	if len(plan.BinClaims) > 1 {
		claimedMap := make(map[string]int64, len(plan.BinClaims))
		for _, c := range plan.BinClaims {
			claimedMap[c.NodeName] = c.BinID
		}
		plan.PerBinDestinations = resolvePerBinDestinations(steps, claimedMap)
	}

	plan.PreWaitSteps, plan.HasWait = splitAtWait(steps)
	return plan
}

// selectClaim walks bin candidates for a single pickup step and returns the
// first eligible bin, or (nil, rejectReasons) if every candidate failed. The
// reject reasons match the strings the live claim path emits so
// log lines stay diff-stable across the refactor.
func selectClaim(candidates []*bins.Bin, payloadCode string) (*bins.Bin, []string) {
	var rejects []string
	for _, b := range candidates {
		if reason := binresolver.BinUnavailableReason(b, payloadCode); reason != "" {
			rejects = append(rejects, fmt.Sprintf("bin=%d (%s): %s", b.ID, b.Label, reason))
			continue
		}
		return b, nil
	}
	return nil, rejects
}

// distinctSourceNeeds returns the pickup steps that require sourcing a NEW bin —
// the order's distinct source needs — with relay re-grabs removed. A swap's step
// list re-picks the same bin as it relays through staging (BuildSingleSwapSteps:
// 4 pickup actions, 2 distinct bins), so the pickup-action count overstates the
// bins the order must actually find.
//
// A pickup at node N is a relay re-grab (the order re-collecting a bin it earlier
// parked at N) iff an EARLIER step is a dropoff at N: at reserve time N is empty
// (the bin hasn't relayed there yet) and the order already holds that bin, so the
// re-grab is silently skipped — not a miss. A pickup at N with no earlier
// same-order dropoff(N) is a TRUE source, and an empty N there is a real miss.
//
// Pure over the resolved step list — no live node state, which is exactly why it
// is correct at reserve time (staging nodes are still empty then). The dispatch
// reserve keys on this to distinguish "genuinely missing a distinct bin" (hold
// and keep trying) from "expected empty staging re-grab" (skip). See
// distinct_bin_pure_test.go for the swap-relay fixture.
func distinctSourceNeeds(steps []resolvedStep) []resolvedStep {
	dropped := make(map[string]bool, len(steps))
	var needs []resolvedStep
	for _, s := range steps {
		switch s.Action {
		case protocol.ActionDropoff:
			if s.Node != "" {
				dropped[s.Node] = true
			}
		case protocol.ActionPickup:
			if s.Node != "" && dropped[s.Node] {
				continue // relay re-grab of a bin this order already holds
			}
			needs = append(needs, s)
		}
	}
	return needs
}
