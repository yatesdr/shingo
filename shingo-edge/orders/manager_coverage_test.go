package orders

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/messaging"
	"shingoedge/store/processes"
)

// ═══════════════════════════════════════════════════════════════════════
// Coverage additions for orders.Manager.
//
// PR 3.1 — order creation family (CreateRetrieveOrder, CreateStoreOrder,
//   SubmitStoreOrder, CreateMoveOrder, CreateMoveOrderWithUOP,
//   CreateComplexOrder, CreateIngestOrder). Every happy path is asserted
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

func (e *capturingEmitter) EmitOrderCreated(orderID int64, orderUUID, orderType string, payloadID, processNodeID *int64) {
	e.created = append(e.created, orderType+":"+orderUUID)
}

func (e *capturingEmitter) EmitOrderStatusChanged(orderID int64, orderUUID, orderType, oldStatus, newStatus, eta string, payloadID, processNodeID *int64) {
	e.status = append(e.status, oldStatus+"→"+newStatus)
}

func (e *capturingEmitter) EmitOrderCompleted(orderID int64, orderUUID, orderType string, payloadID, processNodeID *int64) {
	e.completed = append(e.completed, orderType+":"+orderUUID)
}

func (e *capturingEmitter) EmitOrderFailed(orderID int64, orderUUID, orderType, reason string) {
	e.failed = append(e.failed, orderType+":"+reason)
}

// seedProcessStyleNode seeds a process, a style on it, and a process_node
// for the given core node name. Returns the ids needed for wiring.
func seedProcessStyleNode(t *testing.T, db *store.DB, procName, styleName, coreNode string) (processID, styleID, nodeID int64) {
	t.Helper()
	pid, err := db.CreateProcess(procName, "", "active_production", "", "", false)
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
		SwapMode:     "simple",
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
	if err := json.Unmarshal(matches[0].Payload, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Type != msgType {
		t.Fatalf("envelope.Type: got %q, want %q", env.Type, msgType)
	}
	if err := env.DecodePayload(target); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return env
}

// ═══════════════════════════════════════════════════════════════════════
// PR 3.1 — Order creation family.
// ═══════════════════════════════════════════════════════════════════════

func TestCreateRetrieveOrder_HappyPath(t *testing.T) {
	db := testManagerDB(t)
	emitter := &capturingEmitter{}
	mgr := NewManager(db, emitter, "edge.station")

	order, err := mgr.CreateRetrieveOrder(nil, false, 7, "LINE-1", "STAGE-1", "LOAD-A", "PL-42", false)
	if err != nil {
		t.Fatalf("CreateRetrieveOrder: %v", err)
	}
	if order.OrderType != TypeRetrieve {
		t.Errorf("OrderType: got %q, want %q", order.OrderType, TypeRetrieve)
	}
	if order.Status != StatusSubmitted {
		t.Errorf("Status: got %q, want %q (auto-submit)", order.Status, StatusSubmitted)
	}
	if order.Quantity != 7 || order.DeliveryNode != "LINE-1" || order.StagingNode != "STAGE-1" || order.LoadType != "LOAD-A" {
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
	if req.DeliveryNode != "LINE-1" || req.StagingNode != "STAGE-1" || req.LoadType != "LOAD-A" {
		t.Errorf("OrderRequest routing fields wrong: %+v", req)
	}

	if len(emitter.created) != 1 || !strings.HasPrefix(emitter.created[0], "retrieve:") {
		t.Errorf("created events: %v", emitter.created)
	}
	if len(emitter.status) == 0 || emitter.status[0] != StatusPending+"→"+StatusSubmitted {
		t.Errorf("status events: %v (want pending→submitted first)", emitter.status)
	}
}

func TestCreateRetrieveOrder_PayloadMetaFromStyleClaim(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	pid, sid, nid := seedProcessStyleNode(t, db, "P1", "S1", "CN-A")
	_ = seedClaim(t, db, sid, "CN-A", "PL-AUTO")
	active := sid
	if err := db.SetActiveStyle(pid, &active); err != nil {
		t.Fatalf("SetActiveStyle: %v", err)
	}

	order, err := mgr.CreateRetrieveOrder(&nid, false, 1, "LINE-1", "", "", "", false)
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
	if req.PayloadDesc != "PL-AUTO" {
		t.Errorf("envelope PayloadDesc: got %q, want PL-AUTO", req.PayloadDesc)
	}
}

func TestCreateStoreOrder_DoesNotAutoSubmit(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	order, err := mgr.CreateStoreOrder(nil, 0, "NODE-X")
	if err != nil {
		t.Fatalf("CreateStoreOrder: %v", err)
	}
	if order.OrderType != TypeStore {
		t.Errorf("OrderType: got %q, want %q", order.OrderType, TypeStore)
	}
	if order.Status != StatusPending {
		t.Errorf("Status: got %q, want %q (store orders wait for count)", order.Status, StatusPending)
	}
	if order.SourceNode != "NODE-X" {
		t.Errorf("SourceNode: got %q, want NODE-X", order.SourceNode)
	}
	msgs, _ := db.ListPendingOutbox(10)
	if len(msgs) != 0 {
		t.Errorf("outbox: got %d msgs, want 0 (store doesn't auto-submit at create)", len(msgs))
	}
}

func TestSubmitStoreOrder_EmitsWaybillAndTransitions(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	order, err := mgr.CreateStoreOrder(nil, 0, "SRC-NODE")
	if err != nil {
		t.Fatalf("CreateStoreOrder: %v", err)
	}

	if err := mgr.SubmitStoreOrder(order.ID, 42); err != nil {
		t.Fatalf("SubmitStoreOrder: %v", err)
	}

	updated, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if updated.Status != StatusSubmitted {
		t.Errorf("Status: got %q, want %q", updated.Status, StatusSubmitted)
	}
	if updated.FinalCount == nil || *updated.FinalCount != 42 {
		t.Errorf("FinalCount: %v, want 42", updated.FinalCount)
	}
	if !updated.CountConfirmed {
		t.Error("CountConfirmed: got false, want true")
	}

	var wb protocol.OrderStorageWaybill
	decodeOnlyOutboxPayload(t, db, protocol.TypeOrderStorageWaybill, &wb)
	if wb.OrderUUID != order.UUID || wb.SourceNode != "SRC-NODE" || wb.FinalCount != 42 {
		t.Errorf("waybill fields wrong: %+v", wb)
	}
}

func TestCreateMoveOrder_HappyPath(t *testing.T) {
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
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	steps := []protocol.ComplexOrderStep{
		{Action: "pickup", Node: "A"},
		{Action: "dropoff", Node: "B"},
	}
	order, err := mgr.CreateComplexOrder(nil, 2, "DEL-C", steps)
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

func TestCreateIngestOrder_QueuesIngestEnvelope(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	manifest := []protocol.IngestManifestItem{{PartNumber: "P1", Quantity: 2}}
	producedAt := time.Now().UTC().Format(time.RFC3339)

	order, err := mgr.CreateIngestOrder(nil, "PL-X", "BIN-1", "SRC-I", 10, manifest, true, producedAt)
	if err != nil {
		t.Fatalf("CreateIngestOrder: %v", err)
	}
	if order.OrderType != TypeIngest || order.Status != StatusSubmitted {
		t.Errorf("order: type=%q status=%q", order.OrderType, order.Status)
	}
	if order.PayloadCode != "PL-X" || !order.AutoConfirm {
		t.Errorf("payload_code=%q auto_confirm=%v", order.PayloadCode, order.AutoConfirm)
	}

	var req protocol.OrderIngestRequest
	decodeOnlyOutboxPayload(t, db, protocol.TypeOrderIngest, &req)
	if req.PayloadCode != "PL-X" || req.BinLabel != "BIN-1" || req.Quantity != 10 {
		t.Errorf("ingest envelope: %+v", req)
	}
	if len(req.Manifest) != 1 || req.Manifest[0].PartNumber != "P1" {
		t.Errorf("manifest: %+v", req.Manifest)
	}
	if req.ProducedAt != producedAt {
		t.Errorf("ProducedAt: got %q, want %q", req.ProducedAt, producedAt)
	}
}

// ═══════════════════════════════════════════════════════════════════════
// PR 3.2 — HandleDispatchReply 8-way switch + unknown + missing-order.
// ═══════════════════════════════════════════════════════════════════════

func TestHandleDispatchReply_Ack(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, err := db.CreateOrder("uuid-ack", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.UpdateOrderStatus(oid, StatusSubmitted); err != nil {
		t.Fatalf("set submitted: %v", err)
	}

	if err := mgr.HandleDispatchReply("uuid-ack", ReplyAck, "", "", "core ack"); err != nil {
		t.Fatalf("HandleDispatchReply: %v", err)
	}
	o, _ := db.GetOrder(oid)
	if o.Status != StatusAcknowledged {
		t.Errorf("Status: got %q, want %q", o.Status, StatusAcknowledged)
	}
}

func TestHandleDispatchReply_Waybill_PersistsIDAndETA(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-wb", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusSubmitted)
	_ = db.UpdateOrderStatus(oid, StatusAcknowledged)

	if err := mgr.HandleDispatchReply("uuid-wb", ReplyWaybill, "WB-123", "2026-01-01T00:00:00Z", "dispatched"); err != nil {
		t.Fatalf("HandleDispatchReply: %v", err)
	}
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
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-q", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusSubmitted)

	if err := mgr.HandleDispatchReply("uuid-q", ReplyQueued, "", "", "awaiting inventory"); err != nil {
		t.Fatalf("HandleDispatchReply: %v", err)
	}
	o, _ := db.GetOrder(oid)
	if o.Status != StatusQueued {
		t.Errorf("Status: got %q, want %q", o.Status, StatusQueued)
	}
}

func TestHandleDispatchReply_Update_ETAOnlyDoesNotTouchWaybill(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-upd", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusSubmitted)
	_ = db.UpdateOrderStatus(oid, StatusAcknowledged)
	_ = db.UpdateOrderWaybill(oid, "WB-old", "OLD-ETA")
	_ = db.UpdateOrderStatus(oid, StatusInTransit)

	// waybillID arg is IGNORED on update — this confirms the non-touch.
	if err := mgr.HandleDispatchReply("uuid-upd", ReplyUpdate, "IGNORED-WB", "NEW-ETA", "status"); err != nil {
		t.Fatalf("HandleDispatchReply: %v", err)
	}
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
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-upd2", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusSubmitted)
	_ = db.UpdateOrderWaybill(oid, "WB-stay", "ETA-stay")

	if err := mgr.HandleDispatchReply("uuid-upd2", ReplyUpdate, "", "", ""); err != nil {
		t.Fatalf("HandleDispatchReply: %v", err)
	}
	o, _ := db.GetOrder(oid)
	if o.ETA == nil || *o.ETA != "ETA-stay" {
		t.Errorf("ETA should remain: got %v", o.ETA)
	}
}

func TestHandleDispatchReply_Delivered_NoAutoConfirm(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-del", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusSubmitted)
	_ = db.UpdateOrderStatus(oid, StatusAcknowledged)
	_ = db.UpdateOrderStatus(oid, StatusInTransit)

	if err := mgr.HandleDispatchReply("uuid-del", ReplyDelivered, "", "", "delivered to line"); err != nil {
		t.Fatalf("HandleDispatchReply: %v", err)
	}
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
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	// auto_confirm=true.
	oid, _ := db.CreateOrder("uuid-ac", TypeRetrieve, nil, false, 2, "X", "", "", "", true, "")
	_ = db.UpdateOrderStatus(oid, StatusSubmitted)
	_ = db.UpdateOrderStatus(oid, StatusInTransit)

	if err := mgr.HandleDispatchReply("uuid-ac", ReplyDelivered, "", "", "delivered"); err != nil {
		t.Fatalf("HandleDispatchReply: %v", err)
	}
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
	db := testManagerDB(t)
	emitter := &capturingEmitter{}
	mgr := NewManager(db, emitter, "edge")

	oid, _ := db.CreateOrder("uuid-err", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusSubmitted)

	if err := mgr.HandleDispatchReply("uuid-err", ReplyError, "", "", "rack offline"); err != nil {
		t.Fatalf("HandleDispatchReply: %v", err)
	}
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
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-stg", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusSubmitted)
	_ = db.UpdateOrderStatus(oid, StatusInTransit)

	if err := mgr.HandleDispatchReply("uuid-stg", ReplyStaged, "", "", "dwell"); err != nil {
		t.Fatalf("HandleDispatchReply: %v", err)
	}
	o, _ := db.GetOrder(oid)
	if o.Status != StatusStaged {
		t.Errorf("Status: got %q, want staged", o.Status)
	}
}

func TestHandleDispatchReply_Cancelled(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-can", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusSubmitted)

	if err := mgr.HandleDispatchReply("uuid-can", ReplyCancelled, "", "", "stopped"); err != nil {
		t.Fatalf("HandleDispatchReply: %v", err)
	}
	o, _ := db.GetOrder(oid)
	if o.Status != StatusCancelled {
		t.Errorf("Status: got %q, want cancelled", o.Status)
	}
}

func TestHandleDispatchReply_UnknownType(t *testing.T) {
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
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-same", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusSubmitted)

	if err := mgr.ApplyCoreStatusSnapshot(protocol.OrderStatusSnapshot{
		OrderUUID: "uuid-same", Found: true, Status: StatusSubmitted,
	}); err != nil {
		t.Fatalf("ApplyCoreStatusSnapshot: %v", err)
	}
}

func TestApplyCoreStatusSnapshot_DeliveredToConfirmed_UsesNormalTransition(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-d2c", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusSubmitted)
	_ = db.UpdateOrderStatus(oid, StatusInTransit)
	_ = db.UpdateOrderStatus(oid, StatusDelivered)

	if err := mgr.ApplyCoreStatusSnapshot(protocol.OrderStatusSnapshot{
		OrderUUID: "uuid-d2c", Found: true, Status: StatusConfirmed,
	}); err != nil {
		t.Fatalf("ApplyCoreStatusSnapshot: %v", err)
	}
	o, _ := db.GetOrder(oid)
	if o.Status != StatusConfirmed {
		t.Errorf("Status: got %q, want confirmed", o.Status)
	}
}

func TestApplyCoreStatusSnapshot_ForceConfirmedFromNonDelivered(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-fcc", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusSubmitted)
	// submitted → confirmed is normally invalid; ForceTransition allows it.

	if err := mgr.ApplyCoreStatusSnapshot(protocol.OrderStatusSnapshot{
		OrderUUID: "uuid-fcc", Found: true, Status: StatusConfirmed,
	}); err != nil {
		t.Fatalf("ApplyCoreStatusSnapshot: %v", err)
	}
	o, _ := db.GetOrder(oid)
	if o.Status != StatusConfirmed {
		t.Errorf("Status: got %q, want confirmed (forced)", o.Status)
	}
}

func TestApplyCoreStatusSnapshot_AllForcedStatusPaths(t *testing.T) {
	cases := []struct {
		name   string
		target string
	}{
		{"cancelled", StatusCancelled},
		{"failed", StatusFailed},
		{"delivered", StatusDelivered},
		{"staged", StatusStaged},
		{"in_transit", StatusInTransit},
		{"acknowledged", StatusAcknowledged},
		{"queued", StatusQueued},
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
			if o.Status != tc.target {
				t.Errorf("Status: got %q, want %q", o.Status, tc.target)
			}
		})
	}
}

func TestApplyCoreStatusSnapshot_UnknownStatusIsNoop(t *testing.T) {
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

func TestApplyCoreStatusSnapshot_MissingOrder(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	err := mgr.ApplyCoreStatusSnapshot(protocol.OrderStatusSnapshot{
		OrderUUID: "no-such-order", Found: true, Status: StatusConfirmed,
	})
	if err == nil {
		t.Fatal("expected error for missing order")
	}
}

// ───────────────────────────────────────────────────────────────────────
// lookupPayloadMeta — exercised via CreateMoveOrder, which threads the
// (desc, code) pair into the OrderRequest envelope.
// ───────────────────────────────────────────────────────────────────────

func TestLookupPayloadMeta_NilProcessNode_NoLookup(t *testing.T) {
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
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	pid, sid, nid := seedProcessStyleNode(t, db, "P-LM", "S-LM", "CN-LM")
	_ = seedClaim(t, db, sid, "CN-LM", "PL-ACTIVE")
	active := sid
	if err := db.SetActiveStyle(pid, &active); err != nil {
		t.Fatalf("SetActiveStyle: %v", err)
	}

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
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-rel", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusSubmitted)
	_ = db.UpdateOrderStatus(oid, StatusInTransit)
	_ = db.UpdateOrderStatus(oid, StatusStaged)

	if err := mgr.ReleaseOrder(oid, nil, ""); err != nil {
		t.Fatalf("ReleaseOrder: %v", err)
	}
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
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-cb", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusSubmitted)
	_ = db.UpdateOrderStatus(oid, StatusInTransit)
	_ = db.UpdateOrderStatus(oid, StatusStaged)

	if err := mgr.ReleaseOrder(oid, nil, "stephen-station-7"); err != nil {
		t.Fatalf("ReleaseOrder: %v", err)
	}
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
			_ = db.UpdateOrderStatus(oid, StatusSubmitted)
			_ = db.UpdateOrderStatus(oid, StatusInTransit)
			_ = db.UpdateOrderStatus(oid, StatusStaged)

			if err := mgr.ReleaseOrder(oid, tc.uop, ""); err != nil {
				t.Fatalf("ReleaseOrder: %v", err)
			}
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
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-rns", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusSubmitted)

	if err := mgr.ReleaseOrder(oid, nil, ""); err != nil {
		t.Fatalf("ReleaseOrder on pre-dispatch (submitted) should be a silent no-op, got: %v", err)
	}

	// Status didn't change — no envelope queued, no transition.
	o, _ := db.GetOrder(oid)
	if o.Status != StatusSubmitted {
		t.Errorf("status changed from submitted to %q; pre-dispatch release should be a no-op", o.Status)
	}
}

func TestReleaseOrder_MissingOrder(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	err := mgr.ReleaseOrder(99999, nil, "")
	if err == nil {
		t.Fatal("expected error for missing order")
	}
}

func TestAbortOrder_HappyPath_QueuesCancelAndTransitions(t *testing.T) {
	db := testManagerDB(t)
	emitter := &capturingEmitter{}
	mgr := NewManager(db, emitter, "edge")

	oid, _ := db.CreateOrder("uuid-abrt", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusSubmitted)

	if err := mgr.AbortOrder(oid); err != nil {
		t.Fatalf("AbortOrder: %v", err)
	}
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
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-abrt-t", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusCancelled)

	err := mgr.AbortOrder(oid)
	if err == nil {
		t.Fatal("expected error aborting terminal order")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("error should mention terminal state: %v", err)
	}
}

func TestAbortOrder_MissingOrder(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	err := mgr.AbortOrder(99999)
	if err == nil {
		t.Fatal("expected error for missing order")
	}
}

func TestRedirectOrder_HappyPath_UpdatesDeliveryAndQueues(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-rd", TypeRetrieve, nil, false, 1, "OLD-LINE", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusSubmitted)

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
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-rdt", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusConfirmed)

	if _, err := mgr.RedirectOrder(oid, "Y"); err == nil {
		t.Fatal("expected error redirecting terminal order")
	}
}

func TestRedirectOrder_MissingOrder(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	if _, err := mgr.RedirectOrder(99999, "X"); err == nil {
		t.Fatal("expected error for missing order")
	}
}

func TestTransitionOrder_DelegatesToLifecycle(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-t", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	if err := mgr.TransitionOrder(oid, StatusSubmitted, "test"); err != nil {
		t.Fatalf("TransitionOrder: %v", err)
	}
	o, _ := db.GetOrder(oid)
	if o.Status != StatusSubmitted {
		t.Errorf("Status: got %q, want submitted", o.Status)
	}
}

func TestHandleDeliveredWithExpiry_StoresStagedExpireAt(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-he", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusSubmitted)
	_ = db.UpdateOrderStatus(oid, StatusInTransit)

	future := time.Now().UTC().Add(1 * time.Hour)
	if err := mgr.HandleDeliveredWithExpiry("uuid-he", "dwell", &future); err != nil {
		t.Fatalf("HandleDeliveredWithExpiry: %v", err)
	}
	o, _ := db.GetOrder(oid)
	if o.Status != StatusDelivered {
		t.Errorf("Status: got %q, want delivered", o.Status)
	}
	if o.StagedExpireAt == nil {
		t.Error("StagedExpireAt: got nil, want future timestamp")
	}
}

func TestHandleDeliveredWithExpiry_MissingOrder(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	err := mgr.HandleDeliveredWithExpiry("missing-uuid", "", nil)
	if err == nil {
		t.Fatal("expected error for missing order")
	}
}

func TestConfirmDelivery_RequiresDelivered(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-cr", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusSubmitted)

	err := mgr.ConfirmDelivery(oid, 5)
	if err == nil {
		t.Fatal("expected error confirming non-delivered order")
	}
	if !strings.Contains(err.Error(), "delivered") {
		t.Errorf("error should mention delivered status: %v", err)
	}
}

func TestConfirmDelivery_HappyPath(t *testing.T) {
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-ch", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	_ = db.UpdateOrderStatus(oid, StatusSubmitted)
	_ = db.UpdateOrderStatus(oid, StatusInTransit)
	_ = db.UpdateOrderStatus(oid, StatusDelivered)

	if err := mgr.ConfirmDelivery(oid, 9); err != nil {
		t.Fatalf("ConfirmDelivery: %v", err)
	}
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
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	oid, _ := db.CreateOrder("uuid-so", TypeRetrieve, nil, false, 1, "X", "", "", "", false, "")
	if err := mgr.SubmitOrder(oid); err != nil {
		t.Fatalf("SubmitOrder: %v", err)
	}
	o, _ := db.GetOrder(oid)
	if o.Status != StatusSubmitted {
		t.Errorf("Status: got %q, want submitted", o.Status)
	}
	msgs, _ := db.ListPendingOutbox(10)
	for _, m := range msgs {
		if m.MsgType == protocol.TypeOrderStorageWaybill {
			t.Errorf("unexpected storage waybill for retrieve SubmitOrder")
		}
	}
}

func TestSubmitOrder_MissingOrder(t *testing.T) {
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
	db := testManagerDB(t)
	mgr := NewManager(db, testEmitter{}, "edge")

	steps := []protocol.ComplexOrderStep{
		{Action: "pickup", Node: "LINE1"},
		{Action: "dropoff", Node: "AMRSM"},
	}

	manual, err := mgr.CreateComplexOrder(nil, 1, "LINE1", steps)
	if err != nil {
		t.Fatalf("CreateComplexOrder: %v", err)
	}
	if manual.AutoConfirm {
		t.Errorf("CreateComplexOrder: AutoConfirm=true, want false (lineside delivery requires operator press)")
	}

	auto, err := mgr.CreateComplexOrderWithAutoConfirm(nil, 1, "", steps)
	if err != nil {
		t.Fatalf("CreateComplexOrderWithAutoConfirm: %v", err)
	}
	if !auto.AutoConfirm {
		t.Errorf("CreateComplexOrderWithAutoConfirm: AutoConfirm=false, want true. "+
			"Bug 2a regression: evac legs to the supermarket would sit in delivered until "+
			"manually confirmed, re-opening the FINISHED→CONFIRMED race window where "+
			"the fulfillment scanner re-claims the bin and the late confirm teleports it back.")
	}
}
