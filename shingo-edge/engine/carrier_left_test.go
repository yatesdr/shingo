package engine

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
)

// carrier_left_test.go — the carrier left, so its identity left with it, at
// every door that says so.
//
// These drive the DOORS (the handlers and engine paths that null
// active_bin_id), not a verb, so they hold whichever layer the departure's
// identity write lives in. Each door starts from a bound carrier whose
// resident identity Core's delivery envelope named, and must end with the
// slot empty, the count zero, and the identity cleared as a departure:
// lineside_payload_code '', known false, source 'departed'.

const carrierLeftBin int64 = 31007

// seedCarrierAtSlot binds a carrier with a known resident identity at a fresh
// consume node, and returns the node's id and core name.
func seedCarrierAtSlot(t *testing.T, db *store.DB, prefix string) (nodeID, claimID int64, core string) {
	t.Helper()
	_, nodeID, _, claimID = seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: prefix, PayloadCode: "PART-CL", UOPCapacity: 100, InitialUOP: 50,
	})
	bid := carrierLeftBin
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bid, 50), "bind the carrier")
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeLinesidePayload(nodeID, "PART-CL", true, "delivery"),
		"seat the resident identity, as Core's delivery envelope does")
	return nodeID, claimID, prefix + "-NODE"
}

func assertCarrierLeft(t *testing.T, db *store.DB, nodeID int64, door string) {
	t.Helper()
	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil || rt == nil {
		t.Fatalf("%s: read runtime: %v", door, err)
	}
	if rt.ActiveBinID != nil {
		t.Errorf("%s: active_bin_id = %d, want nil — the carrier left", door, *rt.ActiveBinID)
	}
	if rt.RemainingUOPCached != 0 {
		t.Errorf("%s: remaining_uop_cached = %d, want 0 — the count goes with the carrier", door, rt.RemainingUOPCached)
	}
	if rt.LinesidePayloadCode != "" || rt.LinesidePayloadKnown || rt.LinesideSource != string("departed") {
		t.Errorf("%s: identity code=%q known=%v source=%q, want '' / false / departed — the carrier's "+
			"identity must leave with the carrier, or the next occupant inherits it",
			door, rt.LinesidePayloadCode, rt.LinesidePayloadKnown, rt.LinesideSource)
	}
}

// TestCarrierLeft_EveryDoorClearsTheIdentity walks the doors the brief names
// (Lane H1): the admin-Move release announcement, the bin pickup, the
// cancel reconcile's confirmed-empty arm, and the produce finalize reset.
// Order B's completion (applyOrderBSimple) calls the same verb as the
// produce reset and is covered by its own completion tests' pointer
// assertions.
func TestCarrierLeft_EveryDoorClearsTheIdentity(t *testing.T) {
	t.Parallel()

	t.Run("uop adjustment released", func(t *testing.T) {
		t.Parallel()
		db := testEngineDB(t)
		nodeID, _, core := seedCarrierAtSlot(t, db, "CL-REL")
		eng := testEngine(t, db)
		eng.HandleUOPAdjustment(protocol.UOPAdjustment{CoreNodeName: core, BinID: carrierLeftBin, Released: true})
		assertCarrierLeft(t, db, nodeID, "released")
	})

	t.Run("bin picked up", func(t *testing.T) {
		t.Parallel()
		db := testEngineDB(t)
		nodeID, _, core := seedCarrierAtSlot(t, db, "CL-PICK")
		bid := carrierLeftBin
		orderID, err := db.CreateOrder("uuid-cl-pick", orders.TypeRetrieve,
			&nodeID, false, 1, core, "", "", "", false, "PART-CL", "", "")
		testutil.MustNoErr(t, err, "create order")
		testutil.MustNoErr(t, db.UpdateOrderBinID(orderID, &bid), "order bin")
		eng := testEngine(t, db)
		eng.HandleBinPickedUp("uuid-cl-pick", carrierLeftBin, core)
		assertCarrierLeft(t, db, nodeID, "pickup")
	})

	t.Run("cancel reconcile confirmed empty", func(t *testing.T) {
		t.Parallel()
		db := testEngineDB(t)
		nodeID, claimID, _ := seedCarrierAtSlot(t, db, "CL-CAN")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("[]")) // Core: nothing physically at the node
		}))
		defer srv.Close()
		eng := testEngine(t, db)
		eng.coreClient = NewCoreClient(srv.URL)
		eng.reconcileActiveBinAfterCancel(nodeID, &claimID)
		assertCarrierLeft(t, db, nodeID, "cancel reconcile")
	})

	t.Run("produce finalize at release", func(t *testing.T) {
		t.Parallel()
		db := testEngineDB(t)
		nodeID, _, core := seedCarrierAtSlot(t, db, "CL-PROD")
		orderID, err := db.CreateOrder("uuid-cl-prod", orders.TypeComplex,
			&nodeID, false, 1, core, "", "", "", false, "PART-CL", "", "")
		testutil.MustNoErr(t, err, "create departing order")
		departing, err := db.GetOrder(orderID)
		testutil.MustNoErr(t, err, "read departing order")
		node, err := db.GetProcessNode(nodeID)
		testutil.MustNoErr(t, err, "read node")
		rt, err := db.GetProcessNodeRuntime(nodeID)
		testutil.MustNoErr(t, err, "read runtime")
		eng := testEngine(t, db)
		testutil.MustNoErr(t, eng.finalizeDepartingProduce(node, rt, departing, nil), "finalize")
		assertCarrierLeft(t, db, nodeID, "produce finalize")
	})
}

// TestCarrierBound_ByAdjustmentIsUnknown pins the bind arms' identity: a
// carrier bound by Core's announcement (admin Move onto the node, or a count
// correction binding a staged carrier) arrives with no payload on the
// envelope, so the slot's identity is UNKNOWN — never the previous
// occupant's.
func TestCarrierBound_ByAdjustmentIsUnknown(t *testing.T) {
	t.Parallel()

	t.Run("bound by admin move", func(t *testing.T) {
		t.Parallel()
		db := testEngineDB(t)
		nodeID, _, core := seedCarrierAtSlot(t, db, "CB-MOVE")
		eng := testEngine(t, db)
		eng.HandleUOPAdjustment(protocol.UOPAdjustment{
			CoreNodeName: core, BinID: 31008, Bound: true, NewRemaining: 60, Epoch: 3,
		})
		rt, err := db.GetProcessNodeRuntime(nodeID)
		testutil.MustNoErr(t, err, "read runtime")
		if rt.ActiveBinID == nil || *rt.ActiveBinID != 31008 || rt.RemainingUOPCached != 60 {
			t.Fatalf("bind: active_bin_id=%v remaining=%d, want 31008/60", rt.ActiveBinID, rt.RemainingUOPCached)
		}
		if rt.LinesidePayloadCode != "" || rt.LinesidePayloadKnown {
			t.Errorf("bind kept identity code=%q known=%v — the previous occupant's identity is a "+
				"confident wrong answer about this one", rt.LinesidePayloadCode, rt.LinesidePayloadKnown)
		}
	})

	t.Run("bound by count correction", func(t *testing.T) {
		t.Parallel()
		db := testEngineDB(t)
		nodeID, _, core := seedCarrierAtSlot(t, db, "CB-CORR")
		// The slot is empty (the carrier left) but still says PART-CL: the
		// staged-bind arm must not keep it.
		testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, nil), "empty the slot")
		testutil.MustNoErr(t, db.SetProcessNodeRuntimeLinesidePayload(nodeID, "PART-CL", true, "delivery"),
			"stale identity")
		eng := testEngine(t, db)
		eng.HandleUOPAdjustment(protocol.UOPAdjustment{
			CoreNodeName: core, BinID: 31009, NewRemaining: 40, Epoch: 2, Actor: "operator:test",
		})
		rt, err := db.GetProcessNodeRuntime(nodeID)
		testutil.MustNoErr(t, err, "read runtime")
		if rt.ActiveBinID == nil || *rt.ActiveBinID != 31009 {
			t.Fatalf("correction did not bind the staged carrier: active_bin_id=%v", rt.ActiveBinID)
		}
		if rt.LinesidePayloadCode != "" || rt.LinesidePayloadKnown {
			t.Errorf("correction bind kept identity code=%q known=%v", rt.LinesidePayloadCode, rt.LinesidePayloadKnown)
		}
	})
}

// TestClearLoaderHome_TheCarrierStaysKnownEmpty pins the clear-in-place
// identity: ClearLoaderHome zeroes the home carrier through Core and the
// carrier stays at the slot until Order A lifts it, so it is not a
// departure — it is known-empty, said by the operator whose clear it was
// (the same answer operator_bin_ops.go's CLEAR records).
func TestClearLoaderHome_TheCarrierStaysKnownEmpty(t *testing.T) {
	t.Parallel()
	srv, _ := intentCoreServer(t, []protocol.NodeBinInfo{
		{NodeName: "CLH-HOME", Occupied: true, PayloadCode: "CLH-PART", UOPRemaining: 5, BinID: 11},
		{NodeName: "CLH-BUF", Occupied: true, PayloadCode: "CLH-PART", UOPRemaining: 3, BinID: 12},
	}, nil)
	db := testEngineDB(t)
	nodeID := seedIntentNode(t, db, "CLH-ID-PROC", "CLH-HOME")
	_, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeLinesidePayload(nodeID, "CLH-PART", true, "delivery"),
		"resident identity")
	eng := newIntentEngine(t, db)
	eng.coreClient = NewCoreClient(srv.URL)
	eng.SetCoreLoaders([]protocol.LoaderInfo{consolidationLoader()})

	testutil.MustNoErr(t, eng.ClearLoaderHome(nodeID), "ClearLoaderHome")

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.LinesidePayloadCode != "" || !rt.LinesidePayloadKnown || rt.LinesideSource != "operator" {
		t.Errorf("identity after ClearLoaderHome code=%q known=%v source=%q, want '' / true / operator — "+
			"the carrier is still here and the clear says it is empty",
			rt.LinesidePayloadCode, rt.LinesidePayloadKnown, rt.LinesideSource)
	}
}
