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
	"shingoedge/store"
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

// fakeCoreBinServer stands in for Core's telemetry API in the ClearBin tests.
// It serves /api/telemetry/node-bins (the FetchNodeBins call ClearBin uses to
// decide whether a bin is physically present) with one bin of the given
// occupancy/payload, and answers {"status":"ok"} to everything else (notably
// the /api/telemetry/bin-clear POST ClearBin proxies). Without it, FetchNodeBins
// returns nothing → ClearBin treats the window as empty → no empty-out.
func fakeCoreBinServer(t *testing.T, occupied bool, payload string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/telemetry/node-bins" {
			bin := map[string]any{"occupied": occupied}
			if occupied {
				bin["payload_code"] = payload
			}
			json.NewEncoder(w).Encode([]map[string]any{bin})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// countMovesTo returns how many active move orders at the node target dest —
// used to assert the empty-out (U2) fired exactly once (no double).
func countMovesTo(t *testing.T, db *store.DB, nodeID int64, dest string) (n int, payload string) {
	t.Helper()
	all, err := db.ListActiveOrdersByProcessNode(nodeID)
	if err != nil {
		t.Fatalf("ListActiveOrdersByProcessNode: %v", err)
	}
	for _, o := range all {
		if o.OrderType == orders.TypeMove && o.DeliveryNode == dest {
			n++
			payload = o.PayloadCode
		}
	}
	return n, payload
}

// TestClearBin_FiresEmptyOut_AMRFed: an AMR-fed unloader (a delivered U1 brought
// the full) is cleared by the operator. ClearBin must (a) confirm the U1 and
// (b) create EXACTLY ONE empty-out (U2) move to the outbound. The "exactly one"
// is the regression guard for the refactor: the empty-out now fires from the
// clear itself, and the old U1-completion handler was removed — if it were still
// registered, the live completion chain (exercised here via the bridged emitter)
// would create a second U2.
func TestClearBin_FiresEmptyOut_AMRFed(t *testing.T) {
	t.Parallel()
	srv := fakeCoreBinServer(t, true, "PART-CLR")

	db := testEngineDB(t)
	unloaderNodeID, _ := seedManualSwapClaim(t, db, "U2-VIA-CLR", "consume", "PART-CLR", "STORAGE-NODE")

	orderID, err := db.CreateOrder("uuid-u1-clr", orders.TypeRetrieve,
		&unloaderNodeID, false, 1, "U2-VIA-CLR-MSWAP-NODE", "", "", "", false, "PART-CLR")
	if err != nil {
		t.Fatalf("create U1 order: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusDelivered)), "set U1 delivered")

	eng := testEngine(t, db)
	// Bridged emitter + wired handlers so ConfirmDelivery's terminal transition
	// fires EventOrderCompleted through the live completion chain — the U1
	// retrieve now falls through to normal_replenishment (the unloader_full_in
	// case is gone). This proves that fall-through creates no second empty-out.
	eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
	eng.coreClient = NewCoreClient(srv.URL)
	eng.wireEventHandlers()

	testutil.MustNoErr(t, eng.ClearBin(unloaderNodeID, ""), "ClearBin")

	after, err := db.GetOrder(orderID)
	if err != nil {
		t.Fatalf("reload U1: %v", err)
	}
	if after.Status != orders.StatusConfirmed {
		t.Errorf("U1 status = %q, want %q (ClearBin should confirm the inbound U1)", after.Status, orders.StatusConfirmed)
	}

	n, payload := countMovesTo(t, db, unloaderNodeID, "STORAGE-NODE")
	if n != 1 {
		t.Errorf("empty-out moves to STORAGE-NODE = %d, want exactly 1 (no double-fire)", n)
	}
	if payload != "PART-CLR" {
		t.Errorf("empty-out payload_code = %q, want %q (cleared bin's payload threads onto U2)", payload, "PART-CLR")
	}
}

// TestClearBin_FiresEmptyOut_PressFed is the core of the fix: a press/forklift-fed
// drain has NO inbound U1 (the press delivered the full directly), so the old
// U1-completion trigger never fired and the empty bin stranded. Driving the
// empty-out off the CLEAR makes it fire here too — one U2 move, payload threaded
// from Core's bin manifest (not from any order, because there is none).
func TestClearBin_FiresEmptyOut_PressFed(t *testing.T) {
	t.Parallel()
	srv := fakeCoreBinServer(t, true, "PRESS-PART")

	db := testEngineDB(t)
	unloaderNodeID, _ := seedManualSwapClaim(t, db, "U2-PRESS", "consume", "PRESS-PART", "EMPTY-TOTES")

	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(srv.URL)

	// No U1 order exists — the press fed the window directly.
	testutil.MustNoErr(t, eng.ClearBin(unloaderNodeID, ""), "ClearBin")

	n, payload := countMovesTo(t, db, unloaderNodeID, "EMPTY-TOTES")
	if n != 1 {
		t.Errorf("press-fed empty-out moves = %d, want exactly 1 (the fix: no U1 needed)", n)
	}
	if payload != "PRESS-PART" {
		t.Errorf("press-fed empty-out payload = %q, want %q (from the bin manifest)", payload, "PRESS-PART")
	}
}

// TestClearBin_NoEmptyOut_WhenWindowEmpty pins the hadBin gate: clearing a window
// Core reports as empty creates NO move — otherwise a stray clear would queue a
// phantom empty-out with no bin to pick up (the queue noise this refactor removes).
func TestClearBin_NoEmptyOut_WhenWindowEmpty(t *testing.T) {
	t.Parallel()
	srv := fakeCoreBinServer(t, false, "")

	db := testEngineDB(t)
	unloaderNodeID, _ := seedManualSwapClaim(t, db, "U2-EMPTY", "consume", "PART-X", "EMPTY-TOTES")

	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(srv.URL)

	testutil.MustNoErr(t, eng.ClearBin(unloaderNodeID, ""), "ClearBin")

	if n, _ := countMovesTo(t, db, unloaderNodeID, "EMPTY-TOTES"); n != 0 {
		t.Errorf("empty-out moves on empty window = %d, want 0 (hadBin gate)", n)
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
	if _, err := upsertClaimLegacySimple(db, processes.NodeClaimInput{
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

// seedLegacySimpleClaim seeds a process/node/style/claim carrying the retired
// "simple" swap mode as a legacy DB row (via upsertClaimLegacySimple). "simple"
// is no longer a configurable mode after the ingress lockdown, but legacy rows
// still exist and hit the surviving bare-move / nil-dispatch path — this seeds
// one so PushEmptyOut's guard can be exercised on it.
func seedLegacySimpleClaim(t *testing.T, db *store.DB, prefix string, role protocol.ClaimRole) (nodeID int64) {
	t.Helper()
	processID, err := db.CreateProcess(prefix+"-PROC", prefix+" simple", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create simple process: %v", err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: prefix + "-SIMPLE-NODE",
		Code:         prefix[:3],
		Name:         prefix + " simple",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create simple node: %v", err)
	}
	styleID, err := db.CreateStyle(prefix+"-SIMPLE-STYLE", prefix+" simple", processID)
	if err != nil {
		t.Fatalf("create simple style: %v", err)
	}
	db.SetActiveStyle(processID, &styleID)

	if _, err := upsertClaimLegacySimple(db, processes.NodeClaimInput{
		StyleID:      styleID,
		CoreNodeName: prefix + "-SIMPLE-NODE",
		Role:         role,
		SwapMode:     protocol.SwapModeSimple,
		PayloadCode:  "PART-SMP",
		UOPCapacity:  100,
	}); err != nil {
		t.Fatalf("upsert simple claim: %v", err)
	}
	db.EnsureProcessNodeRuntime(nodeID)
	return nodeID
}

// TestPushEmptyOut_EmptyWindow_CreatesExactlyOneMove: an occupied-but-empty
// carrier is sitting in the window. PushEmptyOut must queue exactly one move
// order (U2 empty-out) to the outbound destination.
func TestPushEmptyOut_EmptyWindow_CreatesExactlyOneMove(t *testing.T) {
	t.Parallel()
	// empty=true, payload="" → occupied but empty carrier
	srv := fakeCoreBinServer(t, true, "")

	db := testEngineDB(t)
	nodeID, _ := seedManualSwapClaim(t, db, "PE-OK", "consume", "PART-PE", "EMPTY-STORE")

	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(srv.URL)

	if err := eng.PushEmptyOut(nodeID); err != nil {
		t.Fatalf("PushEmptyOut: %v", err)
	}

	n, _ := countMovesTo(t, db, nodeID, "EMPTY-STORE")
	if n != 1 {
		t.Errorf("move orders to EMPTY-STORE = %d, want exactly 1", n)
	}
}

// TestPushEmptyOut_FullWindow_ReturnsError: the bin still has a payload —
// PushEmptyOut must refuse (the operator hasn't cleared it yet).
func TestPushEmptyOut_FullWindow_ReturnsError(t *testing.T) {
	t.Parallel()
	srv := fakeCoreBinServer(t, true, "PART-FULL")

	db := testEngineDB(t)
	nodeID, _ := seedManualSwapClaim(t, db, "PE-FULL", "consume", "PART-FULL", "EMPTY-STORE")

	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(srv.URL)

	if err := eng.PushEmptyOut(nodeID); err == nil {
		t.Fatal("PushEmptyOut on a full window: want error, got nil")
	}

	n, _ := countMovesTo(t, db, nodeID, "EMPTY-STORE")
	if n != 0 {
		t.Errorf("move orders = %d, want 0 (no order created for full bin)", n)
	}
}

// TestPushEmptyOut_NonManualSwapNode_ReturnsError: the node's claim is not
// manual_swap — PushEmptyOut must refuse before touching any orders.
func TestPushEmptyOut_NonManualSwapNode_ReturnsError(t *testing.T) {
	t.Parallel()
	srv := fakeCoreBinServer(t, true, "")

	db := testEngineDB(t)
	nodeID := seedLegacySimpleClaim(t, db, "SMP", "consume")

	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(srv.URL)

	if err := eng.PushEmptyOut(nodeID); err == nil {
		t.Fatal("PushEmptyOut on non-manual_swap node: want error, got nil")
	}

	all, _ := db.ListActiveOrdersByProcessNode(nodeID)
	if len(all) != 0 {
		t.Errorf("orders created = %d, want 0 (guard must fire before order creation)", len(all))
	}
}

// TestPushEmptyOut_DoubleTap_StillExactlyOneMove: two PushEmptyOut calls in
// quick succession for the same empty carrier. The in-flight guard must block
// the second call after the first order is created, leaving exactly one move.
func TestPushEmptyOut_DoubleTap_StillExactlyOneMove(t *testing.T) {
	t.Parallel()
	srv := fakeCoreBinServer(t, true, "")

	db := testEngineDB(t)
	nodeID, _ := seedManualSwapClaim(t, db, "PE-DUP", "consume", "PART-DUP", "EMPTY-STORE")

	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(srv.URL)

	if err := eng.PushEmptyOut(nodeID); err != nil {
		t.Fatalf("first PushEmptyOut: %v", err)
	}
	if err := eng.PushEmptyOut(nodeID); err == nil {
		t.Fatal("second PushEmptyOut: want error (in-flight guard), got nil")
	}

	n, _ := countMovesTo(t, db, nodeID, "EMPTY-STORE")
	if n != 1 {
		t.Errorf("move orders after double-tap = %d, want exactly 1 (guard must block second)", n)
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
