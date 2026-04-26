//go:build docker

package www

import (
	"encoding/json"
	"html/template"
	"net/http"
	"testing"

	"shingo/protocol/debuglog"
	"shingocore/config"
	"shingocore/engine"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
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
		engine:   eng,
		orchestration: eng,
		sessions: newSessionStore("test-secret"),
		tmpls:    make(map[string]*template.Template),
		eventHub: hub,
		debugLog: dbgLog,
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
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if vendorID != "" {
		if err := db.UpdateOrderVendor(o.ID, vendorID, "CREATED", ""); err != nil {
			t.Fatalf("update order vendor: %v", err)
		}
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
	if _, err := sim.CreateTransportOrder(fleet.TransportOrderRequest{
		OrderID:  vendorID,
		FromLoc:  fromLoc,
		ToLoc:    toLoc,
		Priority: priority,
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
	h, _ := testHandlers(t)

	rec := postJSON(t, h.apiSetOrderPriority, "/api/order/priority",
		map[string]any{"order_id": 9999999, "priority": 1})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// --- submitSpotRetrieveSpecific ---------------------------------------------

// retrieveSpecificResponse mirrors readBackSpotOrder's JSON envelope.
type retrieveSpecificResponse struct {
	OrderID     int64  `json:"order_id"`
	Status      string `json:"status"`
	ErrorDetail string `json:"error_detail"`
	Error       string `json:"error"`
}

// submitRetrieveSpecific POSTs a retrieve_specific spot order through the
// public apiSpotOrderSubmit handler and returns the decoded envelope.
func submitRetrieveSpecific(t *testing.T, h *Handlers, binLabel, deliveryNode string) (*retrieveSpecificResponse, int) {
	t.Helper()
	rec := postJSON(t, h.apiSpotOrderSubmit, "/api/spot/submit",
		map[string]any{
			"order_type":    "retrieve_specific",
			"bin_label":     binLabel,
			"delivery_node": deliveryNode,
			"description":   "spot-retrieve-specific char-test",
			"priority":      1,
		})
	var resp retrieveSpecificResponse
	if rec.Body.Len() > 0 {
		_ = json.NewDecoder(rec.Body).Decode(&resp)
	}
	return &resp, rec.Code
}

// TestSubmitSpotRetrieveSpecific_HappyPath pins the baseline: bin gets claimed
// by the new order, order advances to "dispatched" with a vendor_order_id.
func TestSubmitSpotRetrieveSpecific_HappyPath(t *testing.T) {
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
	sim := simulator.New(simulator.WithCreateFailure())
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-RS-FAIL")

	resp, status := submitRetrieveSpecific(t, h, bin.Label, sd.LineNode.Name)
	// readBackSpotOrder returns 200 with {status:"failed"} after the handler's
	// rollback path — the dispatch failure is reflected in the body, not the
	// HTTP status.
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body reflects failure); err=%q", status, resp.Error)
	}
	if resp.OrderID == 0 {
		t.Fatalf("order_id missing in response: %+v", resp)
	}

	got, err := db.GetOrder(resp.OrderID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if got.Status != "failed" {
		t.Errorf("order status after dispatch failure: got %q, want %q", got.Status, "failed")
	}
	if got.VendorOrderID != "" {
		t.Errorf("vendor_order_id should be empty after failed dispatch, got %q", got.VendorOrderID)
	}

	// The critical rollback: bin claim released.
	testdb.RequireBinUnclaimed(t, db, bin.ID)
}

func TestSubmitSpotRetrieveSpecific_MissingBinLabel(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	resp, status := submitRetrieveSpecific(t, h, "", sd.LineNode.Name)
	if status != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; resp=%+v", status, resp)
	}
}

func TestSubmitSpotRetrieveSpecific_MissingDeliveryNode(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-RS-NODEST")

	resp, status := submitRetrieveSpecific(t, h, bin.Label, "")
	if status != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; resp=%+v", status, resp)
	}
}

func TestSubmitSpotRetrieveSpecific_UnknownBin(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	resp, status := submitRetrieveSpecific(t, h, "BIN-DOES-NOT-EXIST", sd.LineNode.Name)
	if status != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; resp=%+v", status, resp)
	}
}

func TestSubmitSpotRetrieveSpecific_UnknownDeliveryNode(t *testing.T) {
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
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-RS-CLAIMED")

	// Stand up a prior order and have it claim the bin.
	prior := makeOrder(t, db, "prior-claim-1", "", 0)
	if err := db.ClaimBin(bin.ID, prior.ID); err != nil {
		t.Fatalf("seed prior claim: %v", err)
	}

	resp, status := submitRetrieveSpecific(t, h, bin.Label, sd.LineNode.Name)
	if status != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; resp=%+v", status, resp)
	}

	// Bin still claimed by the prior order — no overwrite.
	testdb.RequireBinClaimedBy(t, db, bin.ID, prior.ID)

	// No new order created for this spot submit (readBackSpotOrder never ran).
	if resp.OrderID != 0 {
		t.Errorf("expected no new order on 409, got order_id=%d", resp.OrderID)
	}
}

