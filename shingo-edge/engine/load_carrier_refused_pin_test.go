package engine

import (
	"strings"
	"testing"
)

// TestLoadBin_CoreRefusalReachesTheOperator holds the Edge half of D1 item 8:
// Core refuses a LOAD onto a carrier the part may not ride (409, a sentence
// naming the carrier, the allowed carriers and what to do with the carrier),
// and that sentence is what LoadBin returns, so the station's toast shows it
// (apiLoadBin writes the error, postAction toasts it). Nothing local follows a
// refused LOAD: no outbound move is filed and the window is not seated, because
// the carrier is still empty on Core's side.
//
// Green at the base: the Edge already carried Core's refusal text through
// coreErrorText. Pinned so the sentence the refusal was written for keeps
// reaching the person standing at the carrier.
func TestLoadBin_CoreRefusalReachesTheOperator(t *testing.T) {
	t.Parallel()
	const window = "SC-LCR-W1"
	eng, db, core, _, nodeID := scLoaderEngine(t, window)
	core.set(window, true, "") // an empty carrier stands on the window
	const refusal = `PART-SC may not ride a SC-OTHER carrier (allowed: SC-STD). Nothing was loaded.`
	core.mu.Lock()
	core.refuseLoad = refusal
	core.mu.Unlock()

	err := eng.LoadBin(nodeID, "", nil, scManifest)
	if err == nil {
		t.Fatal("LoadBin succeeded against Core's refusal")
	}
	if !strings.Contains(err.Error(), refusal) {
		t.Errorf("LoadBin's error lost Core's sentence: %v", err)
	}
	if n := len(scMovesFrom(t, db, window)); n != 0 {
		t.Errorf("outbound moves from %s = %d after a refused LOAD, want 0", window, n)
	}
}
