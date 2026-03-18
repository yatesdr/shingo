package engine

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/store"
)

// OrderRequestResult is returned by RequestOrders to both the event handler
// and the API handler. It contains the created order(s) and metadata for the
// operator canvas to render correctly.
type OrderRequestResult struct {
	HotSwap     bool         `json:"hot_swap"`
	SingleRobot bool         `json:"single_robot"`
	Resupply    *store.Order `json:"resupply"`
	Removal     *store.Order `json:"removal,omitempty"`
	PayloadID   int64        `json:"payload_id"`
}

// RequestOrders creates the appropriate order(s) for a payload based on its
// hot-swap configuration. Called by both handlePayloadReorder (automatic,
// triggered by counter delta) and apiSmartRequest (manual, operator button).
func (e *Engine) RequestOrders(payloadID int64, quantity int64) (*OrderRequestResult, error) {
	payload, err := e.db.GetPayload(payloadID)
	if err != nil {
		return nil, fmt.Errorf("payload %d not found: %w", payloadID, err)
	}

	// Determine hot-swap mode, with legacy fallback
	hotSwapMode := payload.HotSwap
	if hotSwapMode == "" && payload.AutoRemoveEmpties && payload.StagingNode != "" {
		hotSwapMode = "two_robot"
	}

	switch hotSwapMode {
	case "single_robot":
		return e.requestSingleRobot(payload, payloadID, quantity)
	case "two_robot":
		return e.requestTwoRobot(payload, payloadID, quantity)
	default:
		return e.requestSimpleRetrieve(payload, payloadID, quantity)
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
		buildDropoffStep(payload.EmptyDropNode, payload.EmptyDropNodeGroup),
	}

	order, err := e.orderMgr.CreateComplexOrder(&payloadID, quantity, steps)
	if err != nil {
		return nil, fmt.Errorf("single-robot hot-swap: %w", err)
	}

	// Tag with lineside so handleOrderCompleted resets the payload
	e.db.UpdateOrderDeliveryNode(order.ID, payload.Location)

	return &OrderRequestResult{
		HotSwap:     true,
		SingleRobot: true,
		Resupply:    order,
		PayloadID:   payloadID,
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
	// Tag resupply with its final destination so handleOrderCompleted
	// can distinguish it from the removal order.
	e.db.UpdateOrderDeliveryNode(resupply.ID, payload.Location)

	// Order B: empty removal — navigate to line → dwell → pickup empty → drive to storage/drop
	removalSteps := []protocol.ComplexOrderStep{
		{Action: "dropoff", Node: payload.Location}, // navigate to line (no cargo)
		{Action: "wait"},
		{Action: "pickup", Node: payload.Location}, // pick up the empty bin
		buildDropoffStep(payload.EmptyDropNode, payload.EmptyDropNodeGroup),
	}
	removal, err := e.orderMgr.CreateComplexOrder(&payloadID, quantity, removalSteps)
	if err != nil {
		log.Printf("hot-swap: resupply order %d created but removal failed for payload %d: %v",
			resupply.ID, payloadID, err)
		return nil, fmt.Errorf("two-robot removal: %w", err)
	}

	return &OrderRequestResult{
		HotSwap:   true,
		Resupply:  resupply,
		Removal:   removal,
		PayloadID: payloadID,
	}, nil
}

func (e *Engine) requestSimpleRetrieve(payload *store.Payload, payloadID int64, quantity int64) (*OrderRequestResult, error) {
	order, err := e.orderMgr.CreateRetrieveOrder(
		&payloadID, payload.RetrieveEmpty,
		quantity, payload.Location, payload.StagingNode,
		"standard", payload.PayloadCode,
		e.cfg.Web.AutoConfirm,
	)
	if err != nil {
		return nil, fmt.Errorf("simple retrieve: %w", err)
	}

	return &OrderRequestResult{
		HotSwap:   false,
		Resupply:  order,
		PayloadID: payloadID,
	}, nil
}
