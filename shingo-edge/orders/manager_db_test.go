package orders

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/catalog"
	"shingoedge/store/messaging"
	"shingoedge/store/processes"
)

// ═══════════════════════════════════════════════════════════════════════
// Coverage additions for orders.Manager.
//
// PR 3.1 — order creation family (CreateRetrieveOrder, CreateMoveOrder,
//   CreateMoveOrderWithUOP, CreateComplexOrder). Every
//   happy path is asserted
//   against both DB state AND the outbox envelope that was queued, since
//   the whole reason enqueueAndAutoSubmit is non-trivial is that the two
//   must stay consistent.
//
// PR 3.2 — HandleDispatchReply's 8-way switch (ack/waybill/queued/update/
//   delivered/error/staged/cancelled) plus the unknown-type and missing-
//   order error paths. Delivered has two sub-cases (auto-confirm on/off)
//   because the auto-confirm branch is the only thing exercising
//   ConfirmDelivery's happy path.
//
// PR 3.3 — lifecycle helpers: ApplyCoreStatusSnapshot across all forced-
//   transition branches, lookupPayloadMeta via CreateMoveOrder (nil node,
//   active-style-only, target-style overrides active during changeover),
//   ReleaseOrder, AbortOrder and RedirectOrder happy paths plus their
//   terminal-rejection guards, TransitionOrder delegation, and
//   HandleDeliveredWithExpiry persisting the expiry timestamp.
// ═══════════════════════════════════════════════════════════════════════

// ───────────────────────────────────────────────────────────────────────
// Shared helpers.
// ───────────────────────────────────────────────────────────────────────

// capturingEmitter records every event so tests can assert on emitter
// calls without printing or losing arg detail. Pointer receivers so
// appends are visible to the caller.
type capturingEmitter struct {
	created   []string
	status    []string // "oldStatus→newStatus"
	completed []string
	failed    []string
}

func (e *capturingEmitter) EmitOrderCreated(orderID int64, orderUUID string, orderType protocol.OrderType, payloadID, processNodeID *int64) {
	e.created = append(e.created, string(orderType)+":"+orderUUID)
}

func (e *capturingEmitter) EmitOrderStatusChanged(orderID int64, orderUUID string, orderType protocol.OrderType, oldStatus, newStatus, eta string, payloadID, processNodeID *int64) {
	e.status = append(e.status, oldStatus+"→"+newStatus)
}

func (e *capturingEmitter) EmitOrderCompleted(orderID int64, orderUUID string, orderType protocol.OrderType, payloadID, processNodeID *int64) {
	e.completed = append(e.completed, string(orderType)+":"+orderUUID)
}

func (e *capturingEmitter) EmitOrderDelivered(orderID int64, orderUUID string, orderType protocol.OrderType, processNodeID, binID *int64, binUOP *int, binEpoch int64) {
}

func (e *capturingEmitter) EmitOrderDeliveredFallback(binID int64, binUOP *int, binEpoch int64, deliveryNode string) {
}

func (e *capturingEmitter) EmitOrderFailed(orderID int64, orderUUID string, orderType protocol.OrderType, reason string) {
	e.failed = append(e.failed, string(orderType)+":"+reason)
}
func (e *capturingEmitter) EmitOrderFaulted(orderID int64, orderUUID, reason string) {
}

// seedProcessStyleNode seeds a process, a style on it, and a process_node
// for the given core node name. Returns the ids needed for wiring.
func seedProcessStyleNode(t *testing.T, db *store.DB, procName, styleName, coreNode string) (processID, styleID, nodeID int64) {
	t.Helper()
	pid, err := db.CreateProcess(procName, "", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	sid, err := db.CreateStyle(styleName, "", pid)
	if err != nil {
		t.Fatalf("CreateStyle: %v", err)
	}
	nid, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    pid,
		CoreNodeName: coreNode,
		Name:         coreNode,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateProcessNode: %v", err)
	}
	return pid, sid, nid
}

// seedClaim upserts a style_node_claim for a (style, coreNode) pair.
func seedClaim(t *testing.T, db *store.DB, styleID int64, coreNode, payloadCode string) int64 {
	t.Helper()
	id, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:      styleID,
		CoreNodeName: coreNode,
		Role:         "consume",
		SwapMode:     "single_robot",
		PayloadCode:  payloadCode,
	})
	if err != nil {
		t.Fatalf("UpsertStyleNodeClaim: %v", err)
	}
	return id
}

// decodeOnlyOutboxPayload pulls exactly one pending outbox message of the
// given msg type, unmarshals its envelope, decodes the payload into target,
// and returns the envelope for further assertions. Fails if the count
// doesn't match exactly one.
func decodeOnlyOutboxPayload(t *testing.T, db *store.DB, msgType string, target any) protocol.Envelope {
	t.Helper()
	msgs, err := db.ListPendingOutbox(100)
	if err != nil {
		t.Fatalf("ListPendingOutbox: %v", err)
	}
	var matches []messaging.Message
	for _, m := range msgs {
		if m.MsgType == msgType {
			matches = append(matches, m)
		}
	}
	if len(matches) != 1 {
		types := make([]string, 0, len(msgs))
		for _, m := range msgs {
			types = append(types, m.MsgType)
		}
		t.Fatalf("outbox: got %d %s messages, want 1 (all types: %v)", len(matches), msgType, types)
	}
	var env protocol.Envelope
	testutil.MustNoErr(t, json.Unmarshal(matches[0].Payload, &env), "unmarshal envelope")
	if env.Type != msgType {
		t.Fatalf("envelope.Type: got %q, want %q", env.Type, msgType)
	}
	testutil.MustNoErr(t, env.DecodePayload(target), "decode payload")
	return env
}

// ═══════════════════════════════════════════════════════════════════════
// PR 3.1 — Order creation family.
// ═══════════════════════════════════════════════════════════════════════

// TestCreateComplexOrderSibling_CarriesSiblingUUID verifies the removal leg
// of a two-robot swap ships its supply sibling's UUID on the
// ComplexOrderRequest, so Core can pair the legs at intake — the wire half
// of the ALN_003 swap-starvation fix (task 0).
func TestCreateComplexOrderSibling_CarriesSiblingUUID(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, &capturingEmitter{}, "edge.station")

	const supplyUUID = "supply-uuid-abc123"
	evac, err := mgr.CreateComplexOrderSibling(nil, 1, "AMR Supermarket", "ALN_003",
		[]protocol.ComplexOrderStep{{Action: "pickup", Node: "ALN_003"}, {Action: "dropoff", Node: "AMR Supermarket"}},
		true, "", supplyUUID)
	testutil.MustNoErr(t, err, "create evac leg")

	var req protocol.ComplexOrderRequest
	decodeOnlyOutboxPayload(t, db, protocol.TypeComplexOrderRequest, &req)
	if req.OrderUUID != evac.UUID {
		t.Errorf("OrderUUID: got %s, want %s", req.OrderUUID, evac.UUID)
	}
	if req.SiblingOrderUUID != supplyUUID {
		t.Errorf("SiblingOrderUUID: got %q, want %q", req.SiblingOrderUUID, supplyUUID)
	}
}

func TestCreateRetrieveOrder_HappyPath(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	emitter := &capturingEmitter{}
	mgr := NewManager(db, emitter, "edge.station")

	order, err := mgr.CreateRetrieveOrder(nil, false, 7, "LINE-1", "SRC-A", "STAGE-1", "LOAD-A", "PL-42", false, false)
	if err != nil {
		t.Fatalf("CreateRetrieveOrder: %v", err)
	}
	if order.OrderType != TypeRetrieve {
		t.Errorf("OrderType: got %q, want %q", order.OrderType, TypeRetrieve)
	}
	if order.Status != StatusSubmitted {
		t.Errorf("Status: got %q, want %q (auto-submit)", order.Status, StatusSubmitted)
	}
	if order.Quantity != 7 || order.DeliveryNode != "LINE-1" || order.SourceNode != "SRC-A" || order.StagingNode != "STAGE-1" || order.LoadType != "LOAD-A" {
		t.Errorf("order fields wrong: %+v", order)
	}
	if order.PayloadCode != "PL-42" {
		t.Errorf("PayloadCode: got %q, want PL-42", order.PayloadCode)
	}

	var req protocol.OrderRequest
	env := decodeOnlyOutboxPayload(t, db, protocol.TypeOrderRequest, &req)
	if env.Src.Station != "edge.station" {
		t.Errorf("envelope Src.Station: got %q, want edge.station", env.Src.Station)
	}
	if req.OrderUUID != order.UUID || req.OrderType != TypeRetrieve || req.Quantity != 7 {
		t.Errorf("OrderRequest fields wrong: %+v", req)
	}
	if req.DeliveryNode != "LINE-1" || req.SourceNode != "SRC-A" || req.StagingNode != "STAGE-1" || req.LoadType != "LOAD-A" {
		t.Errorf("OrderRequest routing fields wrong: %+v", req)
	}

	if len(emitter.created) != 1 || !strings.HasPrefix(emitter.created[0], "retrieve:") {
		t.Errorf("created events: %v", emitter.created)
	}
	if len(emitter.status) == 0 || emitter.status[0] != string(StatusPending)+"→"+string(StatusSubmitted) {
		t.Errorf("status events: %v (want pending→submitted first)", emitter.status)
	}
}

func TestCreateRetrieveOrder_PayloadMetaFromStyleClaim(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	pid, sid, nid := seedProcessStyleNode(t, db, "P1", "S1", "CN-A")
	_ = seedClaim(t, db, sid, "CN-A", "PL-AUTO")
	active := sid
	testutil.MustNoErr(t, db.SetActiveStyle(pid, &active), "SetActiveStyle")

	// Seed a catalog entry so lookupPayloadMeta can resolve the description.
	testutil.MustNoErr(t, catalog.UpsertCatalog(db.DB, &catalog.CatalogEntry{
		Name:        "Auto Test Payload",
		Code:        "PL-AUTO",
		Description: "Auto Test Payload",
	}), "UpsertCatalog")

	order, err := mgr.CreateRetrieveOrder(&nid, false, 1, "LINE-1", "", "", "", "", false, false)
	if err != nil {
		t.Fatalf("CreateRetrieveOrder: %v", err)
	}
	if order.PayloadCode != "PL-AUTO" {
		t.Errorf("PayloadCode should be derived from style claim: got %q, want PL-AUTO", order.PayloadCode)
	}

	var req protocol.OrderRequest
	decodeOnlyOutboxPayload(t, db, protocol.TypeOrderRequest, &req)
	if req.PayloadCode != "PL-AUTO" {
		t.Errorf("envelope PayloadCode: got %q, want PL-AUTO", req.PayloadCode)
	}
	if req.PayloadDesc != "Auto Test Payload" {
		t.Errorf("envelope PayloadDesc: got %q, want %q", req.PayloadDesc, "Auto Test Payload")
	}
}

func TestCreateMoveOrder_HappyPath(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	order, err := mgr.CreateMoveOrder(nil, 3, "SRC-M", "DST-M", false)
	if err != nil {
		t.Fatalf("CreateMoveOrder: %v", err)
	}
	if order.OrderType != TypeMove || order.Status != StatusSubmitted {
		t.Errorf("order: type=%q status=%q", order.OrderType, order.Status)
	}
	if order.SourceNode != "SRC-M" || order.DeliveryNode != "DST-M" {
		t.Errorf("nodes: source=%q delivery=%q", order.SourceNode, order.DeliveryNode)
	}
	var req protocol.OrderRequest
	decodeOnlyOutboxPayload(t, db, protocol.TypeOrderRequest, &req)
	if req.OrderType != TypeMove || req.SourceNode != "SRC-M" || req.DeliveryNode != "DST-M" {
		t.Errorf("envelope: %+v", req)
	}
	if req.RemainingUOP != nil {
		t.Errorf("RemainingUOP: got %v, want nil (plain CreateMoveOrder)", req.RemainingUOP)
	}
}

func TestCreateMoveOrderWithUOP_ThreadsRemainingUOP(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	remaining := 5
	if _, err := mgr.CreateMoveOrderWithUOP(nil, 1, "SRC", "DST", &remaining, false); err != nil {
		t.Fatalf("CreateMoveOrderWithUOP: %v", err)
	}
	var req protocol.OrderRequest
	decodeOnlyOutboxPayload(t, db, protocol.TypeOrderRequest, &req)
	if req.RemainingUOP == nil || *req.RemainingUOP != 5 {
		t.Errorf("RemainingUOP: %v, want *5", req.RemainingUOP)
	}
}

func TestCreateComplexOrder_PersistsStepsAndQueuesEnvelope(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	steps := []protocol.ComplexOrderStep{
		{Action: "pickup", Node: "A"},
		{Action: "dropoff", Node: "B"},
	}
	order, err := mgr.CreateComplexOrder(nil, 2, "DEL-C", "", steps)
	if err != nil {
		t.Fatalf("CreateComplexOrder: %v", err)
	}
	if order.OrderType != TypeComplex || order.Status != StatusSubmitted {
		t.Errorf("order: type=%q status=%q", order.OrderType, order.Status)
	}

	var req protocol.ComplexOrderRequest
	decodeOnlyOutboxPayload(t, db, protocol.TypeComplexOrderRequest, &req)
	if len(req.Steps) != 2 || req.Steps[0].Action != "pickup" || req.Steps[1].Node != "B" {
		t.Errorf("complex request steps: %+v", req.Steps)
	}
	if req.Quantity != 2 || req.OrderUUID != order.UUID {
		t.Errorf("complex request header: %+v", req)
	}
}

// ═══════════════════════════════════════════════════════════════════════
// PR 3.2 — HandleDispatchReply 8-way switch + unknown + missing-order.
// ═══════════════════════════════════════════════════════════════════════

func TestHandleDispatchReply_Ack(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, err := db.CreateOrder("uuid-ack", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(oid, string(StatusSubmitted)), "set submitted")

	testutil.MustNoErr(t, mgr.HandleDispatchReply("uuid-ack", ReplyAck, "", "", "core ack"), "HandleDispatchReply")
	o, _ := db.GetOrder(oid)
	if o.Status != StatusAcknowledged {
		t.Errorf("Status: got %q, want %q", o.Status, StatusAcknowledged)
	}
}

func TestHandleDispatchReply_Waybill_PersistsIDAndETA(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-wb", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))
	_ = db.UpdateOrderStatus(oid, string(StatusAcknowledged))

	testutil.MustNoErr(t, mgr.HandleDispatchReply("uuid-wb", ReplyWaybill, "WB-123", "2026-01-01T00:00:00Z", "dispatched"), "HandleDispatchReply")
	o, _ := db.GetOrder(oid)
	if o.Status != StatusInTransit {
		t.Errorf("Status: got %q, want %q", o.Status, StatusInTransit)
	}
	if o.WaybillID == nil || *o.WaybillID != "WB-123" {
		t.Errorf("WaybillID: %v, want WB-123", o.WaybillID)
	}
	if o.ETA == nil || *o.ETA != "2026-01-01T00:00:00Z" {
		t.Errorf("ETA: %v, want 2026-01-01T00:00:00Z", o.ETA)
	}
}

func TestHandleDispatchReply_Queued(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-q", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))

	testutil.MustNoErr(t, mgr.HandleDispatchReply("uuid-q", ReplyQueued, "", "", "awaiting inventory"), "HandleDispatchReply")
	o, _ := db.GetOrder(oid)
	if o.Status != StatusQueued {
		t.Errorf("Status: got %q, want %q", o.Status, StatusQueued)
	}
}

func TestHandleDispatchReply_Update_ETAOnlyDoesNotTouchWaybill(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-upd", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))
	_ = db.UpdateOrderStatus(oid, string(StatusAcknowledged))
	_ = db.UpdateOrderWaybill(oid, "WB-old", "OLD-ETA")
	_ = db.UpdateOrderStatus(oid, string(StatusInTransit))

	// waybillID arg is IGNORED on update — this confirms the non-touch.
	testutil.MustNoErr(t, mgr.HandleDispatchReply("uuid-upd", ReplyUpdate, "IGNORED-WB", "NEW-ETA", "status"), "HandleDispatchReply")
	o, _ := db.GetOrder(oid)
	if o.Status != StatusInTransit {
		t.Errorf("Status should be unchanged on update: got %q", o.Status)
	}
	if o.WaybillID == nil || *o.WaybillID != "WB-old" {
		t.Errorf("WaybillID should not be touched by update: %v", o.WaybillID)
	}
	if o.ETA == nil || *o.ETA != "NEW-ETA" {
		t.Errorf("ETA: %v, want NEW-ETA", o.ETA)
	}
}

func TestHandleDispatchReply_Update_EmptyETAIsNoop(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-upd2", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))
	_ = db.UpdateOrderWaybill(oid, "WB-stay", "ETA-stay")

	testutil.MustNoErr(t, mgr.HandleDispatchReply("uuid-upd2", ReplyUpdate, "", "", ""), "HandleDispatchReply")
	o, _ := db.GetOrder(oid)
	if o.ETA == nil || *o.ETA != "ETA-stay" {
		t.Errorf("ETA should remain: got %v", o.ETA)
	}
}

func TestHandleDispatchReply_Delivered_NoAutoConfirm(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-del", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))
	_ = db.UpdateOrderStatus(oid, string(StatusAcknowledged))
	_ = db.UpdateOrderStatus(oid, string(StatusInTransit))

	testutil.MustNoErr(t, mgr.HandleDispatchReply("uuid-del", ReplyDelivered, "", "", "delivered to line"), "HandleDispatchReply")
	o, _ := db.GetOrder(oid)
	if o.Status != StatusDelivered {
		t.Errorf("Status: got %q, want delivered", o.Status)
	}
	// No auto_confirm → no receipt enqueued.
	msgs, _ := db.ListPendingOutbox(10)
	for _, m := range msgs {
		if m.MsgType == protocol.TypeOrderReceipt {
			t.Errorf("unexpected receipt without auto_confirm")
		}
	}
}

func TestHandleDispatchReply_Delivered_AutoConfirmsAndQueuesReceipt(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	// auto_confirm=true.
	oid, _ := db.CreateOrder("uuid-ac", TypeRetrieve, nil, false, 2, "X", "", "", "", true, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))
	_ = db.UpdateOrderStatus(oid, string(StatusInTransit))

	testutil.MustNoErr(t, mgr.HandleDispatchReply("uuid-ac", ReplyDelivered, "", "", "delivered"), "HandleDispatchReply")
	o, _ := db.GetOrder(oid)
	if o.Status != StatusConfirmed {
		t.Errorf("Status: got %q, want confirmed (auto_confirm=true)", o.Status)
	}
	if o.FinalCount == nil || *o.FinalCount != 2 {
		t.Errorf("FinalCount: %v, want 2 (from order.Quantity)", o.FinalCount)
	}

	var r protocol.OrderReceipt
	decodeOnlyOutboxPayload(t, db, protocol.TypeOrderReceipt, &r)
	if r.OrderUUID != "uuid-ac" || r.ReceiptType != "confirmed" || r.FinalCount != 2 {
		t.Errorf("OrderReceipt: %+v", r)
	}
}

func TestHandleDispatchReply_Error_FailsAndEmits(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	emitter := &capturingEmitter{}
	mgr := NewManager(db, emitter, "edge")

	oid, _ := db.CreateOrder("uuid-err", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))

	testutil.MustNoErr(t, mgr.HandleDispatchReply("uuid-err", ReplyError, "", "", "rack offline"), "HandleDispatchReply")
	o, _ := db.GetOrder(oid)
	if o.Status != StatusFailed {
		t.Errorf("Status: got %q, want failed", o.Status)
	}
	if len(emitter.completed) == 0 {
		t.Error("expected completed event on terminal transition")
	}
	if len(emitter.failed) == 0 || !strings.Contains(emitter.failed[0], "rack offline") {
		t.Errorf("expected failed event carrying detail, got %v", emitter.failed)
	}
}

func TestHandleDispatchReply_Staged(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-stg", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))
	_ = db.UpdateOrderStatus(oid, string(StatusInTransit))

	testutil.MustNoErr(t, mgr.HandleDispatchReply("uuid-stg", ReplyStaged, "", "", "dwell"), "HandleDispatchReply")
	o, _ := db.GetOrder(oid)
	if o.Status != StatusStaged {
		t.Errorf("Status: got %q, want staged", o.Status)
	}
}

func TestHandleDispatchReply_Cancelled(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-can", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))

	testutil.MustNoErr(t, mgr.HandleDispatchReply("uuid-can", ReplyCancelled, "", "", "stopped"), "HandleDispatchReply")
	o, _ := db.GetOrder(oid)
	if o.Status != StatusCancelled {
		t.Errorf("Status: got %q, want cancelled", o.Status)
	}
}

func TestHandleDispatchReply_UnknownType(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	if _, err := db.CreateOrder("uuid-unk", TypeRetrieve, nil, false, 1, "X", "", "", "", false, ""); err != nil {
		t.Fatalf("create: %v", err)
	}

	err := mgr.HandleDispatchReply("uuid-unk", "mystery", "", "", "")
	if err == nil {
		t.Fatal("expected error for unknown reply type")
	}
	if !strings.Contains(err.Error(), "unknown reply type") {
		t.Errorf("error message: %v", err)
	}
}

func TestHandleDispatchReply_MissingOrder(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	err := mgr.HandleDispatchReply("nonexistent", ReplyAck, "", "", "")
	if err == nil {
		t.Fatal("expected error for missing order")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not found: %v", err)
	}
}

// ═══════════════════════════════════════════════════════════════════════
// PR 3.3 — ApplyCoreStatusSnapshot, lookupPayloadMeta, Release/Abort/
// Redirect, TransitionOrder, HandleDeliveredWithExpiry.
// ═══════════════════════════════════════════════════════════════════════

func TestApplyCoreStatusSnapshot_NotFoundIsNoop(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-nf", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")

	if err := mgr.ApplyCoreStatusSnapshot(protocol.OrderStatusSnapshot{
		OrderUUID: "uuid-nf", Found: false,
	}); err != nil {
		t.Fatalf("ApplyCoreStatusSnapshot: %v", err)
	}
	o, _ := db.GetOrder(oid)
	if o.Status != StatusPending {
		t.Errorf("Status should be unchanged: got %q", o.Status)
	}
}

func TestApplyCoreStatusSnapshot_SameStatusIsNoop(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-same", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))

	if err := mgr.ApplyCoreStatusSnapshot(protocol.OrderStatusSnapshot{
		OrderUUID: "uuid-same", Found: true, Status: string(StatusSubmitted),
	}); err != nil {
		t.Fatalf("ApplyCoreStatusSnapshot: %v", err)
	}
}

func TestApplyCoreStatusSnapshot_DeliveredToConfirmed_UsesNormalTransition(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-d2c", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))
	_ = db.UpdateOrderStatus(oid, string(StatusInTransit))
	_ = db.UpdateOrderStatus(oid, string(StatusDelivered))

	if err := mgr.ApplyCoreStatusSnapshot(protocol.OrderStatusSnapshot{
		OrderUUID: "uuid-d2c", Found: true, Status: string(StatusConfirmed),
	}); err != nil {
		t.Fatalf("ApplyCoreStatusSnapshot: %v", err)
	}
	o, _ := db.GetOrder(oid)
	if o.Status != StatusConfirmed {
		t.Errorf("Status: got %q, want confirmed", o.Status)
	}
}

func TestApplyCoreStatusSnapshot_ForceConfirmedFromNonDelivered(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-fcc", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))
	// submitted → confirmed is normally invalid; ForceTransition allows it.

	if err := mgr.ApplyCoreStatusSnapshot(protocol.OrderStatusSnapshot{
		OrderUUID: "uuid-fcc", Found: true, Status: string(StatusConfirmed),
	}); err != nil {
		t.Fatalf("ApplyCoreStatusSnapshot: %v", err)
	}
	o, _ := db.GetOrder(oid)
	if o.Status != StatusConfirmed {
		t.Errorf("Status: got %q, want confirmed (forced)", o.Status)
	}
}

func TestApplyCoreStatusSnapshot_AllForcedStatusPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		target string
	}{
		{"cancelled", string(StatusCancelled)},
		{"failed", string(StatusFailed)},
		{"delivered", string(StatusDelivered)},
		{"staged", string(StatusStaged)},
		{"in_transit", string(StatusInTransit)},
		{"acknowledged", string(StatusAcknowledged)},
		{"queued", string(StatusQueued)},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testManagerDB(t)
			mgr := NewManager(db, testEmitter{}, "edge")
			orderUUID := fmt.Sprintf("uuid-forced-%d", i)
			oid, err := db.CreateOrder(orderUUID, TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := mgr.ApplyCoreStatusSnapshot(protocol.OrderStatusSnapshot{
				OrderUUID: orderUUID, Found: true, Status: tc.target,
			}); err != nil {
				t.Fatalf("ApplyCoreStatusSnapshot: %v", err)
			}
			o, _ := db.GetOrder(oid)
			if string(o.Status) != tc.target {
				t.Errorf("Status: got %q, want %q", o.Status, tc.target)
			}
		})
	}
}

func TestApplyCoreStatusSnapshot_UnknownStatusIsNoop(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-weird", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")

	if err := mgr.ApplyCoreStatusSnapshot(protocol.OrderStatusSnapshot{
		OrderUUID: "uuid-weird", Found: true, Status: "something_weird",
	}); err != nil {
		t.Fatalf("ApplyCoreStatusSnapshot: %v", err)
	}
	o, _ := db.GetOrder(oid)
	if o.Status != StatusPending {
		t.Errorf("Status should be unchanged: got %q", o.Status)
	}
}

// TestApplyCoreStatusSnapshot_Reshuffling exercises the explicit
// Reshuffling arm added alongside the complex-order buried-reshuffle
// fix. Pre-fix, the snapshot switch's `default: return nil` silently
// dropped a Reshuffling status on edge reconciliation (e.g., after an
// edge restart while a compound was in flight), leaving the edge
// mirror stuck on a stale status.
func TestApplyCoreStatusSnapshot_Reshuffling(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-resh", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")

	if err := mgr.ApplyCoreStatusSnapshot(protocol.OrderStatusSnapshot{
		OrderUUID: "uuid-resh", Found: true, Status: string(StatusReshuffling),
	}); err != nil {
		t.Fatalf("ApplyCoreStatusSnapshot: %v", err)
	}
	o, _ := db.GetOrder(oid)
	if o.Status != StatusReshuffling {
		t.Errorf("Status: got %q, want reshuffling", o.Status)
	}
}

// TestApplyCoreStatusSnapshot_SimpleRetrieveReshuffleSurfaces is the
// pre-existing-bug regression test that lands alongside the new
// feature: a simple-retrieve order driven into Reshuffling on the
// core side must surface on the edge mirror after snapshot
// reconciliation.
func TestApplyCoreStatusSnapshot_SimpleRetrieveReshuffleSurfaces(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-simple-resh", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))

	if err := mgr.ApplyCoreStatusSnapshot(protocol.OrderStatusSnapshot{
		OrderUUID: "uuid-simple-resh", Found: true, Status: string(StatusReshuffling),
	}); err != nil {
		t.Fatalf("ApplyCoreStatusSnapshot: %v", err)
	}
	o, _ := db.GetOrder(oid)
	if o.Status != StatusReshuffling {
		t.Errorf("Status: got %q, want reshuffling", o.Status)
	}
}

func TestApplyCoreStatusSnapshot_MissingOrder(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	err := mgr.ApplyCoreStatusSnapshot(protocol.OrderStatusSnapshot{
		OrderUUID: "no-such-order", Found: true, Status: string(StatusConfirmed),
	})
	if err == nil {
		t.Fatal("expected error for missing order")
	}
}

// ═══════════════════════════════════════════════════════════════════════
// The Core→Edge status mapping (orders.ApplyCoreStatus).
//
// ApplyCoreStatus is the one function used by both the live-push path
// (HandleOrderUpdate) and the boot-reconcile path (ApplyCoreStatusSnapshot).
// It maps Core's pushed status vocabulary fully onto Edge — closing the gap
// where Edge previously discarded sourcing/dispatched/faulted and only
// branched on queued. These tests pin each arm.
// ═══════════════════════════════════════════════════════════════════════

// TestApplyCoreStatus_Sourcing_TransitionsRow asserts the central case: a
// sourcing push from Core updates the Edge row to StatusSourcing. Previously
// the push was discarded and the row stayed on acknowledged, so the HMI kept
// showing a moving robot for an order still hunting bins.
func TestApplyCoreStatus_Sourcing_TransitionsRow(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, err := db.CreateOrder("uuid-src", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	testutil.MustNoErr(t, err, "create")
	// Edge's typical pre-dispatch lifecycle: pending → submitted → acknowledged
	// (Core's intake ACK). acknowledged → sourcing is a valid shared-table edge.
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))
	_ = db.UpdateOrderStatus(oid, string(StatusAcknowledged))

	order, err := db.GetOrder(oid)
	testutil.MustNoErr(t, err, "get order")
	testutil.MustNoErr(t, mgr.ApplyCoreStatus(order, StatusSourcing, "reserving"), "ApplyCoreStatus sourcing")

	got, _ := db.GetOrder(oid)
	if got.Status != StatusSourcing {
		t.Errorf("sourcing push: status got %q, want %q", got.Status, StatusSourcing)
	}
}

// TestApplyCoreStatus_Faulted_TransitionsRow asserts that a faulted push sets
// StatusFaulted on Edge. Previously Edge had no writer for faulted and discarded
// it, leaving the amber UI affordances unreachable.
func TestApplyCoreStatus_Faulted_TransitionsRow(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-flt", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))
	_ = db.UpdateOrderStatus(oid, string(StatusAcknowledged))
	_ = db.UpdateOrderStatus(oid, string(StatusInTransit))

	order, _ := db.GetOrder(oid)
	testutil.MustNoErr(t, mgr.ApplyCoreStatus(order, StatusFaulted, "RDS FAILED"), "ApplyCoreStatus faulted")

	got, _ := db.GetOrder(oid)
	if got.Status != StatusFaulted {
		t.Errorf("faulted push: status got %q, want %q", got.Status, StatusFaulted)
	}
}

// TestApplyCoreStatus_Dispatched_TransitionsRow covers the dispatched arm.
// acknowledged → dispatched is a valid shared-table edge.
func TestApplyCoreStatus_Dispatched_TransitionsRow(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-disp", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))
	_ = db.UpdateOrderStatus(oid, string(StatusAcknowledged))

	order, _ := db.GetOrder(oid)
	testutil.MustNoErr(t, mgr.ApplyCoreStatus(order, StatusDispatched, "fleet created"), "ApplyCoreStatus dispatched")

	got, _ := db.GetOrder(oid)
	if got.Status != StatusDispatched {
		t.Errorf("dispatched push: status got %q, want %q", got.Status, StatusDispatched)
	}
}

// TestApplyCoreStatus_Queued_UnchangedBehavior pins that the queued arm keeps
// its existing behavior under the new mapping (the one status Edge branched on
// before this mapping existed).
func TestApplyCoreStatus_Queued_TransitionsRow(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-q2", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))

	order, _ := db.GetOrder(oid)
	testutil.MustNoErr(t, mgr.ApplyCoreStatus(order, StatusQueued, "awaiting inventory"), "ApplyCoreStatus queued")

	got, _ := db.GetOrder(oid)
	if got.Status != StatusQueued {
		t.Errorf("queued push: status got %q, want %q", got.Status, StatusQueued)
	}
}

// TestApplyCoreStatus_StagedDeliveredTerminal_Noop pins that staged/delivered/
// terminal statuses are NO-OPs in this mapping — dedicated envelopes
// (order.staged, order.delivered, etc.) own them, and the mapping must NOT
// double-handle. A staged push via the generic update path leaves the row alone
// (the dedicated OrderStaged envelope does the real write).
func TestApplyCoreStatus_StagedDeliveredTerminal_Noop(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	for _, tc := range []struct {
		name string
		to   protocol.Status
	}{
		{"staged", StatusStaged},
		{"delivered", StatusDelivered},
		{"confirmed", StatusConfirmed},
		{"failed", StatusFailed},
		{"cancelled", StatusCancelled},
		{"skipped", StatusSkipped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oid, _ := db.CreateOrder("uuid-noop-"+tc.name, TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
			_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))
			_ = db.UpdateOrderStatus(oid, string(StatusAcknowledged))
			_ = db.UpdateOrderStatus(oid, string(StatusInTransit))

			order, _ := db.GetOrder(oid)
			// These are intentionally no-op in the generic mapping (must not error).
			testutil.MustNoErr(t, mgr.ApplyCoreStatus(order, tc.to, "should noop"), "ApplyCoreStatus "+tc.name)

			got, _ := db.GetOrder(oid)
			if got.Status != StatusInTransit {
				t.Errorf("%s push via the generic mapping should be a no-op: got %q, want in_transit", tc.name, got.Status)
			}
		})
	}
}

// TestApplyCoreStatus_InvalidFromStatus_GracefulNoop pins that when the pushed
// status is not reachable from Edge's current status in the shared table (e.g.
// an in_transit order being told "queued"), the mapping must NOT hard-fail —
// it returns the error and the caller swallows it, matching the old discard
// behavior. 26 such (from,to) pairs exist by design; no table edges are added.
func TestApplyCoreStatus_InvalidFromStatus_GracefulNoop(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-inv", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))
	_ = db.UpdateOrderStatus(oid, string(StatusAcknowledged))
	_ = db.UpdateOrderStatus(oid, string(StatusInTransit))

	order, _ := db.GetOrder(oid)
	// in_transit → queued is NOT in the shared table. The mapping returns an
	// error but the row is untouched (graceful, not a hard failure).
	err := mgr.ApplyCoreStatus(order, StatusQueued, "late queue_reason")
	if err == nil {
		t.Fatal("expected invalid-transition error from in_transit→queued")
	}
	got, _ := db.GetOrder(oid)
	if got.Status != StatusInTransit {
		t.Errorf("invalid push should leave row untouched: got %q, want in_transit", got.Status)
	}
}

// TestHandleCoreStatusPush_Sourcing_LivePush exercises the live-push path: the
// path HandleOrderUpdate drives must apply a pushed sourcing status to the Edge
// row. HandleCoreStatusPush is the entry point for the live channel; previously
// only ReplyQueued changed status (HandleDispatchReply), so a sourcing push left
// the row untouched.
func TestHandleCoreStatusPush_Sourcing_LivePush(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-live-src", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))
	_ = db.UpdateOrderStatus(oid, string(StatusAcknowledged))

	testutil.MustNoErr(t, mgr.HandleCoreStatusPush("uuid-live-src", StatusSourcing, "reserving"), "HandleCoreStatusPush sourcing")

	got, _ := db.GetOrder(oid)
	if got.Status != StatusSourcing {
		t.Errorf("live sourcing push: status got %q, want sourcing", got.Status)
	}
}

// TestApplyCoreStatusSnapshot_SourcingAndFaulted exercises the snapshot path:
// after an Edge restart, a snapshot carrying sourcing/faulted reconciles the
// row (previously the default arm returned nil and the row was untouched).
func TestApplyCoreStatusSnapshot_SourcingAndFaulted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		target protocol.Status
	}{
		{"sourcing", StatusSourcing},
		{"faulted", StatusFaulted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testManagerDB(t)
			mgr := NewManager(db, testEmitter{}, "edge")

			oid, _ := db.CreateOrder("uuid-snap-"+tc.name, TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
			_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))
			_ = db.UpdateOrderStatus(oid, string(StatusAcknowledged))
			_ = db.UpdateOrderStatus(oid, string(StatusInTransit))

			if err := mgr.ApplyCoreStatusSnapshot(protocol.OrderStatusSnapshot{
				OrderUUID: "uuid-snap-" + tc.name, Found: true, Status: string(tc.target),
			}); err != nil {
				t.Fatalf("ApplyCoreStatusSnapshot %s: %v", tc.name, err)
			}
			got, _ := db.GetOrder(oid)
			if got.Status != tc.target {
				t.Errorf("snapshot %s: got %q, want %q", tc.name, got.Status, tc.target)
			}
		})
	}
}

// TestApplyCoreStatus_ForcesFleetStatusOverSkippedIntermediate pins the SPR 2399
// fix: a live fleet-status push force-adopts even when Core coalesced the burst
// and dropped the intermediate step. The edge, still at `sourcing`, must adopt
// in_transit — sourcing→in_transit is not a legal validated edge, and the old
// validated path rejected it and silently froze the mirror behind a stale status
// (which then hid the two-robot RELEASE, since swap_ready reads the stale status).
func TestApplyCoreStatus_ForcesFleetStatusOverSkippedIntermediate(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-2399", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	if err := db.UpdateOrderStatus(oid, string(StatusSourcing)); err != nil {
		t.Fatalf("seed sourcing: %v", err)
	}

	// Core dropped `dispatched` and pushes in_transit straight from sourcing.
	if err := mgr.HandleCoreStatusPush("uuid-2399", StatusInTransit, "fleet state: RUNNING"); err != nil {
		t.Fatalf("HandleCoreStatusPush in_transit: %v", err)
	}
	got, err := db.GetOrder(oid)
	testutil.MustNoErr(t, err, "get order")
	if got.Status != StatusInTransit {
		t.Fatalf("edge status = %q, want in_transit — the fleet arm must force-adopt over the dropped `dispatched` step, not reject and freeze", got.Status)
	}
}

// TestApplyCoreStatus_TerminalNotResurrected: a stale/out-of-order fleet push
// after the edge already reached a terminal state must be ignored — forcing it
// would resurrect a finished order.
func TestApplyCoreStatus_TerminalNotResurrected(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-term", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	if err := db.UpdateOrderStatus(oid, string(StatusCancelled)); err != nil {
		t.Fatalf("seed terminal: %v", err)
	}

	if err := mgr.HandleCoreStatusPush("uuid-term", StatusInTransit, "stale fleet push"); err != nil {
		t.Fatalf("HandleCoreStatusPush: %v", err)
	}
	got, err := db.GetOrder(oid)
	testutil.MustNoErr(t, err, "get order")
	if got.Status != StatusCancelled {
		t.Fatalf("terminal edge order moved to %q by a stale fleet push — must stay cancelled", got.Status)
	}
}

// ───────────────────────────────────────────────────────────────────────
// lookupPayloadMeta — exercised via CreateMoveOrder, which threads the
// (desc, code) pair into the OrderRequest envelope.
// ───────────────────────────────────────────────────────────────────────

func TestLookupPayloadMeta_NilProcessNode_NoLookup(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	if _, err := mgr.CreateMoveOrder(nil, 1, "S", "D", false); err != nil {
		t.Fatalf("CreateMoveOrder: %v", err)
	}
	var req protocol.OrderRequest
	decodeOnlyOutboxPayload(t, db, protocol.TypeOrderRequest, &req)
	if req.PayloadCode != "" || req.PayloadDesc != "" {
		t.Errorf("expected empty payload meta with nil node, got code=%q desc=%q",
			req.PayloadCode, req.PayloadDesc)
	}
}

func TestLookupPayloadMeta_ActiveStyleOnly(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	pid, sid, nid := seedProcessStyleNode(t, db, "P-LM", "S-LM", "CN-LM")
	_ = seedClaim(t, db, sid, "CN-LM", "PL-ACTIVE")
	active := sid
	testutil.MustNoErr(t, db.SetActiveStyle(pid, &active), "SetActiveStyle")

	if _, err := mgr.CreateMoveOrder(&nid, 1, "S", "D", false); err != nil {
		t.Fatalf("CreateMoveOrder: %v", err)
	}
	var req protocol.OrderRequest
	decodeOnlyOutboxPayload(t, db, protocol.TypeOrderRequest, &req)
	if req.PayloadCode != "PL-ACTIVE" {
		t.Errorf("PayloadCode: got %q, want PL-ACTIVE", req.PayloadCode)
	}
}

func TestLookupPayloadMeta_TargetStyleOverridesActiveDuringChangeover(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	pid, activeSid, nid := seedProcessStyleNode(t, db, "P-TS", "S-active", "CN-TS")
	_ = seedClaim(t, db, activeSid, "CN-TS", "PL-OLD")

	targetSid, err := db.CreateStyle("S-target", "", pid)
	if err != nil {
		t.Fatalf("CreateStyle target: %v", err)
	}
	_ = seedClaim(t, db, targetSid, "CN-TS", "PL-NEW")

	active := activeSid
	target := targetSid
	_ = db.SetActiveStyle(pid, &active)
	_ = db.SetTargetStyle(pid, &target)

	if _, err := mgr.CreateMoveOrder(&nid, 1, "S", "D", false); err != nil {
		t.Fatalf("CreateMoveOrder: %v", err)
	}
	var req protocol.OrderRequest
	decodeOnlyOutboxPayload(t, db, protocol.TypeOrderRequest, &req)
	if req.PayloadCode != "PL-NEW" {
		t.Errorf("PayloadCode should prefer target style during changeover: got %q, want PL-NEW", req.PayloadCode)
	}
}

func TestLookupPayloadMeta_NoActiveStyleFallsThrough(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	_, _, nid := seedProcessStyleNode(t, db, "P-NA", "S-NA", "CN-NA")
	// Don't set ActiveStyleID — lookup should early-return empty.

	if _, err := mgr.CreateMoveOrder(&nid, 1, "S", "D", false); err != nil {
		t.Fatalf("CreateMoveOrder: %v", err)
	}
	var req protocol.OrderRequest
	decodeOnlyOutboxPayload(t, db, protocol.TypeOrderRequest, &req)
	if req.PayloadCode != "" {
		t.Errorf("PayloadCode: got %q, want empty (no active style)", req.PayloadCode)
	}
}

// ───────────────────────────────────────────────────────────────────────
// Release / Abort / Redirect / Transition / Confirm happy paths + guards.
// ───────────────────────────────────────────────────────────────────────

func TestReleaseOrder_HappyPath(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-rel", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))
	_ = db.UpdateOrderStatus(oid, string(StatusInTransit))
	_ = db.UpdateOrderStatus(oid, string(StatusStaged))

	testutil.MustNoErr(t, mgr.ReleaseOrder(oid, nil, ""), "ReleaseOrder")
	o, _ := db.GetOrder(oid)
	if o.Status != StatusInTransit {
		t.Errorf("Status: got %q, want in_transit", o.Status)
	}

	var rel protocol.OrderRelease
	decodeOnlyOutboxPayload(t, db, protocol.TypeOrderRelease, &rel)
	if rel.OrderUUID != "uuid-rel" {
		t.Errorf("OrderRelease.OrderUUID: got %q", rel.OrderUUID)
	}
	if rel.RemainingUOP != nil {
		t.Errorf("OrderRelease.RemainingUOP: got %v, want nil (plain ReleaseOrder call)", rel.RemainingUOP)
	}
	if rel.CalledBy != "" {
		t.Errorf("OrderRelease.CalledBy: got %q, want empty (system caller)", rel.CalledBy)
	}
}

// TestReleaseOrder_ThreadsCalledBy verifies that calledBy lands on the
// envelope so Core's bin audit can record operator identity instead of
// always writing actor=system.
func TestReleaseOrder_ThreadsCalledBy(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-cb", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))
	_ = db.UpdateOrderStatus(oid, string(StatusInTransit))
	_ = db.UpdateOrderStatus(oid, string(StatusStaged))

	testutil.MustNoErr(t, mgr.ReleaseOrder(oid, nil, "stephen-station-7"), "ReleaseOrder")
	var rel protocol.OrderRelease
	decodeOnlyOutboxPayload(t, db, protocol.TypeOrderRelease, &rel)
	if rel.CalledBy != "stephen-station-7" {
		t.Errorf("OrderRelease.CalledBy: got %q, want %q", rel.CalledBy, "stephen-station-7")
	}
}

// TestReleaseOrder_ThreadsRemainingUOP verifies that a non-nil remainingUOP
// passed to ReleaseOrder lands on the OrderRelease envelope. Asserted at three
// values to cover Core's nil/zero/positive routing semantics in
// BinManifestService.SyncOrClearForReleased.
func TestReleaseOrder_ThreadsRemainingUOP(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		uop  *int
	}{
		{"nil_unchanged", nil},
		{"zero_clears", func() *int { z := 0; return &z }()},
		{"positive_syncs", func() *int { v := 800; return &v }()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testManagerDB(t)
			mgr := NewManager(db, testEmitter{}, "edge")

			oid, _ := db.CreateOrder("uuid-uop-"+tc.name, TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
			_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))
			_ = db.UpdateOrderStatus(oid, string(StatusInTransit))
			_ = db.UpdateOrderStatus(oid, string(StatusStaged))

			testutil.MustNoErr(t, mgr.ReleaseOrder(oid, tc.uop, ""), "ReleaseOrder")
			var rel protocol.OrderRelease
			decodeOnlyOutboxPayload(t, db, protocol.TypeOrderRelease, &rel)
			switch {
			case tc.uop == nil && rel.RemainingUOP != nil:
				t.Errorf("RemainingUOP: got %v, want nil", *rel.RemainingUOP)
			case tc.uop != nil && rel.RemainingUOP == nil:
				t.Errorf("RemainingUOP: got nil, want *%d", *tc.uop)
			case tc.uop != nil && rel.RemainingUOP != nil && *rel.RemainingUOP != *tc.uop:
				t.Errorf("RemainingUOP: got %d, want %d", *rel.RemainingUOP, *tc.uop)
			}
		})
	}
}

// TestReleaseOrder_PreDispatchSkip covers the post-2026-04-27 contract:
// ReleaseOrder no longer rejects non-staged orders. Pre-dispatch statuses
// (pending, submitted) are treated as silent no-ops because the consolidated
// release path fans out unconditionally and the sibling may legitimately be
// pre-dispatch in unusual timing scenarios. Manager logs a debug line and
// returns nil; the operator's intent is preserved (Order B's release fired)
// without aborting the whole call. Terminal statuses still error.
func TestReleaseOrder_PreDispatchSkip(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-rns", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))

	testutil.MustNoErr(t, mgr.ReleaseOrder(oid, nil, ""), "ReleaseOrder on pre-dispatch (submitted) should be a silent no-op, got")

	// Status didn't change — no envelope queued, no transition.
	o, _ := db.GetOrder(oid)
	if o.Status != StatusSubmitted {
		t.Errorf("status changed from submitted to %q; pre-dispatch release should be a no-op", o.Status)
	}
}

func TestReleaseOrder_MissingOrder(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	err := mgr.ReleaseOrder(99999, nil, "")
	if err == nil {
		t.Fatal("expected error for missing order")
	}
}

func TestAbortOrder_HappyPath_QueuesCancelAndTransitions(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	emitter := &capturingEmitter{}
	mgr := NewManager(db, emitter, "edge")

	oid, _ := db.CreateOrder("uuid-abrt", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))

	testutil.MustNoErr(t, mgr.AbortOrder(oid), "AbortOrder")
	o, _ := db.GetOrder(oid)
	if o.Status != StatusCancelled {
		t.Errorf("Status: got %q, want cancelled", o.Status)
	}

	var cancel protocol.OrderCancel
	decodeOnlyOutboxPayload(t, db, protocol.TypeOrderCancel, &cancel)
	if cancel.OrderUUID != "uuid-abrt" || cancel.Reason == "" {
		t.Errorf("OrderCancel: %+v", cancel)
	}
	if len(emitter.completed) == 0 {
		t.Error("expected completed event on terminal transition")
	}
}

func TestAbortOrder_RejectsTerminal(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-abrt-t", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusCancelled))

	err := mgr.AbortOrder(oid)
	if err == nil {
		t.Fatal("expected error aborting terminal order")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("error should mention terminal state: %v", err)
	}
}

func TestAbortOrder_MissingOrder(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	err := mgr.AbortOrder(99999)
	if err == nil {
		t.Fatal("expected error for missing order")
	}
}

func TestRedirectOrder_HappyPath_UpdatesDeliveryAndQueues(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-rd", TypeRetrieve, nil, false, 1, "OLD-LINE", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))

	o, err := mgr.RedirectOrder(oid, "NEW-LINE")
	if err != nil {
		t.Fatalf("RedirectOrder: %v", err)
	}
	if o.DeliveryNode != "NEW-LINE" {
		t.Errorf("DeliveryNode: got %q, want NEW-LINE", o.DeliveryNode)
	}
	var rd protocol.OrderRedirect
	decodeOnlyOutboxPayload(t, db, protocol.TypeOrderRedirect, &rd)
	if rd.NewDeliveryNode != "NEW-LINE" || rd.OrderUUID != "uuid-rd" {
		t.Errorf("OrderRedirect: %+v", rd)
	}
}

func TestRedirectOrder_RejectsTerminal(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-rdt", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusConfirmed))

	if _, err := mgr.RedirectOrder(oid, "Y"); err == nil {
		t.Fatal("expected error redirecting terminal order")
	}
}

func TestRedirectOrder_MissingOrder(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	if _, err := mgr.RedirectOrder(99999, "X"); err == nil {
		t.Fatal("expected error for missing order")
	}
}

func TestTransitionOrder_DelegatesToLifecycle(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-t", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	testutil.MustNoErr(t, mgr.TransitionOrder(oid, StatusSubmitted, "test"), "TransitionOrder")
	o, _ := db.GetOrder(oid)
	if o.Status != StatusSubmitted {
		t.Errorf("Status: got %q, want submitted", o.Status)
	}
}

func TestHandleDeliveredWithExpiry_StoresStagedExpireAt(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-he", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))
	_ = db.UpdateOrderStatus(oid, string(StatusInTransit))

	future := time.Now().UTC().Add(1 * time.Hour)
	testutil.MustNoErr(t, mgr.HandleDeliveredWithExpiry("uuid-he", "dwell", &future, nil, nil, 0, ""), "HandleDeliveredWithExpiry")
	o, _ := db.GetOrder(oid)
	if o.Status != StatusDelivered {
		t.Errorf("Status: got %q, want delivered", o.Status)
	}
	if o.StagedExpireAt == nil {
		t.Error("StagedExpireAt: got nil, want future timestamp")
	}
}

func TestHandleDeliveredWithExpiry_MissingOrder(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	err := mgr.HandleDeliveredWithExpiry("missing-uuid", "", nil, nil, nil, 0, "")
	if err == nil {
		t.Fatal("expected error for missing order")
	}
}

func TestConfirmDelivery_RequiresDelivered(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-cr", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))

	err := mgr.ConfirmDelivery(oid, 5)
	if err == nil {
		t.Fatal("expected error confirming non-delivered order")
	}
	if !strings.Contains(err.Error(), "delivered") {
		t.Errorf("error should mention delivered status: %v", err)
	}
}

func TestConfirmDelivery_HappyPath(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-ch", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, string(StatusSubmitted))
	_ = db.UpdateOrderStatus(oid, string(StatusInTransit))
	_ = db.UpdateOrderStatus(oid, string(StatusDelivered))

	testutil.MustNoErr(t, mgr.ConfirmDelivery(oid, 9), "ConfirmDelivery")
	o, _ := db.GetOrder(oid)
	if o.Status != StatusConfirmed {
		t.Errorf("Status: got %q, want confirmed", o.Status)
	}
	if o.FinalCount == nil || *o.FinalCount != 9 {
		t.Errorf("FinalCount: %v, want 9", o.FinalCount)
	}

	var r protocol.OrderReceipt
	decodeOnlyOutboxPayload(t, db, protocol.TypeOrderReceipt, &r)
	if r.FinalCount != 9 || r.ReceiptType != "confirmed" {
		t.Errorf("OrderReceipt: %+v", r)
	}
}

func TestSubmitOrder_RetrieveDoesNotBuildWaybill(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-so", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	testutil.MustNoErr(t, mgr.SubmitOrder(oid), "SubmitOrder")
	o, _ := db.GetOrder(oid)
	if o.Status != StatusSubmitted {
		t.Errorf("Status: got %q, want submitted", o.Status)
	}
}

func TestSubmitOrder_MissingOrder(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	if err := mgr.SubmitOrder(99999); err == nil {
		t.Fatal("expected error for missing order")
	}
}

// TestCreateComplexOrder_AutoConfirmSplit covers the Bug 2a fix
// (plant-test 2026-04-27 SMN_001 / SMN_002 teleport): Order B (evac) on
// a two-robot swap ends at the supermarket / outbound staging where no
// operator is present to press CONFIRM. Pre-fix, both legs were created
// with AutoConfirm=false, so Order B sat in `delivered` until something
// (admin page, global flag) advanced it — leaving a window during which
// the fulfillment scanner could re-claim the bin and have the late
// CONFIRMED clobber state.
//
// CreateComplexOrderWithAutoConfirm sets the flag to true so handleDelivered
// instantly transitions delivered → confirmed via Manager.ConfirmDelivery,
// closing the race window. CreateComplexOrder retains the manual-confirm
// default for lineside deliveries.
func TestCreateComplexOrder_AutoConfirmSplit(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	steps := []protocol.ComplexOrderStep{
		{Action: "pickup", Node: "LINE1"},
		{Action: "dropoff", Node: "AMRSM"},
	}

	manual, err := mgr.CreateComplexOrder(nil, 1, "LINE1", "LINE1", steps)
	if err != nil {
		t.Fatalf("CreateComplexOrder: %v", err)
	}
	if manual.AutoConfirm {
		t.Errorf("CreateComplexOrder: AutoConfirm=true, want false (lineside delivery requires operator press)")
	}

	auto, err := mgr.CreateComplexOrderWithAutoConfirm(nil, 1, "", "LINE1", steps)
	if err != nil {
		t.Fatalf("CreateComplexOrderWithAutoConfirm: %v", err)
	}
	if !auto.AutoConfirm {
		t.Errorf("CreateComplexOrderWithAutoConfirm: AutoConfirm=false, want true. " +
			"Bug 2a regression: evac legs to the supermarket would sit in delivered until " +
			"manually confirmed, re-opening the FINISHED→CONFIRMED race window where " +
			"the fulfillment scanner re-claims the bin and the late confirm teleports it back.")
	}
}
