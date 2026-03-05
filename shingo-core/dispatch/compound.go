package dispatch

import (
	"fmt"
	"log"

	"shingocore/store"
)

const StatusReshuffling = "reshuffling"

// CreateCompoundOrder creates a parent order with child orders for a reshuffle plan.
// All children and instance claims are created in a single transaction.
func (d *Dispatcher) CreateCompoundOrder(parentOrder *store.Order, plan *ReshufflePlan) error {
	d.db.UpdateOrderStatus(parentOrder.ID, StatusReshuffling,
		fmt.Sprintf("reshuffling: %d steps to unbury instance %d", len(plan.Steps), plan.TargetInstance.ID))

	var children []store.CompoundChild
	for _, step := range plan.Steps {
		child := &store.Order{
			EdgeUUID:      fmt.Sprintf("%s-step-%d", parentOrder.EdgeUUID, step.Sequence),
			StationID:     parentOrder.StationID,
			OrderType:     OrderTypeMove,
			Status:        StatusPending,
			ParentOrderID: &parentOrder.ID,
			Sequence:      step.Sequence,
			PayloadDesc:   fmt.Sprintf("reshuffle %s: instance %d", step.StepType, step.InstanceID),
			InstanceID:    &step.InstanceID,
		}

		if step.FromNode != nil {
			child.PickupNode = step.FromNode.Name
		}
		if step.ToNode != nil {
			child.DeliveryNode = step.ToNode.Name
		}
		if step.StepType == "retrieve" && child.DeliveryNode == "" {
			child.DeliveryNode = parentOrder.DeliveryNode
		}

		children = append(children, store.CompoundChild{Order: child, InstanceID: step.InstanceID})
	}

	if err := d.db.CreateCompoundChildren(children); err != nil {
		return fmt.Errorf("create compound children: %w", err)
	}

	// Start executing the first child
	return d.AdvanceCompoundOrder(parentOrder.ID)
}

// AdvanceCompoundOrder dispatches the next pending child order in a compound sequence.
func (d *Dispatcher) AdvanceCompoundOrder(parentOrderID int64) error {
	next, err := d.db.GetNextChildOrder(parentOrderID)
	if err != nil {
		// No more children — compound order is complete
		d.db.UpdateOrderStatus(parentOrderID, StatusConfirmed, "reshuffle complete")
		d.db.CompleteOrder(parentOrderID)

		// Unlock lane
		d.unlockLaneForCompound(parentOrderID)

		parent, err := d.db.GetOrder(parentOrderID)
		if err == nil {
			d.emitter.EmitOrderCompleted(parentOrderID, parent.EdgeUUID, parent.StationID)
		}
		return nil
	}

	// Dispatch the child to fleet
	if next.PickupNode == "" || next.DeliveryNode == "" {
		d.db.UpdateOrderStatus(next.ID, StatusFailed, "missing pickup or delivery node")
		return d.AdvanceCompoundOrder(parentOrderID)
	}

	pickupNode, err := d.db.GetNodeByName(next.PickupNode)
	if err != nil {
		d.db.UpdateOrderStatus(next.ID, StatusFailed, fmt.Sprintf("pickup node %q not found", next.PickupNode))
		return d.AdvanceCompoundOrder(parentOrderID)
	}

	destNode, err := d.db.GetNodeByName(next.DeliveryNode)
	if err != nil {
		d.db.UpdateOrderStatus(next.ID, StatusFailed, fmt.Sprintf("delivery node %q not found", next.DeliveryNode))
		return d.AdvanceCompoundOrder(parentOrderID)
	}

	d.db.UpdateOrderStatus(next.ID, StatusSourcing, "dispatching reshuffle step")
	log.Printf("dispatch: advancing compound order %d, step %d (seq %d)", parentOrderID, next.ID, next.Sequence)

	// Build a synthetic envelope for the child dispatch
	env := d.syntheticEnvelope(next.StationID)
	d.dispatchToFleet(next, env, pickupNode, destNode)
	return nil
}

// HandleChildOrderComplete is called when a child order completes.
func (d *Dispatcher) HandleChildOrderComplete(childOrder *store.Order) {
	if childOrder.ParentOrderID == nil {
		return
	}
	d.AdvanceCompoundOrder(*childOrder.ParentOrderID)
}

// HandleChildOrderFailure handles failure of a child in a compound order.
// Cancels remaining children and fails the parent.
func (d *Dispatcher) HandleChildOrderFailure(parentOrderID, childOrderID int64) {
	log.Printf("dispatch: child order %d failed in compound %d, cancelling remaining", childOrderID, parentOrderID)

	// Cancel remaining pending children
	children, err := d.db.ListChildOrders(parentOrderID)
	if err != nil {
		return
	}
	for _, child := range children {
		if child.ID == childOrderID {
			continue
		}
		if child.Status == StatusPending || child.Status == StatusSourcing {
			d.db.UpdateOrderStatus(child.ID, StatusCancelled, "parent reshuffle failed")
			d.unclaimOrderInstances(child.ID)
		}
	}

	// Fail the parent
	parent, err := d.db.GetOrder(parentOrderID)
	if err != nil {
		return
	}
	d.db.UpdateOrderStatus(parentOrderID, StatusFailed, fmt.Sprintf("child order %d failed", childOrderID))
	d.emitter.EmitOrderFailed(parentOrderID, parent.EdgeUUID, parent.StationID, "reshuffle_failed",
		fmt.Sprintf("child order %d failed during reshuffle", childOrderID))

	// Unlock lane
	d.unlockLaneForCompound(parentOrderID)
}

// unlockLaneForCompound finds and unlocks the lane associated with a compound order's children.
func (d *Dispatcher) unlockLaneForCompound(parentOrderID int64) {
	if d.laneLock == nil {
		return
	}
	children, err := d.db.ListChildOrders(parentOrderID)
	if err != nil {
		return
	}
	for _, child := range children {
		if child.PickupNode != "" {
			node, err := d.db.GetNodeByName(child.PickupNode)
			if err == nil && node.ParentID != nil {
				d.laneLock.Unlock(*node.ParentID)
				return
			}
		}
	}
}
