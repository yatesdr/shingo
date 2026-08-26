package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
)

// exclusive_slot_test.go — the flag has to survive every hop, and losing it is
// silent.
//
// ── WHY THIS IS A TEST AND NOT A CODE READ ────────────────────────────────
//
// ExclusiveSlot is read off the PERSISTED steps by slotNeeds, on the intake pass
// AND on every scanner replay. So it has to survive: wire → resolvedStep →
// steps_json → resolvedStep → slotNeeds. There are five construction points that
// rebuild a step (resolveComplexSteps' two arms, reResolveComplexSteps' two, and
// stepsAsResolved), and a step rebuilt without the field carries false.
//
// FALSE IS NOT AN ERROR ANYWHERE. It is exactly the pre-fix behaviour: the node
// is not reserved, the order dispatches, and a robot finds out. So a dropped hop
// produces no panic, no log line and no failing assertion near the mistake — it
// produces a robot standing at an occupied staging node on the first retry,
// which is the failure this whole change exists to remove. The same shape as
// Empty, which is threaded through the same five points for the same reason.

// TestExclusiveSlot_SurvivesTheStepsJSONRoundTrip pins the persistence hop.
// steps_json IS the replay input; a field that does not round-trip is a field
// that works once and then stops.
//
// MUTATION (verified): drop `exclusive_slot` from resolvedStep's json tag (or
// the field). The declared step comes back false and this fails.
func TestExclusiveSlot_SurvivesTheStepsJSONRoundTrip(t *testing.T) {
	t.Parallel()
	in := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "SRC"},
		{Action: protocol.ActionDropoff, Node: "STAGING", ExclusiveSlot: true},
		{Action: protocol.ActionWait, Node: "STAGING"},
		{Action: protocol.ActionPickup, Node: "STAGING"},
		{Action: protocol.ActionDropoff, Node: "LINE"},
	}
	j, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal steps: %v", err)
	}
	var out []resolvedStep
	if err := json.Unmarshal(j, &out); err != nil {
		t.Fatalf("unmarshal steps: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("round-trip returned %d steps, want %d", len(out), len(in))
	}
	if !out[1].ExclusiveSlot {
		t.Errorf("the staging dropoff came back undeclared after a steps_json round-trip.\n"+
			"slotNeeds reads this off the persisted steps on every scanner replay, so an order "+
			"would reserve its staging node on the first pass and silently stop on the retry.\n"+
			"json: %s", j)
	}
	// And the plain dropoff must NOT acquire it — omitempty plus a bare literal
	// has to still mean "no".
	if out[4].ExclusiveSlot {
		t.Error("a plain LINE dropoff came back declared exclusive. Gating a line node re-creates " +
			"the deadlock 2b05dce fixed, so the default has to survive as false")
	}
}

// TestExclusiveSlot_StepsAsResolvedCarriesIt pins the one rebuild point that is
// pure enough to test without a database.
//
// stepsAsResolved is the capacity-error path: when intake resolution fails
// because a group is full, the ORIGINAL wire steps are preserved through it so
// DispatchPreparedComplex can re-resolve on each replay. An order that took that
// path is precisely an order that will be retried — so a field dropped here is
// dropped for exactly the population most likely to need it.
//
// MUTATION (verified): remove ExclusiveSlot from the literal in stepsAsResolved.
func TestExclusiveSlot_StepsAsResolvedCarriesIt(t *testing.T) {
	t.Parallel()
	got := stepsAsResolved([]protocol.ComplexOrderStep{
		{Action: protocol.ActionPickup, Node: "SRC"},
		{Action: protocol.ActionDropoff, Node: "STAGING", ExclusiveSlot: true},
		{Action: protocol.ActionDropoff, Node: "LINE"},
	})
	if len(got) != 3 {
		t.Fatalf("stepsAsResolved returned %d steps, want 3", len(got))
	}
	if !got[1].ExclusiveSlot {
		t.Error("stepsAsResolved dropped the declaration on the staging dropoff. This is the " +
			"group-was-full path, so the order it drops the flag for is one already guaranteed " +
			"to be replayed")
	}
	if got[2].ExclusiveSlot {
		t.Error("stepsAsResolved invented a declaration on a plain LINE dropoff")
	}
}

// TestExclusiveSlot_IsDropoffOnly states the contract the Edge side is held to,
// so that a reader of either side finds the same rule.
//
// The field is meaningless on a pickup or a wait — a robot removing a bin does
// not need the node reserved against other placements — and Core reads it only
// after testing ActionDropoff. Asserted here rather than left implicit because
// "ignored on other actions" is the kind of claim that quietly stops being true.
func TestExclusiveSlot_IsDropoffOnly(t *testing.T) {
	t.Parallel()
	// A step set where every NON-dropoff carries the flag. slotNeeds must return
	// nothing from it — the Action test comes first, and no amount of declaring
	// makes a pickup a placement.
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "SRC", ExclusiveSlot: true},
		{Action: protocol.ActionWait, Node: "STAGING", ExclusiveSlot: true},
		{Action: protocol.ActionDropoff, Node: "", ExclusiveSlot: true}, // deferred destination
	}
	a := &Allocator{} // nil db is safe: no arm below reaches isConcreteStorageDropoff
	if needs := a.slotNeeds(steps); len(needs) != 0 {
		t.Errorf("slotNeeds returned %+v for a set with no concrete dropoff — a declared PICKUP or "+
			"WAIT must not reserve a node, and a blank dropoff has no node to reserve", needs)
	}
}
