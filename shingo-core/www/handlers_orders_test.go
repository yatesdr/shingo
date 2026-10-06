//go:build docker

package www

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/debuglog"
	"shingo/protocol/testutil"
	"shingocore/config"
	"shingocore/engine"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// Characterization tests for handlers_orders.go — pinned before the Stage 1
// refactor that replaces h.engine.DB() with named query methods. These tests
// cover the two write-path contracts most sensitive to reordering:
//
//   - apiSetOrderPriority: the fleet priority update MUST run before the DB
//     priority update when the order has a vendor_order_id. A fleet failure
//     leaves the DB priority unchanged. See handlers_orders.go:180.
//
//   - submitSpotRetrieveSpecific: the create→claim→dispatch→readback sequence
//     MUST roll back the bin claim on dispatch failure and leave the order in
//     the "failed" terminal state. See handlers_orders.go:429.

// testHandlersWithSim is testHandlers(t) parameterized by a caller-supplied
// simulator. Use simulator.WithCreateFailure() to inject dispatch failures for
// rollback characterization.
func testHandlersWithSim(t *testing.T, sim *simulator.SimulatorBackend) (*Handlers, *store.DB) {
	t.Helper()

	db := testdb.Open(t)

	cfg := config.Defaults()
	cfg.Messaging.StationID = "test-www"

	eng := engine.New(engine.Config{
		AppConfig: cfg,
		DB:        db,
		Fleet:     sim,
		MsgClient: nil,
		LogFunc:   t.Logf,
	})
	eng.Start()
	t.Cleanup(func() { eng.Stop() })

	hub := NewEventHub()
	hub.Start()
	t.Cleanup(func() { hub.Stop() })

	dbgLog, _ := debuglog.New(64, nil)

	h := &Handlers{
		engine:        eng,
		orchestration: eng,
		sessions:      newSessionStore("test-secret"),
		tmpls:         make(map[string]*template.Template),
		eventHub:      hub,
		debugLog:      dbgLog,
	}
	return h, db
}

// testHandlersForRendering builds the page handlers over an engine that is
// constructed but never Started, for tests that assert what the orders board
// RENDERS rather than what the plant does.
//
// Start() launches the fulfillment scanner (`go e.fulfillment.RunOnce()`,
// engine_lifecycle.go) and does not wait for it. A rendering test seeds order
// state by writing it straight to the database, so the two race: when the
// goroutine happens to land after the seeding, the scanner finds the seeded
// order in the acquiring set, re-derives its wait from a database that holds no
// bins at all, and rewrites queue_reason to "Waiting for material" via
// dispatch.WriteQueueDetail. That is the scanner behaving correctly — a wait's
// cause is meant to say why the order is waiting NOW — against a fixture that
// asserts a cause nothing in this database supports. It turned CI red on a
// loaded runner while passing 65 consecutive local runs.
//
// Awaiting the boot scan would only sequence the race. Not starting the engine
// removes it: with no Start there is no scanner, no periodic sweep and no event
// triggers, so nothing exists that can rewrite a seeded row. New wires
// orderService and nodeService (engine.go), which is everything the read path
// through handleOrders touches, so the page renders exactly as it does in
// production.
func testHandlersForRendering(t *testing.T) (*Handlers, *store.DB) {
	t.Helper()

	db := testdb.Open(t)

	cfg := config.Defaults()
	cfg.Messaging.StationID = "test-www"

	eng := engine.New(engine.Config{
		AppConfig: cfg,
		DB:        db,
		Fleet:     simulator.New(),
		MsgClient: nil,
		LogFunc:   t.Logf,
	})

	hub := NewEventHub()
	hub.Start()
	t.Cleanup(func() { hub.Stop() })

	dbgLog, _ := debuglog.New(64, nil)

	h := &Handlers{
		engine:        eng,
		orchestration: eng,
		sessions:      newSessionStore("test-secret"),
		tmpls:         make(map[string]*template.Template),
		eventHub:      hub,
		debugLog:      dbgLog,
	}
	return h, db
}

// makeOrder inserts a pending "move" order with the given vendor_order_id and
// priority. Returns the persisted order.
func makeOrder(t *testing.T, db *store.DB, uuid, vendorID string, priority int) *orders.Order {
	t.Helper()
	o := &orders.Order{
		EdgeUUID:   uuid,
		StationID:  "line-prio",
		OrderType:  "move",
		Status:     "pending",
		Quantity:   1,
		SourceNode: "STORAGE-A1",
		Priority:   priority,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")
	if vendorID != "" {
		testutil.MustNoErr(t, db.UpdateOrderVendor(o.ID, vendorID, "CREATED", ""), "update order vendor")
	}
	got, err := db.GetOrder(o.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	return got
}

// registerVendorOrder creates a CREATED order in the simulator so subsequent
// SetOrderPriority/CancelOrder calls find it. Returns the vendor id used.
func registerVendorOrder(t *testing.T, sim *simulator.SimulatorBackend, vendorID, fromLoc, toLoc string, priority int) {
	t.Helper()
	if _, err := sim.CreateOrder(fleet.CreateOrderRequest{
		OrderID: vendorID,
		Blocks: []fleet.OrderBlock{
			{BlockID: vendorID + "_load", Location: fromLoc, BinTask: "JackLoad"},
			{BlockID: vendorID + "_unload", Location: toLoc, BinTask: "JackUnload"},
		},
		Priority: priority,
		Complete: true,
	}); err != nil {
		t.Fatalf("register vendor order: %v", err)
	}
}

// --- apiSetOrderPriority ----------------------------------------------------

// TestApiSetOrderPriority_FleetThenDBHappyPath pins the fleet-then-DB order:
// when the order has a vendor_order_id the handler calls
// Fleet().SetOrderPriority first, then DB().UpdateOrderPriority. Both sides
// reflect the new priority when both calls succeed.
func TestApiSetOrderPriority_FleetThenDBHappyPath(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)

	order := makeOrder(t, db, "prio-happy-1", "vendor-prio-1", 1)
	registerVendorOrder(t, sim, order.VendorOrderID, "STORAGE-A1", "LINE1-IN", 1)

	rec := postJSON(t, h.apiSetOrderPriority, "/api/order/priority",
		map[string]any{"order_id": order.ID, "priority": 7})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	got, _ := db.GetOrder(order.ID)
	if got.Priority != 7 {
		t.Errorf("db priority: got %d, want 7", got.Priority)
	}
}

// TestApiSetOrderPriority_FleetFailureSkipsDBUpdate is the critical
// characterization: if the fleet call fails (500), the DB UpdateOrderPriority
// MUST NOT run. Reversing the order in a refactor would leak divergent
// priorities between fleet and DB.
func TestApiSetOrderPriority_FleetFailureSkipsDBUpdate(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)

	// Give the order a vendor_order_id that is NOT registered with the
	// simulator — the simulator's SetOrderPriority then returns
	// "order %s not found" and forces the handler's 500 path.
	order := makeOrder(t, db, "prio-fleetfail-1", "vendor-missing-1", 3)

	rec := postJSON(t, h.apiSetOrderPriority, "/api/order/priority",
		map[string]any{"order_id": order.ID, "priority": 9})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}

	got, _ := db.GetOrder(order.ID)
	if got.Priority != 3 {
		t.Errorf("db priority after fleet failure: got %d, want unchanged 3", got.Priority)
	}
}

// TestApiSetOrderPriority_NoVendorIDSkipsFleet pins the skip-fleet branch: an
// order without a vendor_order_id goes straight to the DB update without
// touching the fleet. This guards against an over-zealous refactor that calls
// fleet unconditionally.
func TestApiSetOrderPriority_NoVendorIDSkipsFleet(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)

	// No vendor id — the fleet branch should be skipped.
	order := makeOrder(t, db, "prio-novendor-1", "", 1)

	rec := postJSON(t, h.apiSetOrderPriority, "/api/order/priority",
		map[string]any{"order_id": order.ID, "priority": 5})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	got, _ := db.GetOrder(order.ID)
	if got.Priority != 5 {
		t.Errorf("db priority: got %d, want 5", got.Priority)
	}
}

func TestApiSetOrderPriority_OrderNotFound(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	rec := postJSON(t, h.apiSetOrderPriority, "/api/order/priority",
		map[string]any{"order_id": 9999999, "priority": 1})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// --- submitSpotRetrieveSpecific ---------------------------------------------

// retrieveSpecificResponse mirrors readBackManualOrder's JSON envelope.
type retrieveSpecificResponse struct {
	OrderID     int64  `json:"order_id"`
	Status      string `json:"status"`
	ErrorDetail string `json:"error_detail"`
	Error       string `json:"error"`
}

// submitRetrieveSpecific lives in handlers_bin_move_test.go. There were two
// copies of it in this package.

// TestSubmitSpotRetrieveSpecific_HappyPath pins the baseline: bin gets claimed
// by the new order, order advances to "dispatched" with a vendor_order_id.
func TestSubmitSpotRetrieveSpecific_HappyPath(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-RS-OK")

	resp, status := submitRetrieveSpecific(t, h, bin.Label, sd.LineNode.Name)
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200; err=%q", status, resp.Error)
	}
	if resp.OrderID == 0 {
		t.Fatalf("order_id missing in response: %+v", resp)
	}

	got, err := db.GetOrder(resp.OrderID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if got.Status != "dispatched" {
		t.Errorf("order status: got %q, want %q", got.Status, "dispatched")
	}
	if got.VendorOrderID == "" {
		t.Error("vendor_order_id should be set after successful dispatch")
	}

	testdb.RequireBinClaimedBy(t, db, bin.ID, got.ID)
}

// TestSubmitSpotRetrieveSpecific_DispatchFailureRollsBackClaim is the core
// rollback contract: CreateOrder succeeds, ClaimBin succeeds, DispatchDirect
// fails (fleet create injected), and then (a) the order MUST end up "failed"
// (via dispatcher's FailOrderAtomic) and (b) the bin MUST be unclaimed
// (belt-and-suspenders: dispatcher clears claims for the order in
// FailOrderAtomic, handler then calls UnclaimBin for the specific bin). A
// refactor that drops either half would leak a dangling claim.
func TestSubmitSpotRetrieveSpecific_DispatchFailureRollsBackClaim(t *testing.T) {
	t.Parallel()
	sim := simulator.New(simulator.WithCreateFailure())
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-RS-FAIL")

	resp, status := submitRetrieveSpecific(t, h, bin.Label, sd.LineNode.Name)
	// A dispatch failure is a failure. This used to answer 200 with
	// {status:"failed"} and an order id, on the reasoning that the body
	// reflected the outcome — but a 2xx now means an order exists and is on
	// its way, and after this rollback nothing is.
	if status != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want %d — the bin is not moving, so this is not a success; err=%q",
			status, http.StatusInternalServerError, resp.Error)
	}
	if resp.Error == "" {
		t.Error("rejection carried no message")
	}
	if resp.OrderID != 0 {
		t.Errorf("order_id = %d in a rejection body; a refused submission has no order to point at", resp.OrderID)
	}

	// The row still has to be found, and no longer through the response — the
	// rollback assertions below are the actual subject of this test.
	rows, err := db.ListOrdersByStation("core-operator", 50)
	if err != nil {
		t.Fatalf("list spot orders: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d spot order rows, want 1", len(rows))
	}
	got, err := db.GetOrder(rows[0].ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if got.Status != "failed" {
		t.Errorf("order status after dispatch failure: got %q, want %q", got.Status, "failed")
	}
	if got.VendorOrderID != "" {
		t.Errorf("vendor_order_id should be empty after failed dispatch, got %q", got.VendorOrderID)
	}

	// The critical rollback: bin claim released AND re-acquirable (the reservation
	// released too, not just claimed_by — else the confirmed row bricks the bin).
	testdb.RequireBinUnclaimed(t, db, bin.ID)
	probe := testdb.CreateOrder(t, db)
	if err := reservations.Acquire(db, probe.ID, probe.ID, bin.ID, "test"); err != nil {
		t.Errorf("bin not re-acquirable after spot-submit rollback: %v (reservation leaked?)", err)
	}
}

func TestSubmitSpotRetrieveSpecific_MissingBinLabel(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	resp, status := submitRetrieveSpecific(t, h, "", sd.LineNode.Name)
	if status != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; resp=%+v", status, resp)
	}
}

func TestSubmitSpotRetrieveSpecific_MissingDeliveryNode(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-RS-NODEST")

	resp, status := submitRetrieveSpecific(t, h, bin.Label, "")
	if status != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; resp=%+v", status, resp)
	}
}

func TestSubmitSpotRetrieveSpecific_UnknownBin(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	resp, status := submitRetrieveSpecific(t, h, "BIN-DOES-NOT-EXIST", sd.LineNode.Name)
	if status != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; resp=%+v", status, resp)
	}
}

func TestSubmitSpotRetrieveSpecific_UnknownDeliveryNode(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-RS-BADDEST")

	resp, status := submitRetrieveSpecific(t, h, bin.Label, "NO-SUCH-NODE")
	if status != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; resp=%+v", status, resp)
	}
}

// TestSubmitSpotRetrieveSpecific_BinAlreadyClaimed pins the claim-guard:
// attempting a retrieve_specific on a bin already claimed by another order
// returns 409 Conflict and performs zero writes.
func TestSubmitSpotRetrieveSpecific_BinAlreadyClaimed(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-RS-CLAIMED")

	// Stand up a prior order and have it claim the bin.
	prior := makeOrder(t, db, "prior-claim-1", "", 0)
	testdb.ClaimBinForTest(t, db, bin.ID, prior.ID)

	resp, status := submitRetrieveSpecific(t, h, bin.Label, sd.LineNode.Name)
	if status != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; resp=%+v", status, resp)
	}

	// Bin still claimed by the prior order — no overwrite.
	testdb.RequireBinClaimedBy(t, db, bin.ID, prior.ID)

	// No new order created for this spot submit (readBackManualOrder never ran).
	if resp.OrderID != 0 {
		t.Errorf("expected no new order on 409, got order_id=%d", resp.OrderID)
	}
}

// --- the orders board as an operator reads it (U4 pins) ---------------------
//
// Pinned at the tree before the table change, then moved to what the change
// predicts; each moved expectation names its brief item in the U4 notes.

// seedBoardOrder writes one order straight to the database with the fields the
// board renders, and backdates created_at to a fixed instant when one is given
// so the Created cell is checkable.
func seedBoardOrder(t *testing.T, db *store.DB, o *orders.Order, created string) *orders.Order {
	t.Helper()
	if o.Quantity == 0 {
		o.Quantity = 1
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create "+o.EdgeUUID)
	if created != "" {
		_, err := db.DB.Exec(`UPDATE orders SET created_at=$2 WHERE id=$1`, o.ID, created)
		testutil.MustNoErr(t, err, "backdate "+o.EdgeUUID)
	}
	return o
}

// boardRow returns the <tr> for one order id, or "" when the board has none.
func boardRow(body string, id int64) string {
	start := strings.Index(body, fmt.Sprintf(`<tr data-order-id="%d"`, id))
	if start < 0 {
		return ""
	}
	end := strings.Index(body[start:], "</tr>")
	if end < 0 {
		return body[start:]
	}
	return body[start : start+end]
}

// createdStamp is the visible text of the Created cell's <time> for the fixed
// instant seedBoardOrder backdates to.
func createdStamp(row string) string {
	const open = `<time data-utc="2026-10-05T12:24:13Z">`
	i := strings.Index(row, open)
	if i < 0 {
		return ""
	}
	rest := row[i+len(open):]
	return rest[:strings.Index(rest, "</time>")]
}

// visibleText is a fragment's text with the tags taken out and whitespace
// collapsed — what a reader sees in a cell.
func visibleText(html string) string {
	var b strings.Builder
	in := false
	for _, r := range html {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
			b.WriteRune(' ')
		case !in:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// routeText is the visible text of a row's route cell.
func routeText(row string) string {
	i := strings.Index(row, `<span class="order-route">`)
	if i < 0 {
		return ""
	}
	rest := row[i:]
	return visibleText(rest[:strings.Index(rest, "</td>")])
}

// U4: From → Line → To in place of the UUID column, the payload printed once,
// and Created with seconds (rows share a minute).
func TestOrdersBoard_RowColumns(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForRendering(t)

	o := seedBoardOrder(t, db, &orders.Order{
		EdgeUUID: "pin-uuid-7f3a", StationID: "line-1", OrderType: "complex",
		Status: protocol.StatusInTransit, SourceNode: "UTN_014", ProcessNode: "ALN_003",
		DeliveryNode: "UTN_013", PayloadCode: "SHIM", PayloadDesc: "SHIM (dev)",
	}, "2026-10-05T12:24:13Z")

	body := renderOrdersPage(t, h, "")
	row := boardRow(body, o.ID)
	if row == "" {
		t.Fatalf("order %d is not on the Active board", o.ID)
	}

	if strings.Contains(body, "<th data-sort>UUID</th>") {
		t.Error("the board still has a UUID column; the pop-up is where the UUID lives")
	}
	if strings.Contains(row, "pin-uuid-7f3a") {
		t.Error("the row still prints the edge UUID")
	}
	if got, want := routeText(row), "UTN_014 → ALN_003 → UTN_013"; got != want {
		t.Errorf("route = %q, want %q", got, want)
	}
	if strings.Count(visibleText(row), "SHIM") != 1 {
		t.Errorf("the payload must be printed once, row reads %q", visibleText(row))
	}
	if !strings.Contains(row, `<code title="SHIM (dev)">SHIM</code>`) {
		t.Error("the payload description should ride in the code's title")
	}
	if stamp := createdStamp(row); !strings.Contains(stamp, ":24:13") {
		t.Errorf("Created must carry seconds, got %q", stamp)
	}
}

// U4: the line node is omitted when it repeats an end; an end not chosen yet is
// a dash that says so, not a blank.
func TestOrdersBoard_RouteOmitsARepeatedNode(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForRendering(t)

	o := seedBoardOrder(t, db, &orders.Order{
		EdgeUUID: "pin-route-repeat", StationID: "line-1", OrderType: "retrieve",
		Status: protocol.StatusInTransit, SourceNode: "SYN_MARKET_01", ProcessNode: "ALN_008",
		DeliveryNode: "ALN_008",
	}, "")
	open := seedBoardOrder(t, db, &orders.Order{
		EdgeUUID: "pin-route-open", StationID: "line-1", OrderType: "complex",
		Status: protocol.StatusSourcing, SourceNode: "UTN_013",
	}, "")

	body := renderOrdersPage(t, h, "")
	if got, want := routeText(boardRow(body, o.ID)), "SYN_MARKET_01 → ALN_008"; got != want {
		t.Errorf("route = %q, want %q", got, want)
	}
	openRow := boardRow(body, open.ID)
	if got, want := routeText(openRow), "UTN_013 → —"; got != want {
		t.Errorf("route = %q, want %q", got, want)
	}
	if !strings.Contains(openRow, `title="not assigned yet"`) {
		t.Error("an unassigned destination must say so in its title (no data, not zero)")
	}
}

// U4: real paging in place of the ?limit= notice. Both directions of the old
// honesty pin survive: the board says what it holds back, and says nothing
// when it holds back nothing.
func TestOrdersBoard_Paging(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForRendering(t)

	var ids []int64
	for i := range 5 {
		o := seedBoardOrder(t, db, &orders.Order{
			EdgeUUID: fmt.Sprintf("pin-page-%d", i), StationID: "line-1", OrderType: "move",
			Status: protocol.StatusConfirmed,
		}, "")
		ids = append(ids, o.ID)
	}

	page1 := renderOrdersPage(t, h, "?status=all&limit=3")
	page2 := renderOrdersPage(t, h, "?status=all&limit=3&page=2")
	for _, id := range ids[2:] {
		if boardRow(page1, id) == "" {
			t.Errorf("page 1 is missing order %d (newest three)", id)
		}
		if boardRow(page2, id) != "" {
			t.Errorf("page 2 repeats order %d from page 1", id)
		}
	}
	for _, id := range ids[:2] {
		if boardRow(page2, id) == "" {
			t.Errorf("page 2 is missing order %d (the oldest two)", id)
		}
	}
	if strings.Contains(page1, "?limit=") && strings.Contains(page1, "to see them all") {
		t.Error("the ?limit= notice is still on the page")
	}
	if !strings.Contains(page1, "1–3 of 5") || !strings.Contains(page1, `href="/orders?limit=3&amp;page=2&amp;status=all"`) {
		t.Error("page 1 must say 1–3 of 5 and link page 2")
	}
	if !strings.Contains(page2, "4–5 of 5") || !strings.Contains(page2, `href="/orders?limit=3&amp;status=all"`) {
		t.Error("page 2 must say 4–5 of 5 and link back to page 1")
	}
	if strings.Contains(page2, `rel="next"`) {
		t.Error("the last page offers a next page")
	}

	whole := renderOrdersPage(t, h, "?status=all&limit=50")
	if strings.Contains(whole, `class="orders-pager"`) {
		t.Error("a view the page holds whole still draws a pager")
	}
}

// U4: the filter box queries the server, so it finds an order that is not on
// the page in front of it.
func TestOrdersBoard_SearchQueriesTheServer(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForRendering(t)

	hit := seedBoardOrder(t, db, &orders.Order{
		EdgeUUID: "pin-search-hit", StationID: "line-1", OrderType: "move",
		Status: protocol.StatusConfirmed,
	}, "")
	_, err := db.DB.Exec(`UPDATE orders SET robot_id='AMR-17' WHERE id=$1`, hit.ID)
	testutil.MustNoErr(t, err, "set robot")
	// Newer than the hit, so on a one-row page the hit is not on page 1.
	miss := seedBoardOrder(t, db, &orders.Order{
		EdgeUUID: "pin-search-miss", StationID: "line-1", OrderType: "move",
		Status: protocol.StatusConfirmed,
	}, "")
	_, err = db.DB.Exec(`UPDATE orders SET robot_id='AMR-03' WHERE id=$1`, miss.ID)
	testutil.MustNoErr(t, err, "set robot")

	body := renderOrdersPage(t, h, "?status=all&limit=1&q=amr-17")
	if boardRow(body, hit.ID) == "" {
		t.Error("the matching order is not on the page")
	}
	if boardRow(body, miss.ID) != "" {
		t.Error("an order that does not match the search is on the page")
	}

	// A search term's own wildcards are literal.
	if boardRow(renderOrdersPage(t, h, "?status=all&q=AMR_17"), hit.ID) != "" {
		t.Error("an underscore in the search matched any character")
	}
}

// U4: ?ids= (the Overview alert line) is exactly those orders, whatever their
// status now.
func TestOrdersBoard_IDsFilter(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForRendering(t)

	a := seedBoardOrder(t, db, &orders.Order{EdgeUUID: "pin-ids-a", StationID: "line-1",
		OrderType: "move", Status: protocol.StatusQueued}, "")
	b := seedBoardOrder(t, db, &orders.Order{EdgeUUID: "pin-ids-b", StationID: "line-1",
		OrderType: "move", Status: protocol.StatusQueued}, "")
	gone := seedBoardOrder(t, db, &orders.Order{EdgeUUID: "pin-ids-gone", StationID: "line-1",
		OrderType: "move", Status: protocol.StatusConfirmed}, "")

	body := renderOrdersPage(t, h, fmt.Sprintf("?ids=%d,%d", a.ID, gone.ID))
	if boardRow(body, a.ID) == "" {
		t.Error("a named active order is missing")
	}
	if boardRow(body, b.ID) != "" {
		t.Error("an order the link did not name is on the page")
	}
	if boardRow(body, gone.ID) == "" {
		t.Error("a named order that has since finished is missing")
	}
	if !strings.Contains(body, "Showing 2 orders picked by id") {
		t.Error("the page does not say it is showing a pick")
	}
}

// U4: a chip for every status an order can hold — staged, cancelled and
// skipped added; reshuffling kept (BeginReshuffle writes it).
func TestOrdersBoard_StatusChips(t *testing.T) {
	t.Parallel()
	h, _ := testHandlersForRendering(t)
	body := renderOrdersPage(t, h, "")
	for _, s := range []string{"pending", "sourcing", "queued", "dispatched", "in_transit", "staged",
		"faulted", "delivered", "confirmed", "failed", "cancelled", "skipped", "reshuffling"} {
		if !strings.Contains(body, `href="/orders?status=`+s+`"`) {
			t.Errorf("there is no %s chip", s)
		}
	}
}
