//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// TestCompound_HeldLegResumesWhenTheLaneClears is the wedge, and it asserts the
// RESUMPTION rather than the refusal.
//
// A test that ends at "it was refused" is green while the system is wedged
// forever, which is the whole failure here (DESIGN §16 rule 9). So the refusal is
// only the setup; the assertion is that the leg goes out.
//
// THE SCENARIO IS THE ONE WITH NO SIBLING. AdvanceCompoundOrder's own comment
// names its releaser — "the sibling's dropoff completion releases its occupancy
// and re-enters this function" — and that releaser exists only while a sibling is
// in flight. Here a FOREIGN order holds the lane and no sibling is running, so
// nothing was ever going to come back: the leg stays `pending`, writes no status,
// and every re-drive is keyed on events that will not fire.
//
// MUTATION: drop the RedriveHeldCompoundLegs call and the leg never dispatches —
// the foreign order leaves, the lane is free, and the leg sits pending forever.
func TestCompound_HeldLegResumesWhenTheLaneClears(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	parent, children, lane, _ := twoLegCompound(t, db, "HELD")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// A FOREIGN order is inside the lane — not a sibling, so its completion is
	// not wired to re-drive this parent.
	foreign := digHolder(t, db, "HELD-foreign-occupant")
	if err := reservations.AcquireOccupancy(db.DB, foreign.ID, lane); err != nil {
		t.Fatalf("foreign occupancy: %v", err)
	}

	if err := d.AdvanceCompoundOrder(parent.ID); err != nil {
		t.Fatalf("advance (held): %v", err)
	}

	held, err := db.GetOrder(children[0].ID)
	if err != nil {
		t.Fatalf("get held leg: %v", err)
	}
	if held.Status != StatusPending {
		t.Fatalf("held leg status = %s, want pending (the lane is occupied by a foreign order)", held.Status)
	}

	// (a) THE REFUSAL IS VISIBLE. Every other wait in the system records why;
	// this one used to leave nothing but a debug log that is nil unless DebugLog
	// is wired.
	if held.QueueCode == "" && held.QueueCause == "" {
		t.Error("a leg held at its lane wrote NOTHING to its row — no queue_code, no queue_cause. " +
			"It is indistinguishable from a leg nobody has looked at yet")
	}
	if held.QueueCause != string(CauseLaneOccupied) {
		t.Errorf("queue_cause = %q, want %q", held.QueueCause, CauseLaneOccupied)
	}
	if held.QueueCode != string(protocol.QueueWaitingForSlot) {
		t.Errorf("queue_code = %q, want %q", held.QueueCode, protocol.QueueWaitingForSlot)
	}

	// THE LANE CLEARS — the foreign order places its bin and leaves.
	if err := reservations.ReleaseAllOccupancy(db.DB, foreign.ID); err != nil {
		t.Fatalf("release foreign occupancy: %v", err)
	}

	// (b) THE LANE-CLEARING EVENT IS THE WAKE-UP. This is what the engine's
	// lane-gate trigger set calls on every event that frees a lane — not a timer,
	// and not a sibling completing (there is no sibling).
	d.RedriveHeldCompoundLegs(lane)

	if !inFlight(t, db, children[0].ID) {
		after, _ := db.GetOrder(children[0].ID)
		t.Fatalf("the held leg did not resume when its lane cleared — status %s, vendor %q. "+
			"Nothing re-drives a leg whose refusal wrote no status: its named releaser is a "+
			"sibling's dropoff, and there is no sibling", after.Status, after.VendorOrderID)
	}

	// The cause is cleared once it goes out, so the row does not keep claiming a
	// wait that ended.
	out, err := db.GetOrder(children[0].ID)
	if err != nil {
		t.Fatalf("get dispatched leg: %v", err)
	}
	if out.QueueCode != "" || out.QueueCause != "" {
		t.Errorf("dispatched leg still carries queue_code=%q queue_cause=%q; want both cleared",
			out.QueueCode, out.QueueCause)
	}
}

// TestCompound_HeldLegOnItsDESTINATIONLaneResumes — the blocking lane is not
// always the source.
//
// An unbury leg is held on the lane it picks FROM, which is the case the sibling
// test covers. A STORE leg is held on the lane it drops INTO: admission asks the
// same questions of the destination (admission.go admits source first, then
// dest), and BRIEF-5c records a foreign dig on the DESTINATION lane holding a
// child. A re-drive that looked only at source_node would leave every
// destination-held leg exactly as wedged as before, and the source-side test
// would still be green — the failure would be invisible in the same way the
// original wedge was.
func TestCompound_HeldLegOnItsDESTINATIONLaneResumes(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	lane := mirrorLane(t, db, "DESTHELD-LANE", 3)
	slots, err := db.ListLaneSlots(lane)
	if err != nil || len(slots) < 1 {
		t.Fatalf("list lane slots: %v (got %d)", err, len(slots))
	}
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	parent := &orders.Order{
		EdgeUUID: "DESTHELD-parent", StationID: "line-1",
		OrderType: OrderTypeRetrieve, Status: StatusReshuffling, Quantity: 1,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent")

	// A STORE leg: picks from ordinary storage, drops INTO the lane.
	child := &orders.Order{
		EdgeUUID: "DESTHELD-child", StationID: "line-1",
		OrderType: OrderTypeMove, Status: StatusPending, Quantity: 1,
		ParentOrderID: &parent.ID, Sequence: 1,
		SourceNode: sd.StorageNode.Name, DeliveryNode: slots[0].Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(child), "create store leg")

	foreign := digHolder(t, db, "DESTHELD-foreign-occupant")
	if err := reservations.AcquireOccupancy(db.DB, foreign.ID, lane); err != nil {
		t.Fatalf("foreign occupancy: %v", err)
	}

	if err := d.AdvanceCompoundOrder(parent.ID); err != nil {
		t.Fatalf("advance (held on destination): %v", err)
	}
	if inFlight(t, db, child.ID) {
		t.Fatal("the store leg dispatched into a lane a foreign order is inside")
	}

	// The lane clears, and the destination-held leg must be found by the same
	// lane-keyed re-drive.
	if err := reservations.ReleaseAllOccupancy(db.DB, foreign.ID); err != nil {
		t.Fatalf("release foreign occupancy: %v", err)
	}
	d.RedriveHeldCompoundLegs(lane)

	if !inFlight(t, db, child.ID) {
		after, _ := db.GetOrder(child.ID)
		t.Fatalf("a leg held on its DESTINATION lane did not resume when that lane cleared — "+
			"status %s, vendor %q. The re-drive must key on delivery_node as well as source_node",
			after.Status, after.VendorOrderID)
	}
}

// TestListHeldLegParentsInLane_MatchesDottedAndBareNames pins the name-form
// matching the re-drive depends on. A lane whose legs are addressed in the form
// this query does not match returns no parents, and the re-drive silently does
// nothing — the same invisible wedge, one layer down.
func TestListHeldLegParentsInLane_MatchesDottedAndBareNames(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	lane := mirrorLane(t, db, "NAMES-LANE", 3)
	slots, err := db.ListLaneSlots(lane)
	if err != nil || len(slots) < 2 {
		t.Fatalf("list lane slots: %v (got %d)", err, len(slots))
	}
	laneNode, err := db.GetNode(lane)
	if err != nil {
		t.Fatalf("get lane node: %v", err)
	}

	bareParent := digHolder(t, db, "NAMES-bare-parent")
	dottedParent := digHolder(t, db, "NAMES-dotted-parent")

	mk := func(uuid string, parentID int64, source string) {
		c := &orders.Order{
			EdgeUUID: uuid, StationID: "line-1",
			OrderType: OrderTypeMove, Status: StatusPending, Quantity: 1,
			ParentOrderID: &parentID, Sequence: 1,
			SourceNode: source, DeliveryNode: sd.LineNode.Name,
		}
		testutil.MustNoErr(t, db.CreateOrder(c), "create leg "+uuid)
	}
	mk("NAMES-bare-leg", bareParent.ID, slots[0].Name)
	mk("NAMES-dotted-leg", dottedParent.ID, laneNode.Name+"."+slots[1].Name)

	got, err := db.ListHeldLegParentsInLane(lane)
	if err != nil {
		t.Fatalf("ListHeldLegParentsInLane: %v", err)
	}
	found := map[int64]bool{}
	for _, id := range got {
		found[id] = true
	}
	if !found[bareParent.ID] {
		t.Errorf("bare-named leg's parent %d not found in %v", bareParent.ID, got)
	}
	if !found[dottedParent.ID] {
		t.Errorf("dotted-named leg's parent %d not found in %v — orders carry both forms, so "+
			"matching one silently loses half the plant", dottedParent.ID, got)
	}
}
