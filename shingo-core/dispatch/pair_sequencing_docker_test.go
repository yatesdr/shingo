//go:build docker

package dispatch

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestPairRule_PressIndexDispatchesBothWithTheClearerFirst is the pin for the
// mechanism that replaced the index anti-collision arm's WAIT.
//
// ── WHAT IT REPLACED, AND WHY THE WAIT HAD TO GO ──────────────────────────
//
// That arm held a filler until its clearer was COMMITTED to the fleet, so a
// robot could not drive a bin onto a press position nothing had cleared (HOP
// 2026-07, two bins on one position). It worked while the two legs dispatched
// independently. Under the pair rule it deadlocks the press permanently:
// holding the filler parks the whole pair, so the clearer never reaches its own
// fleet create and the condition the filler waits on can never come true. That
// was MEASURED — a steady-state press-index pair sat queued pass after pass,
// clearer in `sourcing`, filler in `queued`, both on swap-hold, no vendor order
// on either. Both robots and the press out until somebody cancels a leg, which
// is the exact mutual hold SYNTH-round2 warned about.
//
// The property is now carried by ORDERING instead: dispatchPairInOnePass hands
// the leg that CLEARS the shared position to the fleet before the leg that FILLS
// it. Same guarantee — the clearer is committed first — reached deterministically
// by something that waits for nothing and therefore cannot deadlock.
//
// ── THE FILLER IS CREATED FIRST HERE, DELIBERATELY ────────────────────────
//
// A press-index CHANGEOVER creates the supply (the filler) first, so it holds
// the LOWER order id. The legs are otherwise processed in ascending id order, so
// this fixture is the one where a missing sort is visible: without the
// role-ordering the filler would be committed first and the pin fails. Creating
// the clearer first would pass on id order alone and prove nothing.
func TestPairRule_PressIndexDispatchesBothWithTheClearerFirst(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, front, bp := setupTestData(t, db)
	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	back := &nodes.Node{Name: "PIQ-BACK", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(back), "create back position")
	outb := &nodes.Node{Name: "PIQ-OUT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(outb), "create outbound")
	inb := &nodes.Node{Name: "PIQ-IN", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(inb), "create inbound")

	for _, b := range []*bins.Bin{
		{BinTypeID: 1, Label: "PIQ-FRONT-BIN", NodeID: &front.ID, Status: "staged", PayloadCode: bp.Code},
		{BinTypeID: 1, Label: "PIQ-BACK-BIN", NodeID: &back.ID, Status: "staged", PayloadCode: bp.Code},
		{BinTypeID: 1, Label: "PIQ-IN-BIN", NodeID: &inb.ID, Status: "available", PayloadCode: bp.Code},
	} {
		testutil.MustNoErr(t, db.CreateBin(b), "create bin "+b.Label)
	}

	// The FILLER first, so it takes the lower order id (the changeover order).
	// pickup(back) -> dropoff(front): places on the shared position, lifts nothing
	// off it.
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "piq-filler", PayloadCode: bp.Code, Quantity: 1,
		ProcessNode: front.Name, SiblingOrderUUID: "piq-clearer",
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionWait, Node: back.Name},
			{Action: protocol.ActionPickup, Node: back.Name},
			{Action: protocol.ActionDropoff, Node: front.Name},
		},
	})
	// The CLEARER: lifts the front position's bin and fetches its own fresh
	// carrier (two pickups → legSecuresOwnReplacement).
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "piq-clearer", PayloadCode: bp.Code, Quantity: 1,
		ProcessNode: front.Name, SiblingOrderUUID: "piq-filler",
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionWait, Node: front.Name},
			{Action: protocol.ActionPickup, Node: front.Name},
			{Action: protocol.ActionDropoff, Node: outb.Name},
			{Action: protocol.ActionPickup, Node: inb.Name},
			{Action: protocol.ActionDropoff, Node: back.Name},
		},
	})

	reload := func(uuid string) *orders.Order {
		t.Helper()
		o, err := db.GetOrderByUUID(uuid)
		testutil.MustNoErr(t, err, "reload "+uuid)
		if o == nil {
			t.Fatalf("order %s missing", uuid)
		}
		return o
	}
	filler, clearer := reload("piq-filler"), reload("piq-clearer")
	if filler.ID > clearer.ID {
		t.Fatalf("fixture broken: the filler must hold the lower id (%d vs %d), or this pins nothing",
			filler.ID, clearer.ID)
	}

	// One scanner pass over both rows, exactly as scan() walks them.
	for _, uuid := range []string{"piq-filler", "piq-clearer"} {
		if o := reload(uuid); protocol.IsAcquiring(o.Status) {
			_ = d.DispatchPreparedComplex(o)
		}
	}

	filler, clearer = reload("piq-filler"), reload("piq-clearer")
	if clearer.VendorOrderID == "" || filler.VendorOrderID == "" {
		t.Fatalf("the pair did not dispatch in one pass: clearer=%q/%s filler=%q/%s (cause %q) — "+
			"a filler held on an acquiring clearer parks the pair, and the clearer can then never commit",
			clearer.VendorOrderID, clearer.Status, filler.VendorOrderID, filler.Status, filler.QueueCause)
	}

	// And the clearer reached the fleet FIRST.
	var order []string
	for _, req := range backend.CreateRequests() {
		switch req.ExternalID {
		case "piq-clearer", "piq-filler":
			order = append(order, req.ExternalID)
		}
	}
	if len(order) != 2 {
		t.Fatalf("fleet saw %d of the pair's creates (%v), want 2", len(order), order)
	}
	if order[0] != "piq-clearer" {
		t.Fatalf("fleet create order = %v, want the CLEARER first — a filler committed before the leg that "+
			"empties the position is a bin driven onto an un-cleared press (HOP 2026-07)", order)
	}
	if !strings.HasPrefix(clearer.VendorOrderID, "sg-") {
		t.Errorf("clearer vendor id = %q, want the sg- form", clearer.VendorOrderID)
	}
}
