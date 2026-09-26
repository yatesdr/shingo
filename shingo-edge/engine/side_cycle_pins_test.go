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
// Written as characterisation pins at c0c525c0. The D3 pins asserted the defect
// there (an L1 that never confirms, an L1 confirm that files no L2, a landing
// that neither clears nor re-pushes) and were flipped by the D3 fix; each says
// what it asserted before.
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
	mu       sync.Mutex
	windows  map[string]*scWindow
	srv      *httptest.Server
	binReads int
}

// nodeBinReads is how many node-bins requests Core has served so far.
func (c *scCore) nodeBinReads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.binReads
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
		c.binReads++
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
		win := c.windows[req["node_name"]]
		if win == nil || !win.occupied {
			// Core refuses to clear a node that holds no bin.
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no bin at node"})
			return
		}
		cleared := win.payload
		win.payload = ""
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "bin_id": 77, "delta_epoch": 4, "cleared_payload_code": cleared})
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

	if _, _, claim, err := eng.loadActiveNode(nodeID); err != nil || claim == nil || claim.ID != 0 {
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

// TestPinD3a_LoadAfterEcho_ConfirmsTheL1AndFilesOneL2 is D3 in the shape the
// plants run: the Edge creates the L1, Core's projection echoes it back as
// retrieve_empty, it is delivered, and the operator LOADs at a Core-owned window.
//
// LOAD confirms the L1; the completion chain files exactly one L2 carrying the
// part LOAD held (from the runtime row, no Core re-read); when the L2 lands the
// runtime pointer clears and the loader is re-pushed an empty.
//
// BEFORE D3 (pinned at c0c525c0 as ..._TakesTheFallbackAndStrandsTheL1): the
// lookup filtered order_type='retrieve', so the echoed L1 was invisible. LOAD
// took the fallback, the L1 stayed `delivered` for good, and the next push after
// the L2 landed computed to_fire=0 because the stranded L1 counted as in flight.
func TestPinD3a_LoadAfterEcho_ConfirmsTheL1AndFilesOneL2(t *testing.T) {
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
	readsBefore := core.nodeBinReads()
	testutil.MustNoErr(t, eng.LoadBin(nodeID, "", nil, scManifest), "LoadBin")
	if n := core.nodeBinReads() - readsBefore; n != 1 {
		t.Errorf("node-bins reads during LOAD = %d, want 1 (the occupancy check) — the L2's part "+
			"comes from LOAD's hand, not a second read", n)
	}

	after, err := db.GetOrder(l1.ID)
	testutil.MustNoErr(t, err, "reload L1 after LOAD")
	if after.Status != orders.StatusConfirmed {
		t.Fatalf("L1 status after LOAD = %q, want confirmed (the echoed type is found)", after.Status)
	}
	moves := scMovesFrom(t, db, window)
	if len(moves) != 1 {
		t.Fatalf("L2s leaving %s = %d, want exactly 1 (from L1-completion)", window, len(moves))
	}
	if moves[0].DeliveryNode != "FG-MARKET" || moves[0].PayloadCode != "PART-SC" {
		t.Errorf("L2 = to %q payload %q, want FG-MARKET / PART-SC (the part LOAD held)",
			moves[0].DeliveryNode, moves[0].PayloadCode)
	}

	core.set(window, false, "") // the bin leaves
	scLand(t, eng, db, moves[0])
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.ActiveOrderID != nil {
		t.Errorf("runtime active order after the L2 landed = %d, want cleared (applyManualSwap)", *rt.ActiveOrderID)
	}
	active, err := db.ListActiveOrdersByDeliveryNodeSet([]string{window})
	testutil.MustNoErr(t, err, "list active")
	fresh := 0
	for _, o := range active {
		if o.RetrieveEmpty && o.ID != l1.ID {
			fresh++
		}
	}
	if fresh != 1 {
		t.Errorf("new empties staged by the L2 landing = %d, want 1 (the produce re-push)", fresh)
	}
	decisions := logs.matching("loader_budget loader=loader:" + window)
	if len(decisions) == 0 || !strings.Contains(decisions[len(decisions)-1], "to_fire=1") {
		t.Errorf("landing push decisions = %q, want the last to fire 1 — nothing stranded holds the window", decisions)
	}
}

// ── D3b: the confirmed branch at a Core-owned loader ─────────────────────

// TestPinD3b_ConfirmedL1AtACoreOwnedLoader_FilesOneL2: LOAD's confirmed branch
// at a loader with no stored claim. The completion ctx resolves the claim through
// claimAtNode (stored, else SynthClaim), so loader_empty_in matches and files
// exactly one L2 with the loaded part.
//
// BEFORE D3 (pinned at c0c525c0 as ..._CreatesNoL2): the ctx read stored claims
// only, IsLoaderNode() on nil was false, and the confirmed branch filed NO L2 —
// the loaded bin would have sat on the window.
func TestPinD3b_ConfirmedL1AtACoreOwnedLoader_FilesOneL2(t *testing.T) {
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
		t.Fatalf("L1 status = %q, want confirmed", after.Status)
	}
	if after.FinalCount == nil || *after.FinalCount != 40 {
		t.Errorf("L1 final count = %v, want 40 (Core's LoadBin answer, not the argument)", after.FinalCount)
	}
	moves := scMovesFrom(t, db, window)
	if len(moves) != 1 || moves[0].PayloadCode != "PART-SC" {
		t.Errorf("L2s leaving %s = %+v, want exactly one carrying PART-SC", window, moves)
	}
}

// ── an L2 landing ────────────────────────────────────────────────────────

// TestPinCoreOwnedLoader_L2LandingClearsAndRePushes: an L2 landing at a
// Core-owned loader runs applyManualSwap — the runtime order pointer is cleared
// and the loader is re-pushed one empty. The control push afterwards finds that
// empty in flight and fires nothing.
//
// BEFORE D3 (pinned at c0c525c0 as ..._NeitherClearsNorRePushes): matchManualSwap
// read stored claims only, so the pointer kept naming the landed L2 and nothing
// was staged until something else pushed.
func TestPinCoreOwnedLoader_L2LandingClearsAndRePushes(t *testing.T) {
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
	if rt.ActiveOrderID != nil {
		t.Errorf("runtime active order after the L2 landed = %d, want cleared", *rt.ActiveOrderID)
	}
	if n := scActiveEmptiesTo(t, db, window); n != 1 {
		t.Errorf("empties staged by the landing = %d, want 1 (the produce re-push)", n)
	}

	eng.SweepPushLoaders()
	if n := scActiveEmptiesTo(t, db, window); n != 1 {
		t.Errorf("control: a push sweep after the re-push left %d empties, want still 1", n)
	}
}

// TestLanding_RePushesOnlyItsOwnLoader: an L2 landing re-pushes the loader that
// owns the node and no other, at a cost of ONE Core occupancy read — for a
// Core-owned loader and for one still holding a stored claim alike. The push
// used to walk every operator-staged produce loader (the push that preceded
// rePushOwnLoader), one read
// each, for a landing that frees exactly one window.
func TestLanding_RePushesOnlyItsOwnLoader(t *testing.T) {
	t.Parallel()
	for _, stored := range []bool{false, true} {
		name := "core_owned"
		if stored {
			name = "stored_claim"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			eng := testEngine(t, db)
			eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
			eng.wireEventHandlers()
			core := newSCCore(t)
			eng.coreClient = NewCoreClient(core.srv.URL)

			prefix := "RP-" + name
			own := prefix + "-MSWAP-NODE"
			other := prefix + "-OTHER"
			seedCoreLoader(t, eng,
				sharedLoaderInfo(own, "produce", "operator", "PART-RP", 0, 0),
				sharedLoaderInfo(other, "produce", "operator", "PART-RP", 0, 0))
			var nodeID int64
			if stored {
				nodeID, _ = seedManualSwapClaim(t, db, prefix, protocol.ClaimRoleProduce, "PART-RP", "FG-MARKET")
			} else {
				procID, err := db.CreateProcess(prefix+"-PROC", "", "active_production", "", "", false)
				testutil.MustNoErr(t, err, "create process")
				nodeID, err = db.CreateProcessNode(processes.NodeInput{
					ProcessID: procID, CoreNodeName: own, Code: "RP", Name: own, Sequence: 1, Enabled: true,
				})
				testutil.MustNoErr(t, err, "create node")
				_, err = db.EnsureProcessNodeRuntime(nodeID)
				testutil.MustNoErr(t, err, "ensure runtime")
			}
			// The other loader's window: a process node here, free, nothing in
			// flight — a push that walked every loader would stage an empty there.
			otherProc, err := db.CreateProcess(prefix+"-OTHER-PROC", "", "active_production", "", "", false)
			testutil.MustNoErr(t, err, "create other process")
			_, err = db.CreateProcessNode(processes.NodeInput{
				ProcessID: otherProc, CoreNodeName: other, Code: "RO", Name: other, Sequence: 1, Enabled: true,
			})
			testutil.MustNoErr(t, err, "create other node")
			if _, _, claim, err := eng.loadActiveNode(nodeID); err != nil || claim == nil || (claim.ID != 0) != stored {
				t.Fatalf("fixture: claim at %s = %+v, want stored=%v", own, claim, stored)
			}
			core.set(own, true, "")
			core.set(other, false, "")

			testutil.MustNoErr(t, eng.LoadBin(nodeID, "PART-RP", nil, scManifest), "LoadBin")
			moves := scMovesFrom(t, db, own)
			if len(moves) != 1 {
				t.Fatalf("L2s leaving %s = %d, want 1", own, len(moves))
			}
			core.set(own, false, "")
			readsBefore := core.nodeBinReads()
			scLand(t, eng, db, moves[0])

			if n := scActiveEmptiesTo(t, db, own); n != 1 {
				t.Errorf("empties staged at the landing loader = %d, want 1 (its own landing re-pushes it)", n)
			}
			if n := scActiveEmptiesTo(t, db, other); n != 0 {
				t.Errorf("empties staged at the OTHER loader = %d, want 0 (a landing frees only its own window)", n)
			}
			if n := core.nodeBinReads() - readsBefore; n != 1 {
				t.Errorf("node-bins reads on the landing = %d, want 1", n)
			}
		})
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
	got, ok := eng.confirmDeliveredAt(core, true, 55)
	if !ok || got != oldest {
		t.Fatalf("confirmDeliveredAt(L1) = (%d, %v), want (%d, true): the oldest delivered empty-in "+
			"at the core node, even though a sibling process_node tracks it", got, ok, oldest)
	}
	o, err := db.GetOrder(oldest)
	testutil.MustNoErr(t, err, "get order")
	if o.Status != orders.StatusConfirmed || o.FinalCount == nil || *o.FinalCount != 55 {
		t.Errorf("confirmed L1 = status %q final %v, want confirmed with 55", o.Status, o.FinalCount)
	}
	for _, id := range []int64{u1, newer} {
		if o, err := db.GetOrder(id); err != nil {
			t.Fatalf("get order %d: %v", id, err)
		} else if o.Status != orders.StatusDelivered {
			t.Errorf("order %d status = %q, want delivered (one confirm per tap, never the U1)", id, o.Status)
		}
	}
}

// TestPinConfirmL1_FindsTheEchoedTypeSpelling is D3a at the store query: a
// delivered empty-in whose stored type is `retrieve_empty` (what the echo writes)
// is found and confirmed. Before D3a the lookup filtered order_type='retrieve'
// and missed it.
func TestPinConfirmL1_FindsTheEchoedTypeSpelling(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, _ := seedManualSwapClaim(t, db, "CL1-ECHO", "produce", "PART-CE", "FG-MARKET")
	const core = "CL1-ECHO-MSWAP-NODE"
	id := scDeliveredRetrieve(t, db, "cl1-echo", protocol.OrderTypeRetrieveEmpty, nodeID, true, core)

	eng := testEngine(t, db)
	if got, ok := eng.confirmDeliveredAt(core, true, 10); !ok || got != id {
		t.Errorf("confirmDeliveredAt(L1) = (%d, %v), want (%d, true) — the echo spelled the type "+
			"retrieve_empty and the flag says empty-in", got, ok, id)
	}
	if o, err := db.GetOrder(id); err != nil {
		t.Fatalf("get order %d: %v", id, err)
	} else if o.Status != orders.StatusConfirmed {
		t.Errorf("echoed L1 status = %q, want confirmed", o.Status)
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
	if got, ok := eng.confirmDeliveredAt(core, false, 0); !ok || got != id {
		t.Fatalf("confirmDeliveredAt(U1) = (%d, %v), want (%d, true)", got, ok, id)
	}
	o, err := db.GetOrder(id)
	testutil.MustNoErr(t, err, "get order")
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
	if _, _, claim, err := eng.loadActiveNode(nodeID); err != nil || claim == nil || claim.ID == 0 {
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
// no move from any creator. PUSH EMPTY, whose whole operation is the move, is
// refused rather than reported done.
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
		// PUSH EMPTY with nowhere to push is refused, not reported done: the
		// empty-out IS the operation, and nothing was committed.
		if err := eng.PushEmptyOut(nodeID); err == nil {
			t.Error("PushEmptyOut with no outbound returned nil, want a refusal")
		}
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
// applyLoaderEmptyIn, as it was at c0c525c0. The third site is
// TestLoadFallback_RefusesASameNodeL2.
func TestPinOutbound_SameNodeFilesNothing_AtTheTwoGuardedSites(t *testing.T) {
	t.Parallel()

	t.Run("unloader_empty_out", func(t *testing.T) {
		t.Parallel()
		eng, db, nodeID, core := scStoredWithCore(t, "OBSU", protocol.ClaimRoleConsume, "OBSU-MSWAP-NODE", "")
		c := newSCCore(t)
		c.set(core, true, "")
		eng.coreClient = NewCoreClient(c.srv.URL)
		if err := eng.PushEmptyOut(nodeID); err == nil {
			t.Error("PushEmptyOut with a same-node outbound returned nil, want a refusal")
		}
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

// TestLoadFallback_RefusesASameNodeL2: LoadBin's fallback refuses an outbound
// that resolves to the window's own name, like the other two creators. At
// c0c525c0 it filed a move from the window to itself (flat bug).
func TestLoadFallback_RefusesASameNodeL2(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, core := scStoredWithCore(t, "OBSF", protocol.ClaimRoleProduce, "OBSF-MSWAP-NODE", "")
	c := newSCCore(t)
	c.set(core, true, "")
	eng.coreClient = NewCoreClient(c.srv.URL)
	testutil.MustNoErr(t, eng.LoadBin(nodeID, "PART-OB", nil, scManifest), "LoadBin")
	if got := scDestsFrom(t, db, core); len(got) != 0 {
		t.Errorf("L2 destinations = %v, want none — a same-node move is refused at every creator", got)
	}
}
