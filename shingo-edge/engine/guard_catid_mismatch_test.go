// guard_catid_mismatch_test.go — regression tests for the hop A5 guard that
// refuses outgoing-style relief when the press's live CATID_01 diverges from the
// active style's expected_catid (2026-07-23).
package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// seedCatidMonitor attaches a catidMonitor to eng with a single process's live
// CATID pre-confirmed to live. Mirrors what the poll loop would leave behind.
func seedCatidMonitor(eng *Engine, processID int64, procName, live string) {
	eng.catidMon = &catidMonitor{
		eng: eng,
		states: map[int64]*catidState{
			processID: {
				plcName:       "test-plc",
				tagName:       "MES.CATID_01",
				processName:   procName,
				lastConfirmed: live,
				seenValue:     true,
			},
		},
	}
}

// TestGuardCatidMismatch_InertAndBlocking exercises every branch of the guard:
// inert on empty expected_catid, inert without a monitor / observation, exempt
// for loaders, blocks on divergence, passes on a match.
func TestGuardCatidMismatch_InertAndBlocking(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedProduceNode(t, db, "two_robot")
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")
	eng := testEngine(t, db)

	lineClaim := &processes.NodeClaim{SwapMode: protocol.SwapModeTwoRobot, StyleID: styleID}

	// 1. expected_catid unset → INERT even with a live value present.
	seedCatidMonitor(eng, processID, "PRODUCE-PROC", "99999999")
	if err := eng.guardCatidMismatch(node, lineClaim); err != nil {
		t.Errorf("unconfigured expected_catid must be inert, got: %v", err)
	}

	// Configure the active style's expected_catid.
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleID, "40016911"), "set expected_catid")

	// 2. No monitor at all → fail-open (inert).
	eng.catidMon = nil
	if err := eng.guardCatidMismatch(node, lineClaim); err != nil {
		t.Errorf("no monitor must be fail-open, got: %v", err)
	}

	// 3. Monitor present but no observation yet (seenValue=false) → fail-open.
	eng.catidMon = &catidMonitor{eng: eng, states: map[int64]*catidState{
		processID: {seenValue: false},
	}}
	if err := eng.guardCatidMismatch(node, lineClaim); err != nil {
		t.Errorf("unobserved CATID must be fail-open, got: %v", err)
	}

	// 4. Live matches expected → passes.
	seedCatidMonitor(eng, processID, "PRODUCE-PROC", "40016911")
	if err := eng.guardCatidMismatch(node, lineClaim); err != nil {
		t.Errorf("matching CATID must not block, got: %v", err)
	}

	// 5. Live diverges from expected → BLOCKS, naming node + both values.
	seedCatidMonitor(eng, processID, "PRODUCE-PROC", "40016911")
	eng.catidMon.states[processID].lastConfirmed = "50029999"
	err = eng.guardCatidMismatch(node, lineClaim)
	if err == nil {
		t.Fatal("divergent CATID must block outgoing-style relief")
	}
	for _, want := range []string{node.Name, "50029999", "40016911"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("block message %q missing %q", err.Error(), want)
		}
	}

	// 6. Loaders (manual_swap) are exempt even when the part diverges.
	loaderClaim := &processes.NodeClaim{SwapMode: protocol.SwapModeManualSwap, StyleID: styleID}
	if err := eng.guardCatidMismatch(node, loaderClaim); err != nil {
		t.Errorf("manual_swap loader must stay available despite CATID divergence, got: %v", err)
	}
}

// TestRequestProduceSwap_BlockedOnCatidMismatch pins the guard on the actual
// relief path: a produce swap is refused when the press's live CATID diverges
// from the active style's expected value, with a clear message.
func TestRequestProduceSwap_BlockedOnCatidMismatch(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	testutil.MustNoErr(t, db.SetStyleExpectedCATID(styleID, "40016911"), "set expected_catid")
	seedCatidMonitor(eng, processID, "PRODUCE-PROC", "50029999") // wrong part on press

	if _, err := eng.RequestProduceSwap(nodeID); err == nil {
		t.Fatal("expected RequestProduceSwap to be refused when live CATID mismatches the active style")
	} else if !strings.Contains(err.Error(), "CATID") {
		t.Errorf("error = %q, want the CATID mismatch message", err.Error())
	}
}
