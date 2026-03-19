package engine

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/store"
)

// OrderRequestResult is returned by RequestOrders to both the event handler
// and the API handler. CycleMode tells the operator canvas how to render.
type OrderRequestResult struct {
	CycleMode string       `json:"cycle_mode"` // "sequential", "two_robot", "single_robot"
	Resupply  *store.Order `json:"resupply"`
	Removal   *store.Order `json:"removal,omitempty"`
	PayloadID int64        `json:"payload_id"`
}

// RequestOrders creates the appropriate order(s) for a payload based on its
// cycle mode. Called by both handlePayloadReorder (automatic, triggered by
// counter delta) and apiSmartRequest (manual, operator button).
func (e *Engine) RequestOrders(payloadID int64, quantity int64) (*OrderRequestResult, error) {
	payload, err := e.db.GetPayload(payloadID)
	if err != nil {
		return nil, fmt.Errorf("payload %d not found: %w", payloadID, err)
	}

	mode := payload.CycleMode
	if mode == "" {
		mode = "sequential"
	}

	switch mode {
	case "single_robot":
		return e.requestSingleRobot(payload, payloadID, quantity)
	case "two_robot":
		return e.requestTwoRobot(payload, payloadID, quantity)
	default:
		return e.requestSequential(payload, payloadID, quantity)
	}
}

func (e *Engine) requestSingleRobot(payload *store.Payload, payloadID int64, quantity int64) (*OrderRequestResult, error) {
	steps := []protocol.ComplexOrderStep{
		// 1. Pickup full bin from source
		buildPickupStep(payload.FullPickupNode, payload.FullPickupNodeGroup),
		// 2. Drop full at staging 1
		buildDropoffStep(payload.StagingNode, payload.StagingNodeGroup),
		// 3. Navigate to lineside (no cargo)
		{Action: "dropoff", Node: payload.Location},
		// 4. Wait for operator release
		{Action: "wait"},
		// 5. Pickup empty bin from lineside
		{Action: "pickup", Node: payload.Location},
		// 6. Drop empty at staging 2
		buildDropoffStep(payload.StagingNode2, payload.StagingNode2Group),
		// 7. Pickup full from staging 1
		buildPickupStep(payload.StagingNode, payload.StagingNodeGroup),
		// 8. Drop full at lineside (the swap)
		{Action: "dropoff", Node: payload.Location},
		// 9. Pickup empty from staging 2
		buildPickupStep(payload.StagingNode2, payload.StagingNode2Group),
		// 10. Convey empty to drop destination
		buildDropoffStep(payload.OutgoingNode, payload.OutgoingNodeGroup),
	}

	order, err := e.orderMgr.CreateComplexOrder(&payloadID, quantity, steps)
	if err != nil {
		return nil, fmt.Errorf("single-robot hot-swap: %w", err)
	}

	// Tag with lineside so handleOrderCompleted resets the payload
	e.db.UpdateOrderDeliveryNode(order.ID, payload.Location)

	return &OrderRequestResult{
		CycleMode: "single_robot",
		Resupply:  order,
		PayloadID: payloadID,
	}, nil
}

func (e *Engine) requestTwoRobot(payload *store.Payload, payloadID int64, quantity int64) (*OrderRequestResult, error) {
	// Order A: resupply — pickup full → stage → dwell → pickup from staging → deliver to line
	resupplySteps := []protocol.ComplexOrderStep{
		buildPickupStep(payload.FullPickupNode, payload.FullPickupNodeGroup),
		buildDropoffStep(payload.StagingNode, payload.StagingNodeGroup),
		{Action: "wait"},
		buildPickupStep(payload.StagingNode, payload.StagingNodeGroup),
		{Action: "dropoff", Node: payload.Location},
	}
	resupply, err := e.orderMgr.CreateComplexOrder(&payloadID, quantity, resupplySteps)
	if err != nil {
		return nil, fmt.Errorf("two-robot resupply: %w", err)
	}
	e.db.UpdateOrderDeliveryNode(resupply.ID, payload.Location)

	// Order B: empty removal — navigate to line → dwell → pickup empty → drive to storage/drop
	removalSteps := []protocol.ComplexOrderStep{
		{Action: "dropoff", Node: payload.Location},
		{Action: "wait"},
		{Action: "pickup", Node: payload.Location},
		buildDropoffStep(payload.OutgoingNode, payload.OutgoingNodeGroup),
	}
	removal, err := e.orderMgr.CreateComplexOrder(&payloadID, quantity, removalSteps)
	if err != nil {
		log.Printf("hot-swap: resupply order %d created but removal failed for payload %d: %v",
			resupply.ID, payloadID, err)
		return nil, fmt.Errorf("two-robot removal: %w", err)
	}

	return &OrderRequestResult{
		CycleMode: "two_robot",
		Resupply:  resupply,
		Removal:   removal,
		PayloadID: payloadID,
	}, nil
}

// requestSequential creates Order A for the sequential cycle:
// navigate(lineside) → wait → pickup(lineside) → dropoff(destination).
// The wait comes BEFORE the pickup so the operator can confirm the bin is
// ready (may still have parts being consumed/filled). On release, Edge
// creates Order B (retrieve replacement) via handleSequentialBackfill.
func (e *Engine) requestSequential(payload *store.Payload, payloadID int64, quantity int64) (*OrderRequestResult, error) {
	steps := []protocol.ComplexOrderStep{
		// 1. Navigate to lineside (no cargo — robot positions at the bin)
		{Action: "dropoff", Node: payload.Location},
		// 2. Wait for operator release (bin may still have parts)
		{Action: "wait"},
		// 3. Pickup outgoing bin from lineside
		{Action: "pickup", Node: payload.Location},
		// 4. Deliver outgoing bin to destination
		buildDropoffStep(payload.OutgoingNode, payload.OutgoingNodeGroup),
	}

	order, err := e.orderMgr.CreateComplexOrder(&payloadID, quantity, steps)
	if err != nil {
		return nil, fmt.Errorf("sequential pickup: %w", err)
	}

	// Tag with lineside so handleOrderCompleted resets the payload
	e.db.UpdateOrderDeliveryNode(order.ID, payload.Location)

	return &OrderRequestResult{
		CycleMode: "sequential",
		Resupply:  order,
		PayloadID: payloadID,
	}, nil
}
