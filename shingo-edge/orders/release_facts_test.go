package orders

import (
	"testing"

	"shingo/protocol"
	"shingoedge/release"
)

// TestComplexOrderStoresItsReleaseFacts: a complex order is created with the
// release facts of its steps on its row (orders.release_facts), so the release
// layer reads them instead of decoding steps_json; an order created with no
// steps stores none, and the release reads it as any order with no plan.
func TestComplexOrderStoresItsReleaseFacts(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	m := NewManager(db, testEmitter{}, "edge.station")
	steps := []protocol.ComplexOrderStep{
		{Action: protocol.ActionWait, Node: "PRESS", WaitKind: protocol.WaitKindStation},
		{Action: protocol.ActionPickup, Node: "PRESS"},
		{Action: protocol.ActionDropoff, Node: "OUT"},
	}
	o, err := m.CreateComplexOrder(nil, 1, "OUT", "PRESS", steps, NoDemand())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := release.DecodeFacts(o.ReleaseFacts)
	if err != nil {
		t.Fatalf("stored facts do not decode (%q): %v", o.ReleaseFacts, err)
	}
	want := release.FactsFromSteps(steps, "PRESS")
	if !got.DepartsFrom("PRESS") || !got.PlacesBinAt("OUT") || got.Role != release.RoleDeparting ||
		got.DepartingStep == nil || *got.DepartingStep != *want.DepartingStep || len(got.Touches) != 2 {
		t.Errorf("stored facts %+v, want %+v", got, want)
	}

	none, err := m.CreateComplexOrder(nil, 1, "OUT", "PRESS", nil, NoDemand())
	if err != nil {
		t.Fatalf("create with no steps: %v", err)
	}
	if none.ReleaseFacts != "" {
		t.Errorf("an order with no steps stored facts %q, want none", none.ReleaseFacts)
	}
}
