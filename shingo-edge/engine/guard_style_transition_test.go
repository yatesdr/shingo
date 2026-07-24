// guard_style_transition_test.go — regression tests for the hop A2 guard that
// refuses outgoing-style relief while a changeover is armed (2026-07-23).
package engine

import (
	"errors"
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

	// The outgoing-style LINE claim IS blocked — with a *ChangeoverArmedError that
	// names the target + outgoing style and carries the process id, so the UI can
	// offer the abandon exit inline instead of a dead-end toast.
	fromClaim := &processes.NodeClaim{SwapMode: protocol.SwapModeTwoRobot, StyleID: styleID}
	blockErr := eng.guardStyleTransition(node, fromClaim)
	var armed *ChangeoverArmedError
	if !errors.As(blockErr, &armed) {
		t.Fatalf("outgoing-style line relief must be blocked with *ChangeoverArmedError, got: %v", blockErr)
	}
	if armed.ProcessID != processID {
		t.Errorf("ChangeoverArmedError.ProcessID = %d, want %d", armed.ProcessID, processID)
	}
	// Message names the changeover target (OTHER-STYLE) and offers the exit for
	// the outgoing style (PROD-STYLE, seeded by seedProduceNode).
	for _, want := range []string{"changeover to OTHER-STYLE", "abandon", "PROD-STYLE material"} {
		if !strings.Contains(armed.Error(), want) {
			t.Errorf("refusal %q missing %q", armed.Error(), want)
		}
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
	} else if !strings.Contains(err.Error(), "abandon") || !strings.Contains(err.Error(), "CO-TARGET") {
		t.Errorf("error = %q, want the armed-changeover exit message naming the target", err.Error())
	}
}

// TestChangeoverAbandonThenRequest_Succeeds proves a material-request refusal is
// never a dead end: an armed changeover refuses the outgoing-style produce swap
// with a *ChangeoverArmedError, and after the operator takes the exit — abandon
// the changeover — the very same request proceeds with no refresh or restart
// (2026-07-24).
func TestChangeoverAbandonThenRequest_Succeeds(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	otherStyleID, err := db.CreateStyle("CO-TARGET", "target", processID)
	testutil.MustNoErr(t, err, "create target style")
	fromStyle := styleID
	_, err = eng.changeoverService.Create(processID, &fromStyle, otherStyleID, "test", "", nil, nil, nil, nil)
	testutil.MustNoErr(t, err, "create changeover")

	// 1. Armed changeover ⇒ refused with the actionable typed error carrying the
	//    process id the UI needs to build the abandon action.
	_, err = eng.RequestProduceSwap(nodeID)
	var armed *ChangeoverArmedError
	if !errors.As(err, &armed) {
		t.Fatalf("armed changeover must refuse with *ChangeoverArmedError, got: %v", err)
	}
	if armed.ProcessID != processID {
		t.Errorf("exit ProcessID = %d, want %d", armed.ProcessID, processID)
	}

	// 2. Operator takes the exit: abandon the changeover.
	testutil.MustNoErr(t, eng.CancelProcessChangeover(processID), "abandon changeover")

	// 3. The SAME request now proceeds — the changeover dead-end is gone.
	if _, err := eng.RequestProduceSwap(nodeID); err != nil {
		var stillArmed *ChangeoverArmedError
		if errors.As(err, &stillArmed) {
			t.Fatalf("request still blocked by the changeover after abandon: %v", err)
		}
		t.Fatalf("request should proceed after abandon, got: %v", err)
	}
}
