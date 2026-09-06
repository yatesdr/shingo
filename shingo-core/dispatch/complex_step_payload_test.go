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

// A LEG'S PAYLOAD MUST SURVIVE THE REPLAY, which is the half the first fix
// missed and the sim caught on 2026-08-25.
//
// The first resolution honoured the incoming style's carrier type and parked
// correctly when none was free — "no empty in group SYN_PRESS_EMPTIES for
// payload=PANEL-C, waiting". Then the retry rebuilt the step from resolvedStep,
// which mirrored Empty but not PayloadCode, resolved against the ORDER's payload
// (the outgoing style's), found a carrier of the type the press was leaving, and
// delivered it. Right until it retried.
//
// resolvedStep carries what a replay needs. Empty was threaded through for this
// exact reason and its comment says so; the payload is the same argument.
func TestResolvedStep_CarriesPayloadThroughReplay(t *testing.T) {
	t.Parallel()
	wire := []protocol.ComplexOrderStep{{
		Action:      protocol.ActionPickup,
		Node:        "EMPTIES",
		Empty:       true,
		PayloadCode: "PANEL-C",
	}}

	got := stepsAsResolved(wire)
	if len(got) != 1 {
		t.Fatalf("stepsAsResolved returned %d steps", len(got))
	}
	if got[0].PayloadCode != "PANEL-C" {
		t.Errorf("stepsAsResolved dropped the leg's payload (%q).\n"+
			"These are the steps a replay re-resolves from; a payload lost here means the retry "+
			"silently resolves against the ORDER's payload, which on a changeover is the style "+
			"being left.", got[0].PayloadCode)
	}
	if !got[0].Empty {
		t.Error("stepsAsResolved dropped Empty — the distinction it was threaded through to keep")
	}

	// And the round trip a replay actually performs: resolvedStep -> wire step.
	replayed := protocol.ComplexOrderStep{
		Action: got[0].Action, Node: got[0].Node, Empty: got[0].Empty,
		PayloadCode: got[0].PayloadCode, ExclusiveSlot: got[0].ExclusiveSlot,
	}
	if stepPayload(replayed, "PANEL-A") != "PANEL-C" {
		t.Errorf("after a replay the leg resolves against the order's payload again — "+
			"got %q, want the leg's PANEL-C", stepPayload(replayed, "PANEL-A"))
	}
}

// TestResolvedStepPayload_TheStepWinsOverTheOrder pins the half of this rule that
// bin SELECTION uses, and it was the half that did not follow it.
//
// resolveStepNode has always resolved WHICH NODE a leg goes to through
// stepPayload. reserveComplexPlan then chose the BIN with order.PayloadCode
// directly, so the two answered different questions about the same step: node
// resolution sent a changeover's refill leg to the slot holding the INCOMING
// material, and bin selection went to that slot looking for the OUTGOING part.
// It found the incoming bin, called it unavailable, and the changeover waited
// forever with its material in front of it.
//
// The order's payload is deliberately the FROM style on such a swap — its
// opening pickup lifts the old bin off the line, and filtering that for the new
// payload is ALN_001 — so the order-level answer is right for one leg and wrong
// for the other. Only the step can tell them apart.
func TestResolvedStepPayload_TheStepWinsOverTheOrder(t *testing.T) {
	t.Parallel()

	withOwn := resolvedStep{Node: "MARKET", PayloadCode: "INCOMING"}
	if got := resolvedStepPayload(withOwn, "OUTGOING"); got != "INCOMING" {
		t.Errorf("payload = %q, want INCOMING — a leg that names its own payload is answering a "+
			"different question from the order it rides on, and on a changeover the order's "+
			"answer is the style being left", got)
	}

	blank := resolvedStep{Node: "MARKET"}
	if got := resolvedStepPayload(blank, "OUTGOING"); got != "OUTGOING" {
		t.Errorf("payload = %q, want OUTGOING — a leg with no payload of its own still rides "+
			"the order's, which is what every non-changeover leg relies on", got)
	}

	// The two spellings of this rule must not drift: the wire form and the
	// resolved form are the same decision on either side of a replay.
	wire := protocol.ComplexOrderStep{Node: "MARKET", PayloadCode: "INCOMING"}
	if stepPayload(wire, "OUTGOING") != resolvedStepPayload(withOwn, "OUTGOING") {
		t.Error("stepPayload and resolvedStepPayload disagree — they are one rule written twice, " +
			"and a replay crosses between them")
	}
}
