//go:build docker

package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// THE WATCH ALSO SEES WHAT A LIVE ORDER CARRIES (watchOrderDeck). A bin a live
// order has lifted sits at _TRANSIT, claimed by that order; the watch records
// its deck going loaded and the first at-rest empty reading after it, and
// places nothing while the order runs. When the order ends without arriving,
// that set-down is where the bin went (placeStrandedBin).
//
// SPR 2026-10-08, bin 135: lifted at SMN_0013 by order 7699, set back down on
// SMN_0013 before the order was cancelled; the cancel read the robot as busy and
// declined, and every later sweep read it somewhere else. The slot looked empty
// and the next order was sent into a tote.

// liveOrderCarries puts the seeded bin under its order while the order runs:
// the order live and the bin claimed by it, as handlePickupBlockCompleted
// leaves them.
func liveOrderCarries(t *testing.T, db *store.DB, bin *bins.Bin, ord *orders.Order) {
	t.Helper()
	_, err := db.DB.Exec(`UPDATE orders SET status=$1 WHERE id=$2`, string(protocol.StatusInTransit), ord.ID)
	testutil.MustNoErr(t, err, "order live")
	_, err = db.DB.Exec(`UPDATE bins SET claimed_by=$1 WHERE id=$2`, ord.ID, bin.ID)
	testutil.MustNoErr(t, err, "bin claimed by the order")
}

// orderEnds terminalises the order the way TerminalizeOrderWithReason does:
// the status flips and the claim is released, the bin left at _TRANSIT.
func orderEnds(t *testing.T, db *store.DB, bin *bins.Bin, ord *orders.Order) {
	t.Helper()
	_, err := db.DB.Exec(`UPDATE orders SET status=$1 WHERE id=$2`, string(protocol.StatusCancelled), ord.ID)
	testutil.MustNoErr(t, err, "order cancelled")
	_, err = db.DB.Exec(`UPDATE bins SET claimed_by=NULL WHERE id=$1`, bin.ID)
	testutil.MustNoErr(t, err, "claim released")
}

// The set-down during the order decides, not where the robot is at the end:
// the robot has driven on to a charger and reads busy with another task.
func TestStrandedTransit_ASetDownDuringTheOrderIsWhereTheBinWent(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	slot := &nodes.Node{Name: "SET-DOWN-1", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(slot), "slot")
	charger := &nodes.Node{Name: "CHARGER-SD", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(charger), "charger")

	bin, ord := seedStranded(t, db, "AMR-SD1")
	liveOrderCarries(t, db, bin, ord)

	cacheRobot(eng, loadedDeck("AMR-SD1"))
	eng.sweepCarriedBins()
	cacheRobot(eng, atPoint("AMR-SD1", "SET-DOWN-1", 0, 0))
	eng.sweepCarriedBins()
	if got := binNodeName(t, db, bin.ID); got != "_TRANSIT" {
		t.Fatalf("the watch placed a bin its live order still holds, at %q", got)
	}

	// The robot drives on, and is busy, before the order is cancelled.
	away := atPoint("AMR-SD1", "CHARGER-SD", 0, 0)
	away.Busy = true
	cacheRobot(eng, away)
	eng.sweepCarriedBins()
	orderEnds(t, db, bin, ord)
	eng.inferStrandedTransitBin(ord.ID)

	if got := binNodeName(t, db, bin.ID); got != "SET-DOWN-1" {
		t.Errorf("bin is at %q, want SET-DOWN-1, where the deck emptied during the order", got)
	}
}

// A swap's supply sets its carrier down at staging and lifts it again. The
// re-lift forgets that set-down: at the end the deck is loaded, so the bin
// rides the robot, and when the deck later empties somewhere Core cannot name
// it is stranded there — never placed at the staging node it left.
func TestStrandedTransit_AReliftForgetsAnEarlierSetDown(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	staging := &nodes.Node{Name: "STAGE-SD", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(staging), "staging")

	bin, ord := seedStranded(t, db, "AMR-SD2")
	liveOrderCarries(t, db, bin, ord)

	cacheRobot(eng, loadedDeck("AMR-SD2"))
	eng.sweepCarriedBins()
	cacheRobot(eng, atPoint("AMR-SD2", "STAGE-SD", 0, 0))
	eng.sweepCarriedBins()
	cacheRobot(eng, loadedDeck("AMR-SD2"))
	eng.sweepCarriedBins()

	orderEnds(t, db, bin, ord)
	eng.inferStrandedTransitBin(ord.ID)
	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-SD2" {
		t.Fatalf("bin is at %q, want riding AMR-SD2: the deck was loaded at the end", got)
	}

	cacheRobot(eng, atPoint("AMR-SD2", "LM-UNKNOWN", 0, 0))
	eng.sweepCarriedBins()
	b, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin")
	if b.NodeName == "STAGE-SD" {
		t.Fatal("the bin was placed at the staging node its order had already lifted it from")
	}
	if b.AnomalyAt == nil {
		t.Error("a bin set down where Core cannot name is stranded, with the spot in its note")
	}
}

// A set-down off any slot stays stranded, as the carried-bin watch's does: the
// robot later standing still at a real, empty node is not where the bin is.
func TestStrandedTransit_ASetDownOffAnySlotStaysStranded(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	later := &nodes.Node{Name: "LATER-SD", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(later), "a real node the robot stops at afterwards")

	bin, ord := seedStranded(t, db, "AMR-SD3")
	liveOrderCarries(t, db, bin, ord)

	cacheRobot(eng, loadedDeck("AMR-SD3"))
	eng.sweepCarriedBins()
	cacheRobot(eng, atPoint("AMR-SD3", "LM-AISLE", 41.25, 7.5))
	eng.sweepCarriedBins()

	cacheRobot(eng, atPoint("AMR-SD3", "LATER-SD", 0, 0))
	orderEnds(t, db, bin, ord)
	eng.inferStrandedTransitBin(ord.ID)

	b, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin")
	if b.NodeName != "_TRANSIT" {
		t.Fatalf("bin is at %q, want stranded at _TRANSIT: it was set down off any slot", b.NodeName)
	}
	for _, want := range []string{"set down at an unknown spot", "x=41.25"} {
		if !strings.Contains(b.AnomalyNote, want) {
			t.Errorf("anomaly note %q is missing %q — the floor needs where it was set down", b.AnomalyNote, want)
		}
	}
}

// A re-lift the watch never sampled — the poll short-circuits on an unchanged
// fleet, so a tick can be missed — still undoes the earlier set-down: the order
// ends with the deck loaded, the bin rides the robot, and the next empty
// reading is the drop, not the stale one.
func TestStrandedTransit_AnUnsampledReliftStillUndoesTheSetDown(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	for _, name := range []string{"STAGE-UN", "DROP-UN"} {
		testutil.MustNoErr(t, db.CreateNode(&nodes.Node{Name: name, Enabled: true}), name)
	}

	bin, ord := seedStranded(t, db, "AMR-SD5")
	liveOrderCarries(t, db, bin, ord)
	cacheRobot(eng, loadedDeck("AMR-SD5"))
	eng.sweepCarriedBins()
	cacheRobot(eng, atPoint("AMR-SD5", "STAGE-UN", 0, 0))
	eng.sweepCarriedBins()

	// Lifted again with no sweep in between; the order ends loaded.
	cacheRobot(eng, loadedDeck("AMR-SD5"))
	orderEnds(t, db, bin, ord)
	eng.inferStrandedTransitBin(ord.ID)

	cacheRobot(eng, atPoint("AMR-SD5", "DROP-UN", 0, 0))
	eng.sweepCarriedBins()
	if got := binNodeName(t, db, bin.ID); got != "DROP-UN" {
		t.Errorf("bin is at %q, want DROP-UN, where the loaded deck emptied — not the earlier staging set-down", got)
	}
}

// A set-down during the order onto a slot Core shows occupied: the robot put the
// bin there, which it could not have done onto a bin really standing there, so
// at the order's end the bin is placed and the record that was standing there is
// evicted to _TRANSIT, as a delivery does.
func TestStrandedTransit_ASetDownOntoAStaleRecordEvictsIt(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	slot := &nodes.Node{Name: "BUSY-SD", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(slot), "slot")

	bin, ord := seedStranded(t, db, "AMR-SD4")
	stale := &bins.Bin{BinTypeID: bin.BinTypeID, Label: "stale-sd", NodeID: &slot.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(stale), "a stale record in the slot")
	liveOrderCarries(t, db, bin, ord)

	cacheRobot(eng, loadedDeck("AMR-SD4"))
	eng.sweepCarriedBins()
	cacheRobot(eng, atPoint("AMR-SD4", "BUSY-SD", 0, 0))
	eng.sweepCarriedBins()
	orderEnds(t, db, bin, ord)
	eng.inferStrandedTransitBin(ord.ID)

	if got := binNodeName(t, db, bin.ID); got != "BUSY-SD" {
		t.Errorf("bin is at %q, want BUSY-SD, where the robot set it down", got)
	}
	if got := binNodeName(t, db, stale.ID); got != "_TRANSIT" {
		t.Errorf("the stale record is at %q, want evicted to _TRANSIT", got)
	}
}

// SPR 2026-10-08, CARRIER-0010. A cancelled order left the bin riding AMR-06,
// and the deck emptied in an aisle Core cannot name. The bin is stranded there
// and taken off the deck. The robot's next job, a lift and a set-down at a slot,
// is that job's bin and never this one's: it used to be credited to it,
// because a decline left the bin recorded on a deck seen empty.
func TestCarriedBin_AnAisleSetDownStrandsAndTheRobotsNextJobIsNotThisBin(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	next := &nodes.Node{Name: "NEXT-JOB-SLOT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(next), "the next job's slot")

	bin, ord := seedStranded(t, db, "AMR-AISLE")
	cacheRobot(eng, loadedDeck("AMR-AISLE"))
	eng.inferStrandedTransitBin(ord.ID)
	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-AISLE" {
		t.Fatalf("setup: bin is at %q, want riding AMR-AISLE", got)
	}

	cacheRobot(eng, atPoint("AMR-AISLE", "LM168", -40.15, 56.68))
	eng.sweepCarriedBins()
	if got := binNodeName(t, db, bin.ID); got != "_TRANSIT" {
		t.Fatalf("bin is at %q, want stranded at _TRANSIT: the deck read empty in an aisle", got)
	}
	if note := binNote(t, db, bin.ID); !strings.Contains(note, "x=-40.15") {
		t.Errorf("note = %q, want where the deck emptied", note)
	}

	// The robot goes back to work: lifts another job's bin, sets it down at a slot.
	cacheRobot(eng, loadedDeck("AMR-AISLE"))
	eng.sweepCarriedBins()
	eng.sweepStrandedBins()
	cacheRobot(eng, atPoint("AMR-AISLE", "NEXT-JOB-SLOT", 0, 0))
	eng.sweepCarriedBins()
	eng.sweepStrandedBins()

	if got := binNodeName(t, db, bin.ID); got != "_TRANSIT" {
		t.Errorf("bin is at %q, want still stranded: the robot's next job is not this bin", got)
	}
}
