package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// order_intent_restart_test.go — the pull-from-market intent survives an Edge
// restart.
//
// A pullback arms a FUTURE action when it dispatches its order: it auto-clears
// the bin when the order delivers to the loader window. The arming used to
// live only in a map on the Engine, so an Edge restart between arming and
// firing lost it: the pulled-back bin arrived uncleared. "Restart" here is the
// real thing: the DB file is closed and reopened and a new Engine is built
// over it, so nothing in memory carries across.

// intentCoreServer fakes the Core telemetry endpoints the pullback calls and
// counts bin-clear requests.
func intentCoreServer(t *testing.T, bins []protocol.NodeBinInfo) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var clears atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/telemetry/node-bins":
			json.NewEncoder(w).Encode(bins)
		case r.URL.Path == "/api/telemetry/bin-clear":
			clears.Add(1)
			json.NewEncoder(w).Encode(protocol.BinClearResponse{BinID: 77, Status: "ok"})
		default:
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &clears
}

// openIntentDB opens (or reopens) the engine test DB at path.
func openIntentDB(t *testing.T, path string) *store.DB {
	t.Helper()
	db, err := store.OpenMigrated(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	return db
}

func seedIntentNode(t *testing.T, db *store.DB, proc, coreNode string) int64 {
	t.Helper()
	procID, err := db.CreateProcess(proc, "", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: coreNode, Code: coreNode, Name: coreNode, Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create process node")
	return nodeID
}

// newIntentEngine is testEngine plus what engine.New builds for these features.
func newIntentEngine(t *testing.T, db *store.DB) *Engine {
	t.Helper()
	eng := testEngine(t, db)
	return eng
}

func pullbackLoader(window string) protocol.LoaderInfo {
	return protocol.LoaderInfo{
		Name: window, LoaderKey: "loader:" + window, Role: string(protocol.ClaimRoleProduce),
		Layout: "shared_window", Replenishment: "operator", InboundSource: "EMPTY-SUPER",
		OutboundDest: "PB-MARKET",
		Positions:    []protocol.LoaderPosition{{CoreNodeName: window}},
		Payloads:     []protocol.LoaderPayloadInfo{{PayloadCode: "PB-PART"}},
	}
}

// armPullback runs PullFromMarket on a fresh DB file and returns everything a
// test needs to deliver the order, on the same Engine or after a restart.
type pullbackFixture struct {
	path   string
	db     *store.DB
	eng    *Engine
	nodeID int64
	order  *storeorders.Order
	srv    *httptest.Server
	clears *atomic.Int32
}

func armPullback(t *testing.T) *pullbackFixture {
	t.Helper()
	f := &pullbackFixture{path: copyEngineDBTemplate(t)}
	f.srv, f.clears = intentCoreServer(t,
		[]protocol.NodeBinInfo{{NodeName: "PB-MKT-1", BinID: 88, Occupied: true, PayloadCode: "PB-PART", UOPRemaining: 40}})

	f.db = openIntentDB(t, f.path)
	f.nodeID = seedIntentNode(t, f.db, "PB-PROC", "PB-WIN")
	f.eng = newIntentEngine(t, f.db)
	f.eng.coreClient = stubCoreClient(f.srv.URL)
	f.eng.SetCoreLoaders([]protocol.LoaderInfo{pullbackLoader("PB-WIN")})
	// The market's members, as Core answers a watched group on the bins feed,
	// and the bin on its slot: the pull's source check reads the table.
	holdActableBins(t, f.eng,
		protocol.NodeBinsRow{Node: "PB-MARKET", State: protocol.BinsStateGroup, Members: []string{"PB-MKT-1"}},
		protocol.NodeBinsRow{Node: "PB-MKT-1", BinID: 88, Occupied: true, PayloadCode: "PB-PART", Stocked: true})

	testutil.MustNoErr(t, f.eng.PullFromMarket(f.nodeID, "PB-MKT-1"), "PullFromMarket")
	all, err := f.db.ListActiveOrdersByProcessNode(f.nodeID)
	testutil.MustNoErr(t, err, "list orders")
	if len(all) != 1 || all[0].DeliveryNode != "PB-WIN" || all[0].SourceNode != "PB-MKT-1" {
		t.Fatalf("fixture: want exactly one pullback move PB-MKT-1 -> PB-WIN, got %+v", all)
	}
	f.order = &all[0]
	return f
}

func deliverTo(eng *Engine, order *storeorders.Order, nodeID int64) {
	bid, uop := int64(77), 40
	eng.handleNodeOrderDelivered(OrderDeliveredEvent{
		OrderID: order.ID, OrderUUID: order.UUID, OrderType: order.OrderType,
		ProcessNodeID: &nodeID, BinID: &bid, BinUOP: &uop,
	})
}

// Control: without a restart the pullback auto-clears exactly once, and a
// replayed delivery does not clear again. The feature works in-process today
// and must keep working.
func TestPullbackIntentFiresOnceWithoutRestart(t *testing.T) {
	t.Parallel()
	f := armPullback(t)
	defer f.db.Close()
	deliverTo(f.eng, f.order, f.nodeID)
	deliverTo(f.eng, f.order, f.nodeID)
	if got := f.clears.Load(); got != 1 {
		t.Fatalf("bin-clear calls after the pullback delivered (twice, same Engine) = %d, want exactly 1", got)
	}
}

// The lane's reason, pullback half: arm, restart, deliver -> auto-clear fired.
func TestPullbackIntentSurvivesRestart(t *testing.T) {
	t.Parallel()
	f := armPullback(t)
	// Restart: the file is closed and reopened, and a new Engine comes up over it.
	f.db.Close()

	db := openIntentDB(t, f.path)
	defer db.Close()
	eng := newIntentEngine(t, db)
	eng.coreClient = stubCoreClient(f.srv.URL)
	eng.SetCoreLoaders([]protocol.LoaderInfo{pullbackLoader("PB-WIN")})

	deliverTo(eng, f.order, f.nodeID)
	if got := f.clears.Load(); got != 1 {
		t.Fatalf("bin-clear calls after the restarted Edge saw the pullback deliver = %d, want 1: "+
			"the pull-from-market intent did not survive the restart, so the bin arrived at the "+
			"window with its count still on it", got)
	}
}

// Unknown-kind decode: a row carrying a kind this build does not know (or a
// malformed envelope) is logged and dropped at the firing site, and the
// delivery path around it still runs — the bin still binds to the node. The
// retired "consolidation" kind, which an older build armed on a loader home's
// clear, is one of them.
func TestTakeIntent_UnknownKindDropsWithoutBreakingDelivery(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{"kind":"horseshoe"}`, `not json`,
		`{"kind":"consolidation","buffer_core_name":"BUF","home_core_name":"HOME","home_process_node_id":9,"payload":"P"}`} {
		f := armPullback(t)
		testutil.MustNoErr(t, f.db.SetOrderPendingIntent(f.order.ID, raw), "plant intent")

		deliverTo(f.eng, f.order, f.nodeID)

		if got := f.clears.Load(); got != 0 {
			t.Errorf("%q: bin-clear calls = %d, want 0 (an unknown intent never fires)", raw, got)
		}
		if got, _ := f.db.GetOrderPendingIntent(f.order.ID); got != "" {
			t.Errorf("%q: pending_intent after delivery = %q, want dropped (\"\")", raw, got)
		}
		rt, err := f.db.GetProcessNodeRuntime(f.nodeID)
		testutil.MustNoErr(t, err, "runtime")
		if rt.ActiveBinID == nil || *rt.ActiveBinID != 77 {
			t.Errorf("%q: ActiveBinID = %v, want 77 — the delivery path must run past a bad intent", raw, rt.ActiveBinID)
		}
		f.db.Close()
	}
}

// Cost record (not a budget assertion): microseconds per arming write, and per
// fire-site read+take, on the engine test DB. Run with -v to read the numbers.
func TestArmIntentCost(t *testing.T) {
	f := armPullback(t)
	defer f.db.Close()
	const n = 500
	it := orderIntent{Kind: intentPullback}
	start := time.Now()
	for i := range n {
		// Alternate the node so every write changes the row: rewriting the
		// same bytes dirties no page and would under-report the commit.
		it.NodeID = int64(i%2 + 1)
		f.eng.armOrderIntent(f.order.ID, it)
	}
	arm := time.Since(start)
	start = time.Now()
	for range n {
		f.eng.armOrderIntent(f.order.ID, it)
		f.eng.takeOrderIntent(f.order.ID, intentPullback)
	}
	armTake := time.Since(start)
	start = time.Now()
	for range n {
		f.eng.takeOrderIntent(f.order.ID, intentPullback) // nothing armed: the per-event read
	}
	miss := time.Since(start)
	t.Logf("COST arm write: %.1f µs/op; arm+take: %.1f µs/op; fire-site read with nothing armed: %.1f µs/op (n=%d)",
		float64(arm.Microseconds())/n, float64(armTake.Microseconds())/n, float64(miss.Microseconds())/n, n)
}

// TestPullFromMarket_TheMoveNamesTheBin: the pull-back's move names the bin its
// read of the slot found, so Core refuses it should that bin have left the
// slot, and lifts it and no other.
func TestPullFromMarket_TheMoveNamesTheBin(t *testing.T) {
	t.Parallel()
	f := armPullback(t)
	defer f.db.Close()
	if moves := emptyOutMoves(t, f.eng, "PB-MKT-1"); len(moves) != 1 || moves[0].BinID != 88 {
		t.Errorf("pull-back move requests = %+v, want one naming bin 88", moves)
	}
}
