package engine

import (
	"fmt"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/store/orders"
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

	order := &orders.Order{
		EdgeUUID:     edgeUUID,
		StationID:    req.StationID,
		OrderType:    "move",
		Status:       "pending",
		SourceNode:   sourceNode.Name,
		DeliveryNode: destNode.Name,
		Priority:     req.Priority,
		PayloadDesc:  req.Desc,
	}
	if err := e.db.CreateOrder(order); err != nil {
		return nil, fmt.Errorf("create order: %w", err)
	}
	if err := e.dispatcher.Lifecycle().MarkPending(order, req.Desc); err != nil {
		e.logFn("engine: mark direct order %d pending: %v", order.ID, err)
	}

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

// TerminateOrder cancels an order, unclaims any payloads, and emits a cancellation event.
func (e *Engine) TerminateOrder(orderID int64, actor string) error {
	order, err := e.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("order not found")
	}

	// Reject terminal AND post-delivery statuses. Once the bin is at the
	// destination (Delivered/Confirmed) or terminal, terminate is a no-op.
	if dispatch.IsPostDelivery(order.Status) || protocol.IsTerminal(order.Status) {
		return fmt.Errorf("cannot terminate order in status %q", order.Status)
	}

	// Route through lifecycle.CancelOrder for atomic transition + emit.
	// CancelOrder also cancels the vendor leg if active (no need to call
	// e.fleet.CancelOrder separately).
	e.dispatcher.Lifecycle().CancelOrder(order, order.StationID, "cancelled by "+actor)
	return nil
}

// failOrderAndEmit fails an order in the DB AND emits EventOrderFailed so the
// standard handler chain (audit, return order, edge notification) fires.
//
// Use this from any caller that previously did a bare db.FailOrderAtomic and
// would otherwise leave the order silently failed in the DB. The fulfillment
// scanner's structural-error path uses this to ensure scanner-driven failures
// reach Edge via the same notification pipeline as fleet-driven failures.
//
// Looks up StationID and EdgeUUID from the order so the EventOrderFailed
// payload is complete — without these fields populated, the wiring.go
// handler's notification gate skips the edge push.
func (e *Engine) failOrderAndEmit(orderID int64, errorCode, detail string) {
	order, err := e.db.GetOrder(orderID)
	if err != nil {
		e.logFn("engine: load order %d for fail: %v", orderID, err)
		return
	}
	// Route through lifecycle.Fail for atomic transition + emit.
	if err := e.dispatcher.Lifecycle().Fail(order, order.StationID, errorCode, detail); err != nil {
		e.logFn("engine: fail order %d (%s): %v", orderID, errorCode, err)
	}
}
