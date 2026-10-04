package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// order_intent_restart_test.go — the pull-from-market and clear-loader-home
// intents survive an Edge restart.
//
// Both features arm a FUTURE action when they dispatch their first order: a
// pullback auto-clears the bin when its order delivers to the loader window; a
// home consolidation fires Order B (buffer partial -> home) when Order A's robot
// lifts the empty off the home. The arming used to live only in two maps on the
// Engine, so an Edge restart between arming and firing lost it: the pulled-back
// bin arrived uncleared, and the home was emptied with no partial ever brought
// in. "Restart" here is the real thing: the DB file is closed and reopened and a
// new Engine is built over it, so nothing in memory carries across.

// intentCoreServer fakes the Core telemetry endpoints both features call and
// counts bin-clear requests.
func intentCoreServer(t *testing.T, bins []protocol.NodeBinInfo, children []protocol.NodeChildInfo) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var clears atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/telemetry/node-bins":
			json.NewEncoder(w).Encode(bins)
		case strings.HasSuffix(r.URL.Path, "/children"):
			json.NewEncoder(w).Encode(children)
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
	procID, err := db.CreateProcess(proc, "", "active_production", "", "", false)
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
		OutboundDest: "PB-MARKET", ConfigGen: 1,
		Positions: []protocol.LoaderPosition{{CoreNodeName: window, Kind: "window"}},
		Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PB-PART"}},
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
		[]protocol.NodeBinInfo{{NodeName: "PB-MKT-1", Occupied: true, PayloadCode: "PB-PART", UOPRemaining: 40}},
		[]protocol.NodeChildInfo{{Name: "PB-MKT-1"}})

	f.db = openIntentDB(t, f.path)
	f.nodeID = seedIntentNode(t, f.db, "PB-PROC", "PB-WIN")
	f.eng = newIntentEngine(t, f.db)
	f.eng.coreClient = stubCoreClient(f.srv.URL)
	f.eng.SetCoreLoaders([]protocol.LoaderInfo{pullbackLoader("PB-WIN")})

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

func consolidationLoader() protocol.LoaderInfo {
	return protocol.LoaderInfo{
		Name: "LDR-CLH", LoaderKey: "loader:clh", Role: string(protocol.ClaimRoleProduce),
		Layout: "dedicated_positions", Replenishment: "operator", ConfigGen: 1,
		Positions: []protocol.LoaderPosition{
			{CoreNodeName: "CLH-HOME", PayloadCode: "CLH-PART", Kind: "position", HomeKind: "home"},
			{CoreNodeName: "CLH-BUF", Kind: "position", HomeKind: "buffer"},
		},
		Payloads: []protocol.LoaderPayloadInfo{{PayloadCode: "CLH-PART"}},
	}
}

// countMovesFrom counts active move orders at nodeID with the given source.
func countMovesFrom(t *testing.T, db *store.DB, nodeID int64, source, dest string) int {
	t.Helper()
	all, err := db.ListActiveOrdersByProcessNode(nodeID)
	testutil.MustNoErr(t, err, "list orders")
	n := 0
	for _, o := range all {
		if o.OrderType == orders.TypeMove && o.SourceNode == source && o.DeliveryNode == dest {
			n++
		}
	}
	return n
}

// The lane's reason, consolidation half: arm (ClearLoaderHome creates Order A),
// restart, Order A's carrier is picked up at the home -> Order B fires.
func TestConsolidationIntentSurvivesRestart(t *testing.T) {
	t.Parallel()
	path := copyEngineDBTemplate(t)
	srv, _ := intentCoreServer(t, []protocol.NodeBinInfo{
		{NodeName: "CLH-HOME", Occupied: true, PayloadCode: "CLH-PART", UOPRemaining: 5, BinID: 11},
		{NodeName: "CLH-BUF", Occupied: true, PayloadCode: "CLH-PART", UOPRemaining: 3, BinID: 12},
	}, nil)

	db := openIntentDB(t, path)
	nodeID := seedIntentNode(t, db, "CLH-PROC", "CLH-HOME")
	eng := newIntentEngine(t, db)
	eng.coreClient = stubCoreClient(srv.URL)
	eng.SetCoreLoaders([]protocol.LoaderInfo{consolidationLoader()})

	testutil.MustNoErr(t, eng.ClearLoaderHome(nodeID), "ClearLoaderHome")
	var orderA *storeorders.Order
	all, err := db.ListActiveOrdersByProcessNode(nodeID)
	testutil.MustNoErr(t, err, "list orders")
	for i := range all {
		if all[i].SourceNode == "CLH-HOME" && all[i].DeliveryNode == "CLH-BUF" {
			orderA = &all[i]
		}
	}
	if orderA == nil || len(all) != 1 {
		t.Fatalf("fixture: want exactly Order A (home->buffer), got %d orders", len(all))
	}
	db.Close()

	// Restart.
	db2 := openIntentDB(t, path)
	defer db2.Close()
	eng2 := newIntentEngine(t, db2)
	eng2.coreClient = stubCoreClient(srv.URL)
	eng2.SetCoreLoaders([]protocol.LoaderInfo{consolidationLoader()})

	eng2.HandleBinPickedUp(orderA.UUID, 11, "CLH-HOME")
	if n := countMovesFrom(t, db2, nodeID, "CLH-BUF", "CLH-HOME"); n != 1 {
		t.Fatalf("Order B (buffer->home) after the restarted Edge saw Order A's pickup = %d, want 1: "+
			"the consolidation intent did not survive the restart, so the home was emptied and "+
			"the partial never brought in", n)
	}
	// A replayed pickup (Core re-fires FINISHED blocks) must not fire a second Order B.
	eng2.HandleBinPickedUp(orderA.UUID, 11, "CLH-HOME")
	if n := countMovesFrom(t, db2, nodeID, "CLH-BUF", "CLH-HOME"); n != 1 {
		t.Fatalf("Order B count after a replayed pickup = %d, want still 1", n)
	}
}

// Control for the consolidation pin: on ONE Engine, Order A's pickup fires
// Order B once, and a replayed pickup does not fire a second. Proves the
// restart pin's fixture reaches the firing site, so its red is the restart.
func TestConsolidationIntentFiresOnceWithoutRestart(t *testing.T) {
	t.Parallel()
	srv, _ := intentCoreServer(t, []protocol.NodeBinInfo{
		{NodeName: "CLH-HOME", Occupied: true, PayloadCode: "CLH-PART", UOPRemaining: 5, BinID: 11},
		{NodeName: "CLH-BUF", Occupied: true, PayloadCode: "CLH-PART", UOPRemaining: 3, BinID: 12},
	}, nil)
	db := openIntentDB(t, copyEngineDBTemplate(t))
	defer db.Close()
	nodeID := seedIntentNode(t, db, "CLH-PROC", "CLH-HOME")
	eng := newIntentEngine(t, db)
	eng.coreClient = stubCoreClient(srv.URL)
	eng.SetCoreLoaders([]protocol.LoaderInfo{consolidationLoader()})

	testutil.MustNoErr(t, eng.ClearLoaderHome(nodeID), "ClearLoaderHome")
	all, err := db.ListActiveOrdersByProcessNode(nodeID)
	testutil.MustNoErr(t, err, "list orders")
	if len(all) != 1 {
		t.Fatalf("fixture: want exactly Order A, got %d orders", len(all))
	}
	eng.HandleBinPickedUp(all[0].UUID, 11, "CLH-HOME")
	eng.HandleBinPickedUp(all[0].UUID, 11, "CLH-HOME")
	if n := countMovesFrom(t, db, nodeID, "CLH-BUF", "CLH-HOME"); n != 1 {
		t.Fatalf("Order B after Order A's pickup (twice, same Engine) = %d, want exactly 1", n)
	}
}

// Unknown-kind decode: a row carrying a kind this build does not know (or a
// malformed envelope) is logged and dropped at the firing site, and the
// delivery path around it still runs — the bin still binds to the node.
func TestTakeIntent_UnknownKindDropsWithoutBreakingDelivery(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{"kind":"horseshoe"}`, `not json`} {
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

// A take at the other kind's site leaves the intent for its own site.
func TestTakeIntent_OtherKindIsLeftInPlace(t *testing.T) {
	t.Parallel()
	f := armPullback(t)
	defer f.db.Close()
	if _, ok := f.eng.takeOrderIntent(f.order.ID, intentConsolidation); ok {
		t.Fatal("a consolidation take consumed a pullback intent")
	}
	if it, ok := f.eng.takeOrderIntent(f.order.ID, intentPullback); !ok || it.NodeID != f.nodeID {
		t.Fatalf("pullback take after the wrong-kind take = (%+v, %v), want the armed intent", it, ok)
	}
}

// Cost record (not a budget assertion): microseconds per arming write, and per
// fire-site read+take, on the engine test DB. Run with -v to read the numbers.
func TestArmIntentCost(t *testing.T) {
	f := armPullback(t)
	defer f.db.Close()
	const n = 500
	it := orderIntent{Kind: intentConsolidation, BufferCoreName: "BUF", HomeCoreName: "HOME", HomeProcessNodeID: 9, Payload: "P"}
	start := time.Now()
	for i := range n {
		// Alternate the payload so every write changes the row: rewriting the
		// same bytes dirties no page and would under-report the commit.
		it.Payload = fmt.Sprintf("P%d", i%2)
		f.eng.armOrderIntent(f.order.ID, it)
	}
	arm := time.Since(start)
	start = time.Now()
	for range n {
		f.eng.armOrderIntent(f.order.ID, it)
		f.eng.takeOrderIntent(f.order.ID, intentConsolidation)
	}
	armTake := time.Since(start)
	start = time.Now()
	for range n {
		f.eng.takeOrderIntent(f.order.ID, intentConsolidation) // nothing armed: the per-event read
	}
	miss := time.Since(start)
	t.Logf("COST arm write: %.1f µs/op; arm+take: %.1f µs/op; fire-site read with nothing armed: %.1f µs/op (n=%d)",
		float64(arm.Microseconds())/n, float64(armTake.Microseconds())/n, float64(miss.Microseconds())/n, n)
}
