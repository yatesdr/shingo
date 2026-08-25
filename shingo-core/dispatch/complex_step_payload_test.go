package dispatch

import (
	"testing"

	"shingo/protocol"
)

// N1-c — a leg may name the payload its bin selection resolves against.
//
// The case is one order doing two jobs: a changeover swap lifts the outgoing
// style's bin off the line (so the order carries the from-style payload, or its
// opening pickup finds nothing) and fetches the incoming style's carrier on the
// same trip. `Empty` drops the full-bin content match; it does NOT drop
// bin-type compatibility, which resolves against the payload that reaches
// PayloadBinTypeAdvisoryClause. So the refill fetched a carrier of the type the
// press was LEAVING — wrong carrier one direction, and in the other an
// unsatisfiable wait for a type the plant had none of, which parked two supply
// legs on the sim until an operator abandoned them.
func TestStepPayload_StepOverridesTheOrder(t *testing.T) {
	t.Parallel()
	step := protocol.ComplexOrderStep{
		Action:      protocol.ActionPickup,
		Node:        "EMPTIES",
		Empty:       true,
		PayloadCode: "PANEL-C",
	}
	if got := stepPayload(step, "PANEL-A"); got != "PANEL-C" {
		t.Errorf("stepPayload = %q, want the STEP's PANEL-C.\n"+
			"The order carries the outgoing style's payload by necessity; the refill leg has to "+
			"be able to say it is for the incoming one, or the press gets the carrier type it is "+
			"leaving.", got)
	}
}

// The default is unchanged for every leg that says nothing, which is all of
// them but one.
func TestStepPayload_SilentStepUsesTheOrder(t *testing.T) {
	t.Parallel()
	step := protocol.ComplexOrderStep{Action: protocol.ActionPickup, Node: "MARKET"}
	if got := stepPayload(step, "PANEL-A"); got != "PANEL-A" {
		t.Errorf("stepPayload = %q, want the order's PANEL-A — a step that names no payload "+
			"must resolve exactly as it did before this field existed", got)
	}
	if got := stepPayload(step, ""); got != "" {
		t.Errorf("stepPayload = %q, want empty — an order with no payload and a step with none "+
			"has nothing to resolve against, and inventing one here would be a guess", got)
	}
}
