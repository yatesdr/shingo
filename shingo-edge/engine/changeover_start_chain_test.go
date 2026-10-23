package engine

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// TestChangeoverStart_CancelsAQueuedSwapsClearAndPrime: a press that changes
// over has a queued swap's chain on its process node (W6: a clear of the
// on-deck's carrier to the outbound, and a prime to the on-deck), and an index
// leg that lifts at the same on-deck. START cancels the clear and the prime
// with its other cancels, and leaves the index leg alone: it is the swap's, not
// the chain's. On a press that also keeps a spot the line is in START's lock
// set twice over (a spot line and a changing press) and is locked once
// (lockCells): primeNodeLock is not re-entrant, so a second lock would hang the
// start here.
func TestChangeoverStart_CancelsAQueuedSwapsClearAndPrime(t *testing.T) {
	t.Parallel()
	for _, spot := range []struct {
		name string
		keep bool
	}{{"no spot", false}, {"keeps a spot", true}} {
		t.Run(spot.name, func(t *testing.T) {
			t.Parallel()
			fx := seedKeepStagedChangeover(t,
				[]coClaim{{"L1", "SPOT", "SRC-OLD", "PART-OLD", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobotPressIndex, spot.keep, false}},
				[]coClaim{{"L1", "SPOT", "SRC-NEW", "PART-NEW", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobotPressIndex, spot.keep, false}},
				map[string]protocol.NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-OLD"}})
			nodeID := fx.nodeIDs["L1"]
			deck := "KSCO-PAIR-L1"

			clear, err := fx.eng.orderMgr.CreateMoveOrderForBin(&nodeID, deck, "KSCO-DEST", "OTHER", 77, orders.Origin{})
			testutil.MustNoErr(t, err, "the queued clear")
			prime, err := fx.eng.orderMgr.CreateMoveOrder(&nodeID, 1, "SRC-OLD", deck, false, orders.Origin{})
			testutil.MustNoErr(t, err, "the queued prime")
			index, err := fx.eng.orderMgr.CreateComplexOrder(&nodeID, 1, "KSCO-DEST", "L1",
				[]protocol.ComplexOrderStep{{Action: protocol.ActionPickup, Node: deck}, {Action: protocol.ActionDropoff, Node: "KSCO-DEST"}},
				orders.Origin{})
			testutil.MustNoErr(t, err, "an index leg lifting at the on-deck")

			startKSChangeover(t, fx)

			for _, c := range []struct {
				name      string
				id        int64
				cancelled bool
			}{{"the clear", clear.ID, true}, {"the prime", prime.ID, true}, {"the index leg", index.ID, false}} {
				o, err := fx.db.GetOrder(c.id)
				testutil.MustNoErr(t, err, c.name)
				if got := o.Status == protocol.StatusCancelled; got != c.cancelled {
					t.Errorf("%s is %s after START; want cancelled %v", c.name, o.Status, c.cancelled)
				}
			}
		})
	}
}

// TestPressRequest_ARequestLetInAfterAStartIsRefused: a press line with no
// spot is in a start's lock set (it changes, and the start cancels its chain),
// so a REQUEST on it that passed the changeover guard and then waited on the
// cell lock is asked again under it, as on a keep-staged line
// (TestKeepStagedRequest_ARequestLetInAfterAStartIsRefused, whose choreography
// this is: the changeover arms only once the request is parked on the lock).
func TestPressRequest_ARequestLetInAfterAStartIsRefused(t *testing.T) {
	t.Parallel()
	fx := seedKeepStagedChangeover(t,
		[]coClaim{{"L1", "", "SRC-OLD", "PART-OLD", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobotPressIndex, false, false}},
		[]coClaim{{"L1", "", "SRC-NEW", "PART-NEW", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobotPressIndex, false, false}},
		map[string]protocol.NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-OLD"},
			"KSCO-PAIR-L1": {Occupied: true, PayloadCode: "PART-OLD"}})
	proc, err := fx.db.GetProcess(fx.processID)
	testutil.MustNoErr(t, err, "process")

	mu := fx.eng.primeNodeLock(&processes.NodeClaim{CoreNodeName: "L1"})
	mu.Lock()
	done := make(chan error, 1)
	gid := make(chan string, 1)
	go func() {
		gid <- goroutineID()
		_, err := fx.eng.RequestNodeMaterial(fx.nodeIDs["L1"], 1)
		done <- err
	}()
	waitParkedOnCellLock(t, <-gid, done)
	_, err = fx.eng.changeoverService.Create(fx.processID, proc.ActiveStyleID, fx.toStyleID, "test", "", nil, nil, nil, nil)
	testutil.MustNoErr(t, err, "arm the changeover")
	mu.Unlock()

	var armed *ChangeoverArmedError
	if err := <-done; !errors.As(err, &armed) {
		t.Fatalf("the request let in after the changeover armed returned %v, want ChangeoverArmedError", err)
	}
	live, err := fx.db.ListActiveOrdersByProcessNode(fx.nodeIDs["L1"])
	testutil.MustNoErr(t, err, "orders")
	if len(live) != 0 {
		t.Fatalf("%d order(s) created by a request refused under the lock", len(live))
	}
}

// TestStartLocksLine_ThePredicateTheDoorsReCheck: the one predicate START's
// lock set and both request doors' under-lock re-check share: a line that
// keeps a spot, or a press; any other line is never locked by a start.
func TestStartLocksLine_ThePredicateTheDoorsReCheck(t *testing.T) {
	t.Parallel()
	_, _, press := pressIndexFixtures("")
	_, _, plain := produceFixtures(protocol.SwapModeTwoRobot)
	spotted := *plain
	spotted.KeepStagedNode = "SPOT"
	for _, c := range []struct {
		name  string
		claim *processes.NodeClaim
		want  bool
	}{{"a press", press, true}, {"a line keeping a spot", &spotted, true}, {"any other line", plain, false}} {
		if got := startLocksLine(c.claim); got != c.want {
			t.Errorf("%s: startLocksLine = %v, want %v", c.name, got, c.want)
		}
	}
}
