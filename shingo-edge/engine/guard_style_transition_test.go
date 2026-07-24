// guard_style_transition_test.go — regression tests for the hop A2 guard that
// refuses outgoing-style relief while a changeover is armed (2026-07-23).
package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// TestGuardStyleTransition_ExemptsLoaderAndNonOutgoingStyle unit-tests the guard:
// during an active changeover it blocks the OUTGOING (from) style on a line
// produce/consume claim, but exempts manual_swap loaders (Springfield: loaders
// must keep supplying empties across a changeover) and any non-outgoing style.
func TestGuardStyleTransition_ExemptsLoaderAndNonOutgoingStyle(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedProduceNode(t, db, "two_robot")
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")
	eng := testEngine(t, db)

	otherStyleID, err := db.CreateStyle("OTHER-STYLE", "other", processID)
	testutil.MustNoErr(t, err, "create other style")

	// Arm a changeover FROM styleID TO otherStyleID.
	fromStyle := styleID
	_, err = eng.changeoverService.Create(processID, &fromStyle, otherStyleID, "test", "", nil, nil, nil, nil)
	testutil.MustNoErr(t, err, "create changeover")

	// Loader (manual_swap) claim is exempt even when it is the outgoing style.
	loaderClaim := &processes.NodeClaim{SwapMode: protocol.SwapModeManualSwap, StyleID: styleID}
	if err := eng.guardStyleTransition(node, loaderClaim); err != nil {
		t.Errorf("manual_swap loader must stay available during a changeover: %v", err)
	}

	// A claim for a DIFFERENT (not outgoing) style is not blocked.
	toClaim := &processes.NodeClaim{SwapMode: protocol.SwapModeTwoRobot, StyleID: otherStyleID}
	if err := eng.guardStyleTransition(node, toClaim); err != nil {
		t.Errorf("non-outgoing style must not be blocked: %v", err)
	}

	// The outgoing-style LINE claim IS blocked.
	fromClaim := &processes.NodeClaim{SwapMode: protocol.SwapModeTwoRobot, StyleID: styleID}
	if err := eng.guardStyleTransition(node, fromClaim); err == nil {
		t.Error("outgoing-style line relief must be blocked during a changeover")
	}
}

// TestRequestProduceSwap_BlockedForOutgoingStyleDuringChangeover pins the guard
// on the actual relief path: a produce swap for the outgoing style is refused
// while a changeover is armed, with a clear message.
func TestRequestProduceSwap_BlockedForOutgoingStyleDuringChangeover(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	otherStyleID, err := db.CreateStyle("CO-TARGET", "target", processID)
	testutil.MustNoErr(t, err, "create target style")
	fromStyle := styleID
	_, err = eng.changeoverService.Create(processID, &fromStyle, otherStyleID, "test", "", nil, nil, nil, nil)
	testutil.MustNoErr(t, err, "create changeover")

	if _, err := eng.RequestProduceSwap(nodeID); err == nil {
		t.Fatal("expected RequestProduceSwap to be refused for the outgoing style during a changeover")
	} else if !strings.Contains(err.Error(), "outgoing style") {
		t.Errorf("error = %q, want the outgoing-style changeover message", err.Error())
	}
}
