package engine

import "testing"

// operator_bin_ops_pulled_directly_test.go — a CLEAR at a window of a stage 2
// whose finished carts the process pulls straight off it
// (LoaderInfo.PulledDirectly). The cart stays for the line: no empty-out, and
// no "nowhere to send" error, because nowhere is where it is meant to go. With
// the option off the same blank outbound is still the operator's error
// (TestClearBin_NoOutboundTellsTheOperator, unchanged).
func TestClearBin_PulledDirectlyLeavesTheCartQuietly(t *testing.T) {
	t.Parallel()
	eng := testEngine(t, testEngineDB(t))
	core := newClearCaptureCore(t)
	eng.coreClient = stubCoreClient(core.srv.URL)
	info := sharedLoaderInfo("PD-W1", "consume", "operator", "PART-HL", 0, 0)
	info.InboundSource = ""
	info.OutboundDest = ""
	info.PulledDirectly = true
	nodeID := coreUnloaderWindow(t, eng, "PD-W1", info)

	err := eng.ClearBin(nodeID, "")

	if clears, _ := core.snapshot(); len(clears) != 1 {
		t.Fatalf("bin-clear POSTs = %d, want 1 (the clear itself happens)", len(clears))
	}
	if moves := scMovesFrom(t, eng.db, "PD-W1"); len(moves) != 0 {
		t.Errorf("U2s leaving PD-W1 = %d, want 0: the line pulls the cart off the window", len(moves))
	}
	if err != nil {
		t.Errorf("ClearBin = %v, want nil: the cart staying is the setting, not a fault", err)
	}
}
