//go:build docker

package engine

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// ---------------------------------------------------------------------------
// A guard on this path failed OPEN on a read error — the direction that costs
// the most.
//
// The carrier-node stand-down asks "is a recovery order already moving this
// bin". It discarded the error, so a list it could not read meant "no" and the
// watch went on to place the bin — racing the arrival of the very order it was
// written to yield to. Standing down wrongly costs one tick; the next poll asks
// again. Proceeding wrongly is the double placement.
// ---------------------------------------------------------------------------

// withTableHidden renames a table for the duration of fn, so a query that reads
// it fails the way a real outage does. Restored even if the assertion fails.
//
// A forced error is the only honest way to test a read-error branch here: the
// store methods take no injectable failure, and asserting on a branch nothing
// can enter is how a fail-open survives being "covered".
func withTableHidden(t *testing.T, db *store.DB, table string, fn func()) {
	t.Helper()
	_, err := db.DB.Exec(`ALTER TABLE ` + table + ` RENAME TO ` + table + `_hidden`)
	testutil.MustNoErr(t, err, "hide "+table)
	defer func() {
		if _, rerr := db.DB.Exec(`ALTER TABLE ` + table + `_hidden RENAME TO ` + table); rerr != nil {
			t.Fatalf("restore %s: %v", table, rerr)
		}
	}()
	fn()
}

// TestSweepCarriedBins_StandsDownWhenItCannotTell is the F5 regression for the
// carrier-node guard.
func TestSweepCarriedBins_StandsDownWhenItCannotTell(t *testing.T) {
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	elsewhere := &nodes.Node{Name: "ELSEWHERE-FC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(elsewhere), "create the node the watch would pick")
	bin := seedCarried(t, db, "AMR-FC", "")

	// The deck has just emptied at a node the watch could name, and the robot
	// is parked — every condition for a placement is met except the one it
	// cannot check.
	robot := dispatchableRobot("AMR-FC")
	robot.JackState = 3
	robot.LiftHeight = -0.0001
	robot.CurrentStation = "ELSEWHERE-FC"
	cacheRobot(eng, robot)

	withTableHidden(t, db, "orders", func() {
		eng.sweepCarriedBins()
	})

	if got := binNodeName(t, db, bin.ID); got != bins.CarrierNodePrefix+"AMR-FC" {
		t.Errorf("bin was placed at %q while the guard could not read the order list.\n"+
			"A read it cannot complete is a question it cannot answer, and this guard exists "+
			"precisely to avoid racing a recovery order's arrival. Standing down costs one tick; "+
			"placing costs a bin recorded somewhere it is not.", got)
	}
}
