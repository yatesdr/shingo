package orders

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingoedge/store/orders"
)

// CreateRetrieveOrder creates a new retrieve order and enqueues it to the outbox.
// If payloadCode is empty and payloadID is set, it is derived from the payload.
//
// sourceNode names the supermarket node group (or specific node) that Core
// should pull the bin from. Empty string falls back to Core's global FIFO
// search (legacy behaviour). For bin_loader manual_swap claims this MUST be
// claim.InboundSource — otherwise the planner happily pulls a payload-matching
// empty/full bin from anywhere in the system, including the empty-tote return
// area instead of the configured supermarket. See planRetrieveEmpty in
// shingo-core/dispatch/planning_service.go for the resolver branch.
func (m *Manager) CreateRetrieveOrder(processNodeID *int64, retrieveEmpty bool, quantity int64, deliveryNode, sourceNode, stagingNode, loadType, payloadCode string, autoConfirm, skipAutoConfirm bool) (*orders.Order, error) {
	orderUUID := uuid.New().String()

	payloadDesc, payloadCode := m.lookupPayloadMeta(processNodeID, payloadCode)

	orderID, err := m.db.CreateOrder(orderUUID, TypeRetrieve,
		processNodeID, retrieveEmpty,
		quantity, deliveryNode, stagingNode, sourceNode, loadType, autoConfirm, payloadCode)
	if err != nil {
		return nil, fmt.Errorf("create order: %w", err)
	}

	env, envErr := m.sender.build(protocol.TypeOrderRequest, &protocol.OrderRequest{
		OrderUUID:       orderUUID,
		OrderType:       TypeRetrieve,
		PayloadDesc:     payloadDesc,
		PayloadCode:     payloadCode,
		RetrieveEmpty:   retrieveEmpty,
		Quantity:        quantity,
		DeliveryNode:    deliveryNode,
		SourceNode:      sourceNode,
		StagingNode:     stagingNode,
		LoadType:        loadType,
		SkipAutoConfirm: skipAutoConfirm,
	})
	m.enqueueAndAutoSubmit(orderID, orderUUID, env, envErr)

	m.DebugLog.Log("create: type=%s id=%d uuid=%s delivery=%s", TypeRetrieve, orderID, orderUUID, deliveryNode)
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeRetrieve, nil, processNodeID)

	return m.db.GetOrder(orderID)
}

// CreateMoveOrder creates a new move order (e.g., quality hold).
// autoConfirm threads through to the order row so Manager.handleDelivered
// can self-confirm instead of stranding the order at "delivered" when no
// operator station is wired up to confirm manually.
func (m *Manager) CreateMoveOrder(processNodeID *int64, quantity int64, sourceNode, deliveryNode string, autoConfirm bool) (*orders.Order, error) {
	orderUUID := uuid.New().String()

	payloadDesc, payloadCode := m.lookupPayloadMeta(processNodeID, "")

	orderID, err := m.db.CreateOrder(orderUUID, TypeMove,
		processNodeID, false,
		quantity, deliveryNode, "", sourceNode, "", autoConfirm, payloadCode)
	if err != nil {
		return nil, fmt.Errorf("create move order: %w", err)
	}

	env, envErr := m.sender.build(protocol.TypeOrderRequest, &protocol.OrderRequest{
		OrderUUID:    orderUUID,
		OrderType:    TypeMove,
		PayloadDesc:  payloadDesc,
		PayloadCode:  payloadCode,
		Quantity:     quantity,
		DeliveryNode: deliveryNode,
		SourceNode:   sourceNode,
	})
	m.enqueueAndAutoSubmit(orderID, orderUUID, env, envErr)

	m.DebugLog.Log("create: type=%s id=%d uuid=%s source=%s delivery=%s", TypeMove, orderID, orderUUID, sourceNode, deliveryNode)
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeMove, nil, processNodeID)
	return m.db.GetOrder(orderID)
}

// CreateMoveOrderWithPayloadCode is CreateMoveOrder with an explicit payload
// code instead of falling back to the active claim's primary payload. The
// manual_swap loader / unloader case needs this: a claim can list multiple
// allowed_payload_codes and the operator picks one at LoadBin time. Without
// threading that pick through to L2 / U2, the side-cycle move ends up tagged
// with claim.PayloadCode (the primary), and operator station tiles — which
// filter active orders by payload_code per card — show no in-transit state on
// the loaded payload's tile and may render unrelated tiles as queued via the
// no-payload-code fallback in operator-render.js / operator-modal.js.
func (m *Manager) CreateMoveOrderWithPayloadCode(processNodeID *int64, quantity int64, sourceNode, deliveryNode, payloadCode string, autoConfirm bool) (*orders.Order, error) {
	orderUUID := uuid.New().String()

	payloadDesc, payloadCode := m.lookupPayloadMeta(processNodeID, payloadCode)

	orderID, err := m.db.CreateOrder(orderUUID, TypeMove,
		processNodeID, false,
		quantity, deliveryNode, "", sourceNode, "", autoConfirm, payloadCode)
	if err != nil {
		return nil, fmt.Errorf("create move order: %w", err)
	}

	env, envErr := m.sender.build(protocol.TypeOrderRequest, &protocol.OrderRequest{
		OrderUUID:    orderUUID,
		OrderType:    TypeMove,
		PayloadDesc:  payloadDesc,
		PayloadCode:  payloadCode,
		Quantity:     quantity,
		DeliveryNode: deliveryNode,
		SourceNode:   sourceNode,
	})
	m.enqueueAndAutoSubmit(orderID, orderUUID, env, envErr)

	m.DebugLog.Log("create: type=%s id=%d uuid=%s source=%s delivery=%s payload=%s", TypeMove, orderID, orderUUID, sourceNode, deliveryNode, payloadCode)
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeMove, nil, processNodeID)
	return m.db.GetOrder(orderID)
}

// CreateMoveOrderWithUOP creates a move order and threads remainingUOP into the
// protocol envelope so Core can atomically clear/sync the bin manifest on claim.
// autoConfirm mirrors CreateMoveOrder so operator-initiated moves at a
// manual_swap node can self-confirm on delivery.
func (m *Manager) CreateMoveOrderWithUOP(processNodeID *int64, quantity int64, sourceNode, deliveryNode string, remainingUOP *int, autoConfirm bool) (*orders.Order, error) {
	orderUUID := uuid.New().String()

	payloadDesc, payloadCode := m.lookupPayloadMeta(processNodeID, "")

	orderID, err := m.db.CreateOrder(orderUUID, TypeMove,
		processNodeID, false,
		quantity, deliveryNode, "", sourceNode, "", autoConfirm, payloadCode)
	if err != nil {
		return nil, fmt.Errorf("create move order: %w", err)
	}

	env, envErr := m.sender.build(protocol.TypeOrderRequest, &protocol.OrderRequest{
		OrderUUID:    orderUUID,
		OrderType:    TypeMove,
		PayloadDesc:  payloadDesc,
		PayloadCode:  payloadCode,
		Quantity:     quantity,
		DeliveryNode: deliveryNode,
		SourceNode:   sourceNode,
		RemainingUOP: remainingUOP,
	})
	m.enqueueAndAutoSubmit(orderID, orderUUID, env, envErr)

	m.DebugLog.Log("create: type=%s id=%d uuid=%s source=%s delivery=%s remainingUOP=%v", TypeMove, orderID, orderUUID, sourceNode, deliveryNode, remainingUOP)
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeMove, nil, processNodeID)
	return m.db.GetOrder(orderID)
}

// CreateComplexOrder creates a new multi-step complex order and enqueues
// it to the outbox. The order is created with auto_confirm=false: it
// requires an operator HMI press to transition delivered → confirmed.
// Use this for deliveries whose destination is at the lineside, where an
// operator can inspect the bin. For deliveries to the supermarket /
// outbound staging (no operator present), use
// CreateComplexOrderWithAutoConfirm instead. deliveryNode is stored on
// the order for downstream logic (e.g., handleOrderCompleted uses it to
// determine which payload to reset on completion).
//
// processNodeName is the dot-name of the line node the order belongs to
// (typically claim.CoreNodeName). Threaded through to
// ComplexOrderRequest.ProcessNode so Core picks the line bin for
// order.BinID and targets it at release-time fallback. Pass "" when the
// order has no distinct line node — Core falls back to source-node
// behavior.
func (m *Manager) CreateComplexOrder(processNodeID *int64, quantity int64, deliveryNode, processNodeName string, steps []protocol.ComplexOrderStep) (*orders.Order, error) {
	return m.createComplexOrder(processNodeID, quantity, deliveryNode, processNodeName, steps, false, "", "")
}

// CreateComplexOrderWithAutoConfirm creates an auto-confirm complex order.
// Used for orders whose destination is the supermarket / outbound staging,
// where there is no operator to press CONFIRM. The order auto-transitions
// delivered → confirmed in handleDelivered the moment the fleet reports
// FINISHED, eliminating the FINISHED → CONFIRMED race window where the
// scanner can re-claim a delivered bin and the late confirm clobbers state
// (the SMN_001 / SMN_002 teleport bug, plant-test 2026-04-27).
func (m *Manager) CreateComplexOrderWithAutoConfirm(processNodeID *int64, quantity int64, deliveryNode, processNodeName string, steps []protocol.ComplexOrderStep) (*orders.Order, error) {
	return m.createComplexOrder(processNodeID, quantity, deliveryNode, processNodeName, steps, true, "", "")
}

func (m *Manager) CreateComplexOrderWithPayload(processNodeID *int64, quantity int64, deliveryNode, processNodeName string, steps []protocol.ComplexOrderStep, autoConfirm bool, payloadCode string) (*orders.Order, error) {
	return m.createComplexOrder(processNodeID, quantity, deliveryNode, processNodeName, steps, autoConfirm, payloadCode, "")
}

// CreateComplexOrderSibling creates a complex order and records the
// two-robot swap sibling UUID on the outbound ComplexOrderRequest, so Core
// can pair the legs at intake — before the removal leg's synchronous
// dispatch claims the line bin. siblingUUID is the *other* leg's edge UUID,
// or "" for non-swap / first-created legs.
func (m *Manager) CreateComplexOrderSibling(processNodeID *int64, quantity int64, deliveryNode, processNodeName string, steps []protocol.ComplexOrderStep, autoConfirm bool, payloadCode, siblingUUID string) (*orders.Order, error) {
	return m.createComplexOrder(processNodeID, quantity, deliveryNode, processNodeName, steps, autoConfirm, payloadCode, siblingUUID)
}

func (m *Manager) createComplexOrder(processNodeID *int64, quantity int64, deliveryNode, processNodeName string, steps []protocol.ComplexOrderStep, autoConfirm bool, payloadOverride, siblingUUID string) (*orders.Order, error) {
	orderUUID := uuid.New().String()

	stepsJSON, err := json.Marshal(steps)
	if err != nil {
		return nil, fmt.Errorf("marshal steps: %w", err)
	}

	payloadDesc, payloadCode := m.lookupPayloadMeta(processNodeID, payloadOverride)

	orderID, err := m.db.CreateOrder(orderUUID, TypeComplex,
		processNodeID, false,
		quantity, deliveryNode, "", "", "", autoConfirm, payloadCode)
	if err != nil {
		return nil, fmt.Errorf("create complex order: %w", err)
	}

	if err := m.db.UpdateOrderStepsJSON(orderID, string(stepsJSON)); err != nil {
		return nil, fmt.Errorf("store steps: %w", err)
	}

	env, envErr := m.sender.build(protocol.TypeComplexOrderRequest, &protocol.ComplexOrderRequest{
		OrderUUID:        orderUUID,
		PayloadCode:      payloadCode,
		PayloadDesc:      payloadDesc,
		Quantity:         quantity,
		ProcessNode:      processNodeName,
		Steps:            steps,
		SiblingOrderUUID: siblingUUID,
	})
	m.enqueueAndAutoSubmit(orderID, orderUUID, env, envErr)

	m.DebugLog.Log("create: type=%s id=%d uuid=%s steps=%d", TypeComplex, orderID, orderUUID, len(steps))
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeComplex, nil, processNodeID)

	return m.db.GetOrder(orderID)
}

// QueueIngestManifest sends a manifest-only ingest stamp to Core WITHOUT
// minting a local order. Swap-mode produce finalize uses this: the swap's
// complex order carries the bin, and the ingest exists only to stamp Core's
// bin manifest. Creating a local order there made a phantom that the
// operator-abort fan-out later cancelled, producing the "not_found" error —
// and Core's manifest-only ingest handler creates no order and sends no reply,
// so nothing ever matched it back. This mirrors CreateIngestOrder's envelope
// but ships it through the fire-and-forget Queue path (like ConfirmDelivery's
// receipt): no order row, no transition, no EmitOrderCreated. The stamp is
// still delivered (durable outbox, idempotent SetForProduction at Core).
// binID (0 = absent) pins the exact Core bin for the release-time produce
// manifest — see protocol.OrderIngestRequest.BinID.
func (m *Manager) QueueIngestManifest(payloadCode, binLabel string, binID int64, sourceNode string, quantity int64, manifest []protocol.IngestManifestItem, producedAt string) error {
	return m.sender.Queue(protocol.TypeOrderIngest, &protocol.OrderIngestRequest{
		OrderUUID:   uuid.New().String(),
		BinID:       binID,
		PayloadCode: payloadCode,
		BinLabel:    binLabel,
		SourceNode:  sourceNode,
		Quantity:    quantity,
		Manifest:    manifest,
		ProducedAt:  producedAt,
	})
}
