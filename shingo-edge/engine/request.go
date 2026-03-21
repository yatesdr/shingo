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
	CycleMode      string       `json:"cycle_mode"` // "sequential", "two_robot", "single_robot"
	PrimaryOrder   *store.Order `json:"primary_order"`
	SecondaryOrder *store.Order `json:"secondary_order,omitempty"`
	PayloadID      int64        `json:"payload_id"`
}

// RequestOrders creates the appropriate order(s) for a slot based on its
// cycle mode. Called by both handleSlotReorder (automatic, triggered by
// counter delta) and apiSmartRequest (manual, operator button).
func (e *Engine) RequestOrders(payloadID int64, quantity int64) (*OrderRequestResult, error) {
	slot, err := e.db.GetSlot(payloadID)
	if err != nil {
		return nil, fmt.Errorf("slot %d not found: %w", payloadID, err)
	}

	mode := slot.CycleMode
	if mode == "" {
		mode = store.CycleModeSequential
	}

	switch mode {
	case store.CycleModeSequential:
		return e.requestSequential(slot, payloadID, quantity)
	case store.CycleModeSingleRobot:
		return e.requestSingleRobot(slot, payloadID, quantity)
	case store.CycleModeTwoRobot:
		return e.requestTwoRobot(slot, payloadID, quantity)
	default:
		return nil, fmt.Errorf("unknown cycle mode: %q for slot %d", mode, payloadID)
	}
}

func (e *Engine) requestSingleRobot(slot *store.MaterialSlot, payloadID int64, quantity int64) (*OrderRequestResult, error) {
	steps := []protocol.ComplexOrderStep{
		// 1. Pickup full bin from source
		buildPickupStep(slot.FullPickupNode, slot.FullPickupNodeGroup),
		// 2. Drop full at staging 1
		buildDropoffStep(slot.StagingNode, slot.StagingNodeGroup),
		// 3. Navigate to lineside (no cargo)
		{Action: "dropoff", Node: slot.Location},
		// 4. Wait for operator release
		{Action: "wait"},
		// 5. Pickup empty bin from lineside
		{Action: "pickup", Node: slot.Location},
		// 6. Drop empty at staging 2
		buildDropoffStep(slot.StagingNode2, slot.StagingNode2Group),
		// 7. Pickup full from staging 1
		buildPickupStep(slot.StagingNode, slot.StagingNodeGroup),
		// 8. Drop full at lineside (the swap)
		{Action: "dropoff", Node: slot.Location},
		// 9. Pickup empty from staging 2
		buildPickupStep(slot.StagingNode2, slot.StagingNode2Group),
		// 10. Convey empty to drop destination
		buildDropoffStep(slot.OutgoingNode, slot.OutgoingNodeGroup),
	}

	order, err := e.orderMgr.CreateComplexOrder(&payloadID, quantity, slot.Location, steps)
	if err != nil {
		return nil, fmt.Errorf("single-robot hot-swap: %w", err)
	}

	return &OrderRequestResult{
		CycleMode: store.CycleModeSingleRobot,
		PrimaryOrder: order,
		PayloadID: payloadID,
	}, nil
}

func (e *Engine) requestTwoRobot(slot *store.MaterialSlot, payloadID int64, quantity int64) (*OrderRequestResult, error) {
	// Order A: resupply — pickup full → stage → dwell → pickup from staging → deliver to line
	resupplySteps := []protocol.ComplexOrderStep{
		buildPickupStep(slot.FullPickupNode, slot.FullPickupNodeGroup),
		buildDropoffStep(slot.StagingNode, slot.StagingNodeGroup),
		{Action: "wait"},
		buildPickupStep(slot.StagingNode, slot.StagingNodeGroup),
		{Action: "dropoff", Node: slot.Location},
	}
	resupply, err := e.orderMgr.CreateComplexOrder(&payloadID, quantity, slot.Location, resupplySteps)
	if err != nil {
		return nil, fmt.Errorf("two-robot resupply: %w", err)
	}

	// Order B: empty removal — navigate to line → dwell → pickup empty → drive to storage/drop
	// No delivery_node: this order removes material, it doesn't deliver to line.
	removalSteps := []protocol.ComplexOrderStep{
		{Action: "dropoff", Node: slot.Location},
		{Action: "wait"},
		{Action: "pickup", Node: slot.Location},
		buildDropoffStep(slot.OutgoingNode, slot.OutgoingNodeGroup),
	}
	removal, err := e.orderMgr.CreateComplexOrder(&payloadID, quantity, "", removalSteps)
	if err != nil {
		// Removal failed but resupply is already submitted. Cancel the
		// resupply to avoid a half-cycle (delivery without removal).
		log.Printf("hot-swap: resupply order %d created but removal failed for slot %d: %v — cancelling resupply",
			resupply.ID, payloadID, err)
		if abortErr := e.orderMgr.AbortOrder(resupply.ID); abortErr != nil {
			log.Printf("hot-swap: failed to cancel resupply order %d: %v", resupply.ID, abortErr)
		}
		return nil, fmt.Errorf("two-robot removal: %w", err)
	}

	return &OrderRequestResult{
		CycleMode: store.CycleModeTwoRobot,
		PrimaryOrder: resupply,
		SecondaryOrder: removal,
		PayloadID: payloadID,
	}, nil
}

// requestSequential creates Order A for the sequential cycle:
// navigate(lineside) → wait → pickup(lineside) → dropoff(destination).
// The wait comes BEFORE the pickup so the operator can confirm the bin is
// ready (may still have parts being consumed/filled). On release, Edge
// creates Order B (retrieve replacement) via handleSequentialBackfill.
func (e *Engine) requestSequential(slot *store.MaterialSlot, payloadID int64, quantity int64) (*OrderRequestResult, error) {
	steps := []protocol.ComplexOrderStep{
		// 1. Navigate to lineside (no cargo — robot positions at the bin)
		{Action: "dropoff", Node: slot.Location},
		// 2. Wait for operator release (bin may still have parts)
		{Action: "wait"},
		// 3. Pickup outgoing bin from lineside
		{Action: "pickup", Node: slot.Location},
		// 4. Deliver outgoing bin to destination
		buildDropoffStep(slot.OutgoingNode, slot.OutgoingNodeGroup),
	}

	order, err := e.orderMgr.CreateComplexOrder(&payloadID, quantity, slot.Location, steps)
	if err != nil {
		return nil, fmt.Errorf("sequential pickup: %w", err)
	}

	return &OrderRequestResult{
		CycleMode: store.CycleModeSequential,
		PrimaryOrder: order,
		PayloadID: payloadID,
	}, nil
}
