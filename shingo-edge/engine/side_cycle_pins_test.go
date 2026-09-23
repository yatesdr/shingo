package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// side_cycle_pins_test.go — characterisation pins for the loader/unloader side
// cycle at c0c525c0, written before the half-loader work changes any of it.
//
// EVERY ASSERTION HERE IS TODAY'S BEHAVIOUR, including behaviour that is a
// defect. The D3 pins below assert the defect (an L1 that never confirms, an L1
// confirm that creates no L2); when D3 is fixed they flip, and the flip is the
// predicted test diff. They are not a statement that the behaviour is right.
//
// The Core-owned-loader cases use the shape every loader has after the first
// node-list sync: a Core loader in the cache and NO stored style_node_claim, so
// the operator doors run on domain.Loader.SynthClaim (operator_helpers.go
// loadActiveNode) while the completion chain reads stored claims only
// (orderCompletionCtx.Claim).

// ── fixture: a Core stand-in with one mutable window per node ──────────────

// scWindow is what the fake Core reports at one node.
type scWindow struct {
	occupied bool
	payload  string
}

// scCore serves node-bins, bin-load and bin-clear from a per-node map the test
// mutates as the physical world changes (empty arrives, bin leaves).
type scCore struct {
	mu      sync.Mutex
	windows map[string]*scWindow
	srv     *httptest.Server
}

func newSCCore(t *testing.T) *scCore {
	t.Helper()
	c := &scCore{windows: map[string]*scWindow{}}
	c.srv = httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *scCore) set(node string, occupied bool, payload string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.windows[node] = &scWindow{occupied: occupied, payload: payload}
}

func (c *scCore) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch r.URL.Path {
	case "/api/telemetry/node-bins":
		var out []NodeBinInfo
		for _, n := range strings.Split(r.URL.Query().Get("nodes"), ",") {
			win := c.windows[n]
			if win == nil {
				out = append(out, NodeBinInfo{NodeName: n})
				continue
			}
			out = append(out, NodeBinInfo{NodeName: n, Occupied: win.occupied, PayloadCode: win.payload, BinID: 77})
		}
		_ = json.NewEncoder(w).Encode(out)
	case "/api/telemetry/bin-load":
		var req BinLoadRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if win := c.windows[req.NodeName]; win != nil {
			win.payload = req.PayloadCode
		}
		_ = json.NewEncoder(w).Encode(BinLoadResponse{
			Status: "ok", BinID: 77, PayloadCode: req.PayloadCode, UOPRemaining: 40, DeltaEpoch: 3,
		})
	case "/api/telemetry/bin-clear":
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		if win := c.windows[req["node_name"]]; win != nil {
			win.payload = ""
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "bin_id": 77, "delta_epoch": 4})
	default:
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

// scLog records logFn lines so a pin can read the loader_budget decision record.
type scLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *scLog) fn(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *scLog) matching(sub string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			out = append(out, s)
		}
	}
	return out
}

// scLoaderEngine stands up a Core-owned OPERATOR-staged produce loader with one
// window and no stored claim, wired to the live completion chain.
func scLoaderEngine(t *testing.T, window string) (*Engine, *store.DB, *scCore, *scLog, int64) {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
	eng.wireEventHandlers()
	core := newSCCore(t)
	eng.coreClient = NewCoreClient(core.srv.URL)
	logs := &scLog{}
	eng.logFn = logs.fn

	procID, err := db.CreateProcess(window+"-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: window, Code: "W1", Name: window, Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	seedCoreLoader(t, eng, sharedLoaderInfo(window, "produce", "operator", "PART-SC", 0, 0))

	if _, _, claim, _ := eng.loadActiveNode(nodeID); claim == nil || claim.ID != 0 {
		t.Fatalf("fixture: want a synthesized claim (ID 0) at %s, got %+v", window, claim)
	}
	return eng, db, core, logs, nodeID
}

// scDeliver drives an order to delivered through the Edge's own delivery
// handler, from dispatched (the status the echo or the test stamped).
func scDeliver(t *testing.T, eng *Engine, db *store.DB, o *storeorders.Order) {
	t.Helper()
	testutil.MustNoErr(t, db.UpdateOrderStatus(o.ID, string(protocol.StatusDispatched)), "stamp dispatched")
	bin := int64(77)
	testutil.MustNoErr(t, eng.orderMgr.HandleDeliveredWithExpiry(o.UUID, "delivered", nil, &bin, nil, nil, 0, o.DeliveryNode, ""),
		"deliver order "+o.UUID)
}

// scMovesFrom returns the non-terminal move orders leaving coreNode.
func scMovesFrom(t *testing.T, db *store.DB, coreNode string) []storeorders.Order {
	t.Helper()
	all, err := db.ListOrders()
	testutil.MustNoErr(t, err, "list orders")
	var out []storeorders.Order
	for _, o := range all {
		if o.OrderType == orders.TypeMove && o.SourceNode == coreNode && !o.Status.IsTerminal() {
			out = append(out, o)
		}
	}
	return out
}

// scActiveEmptiesTo counts the non-terminal retrieve_empty orders delivering to
// coreNode, whichever spelling of the type the row carries.
func scActiveEmptiesTo(t *testing.T, db *store.DB, coreNode string) int {
	t.Helper()
	all, err := db.ListActiveOrdersByDeliveryNodeSet([]string{coreNode})
	testutil.MustNoErr(t, err, "list active by delivery node")
	n := 0
	for _, o := range all {
		if o.RetrieveEmpty {
			n++
		}
	}
	return n
}

// scEcho applies the projection Core sends back for an Edge-authored L1:
// ProjectionFor spells a promoted empty-in `retrieve_empty` (Core's
// lifecycle_service promotes retrieve+flag), and UpsertProjection overwrites the
// stored order_type with it.
func scEcho(t *testing.T, eng *Engine, o *storeorders.Order) {
	t.Helper()
	_, err := eng.ApplyOrderProjection(protocol.OrderProjection{
		OrderUUID:     o.UUID,
		OrderType:     protocol.OrderTypeRetrieveEmpty,
		Status:        string(protocol.StatusDispatched),
		Quantity:      1,
		SourceNode:    o.SourceNode,
		DeliveryNode:  o.DeliveryNode,
		RetrieveEmpty: true,
	})
	testutil.MustNoErr(t, err, "apply echo")
}

// scLand drives an auto-confirming move through delivery, so its confirm runs
// the completion chain exactly as Core's delivery envelope would.
func scLand(t *testing.T, eng *Engine, db *store.DB, move storeorders.Order) {
	t.Helper()
	scDeliver(t, eng, db, &move)
	after, err := db.GetOrder(move.ID)
	testutil.MustNoErr(t, err, "reload move")
	if after.Status != orders.StatusConfirmed {
		t.Fatalf("fixture: move %d status %q after delivery, want confirmed (auto-confirm)", move.ID, after.Status)
	}
}

var scManifest = []protocol.IngestManifestItem{{PartNumber: "PN-SC", Quantity: 40, Description: "synthetic"}}

// ── D3a: LOAD after the echo ─────────────────────────────────────────────

// TestPinD3a_LoadAfterEcho_TakesTheFallbackAndStrandsTheL1 is D3's first pin,
// in the shape the plants run: the Edge creates the L1, Core's projection echoes
// it back as retrieve_empty, it is delivered, and the operator LOADs at a
// Core-owned window.
//
// AT BASE: confirmLoaderL1OnLoad looks for order_type='retrieve' only, so the
// echoed L1 is invisible to it. LOAD takes the fallback, which files the L2
// (bins still flow), and the L1 stays `delivered` for good. After the L2 lands
// and the window is empty, the next push computes to_fire=0 because
// withLoaderBudget counts the stranded L1 as in flight.
//
// FLIPS WITH D3: the L1 confirms, the L2 comes from the completion chain
// (via L1-completion), and the push fires.
func TestPinD3a_LoadAfterEcho_TakesTheFallbackAndStrandsTheL1(t *testing.T) {
	t.Parallel()
	const window = "SC-D3A-W1"
	eng, db, core, logs, nodeID := scLoaderEngine(t, window)
	core.set(window, false, "")

	l1, err := eng.RequestEmptyBin(nodeID, "")
	testutil.MustNoErr(t, err, "RequestEmptyBin")
	if l1.OrderType != orders.TypeRetrieve || !l1.RetrieveEmpty {
		t.Fatalf("fixture: Edge L1 = %s retrieve_empty=%v, want retrieve + flag", l1.OrderType, l1.RetrieveEmpty)
	}
	scEcho(t, eng, l1)
	echoed, err := db.GetOrder(l1.ID)
	testutil.MustNoErr(t, err, "reload L1")
	if echoed.OrderType != protocol.OrderTypeRetrieveEmpty {
		t.Fatalf("fixture: echo did not rewrite the type (got %q) — the pin would test a path the plants never run", echoed.OrderType)
	}

	core.set(window, true, "") // the empty lands
	scDeliver(t, eng, db, l1)
	testutil.MustNoErr(t, eng.LoadBin(nodeID, "", nil, scManifest), "LoadBin")

	after, err := db.GetOrder(l1.ID)
	testutil.MustNoErr(t, err, "reload L1 after LOAD")
	if after.Status != orders.StatusDelivered {
		t.Fatalf("PREMISE CHECK: L1 status after LOAD = %q, want delivered (D3a). If it confirmed, "+
			"the echoed type is already found and D3's premise is wrong — stop.", after.Status)
	}
	moves := scMovesFrom(t, db, window)
	if len(moves) != 1 || moves[0].DeliveryNode != "FG-MARKET" {
		t.Fatalf("moves leaving %s = %+v, want exactly one L2 to FG-MARKET from LOAD's fallback", window, moves)
	}
	// The L2 lands; the window is empty; the stranded L1 still counts.
	core.set(window, false, "")
	scLand(t, eng, db, moves[0])
	if n := scActiveEmptiesTo(t, db, window); n != 1 {
		t.Fatalf("active empties at %s after the L2 landed = %d, want 1 (the stranded L1)", window, n)
	}
	eng.MaybePushLoader(nodeID)
	decisions := logs.matching("loader_budget loader=loader:" + window)
	if len(decisions) == 0 {
		t.Fatalf("no loader_budget decision recorded for %s", window)
	}
	last := decisions[len(decisions)-1]
	if !strings.Contains(last, "to_fire=0") || !strings.Contains(last, "in_flight_total=1") {
		t.Errorf("next push decision = %q, want to_fire=0 with in_flight_total=1 — the stranded L1 holds the window", last)
	}
	if n := scActiveEmptiesTo(t, db, window); n != 1 {
		t.Errorf("active empties after the push = %d, want 1 (nothing new staged)", n)
	}
}

// ── D3b: the confirmed branch at a Core-owned loader ─────────────────────

// TestPinD3b_ConfirmedL1AtACoreOwnedLoader_CreatesNoL2 is D3's second pin: what
// LOAD's confirmed branch does once the lookup can see the L1. The lookup is
// made to see it here by leaving the row's type as the Edge wrote it (no echo),
// which is exactly the row a type-agnostic lookup finds after the echo.
//
// AT BASE: LOAD confirms the L1 and returns, expecting the completion chain to
// file the L2. matchLoaderEmptyIn reads orderCompletionCtx.Claim(), which
// resolves stored claims only; a Core-owned loader has none, IsLoaderNode() on
// nil is false, the row never matches, and NO L2 is created. The loaded bin
// would sit on the window.
//
// FLIPS WITH D3: one claim resolver (stored claim, else SynthClaim) serves the
// completion ctx, so loader_empty_in matches and files exactly one L2.
func TestPinD3b_ConfirmedL1AtACoreOwnedLoader_CreatesNoL2(t *testing.T) {
	t.Parallel()
	const window = "SC-D3B-W1"
	eng, db, core, _, nodeID := scLoaderEngine(t, window)
	core.set(window, false, "")

	l1, err := eng.RequestEmptyBin(nodeID, "")
	testutil.MustNoErr(t, err, "RequestEmptyBin")
	core.set(window, true, "")
	scDeliver(t, eng, db, l1)
	testutil.MustNoErr(t, eng.LoadBin(nodeID, "", nil, scManifest), "LoadBin")

	after, err := db.GetOrder(l1.ID)
	testutil.MustNoErr(t, err, "reload L1")
	if after.Status != orders.StatusConfirmed {
		t.Fatalf("L1 status = %q, want confirmed (the un-echoed row is found by today's lookup)", after.Status)
	}
	if after.FinalCount == nil || *after.FinalCount != 40 {
		t.Errorf("L1 final count = %v, want 40 (Core's LoadBin answer, not the argument)", after.FinalCount)
	}
	if moves := scMovesFrom(t, db, window); len(moves) != 0 {
		t.Errorf("moves leaving %s = %d, want 0 at base — the completion ctx cannot see a Core-owned "+
			"loader, so the confirmed branch files no L2 (D3b)", window, len(moves))
	}
}

// ── Core-owned loader: what an L2 landing does ───────────────────────────

// TestPinCoreOwnedLoader_L2LandingNeitherClearsNorRePushes pins the two
// applyManualSwap effects a Core-owned loader does not get at base, because
// matchManualSwap reads the same stored-claim-only ctx.Claim():
//
//   - the runtime order pointer is NOT cleared (ClearProcessNodeRuntimeOrders
//     never runs), so it still names the landed L2;
//   - the produce re-push (MaybePushLoader) does NOT run, so a free window with
//     nothing in flight gets no empty until something else pushes.
//
// The control at the end calls MaybePushLoader directly and it stages one empty,
// proving the window was free and the push would have fired.
//
// FLIPS WITH D3b: the pointer is cleared and the empty is staged on the landing.
func TestPinCoreOwnedLoader_L2LandingNeitherClearsNorRePushes(t *testing.T) {
	t.Parallel()
	const window = "SC-L2-W1"
	eng, db, core, _, nodeID := scLoaderEngine(t, window)
	core.set(window, true, "") // an empty placed by hand — no L1 behind it

	testutil.MustNoErr(t, eng.LoadBin(nodeID, "", nil, scManifest), "LoadBin")
	moves := scMovesFrom(t, db, window)
	if len(moves) != 1 {
		t.Fatalf("moves leaving %s = %d, want 1 (LOAD's fallback L2)", window, len(moves))
	}
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.ActiveOrderID == nil || *rt.ActiveOrderID != moves[0].ID {
		t.Fatalf("fixture: runtime active order = %v, want the L2 %d", rt.ActiveOrderID, moves[0].ID)
	}

	core.set(window, false, "") // the bin leaves
	scLand(t, eng, db, moves[0])

	rt, err = db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime after landing")
	if rt.ActiveOrderID == nil || *rt.ActiveOrderID != moves[0].ID {
		t.Errorf("runtime active order after the L2 landed = %v, want still %d at base "+
			"(applyManualSwap's pointer clear does not run for a Core-owned loader)", rt.ActiveOrderID, moves[0].ID)
	}
	if n := scActiveEmptiesTo(t, db, window); n != 0 {
		t.Errorf("empties staged by the landing = %d, want 0 at base (no produce re-push)", n)
	}

	eng.MaybePushLoader(nodeID)
	if n := scActiveEmptiesTo(t, db, window); n != 1 {
		t.Errorf("control: MaybePushLoader on the free window staged %d empties, want 1", n)
	}
}

// ── A3.2: the confirm-delivered pair ─────────────────────────────────────

// scDeliveredRetrieve seeds a delivered retrieve at deliveryNode, tracked at
// processNodeID, with the given stored type spelling.
func scDeliveredRetrieve(t *testing.T, db *store.DB, uuid string, typ protocol.OrderType, processNodeID int64, retrieveEmpty bool, deliveryNode string) int64 {
	t.Helper()
	id, err := db.CreateOrder(uuid, typ, &processNodeID, retrieveEmpty, 1, deliveryNode, "", "", "", false, "", "", "")
	testutil.MustNoErr(t, err, "create "+uuid)
	testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(orders.StatusDelivered)), "deliver "+uuid)
	return id
}

// TestPinConfirmL1_OldestDeliveredAtTheCoreNode_WithTheSeatedCount: the loader
// side of the pair confirms the OLDEST delivered empty-in at the core node, finds
// it through a sibling process_node row, skips a delivered U1 at the same node,
// and records the count it was handed.
func TestPinConfirmL1_OldestDeliveredAtTheCoreNode_WithTheSeatedCount(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, _ := seedManualSwapClaim(t, db, "CL1-PAIR", "produce", "PART-CL", "FG-MARKET")
	sibling, _ := seedManualSwapClaim(t, db, "CL1-SIB", "produce", "PART-CL", "FG-MARKET")
	const core = "CL1-PAIR-MSWAP-NODE"

	u1 := scDeliveredRetrieve(t, db, "cl1-u1", orders.TypeRetrieve, nodeID, false, core)
	oldest := scDeliveredRetrieve(t, db, "cl1-old", orders.TypeRetrieve, sibling, true, core)
	newer := scDeliveredRetrieve(t, db, "cl1-new", orders.TypeRetrieve, nodeID, true, core)

	eng := testEngine(t, db)
	got, ok := eng.confirmLoaderL1OnLoad(core, 55)
	if !ok || got != oldest {
		t.Fatalf("confirmLoaderL1OnLoad = (%d, %v), want (%d, true): the oldest delivered empty-in "+
			"at the core node, even though a sibling process_node tracks it", got, ok, oldest)
	}
	o, _ := db.GetOrder(oldest)
	if o.Status != orders.StatusConfirmed || o.FinalCount == nil || *o.FinalCount != 55 {
		t.Errorf("confirmed L1 = status %q final %v, want confirmed with 55", o.Status, o.FinalCount)
	}
	for _, id := range []int64{u1, newer} {
		if o, _ := db.GetOrder(id); o.Status != orders.StatusDelivered {
			t.Errorf("order %d status = %q, want delivered (one confirm per tap, never the U1)", id, o.Status)
		}
	}
}

// TestPinConfirmL1_MissesTheEchoedTypeSpelling is D3a at the store query: a
// delivered empty-in whose stored type is `retrieve_empty` (what the echo
// writes) is not found. FLIPS WITH D3a: the lookup keys on retrieve_empty.
func TestPinConfirmL1_MissesTheEchoedTypeSpelling(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, _ := seedManualSwapClaim(t, db, "CL1-ECHO", "produce", "PART-CE", "FG-MARKET")
	const core = "CL1-ECHO-MSWAP-NODE"
	id := scDeliveredRetrieve(t, db, "cl1-echo", protocol.OrderTypeRetrieveEmpty, nodeID, true, core)

	eng := testEngine(t, db)
	if got, ok := eng.confirmLoaderL1OnLoad(core, 10); ok {
		t.Errorf("confirmLoaderL1OnLoad found order %d at base; want a miss — the lookup filters "+
			"order_type='retrieve' and the echo spelled it retrieve_empty (D3a)", got)
	}
	if o, _ := db.GetOrder(id); o.Status != orders.StatusDelivered {
		t.Errorf("echoed L1 status = %q, want delivered", o.Status)
	}
}

// TestPinConfirmU1_RecordsAZeroFinalCount: the unloader side of the pair records
// 0 — the carrier is empty once the operator has processed it.
func TestPinConfirmU1_RecordsAZeroFinalCount(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, _ := seedManualSwapClaim(t, db, "CU1-ZERO", "consume", "PART-CZ", "EMPTY-TOTES")
	const core = "CU1-ZERO-MSWAP-NODE"
	id := scDeliveredRetrieve(t, db, "cu1-zero", orders.TypeRetrieve, nodeID, false, core)

	eng := testEngine(t, db)
	if got, ok := eng.confirmUnloaderU1OnClear(core); !ok || got != id {
		t.Fatalf("confirmUnloaderU1OnClear = (%d, %v), want (%d, true)", got, ok, id)
	}
	o, _ := db.GetOrder(id)
	if o.FinalCount == nil || *o.FinalCount != 0 {
		t.Errorf("U1 final count = %v, want 0", o.FinalCount)
	}
}

// ── A3.3: the double-tap guard ───────────────────────────────────────────

// TestPinDoubleTapGuard_KeysOnAnyMoveAtTheProcessNode pins what the guard in
// ClearBin and PushEmptyOut actually checks: any non-terminal MOVE tracked at
// this process_node — including one ARRIVING here. That is the opposite
// direction from outboundMoveInFlight (core node, departures only), which is why
// the two must not be merged. PUSH EMPTY refuses; CLEAR skips the U2 without
// failing the clear.
func TestPinDoubleTapGuard_KeysOnAnyMoveAtTheProcessNode(t *testing.T) {
	t.Parallel()
	srv := fakeCoreBinServer(t, true, "")
	db := testEngineDB(t)
	nodeID, _ := seedManualSwapClaim(t, db, "DT-ARR", "consume", "PART-DT", "EMPTY-TOTES")
	const core = "DT-ARR-MSWAP-NODE"
	// A move ARRIVING at this window, tracked here.
	inbound, err := db.CreateOrder("dt-arriving", orders.TypeMove, &nodeID, false, 1, core, "", "ELSEWHERE", "", true, "", "", "")
	testutil.MustNoErr(t, err, "create arriving move")

	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(srv.URL)
	err = eng.PushEmptyOut(nodeID)
	if err == nil || !strings.Contains(err.Error(), "already has an empty-out in flight") {
		t.Errorf("PushEmptyOut with an arriving move tracked here = %v, want the in-flight refusal", err)
	}
	if eng.outboundMoveInFlight(nodeID, core) {
		t.Errorf("outboundMoveInFlight saw the arriving move %d — it counts departures only", inbound)
	}
	if n, _ := countMovesTo(t, db, nodeID, "EMPTY-TOTES"); n != 0 {
		t.Errorf("empty-outs created = %d, want 0", n)
	}
}

// TestPinDoubleTapGuard_ClearAfterPushEmptySkips: a CLEAR landing after a PUSH
// EMPTY already filed the U2 skips its own U2 and still succeeds.
func TestPinDoubleTapGuard_ClearAfterPushEmptySkips(t *testing.T) {
	t.Parallel()
	srv := fakeCoreBinServer(t, true, "")
	db := testEngineDB(t)
	nodeID, _ := seedManualSwapClaim(t, db, "DT-PEC", "consume", "PART-DP", "EMPTY-TOTES")

	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(srv.URL)
	testutil.MustNoErr(t, eng.PushEmptyOut(nodeID), "PushEmptyOut")
	testutil.MustNoErr(t, eng.ClearBin(nodeID, ""), "ClearBin after PUSH EMPTY")
	if n, _ := countMovesTo(t, db, nodeID, "EMPTY-TOTES"); n != 1 {
		t.Errorf("empty-outs after PUSH EMPTY then CLEAR = %d, want 1", n)
	}
}

// ── A3.4: the three outbound-resolve sites ───────────────────────────────

// scStoredWithCore seeds a stored manual_swap claim whose outbound is claimDest,
// then a Core loader for the same node whose outbound is coreDest. The claim is
// seeded AFTER the loader sync so the quarantine does not move it — the shape
// that survives until a node-list response arrives, or a style clone re-mints.
func scStoredWithCore(t *testing.T, prefix string, role protocol.ClaimRole, claimDest, coreDest string) (*Engine, *store.DB, int64, string) {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	core := prefix + "-MSWAP-NODE"
	if coreDest != "" {
		info := sharedLoaderInfo(core, string(role), "operator", "PART-OB", 0, 0)
		info.OutboundDest = coreDest
		seedCoreLoader(t, eng, info)
	}
	nodeID, _ := seedManualSwapClaim(t, db, prefix, role, "PART-OB", claimDest)
	if _, _, claim, _ := eng.loadActiveNode(nodeID); claim == nil || claim.ID == 0 {
		t.Fatalf("fixture: want the STORED claim at %s, got %+v", core, claim)
	}
	return eng, db, nodeID, core
}

func scDestsFrom(t *testing.T, db *store.DB, core string) []string {
	t.Helper()
	var out []string
	for _, m := range scMovesFrom(t, db, core) {
		out = append(out, m.DeliveryNode)
	}
	return out
}

// TestPinOutbound_CoreOverridesTheClaim_AtAllThreeSites: where the stored claim
// and Core's loader disagree, all three creators route to Core's outbound —
// LoadBin's fallback, createUnloaderEmptyOut, and applyLoaderEmptyIn.
func TestPinOutbound_CoreOverridesTheClaim_AtAllThreeSites(t *testing.T) {
	t.Parallel()

	t.Run("load_fallback", func(t *testing.T) {
		t.Parallel()
		eng, db, nodeID, core := scStoredWithCore(t, "OBF", protocol.ClaimRoleProduce, "OLD-DEST", "NEW-DEST")
		c := newSCCore(t)
		c.set(core, true, "")
		eng.coreClient = NewCoreClient(c.srv.URL)
		testutil.MustNoErr(t, eng.LoadBin(nodeID, "PART-OB", nil, scManifest), "LoadBin")
		if got := scDestsFrom(t, db, core); len(got) != 1 || got[0] != "NEW-DEST" {
			t.Errorf("L2 destinations = %v, want [NEW-DEST]", got)
		}
	})

	t.Run("unloader_empty_out", func(t *testing.T) {
		t.Parallel()
		eng, db, nodeID, core := scStoredWithCore(t, "OBU", protocol.ClaimRoleConsume, "OLD-DEST", "NEW-DEST")
		c := newSCCore(t)
		c.set(core, true, "")
		eng.coreClient = NewCoreClient(c.srv.URL)
		testutil.MustNoErr(t, eng.PushEmptyOut(nodeID), "PushEmptyOut")
		if got := scDestsFrom(t, db, core); len(got) != 1 || got[0] != "NEW-DEST" {
			t.Errorf("U2 destinations = %v, want [NEW-DEST]", got)
		}
	})

	t.Run("l1_completion", func(t *testing.T) {
		t.Parallel()
		eng, db, nodeID, core := scStoredWithCore(t, "OBC", protocol.ClaimRoleProduce, "OLD-DEST", "NEW-DEST")
		eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
		eng.wireEventHandlers()
		c := newSCCore(t)
		c.set(core, true, "")
		eng.coreClient = NewCoreClient(c.srv.URL)
		scDeliveredRetrieve(t, db, "obc-l1", orders.TypeRetrieve, nodeID, true, core)
		testutil.MustNoErr(t, eng.LoadBin(nodeID, "PART-OB", nil, scManifest), "LoadBin")
		if got := scDestsFrom(t, db, core); len(got) != 1 || got[0] != "NEW-DEST" {
			t.Errorf("L2 destinations = %v, want [NEW-DEST]", got)
		}
	})
}

// TestPinOutbound_BlankFilesNothing_AtAllThreeSites: no outbound anywhere means
// no move from any creator.
func TestPinOutbound_BlankFilesNothing_AtAllThreeSites(t *testing.T) {
	t.Parallel()

	t.Run("load_fallback", func(t *testing.T) {
		t.Parallel()
		eng, db, nodeID, core := scStoredWithCore(t, "OBBF", protocol.ClaimRoleProduce, "", "")
		c := newSCCore(t)
		c.set(core, true, "")
		eng.coreClient = NewCoreClient(c.srv.URL)
		testutil.MustNoErr(t, eng.LoadBin(nodeID, "PART-OB", nil, scManifest), "LoadBin")
		if got := scDestsFrom(t, db, core); len(got) != 0 {
			t.Errorf("L2 destinations = %v, want none", got)
		}
	})

	t.Run("unloader_empty_out", func(t *testing.T) {
		t.Parallel()
		eng, db, nodeID, core := scStoredWithCore(t, "OBBU", protocol.ClaimRoleConsume, "", "")
		c := newSCCore(t)
		c.set(core, true, "")
		eng.coreClient = NewCoreClient(c.srv.URL)
		testutil.MustNoErr(t, eng.PushEmptyOut(nodeID), "PushEmptyOut")
		if got := scDestsFrom(t, db, core); len(got) != 0 {
			t.Errorf("U2 destinations = %v, want none", got)
		}
	})

	t.Run("l1_completion", func(t *testing.T) {
		t.Parallel()
		eng, db, nodeID, core := scStoredWithCore(t, "OBBC", protocol.ClaimRoleProduce, "", "")
		eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
		eng.wireEventHandlers()
		c := newSCCore(t)
		c.set(core, true, "")
		eng.coreClient = NewCoreClient(c.srv.URL)
		scDeliveredRetrieve(t, db, "obbc-l1", orders.TypeRetrieve, nodeID, true, core)
		testutil.MustNoErr(t, eng.LoadBin(nodeID, "PART-OB", nil, scManifest), "LoadBin")
		if got := scDestsFrom(t, db, core); len(got) != 0 {
			t.Errorf("L2 destinations = %v, want none", got)
		}
	})
}

// TestPinOutbound_SameNodeFilesNothing_AtTheTwoGuardedSites: an outbound equal
// to the window's own node is refused by createUnloaderEmptyOut and
// applyLoaderEmptyIn. LoadBin's fallback has no such check — see
// TestFailingFirst_LoadFallback_RefusesASameNodeL2 (not a pin: red at base).
func TestPinOutbound_SameNodeFilesNothing_AtTheTwoGuardedSites(t *testing.T) {
	t.Parallel()

	t.Run("unloader_empty_out", func(t *testing.T) {
		t.Parallel()
		eng, db, nodeID, core := scStoredWithCore(t, "OBSU", protocol.ClaimRoleConsume, "OBSU-MSWAP-NODE", "")
		c := newSCCore(t)
		c.set(core, true, "")
		eng.coreClient = NewCoreClient(c.srv.URL)
		testutil.MustNoErr(t, eng.PushEmptyOut(nodeID), "PushEmptyOut")
		if got := scDestsFrom(t, db, core); len(got) != 0 {
			t.Errorf("U2 destinations = %v, want none (same-node)", got)
		}
	})

	t.Run("l1_completion", func(t *testing.T) {
		t.Parallel()
		eng, db, nodeID, core := scStoredWithCore(t, "OBSC", protocol.ClaimRoleProduce, "OBSC-MSWAP-NODE", "")
		eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
		eng.wireEventHandlers()
		c := newSCCore(t)
		c.set(core, true, "")
		eng.coreClient = NewCoreClient(c.srv.URL)
		scDeliveredRetrieve(t, db, "obsc-l1", orders.TypeRetrieve, nodeID, true, core)
		testutil.MustNoErr(t, eng.LoadBin(nodeID, "PART-OB", nil, scManifest), "LoadBin")
		if got := scDestsFrom(t, db, core); len(got) != 0 {
			t.Errorf("L2 destinations = %v, want none (same-node)", got)
		}
	})
}
