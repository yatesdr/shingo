//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// --- Characterization tests for handleOrderCompleted (wiring.go:410-495) ---
//
// handleOrderCompleted is the safety-net arrival path triggered by Edge receipt
// confirmation (HandleOrderReceipt → EventOrderCompleted). The bin should
// normally already be at its destination (moved by handleOrderDelivered at
// FINISHED), so the primary job is idempotency. These tests characterize
// each branch.

// deliveredOrder creates fixtures, dispatches a retrieve order, and drives it
// through RUNNING → FINISHED so it's in "delivered" status ready for receipt.
func deliveredOrder(t *testing.T) (db *store.DB, eng *Engine, sim *simulator.SimulatorBackend, d *dispatch.Dispatcher, order *store.Order, lineNode *store.Node) {
	t.Helper()
	db = testDB(t)
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-CO-1")

	sim = simulator.New()
	eng = newTestEngine(t, db, sim)
	d = eng.Dispatcher()

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "co-order-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  sd.Payload.Code,
		DeliveryNode: sd.LineNode.Name,
		Quantity:     1,
	})

	order = testdb.RequireOrder(t, db, "co-order-1")

	// Drive to delivered
	sim.DriveState(order.VendorOrderID, "RUNNING")
	sim.DriveState(order.VendorOrderID, "FINISHED")

	order = testdb.RequireOrderStatus(t, db, "co-order-1", "delivered")

	return db, eng, sim, d, order, sd.LineNode
}

// TC-CO-1: Normal receipt — bin already at destination (idempotent safety net).
// handleOrderCompleted should detect bin is already at dest and skip ApplyBinArrival.
func TestOrderCompleted_BinAlreadyAtDest(t *testing.T) {
	t.Parallel()
	db, _, _, d, order, lineNode := deliveredOrder(t)

	// Verify bin was already moved by handleOrderDelivered (FINISHED)
	if order.BinID != nil {
		bin, _ := db.GetBin(*order.BinID)
		if bin.NodeID != nil && *bin.NodeID == lineNode.ID {
			t.Logf("bin already at line node before receipt — good, idempotency path will trigger")
		}
	}

	// Send receipt — triggers handleOrderCompleted
	env := testEnvelope()
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "co-order-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	// Order should be confirmed after receipt
	got := testdb.AssertOrderStatus(t, db, "co-order-1", "confirmed")

	// Bin should still be at line node (no double-move)
	if got.BinID != nil {
		testdb.AssertBinAtNode(t, db, *got.BinID, lineNode.ID)
	}
}

// TC-CO-2: handleOrderCompleted with missing BinID — early return, no crash.
func TestOrderCompleted_NoBinID(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	// Create an order with no bin assigned
	order := &store.Order{
		EdgeUUID:     "co-no-bin",
		StationID:    "line-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		Status:       "delivered",
		SourceNode:   "STORAGE-A1",
		DeliveryNode: "LINE1-IN",
		// BinID is nil
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}

	// Should not panic — early return on nil BinID
	eng.handleOrderCompleted(OrderCompletedEvent{
		OrderID:   order.ID,
		EdgeUUID:  order.EdgeUUID,
		StationID: order.StationID,
	})
}

// TC-CO-3: handleOrderCompleted with missing source/delivery nodes — early return.
func TestOrderCompleted_MissingNodes(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	// Order with empty source and delivery nodes
	order := &store.Order{
		EdgeUUID:  "co-no-nodes",
		StationID: "line-1",
		OrderType: dispatch.OrderTypeRetrieve,
		Status:    "delivered",
		// SourceNode and DeliveryNode empty
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}

	// Should not panic — early return
	eng.handleOrderCompleted(OrderCompletedEvent{
		OrderID:   order.ID,
		EdgeUUID:  order.EdgeUUID,
		StationID: order.StationID,
	})
}

// TC-CO-4: handleOrderCompleted for non-existent order — log and return.
func TestOrderCompleted_NonExistentOrder(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	// Should not panic
	eng.handleOrderCompleted(OrderCompletedEvent{
		OrderID:   999999,
		EdgeUUID:  "ghost",
		StationID: "line-1",
	})
}

// TC-CO-5: handleOrderCompleted safety net — bin NOT at dest yet.
// Simulates the case where handleOrderDelivered failed or didn't move the bin.
// handleOrderCompleted should apply the arrival.
func TestOrderCompleted_SafetyNetArrival(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-CO-SN")

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	// Create a delivered order with bin still at source (simulating delivery arrival failure).
	order := &store.Order{
		EdgeUUID:     "co-safety-net",
		StationID:    "line-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		Status:       "delivered",
		SourceNode:   sd.StorageNode.Name,
		DeliveryNode: sd.LineNode.Name,
		BinID:        &bin.ID,
		PayloadCode:  sd.Payload.Code,
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}

	// Verify bin is still at storage node
	testdb.RequireBinAtNode(t, db, bin.ID, sd.StorageNode.ID)

	eng.handleOrderCompleted(OrderCompletedEvent{
		OrderID:   order.ID,
		EdgeUUID:  order.EdgeUUID,
		StationID: order.StationID,
	})

	// Bin should now be at the delivery (line) node via ApplyBinArrival
	testdb.AssertBinAtNode(t, db, bin.ID, sd.LineNode.ID)
}

// TestRegression_LinesideStagingPreventsFifoPoach is the load-bearing
// regression guard for the override removal. Asserts that after a complex
// order with WaitIndex>0 delivers a bin to a lineside (non-storage-slot)
// node, FindSourceBinFIFO does NOT return that bin. This is the exact poach
// path observed in the 4/14 incidents (two-robot swap + overnight loader).
//
// If a future well-meaning dev re-introduces a `staged=false` override for
// complex deliveries, this test fails with a clear message about the
// lineside bin being visible to FIFO retrieves.
func TestRegression_LinesideStagingPreventsFifoPoach(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	// Create a bin at storage, then deliver it to lineside via a complex
	// order with WaitIndex=1 (the path that previously triggered the
	// staged=false override).
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-FIFO-POACH")

	order := &store.Order{
		EdgeUUID:     "regr-fifo-poach",
		StationID:    "line-1",
		OrderType:    dispatch.OrderTypeComplex,
		Status:       "delivered",
		SourceNode:   sd.StorageNode.Name,
		DeliveryNode: sd.LineNode.Name,
		BinID:        &bin.ID,
		PayloadCode:  sd.Payload.Code,
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderWaitIndex(order.ID, 1); err != nil {
		t.Fatalf("update wait index: %v", err)
	}

	eng.handleOrderCompleted(OrderCompletedEvent{
		OrderID: order.ID, EdgeUUID: order.EdgeUUID, StationID: order.StationID,
	})

	// Sanity: bin moved to lineside and unclaimed
	binAfter := testdb.RequireBin(t, db, bin.ID)
	if binAfter.NodeID == nil || *binAfter.NodeID != sd.LineNode.ID {
		t.Fatalf("bin should be at lineNode after complete; got node=%v", binAfter.NodeID)
	}
	if binAfter.ClaimedBy != nil {
		t.Fatalf("bin should be unclaimed after delivery completion; got claimed_by=%v", binAfter.ClaimedBy)
	}

	// Critical: bin must be staged so FindSourceBinFIFO can't see it
	if binAfter.Status != "staged" {
		t.Fatalf("REGRESSION: lineside bin status = %q, want staged. The WaitIndex>0 "+
			"override has been re-introduced — lineside bins are now poachable by "+
			"FindSourceBinFIFO retrieve queries.", binAfter.Status)
	}

	// Direct assertion: FindSourceBinFIFO must not return this bin
	found, err := db.FindSourceBinFIFO(sd.Payload.Code)
	if err == nil && found != nil && found.ID == bin.ID {
		t.Errorf("REGRESSION: FindSourceBinFIFO returned the lineside-staged bin %d — "+
			"the staged exclusion in bin_manifest.go is not protecting lineside deliveries", bin.ID)
	}
}

// Note: TC-CO-6 (TestOrderCompleted_RetrieveEmptyOverride) and TC-CO-7
// (TestOrderCompleted_ComplexWaitOverride) deleted 2026-04-14 along with
// the staging overrides they validated. The overrides at applyBinArrivalForOrder
// (and three other sites in wiring.go) forced staged=false for complex orders
// with WaitIndex > 0 and for retrieve_empty deliveries. They allowed lineside
// bins to be poached via FindSourceBinFIFO. With the overrides removed, the
// tests no longer have anything to assert. See TestOrderCompleted_LinesideStaging
// below for the new positive-case test.

// TC-CO-6 replacement: lineside deliveries should arrive `staged` so that
// FindSourceBinFIFO excludes them — protecting from unloader/loader auto-request
// poaching. Covers both retrieve_empty (loader) and complex-order (swap) paths.
func TestOrderCompleted_LinesideStaging(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)

	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	// Case 1: retrieve_empty delivery to lineside (loader path).
	binEmpty := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-LSS-EMPTY")
	emptyOrder := &store.Order{
		EdgeUUID:     "lss-retrieve-empty",
		StationID:    "line-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		Status:       "delivered",
		SourceNode:   sd.StorageNode.Name,
		DeliveryNode: sd.LineNode.Name,
		BinID:        &binEmpty.ID,
		PayloadCode:  sd.Payload.Code,
		PayloadDesc:  "retrieve_empty",
	}
	if err := db.CreateOrder(emptyOrder); err != nil {
		t.Fatalf("create empty order: %v", err)
	}
	eng.handleOrderCompleted(OrderCompletedEvent{
		OrderID: emptyOrder.ID, EdgeUUID: emptyOrder.EdgeUUID, StationID: emptyOrder.StationID,
	})
	testdb.AssertBinAtNode(t, db, binEmpty.ID, sd.LineNode.ID)
	emptyAfter := testdb.RequireBin(t, db, binEmpty.ID)
	if emptyAfter.Status != "staged" {
		t.Errorf("retrieve_empty delivery: bin status = %q, want staged "+
			"(lineside delivery must be protected from FindSourceBinFIFO poaching)",
			emptyAfter.Status)
	}

	// Case 2: complex order with WaitIndex > 0 delivery to lineside (swap path).
	binComplex := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-LSS-CPLX")
	complexOrder := &store.Order{
		EdgeUUID:     "lss-complex-wait",
		StationID:    "line-1",
		OrderType:    dispatch.OrderTypeComplex,
		Status:       "delivered",
		SourceNode:   sd.StorageNode.Name,
		DeliveryNode: sd.LineNode.Name,
		BinID:        &binComplex.ID,
		PayloadCode:  sd.Payload.Code,
	}
	if err := db.CreateOrder(complexOrder); err != nil {
		t.Fatalf("create complex order: %v", err)
	}
	if err := db.UpdateOrderWaitIndex(complexOrder.ID, 1); err != nil {
		t.Fatalf("update wait index: %v", err)
	}
	eng.handleOrderCompleted(OrderCompletedEvent{
		OrderID: complexOrder.ID, EdgeUUID: complexOrder.EdgeUUID, StationID: complexOrder.StationID,
	})
	testdb.AssertBinAtNode(t, db, binComplex.ID, sd.LineNode.ID)
	complexAfter := testdb.RequireBin(t, db, binComplex.ID)
	if complexAfter.Status != "staged" {
		t.Errorf("complex order WaitIndex>0 delivery: bin status = %q, want staged "+
			"(WaitIndex>0 override removed; lineside bins now stage like simple orders)",
			complexAfter.Status)
	}
}
