package engine

import (
	"testing"

	"shingo/protocol"
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

// TestLoadBin_SendsWhatItRead: the LOAD carries the generation of the carrier
// its check read (the node-bins row's delta_epoch) and what it read the
// carrier to be, in the door's terms: bare, the only carrier the LOAD fills
// (a labelled one is answered without a load, loadOntoLabelledCarrier). Core
// refuses the LOAD when either no longer holds.
func TestLoadBin_SendsWhatItRead(t *testing.T) {
	t.Parallel()
	const window = "SC-LREAD-W1"
	eng, _, core, _, nodeID := scLoaderEngine(t, window)
	core.set(window, true, "")

	testutil.MustNoErr(t, eng.LoadBin(nodeID, "", nil, scManifest), "LoadBin")

	core.mu.Lock()
	defer core.mu.Unlock()
	if len(core.loads) != 1 || core.loads[0].Epoch != 5 || core.loads[0].Expect != protocol.LoadExpectBare {
		t.Errorf("bin-load requests = %+v, want one at epoch 5 expecting bare", core.loads)
	}
}
