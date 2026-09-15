//go:build docker

package engine

import (
	"testing"

	"shingocore/fleet/simulator"
	"shingocore/store"
	"shingocore/store/payloads"
)

// These are the end-to-end half of dispatch's rule table. The unit tests in
// dispatch/robot_group_test.go pin what decideRobotGroup decides; these pin
// that the decision reaches the FLEET REQUEST through the real dispatch path,
// which is the only thing a robot actually sees.
//
// They sit beside TestMove_DispatchesAgainstTheBinsPayload because they are the
// same guard one step further on: that one proved a move carries the payload's
// group at all, these prove the group changes with how full the bin is.

// drainBinTo sets the bin's remaining count without touching anything else, so
// a test can put a bin at a chosen fraction of its payload's capacity.
func drainBinTo(t *testing.T, db *store.DB, binID int64, remaining int) {
	t.Helper()
	if _, err := db.Exec(`UPDATE bins SET uop_remaining=$1 WHERE id=$2`, remaining, binID); err != nil {
		t.Fatalf("drain bin %d to %d: %v", binID, remaining, err)
	}
}

// configureRelaxable sets a payload up the way a plant would: heavy while
// loaded, relaxed to the small robots at or below 20% of capacity.
func configureRelaxable(t *testing.T, db *store.DB, p *payloads.Payload) {
	t.Helper()
	p.RobotGroup = "HEAVY-1500"
	p.UOPCapacity = 100
	p.NearEmptyEnabled = true
	p.NearEmptyRobotGroup = "SMALL-600"
	p.NearEmptyThresholdPct = 20
	if err := db.UpdatePayload(p); err != nil {
		t.Fatalf("configure %s: %v", p.Code, err)
	}
}

// groupAskedFor moves one bin to the line and reports the robot group the fleet
// request carried.
//
// ONE MOVE PER TEST, and that is not tidiness: a second delivery to the same
// node is refused by the in-flight gate ("1 order already inbound"), so a test
// that asserted the full and drained cases against one destination would fail
// on the gate rather than on the group.
func groupAskedFor(t *testing.T, db *store.DB, label string, remaining int, dest int64) string {
	t.Helper()
	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	res, err := eng.CreateBinMove(BinMoveRequest{
		Selection: BinSelectionByLabel, BinLabel: label,
		DestNodeID: dest,
		StationID:  "test-station", Desc: "capability check",
	})
	if err != nil {
		t.Fatalf("CreateBinMove(%s): %v", label, err)
	}
	group, known := sim.RobotGroupFor(res.VendorOrderID)
	if !known {
		t.Fatalf("fleet has no order %s — nothing reached the fleet, so this proves nothing",
			res.VendorOrderID)
	}
	return group
}

// A full bin still goes to the heavy robots. The companion to the test below:
// relaxation firing proves nothing on its own, since a bug that relaxed
// unconditionally would pass that one.
func TestMove_FullBinStaysOnTheHeavyGroup(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, payload := setupTestData(t, db)
	configureRelaxable(t, db, payload)

	bin := createTestBinAtNode(t, db, payload.Code, storageNode.ID, "BIN-NE-FULL")
	drainBinTo(t, db, bin.ID, 100)

	if group := groupAskedFor(t, db, bin.Label, 100, lineNode.ID); group != "HEAVY-1500" {
		t.Errorf("a FULL bin asked for %q, want HEAVY-1500. Relaxing a full bin is the "+
			"failure this feature exists to avoid, not the feature.", group)
	}
}

// A bin drained to exactly the threshold relaxes — the inclusive boundary,
// end to end.
func TestMove_DrainedBinRelaxesToTheNearEmptyGroup(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, payload := setupTestData(t, db)
	configureRelaxable(t, db, payload)

	bin := createTestBinAtNode(t, db, payload.Code, storageNode.ID, "BIN-NE-DRAINED")
	drainBinTo(t, db, bin.ID, 20) // 20 of 100, at the inclusive boundary

	if group := groupAskedFor(t, db, bin.Label, 20, lineNode.ID); group != "SMALL-600" {
		t.Errorf("a bin at 20%% of capacity asked for %q, want SMALL-600. "+
			"The whole point of the feature is that this bin no longer needs the heavy "+
			"robots; an unchanged group means dispatch never consulted the count.", group)
	}
}

// A carrier that names a required group refuses the relaxation — and still does
// not override a loaded bin.
//
// This is the FG-rack case: a rack heavy in its own right must stay on the big
// robots when empty, whatever its payload permits. It is also the correction
// that shaped the rule — applying the carrier group at every fill level would
// let a carrier misconfigured to something lighter take a full heavy load, so
// the loaded half is asserted here too.
func TestMove_CarrierRefusesTheRelaxation(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, payload := setupTestData(t, db)
	configureRelaxable(t, db, payload)

	bin := createTestBinAtNode(t, db, payload.Code, storageNode.ID, "BIN-NE-RACK")
	drainBinTo(t, db, bin.ID, 5) // well below the threshold

	// The carrier this bin rides refuses to be relaxed.
	if _, err := db.Exec(`UPDATE bin_types SET required_robot_group='RACK-ONLY'
		WHERE id=(SELECT bin_type_id FROM bins WHERE id=$1)`, bin.ID); err != nil {
		t.Fatalf("set carrier restriction: %v", err)
	}

	if group := groupAskedFor(t, db, bin.Label, 5, lineNode.ID); group != "RACK-ONLY" {
		t.Errorf("a drained bin on a restricted carrier asked for %q, want RACK-ONLY. "+
			"The payload permits SMALL-600 at this fill level; the carrier is what has to "+
			"refuse it, and a payload setting must not be able to undo that.", group)
	}
}
