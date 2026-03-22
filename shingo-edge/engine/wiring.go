package engine

import (
	"log"

	"shingo/protocol"
	"shingoedge/orders"
	"shingoedge/store"
)

// wireEventHandlers keeps process ownership in Edge and updates op-node runtime
// from order lifecycle events. Counter deltas still feed hourly production.
func (e *Engine) wireEventHandlers() {
	e.Events.SubscribeTypes(func(evt Event) {
		if delta, ok := evt.Payload.(CounterDeltaEvent); ok {
			e.hourlyTracker.HandleDelta(delta)
		}
	}, EventCounterDelta)

	e.Events.SubscribeTypes(func(evt Event) {
		if completed, ok := evt.Payload.(OrderCompletedEvent); ok {
			e.handleOpNodeOrderCompleted(completed)
		}
	}, EventOrderCompleted)

	e.Events.SubscribeTypes(func(evt Event) {
		if failed, ok := evt.Payload.(OrderFailedEvent); ok {
			e.handleOpNodeOrderFailed(failed)
		}
	}, EventOrderFailed)
}

// scanProduceSlots is intentionally a no-op in the operator-station architecture.
// Initial provisioning is now an explicit station or Edge operation on op nodes.
func (e *Engine) scanProduceSlots() {}

func buildPickupStep(node, nodeGroup string) protocol.ComplexOrderStep {
	if node != "" {
		return protocol.ComplexOrderStep{Action: "pickup", Node: node}
	}
	if nodeGroup != "" {
		return protocol.ComplexOrderStep{Action: "pickup", NodeGroup: nodeGroup}
	}
	return protocol.ComplexOrderStep{Action: "pickup"}
}

func buildDropoffStep(node, nodeGroup string) protocol.ComplexOrderStep {
	if node != "" {
		return protocol.ComplexOrderStep{Action: "dropoff", Node: node}
	}
	if nodeGroup != "" {
		return protocol.ComplexOrderStep{Action: "dropoff", NodeGroup: nodeGroup}
	}
	return protocol.ComplexOrderStep{Action: "dropoff"}
}

func (e *Engine) handleOpNodeOrderCompleted(completed OrderCompletedEvent) {
	if completed.OpNodeID == nil {
		return
	}
	order, err := e.db.GetOrder(completed.OrderID)
	if err != nil {
		return
	}
	node, err := e.db.GetOpStationNode(*completed.OpNodeID)
	if err != nil {
		return
	}
	runtime, err := e.db.EnsureOpNodeRuntime(node.ID)
	if err != nil {
		return
	}

	var stationTaskID *int64
	if changeover, err := e.db.GetActiveProcessChangeover(node.ProcessID); err == nil {
		if stationTask, err := e.db.GetChangeoverStationTaskByStation(changeover.ID, node.OperatorStationID); err == nil {
			stationTaskID = &stationTask.ID
		}
	}
	var nodeTask *store.ChangeoverNodeTask
	if stationTaskID != nil {
		if t, err := e.db.GetChangeoverNodeTaskByNode(*stationTaskID, node.ID); err == nil {
			nodeTask = t
		}
	}

	// Staged delivery during runout phase.
	if nodeTask != nil && nodeTask.NextMaterialOrderID != nil && *nodeTask.NextMaterialOrderID == order.ID &&
		node.StagingNode != "" && order.DeliveryNode == node.StagingNode && runtime.StagedAssignmentID != nil {
		assignment, err := e.db.GetOpNodeAssignment(*runtime.StagedAssignmentID)
		if err == nil {
			_ = e.db.SetOpNodeRuntime(node.ID, runtime.EffectiveStyleID, runtime.ActiveAssignmentID, runtime.StagedAssignmentID,
				assignment.PayloadCode, "staged", runtime.RemainingUOP, runtime.ManifestStatus)
		}
		if nodeTask != nil {
			_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "staged")
		}
		return
	}

	// Empty line / clear access for tool change.
	if nodeTask != nil && nodeTask.OldMaterialReleaseOrderID != nil && *nodeTask.OldMaterialReleaseOrderID == order.ID &&
		order.OrderType == orders.TypeMove && order.PickupNode == node.DeliveryNode && order.DeliveryNode == node.OutgoingNode {
		_ = e.db.SetOpNodeRuntime(node.ID, runtime.EffectiveStyleID, runtime.ActiveAssignmentID, runtime.StagedAssignmentID,
			runtime.LoadedPayloadCode, "empty", 0, runtime.ManifestStatus)
		if nodeTask != nil {
			_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "line_cleared")
		}
		return
	}

	// Release staged or replenished material into production.
	if order.DeliveryNode == node.DeliveryNode {
		if nodeTask != nil && nodeTask.NextMaterialOrderID != nil && *nodeTask.NextMaterialOrderID == order.ID && runtime.StagedAssignmentID != nil {
			assign, err := e.db.GetOpNodeAssignment(*runtime.StagedAssignmentID)
			if err == nil {
				assignID := assign.ID
				styleID := assign.StyleID
				_ = e.db.SetOpNodeRuntime(node.ID, &styleID, &assignID, nil, assign.PayloadCode, "active", assign.UOPCapacity, "pending_confirmation")
				if nodeTask != nil {
					_ = e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "released")
				}
				_ = e.tryCompleteProcessChangeover(node.ProcessID)
				return
			}
		}
		if runtime.ActiveAssignmentID != nil && order.OrderType == orders.TypeRetrieve {
			assign, err := e.db.GetOpNodeAssignment(*runtime.ActiveAssignmentID)
			if err == nil {
				_ = e.db.SetOpNodeRuntime(node.ID, runtime.EffectiveStyleID, runtime.ActiveAssignmentID, runtime.StagedAssignmentID,
					assign.PayloadCode, "active", assign.UOPCapacity, runtime.ManifestStatus)
			}
		}
	}
}

func (e *Engine) handleOpNodeOrderFailed(failed OrderFailedEvent) {
	order, err := e.db.GetOrder(failed.OrderID)
	if err != nil || order.OpNodeID == nil {
		return
	}
	runtime, err := e.db.EnsureOpNodeRuntime(*order.OpNodeID)
	if err != nil {
		return
	}
	assign, err := e.db.GetPreferredOpNodeAssignment(*order.OpNodeID)
	if err != nil {
		log.Printf("order failed: op-node assignment lookup %d: %v", *order.OpNodeID, err)
		return
	}
	_ = e.db.SetOpNodeRuntime(*order.OpNodeID, runtime.EffectiveStyleID, runtime.ActiveAssignmentID, runtime.StagedAssignmentID,
		assign.PayloadCode, "empty", runtime.RemainingUOP, runtime.ManifestStatus)
}
