// flow_steady.go — a preview's steady state: the orders each cell of a draft
// makes when its position asks for a swap, as the request-time builders make
// them (FlowPreviewRequest.Steps).
//
// THERE IS NO STEP LOGIC HERE. Every leg is the output of the function the
// runtime calls for it: BuildSwapDispatch for the pair (consume_plan.go and
// produce_plan.go call it at REQUEST), BuildSequentialBackfillSteps for a
// sequential line's Order B (ensureSequentialBackfill, at A's release), and
// carrierSpec for Robot 3's carrier to an index press's back
// (followOnOnPickup, at Robot 2's lift). What this file decides is only which
// of those a cell's legs owe, and it reads that from the legs' own steps
// (followOnPositions), as the runtime does.

package engine

import (
	"shingo/protocol"
	"shingoedge/store/processes"
)

// SteadyCell is one draft cell's steady-state orders.
type SteadyCell struct {
	CoreNodeName string `json:"core_node_name"`
	Role         string `json:"role"`
	SwapMode     string `json:"swap_mode"`
	// Error is BuildSwapDispatch's refusal of the cell as drafted (a field
	// its mode needs is blank), with no legs.
	Error string `json:"error,omitempty"`
	// FollowOns is the pair's count of orders made after a lift
	// (SwapDispatch.FollowOns), the count the plan expects.
	FollowOns int `json:"follow_ons"`
	// Legs is never nil: a cell whose mode makes no complex swap has none.
	Legs []SteadyLeg `json:"legs"`
}

// SteadyLeg is one order of a cell's steady state.
type SteadyLeg struct {
	// Leg is SteadyLegA, SteadyLegB or SteadyLegFollowOn.
	Leg string `json:"leg"`
	// Position is the position a follow-on is owed to.
	Position string `json:"position,omitempty"`
	// Kind is SteadyKindComplex (Steps) or SteadyKindRetrieve (Retrieve).
	Kind     string                      `json:"kind"`
	Steps    []protocol.ComplexOrderStep `json:"steps,omitempty"`
	Retrieve *SteadyRetrieve             `json:"retrieve,omitempty"`
}

// SteadyRetrieve is a plain retrieve's fields as carrierSpec builds them.
type SteadyRetrieve struct {
	Source      string `json:"source"`
	Delivery    string `json:"delivery"`
	PayloadCode string `json:"payload_code"`
	Empty       bool   `json:"empty"`
}

const (
	SteadyLegA        = "A"
	SteadyLegB        = "B"
	SteadyLegFollowOn = "follow_on"

	SteadyKindComplex  = "complex"
	SteadyKindRetrieve = "retrieve"
)

// steadyCells is the draft's steady state, one entry per cell of the request,
// in the request's order. Pure: no read and no write.
func steadyCells(d flowDraft) []SteadyCell {
	out := make([]SteadyCell, 0, len(d.cells))
	for i := range d.cells {
		out = append(out, steadyCell(&d.cells[i].claim))
	}
	return out
}

// steadyCell is one claim's steady state.
//
// The node handed to BuildSwapDispatch is named for the claim's core node: it
// is read only for a refusal's wording, and a draft cell may have no process
// node yet.
func steadyCell(claim *processes.NodeClaim) SteadyCell {
	out := SteadyCell{CoreNodeName: claim.CoreNodeName, Role: string(claim.Role), SwapMode: string(claim.SwapMode),
		Legs: []SteadyLeg{}}
	disp, err := BuildSwapDispatch(&processes.Node{Name: claim.CoreNodeName}, claim)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	if disp == nil {
		return out
	}
	out.FollowOns = disp.FollowOns
	out.Legs = append(out.Legs, SteadyLeg{Leg: SteadyLegA, Kind: SteadyKindComplex, Steps: disp.StepsA})
	if disp.StepsB != nil {
		out.Legs = append(out.Legs, SteadyLeg{Leg: SteadyLegB, Kind: SteadyKindComplex, Steps: disp.StepsB})
	}
	// A sequential line's follow-on is its Order B: the line its removal lifts
	// and nothing sets down on.
	if claim.SwapMode == protocol.SwapModeSequential &&
		len(followOnPositions([]string{claim.CoreNodeName}, disp.StepsA, disp.StepsB)) > 0 {
		out.Legs = append(out.Legs, SteadyLeg{Leg: SteadyLegFollowOn, Position: claim.CoreNodeName,
			Kind: SteadyKindComplex, Steps: BuildSequentialBackfillSteps(claim)})
	}
	// An index press's follow-ons are Robot 3's: each position behind the front
	// the pair lifts from and sets nothing down on, as followOnOnPickup finds it
	// over the pair's steps. Quantity 1 is what followOnOnPickup asks for.
	for _, pos := range followOnPositions(claim.DependentPositions(), disp.StepsA, disp.StepsB) {
		spec := carrierSpec(claim, pos, 1)
		out.Legs = append(out.Legs, SteadyLeg{Leg: SteadyLegFollowOn, Position: pos, Kind: SteadyKindRetrieve,
			Retrieve: &SteadyRetrieve{Source: spec.Source, Delivery: spec.Delivery, PayloadCode: spec.Payload, Empty: spec.Empty}})
	}
	return out
}
