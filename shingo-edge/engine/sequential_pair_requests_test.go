package engine

import (
	"sync/atomic"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// AN A/B PAIR IS TWO LINES. Each side of a sequential pair is its own position
// with its own swap: the removal lifts that side's bin and its backfill brings
// that side the next one. A swap working one side says nothing about the other,
// so a request on the partner goes through while it runs, and once a side's swap
// has ended its next request goes through and mints its own backfill.

const (
	pairA = "PAIR-A"
	pairB = "PAIR-B"
)

// seedSequentialPair is a sequential A/B pair of the given role, each side
// holding a bin by Core's account and drawing from one market.
func seedSequentialPair(t *testing.T, role protocol.ClaimRole) (eng *Engine, db *store.DB, a, b int64) {
	t.Helper()
	db = testEngineDB(t)
	eng = testEngine(t, db)
	eng.logFn = func(string, ...any) {}
	eng.wireEventHandlers()
	var calls atomic.Int32
	eng.coreClient = NewCoreClient(censusStub(t, &calls, pairA, pairB).URL)

	procID, err := db.CreateProcess("PAIR-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "process")
	styleID, err := db.CreateStyle("PAIR-STYLE", "", procID)
	testutil.MustNoErr(t, err, "style")
	testutil.MustNoErr(t, db.SetActiveStyle(procID, &styleID), "active style")
	ids := map[string]int64{}
	for i, side := range []struct{ own, partner string }{{pairA, pairB}, {pairB, pairA}} {
		id, err := db.CreateProcessNode(processes.NodeInput{
			ProcessID: procID, CoreNodeName: side.own, Code: side.own, Name: side.own, Sequence: i + 1, Enabled: true,
		})
		testutil.MustNoErr(t, err, "node "+side.own)
		claimID, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
			StyleID: styleID, CoreNodeName: side.own, Role: role, SwapMode: protocol.SwapModeSequential,
			PayloadCode: ksPart, UOPCapacity: 40, InboundSource: ksMarket, OutboundDestination: ksDest,
			PairedCoreNode: side.partner,
		})
		testutil.MustNoErr(t, err, "claim "+side.own)
		_, err = db.EnsureProcessNodeRuntime(id)
		testutil.MustNoErr(t, err, "runtime "+side.own)
		testutil.MustNoErr(t, db.SetProcessNodeRuntime(id, &claimID, 30), "runtime claim "+side.own)
		ids[side.own] = id
	}
	testutil.MustNoErr(t, db.SetActivePull(ids[pairA], true), "the line draws from A")
	return eng, db, ids[pairA], ids[pairB]
}

// pressPair presses the role's request button on one side of the pair.
func pressPair(eng *Engine, role protocol.ClaimRole, nodeID int64) error {
	var err error
	if role == protocol.ClaimRoleProduce {
		_, err = eng.RequestProduceSwap(nodeID)
	} else {
		_, err = eng.RequestNodeMaterial(nodeID, 1)
	}
	return err
}

// removalOf is the side's live removal: the swap order that lifts its bin.
func removalOf(t *testing.T, db *store.DB, nodeID int64) int64 {
	t.Helper()
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")
	if rt.ActiveOrderID == nil {
		t.Fatalf("node %d: the request left no removal on the side", nodeID)
	}
	return *rt.ActiveOrderID
}

func TestSequentialPair_EachSideRunsItsOwnSwap(t *testing.T) {
	t.Parallel()
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
		t.Run(string(role), func(t *testing.T) {
			t.Parallel()
			eng, db, a, b := seedSequentialPair(t, role)

			// A's removal lifts A's bin; once it is on its way, A's backfill is
			// made.
			if err := pressPair(eng, role, a); err != nil {
				t.Fatalf("the request on A: %v", err)
			}
			removalA := removalOf(t, db, a)
			driveToInTransit(t, eng, removalA, a)
			backfills := ordersDroppingAt(t, db, pairA)
			if len(backfills) != 1 {
				t.Fatalf("orders bringing A a bin after its removal left: %v, want one backfill", backfills)
			}

			// A's swap is still working A. B is another position: its request goes
			// through and makes B's own removal.
			if err := pressPair(eng, role, b); err != nil {
				t.Fatalf("the request on B while A's swap runs was refused: %v", err)
			}
			removalB := removalOf(t, db, b)
			if removalB == removalA {
				t.Fatal("B's request named A's removal")
			}

			// A's swap ends: its removal and its backfill are done. A's next
			// request goes through, and its removal mints its own backfill.
			for _, id := range append([]int64{removalA}, backfills...) {
				testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(orders.StatusConfirmed)), "A's swap done")
			}
			if err := pressPair(eng, role, a); err != nil {
				t.Fatalf("A's next request after its swap ended was refused: %v", err)
			}
			next := removalOf(t, db, a)
			if next == removalA {
				t.Fatal("A's next request made no new removal")
			}
			driveToInTransit(t, eng, next, a)
			if got := ordersDroppingAt(t, db, pairA); len(got) != 2 {
				t.Errorf("orders ever bringing A a bin: %v, want the first backfill and the second", got)
			}
		})
	}
}
