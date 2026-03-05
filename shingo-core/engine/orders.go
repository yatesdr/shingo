package engine

import (
	"fmt"

	"shingocore/store"

	"github.com/google/uuid"
)

// DirectOrderRequest holds the parameters for creating a direct fleet order.
type DirectOrderRequest struct {
	FromNodeID int64
	ToNodeID   int64
	StationID  string
	Priority   int
	Desc       string
}

// DirectOrderResult holds the result of a successfully created direct order.
type DirectOrderResult struct {
	OrderID       int64
	VendorOrderID string
	FromNode      string
	ToNode        string
}

// CreateDirectOrder creates a transport order in the DB and dispatches it to the fleet.
func (e *Engine) CreateDirectOrder(req DirectOrderRequest) (*DirectOrderResult, error) {
	if req.FromNodeID == req.ToNodeID {
		return nil, fmt.Errorf("source and destination must be different")
	}

	sourceNode, err := e.db.GetNode(req.FromNodeID)
	if err != nil {
		return nil, fmt.Errorf("source node not found")
	}
	destNode, err := e.db.GetNode(req.ToNodeID)
	if err != nil {
		return nil, fmt.Errorf("destination node not found")
	}

	edgeUUID := req.StationID + "-" + uuid.New().String()[:8]

	order := &store.Order{
		EdgeUUID:     edgeUUID,
		StationID:    req.StationID,
		OrderType:    "move",
		Status:       "pending",
		PickupNode:   sourceNode.Name,
		DeliveryNode: destNode.Name,
		Priority:     req.Priority,
		PayloadDesc:  req.Desc,
	}
	if err := e.db.CreateOrder(order); err != nil {
		return nil, fmt.Errorf("create order: %w", err)
	}
	e.db.UpdateOrderStatus(order.ID, "pending", req.Desc)

	vendorOrderID, err := e.dispatcher.DispatchDirect(order, sourceNode, destNode)
	if err != nil {
		return nil, fmt.Errorf("fleet dispatch failed: %w", err)
	}

	return &DirectOrderResult{
		OrderID:       order.ID,
		VendorOrderID: vendorOrderID,
		FromNode:      sourceNode.Name,
		ToNode:        destNode.Name,
	}, nil
}

// TerminateOrder cancels an order, unclaims any instances, and emits a cancellation event.
func (e *Engine) TerminateOrder(orderID int64, actor string) error {
	order, err := e.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("order not found")
	}

	// Cancel vendor order if active
	if order.VendorOrderID != "" {
		if err := e.fleet.CancelOrder(order.VendorOrderID); err != nil {
			e.logFn("engine: cancel vendor order %s: %v", order.VendorOrderID, err)
		}
	}

	// Unclaim any instances held by this order
	e.db.UnclaimOrderInstances(orderID)

	detail := "cancelled by " + actor
	if err := e.db.UpdateOrderStatus(orderID, "cancelled", detail); err != nil {
		return fmt.Errorf("update status: %w", err)
	}

	e.Events.Emit(Event{
		Type: EventOrderCancelled,
		Payload: OrderCancelledEvent{
			OrderID:   order.ID,
			EdgeUUID:  order.EdgeUUID,
			StationID: order.StationID,
			Reason:    detail,
		},
	})

	return nil
}
