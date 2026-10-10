package engine

import (
	"testing"
)

// TestPin_ALoadWhoseReplyWasLostFilesItsOutboundOnTheRetry: the operator (or
// the sim) presses LOAD, Core loads the carrier, and the reply never arrives
// (a timeout). The Edge took that as a failure, so the loader's outbound move
// was never filed; every retry was then refused "already loaded — wait for
// outbound move", a move that nothing would ever file. The full carrier sat at
// the window and its part ran dry downstream (Lane 0 calibration run, PLK_X2
// STUD, orders 362/363 stranded 108 min). A window holding a carrier of a part
// this loader loads, with units, and owing no outbound is that lost reply: LOAD
// files the outbound and succeeds.
func TestPin_ALoadWhoseReplyWasLostFilesItsOutboundOnTheRetry(t *testing.T) {
	t.Parallel()
	const window = "SC-LRL-W1"
	eng, db, core, _, nodeID := scLoaderEngine(t, window)
	core.setCount(window, true, "PART-SC", 40) // Core loaded it; the Edge never heard

	if err := eng.LoadBin(nodeID, "", nil, scManifest); err != nil {
		t.Fatalf("LoadBin on its own lost reply: %v; it files the outbound and succeeds", err)
	}
	if n := len(scMovesFrom(t, db, window)); n != 1 {
		t.Errorf("outbound moves from %s = %d, want 1", window, n)
	}
	core.mu.Lock()
	loads := len(core.loadBinIDs)
	core.mu.Unlock()
	if loads != 0 {
		t.Errorf("bin-load requests = %d, want 0: the carrier is already loaded", loads)
	}

	// A second press with the outbound now owed is refused, as before.
	if err := eng.LoadBin(nodeID, "", nil, scManifest); err == nil {
		t.Error("a second LOAD with the outbound owed succeeded; it waits for that move")
	}
	if n := len(scMovesFrom(t, db, window)); n != 1 {
		t.Errorf("outbound moves after the second press = %d, want still 1", n)
	}
}
