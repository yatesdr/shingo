package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	ordermgr "shingoedge/orders"
	"shingoedge/store/processes"
)

// send_back_start_test.go — START's SEND BACK step (sendBackAtStart): a
// changing tote press's SEND BACK refuses the start while one of its orders
// moves, and is cancelled with the start's other cancels while none does; an
// Unchanged press's is left alone.

// seedSendBackAtStart is a produce tote press L1 changing over (or not, when
// the part stays) with its outside KSCO-PAIR-L1 a process node of the process,
// and SEND BACK's two orders on it.
func seedSendBackAtStart(t *testing.T, toPart string) (*coFixture, int64, int64) {
	t.Helper()
	fx := seedKeepStagedChangeover(t,
		[]coClaim{{"L1", "", "SRC-OLD", "PART-OLD", protocol.ClaimRoleProduce, protocol.SwapModeTwoRobotPressIndex, false, false}},
		[]coClaim{{"L1", "", "SRC-OLD", toPart, protocol.ClaimRoleProduce, protocol.SwapModeTwoRobotPressIndex, false, false}},
		map[string]protocol.NodeBinInfo{"L1": {Occupied: true}, "KSCO-PAIR-L1": {Occupied: true}})
	proc, err := fx.db.GetProcess(fx.processID)
	testutil.MustNoErr(t, err, "process")
	outside, err := fx.db.CreateProcessNode(processes.NodeInput{ProcessID: proc.ID, CoreNodeName: "KSCO-PAIR-L1",
		Code: "KSCOP", Name: "KSCO-PAIR-L1", Sequence: 9, Enabled: true})
	testutil.MustNoErr(t, err, "the outside's process node")
	move, err := fx.eng.orderMgr.CreateMoveOrderForBin(&outside, "KSCO-PAIR-L1", "KSCO-DEST", "", 77, ordermgr.NoDemand())
	testutil.MustNoErr(t, err, "SEND BACK's move")
	pull, err := fx.eng.orderMgr.CreateRetrieveOrder(ordermgr.RetrieveSpec{Process: &outside, Delivery: "KSCO-PAIR-L1",
		Source: "SRC-OLD", Payload: "PART-OLD", Quantity: 1, Empty: true, Want: protocol.TopsAndBottomsOnly,
		AutoConfirm: true, Origin: ordermgr.NoDemand()})
	testutil.MustNoErr(t, err, "SEND BACK's pull")
	return fx, move.ID, pull.ID
}

func (fx *coFixture) status(t *testing.T, id int64) protocol.Status {
	t.Helper()
	o, err := fx.db.GetOrder(id)
	testutil.MustNoErr(t, err, "order")
	return o.Status
}

// TestChangeoverStart_SendBackMovingRefusesAndCancelsNothing: the move has
// lifted the cart; START refuses with one sentence and cancels nothing, the
// pull included; once the move finishes, START goes and cancels the pull.
func TestChangeoverStart_SendBackMovingRefusesAndCancelsNothing(t *testing.T) {
	t.Parallel()
	fx, move, pull := seedSendBackAtStart(t, "PART-NEW")
	testutil.MustNoErr(t, fx.db.UpdateOrderStatus(move, string(protocol.StatusInTransit)), "the move lifts")
	_, err := fx.eng.StartProcessChangeover(fx.processID, fx.toStyleID, "test", "keep-staged")
	want := "cannot start changeover — KSCO-PAIR-L1: SEND BACK in progress — start the changeover when it finishes"
	if err == nil || err.Error() != want {
		t.Fatalf("START with SEND BACK moving: %v, want %q", err, want)
	}
	if s := fx.status(t, pull); s == protocol.StatusCancelled {
		t.Error("the refused start cancelled SEND BACK's pull")
	}
	if co, err := fx.db.GetActiveProcessChangeover(fx.processID); err == nil && co != nil {
		t.Error("a changeover was armed by a refused start")
	}
}

// TestChangeoverStart_SendBackFaultedNamesTheFault: a faulted SEND BACK order
// refuses with changeoverBlockerFor's faulted sentence.
func TestChangeoverStart_SendBackFaultedNamesTheFault(t *testing.T) {
	t.Parallel()
	fx, move, _ := seedSendBackAtStart(t, "PART-NEW")
	testutil.MustNoErr(t, fx.db.UpdateOrderStatus(move, string(protocol.StatusFaulted)), "the move faults")
	_, err := fx.eng.StartProcessChangeover(fx.processID, fx.toStyleID, "test", "keep-staged")
	if err == nil || !strings.Contains(err.Error(), "faulted mid-move") {
		t.Fatalf("START with SEND BACK faulted: %v, want the faulted sentence", err)
	}
}

// TestChangeoverStart_CancelsAQueuedSendBack: both of SEND BACK's orders are
// unmoved; START cancels them with its other cancels and the changeover goes.
func TestChangeoverStart_CancelsAQueuedSendBack(t *testing.T) {
	t.Parallel()
	fx, move, pull := seedSendBackAtStart(t, "PART-NEW")
	startKSChangeover(t, fx)
	for _, id := range []int64{move, pull} {
		if s := fx.status(t, id); s != protocol.StatusCancelled {
			t.Errorf("SEND BACK's order %d is %s after START, want cancelled", id, s)
		}
	}
}

// TestChangeoverStart_LeavesAnUnchangedPressesSendBack: the press's claim
// does not change; its SEND BACK is not read, and stays.
func TestChangeoverStart_LeavesAnUnchangedPressesSendBack(t *testing.T) {
	t.Parallel()
	fx, move, pull := seedSendBackAtStart(t, "PART-OLD")
	testutil.MustNoErr(t, fx.db.UpdateOrderStatus(move, string(protocol.StatusInTransit)), "the move lifts")
	if _, err := fx.eng.StartProcessChangeover(fx.processID, fx.toStyleID, "test", "keep-staged"); err != nil &&
		strings.Contains(err.Error(), "SEND BACK") {
		t.Fatalf("an Unchanged press's SEND BACK refused the start: %v", err)
	}
	if s := fx.status(t, pull); s == protocol.StatusCancelled {
		t.Error("an Unchanged press's SEND BACK pull was cancelled")
	}
}
