package www

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"shingo/protocol"
	"shingoedge/engine"
	"shingoedge/orders"
	storeorders "shingoedge/store/orders"
)

// ═══════════════════════════════════════════════════════════════════════
// Test router — mirrors the routes from router.go that bind to
// handlers_api_orders.go. Routes unrelated to this file are deliberately
// omitted. Tests use this router exclusively.
//
// Every order endpoint is registered under /api, matching the production
// route layout. All endpoints are public (shop floor); none live under the
// admin router group in production, so there is no 303/401 auth gating to
// test here.
// ═══════════════════════════════════════════════════════════════════════

func newApiOrdersRouter(t *testing.T) (*Handlers, *chi.Mux) {
	t.Helper()
	h, r := newTestHandlers(t)

	r.Route("/api", func(r chi.Router) {
		// Delivery confirmation (public)
		r.Post("/confirm-delivery/{orderID}", h.apiConfirmDelivery)

		// Order creation
		r.Post("/orders/retrieve", h.apiCreateRetrieveOrder)
		r.Post("/orders/store", h.apiCreateStoreOrder)
		r.Post("/orders/move", h.apiCreateMoveOrder)
		r.Post("/orders/complex", h.apiCreateComplexOrder)
		r.Post("/orders/ingest", h.apiCreateIngestOrder)

		// Order lifecycle
		r.Post("/orders/{orderID}/release", h.apiReleaseOrder)
		r.Post("/orders/{orderID}/submit", h.apiSubmitOrder)
		r.Post("/orders/{orderID}/cancel", h.apiCancelOrder)
		r.Post("/orders/{orderID}/abort", h.apiCancelOrder) // alias
		r.Post("/orders/{orderID}/redirect", h.apiRedirectOrder)
		r.Post("/orders/{orderID}/count", h.apiSetOrderCount)
	})

	return h, r
}

// ═══════════════════════════════════════════════════════════════════════
// Order creation — apiCreateRetrieveOrder, apiCreateStoreOrder,
// apiCreateMoveOrder, apiCreateComplexOrder, apiCreateIngestOrder
//
// All five endpoints share the same parse-body → call-engine → return-order
// shape. The table-driven TestApiOrders_CreateOrder_InvalidJSON covers the
// decode-error path for every endpoint in one sweep; per-endpoint tests
// exercise the happy path plus any endpoint-specific validation.
//
// DB call sites exercised: DB.CreateOrder (x5 types),
// DB.UpdateOrderStepsJSON (complex), DB.GetOrder (auto-submit),
// DB.EnqueueOutbox (auto-submit), DB.UpdateOrderStatus (auto-submit
// transition), DB.InsertOrderHistory (auto-submit),
// DB.UpdateOrderFinalCount + DB.UpdateOrderStatus (store submit).
// ═══════════════════════════════════════════════════════════════════════

func TestApiOrders_CreateRetrieveOrder_Success(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	body := map[string]interface{}{
		"payload_code":  "BIN-RET-1",
		"quantity":      1,
		"delivery_node": "LINE-A",
		"load_type":     "standard",
	}
	resp := doRequest(t, router, "POST", "/api/orders/retrieve", body, nil)
	assertStatus(t, resp, http.StatusOK)

	var order storeorders.Order
	decodeJSON(t, resp, &order)
	if order.ID == 0 {
		t.Fatal("expected non-zero order id")
	}
	if order.OrderType != orders.TypeRetrieve {
		t.Errorf("order type: got %q, want %q", order.OrderType, orders.TypeRetrieve)
	}
	if order.DeliveryNode != "LINE-A" {
		t.Errorf("delivery_node: got %q, want %q", order.DeliveryNode, "LINE-A")
	}

	// Verify DB state: auto-submit transitioned pending → submitted.
	stored, err := testDB.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if stored.Status != orders.StatusSubmitted {
		t.Errorf("status after create: got %q, want %q (auto-submit)",
			stored.Status, orders.StatusSubmitted)
	}
}

func TestApiOrders_CreateRetrieveOrder_ResolvesDeliveryFromNode(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	// Seed a process node so the handler's GetProcessNode lookup succeeds
	// and fills DeliveryNode from node.CoreNodeName.
	pid := seedProcess(t, "RetResolveLine")
	nodeID := seedProcessNode(t, pid, 0, "core-node-ret-x")

	body := map[string]interface{}{
		"process_node_id": nodeID,
		"payload_code":    "BIN-RET-RES",
		"quantity":        1,
		"load_type":       "standard",
		// delivery_node intentionally omitted — handler should derive it
	}
	resp := doRequest(t, router, "POST", "/api/orders/retrieve", body, nil)
	assertStatus(t, resp, http.StatusOK)

	var order storeorders.Order
	decodeJSON(t, resp, &order)
	if order.DeliveryNode != "core-node-ret-x" {
		t.Errorf("derived delivery_node: got %q, want %q",
			order.DeliveryNode, "core-node-ret-x")
	}
}

func TestApiOrders_CreateRetrieveOrder_BatchSuccess(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	body := map[string]interface{}{
		"payload_code":  "BIN-BATCH",
		"delivery_node": "LINE-B",
		"count":         3,
	}
	resp := doRequest(t, router, "POST", "/api/orders/retrieve", body, nil)
	assertStatus(t, resp, http.StatusOK)

	var out struct {
		Requested int `json:"requested"`
		Created   int `json:"created"`
		Orders    []struct {
			OrderID int64  `json:"order_id"`
			UUID    string `json:"uuid"`
			Error   string `json:"error"`
		} `json:"orders"`
	}
	decodeJSON(t, resp, &out)
	if out.Requested != 3 {
		t.Errorf("requested: got %d, want 3", out.Requested)
	}
	if out.Created != 3 {
		t.Errorf("created: got %d, want 3", out.Created)
	}
	if len(out.Orders) != 3 {
		t.Fatalf("results: got %d, want 3", len(out.Orders))
	}
	// Each sub-order should have a non-zero ID in the DB.
	for i, r := range out.Orders {
		if r.OrderID == 0 {
			t.Errorf("batch order %d: expected non-zero id", i)
		}
	}
}

func TestApiOrders_CreateRetrieveOrder_BatchExceedsMax(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	body := map[string]interface{}{
		"payload_code":  "BIN-TOO-MANY",
		"delivery_node": "LINE-X",
		"count":         MaxBatchRetrieveCount + 1,
	}
	resp := doRequest(t, router, "POST", "/api/orders/retrieve", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "count exceeds maximum of 5")
}

func TestApiOrders_CreateRetrieveOrder_BatchMissingFields(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	// count > 1 but no payload_code or delivery_node → rejected.
	body := map[string]interface{}{"count": 2}
	resp := doRequest(t, router, "POST", "/api/orders/retrieve", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "payload_code and delivery_node required for batch")
}

func TestApiOrders_CreateStoreOrder_Success(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	body := map[string]interface{}{
		"quantity":    7,
		"source_node": "CELL-1",
	}
	resp := doRequest(t, router, "POST", "/api/orders/store", body, nil)
	assertStatus(t, resp, http.StatusOK)

	var order storeorders.Order
	decodeJSON(t, resp, &order)
	if order.ID == 0 {
		t.Fatal("expected non-zero order id")
	}
	if order.OrderType != orders.TypeStore {
		t.Errorf("order type: got %q, want %q", order.OrderType, orders.TypeStore)
	}

	// Verify DB state: SubmitStoreOrder set final_count + count_confirmed
	// and transitioned to submitted.
	stored, err := testDB.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if stored.FinalCount == nil || *stored.FinalCount != 7 {
		t.Errorf("final_count: got %v, want 7", stored.FinalCount)
	}
	if !stored.CountConfirmed {
		t.Error("expected count_confirmed=true after create store order")
	}
	if stored.Status != orders.StatusSubmitted {
		t.Errorf("status: got %q, want %q", stored.Status, orders.StatusSubmitted)
	}
}

func TestApiOrders_CreateMoveOrder_Success(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	body := map[string]interface{}{
		"quantity":      4,
		"source_node":   "SRC-A",
		"delivery_node": "DST-A",
	}
	resp := doRequest(t, router, "POST", "/api/orders/move", body, nil)
	assertStatus(t, resp, http.StatusOK)

	var order storeorders.Order
	decodeJSON(t, resp, &order)
	if order.OrderType != orders.TypeMove {
		t.Errorf("order type: got %q, want %q", order.OrderType, orders.TypeMove)
	}
	if order.SourceNode != "SRC-A" || order.DeliveryNode != "DST-A" {
		t.Errorf("source/delivery: got %q/%q, want SRC-A/DST-A",
			order.SourceNode, order.DeliveryNode)
	}

	stored, _ := testDB.GetOrder(order.ID)
	if stored.Status != orders.StatusSubmitted {
		t.Errorf("status: got %q, want %q (auto-submit)",
			stored.Status, orders.StatusSubmitted)
	}
}

func TestApiOrders_CreateComplexOrder_Success(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	body := map[string]interface{}{
		"quantity": 2,
		"steps": []protocol.ComplexOrderStep{
			{Action: "pickup", Node: "NODE-A"},
			{Action: "dropoff", Node: "NODE-B"},
		},
	}
	resp := doRequest(t, router, "POST", "/api/orders/complex", body, nil)
	assertStatus(t, resp, http.StatusOK)

	var order storeorders.Order
	decodeJSON(t, resp, &order)
	if order.OrderType != orders.TypeComplex {
		t.Errorf("order type: got %q, want %q", order.OrderType, orders.TypeComplex)
	}

	// Verify DB state: steps_json should be populated.
	var stepsJSON string
	testDB.QueryRow("SELECT COALESCE(steps_json,'') FROM orders WHERE id=?", order.ID).Scan(&stepsJSON)
	if stepsJSON == "" {
		t.Error("expected steps_json to be populated for complex order")
	}
}

func TestApiOrders_CreateComplexOrder_MissingSteps(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	body := map[string]interface{}{"quantity": 1}
	resp := doRequest(t, router, "POST", "/api/orders/complex", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "steps are required")
}

func TestApiOrders_CreateIngestOrder_Success(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	body := map[string]interface{}{
		"payload_code": "BIN-INGEST",
		"bin_label":    "BIN-0001",
		"source_node":  "PRODUCE-1",
		"quantity":     24,
		"manifest": []protocol.IngestManifestItem{
			{PartNumber: "PN-1", Quantity: 12},
			{PartNumber: "PN-2", Quantity: 12},
		},
	}
	resp := doRequest(t, router, "POST", "/api/orders/ingest", body, nil)
	assertStatus(t, resp, http.StatusOK)

	var order storeorders.Order
	decodeJSON(t, resp, &order)
	if order.OrderType != orders.TypeIngest {
		t.Errorf("order type: got %q, want %q", order.OrderType, orders.TypeIngest)
	}
	if order.PayloadCode != "BIN-INGEST" {
		t.Errorf("payload_code: got %q, want BIN-INGEST", order.PayloadCode)
	}
}

func TestApiOrders_CreateIngestOrder_MissingPayloadCode(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	body := map[string]interface{}{
		"bin_label":   "BIN-X",
		"source_node": "CELL-1",
		"quantity":    1,
	}
	resp := doRequest(t, router, "POST", "/api/orders/ingest", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "payload_code is required")
}

func TestApiOrders_CreateIngestOrder_MissingBinLabel(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	body := map[string]interface{}{
		"payload_code": "BIN-Y",
		"source_node":  "CELL-1",
		"quantity":     1,
	}
	resp := doRequest(t, router, "POST", "/api/orders/ingest", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "bin_label is required")
}

// TestApiOrders_CreateOrder_InvalidJSON drives all five create endpoints with
// a syntactically valid body whose field has the wrong type, forcing the
// json.Decoder to fail. Each handler returns 400.
func TestApiOrders_CreateOrder_InvalidJSON(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	// quantity is int64 on every create-order request struct, so a string
	// value triggers a decode error consistently.
	cases := []struct {
		name string
		path string
	}{
		{"retrieve", "/api/orders/retrieve"},
		{"store", "/api/orders/store"},
		{"move", "/api/orders/move"},
		{"complex", "/api/orders/complex"},
		{"ingest", "/api/orders/ingest"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]interface{}{"quantity": "not-a-number"}
			resp := doRequest(t, router, "POST", tc.path, body, nil)
			assertStatus(t, resp, http.StatusBadRequest)
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════
// Delivery confirmation — apiConfirmDelivery
// DB call sites: DB.GetOrder, DB.UpdateOrderFinalCount, DB.EnqueueOutbox
// (via sender.Queue), DB.UpdateOrderStatus + DB.InsertOrderHistory (via
// TransitionOrder).
// ═══════════════════════════════════════════════════════════════════════

func TestApiOrders_ConfirmDelivery_Success(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	orderID := seedOrder(t, orders.TypeRetrieve, orders.StatusDelivered)

	body := map[string]int64{"final_count": 42}
	resp := doRequest(t, router, "POST", "/api/confirm-delivery/"+itoa(orderID), body, nil)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	// Verify DB state: transitioned to confirmed with final_count=42.
	stored, err := testDB.GetOrder(orderID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if stored.Status != orders.StatusConfirmed {
		t.Errorf("status: got %q, want %q", stored.Status, orders.StatusConfirmed)
	}
	if stored.FinalCount == nil || *stored.FinalCount != 42 {
		t.Errorf("final_count: got %v, want 42", stored.FinalCount)
	}
}

func TestApiOrders_ConfirmDelivery_InvalidID(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	body := map[string]int64{"final_count": 1}
	resp := doRequest(t, router, "POST", "/api/confirm-delivery/notanumber", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "invalid order ID")
}

func TestApiOrders_ConfirmDelivery_InvalidJSON(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	orderID := seedOrder(t, orders.TypeRetrieve, orders.StatusDelivered)

	body := map[string]interface{}{"final_count": "nope"}
	resp := doRequest(t, router, "POST", "/api/confirm-delivery/"+itoa(orderID), body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestApiOrders_ConfirmDelivery_WrongStatus(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	// Pending orders cannot be confirmed — must be delivered first.
	orderID := seedOrder(t, orders.TypeRetrieve, orders.StatusPending)

	body := map[string]int64{"final_count": 1}
	resp := doRequest(t, router, "POST", "/api/confirm-delivery/"+itoa(orderID), body, nil)
	assertStatus(t, resp, http.StatusBadRequest)

	// Body contains the lifecycle error message.
	var errResp map[string]string
	decodeJSON(t, resp, &errResp)
	if errResp["error"] == "" {
		t.Error("expected non-empty error message for wrong-status confirm")
	}
}

// ═══════════════════════════════════════════════════════════════════════
// Release — apiReleaseOrder
// DB call sites: DB.GetOrder, DB.EnqueueOutbox (sender.Queue),
// DB.UpdateOrderStatus + DB.InsertOrderHistory (TransitionOrder to in_transit).
// ═══════════════════════════════════════════════════════════════════════

func TestApiOrders_ReleaseOrder_Success(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	orderID := seedOrder(t, orders.TypeRetrieve, orders.StatusStaged)

	body := map[string]interface{}{
		"called_by": "test-station",
	}
	resp := doRequest(t, router, "POST", "/api/orders/"+itoa(orderID)+"/release", body, nil)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	stored, _ := testDB.GetOrder(orderID)
	if stored.Status != orders.StatusInTransit {
		t.Errorf("status after release: got %q, want %q",
			stored.Status, orders.StatusInTransit)
	}
}

// TestApiOrders_ReleaseOrder_RejectsBareBody verifies the post-2026-04-27
// guard: a release POST with no body (or empty called_by) is rejected as
// 400 instead of silently producing the disposition-bypass fingerprint
// (called_by="" + remaining_uop=<nil>) that hid the plant-test bug.
// Every legitimate caller (operator.js, kanbans.js) sends called_by.
func TestApiOrders_ReleaseOrder_RejectsBareBody(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	orderID := seedOrder(t, orders.TypeRetrieve, orders.StatusStaged)

	resp := doRequest(t, router, "POST", "/api/orders/"+itoa(orderID)+"/release", nil, nil)
	assertStatus(t, resp, http.StatusBadRequest)

	// Body with empty called_by is also rejected.
	body := map[string]interface{}{
		"disposition": "capture_lineside",
		"called_by":   "",
	}
	resp = doRequest(t, router, "POST", "/api/orders/"+itoa(orderID)+"/release", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestApiOrders_ReleaseOrder_InvalidID(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	resp := doRequest(t, router, "POST", "/api/orders/bad/release", nil, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "invalid order ID")
}

func TestApiOrders_ReleaseOrder_WrongStatus(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	// Terminal orders cannot be released. Pre-dispatch (pending/submitted)
	// is now a silent no-op under the post-2026-04-27 contract because the
	// consolidated release fan-out tolerates pre-dispatch siblings, so this
	// test specifically exercises the terminal-rejection path that's still
	// surfaced as an API error.
	orderID := seedOrder(t, orders.TypeRetrieve, orders.StatusConfirmed)

	body := map[string]interface{}{
		"called_by": "test-station",
	}
	resp := doRequest(t, router, "POST", "/api/orders/"+itoa(orderID)+"/release", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

// TestApiOrders_ReleaseOrder_UnknownDispositionFallsBackToNoOp verifies the
// default branch in buildReleaseDisposition: an unknown disposition string
// (typo, version drift, future API value) maps to ReleaseDisposition{}
// (Mode == "") which produces nil remainingUOP — Core leaves the bin's
// manifest alone. The handler still succeeds; only the disposition is
// downgraded. CalledBy still flows for audit. Pairs with the log.Printf
// in buildReleaseDisposition that catches typos.
func TestApiOrders_ReleaseOrder_UnknownDispositionFallsBackToNoOp(t *testing.T) {
	h, router := newApiOrdersRouter(t)
	orderID := seedOrder(t, orders.TypeRetrieve, orders.StatusStaged)

	body := map[string]interface{}{
		"disposition": "send_partial_bak", // deliberate typo
		"called_by":   "stephen-station-7",
	}
	resp := doRequest(t, router, "POST", "/api/orders/"+itoa(orderID)+"/release", body, nil)
	assertStatus(t, resp, http.StatusOK)

	stub := h.engine.(*stubEngine)
	if stub.lastReleaseOrderDisposition == nil {
		t.Fatal("expected ReleaseOrderWithLineside to have been called")
	}
	if stub.lastReleaseOrderDisposition.Mode != "" {
		t.Errorf("disposition Mode: got %q, want empty (unknown mode falls back to no-op)",
			stub.lastReleaseOrderDisposition.Mode)
	}
	if stub.lastReleaseOrderDisposition.CalledBy != "stephen-station-7" {
		t.Errorf("disposition CalledBy: got %q, want %q (still flows on unknown mode)",
			stub.lastReleaseOrderDisposition.CalledBy, "stephen-station-7")
	}
}

// TestApiOrders_ReleaseOrder_CaptureLinesideDispositionFlows verifies that
// the explicit capture_lineside disposition from the NOTHING PULLED button
// or per-part qty submission is correctly mapped to the engine.
func TestApiOrders_ReleaseOrder_CaptureLinesideDispositionFlows(t *testing.T) {
	h, router := newApiOrdersRouter(t)
	orderID := seedOrder(t, orders.TypeRetrieve, orders.StatusStaged)

	body := map[string]interface{}{
		"disposition": "capture_lineside",
		"qty_by_part": map[string]int{"PART-A": 5},
		"called_by":   "stephen-station-7",
	}
	resp := doRequest(t, router, "POST", "/api/orders/"+itoa(orderID)+"/release", body, nil)
	assertStatus(t, resp, http.StatusOK)

	stub := h.engine.(*stubEngine)
	if stub.lastReleaseOrderDisposition == nil {
		t.Fatal("expected ReleaseOrderWithLineside to have been called")
	}
	if stub.lastReleaseOrderDisposition.Mode != engine.DispositionCaptureLineside {
		t.Errorf("disposition Mode: got %q, want %q",
			stub.lastReleaseOrderDisposition.Mode, engine.DispositionCaptureLineside)
	}
	if got := stub.lastReleaseOrderDisposition.LinesideCapture["PART-A"]; got != 5 {
		t.Errorf("LinesideCapture[PART-A]: got %d, want 5", got)
	}
}

// TestApiOrders_ReleaseOrder_SendPartialBackDispositionFlows verifies the
// SEND PARTIAL BACK button's submission path: disposition arrives at the
// engine, qty_by_part is dropped (no capture for partial-back), CalledBy
// flows through.
func TestApiOrders_ReleaseOrder_SendPartialBackDispositionFlows(t *testing.T) {
	h, router := newApiOrdersRouter(t)
	orderID := seedOrder(t, orders.TypeRetrieve, orders.StatusStaged)

	body := map[string]interface{}{
		"disposition": "send_partial_back",
		"called_by":   "stephen-station-7",
	}
	resp := doRequest(t, router, "POST", "/api/orders/"+itoa(orderID)+"/release", body, nil)
	assertStatus(t, resp, http.StatusOK)

	stub := h.engine.(*stubEngine)
	if stub.lastReleaseOrderDisposition == nil {
		t.Fatal("expected ReleaseOrderWithLineside to have been called")
	}
	if stub.lastReleaseOrderDisposition.Mode != engine.DispositionSendPartialBack {
		t.Errorf("disposition Mode: got %q, want %q",
			stub.lastReleaseOrderDisposition.Mode, engine.DispositionSendPartialBack)
	}
	if len(stub.lastReleaseOrderDisposition.LinesideCapture) != 0 {
		t.Errorf("LinesideCapture should be empty on send_partial_back; got %v",
			stub.lastReleaseOrderDisposition.LinesideCapture)
	}
}

// ═══════════════════════════════════════════════════════════════════════
// Submit — apiSubmitOrder
// DB call sites: DB.GetOrder, DB.UpdateOrderStatus +
// DB.InsertOrderHistory (TransitionOrder pending → submitted).
// ═══════════════════════════════════════════════════════════════════════

func TestApiOrders_SubmitOrder_Success(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	// Use a retrieve order (store orders require count confirmation, which
	// is its own code path exercised in CreateStoreOrder tests).
	orderID := seedOrder(t, orders.TypeRetrieve, orders.StatusPending)

	resp := doRequest(t, router, "POST", "/api/orders/"+itoa(orderID)+"/submit", nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	stored, _ := testDB.GetOrder(orderID)
	if stored.Status != orders.StatusSubmitted {
		t.Errorf("status after submit: got %q, want %q",
			stored.Status, orders.StatusSubmitted)
	}
}

func TestApiOrders_SubmitOrder_InvalidID(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	resp := doRequest(t, router, "POST", "/api/orders/bad/submit", nil, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "invalid order ID")
}

func TestApiOrders_SubmitOrder_MissingOrder(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	// Order ID 99999999 should not exist in testDB.
	resp := doRequest(t, router, "POST", "/api/orders/99999999/submit", nil, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

// ═══════════════════════════════════════════════════════════════════════
// Cancel / Abort — apiCancelOrder (same handler, two route aliases)
// DB call sites: DB.GetOrder, DB.EnqueueOutbox (sender.Queue),
// DB.UpdateOrderStatus + DB.InsertOrderHistory (TransitionOrder to cancelled).
// ═══════════════════════════════════════════════════════════════════════

func TestApiOrders_CancelOrder_Success(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	orderID := seedOrder(t, orders.TypeRetrieve, orders.StatusSubmitted)

	resp := doRequest(t, router, "POST", "/api/orders/"+itoa(orderID)+"/cancel", nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	stored, _ := testDB.GetOrder(orderID)
	if stored.Status != orders.StatusCancelled {
		t.Errorf("status after cancel: got %q, want %q",
			stored.Status, orders.StatusCancelled)
	}
}

func TestApiOrders_AbortOrder_Alias(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	orderID := seedOrder(t, orders.TypeRetrieve, orders.StatusSubmitted)

	// /abort is an alias for /cancel — same handler, same behaviour.
	resp := doRequest(t, router, "POST", "/api/orders/"+itoa(orderID)+"/abort", nil, nil)
	assertStatus(t, resp, http.StatusOK)

	stored, _ := testDB.GetOrder(orderID)
	if stored.Status != orders.StatusCancelled {
		t.Errorf("status after abort: got %q, want %q",
			stored.Status, orders.StatusCancelled)
	}
}

func TestApiOrders_CancelOrder_InvalidID(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	resp := doRequest(t, router, "POST", "/api/orders/bad/cancel", nil, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "invalid order ID")
}

func TestApiOrders_CancelOrder_AlreadyTerminal(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	// Cancelled (terminal) orders cannot be cancelled again.
	orderID := seedOrder(t, orders.TypeRetrieve, orders.StatusCancelled)

	resp := doRequest(t, router, "POST", "/api/orders/"+itoa(orderID)+"/cancel", nil, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

// ═══════════════════════════════════════════════════════════════════════
// Set final count — apiSetOrderCount
// DB call site: DB.UpdateOrderFinalCount (direct, bypasses manager).
// ═══════════════════════════════════════════════════════════════════════

func TestApiOrders_SetOrderCount_Success(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	orderID := seedOrder(t, orders.TypeStore, orders.StatusPending)

	body := map[string]int64{"final_count": 99}
	resp := doRequest(t, router, "POST", "/api/orders/"+itoa(orderID)+"/count", body, nil)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	stored, _ := testDB.GetOrder(orderID)
	if stored.FinalCount == nil || *stored.FinalCount != 99 {
		t.Errorf("final_count: got %v, want 99", stored.FinalCount)
	}
	if !stored.CountConfirmed {
		t.Error("expected count_confirmed=true after set-count")
	}
}

func TestApiOrders_SetOrderCount_InvalidID(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	body := map[string]int64{"final_count": 1}
	resp := doRequest(t, router, "POST", "/api/orders/bad/count", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "invalid order ID")
}

func TestApiOrders_SetOrderCount_InvalidJSON(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	orderID := seedOrder(t, orders.TypeStore, orders.StatusPending)

	body := map[string]interface{}{"final_count": "not-a-number"}
	resp := doRequest(t, router, "POST", "/api/orders/"+itoa(orderID)+"/count", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

// ═══════════════════════════════════════════════════════════════════════
// Redirect — apiRedirectOrder
// DB call sites: DB.GetOrder, DB.EnqueueOutbox (sender.Queue),
// DB.UpdateOrderDeliveryNode.
// ═══════════════════════════════════════════════════════════════════════

func TestApiOrders_RedirectOrder_Success(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	orderID := seedOrder(t, orders.TypeRetrieve, orders.StatusSubmitted)

	body := map[string]string{"delivery_node": "NEW-DEST"}
	resp := doRequest(t, router, "POST", "/api/orders/"+itoa(orderID)+"/redirect", body, nil)
	assertStatus(t, resp, http.StatusOK)

	var order storeorders.Order
	decodeJSON(t, resp, &order)
	if order.DeliveryNode != "NEW-DEST" {
		t.Errorf("delivery_node in response: got %q, want NEW-DEST", order.DeliveryNode)
	}

	// Verify DB state.
	stored, _ := testDB.GetOrder(orderID)
	if stored.DeliveryNode != "NEW-DEST" {
		t.Errorf("delivery_node in DB: got %q, want NEW-DEST", stored.DeliveryNode)
	}
}

func TestApiOrders_RedirectOrder_InvalidID(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	body := map[string]string{"delivery_node": "X"}
	resp := doRequest(t, router, "POST", "/api/orders/bad/redirect", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "invalid order ID")
}

func TestApiOrders_RedirectOrder_MissingDeliveryNode(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	orderID := seedOrder(t, orders.TypeRetrieve, orders.StatusSubmitted)

	body := map[string]string{"delivery_node": ""}
	resp := doRequest(t, router, "POST", "/api/orders/"+itoa(orderID)+"/redirect", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "delivery_node is required")
}

func TestApiOrders_RedirectOrder_InvalidJSON(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	orderID := seedOrder(t, orders.TypeRetrieve, orders.StatusSubmitted)

	// delivery_node must be a string; sending an int triggers a decode error.
	body := map[string]interface{}{"delivery_node": 42}
	resp := doRequest(t, router, "POST", "/api/orders/"+itoa(orderID)+"/redirect", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestApiOrders_RedirectOrder_AlreadyTerminal(t *testing.T) {
	_, router := newApiOrdersRouter(t)

	orderID := seedOrder(t, orders.TypeRetrieve, orders.StatusConfirmed)

	body := map[string]string{"delivery_node": "ANYWHERE"}
	resp := doRequest(t, router, "POST", "/api/orders/"+itoa(orderID)+"/redirect", body, nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

// ═══════════════════════════════════════════════════════════════════════
// Smoke check — json.RawMessage sanity for body decoding.
// The handlers_api_orders.go file uses stdlib json.Decoder. This test
// documents the invariant called out in the PR gotchas: for InvalidJSON
// cases we send syntactically valid JSON with a wrong field type, not
// malformed bytes.
// ═══════════════════════════════════════════════════════════════════════

func TestApiOrders_DecodeHelper_StringInsteadOfInt(t *testing.T) {
	// Sanity: {"quantity":"x"} is syntactically valid JSON — it fails at
	// decode-time, not marshal-time.
	b, err := json.Marshal(map[string]interface{}{"quantity": "x"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var target struct {
		Quantity int64 `json:"quantity"`
	}
	if err := json.Unmarshal(b, &target); err == nil {
		t.Error("expected decode error for string-into-int64")
	}
}
