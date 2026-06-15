package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store/processes"
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
	gotID, ok := eng.confirmUnloaderU1OnClear("U1-CONF-MSWAP-NODE")
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
	_, ok := eng.confirmUnloaderU1OnClear("U1-IGN-L1-MSWAP-NODE")
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
	_, ok := eng.confirmUnloaderU1OnClear("U1-NDLV-MSWAP-NODE")
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

// TestLoadablePayloads_NotGatedByActiveStyle pins that the server gate is the
// loader-wide union of every configured payload — active OR inactive style — and
// is identical for normal and transitional loaders. The active-vs-all split is a
// board display concern, not a load-validation one: a loader responds to what is
// called for, not to the running style. This is what fixed the plant error
// (payload "…" not in allowed list for node) — loading a payload that belongs to
// a style other than the one currently running is no longer rejected.
func TestLoadablePayloads_NotGatedByActiveStyle(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	// Active style claims PART-A; a second (inactive) style on the same loader
	// claims PART-B. Loader-wide union = {PART-A, PART-B}.
	procID, nodeID, _ := seedActiveManualSwapLoader(t, db, "SNF2", "LOADER", "PART-A")
	inactive, err := db.CreateStyle("INACTIVE", "", procID)
	if err != nil {
		t.Fatalf("create inactive style: %v", err)
	}
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: inactive, CoreNodeName: "LOADER",
		Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeManualSwap,
		PayloadCode: "PART-B", AllowedPayloadCodes: []string{"PART-B"},
		InboundSource: "EMPTY-SUPER", OutboundDestination: "FG-MARKET", UOPCapacity: 100,
	}); err != nil {
		t.Fatalf("upsert inactive claim: %v", err)
	}

	node, _, claim, err := loadActiveNode(db, nodeID)
	if err != nil {
		t.Fatalf("loadActiveNode: %v", err)
	}

	// Normal loader: PART-B (an inactive style's payload) is loadable too — the
	// gate is the full configured set, not the active style.
	if got := eng.loadablePayloads(node, claim); !slices.Equal(got, []string{"PART-A", "PART-B"}) {
		t.Errorf("normal loader: loadablePayloads = %v, want [PART-A PART-B] (not gated by active style)", got)
	}

	// Operator-driven flag does not change the server gate — same union.
	if err := db.SetOperatorDrivenLoader("LOADER", true, "test"); err != nil {
		t.Fatalf("set operator-driven: %v", err)
	}
	if got := eng.loadablePayloads(node, claim); !slices.Equal(got, []string{"PART-A", "PART-B"}) {
		t.Errorf("operator-driven loader: loadablePayloads = %v, want [PART-A PART-B] (same loader-wide union)", got)
	}
}

// TestLoadablePayloads_NormalLoaderAcceptsSharedActiveDemand pins that even a
// NORMAL (non-transitional) loader is not gated to one node's active style: a
// loader physically shared by two cells (SNF2 → PART-A, SNF3 → PART-B) must
// accept either cell's active payload, because the system can demand either at
// any time. Pre-fix, loading at the SNF2 node rejected PART-B ("not in allowed
// list") even though SNF3's active style legitimately calls for it.
func TestLoadablePayloads_NormalLoaderAcceptsSharedActiveDemand(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	// Two active processes share LOADER with disjoint active payloads.
	_, snf2Node, _ := seedActiveManualSwapLoader(t, db, "SNF2", "LOADER", "PART-A")
	seedActiveManualSwapLoader(t, db, "SNF3", "LOADER", "PART-B")

	node, _, claim, err := loadActiveNode(db, snf2Node)
	if err != nil {
		t.Fatalf("loadActiveNode: %v", err)
	}

	// At SNF2's node, both active payloads are loadable — the loader-wide active
	// union, not just SNF2's own claim (PART-A).
	if got := eng.loadablePayloads(node, claim); !slices.Equal(got, []string{"PART-A", "PART-B"}) {
		t.Errorf("normal shared loader: loadablePayloads = %v, want [PART-A PART-B] (active union across cells)", got)
	}
}
