package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/orders"
)

// TestConfirmUnloaderU1OnClear_HappyPath verifies the helper finds the
// delivered U1 retrieve_full at the unloader and confirms it. This is the
// symmetric piece to confirmLoaderL1OnLoad — without it, the U1 sits at
// `delivered` after the operator's clear tap and handleUnloaderFullInCompletion
// never fires.
func TestConfirmUnloaderU1OnClear_HappyPath(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	unloaderNodeID, _ := seedManualSwapClaim(t, db, "U1-CONF", "consume", "PART-CONF", "STORAGE-NODE")

	// U1 = retrieve order (NOT retrieve_empty) at the unloader for the payload.
	// Seed it at `delivered` — that's the precondition for confirm.
	orderID, err := db.CreateOrder("uuid-u1-conf", orders.TypeRetrieve,
		&unloaderNodeID, false, 1, "U1-CONF-MSWAP-NODE", "", "", "", false, "PART-CONF")
	if err != nil {
		t.Fatalf("create U1 order: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusDelivered)), "set U1 delivered")

	eng := testEngine(t, db)
	gotID, ok := eng.confirmUnloaderU1OnClear(unloaderNodeID)
	if !ok {
		t.Fatalf("confirmUnloaderU1OnClear returned ok=false; want true (delivered U1 should be found)")
	}
	if gotID != orderID {
		t.Errorf("returned orderID = %d, want %d", gotID, orderID)
	}
	after, err := db.GetOrder(orderID)
	if err != nil {
		t.Fatalf("reload U1 order: %v", err)
	}
	if after.Status != orders.StatusConfirmed {
		t.Errorf("U1 status = %q, want %q after clear-confirm", after.Status, orders.StatusConfirmed)
	}
}

// TestConfirmUnloaderU1OnClear_IgnoresL1 verifies the U1/L1 discriminator:
// an L1 retrieve_empty at the same node must NOT be picked up by the
// unloader-side confirm. The filter is `!RetrieveEmpty`.
func TestConfirmUnloaderU1OnClear_IgnoresL1(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	unloaderNodeID, _ := seedManualSwapClaim(t, db, "U1-IGN-L1", "consume", "PART-IGN", "STORAGE-NODE")

	// Seed an L1 (retrieve_empty=true) at delivered — the wrong shape; helper
	// must skip it.
	orderID, err := db.CreateOrder("uuid-l1-ign", orders.TypeRetrieve,
		&unloaderNodeID, true, 1, "U1-IGN-L1-MSWAP-NODE", "", "", "", false, "PART-IGN")
	if err != nil {
		t.Fatalf("create L1 order: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusDelivered)), "set L1 delivered")

	eng := testEngine(t, db)
	_, ok := eng.confirmUnloaderU1OnClear(unloaderNodeID)
	if ok {
		t.Error("confirmUnloaderU1OnClear returned ok=true on an L1 (retrieve_empty=true); want false")
	}
	after, _ := db.GetOrder(orderID)
	if after.Status != orders.StatusDelivered {
		t.Errorf("L1 status = %q, want unchanged %q", after.Status, orders.StatusDelivered)
	}
}

// TestConfirmUnloaderU1OnClear_RequiresDelivered verifies the helper only
// confirms when the U1 is at `delivered` — not at any other status. Catches
// the rare ordering race where ClearBin is invoked before the fleet's
// delivery event has updated the order row.
func TestConfirmUnloaderU1OnClear_RequiresDelivered(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	unloaderNodeID, _ := seedManualSwapClaim(t, db, "U1-NDLV", "consume", "PART-NDLV", "STORAGE-NODE")

	orderID, err := db.CreateOrder("uuid-u1-ndlv", orders.TypeRetrieve,
		&unloaderNodeID, false, 1, "U1-NDLV-MSWAP-NODE", "", "", "", false, "PART-NDLV")
	if err != nil {
		t.Fatalf("create U1 order: %v", err)
	}
	// Leave it at `in_transit` — pre-delivery.
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusInTransit)), "set U1 in_transit")

	eng := testEngine(t, db)
	_, ok := eng.confirmUnloaderU1OnClear(unloaderNodeID)
	if ok {
		t.Error("confirmUnloaderU1OnClear returned ok=true on an in_transit U1; want false (only delivered confirms)")
	}
}

// TestClearBin_FiresU2ViaU1Confirm is the integration test for the symmetric
// fix: operator tap on a consume manual_swap card invokes Engine.ClearBin,
// which (a) confirms the delivered U1 and (b) triggers
// handleUnloaderFullInCompletion via the OrderCompleted event bus → U2 move
// order created. Pre-fix, step (a) was missing and step (b) never fired —
// bin cleared physically, order stuck at `delivered`.
func TestClearBin_FiresU2ViaU1Confirm(t *testing.T) {
	t.Parallel()
	// Fake Core HTTP server — answers OK to the bin-clear call ClearBin
	// proxies through coreClient. Without this, eng.coreClient.ClearBin
	// would error and the test would fail before we got to verify U2.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()

	db := testEngineDB(t)
	unloaderNodeID, _ := seedManualSwapClaim(t, db, "U2-VIA-CLR", "consume", "PART-CLR", "STORAGE-NODE")

	orderID, err := db.CreateOrder("uuid-u1-clr", orders.TypeRetrieve,
		&unloaderNodeID, false, 1, "U2-VIA-CLR-MSWAP-NODE", "", "", "", false, "PART-CLR")
	if err != nil {
		t.Fatalf("create U1 order: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusDelivered)), "set U1 delivered")

	eng := testEngine(t, db)
	// Swap in a real bridged emitter so ConfirmDelivery's terminal
	// transition fires EventOrderCompleted on eng.Events, where
	// handleUnloaderFullInCompletion is subscribed.
	eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
	eng.coreClient = NewCoreClient(srv.URL)
	eng.wireEventHandlers()

	testutil.MustNoErr(t, eng.ClearBin(unloaderNodeID), "ClearBin")

	// U1 must have been confirmed by ClearBin's pre-clear confirm step.
	after, err := db.GetOrder(orderID)
	if err != nil {
		t.Fatalf("reload U1: %v", err)
	}
	if after.Status != orders.StatusConfirmed {
		t.Errorf("U1 status = %q, want %q (ClearBin should have confirmed before clearing)", after.Status, orders.StatusConfirmed)
	}

	// U2 must exist as a move order from the unloader → OutboundDestination
	// (STORAGE-NODE per the seed). Created by handleUnloaderFullInCompletion
	// after consuming the OrderCompleted event.
	all, err := db.ListActiveOrdersByProcessNode(unloaderNodeID)
	if err != nil {
		t.Fatalf("ListActiveOrdersByProcessNode: %v", err)
	}
	var u2Found bool
	for _, o := range all {
		if o.OrderType == orders.TypeMove && o.DeliveryNode == "STORAGE-NODE" {
			u2Found = true
			if o.PayloadCode != "PART-CLR" {
				t.Errorf("U2 payload_code = %q, want %q (must thread U1 payload onto U2)", o.PayloadCode, "PART-CLR")
			}
		}
	}
	if !u2Found {
		t.Errorf("expected U2 move from unloader to STORAGE-NODE after ClearBin; got %+v", all)
	}
}
