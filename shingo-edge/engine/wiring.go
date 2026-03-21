package engine

import (
	"log"

	"shingo/protocol"
	"shingoedge/orders"
	"shingoedge/store"
)

// wireEventHandlers sets up the full event chain:
// CounterDelta → slot decrement → SlotReorder → order creation
// OrderCompleted → slot reset
func (e *Engine) wireEventHandlers() {
	// CounterDelta → slot consumption
	e.Events.SubscribeTypes(func(evt Event) {
		if delta, ok := evt.Payload.(CounterDeltaEvent); ok {
			e.handleCounterDelta(delta)
		}
	}, EventCounterDelta)

	// CounterDelta → hourly production tracking
	e.Events.SubscribeTypes(func(evt Event) {
		if delta, ok := evt.Payload.(CounterDeltaEvent); ok {
			e.hourlyTracker.HandleDelta(delta)
		}
	}, EventCounterDelta)

	// SlotReorder → create cycle orders
	e.Events.SubscribeTypes(func(evt Event) {
		if reorder, ok := evt.Payload.(SlotReorderEvent); ok {
			e.handleSlotReorder(reorder)
		}
	}, EventSlotReorder)

	// OrderCompleted → reset slot if linked
	e.Events.SubscribeTypes(func(evt Event) {
		if completed, ok := evt.Payload.(OrderCompletedEvent); ok {
			e.handleOrderCompleted(completed)
		}
	}, EventOrderCompleted)

	// SlotEmpty → auto-remove empty bins for non-sequential consume slots
	e.Events.SubscribeTypes(func(evt Event) {
		if empty, ok := evt.Payload.(SlotEmptyEvent); ok {
			e.handleSlotAutoRemove(empty)
		}
	}, EventSlotEmpty)

	// OrderStatusChanged → sequential backfill: when Order A is released,
	// create Order B to deliver the replacement bin.
	e.Events.SubscribeTypes(func(evt Event) {
		if changed, ok := evt.Payload.(OrderStatusChangedEvent); ok {
			e.handleSequentialBackfill(changed)
		}
	}, EventOrderStatusChanged)

	// OrderFailed → reset produce slot from "replenishing" back to "empty"
	e.Events.SubscribeTypes(func(evt Event) {
		if failed, ok := evt.Payload.(OrderFailedEvent); ok {
			e.handleOrderFailed(failed)
		}
	}, EventOrderFailed)
}

// scanProduceSlots checks produce slots on startup and delivers empty bins
// for initial provisioning. This is NOT the cycle trigger — the cycle is triggered
// by handleCounterDelta when remaining crosses the reorder point. This scan handles
// the bootstrap case where a station has no bin yet and needs one delivered.
func (e *Engine) scanProduceSlots() {
	slots, err := e.db.ListProduceSlots()
	if err != nil {
		log.Printf("scan produce slots: %v", err)
		return
	}
	for _, s := range slots {
		if s.Status != store.SlotEmpty && s.Status != store.SlotActive {
			continue
		}
		activeR, _ := e.db.ListActiveOrdersByPayloadAndType(s.ID, orders.TypeRetrieve)
		activeC, _ := e.db.ListActiveOrdersByPayloadAndType(s.ID, orders.TypeComplex)
		if len(activeR) > 0 || len(activeC) > 0 {
			continue
		}
		// Initial provisioning: simple retrieve to deliver an empty bin.
		// No cycle needed — there's nothing to swap (station is empty).
		e.debugFn.Log("startup: produce slot %d needs initial empty bin", s.ID)
		slotID := s.ID
		_, err := e.orderMgr.CreateRetrieveOrder(
			&slotID, true, 1,
			s.Location, s.StagingNode,
			"standard", s.PayloadCode,
			e.cfg.Web.AutoConfirm,
		)
		if err != nil {
			log.Printf("startup: produce slot %d initial provision failed: %v", s.ID, err)
		}
	}
}

func (e *Engine) handleCounterDelta(delta CounterDeltaEvent) {
	if delta.JobStyleID == 0 {
		return
	}

	e.debugFn.Log("counter delta: rp=%d line=%d job_style=%d delta=%d new_count=%d",
		delta.ReportingPointID, delta.LineID, delta.JobStyleID, delta.Delta, delta.NewCount)

	slots, err := e.db.ListActiveSlotsByStyle(delta.JobStyleID)
	if err != nil {
		log.Printf("list active slots for style %d: %v", delta.JobStyleID, err)
		return
	}

	for _, s := range slots {
		oldRemaining := s.Remaining
		newRemaining := oldRemaining - int(delta.Delta)
		if newRemaining < 0 {
			newRemaining = 0
		}

		status := s.Status
		if newRemaining == 0 {
			status = store.SlotEmpty
		}

		if err := e.db.UpdateSlotRemaining(s.ID, newRemaining, status); err != nil {
			log.Printf("update slot %d remaining: %v", s.ID, err)
			continue
		}

		e.Events.Emit(Event{Type: EventSlotUpdated, Payload: SlotUpdatedEvent{
			PayloadID: s.ID, LineID: delta.LineID, JobStyleID: s.StyleID, Location: s.Location,
			OldRemaining: oldRemaining, NewRemaining: newRemaining, Status: status,
		}})

		if newRemaining == 0 && oldRemaining > 0 {
			e.Events.Emit(Event{Type: EventSlotEmpty, Payload: SlotEmptyEvent{
				PayloadID: s.ID, LineID: delta.LineID, JobStyleID: s.StyleID, Location: s.Location,
			}})
		}

		// Crossed reorder point — trigger the material handling cycle.
		// Gated on AutoReorder: ON = system triggers, OFF = operator presses REQUEST button.
		if s.AutoReorder && oldRemaining > s.ReorderPoint && newRemaining <= s.ReorderPoint && s.Status != store.SlotReplenishing {
			if err := e.db.UpdateSlotRemaining(s.ID, newRemaining, store.SlotReplenishing); err != nil {
				log.Printf("update slot %d to replenishing: %v", s.ID, err)
				continue
			}
			e.Events.Emit(Event{Type: EventSlotReorder, Payload: SlotReorderEvent{
				PayloadID: s.ID, LineID: delta.LineID, JobStyleID: s.StyleID, Location: s.Location,
			}})
		}
	}
}

// buildPickupStep creates a ComplexOrderStep for a pickup action.
func buildPickupStep(node, nodeGroup string) protocol.ComplexOrderStep {
	if node != "" {
		return protocol.ComplexOrderStep{Action: "pickup", Node: node}
	}
	if nodeGroup != "" {
		return protocol.ComplexOrderStep{Action: "pickup", NodeGroup: nodeGroup}
	}
	return protocol.ComplexOrderStep{Action: "pickup"}
}

// buildDropoffStep creates a ComplexOrderStep for a dropoff action.
func buildDropoffStep(node, nodeGroup string) protocol.ComplexOrderStep {
	if node != "" {
		return protocol.ComplexOrderStep{Action: "dropoff", Node: node}
	}
	if nodeGroup != "" {
		return protocol.ComplexOrderStep{Action: "dropoff", NodeGroup: nodeGroup}
	}
	return protocol.ComplexOrderStep{Action: "dropoff"}
}

func (e *Engine) handleSlotReorder(reorder SlotReorderEvent) {
	e.debugFn.Log("slot reorder: slot=%d loc=%s", reorder.PayloadID, reorder.Location)

	// Quantity is always 1: one order = one bin.
	result, err := e.RequestOrders(reorder.PayloadID, 1)
	if err != nil {
		log.Printf("create reorder for slot %d: %v", reorder.PayloadID, err)
		// Order creation failed but the slot is already in "replenishing"
		// state. Reset it to "active" so the counter can re-trigger the
		// reorder when it crosses the threshold again.
		slot, pErr := e.db.GetSlot(reorder.PayloadID)
		if pErr == nil && slot.Status == store.SlotReplenishing {
			e.db.UpdateSlotRemaining(slot.ID, slot.Remaining, store.SlotActive)
			log.Printf("slot %d: order creation failed, reset from replenishing to active for retry", reorder.PayloadID)
			e.debugFn.Log("slot %d: reset to active after failed reorder", reorder.PayloadID)
		}
		return
	}
	e.debugFn.Log("slot reorder result: cycle_mode=%s", result.CycleMode)
}

func (e *Engine) handleOrderCompleted(completed OrderCompletedEvent) {
	order, err := e.db.GetOrder(completed.OrderID)
	if err != nil || order.PayloadID == nil {
		return
	}

	slot, err := e.db.GetSlot(*order.PayloadID)
	if err != nil {
		log.Printf("get slot %d for order completion: %v", *order.PayloadID, err)
		return
	}

	e.debugFn.Log("order completed: id=%d type=%s slot=%d delivery=%s loc=%s",
		order.ID, order.OrderType, slot.ID, order.DeliveryNode, slot.Location)

	switch order.OrderType {
	case orders.TypeRetrieve:
		e.resetSlotOnRetrieve(slot)

	case orders.TypeComplex:
		if order.DeliveryNode == slot.Location {
			e.debugFn.Log("complex order %d delivered to lineside — resetting slot %d", order.ID, slot.ID)
			e.resetSlotOnRetrieve(slot)
		} else {
			e.debugFn.Log("complex order %d: delivery_node=%q != location=%q, no reset", order.ID, order.DeliveryNode, slot.Location)
		}

	// Ingest and store are Core's concern — no slot reset needed on Edge.
	}
}

// resetSlotOnRetrieve resets a consume slot to full after a retrieve order delivers.
// UOP capacity is looked up from the payload catalog (synced from Core).
func (e *Engine) resetSlotOnRetrieve(slot *store.MaterialSlot) {
	bp, err := e.db.GetPayloadCatalogByCode(slot.PayloadCode)
	if err != nil || bp.UOPCapacity == 0 {
		log.Printf("reset slot %d: no catalog entry for code %q", slot.ID, slot.PayloadCode)
		return
	}

	if err := e.db.ResetSlot(slot.ID, bp.UOPCapacity); err != nil {
		log.Printf("reset slot %d: %v", slot.ID, err)
		return
	}

	var lineID int64
	if js, err := e.db.GetStyle(slot.StyleID); err == nil {
		lineID = js.LineID
	}

	e.Events.Emit(Event{Type: EventSlotUpdated, Payload: SlotUpdatedEvent{
		PayloadID: slot.ID, LineID: lineID, JobStyleID: slot.StyleID, Location: slot.Location,
		OldRemaining: slot.Remaining, NewRemaining: bp.UOPCapacity,
		Status: store.SlotActive,
	}})
}

// handleSlotAutoRemove auto-removes empty bins for consume slots in
// hot-swap modes. Sequential mode handles removal via Order A's pickup step.
func (e *Engine) handleSlotAutoRemove(empty SlotEmptyEvent) {
	slot, err := e.db.GetSlot(empty.PayloadID)
	if err != nil {
		log.Printf("auto-remove: get slot %d: %v", empty.PayloadID, err)
		return
	}
	// Only consume slots in non-sequential modes need standalone removal.
	// Sequential: Order A picks up the empty as its first action.
	// Hot-swap: complex orders handle removal as part of the step sequence.
	if slot.Role != store.RoleConsume || slot.CycleMode == store.CycleModeSequential || slot.CycleMode == "" {
		return
	}

	activeComplex, _ := e.db.ListActiveOrdersByPayloadAndType(slot.ID, orders.TypeComplex)
	if len(activeComplex) > 0 {
		e.debugFn.Log("slot %d: complex order active, skipping standalone auto-remove", slot.ID)
		return
	}

	e.debugFn.Log("auto-remove empty bin for slot %d at %s", slot.ID, slot.Location)
	slotID := slot.ID
	_, err = e.orderMgr.CreateStoreOrder(&slotID, 0, slot.Location)
	if err != nil {
		log.Printf("create auto-remove store order for slot %d: %v", slot.ID, err)
	}
}

// handleSequentialBackfill creates Order B (backfill) when a sequential Order A
// is released (transitions to in_transit). The operator has confirmed the outgoing
// bin is ready — now fire the replacement delivery.
func (e *Engine) handleSequentialBackfill(changed OrderStatusChangedEvent) {
	if changed.NewStatus != orders.StatusInTransit || changed.OrderType != orders.TypeComplex {
		return
	}

	order, err := e.db.GetOrder(changed.OrderID)
	if err != nil {
		e.debugFn.Log("sequential backfill: get order %d: %v", changed.OrderID, err)
		return
	}
	if order.PayloadID == nil {
		return
	}

	slot, err := e.db.GetSlot(*order.PayloadID)
	if err != nil {
		e.debugFn.Log("sequential backfill: get slot %d: %v", *order.PayloadID, err)
		return
	}

	if slot.CycleMode != store.CycleModeSequential {
		return
	}

	// Guard: don't create Order B if a backfill order already exists.
	// Check both retrieve and complex since Order B is now a complex order.
	activeR, _ := e.db.ListActiveOrdersByPayloadAndType(*order.PayloadID, orders.TypeRetrieve)
	activeC, _ := e.db.ListActiveOrdersByPayloadAndType(*order.PayloadID, orders.TypeComplex)
	// Subtract 1 from complex count because Order A (the one being released) is still active
	if len(activeR) > 0 || len(activeC) > 1 {
		e.debugFn.Log("sequential backfill: slot %d already has active backfill, skipping", *order.PayloadID)
		return
	}

	// Create Order B: pickup replacement from source, deliver to lineside.
	// Uses FullPickupNode if configured, otherwise Core decides.
	slotID := *order.PayloadID

	steps := []protocol.ComplexOrderStep{
		buildPickupStep(slot.FullPickupNode, slot.FullPickupNodeGroup),
		{Action: "dropoff", Node: slot.Location},
	}

	backfill, err := e.orderMgr.CreateComplexOrder(&slotID, 1, slot.Location, steps)
	if err != nil {
		log.Printf("sequential backfill for slot %d: %v", slotID, err)
		return
	}

	e.debugFn.Log("sequential backfill: created Order B %d for slot %d",
		backfill.ID, slotID)
}

// handleOrderFailed resets slots from "replenishing" back to "active"
// when their cycle order fails, so the counter can re-trigger the cycle.
func (e *Engine) handleOrderFailed(failed OrderFailedEvent) {
	e.debugFn.Log("order failed: id=%d type=%s reason=%s", failed.OrderID, failed.OrderType, failed.Reason)

	order, err := e.db.GetOrder(failed.OrderID)
	if err != nil || order.PayloadID == nil {
		return
	}

	slot, err := e.db.GetSlot(*order.PayloadID)
	if err != nil {
		log.Printf("get slot %d for failed order %d: %v", *order.PayloadID, failed.OrderID, err)
		return
	}

	// If the slot was waiting for a cycle order that failed, reset to active
	// so the counter can cross the reorder point again and re-trigger.
	if slot.Status == store.SlotReplenishing {
		if err := e.db.UpdateSlotRemaining(slot.ID, slot.Remaining, store.SlotActive); err != nil {
			log.Printf("failed to reset slot %d from replenishing: %v", slot.ID, err)
			return
		}
		e.debugFn.Log("slot %d: reset to active after order %d failed", slot.ID, order.ID)
		log.Printf("slot %d: order %d failed (%s), reset to active for retry", slot.ID, order.ID, failed.Reason)
	}
}
