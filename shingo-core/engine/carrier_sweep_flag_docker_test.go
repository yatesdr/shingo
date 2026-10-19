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

// The carrier sweep's name test is a literal prefix: `_` in `_ROBOT:` is not
// "any one character", so a synthetic node that only LOOKS like a carrier to a
// LIKE is never removed. And the statement says how many carrier nodes are left.
func TestRetireEmptyCarrierNodes_LiteralPrefixAndWhatIsLeft(t *testing.T) {
	db := testdb.Open(t)
	lookalike := &nodes.Node{Name: "XROBOT:LOOK", IsSynthetic: true, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(lookalike), "create look-alike")
	empty := &nodes.Node{Name: bins.CarrierNodePrefix + "EMPTY", IsSynthetic: true, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(empty), "create empty carrier")
	seedCarried(t, db, "RIDING", "")

	deleted, left, err := db.RetireEmptyCarrierNodes(bins.CarrierNodePrefix)
	testutil.MustNoErr(t, err, "retire")
	if deleted != 1 || left != 1 {
		t.Errorf("deleted=%d left=%d, want 1 (the empty carrier) and 1 (the one a bin rides)", deleted, left)
	}
	if n, err := db.GetNodeByName("XROBOT:LOOK"); err != nil || n == nil {
		t.Errorf("the look-alike synthetic node was removed (err %v)", err)
	}
}

// THE TICK SWEEPS ONLY WHILE A CARRIER NODE MAY EXIST. A sweep that leaves
// none clears the flag, so the next tick does no carrier work; making a
// carrier node sets it again; and the reconciliation sweep runs the carrier
// half regardless, so a carrier node this engine did not make is still found.
func TestSweepCarriedBins_TheTickStopsWhenNoCarrierNodeIsLeft(t *testing.T) {
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, testdb.NewTrackingBackend())

	if eng.carriersClear.Load() {
		t.Fatal("a fresh engine starts clear; it cannot know what the table holds")
	}
	eng.sweepCarriedBins()
	if !eng.carriersClear.Load() {
		t.Fatal("a sweep that found no carrier node left the flag set")
	}

	// A carrier node made behind the engine's back: the tick does not see it.
	stray := &nodes.Node{Name: bins.CarrierNodePrefix + "STRAY", IsSynthetic: true, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(stray), "create stray carrier")
	eng.sweepCarriedBins()
	if !nodeNamed(t, db, stray.Name) {
		t.Fatal("the tick swept while the flag was clear")
	}
	// The reconciliation's run does, and removes it.
	eng.sweepCarriedBinsAll()
	if nodeNamed(t, db, stray.Name) {
		t.Error("the reconciliation sweep left an empty carrier node")
	}
	if !eng.carriersClear.Load() {
		t.Error("the reconciliation sweep found none left and did not clear the flag")
	}

	// Making a carrier node sets the flag: the tick sweeps again.
	if _, err := eng.carrierNode("AMR-FLAG"); err != nil {
		t.Fatalf("carrier node: %v", err)
	}
	if eng.carriersClear.Load() {
		t.Error("making a carrier node left the flag clear")
	}
	eng.sweepCarriedBins()
	if nodeNamed(t, db, bins.CarrierNodePrefix+"AMR-FLAG") {
		t.Error("the tick did not retire the empty carrier node it was told about")
	}
}

// nodeNamed reports whether a node of that name exists.
func nodeNamed(t *testing.T, db *store.DB, name string) bool {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM nodes WHERE name=$1`, name).Scan(&n), "count "+name)
	return n > 0
}
