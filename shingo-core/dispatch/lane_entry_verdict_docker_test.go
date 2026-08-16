//go:build docker

package dispatch

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// lane_entry_verdict_docker_test.go — what site #9's move to GateVerdict had to
// preserve, and what it could not be tested for.
//
// ── WHY THERE IS NO RED TEST HERE, AND THAT IS THE FINDING ────────────────
//
// The brief expected site #9 to be a stated-change-with-red-test: the tiered
// gate returned `park=false, err` on an unreadable lane, park=false means ADMIT,
// so the safety appeared to rest on every caller checking err first.
//
// It does rest on that — and all three callers do it, so the old shape was never
// observably wrong. There is no behaviour to change and therefore no red to
// write. The move buys a STRUCTURAL property (a future caller cannot get it
// wrong) rather than a behavioural fix, and that property is asserted where it
// lives: on the type, in admission_docker_test.go, where a bare GateVerdict is
// shown to refuse.
//
// An attempt was made to reach the undetermined arm with real data. It failed,
// and the reason is worth recording for the sites still to move: every read in
// laneEntryCause is a plain SELECT, and the one that looked promising —
// GetSlotDepth on a lane child with no depth, the row AuditLaneDepths warns
// about — deliberately scans into *int and returns 0 for NULL rather than
// erroring. Nothing in a shared test database can fail one of those SELECTs
// without breaking every other test using the table. The fixture guard in that
// attempt is what caught it, rather than a green test quietly asserting nothing.
//
// So site #9 is CHARACTERISATION: the twelve existing tiered-entry tests
// (lane_entry_pure_test.go, lane_entry_docker_test.go) predate the move, assert
// every tier and both dispositions, and say the same thing after it. This file
// adds the one arm those tests did not already cover, because the move created
// the risk.

// TestAdmitLaneEntry_DepthOneLaneStillAdmits guards the arm this signature
// change could most easily have broken by accident.
//
// The old code collapsed two unrelated situations into one line:
//
//	if err != nil || len(slots) < 2 { return false, "", err }
//
// A depth-1 lane (nothing to order) and an unreadable lane (nothing known) both
// returned park=false, and only the error distinguished them. Splitting them so
// the unreadable case refuses had to leave the depth-1 case ADMITTING — and
// getting that backwards would park every store into every single-slot lane in
// the plant, quietly, with no cause on the row to say why.
//
// That is a bigger blast radius than the property the split was made for, which
// is why it is the arm worth a test.
//
// DESIGN §16 rule 7: the slot count is the first thing this call can answer on.
// The destination resolves, its lane has a group, and that group enforces the
// mouth — everything upstream is satisfied, so the depth-1 arm is what replies.
//
// MUTATION (verified): return GateVerdict{} for the `len(slots) < 2` arm as
// well, collapsing it back with the error case. This fires — a one-slot lane
// refuses every entry, forever, with an empty cause.
func TestAdmitLaneEntry_DepthOneLaneStillAdmits(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, laneID, s0 := gatedLane(t, db, "TEDEPTH1", string(LaneEnforceMouth))
	slots := laneSlots(t, db, laneID)
	// Reduce the lane to a single slot: depth-1 lanes are ordering-exempt.
	if _, err := db.DB.Exec(`DELETE FROM nodes WHERE id = $1`, slots[1].ID); err != nil {
		t.Fatalf("drop the second slot: %v", err)
	}

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "te-depth1"
		o.DeliveryNode = s0.Name
		o.Status = "queued"
	})

	v, err := d.AdmitLaneEntry(order, s0)
	if err != nil {
		t.Fatalf("depth-1 lane: %v", err)
	}
	if !v.Admitted() {
		t.Errorf("a single-slot lane refused with %q. There is no ordering to do in a lane with one "+
			"slot, and refusing here would park every store in every depth-1 lane in the plant",
			v.Cause())
	}
}
