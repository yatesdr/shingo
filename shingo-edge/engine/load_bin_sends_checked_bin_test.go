package engine

import (
	"testing"

	"shingo/protocol/testutil"
)

// TestLoadBin_SendsTheBinItChecked pins the Edge half of I2: LOAD's occupancy
// check reads the carrier at the window, and the load names that carrier's id,
// so Core writes the bin the Edge verified empty rather than whichever one its
// own query returns first (and a multi-bin node without an id answers 409).
func TestLoadBin_SendsTheBinItChecked(t *testing.T) {
	t.Parallel()
	const window = "SC-LBID-W1"
	eng, _, core, _, nodeID := scLoaderEngine(t, window)
	core.set(window, true, "") // an empty carrier is at the window

	testutil.MustNoErr(t, eng.LoadBin(nodeID, "", nil, scManifest), "LoadBin")

	core.mu.Lock()
	defer core.mu.Unlock()
	if len(core.loadBinIDs) != 1 || core.loadBinIDs[0] != 77 {
		t.Errorf("bin-load bin_ids = %v, want [77] (the bin node-bins reported)", core.loadBinIDs)
	}
}
