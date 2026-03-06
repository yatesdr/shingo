package engine

import "log"

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

	// PayloadReorder → create retrieve order
	e.Events.SubscribeTypes(func(evt Event) {
		reorder := evt.Payload.(PayloadReorderEvent)
		e.handlePayloadReorder(reorder)
	}, EventPayloadReorder)

	// OrderCompleted → reset payload if linked
	e.Events.SubscribeTypes(func(evt Event) {
		completed := evt.Payload.(OrderCompletedEvent)
		e.handleOrderCompleted(completed)
	}, EventOrderCompleted)
}

func (e *Engine) handleCounterDelta(delta CounterDeltaEvent) {
	if delta.JobStyleID == 0 {
		return // no active style for this line
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

		// Edge trigger: crossed reorder point (gated on auto-reorder)
		if p.AutoReorder && oldRemaining > p.ReorderPoint && newRemaining <= p.ReorderPoint && p.Status != "replenishing" {
			if err := e.db.UpdatePayloadRemaining(p.ID, newRemaining, "replenishing"); err != nil {
				log.Printf("update payload %d to replenishing: %v", p.ID, err)
				continue
			}
			e.Events.Emit(Event{Type: EventPayloadReorder, Payload: PayloadReorderEvent{
				PayloadID: p.ID, LineID: delta.LineID, JobStyleID: p.JobStyleID, Location: p.Location,
				StagingNode: p.StagingNode, Description: p.Description, BlueprintCode: p.BlueprintCode,
				Remaining: newRemaining, ReorderPoint: p.ReorderPoint,
				ReorderQty: p.ReorderQty, RetrieveEmpty: p.RetrieveEmpty,
			}})
		}
	}
}

func (e *Engine) handlePayloadReorder(reorder PayloadReorderEvent) {
	e.debugFn("payload reorder: payload=%d loc=%s qty=%d",
		reorder.PayloadID, reorder.Location, reorder.ReorderQty)

	payloadID := reorder.PayloadID
	_, err := e.orderMgr.CreateRetrieveOrder(
		&payloadID,
		reorder.RetrieveEmpty,
		int64(reorder.ReorderQty),
		reorder.Location,
		reorder.StagingNode,
		"standard",
		e.cfg.Web.AutoConfirm,
	)
	if err != nil {
		log.Printf("create reorder for payload %d: %v", reorder.PayloadID, err)
	}
}

func (e *Engine) handleOrderCompleted(completed OrderCompletedEvent) {
	order, err := e.db.GetOrder(completed.OrderID)
	if err != nil || order.PayloadID == nil {
		return
	}

	// Only reset payload on retrieve order completion
	if order.OrderType != "retrieve" {
		return
	}

	payload, err := e.db.GetPayload(*order.PayloadID)
	if err != nil {
		log.Printf("get payload %d for order completion: %v", *order.PayloadID, err)
		return
	}

	resetUnits := payload.ProductionUnits
	// If ProductionUnits not configured, try blueprint catalog UOPCapacity
	if resetUnits == 0 && payload.BlueprintCode != "" {
		if bp, err := e.db.GetBlueprintByCode(payload.BlueprintCode); err == nil && bp.UOPCapacity > 0 {
			resetUnits = bp.UOPCapacity
		}
	}
	// Transitional fallback for payloads without blueprint_code yet
	if resetUnits == 0 && payload.Description != "" {
		if bp, err := e.db.GetBlueprintByName(payload.Description); err == nil && bp.UOPCapacity > 0 {
			resetUnits = bp.UOPCapacity
		}
	}

	if err := e.db.ResetPayload(payload.ID, resetUnits); err != nil {
		log.Printf("reset payload %d: %v", payload.ID, err)
		return
	}

	// Determine lineID from the job style
	var lineID int64
	js, err := e.db.GetJobStyle(payload.JobStyleID)
	if err == nil {
		lineID = js.LineID
	}

	e.Events.Emit(Event{Type: EventPayloadUpdated, Payload: PayloadUpdatedEvent{
		PayloadID: payload.ID, LineID: lineID, JobStyleID: payload.JobStyleID, Location: payload.Location,
		OldRemaining: payload.Remaining, NewRemaining: payload.ProductionUnits,
		Status: "active",
	}})
}
