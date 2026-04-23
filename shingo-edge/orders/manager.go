package orders

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"shingo/protocol"
	"shingo/protocol/types"
	"shingoedge/store"

	"github.com/google/uuid"
)

// DebugLogFunc is a nil-safe debug logging function.
type DebugLogFunc = types.DebugLogFunc

// Manager handles the order lifecycle state machine.
type Manager struct {
	db        *store.DB
	emitter   EventEmitter
	stationID string
	lifecycle *LifecycleService
	sender    *OrderSender

	DebugLog DebugLogFunc
}

// NewManager creates an order manager.
func NewManager(db *store.DB, emitter EventEmitter, stationID string) *Manager {
	return &Manager{
		db:        db,
		emitter:   emitter,
		stationID: stationID,
		lifecycle: newLifecycleService(db, emitter, nil),
		sender:    newOrderSender(db, stationID),
	}
}

func (m *Manager) enqueueEnvelope(env *protocol.Envelope) error {
	return m.sender.enqueue(env)
}

// lookupPayloadMeta returns description and payload code from the active style
// node claim for the given process node. If processNodeID is nil or the lookup
// fails, returns empty strings. If payloadCode is already set, it is preserved.
// During changeover, prefers the target style over the active style so that
// newly created orders resolve the correct (new) payload.
func (m *Manager) lookupPayloadMeta(processNodeID *int64, payloadCode string) (desc, code string) {
	if processNodeID == nil {
		return "", payloadCode
	}
	node, err := m.db.GetProcessNode(*processNodeID)
	if err != nil {
		m.DebugLog.Log("process-node lookup failed: id=%d err=%v", *processNodeID, err)
		return "", payloadCode
	}
	process, err := m.db.GetProcess(node.ProcessID)
	if err != nil || process.ActiveStyleID == nil {
		return "", payloadCode
	}
	// During changeover, prefer the target style for payload resolution.
	// Orders created during changeover are typically for the new style's material.
	styleID := *process.ActiveStyleID
	if process.TargetStyleID != nil {
		if _, err := m.db.GetStyleNodeClaimByNode(*process.TargetStyleID, node.CoreNodeName); err == nil {
			styleID = *process.TargetStyleID
		}
	}
	claim, err := m.db.GetStyleNodeClaimByNode(styleID, node.CoreNodeName)
	if err != nil {
		m.DebugLog.Log("style-node-claim lookup failed: node=%s err=%v", node.CoreNodeName, err)
		return "", payloadCode
	}
	if payloadCode == "" {
		payloadCode = claim.PayloadCode
	}
	return claim.PayloadCode, payloadCode
}

// enqueueAndAutoSubmit enqueues a protocol envelope and transitions the order
// to submitted. Used by order types that auto-submit at creation (retrieve,
// move, complex, ingest). Store orders do NOT auto-submit — they wait for
// count confirmation.
//
// If the envelope fails to build or enqueue, the order stays in pending so
// the operator sees an actionable state rather than a stuck "submitted" order
// that Core never received.
func (m *Manager) enqueueAndAutoSubmit(orderID int64, orderUUID string, env *protocol.Envelope, envErr error) {
	if envErr != nil {
		log.Printf("ERROR: build envelope for order %s: %v (order stays pending)", orderUUID, envErr)
		m.DebugLog.Log("enqueue failed: order %s envelope build error: %v", orderUUID, envErr)
		return
	}
	if err := m.enqueueEnvelope(env); err != nil {
		log.Printf("ERROR: enqueue order %s: %v (order stays pending)", orderUUID, err)
		m.DebugLog.Log("enqueue failed: order %s outbox write error: %v", orderUUID, err)
		return
	}
	if err := m.TransitionOrder(orderID, StatusSubmitted, "auto-submitted at creation"); err != nil {
		log.Printf("auto-submit order %s: %v", orderUUID, err)
	}
}

// CreateRetrieveOrder creates a new retrieve order and enqueues it to the outbox.
// If payloadCode is empty and payloadID is set, it is derived from the payload.
func (m *Manager) CreateRetrieveOrder(processNodeID *int64, retrieveEmpty bool, quantity int64, deliveryNode, stagingNode, loadType, payloadCode string, autoConfirm bool) (*store.Order, error) {
	orderUUID := uuid.New().String()

	payloadDesc, payloadCode := m.lookupPayloadMeta(processNodeID, payloadCode)

	orderID, err := m.db.CreateOrder(orderUUID, TypeRetrieve,
		processNodeID, retrieveEmpty,
		quantity, deliveryNode, stagingNode, "", loadType, autoConfirm, payloadCode)
	if err != nil {
		return nil, fmt.Errorf("create order: %w", err)
	}

	env, envErr := m.sender.build(protocol.TypeOrderRequest, &protocol.OrderRequest{
		OrderUUID:     orderUUID,
		OrderType:     TypeRetrieve,
		PayloadDesc:   payloadDesc,
		PayloadCode:   payloadCode,
		RetrieveEmpty: retrieveEmpty,
		Quantity:      quantity,
		DeliveryNode:  deliveryNode,
		StagingNode:   stagingNode,
		LoadType:      loadType,
	})
	m.enqueueAndAutoSubmit(orderID, orderUUID, env, envErr)

	m.DebugLog.Log("create: type=%s id=%d uuid=%s delivery=%s", TypeRetrieve, orderID, orderUUID, deliveryNode)
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeRetrieve, nil, processNodeID)

	return m.db.GetOrder(orderID)
}

// CreateStoreOrder creates a new store order (for returning payloads to warehouse).
func (m *Manager) CreateStoreOrder(processNodeID *int64, quantity int64, sourceNode string) (*store.Order, error) {
	orderUUID := uuid.New().String()

	orderID, err := m.db.CreateOrder(orderUUID, TypeStore,
		processNodeID, false,
		quantity, "", "", sourceNode, "", false, "")
	if err != nil {
		return nil, fmt.Errorf("create store order: %w", err)
	}

	m.DebugLog.Log("create: type=%s id=%d uuid=%s source=%s", TypeStore, orderID, orderUUID, sourceNode)
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeStore, nil, processNodeID)
	return m.db.GetOrder(orderID)
}

// SubmitStoreOrder sets the final count, marks count as confirmed, and submits
// the store order in one atomic operation. This is the correct way to submit a
// store order when the count is known at creation time (e.g., from API).
func (m *Manager) SubmitStoreOrder(orderID int64, finalCount int64) error {
	if err := m.db.UpdateOrderFinalCount(orderID, finalCount, true); err != nil {
		return fmt.Errorf("set final count: %w", err)
	}
	return m.SubmitOrder(orderID)
}

// CreateMoveOrder creates a new move order (e.g., quality hold).
// autoConfirm threads through to the order row so Manager.handleDelivered
// can self-confirm instead of stranding the order at "delivered" when no
// operator station is wired up to confirm manually.
func (m *Manager) CreateMoveOrder(processNodeID *int64, quantity int64, sourceNode, deliveryNode string, autoConfirm bool) (*store.Order, error) {
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

// CreateMoveOrderWithUOP creates a move order and threads remainingUOP into the
// protocol envelope so Core can atomically clear/sync the bin manifest on claim.
// autoConfirm mirrors CreateMoveOrder so operator-initiated moves at a
// manual_swap node can self-confirm on delivery.
func (m *Manager) CreateMoveOrderWithUOP(processNodeID *int64, quantity int64, sourceNode, deliveryNode string, remainingUOP *int, autoConfirm bool) (*store.Order, error) {
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

// CreateComplexOrder creates a new multi-step complex order and enqueues it to the outbox.
// deliveryNode is stored on the order for downstream logic (e.g., handleOrderCompleted
// uses it to determine which payload to reset on completion).
func (m *Manager) CreateComplexOrder(processNodeID *int64, quantity int64, deliveryNode string, steps []protocol.ComplexOrderStep) (*store.Order, error) {
	orderUUID := uuid.New().String()

	stepsJSON, err := json.Marshal(steps)
	if err != nil {
		return nil, fmt.Errorf("marshal steps: %w", err)
	}

	payloadDesc, payloadCode := m.lookupPayloadMeta(processNodeID, "")

	orderID, err := m.db.CreateOrder(orderUUID, TypeComplex,
		processNodeID, false,
		quantity, deliveryNode, "", "", "", false, payloadCode)
	if err != nil {
		return nil, fmt.Errorf("create complex order: %w", err)
	}

	if err := m.db.UpdateOrderStepsJSON(orderID, string(stepsJSON)); err != nil {
		return nil, fmt.Errorf("store steps: %w", err)
	}

	env, envErr := m.sender.build(protocol.TypeComplexOrderRequest, &protocol.ComplexOrderRequest{
		OrderUUID:   orderUUID,
		PayloadCode: payloadCode,
		PayloadDesc: payloadDesc,
		Quantity:    quantity,
		Steps:       steps,
	})
	m.enqueueAndAutoSubmit(orderID, orderUUID, env, envErr)

	m.DebugLog.Log("create: type=%s id=%d uuid=%s steps=%d", TypeComplex, orderID, orderUUID, len(steps))
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeComplex, nil, processNodeID)

	return m.db.GetOrder(orderID)
}

// CreateIngestOrder creates a new ingest order for originating a payload on a bin at a produce station.
// producedAt is an RFC3339 timestamp from the Edge capturing the moment the operator finalized the bin.
func (m *Manager) CreateIngestOrder(processNodeID *int64, payloadCode, binLabel, sourceNode string, quantity int64, manifest []protocol.IngestManifestItem, autoConfirm bool, producedAt string) (*store.Order, error) {
	orderUUID := uuid.New().String()

	orderID, err := m.db.CreateOrder(orderUUID, TypeIngest,
		processNodeID, false,
		quantity, "", "", sourceNode, "", autoConfirm, payloadCode)
	if err != nil {
		return nil, fmt.Errorf("create ingest order: %w", err)
	}

	env, envErr := m.sender.build(protocol.TypeOrderIngest, &protocol.OrderIngestRequest{
		OrderUUID:   orderUUID,
		PayloadCode: payloadCode,
		BinLabel:    binLabel,
		SourceNode:  sourceNode,
		Quantity:    quantity,
		Manifest:    manifest,
		ProducedAt:  producedAt,
	})
	m.enqueueAndAutoSubmit(orderID, orderUUID, env, envErr)

	m.DebugLog.Log("create: type=%s id=%d uuid=%s payload=%s bin=%s", TypeIngest, orderID, orderUUID, payloadCode, binLabel)
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeIngest, nil, processNodeID)

	return m.db.GetOrder(orderID)
}

// ReleaseOrder sends a release message for a staged (dwelling) order.
//
// remainingUOP late-binds the bin's manifest at Core's release handler. Pass
// nil when no manifest change is intended (legacy/Order-A/produce paths). Pass
// &0 to mark the bin empty (NOTHING PULLED disposition). Pass &N (N>0) to
// preserve the manifest with a synced count (SEND PARTIAL BACK disposition).
// See protocol.OrderRelease and BinManifestService.SyncOrClearForReleased.
func (m *Manager) ReleaseOrder(orderID int64, remainingUOP *int) error {
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("get order: %w", err)
	}
	if order.Status != StatusStaged {
		return fmt.Errorf("order must be in staged status to release, got %s", order.Status)
	}

	if err := m.sender.Queue(protocol.TypeOrderRelease, &protocol.OrderRelease{
		OrderUUID:    order.UUID,
		RemainingUOP: remainingUOP,
	}); err != nil {
		return fmt.Errorf("enqueue release: %w", err)
	}

	// Transition Edge status to in_transit now that the robot is resuming.
	// Core won't send a dedicated in_transit message (TypeOrderUpdate ignores
	// status), so we transition locally to keep Edge in sync.
	if err := m.TransitionOrder(orderID, StatusInTransit, "released from staging"); err != nil {
		return fmt.Errorf("transition to in_transit: %w", err)
	}

	if remainingUOP != nil {
		m.DebugLog.Log("release: id=%d uuid=%s remaining_uop=%d", orderID, order.UUID, *remainingUOP)
	} else {
		m.DebugLog.Log("release: id=%d uuid=%s", orderID, order.UUID)
	}
	return nil
}

// HandleDeliveredWithExpiry processes a delivered reply with optional staged expiry.
func (m *Manager) HandleDeliveredWithExpiry(orderUUID, statusDetail string, stagedExpireAt *time.Time) error {
	order, err := m.db.GetOrderByUUID(orderUUID)
	if err != nil {
		return fmt.Errorf("order %s not found: %w", orderUUID, err)
	}
	return m.handleDelivered(order, statusDetail, stagedExpireAt)
}

func (m *Manager) handleDelivered(order *store.Order, statusDetail string, stagedExpireAt *time.Time) error {
	if err := m.lifecycle.HandleDelivered(order, statusDetail, stagedExpireAt); err != nil {
		return err
	}
	if order.AutoConfirm {
		m.DebugLog.Log("auto-confirm: id=%d uuid=%s qty=%d", order.ID, order.UUID, order.Quantity)
		return m.ConfirmDelivery(order.ID, order.Quantity)
	}
	return nil
}

// TransitionOrder moves an order to a new status with validation.
func (m *Manager) TransitionOrder(orderID int64, newStatus, detail string) error {
	m.lifecycle.debug = m.DebugLog
	return m.lifecycle.Transition(orderID, newStatus, detail)
}

// AbortOrder cancels a non-terminal order and enqueues a cancel message.
// The cancel message is enqueued BEFORE the local transition so that Core
// is guaranteed to receive the cancellation — preventing a robot from
// continuing to execute a cancelled order on the floor.
func (m *Manager) AbortOrder(orderID int64) error {
	m.DebugLog.Log("abort: id=%d", orderID)
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("get order: %w", err)
	}
	if IsTerminal(order.Status) {
		return fmt.Errorf("order is already in terminal state: %s", order.Status)
	}

	// Build and enqueue cancel message BEFORE transitioning locally.
	// If enqueue fails, the order stays in its current state so the
	// operator can retry rather than having a locally-cancelled order
	// with a robot still executing on the floor.
	const abortReason = "aborted by operator"
	if err := m.sender.Queue(protocol.TypeOrderCancel, &protocol.OrderCancel{
		OrderUUID: order.UUID,
		Reason:    abortReason,
	}); err != nil {
		return fmt.Errorf("enqueue cancel message: %w", err)
	}

	if err := m.TransitionOrder(orderID, StatusCancelled, abortReason); err != nil {
		return err
	}
	return nil
}

// RedirectOrder changes the delivery node of a non-terminal order and enqueues a redirect message.
// The envelope is built and enqueued before updating the local DB so that
// Core receives the redirect. If enqueue fails, the error is returned.
func (m *Manager) RedirectOrder(orderID int64, newDeliveryNode string) (*store.Order, error) {
	m.DebugLog.Log("redirect: id=%d new_delivery=%s", orderID, newDeliveryNode)
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return nil, fmt.Errorf("get order: %w", err)
	}
	if IsTerminal(order.Status) {
		return nil, fmt.Errorf("order is already in terminal state: %s", order.Status)
	}

	// Build and enqueue redirect message first. If this fails, don't
	// update local state — the operator can retry.
	if err := m.sender.Queue(protocol.TypeOrderRedirect, &protocol.OrderRedirect{
		OrderUUID:       order.UUID,
		NewDeliveryNode: newDeliveryNode,
	}); err != nil {
		return nil, fmt.Errorf("enqueue redirect: %w", err)
	}

	if err := m.db.UpdateOrderDeliveryNode(orderID, newDeliveryNode); err != nil {
		return nil, fmt.Errorf("update delivery node: %w", err)
	}

	return m.db.GetOrder(orderID)
}

// SubmitOrder transitions a pending order to submitted and enqueues it.
// For store orders, it also builds and enqueues the storage waybill.
func (m *Manager) SubmitOrder(orderID int64) error {
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return err
	}

	m.DebugLog.Log("submit: id=%d uuid=%s type=%s", orderID, order.UUID, order.OrderType)

	// For store orders, build and enqueue the waybill BEFORE transitioning.
	// If enqueue fails, the order stays pending so the operator can retry.
	if order.OrderType == TypeStore {
		var finalCount int64
		if order.FinalCount != nil {
			finalCount = *order.FinalCount
		}
		desc, _ := m.lookupPayloadMeta(order.ProcessNodeID, "")
		env, err := m.sender.build(protocol.TypeOrderStorageWaybill, &protocol.OrderStorageWaybill{
			OrderUUID:   order.UUID,
			OrderType:   TypeStore,
			PayloadDesc: desc,
			SourceNode:  order.SourceNode,
			FinalCount:  finalCount,
		})
		if err != nil {
			return fmt.Errorf("build storage waybill: %w", err)
		}
		if err := m.enqueueEnvelope(env); err != nil {
			return fmt.Errorf("enqueue storage waybill: %w", err)
		}
	}

	return m.TransitionOrder(orderID, StatusSubmitted, "submitted to dispatch")
}

// ConfirmDelivery sends a delivery receipt and transitions to confirmed.
func (m *Manager) ConfirmDelivery(orderID int64, finalCount int64) error {
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return err
	}

	if order.Status != StatusDelivered {
		return fmt.Errorf("order must be in delivered status to confirm, got %s", order.Status)
	}

	m.DebugLog.Log("confirm: id=%d uuid=%s count=%d", orderID, order.UUID, finalCount)

	if err := m.db.UpdateOrderFinalCount(orderID, finalCount, true); err != nil {
		return err
	}

	// Enqueue delivery receipt — failure is logged but does not block
	// the confirmation. The receipt is informational; Core tracks delivery
	// via its own fleet polling. The outbox will retry if Kafka is down.
	if err := m.sender.Queue(protocol.TypeOrderReceipt, &protocol.OrderReceipt{
		OrderUUID:   order.UUID,
		ReceiptType: "confirmed",
		FinalCount:  finalCount,
	}); err != nil {
		return fmt.Errorf("enqueue delivery receipt %s: %w", order.UUID, err)
	}

	return m.TransitionOrder(orderID, StatusConfirmed, fmt.Sprintf("confirmed with count %d", finalCount))
}

// HandleDispatchReply processes an inbound reply from central dispatch.
func (m *Manager) HandleDispatchReply(orderUUID, replyType, waybillID, eta, statusDetail string) error {
	m.DebugLog.Log("dispatch reply: uuid=%s type=%s", orderUUID, replyType)
	order, err := m.db.GetOrderByUUID(orderUUID)
	if err != nil {
		return fmt.Errorf("order %s not found: %w", orderUUID, err)
	}

	switch replyType {
	case ReplyAck:
		return m.TransitionOrder(order.ID, StatusAcknowledged, statusDetail)
	case ReplyWaybill:
		if err := m.db.UpdateOrderWaybill(order.ID, waybillID, eta); err != nil {
			return err
		}
		return m.TransitionOrder(order.ID, StatusInTransit, fmt.Sprintf("waybill %s, ETA %s", waybillID, eta))
	case ReplyQueued:
		// Order queued by Core — awaiting inventory
		return m.TransitionOrder(order.ID, StatusQueued, statusDetail)
	case ReplyUpdate:
		// Status update with ETA only — don't touch waybill_id.
		if eta != "" {
			if err := m.db.UpdateOrderETA(order.ID, eta); err != nil {
				return err
			}
		}
		return nil
	case ReplyDelivered:
		return m.handleDelivered(order, statusDetail, nil)
	case ReplyError:
		return m.TransitionOrder(order.ID, StatusFailed, statusDetail)
	case ReplyStaged:
		return m.TransitionOrder(order.ID, StatusStaged, statusDetail)
	case ReplyCancelled:
		return m.TransitionOrder(order.ID, StatusCancelled, statusDetail)
	default:
		return fmt.Errorf("unknown reply type: %s", replyType)
	}
}

// ApplyCoreStatusSnapshot reconciles a local order with Core's authoritative status.
func (m *Manager) ApplyCoreStatusSnapshot(snapshot protocol.OrderStatusSnapshot) error {
	m.lifecycle.debug = m.DebugLog
	return m.lifecycle.ApplyCoreStatusSnapshot(snapshot)
}

// TODO(dead-code): no callers as of 2026-04-17; verify before the next refactor.
func (m *Manager) forceTransitionOrder(orderID int64, newStatus, detail string) error {
	m.lifecycle.debug = m.DebugLog
	return m.lifecycle.ForceTransition(orderID, newStatus, detail)
}
