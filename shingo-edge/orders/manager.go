package orders

import (
	"encoding/json"
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/store"

	"github.com/google/uuid"
)

// Manager handles the order lifecycle state machine.
type Manager struct {
	db        *store.DB
	emitter   EventEmitter
	stationID string

	DebugLog func(string, ...any)
}

// NewManager creates an order manager.
func NewManager(db *store.DB, emitter EventEmitter, stationID string) *Manager {
	return &Manager{
		db:        db,
		emitter:   emitter,
		stationID: stationID,
	}
}

func (m *Manager) debug(format string, args ...any) {
	if fn := m.DebugLog; fn != nil {
		fn(format, args...)
	}
}

// enqueueEnvelope marshals a protocol envelope and enqueues it to the outbox.
func (m *Manager) enqueueEnvelope(env *protocol.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	if _, err := m.db.EnqueueOutbox(data, env.Type); err != nil {
		return fmt.Errorf("enqueue %s: %w", env.Type, err)
	}
	return nil
}

func (m *Manager) src() protocol.Address {
	return protocol.Address{Role: protocol.RoleEdge, Station: m.stationID}
}

func (m *Manager) dst() protocol.Address {
	return protocol.Address{Role: protocol.RoleCore}
}

// CreateRetrieveOrder creates a new retrieve order and enqueues it to the outbox.
func (m *Manager) CreateRetrieveOrder(payloadID *int64, retrieveEmpty bool, quantity float64, deliveryNode, stagingNode, loadType string, autoConfirm bool) (*store.Order, error) {
	orderUUID := uuid.New().String()

	orderID, err := m.db.CreateOrder(orderUUID, TypeRetrieve,
		payloadID, retrieveEmpty,
		quantity, deliveryNode, stagingNode, "", loadType, autoConfirm)
	if err != nil {
		return nil, fmt.Errorf("create order: %w", err)
	}

	// Look up payload description for message
	var payloadDesc string
	if payloadID != nil {
		if p, err := m.db.GetPayload(*payloadID); err == nil {
			payloadDesc = p.Description
		}
	}

	// Build and enqueue protocol envelope
	env, err := protocol.NewEnvelope(protocol.TypeOrderRequest, m.src(), m.dst(), &protocol.OrderRequest{
		OrderUUID:     orderUUID,
		OrderType:     TypeRetrieve,
		PayloadDesc:   payloadDesc,
		RetrieveEmpty: retrieveEmpty,
		Quantity:      quantity,
		DeliveryNode:  deliveryNode,
		StagingNode:   stagingNode,
		LoadType:      loadType,
	})
	if err != nil {
		log.Printf("build envelope for order %s: %v", orderUUID, err)
	} else if err := m.enqueueEnvelope(env); err != nil {
		log.Printf("enqueue order %s: %v", orderUUID, err)
	}

	// Auto-submit: envelope is already enqueued, so advance local state to
	// match so that the ack from core (submitted → acknowledged) is valid.
	if err := m.TransitionOrder(orderID, StatusSubmitted, "auto-submitted at creation"); err != nil {
		log.Printf("auto-submit retrieve order %s: %v", orderUUID, err)
	}

	m.debug("create: type=%s id=%d uuid=%s delivery=%s", TypeRetrieve, orderID, orderUUID, deliveryNode)
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeRetrieve)

	return m.db.GetOrder(orderID)
}

// CreateStoreOrder creates a new store order (for returning payloads to warehouse).
func (m *Manager) CreateStoreOrder(payloadID *int64, quantity float64, pickupNode string) (*store.Order, error) {
	orderUUID := uuid.New().String()

	orderID, err := m.db.CreateOrder(orderUUID, TypeStore,
		payloadID, false,
		quantity, "", "", pickupNode, "", false)
	if err != nil {
		return nil, fmt.Errorf("create store order: %w", err)
	}

	m.debug("create: type=%s id=%d uuid=%s pickup=%s", TypeStore, orderID, orderUUID, pickupNode)
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeStore)
	return m.db.GetOrder(orderID)
}

// CreateMoveOrder creates a new move order (e.g., quality hold).
func (m *Manager) CreateMoveOrder(payloadID *int64, quantity float64, pickupNode, deliveryNode string) (*store.Order, error) {
	orderUUID := uuid.New().String()

	orderID, err := m.db.CreateOrder(orderUUID, TypeMove,
		payloadID, false,
		quantity, deliveryNode, "", pickupNode, "", false)
	if err != nil {
		return nil, fmt.Errorf("create move order: %w", err)
	}

	// Look up payload description for message
	var payloadDesc string
	if payloadID != nil {
		if p, err := m.db.GetPayload(*payloadID); err == nil {
			payloadDesc = p.Description
		}
	}

	// Build and enqueue protocol envelope
	env, err := protocol.NewEnvelope(protocol.TypeOrderRequest, m.src(), m.dst(), &protocol.OrderRequest{
		OrderUUID:    orderUUID,
		OrderType:    TypeMove,
		PayloadDesc:  payloadDesc,
		Quantity:     quantity,
		DeliveryNode: deliveryNode,
		PickupNode:   pickupNode,
	})
	if err != nil {
		log.Printf("build envelope for move order %s: %v", orderUUID, err)
	} else if err := m.enqueueEnvelope(env); err != nil {
		log.Printf("enqueue move order %s: %v", orderUUID, err)
	}

	// Auto-submit: envelope is already enqueued, so advance local state to
	// match so that the ack from core (submitted → acknowledged) is valid.
	if err := m.TransitionOrder(orderID, StatusSubmitted, "auto-submitted at creation"); err != nil {
		log.Printf("auto-submit move order %s: %v", orderUUID, err)
	}

	m.debug("create: type=%s id=%d uuid=%s pickup=%s delivery=%s", TypeMove, orderID, orderUUID, pickupNode, deliveryNode)
	m.emitter.EmitOrderCreated(orderID, orderUUID, TypeMove)
	return m.db.GetOrder(orderID)
}

// TransitionOrder moves an order to a new status with validation.
func (m *Manager) TransitionOrder(orderID int64, newStatus, detail string) error {
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("get order: %w", err)
	}

	if !IsValidTransition(order.Status, newStatus) {
		return fmt.Errorf("invalid transition from %s to %s", order.Status, newStatus)
	}

	// Store orders require count confirmation before submitting
	if order.OrderType == TypeStore && newStatus == StatusSubmitted && !order.CountConfirmed {
		return fmt.Errorf("store order requires count confirmation before submitting")
	}

	oldStatus := order.Status
	m.debug("transition: id=%d uuid=%s %s->%s", orderID, order.UUID, oldStatus, newStatus)
	if err := m.db.UpdateOrderStatus(orderID, newStatus); err != nil {
		return fmt.Errorf("update order status: %w", err)
	}
	if err := m.db.InsertOrderHistory(orderID, oldStatus, newStatus, detail); err != nil {
		log.Printf("insert order history: %v", err)
	}

	// Re-read to pick up any ETA set before transition (e.g. waybill)
	updated, _ := m.db.GetOrder(orderID)
	eta := ""
	if updated != nil && updated.ETA != nil {
		eta = *updated.ETA
	}
	m.emitter.EmitOrderStatusChanged(orderID, order.UUID, order.OrderType, oldStatus, newStatus, eta)

	if IsTerminal(newStatus) {
		m.emitter.EmitOrderCompleted(orderID, order.UUID, order.OrderType)
		if newStatus == StatusFailed {
			m.emitter.EmitOrderFailed(orderID, order.UUID, order.OrderType, detail)
		}
	}

	return nil
}

// AbortOrder cancels a non-terminal order and enqueues a cancel message.
func (m *Manager) AbortOrder(orderID int64) error {
	m.debug("abort: id=%d", orderID)
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("get order: %w", err)
	}
	if IsTerminal(order.Status) {
		return fmt.Errorf("order is already in terminal state: %s", order.Status)
	}

	if err := m.TransitionOrder(orderID, StatusCancelled, "aborted by operator"); err != nil {
		return err
	}

	env, err := protocol.NewEnvelope(protocol.TypeOrderCancel, m.src(), m.dst(), &protocol.OrderCancel{
		OrderUUID: order.UUID,
		Reason:    "aborted by operator",
	})
	if err != nil {
		log.Printf("build cancel envelope for order %s: %v", order.UUID, err)
	} else if err := m.enqueueEnvelope(env); err != nil {
		log.Printf("enqueue order cancel %s: %v", order.UUID, err)
	}

	return nil
}

// RedirectOrder changes the delivery node of a non-terminal order and enqueues a redirect message.
func (m *Manager) RedirectOrder(orderID int64, newDeliveryNode string) (*store.Order, error) {
	m.debug("redirect: id=%d new_delivery=%s", orderID, newDeliveryNode)
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return nil, fmt.Errorf("get order: %w", err)
	}
	if IsTerminal(order.Status) {
		return nil, fmt.Errorf("order is already in terminal state: %s", order.Status)
	}

	if err := m.db.UpdateOrderDeliveryNode(orderID, newDeliveryNode); err != nil {
		return nil, fmt.Errorf("update delivery node: %w", err)
	}

	env, err := protocol.NewEnvelope(protocol.TypeOrderRedirect, m.src(), m.dst(), &protocol.OrderRedirect{
		OrderUUID:       order.UUID,
		NewDeliveryNode: newDeliveryNode,
	})
	if err != nil {
		log.Printf("build redirect envelope for order %s: %v", order.UUID, err)
	} else if err := m.enqueueEnvelope(env); err != nil {
		log.Printf("enqueue redirect %s: %v", order.UUID, err)
	}

	return m.db.GetOrder(orderID)
}

// SubmitOrder transitions a pending order to submitted and enqueues it.
func (m *Manager) SubmitOrder(orderID int64) error {
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return err
	}

	if err := m.TransitionOrder(orderID, StatusSubmitted, "submitted to dispatch"); err != nil {
		return err
	}

	// For store orders, build and enqueue the storage waybill
	if order.OrderType == TypeStore {
		var finalCount float64
		if order.FinalCount != nil {
			finalCount = *order.FinalCount
		}
		env, err := protocol.NewEnvelope(protocol.TypeOrderStorageWaybill, m.src(), m.dst(), &protocol.OrderStorageWaybill{
			OrderUUID:   order.UUID,
			OrderType:   TypeStore,
			PayloadDesc: order.PayloadDesc,
			PickupNode:  order.PickupNode,
			FinalCount:  finalCount,
		})
		if err != nil {
			log.Printf("build storage waybill envelope %s: %v", order.UUID, err)
		} else if err := m.enqueueEnvelope(env); err != nil {
			log.Printf("enqueue storage waybill %s: %v", order.UUID, err)
		}
	}

	return nil
}

// ConfirmDelivery sends a delivery receipt and transitions to confirmed.
func (m *Manager) ConfirmDelivery(orderID int64, finalCount float64) error {
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		return err
	}

	if order.Status != StatusDelivered {
		return fmt.Errorf("order must be in delivered status to confirm, got %s", order.Status)
	}

	// Update final count
	if err := m.db.UpdateOrderFinalCount(orderID, finalCount, true); err != nil {
		return err
	}

	// Enqueue delivery receipt
	env, err := protocol.NewEnvelope(protocol.TypeOrderReceipt, m.src(), m.dst(), &protocol.OrderReceipt{
		OrderUUID:   order.UUID,
		ReceiptType: "confirmed",
		FinalCount:  finalCount,
	})
	if err != nil {
		log.Printf("build receipt envelope for order %s: %v", order.UUID, err)
	} else if err := m.enqueueEnvelope(env); err != nil {
		log.Printf("enqueue delivery receipt %s: %v", order.UUID, err)
	}

	return m.TransitionOrder(orderID, StatusConfirmed, fmt.Sprintf("confirmed with count %.0f", finalCount))
}

// HandleDispatchReply processes an inbound reply from central dispatch.
func (m *Manager) HandleDispatchReply(orderUUID, replyType, waybillID, eta, statusDetail string) error {
	m.debug("dispatch reply: uuid=%s type=%s", orderUUID, replyType)
	order, err := m.db.GetOrderByUUID(orderUUID)
	if err != nil {
		return fmt.Errorf("order %s not found: %w", orderUUID, err)
	}

	switch replyType {
	case "ack":
		return m.TransitionOrder(order.ID, StatusAcknowledged, statusDetail)
	case "waybill":
		if err := m.db.UpdateOrderWaybill(order.ID, waybillID, eta); err != nil {
			return err
		}
		return m.TransitionOrder(order.ID, StatusInTransit, fmt.Sprintf("waybill %s, ETA %s", waybillID, eta))
	case "update":
		// Status update with ETA only — don't touch waybill_id.
		if eta != "" {
			if err := m.db.UpdateOrderETA(order.ID, eta); err != nil {
				return err
			}
		}
		return nil
	case "delivered":
		if err := m.TransitionOrder(order.ID, StatusDelivered, statusDetail); err != nil {
			return err
		}
		// Auto-confirm if enabled
		if order.AutoConfirm {
			return m.ConfirmDelivery(order.ID, order.Quantity)
		}
		return nil
	case "error":
		return m.TransitionOrder(order.ID, StatusFailed, statusDetail)
	case "cancelled":
		return m.TransitionOrder(order.ID, StatusCancelled, statusDetail)
	default:
		return fmt.Errorf("unknown reply type: %s", replyType)
	}
}

