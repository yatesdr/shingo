package engine

import (
	"strings"
	"testing"
)

// TestPin_LoaderRefusesALabelledCarrierAtZero characterises the Edge half of
// the SHIM loop (RULINGS-C2b-rebase6-and-loader-orphan §3): a final release
// at 1, one tick before the pickup, and the carrier comes back to its loader
// still labelled but reading 0. LOAD refuses it as "already loaded" and names
// an outbound move that never comes. The Core half (the pool refuses the same
// carrier both ways) is shingo-core dispatch/binsource
// TestPin_LabelledCarrierAtZeroIsNeitherStockNorContainer.
//
// C2b does not change this; Lane L of C2c changes the message (R5).
func TestPin_LoaderRefusesALabelledCarrierAtZero(t *testing.T) {
	t.Parallel()
	const window = "SC-LZ-W1"
	eng, _, core, _, nodeID := scLoaderEngine(t, window)
	core.setCount(window, true, "SHIM", 0)

	err := eng.LoadBin(nodeID, "", nil, scManifest)
	if err == nil {
		t.Fatal("LoadBin accepted a labelled carrier at 0; the characterised state refuses it")
	}
	if !strings.Contains(err.Error(), "already loaded with SHIM — wait for outbound move") {
		t.Errorf("LoadBin refusal = %q, want the already-loaded refusal", err)
	}
	if n := core.clearCount(); n != 0 {
		t.Errorf("LoadBin sent %d clear(s); the Edge's LOAD clears nothing", n)
	}
}
