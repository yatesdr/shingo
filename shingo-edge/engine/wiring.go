package engine

import (
	"log"

	"shingo/protocol"
	"shingoedge/store"
)

// wireEventHandlers sets up the full event chain:
// CounterDelta → payload decrement → PayloadReorder → order creation
// OrderCompleted → payload reset
func (e *Engine) wireEventHandlers() {
	// CounterDelta → payload consumption
	e.Events.SubscribeTypes(func(evt Event) {
		delta := evt.Payload.(CounterDeltaEvent)
		e.handleCounterDelta(delta)
	}, EventCounterDelta)

	// CounterDelta → hourly production tracking
	e.Events.SubscribeTypes(func(evt Event) {
		delta := evt.Payload.(CounterDeltaEvent)
		e.hourlyTracker.HandleDelta(delta)
	}, EventCounterDelta)

	// PayloadReorder → create cycle orders
	e.Events.SubscribeTypes(func(evt Event) {
		reorder := evt.Payload.(PayloadReorderEvent)
		e.handlePayloadReorder(reorder)
	}, EventPayloadReorder)

	// OrderCompleted → reset payload if linked
	e.Events.SubscribeTypes(func(evt Event) {
		completed := evt.Payload.(OrderCompletedEvent)
		e.handleOrderCompleted(completed)
	}, EventOrderCompleted)

	// PayloadEmpty → auto-remove empty bins for non-sequential consume payloads
	e.Events.SubscribeTypes(func(evt Event) {
		empty := evt.Payload.(PayloadEmptyEvent)
		e.handlePayloadAutoRemove(empty)
	}, EventPayloadEmpty)

	// OrderStatusChanged → sequential backfill: when Order A is released,
	// create Order B to deliver the replacement bin.
	e.Events.SubscribeTypes(func(evt Event) {
		changed := evt.Payload.(OrderStatusChangedEvent)
		e.handleSequentialBackfill(changed)
	}, EventOrderStatusChanged)

	// OrderFailed → reset produce payload from "replenishing" back to "empty"
	e.Events.SubscribeTypes(func(evt Event) {
		failed := evt.Payload.(OrderFailedEvent)
		e.handleOrderFailed(failed)
	}, EventOrderFailed)
}

// scanProducePayloads checks produce payloads on startup and delivers empty bins
// for initial provisioning. This is NOT the cycle trigger — the cycle is triggered
// by handleCounterDelta when remaining crosses the reorder point. This scan handles
// the bootstrap case where a station has no bin yet and needs one delivered.
func (e *Engine) scanProducePayloads() {
	payloads, err := e.db.ListProducePayloads()
	if err != nil {
		log.Printf("scan produce payloads: %v", err)
		return
	}
	for _, p := range payloads {
		if p.Status != "empty" && p.Status != "active" {
			continue
		}
		activeR, _ := e.db.ListActiveOrdersByPayloadAndType(p.ID, "retrieve")
		activeC, _ := e.db.ListActiveOrdersByPayloadAndType(p.ID, "complex")
		if len(activeR) > 0 || len(activeC) > 0 {
			continue
		}
		// Initial provisioning: simple retrieve to deliver an empty bin.
		// No cycle needed — there's nothing to swap (station is empty).
		e.debugFn("startup: produce payload %d needs initial empty bin", p.ID)
		payloadID := p.ID
		_, err := e.orderMgr.CreateRetrieveOrder(
			&payloadID, true, 1,
			p.Location, p.StagingNode,
			"standard", p.PayloadCode,
			e.cfg.Web.AutoConfirm,
		)
		if err != nil {
			log.Printf("startup: produce payload %d initial provision failed: %v", p.ID, err)
		}
	}
}

func (e *Engine) handleCounterDelta(delta CounterDeltaEvent) {
	if delta.JobStyleID == 0 {
		return
	}

	e.debugFn("counter delta: rp=%d line=%d job_style=%d delta=%d new_count=%d",
		delta.ReportingPointID, delta.LineID, delta.JobStyleID, delta.Delta, delta.NewCount)

	payloads, err := e.db.ListActivePayloadsByJobStyle(delta.JobStyleID)
	if err != nil {
		log.Printf("list active payloads for job style %d: %v", delta.JobStyleID, err)
		return
	}

	for _, p := range payloads {
		oldRemaining := p.Remaining
		newRemaining := oldRemaining - int(delta.Delta)
		if newRemaining < 0 {
			newRemaining = 0
		}

		status := p.Status
		if newRemaining == 0 {
			status = "empty"
		}

		if err := e.db.UpdatePayloadRemaining(p.ID, newRemaining, status); err != nil {
			log.Printf("update payload %d remaining: %v", p.ID, err)
			continue
		}

		e.Events.Emit(Event{Type: EventPayloadUpdated, Payload: PayloadUpdatedEvent{
			PayloadID: p.ID, LineID: delta.LineID, JobStyleID: p.JobStyleID, Location: p.Location,
			OldRemaining: oldRemaining, NewRemaining: newRemaining, Status: status,
		}})

		if newRemaining == 0 && oldRemaining > 0 {
			e.Events.Emit(Event{Type: EventPayloadEmpty, Payload: PayloadEmptyEvent{
				PayloadID: p.ID, LineID: delta.LineID, JobStyleID: p.JobStyleID, Location: p.Location,
			}})
		}

		// Crossed reorder point — trigger the material handling cycle.
		// Gated on AutoReorder: ON = system triggers, OFF = operator presses REQUEST button.
		if p.AutoReorder && oldRemaining > p.ReorderPoint && newRemaining <= p.ReorderPoint && p.Status != "replenishing" {
			if err := e.db.UpdatePayloadRemaining(p.ID, newRemaining, "replenishing"); err != nil {
				log.Printf("update payload %d to replenishing: %v", p.ID, err)
				continue
			}
			e.Events.Emit(Event{Type: EventPayloadReorder, Payload: PayloadReorderEvent{
				PayloadID: p.ID, LineID: delta.LineID, JobStyleID: p.JobStyleID, Location: p.Location,
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

func (e *Engine) handlePayloadReorder(reorder PayloadReorderEvent) {
	e.debugFn("payload reorder: payload=%d loc=%s", reorder.PayloadID, reorder.Location)

	// Quantity is always 1: one order = one bin.
	result, err := e.RequestOrders(reorder.PayloadID, 1)
	if err != nil {
		log.Printf("create reorder for payload %d: %v", reorder.PayloadID, err)
		return
	}
	e.debugFn("payload reorder result: cycle_mode=%s", result.CycleMode)
}

func (e *Engine) handleOrderCompleted(completed OrderCompletedEvent) {
	order, err := e.db.GetOrder(completed.OrderID)
	if err != nil || order.PayloadID == nil {
		return
	}

	payload, err := e.db.GetPayload(*order.PayloadID)
	if err != nil {
		log.Printf("get payload %d for order completion: %v", *order.PayloadID, err)
		return
	}

	switch order.OrderType {
	case "retrieve":
		// Replacement bin delivered to station. Reset remaining to UOP capacity.
		// Same for both roles — consume gets a full bin, produce gets an empty bin.
		// Either way, the station is restocked and the counter starts fresh.
		e.resetPayloadOnRetrieve(payload)

	case "complex":
		// Only reset when the replacement is delivered TO the line.
		// The removal/outgoing order has no delivery_node set, so it won't match.
		if order.DeliveryNode == payload.Location {
			e.resetPayloadOnRetrieve(payload)
		}

	case "ingest":
		// Ingest is Core's concern — manifest assignment and storage routing.
		// The cycle was already triggered by the counter. The reset happens when
		// Order B delivers the replacement bin (handled by the "retrieve" case).
		// Nothing to do here on Edge.
		e.debugFn("payload %d: ingest complete (Core stored the bin)", payload.ID)
	}
}

// resetPayloadOnRetrieve resets a consume payload to full after a retrieve order delivers.
// UOP capacity is looked up from the payload catalog (synced from Core).
func (e *Engine) resetPayloadOnRetrieve(payload *store.Payload) {
	bp, err := e.db.GetPayloadCatalogByCode(payload.PayloadCode)
	if err != nil || bp.UOPCapacity == 0 {
		log.Printf("reset payload %d: no catalog entry for code %q", payload.ID, payload.PayloadCode)
		return
	}

	if err := e.db.ResetPayload(payload.ID, bp.UOPCapacity); err != nil {
		log.Printf("reset payload %d: %v", payload.ID, err)
		return
	}

	var lineID int64
	if js, err := e.db.GetJobStyle(payload.JobStyleID); err == nil {
		lineID = js.LineID
	}

	e.Events.Emit(Event{Type: EventPayloadUpdated, Payload: PayloadUpdatedEvent{
		PayloadID: payload.ID, LineID: lineID, JobStyleID: payload.JobStyleID, Location: payload.Location,
		OldRemaining: payload.Remaining, NewRemaining: bp.UOPCapacity,
		Status: "active",
	}})
}

// handlePayloadAutoRemove auto-removes empty bins for consume payloads in
// hot-swap modes. Sequential mode handles removal via Order A's pickup step.
func (e *Engine) handlePayloadAutoRemove(empty PayloadEmptyEvent) {
	payload, err := e.db.GetPayload(empty.PayloadID)
	if err != nil {
		return
	}
	// Only consume payloads in non-sequential modes need standalone removal.
	// Sequential: Order A picks up the empty as its first action.
	// Hot-swap: complex orders handle removal as part of the step sequence.
	if payload.Role != "consume" || payload.CycleMode == "sequential" || payload.CycleMode == "" {
		return
	}

	activeComplex, _ := e.db.ListActiveOrdersByPayloadAndType(payload.ID, "complex")
	if len(activeComplex) > 0 {
		e.debugFn("payload %d: complex order active, skipping standalone auto-remove", payload.ID)
		return
	}

	e.debugFn("auto-remove empty bin for payload %d at %s", payload.ID, payload.Location)
	payloadID := payload.ID
	_, err = e.orderMgr.CreateStoreOrder(&payloadID, 0, payload.Location)
	if err != nil {
		log.Printf("create auto-remove store order for payload %d: %v", payload.ID, err)
	}
}

// handleSequentialBackfill creates Order B (backfill) when a sequential Order A
// is released (transitions to in_transit). The operator has confirmed the outgoing
// bin is ready — now fire the replacement delivery.
func (e *Engine) handleSequentialBackfill(changed OrderStatusChangedEvent) {
	if changed.NewStatus != "in_transit" || changed.OrderType != "complex" {
		return
	}

	order, err := e.db.GetOrder(changed.OrderID)
	if err != nil || order.PayloadID == nil {
		return
	}

	payload, err := e.db.GetPayload(*order.PayloadID)
	if err != nil {
		return
	}

	if payload.CycleMode != "sequential" {
		return
	}

	// Guard: don't create Order B if a backfill order already exists.
	// Check both retrieve and complex since Order B is now a complex order.
	activeR, _ := e.db.ListActiveOrdersByPayloadAndType(*order.PayloadID, "retrieve")
	activeC, _ := e.db.ListActiveOrdersByPayloadAndType(*order.PayloadID, "complex")
	// Subtract 1 from complex count because Order A (the one being released) is still active
	if len(activeR) > 0 || len(activeC) > 1 {
		e.debugFn("sequential backfill: payload %d already has active backfill, skipping", *order.PayloadID)
		return
	}

	// Create Order B: pickup replacement from source, deliver to lineside.
	// Uses FullPickupNode if configured, otherwise Core decides.
	payloadID := *order.PayloadID

	steps := []protocol.ComplexOrderStep{
		buildPickupStep(payload.FullPickupNode, payload.FullPickupNodeGroup),
		{Action: "dropoff", Node: payload.Location},
	}

	backfill, err := e.orderMgr.CreateComplexOrder(&payloadID, 1, steps)
	if err != nil {
		log.Printf("sequential backfill for payload %d: %v", payloadID, err)
		return
	}
	// Tag with lineside so handleOrderCompleted resets the payload
	e.db.UpdateOrderDeliveryNode(backfill.ID, payload.Location)

	e.debugFn("sequential backfill: created Order B %d for payload %d",
		backfill.ID, payloadID)
}

// handleOrderFailed resets payloads from "replenishing" back to "active"
// when their cycle order fails, so the counter can re-trigger the cycle.
func (e *Engine) handleOrderFailed(failed OrderFailedEvent) {
	order, err := e.db.GetOrder(failed.OrderID)
	if err != nil || order.PayloadID == nil {
		return
	}

	payload, err := e.db.GetPayload(*order.PayloadID)
	if err != nil {
		return
	}

	// If the payload was waiting for a cycle order that failed, reset to active
	// so the counter can cross the reorder point again and re-trigger.
	if payload.Status == "replenishing" {
		e.db.UpdatePayloadRemaining(payload.ID, payload.Remaining, "active")
		log.Printf("payload %d: order %d failed (%s), reset to active for retry", payload.ID, order.ID, failed.Reason)
	}
}
