//go:build docker

package engine

import (
	"testing"

	"shingocore/internal/testdb"
)

// ---------------------------------------------------------------------------
// The recovery door's LANE arm parked a busy corridor instead of terminalizing
// it, and this file pinned the park and what re-drove it. Every lane is marked
// now, and a recovery is unload-only (its bin is on the robot), so its lane
// entry is the gate's: a busy corridor holds it at the mark, after dispatch, and
// nothing refuses it before. Those pins are retired; the fleet-refusal arm below
// is unchanged.
// ---------------------------------------------------------------------------

// TestRecoveryFleetRefusalStaysTerminal pins the arm that does NOT convert.
//
// A fleet refusal is the sanctioned demand-by-hand exception: a person pressed
// Recover and is owed an answer, and nothing re-drives a plant whose fleet is
// down. Parking here would hand the operator a row with a cause and no releaser
// — a row with a cause and no releaser.
func TestRecoveryFleetRefusalStaysTerminal(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, db, backend)

	bin, _ := seedCarriedHome(t, db, "AMR-FLEET")
	cacheRobot(eng, dispatchableRobot("AMR-FLEET"))

	backend.SetFail(true)
	if _, _, err := eng.RecoverCarriedBin(bin.ID, "operator:test"); err == nil {
		t.Fatal("a fleet refusal must still answer the operator, not park silently")
	}
}
