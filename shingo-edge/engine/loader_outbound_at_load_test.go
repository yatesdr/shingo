package engine

import (
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
	storeorders "shingoedge/store/orders"
)

// loader_outbound_at_load_test.go — a loader's outbound move (the L2) is filed
// by LOAD, the one act that knows what the carrier holds. The empty's confirm
// files nothing.
//
// Before, the L1's completion filed the L2 as well, whenever the confirm came:
// for a Core-born empty that is Core's auto-confirm sweep, 5 to 10 minutes after
// the empty lands in production, loaded or not. The robot was then sent for a
// carrier the operator might still be filling, the move carried a blank part
// (Core filled it from the bin at dispatch, so the robot group followed whatever
// the bin read by then), and when the confirm and LOAD landed in the same second
// both doors filed a move (PLK_X2, orders 190 and 191 on the B3 sim).

// scCoreBornL1 lands a Core-born empty (a `core-l1-` retrieve_empty the Edge
// never authored) at window, delivered, as Core's projection and delivery
// envelope put it there.
func scCoreBornL1(t *testing.T, eng *Engine, db *store.DB, window, uuid string) *storeorders.Order {
	t.Helper()
	_, err := eng.ApplyOrderProjection(protocol.OrderProjection{
		OrderUUID: uuid, OrderType: protocol.OrderTypeRetrieveEmpty, Status: string(protocol.StatusDispatched),
		Quantity: 1, SourceNode: "EMPTY-MKT", DeliveryNode: window, RetrieveEmpty: true,
	})
	testutil.MustNoErr(t, err, "project the Core-born L1")
	l1, err := db.GetOrderByUUID(uuid)
	testutil.MustNoErr(t, err, "reload the Core-born L1")
	if l1.AuthoredBy != "core" {
		t.Fatalf("fixture: L1 authored_by=%q, want core", l1.AuthoredBy)
	}
	scDeliver(t, eng, db, l1)
	return l1
}

// TestLoaderOutbound_IsFiledAtTheLoad_CoreBorn: Core's auto-confirm of a
// Core-born empty files no L2; the operator's LOAD files exactly one, carrying
// the part loaded.
func TestLoaderOutbound_IsFiledAtTheLoad_CoreBorn(t *testing.T) {
	t.Parallel()
	const window = "SC-ATLOAD-CB"
	eng, db, core, _, nodeID := scLoaderEngine(t, window)
	core.set(window, true, "") // the empty lands
	l1 := scCoreBornL1(t, eng, db, window, "core-l1-atload-cb")

	testutil.MustNoErr(t, eng.orderMgr.HandleCoreStatusPush(l1.UUID, protocol.StatusConfirmed, "auto_confirm_timeout"),
		"Core's auto-confirm")
	if moves := scMovesFrom(t, db, window); len(moves) != 0 {
		t.Fatalf("L2s after Core's auto-confirm, before LOAD = %d (payload %q), want 0 — nobody has "+
			"loaded the carrier, so no robot is owed", len(moves), moves[0].PayloadCode)
	}

	testutil.MustNoErr(t, eng.LoadBin(nodeID, "PART-SC", nil, scManifest), "LoadBin")
	moves := scMovesFrom(t, db, window)
	if len(moves) != 1 {
		t.Fatalf("L2s after LOAD = %d, want exactly 1", len(moves))
	}
	if moves[0].PayloadCode != "PART-SC" || moves[0].DeliveryNode != "FG-MARKET" {
		t.Errorf("L2 = to %q payload %q, want FG-MARKET / PART-SC (the part LOAD held)",
			moves[0].DeliveryNode, moves[0].PayloadCode)
	}
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.ActiveOrderID == nil || *rt.ActiveOrderID != moves[0].ID {
		t.Errorf("runtime active order = %v, want the L2 %d", rt.ActiveOrderID, moves[0].ID)
	}
}

// TestLoaderOutbound_IsFiledAtTheLoad_EdgeBorn: an Edge-born empty confirmed at
// the window (the modal's CONFIRM DELIVERY) files no L2 either; LOAD files it.
// An Edge-born L1 confirmed BY its LOAD is pinned unchanged by
// TestPinD3a_LoadAfterEcho_ConfirmsTheL1AndFilesOneL2 and
// TestPinD3b_ConfirmedL1AtACoreOwnedLoader_FilesOneL2: one L2 with the part.
func TestLoaderOutbound_IsFiledAtTheLoad_EdgeBorn(t *testing.T) {
	t.Parallel()
	const window = "SC-ATLOAD-EB"
	eng, db, core, _, nodeID := scLoaderEngine(t, window)
	core.set(window, false, "")
	l1, err := eng.RequestEmptyBin(nodeID, "")
	testutil.MustNoErr(t, err, "RequestEmptyBin")
	scEcho(t, eng, l1)
	core.set(window, true, "")
	scDeliver(t, eng, db, l1)

	testutil.MustNoErr(t, eng.orderMgr.ConfirmDelivery(l1.ID, 0), "CONFIRM DELIVERY")
	if moves := scMovesFrom(t, db, window); len(moves) != 0 {
		t.Fatalf("L2s after the empty's CONFIRM, before LOAD = %d (payload %q), want 0",
			len(moves), moves[0].PayloadCode)
	}

	testutil.MustNoErr(t, eng.LoadBin(nodeID, "PART-SC", nil, scManifest), "LoadBin")
	moves := scMovesFrom(t, db, window)
	if len(moves) != 1 || moves[0].PayloadCode != "PART-SC" {
		t.Errorf("L2s after LOAD = %+v, want exactly one carrying PART-SC", moves)
	}
}

// TestLoaderOutbound_ConfirmAndLoadTogetherFileOne is the B3 sim's PLK_X2
// double L2 as a regression case: Core's auto-confirm and the operator's LOAD
// land together, and the window still owes exactly one move. Before, each was a
// door, both could read "owes nothing", and both filed.
func TestLoaderOutbound_ConfirmAndLoadTogetherFileOne(t *testing.T) {
	t.Parallel()
	for i := 0; i < 10; i++ {
		window := "SC-ATLOAD-RACE-" + string(rune('A'+i))
		eng, db, core, _, nodeID := scLoaderEngine(t, window)
		core.set(window, true, "")
		l1 := scCoreBornL1(t, eng, db, window, "core-l1-race-"+window)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = eng.orderMgr.HandleCoreStatusPush(l1.UUID, protocol.StatusConfirmed, "auto_confirm_timeout")
		}()
		go func() {
			defer wg.Done()
			_ = eng.LoadBin(nodeID, "PART-SC", nil, scManifest)
		}()
		wg.Wait()

		moves := scMovesFrom(t, db, window)
		if len(moves) != 1 {
			t.Fatalf("round %d: L2s leaving %s = %d, want exactly 1 — one bin leaves a loader once", i, window, len(moves))
		}
		if moves[0].PayloadCode != "PART-SC" {
			t.Errorf("round %d: L2 payload %q, want PART-SC", i, moves[0].PayloadCode)
		}
		after, err := db.GetOrder(l1.ID)
		testutil.MustNoErr(t, err, "reload L1")
		if after.Status != orders.StatusConfirmed {
			t.Errorf("round %d: L1 status %q, want confirmed", i, after.Status)
		}
	}
}
