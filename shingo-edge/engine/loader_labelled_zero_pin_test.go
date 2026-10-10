package engine

import (
	"strings"
	"testing"
)

// TestPin_LoaderRefusesALabelledCarrierAtZero: a final release at 1, one tick
// before the pickup, and the carrier comes back to its loader still labelled
// but reading 0 (RULINGS-C2b-rebase6-and-loader-orphan §3). LOAD refuses it and
// tells the operator what will free it: confirm it is empty, CLEAR it, then
// load. It used to name an outbound move that never comes; row L item 5 changed
// the message, not the check. The Core half (the pool refuses the same carrier
// both ways) is shingo-core dispatch/binsource
// TestPin_LabelledCarrierAtZeroIsNeitherStockNorContainer.
func TestPin_LoaderRefusesALabelledCarrierAtZero(t *testing.T) {
	t.Parallel()
	const window = "SC-LZ-W1"
	eng, _, core, _, nodeID := scLoaderEngine(t, window)
	core.setCount(window, true, "SHIM", 0)

	err := eng.LoadBin(nodeID, "", nil, scManifest)
	if err == nil {
		t.Fatal("LoadBin accepted a labelled carrier at 0; it is refused until the operator clears it")
	}
	if !strings.Contains(err.Error(), "confirm it is empty, CLEAR it, then load") {
		t.Errorf("LoadBin refusal = %q, want it to name the clear", err)
	}
	if strings.Contains(err.Error(), "wait for outbound move") {
		t.Errorf("LoadBin refusal = %q names an outbound move that never comes", err)
	}
	if n := core.clearCount(); n != 0 {
		t.Errorf("LoadBin sent %d clear(s); the Edge's LOAD clears nothing", n)
	}
}

// A labelled carrier WITH units is stock: it still waits for its outbound move.
func TestPin_LoaderRefusesALabelledCarrierWithUnits(t *testing.T) {
	t.Parallel()
	const window = "SC-LZ-W2"
	eng, _, core, _, nodeID := scLoaderEngine(t, window)
	core.setCount(window, true, "SHIM", 5)

	err := eng.LoadBin(nodeID, "", nil, scManifest)
	if err == nil || !strings.Contains(err.Error(), "already loaded with SHIM — wait for outbound move") {
		t.Errorf("LoadBin refusal = %v, want the already-loaded refusal", err)
	}
}
