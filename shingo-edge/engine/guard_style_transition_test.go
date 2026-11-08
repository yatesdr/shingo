// guard_style_transition_test.go — regression tests for the hop A2 guard that
// refuses outgoing-style relief while a changeover is armed (2026-07-23).
package engine

import (
	"errors"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
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
	if err := eng.guardChangeover(nil, node, loaderClaim); err != nil {
		t.Errorf("manual_swap loader must stay available during a changeover: %v", err)
	}

	// A claim for a DIFFERENT (not outgoing) style is not blocked.
	toClaim := &processes.NodeClaim{SwapMode: protocol.SwapModeTwoRobot, StyleID: otherStyleID}
	if err := eng.guardChangeover(nil, node, toClaim); err != nil {
		t.Errorf("non-outgoing style must not be blocked: %v", err)
	}

	// The outgoing-style LINE claim IS blocked — with a *ChangeoverArmedError that
	// names the target + outgoing style and carries the process id, so the UI can
	// offer the abandon exit inline instead of a dead-end toast.
	fromClaim := &processes.NodeClaim{SwapMode: protocol.SwapModeTwoRobot, StyleID: styleID}
	blockErr := eng.guardChangeover(nil, node, fromClaim)
	var armed *ChangeoverArmedError
	if !errors.As(blockErr, &armed) {
		t.Fatalf("outgoing-style line relief must be blocked with *ChangeoverArmedError, got: %v", blockErr)
	}
	if armed.ProcessID != processID {
		t.Errorf("ChangeoverArmedError.ProcessID = %d, want %d", armed.ProcessID, processID)
	}
	// Message names the changeover target (OTHER-STYLE) and offers the exit for
	// the outgoing style (PROD-STYLE, seeded by seedProduceNode).
	for _, want := range []string{"changeover to OTHER-STYLE", "cancel it", "PROD-STYLE material"} {
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

	if _, err := eng.RequestProduceSwap(nodeID, 0); err == nil {
		t.Fatal("expected RequestProduceSwap to be refused for the outgoing style during a changeover")
	} else if !strings.Contains(err.Error(), "cancel it") || !strings.Contains(err.Error(), "CO-TARGET") {
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
	_, err = eng.RequestProduceSwap(nodeID, 0)
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
	if _, err := eng.RequestProduceSwap(nodeID, 0); err != nil {
		var stillArmed *ChangeoverArmedError
		if errors.As(err, &stillArmed) {
			t.Fatalf("request still blocked by the changeover after abandon: %v", err)
		}
		t.Fatalf("request should proceed after abandon, got: %v", err)
	}
}

// commonCellRig is healthRig (A active, pinned 1001; B pinned 1002; the PLC on
// 1001) with three consume positions, each with a cart of A's part: N-COMMON,
// which both styles claim for PART-C; N-EVAC, the same, B's claim evacuating
// on a changeover; and N-MIRROR, whose part differs (A PART-L, B PART-R).
func commonCellRig(t *testing.T) (eng *Engine, pid, a, b int64, nodes map[string]*processes.Node, cm *catidMonitor) {
	t.Helper()
	eng, db, pid, a, b, cm := healthRig(t, "1001")
	cartNode(t, eng, pid, a, "N-COMMON", "PART-C", "PART-C", 9761)
	_, err := upsertClaimRetiredMode(db, processes.NodeClaimInput{StyleID: b, CoreNodeName: "N-COMMON", Role: "consume",
		SwapMode: "simple", PayloadCode: "PART-C", UOPCapacity: 100, InboundSource: "SRC"})
	testutil.MustNoErr(t, err, "B's claim at N-COMMON")
	cartNode(t, eng, pid, a, "N-EVAC", "PART-E", "PART-E", 9762)
	_, err = upsertClaimRetiredMode(db, processes.NodeClaimInput{StyleID: b, CoreNodeName: "N-EVAC", Role: "consume",
		SwapMode: "simple", PayloadCode: "PART-E", UOPCapacity: 100, InboundSource: "SRC", EvacuateOnChangeover: true})
	testutil.MustNoErr(t, err, "B's claim at N-EVAC")
	mirrorNode(t, eng, pid, a, b, "N-MIRROR", "PART-L", "PART-R", 9763)
	ns, err := db.ListProcessNodesByProcess(pid)
	testutil.MustNoErr(t, err, "nodes")
	nodes = map[string]*processes.Node{}
	for i := range ns {
		nodes[ns[i].CoreNodeName] = &ns[i]
	}
	return eng, pid, a, b, nodes, cm
}

func claimOf(t *testing.T, eng *Engine, styleID int64, node string) *processes.NodeClaim {
	t.Helper()
	c, err := eng.db.GetStyleNodeClaimByNode(styleID, node)
	testutil.MustNoErr(t, err, "the claim at "+node)
	return c
}

// TestGuardStyleTransition_RefusesByPosition: with a changeover A -> B open, a
// request under A at a position both styles claim for the same part, which the
// changeover leaves unchanged, passes the request guard and the participant
// gate (CanAcceptOrders, the sweep's); at the position whose part differs both
// refuse as before, and at a same-part position the changeover evacuates too.
// Before, every outgoing-style request at the process was refused, every
// participant was closed to the sweep, and a common position that ran out
// during a changeover got no cart.
func TestGuardStyleTransition_RefusesByPosition(t *testing.T) {
	t.Parallel()
	eng, pid, a, b, nodes, _ := commonCellRig(t)
	ns, err := eng.db.ListProcessNodesByProcess(pid)
	testutil.MustNoErr(t, err, "nodes")
	var participants []domain.ParticipantInput
	for _, n := range []string{"N-COMMON", "N-EVAC", "N-MIRROR"} {
		participants = append(participants, domain.ParticipantInput{CoreNodeName: n, Role: domain.ParticipantRoleTask})
	}
	_, err = eng.changeoverService.Create(pid, &a, b, "test", "", nil, []processes.NodeTaskInput{
		{ProcessID: pid, CoreNodeName: "N-COMMON", Situation: string(SituationUnchanged), State: "unchanged"},
		{ProcessID: pid, CoreNodeName: "N-EVAC", Situation: string(SituationEvacuate), State: "swap_required"},
		{ProcessID: pid, CoreNodeName: "N-MIRROR", Situation: string(SituationSwap), State: "swap_required"},
	}, participants, ns)
	testutil.MustNoErr(t, err, "the changeover A -> B")
	for _, row := range []struct {
		node    string
		refused bool
	}{{"N-COMMON", false}, {"N-EVAC", true}, {"N-MIRROR", true}} {
		err := eng.guardChangeover(nil, nodes[row.node], claimOf(t, eng, a, row.node))
		var armed *ChangeoverArmedError
		if row.refused != errors.As(err, &armed) {
			t.Errorf("%s under A with the changeover open: %v, refused %t wanted", row.node, err, row.refused)
		}
		if ok, why := eng.CanAcceptOrders(nodes[row.node].ID); ok == row.refused {
			t.Errorf("%s with the changeover open: the participant gate accepts %t (%s), refused %t wanted", row.node, ok,
				why, row.refused)
		}
	}
}
