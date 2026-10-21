package engine

import (
	"errors"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
)

// TestLoaderEmpties_RequestEmptyAsksCore: REQUEST EMPTY at a loader asks Core
// (loader.empties) for one empty at the loader's key, routed to the asking
// node, attributed to the operator's episode; the order Core made is the one
// projected here, tracked at the asking node and its runtime's active order.
func TestLoaderEmpties_RequestEmptyAsksCore(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	core := answerEmpties(t, eng)
	nodeID := seedCapManualSwap(t, db, "LEA-PROC", "LEA-W1", protocol.ClaimRoleProduce, []string{"PART-LE"}, 0, false)
	seedCoreLoader(t, eng, sharedLoaderInfo("LEA-W1", "produce", "operator", "PART-LE", 0, 0))

	o, err := eng.RequestEmptyBin(nodeID, "PART-LE")
	testutil.MustNoErr(t, err, "RequestEmptyBin")
	ask := core.lastAsk()
	if ask.LoaderKey != "loader:LEA-W1" || ask.MemberNode != "LEA-W1" || ask.Count != 1 || ask.PayloadCode != "PART-LE" {
		t.Errorf("ask = %+v, want loader:LEA-W1, member LEA-W1, one, PART-LE", ask)
	}
	if ask.OriginClass != protocol.OriginClassAttached || ask.OriginID == "" {
		t.Errorf("ask origin = %q/%q, want the operator's episode attached", ask.OriginID, ask.OriginClass)
	}
	if !strings.HasPrefix(o.UUID, "core-l1-") || o.ProcessNodeID == nil || *o.ProcessNodeID != nodeID {
		t.Fatalf("order = %s at %v, want Core's projected empty at node %d", o.UUID, o.ProcessNodeID, nodeID)
	}
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.ActiveOrderID == nil || *rt.ActiveOrderID != o.ID {
		t.Errorf("runtime active order = %v, want %d", rt.ActiveOrderID, o.ID)
	}
}

// TestLoaderEmpties_NoneMadeSaysWhy: when Core makes none the operator is told
// why, from Core's answer: its Skipped sentence, or each window held with its
// cause, in name order.
func TestLoaderEmpties_NoneMadeSaysWhy(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	core := answerEmpties(t, eng)
	nodeID := seedCapManualSwap(t, db, "LEN-PROC", "LEN-W1", protocol.ClaimRoleProduce, []string{"PART-LE"}, 0, false)
	seedCoreLoader(t, eng, sharedLoaderInfo("LEN-W1", "produce", "operator", "PART-LE", 0, 0))

	core.mu.Lock()
	core.answer = func(protocol.LoaderEmptiesRequest) protocol.LoaderEmptiesResponse {
		return protocol.LoaderEmptiesResponse{Want: 1, Created: []string{},
			HeldBy: map[string]string{"LEN-W2": "window-order-open", "LEN-W1": "occupied"}}
	}
	core.mu.Unlock()
	_, err := eng.RequestEmptyBin(nodeID, "")
	if err == nil || err.Error() != "node LEN-W1: no empty ordered — every window is spoken for (LEN-W1 occupied, LEN-W2 window-order-open)" {
		t.Errorf("all held: %v", err)
	}

	core.mu.Lock()
	core.answer = func(protocol.LoaderEmptiesRequest) protocol.LoaderEmptiesResponse {
		return protocol.LoaderEmptiesResponse{Created: []string{}, Skipped: "loader has no inbound source"}
	}
	core.mu.Unlock()
	_, err = eng.RequestEmptyBin(nodeID, "")
	if err == nil || err.Error() != "node LEN-W1: no empty ordered — loader has no inbound source" {
		t.Errorf("skipped: %v", err)
	}
	if n := scActiveEmptiesTo(t, db, "LEN-W1"); n != 0 {
		t.Errorf("empties at the window = %d, want 0 (the Edge orders none of its own)", n)
	}
}

// TestLoaderEmpties_ACoreWithoutTheKindIsRefused: a Core that does not answer
// loader.empties gets no ask and the Edge orders nothing of its own count; the
// tap is refused with the reason, and the push logs it.
func TestLoaderEmpties_ACoreWithoutTheKindIsRefused(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedCapManualSwap(t, db, "LEK-PROC", "LEK-W1", protocol.ClaimRoleProduce, []string{"PART-LE"}, 0, false)
	seedCoreLoader(t, eng, sharedLoaderInfo("LEK-W1", "produce", "operator", "PART-LE", 0, 0))

	_, err := eng.RequestEmptyBin(nodeID, "")
	if err == nil || !strings.Contains(err.Error(), "does not answer the loader.empties ask") {
		t.Errorf("REQUEST EMPTY at a Core without the kind: %v", err)
	}
	dl, lerr := eng.loaders().LoaderAt("LEK-W1", domain.RoleProduce)
	testutil.MustNoErr(t, lerr, "resolve loader")
	if created, err := eng.stageOperatorEmpty(dl, "", 1, "", orders.NoDemand()); created != 0 || err == nil {
		t.Errorf("push at a Core without the kind: created=%d err=%v, want 0 and the reason", created, err)
	}
	msgs, err := db.ListPendingOutbox(10)
	testutil.MustNoErr(t, err, "list outbox")
	for _, m := range msgs {
		if m.MsgType == protocol.SubjectEdgeAsk || m.MsgType == protocol.TypeOrderRequest {
			t.Errorf("outbox holds a %s, want no ask and no order", m.MsgType)
		}
	}
	if n := scActiveEmptiesTo(t, db, "LEK-W1"); n != 0 {
		t.Errorf("empties at the window = %d, want 0", n)
	}
}

// TestLoaderEmpties_ThePushServesNoDemand: the window-free push asks for one
// payload-agnostic empty and says it serves no demand.
func TestLoaderEmpties_ThePushServesNoDemand(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	core := answerEmpties(t, eng)
	seedCoreLoader(t, eng, sharedLoaderInfo("LEP-W1", "produce", "operator", "PART-LE", 0, 0))
	dl, err := eng.loaders().LoaderAt("LEP-W1", domain.RoleProduce)
	testutil.MustNoErr(t, err, "resolve loader")

	eng.maybeStageLoaderEmpty(dl)
	ask := core.lastAsk()
	if ask.LoaderKey != "loader:LEP-W1" || ask.Count != 1 || ask.PayloadCode != "" ||
		ask.OriginClass != protocol.OriginClassNoDemand || ask.OriginID != "" {
		t.Errorf("push ask = %+v, want one blank empty at loader:LEP-W1, no_demand", ask)
	}
}

// TestLoaderEmpties_APendingAnswerIsSent: an answer that has not come within
// the wait is ErrAskPending through REQUEST EMPTY's error, which the screen
// shows as sent and waiting.
func TestLoaderEmpties_APendingAnswerIsSent(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.setAskKinds([]string{protocol.AskLoaderEmpties})
	nodeID := seedCapManualSwap(t, db, "LEW-PROC", "LEW-W1", protocol.ClaimRoleProduce, []string{"PART-LE"}, 0, false)
	seedCoreLoader(t, eng, sharedLoaderInfo("LEW-W1", "produce", "operator", "PART-LE", 0, 0))

	_, err := eng.RequestEmptyBin(nodeID, "")
	if !errors.Is(err, ErrAskPending) {
		t.Errorf("unanswered REQUEST EMPTY: %v, want ErrAskPending", err)
	}
}
