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

	// PayloadNeedsEmptyBin → request empty bin for produce payloads
	e.Events.SubscribeTypes(func(evt Event) {
		need := evt.Payload.(PayloadNeedsEmptyBinEvent)
		e.handlePayloadNeedsEmptyBin(need)
	}, EventPayloadNeedsEmptyBin)

	// PayloadEmpty → auto-remove empty bins for consume payloads
	e.Events.SubscribeTypes(func(evt Event) {
		empty := evt.Payload.(PayloadEmptyEvent)
		e.handlePayloadAutoRemove(empty)
	}, EventPayloadEmpty)

	// OrderStatusChanged → auto-release staged resupply when store order goes in-transit
	e.Events.SubscribeTypes(func(evt Event) {
		changed := evt.Payload.(OrderStatusChangedEvent)
		e.handleAutoReleaseStagedResupply(changed)
	}, EventOrderStatusChanged)

	// OrderFailed → reset produce payload from "replenishing" back to "empty"
	e.Events.SubscribeTypes(func(evt Event) {
		failed := evt.Payload.(OrderFailedEvent)
		e.handleOrderFailed(failed)
	}, EventOrderFailed)
}

// scanProducePayloads checks produce payloads on startup and emits empty bin requests if needed.
func (e *Engine) scanProducePayloads() {
	payloads, err := e.db.ListProducePayloads()
	if err != nil {
		log.Printf("scan produce payloads: %v", err)
		return
	}
	for _, p := range payloads {
		// Only request empty bin if payload is in empty/active status (not already awaiting or replenishing)
		if p.Status != "empty" && p.Status != "active" {
			continue
		}
		// Check if a supply order is already active
		active, _ := e.db.ListActiveOrdersByPayloadAndType(p.ID, "retrieve")
		if len(active) > 0 {
			continue
		}
		var lineID int64
		if js, err := e.db.GetJobStyle(p.JobStyleID); err == nil {
			lineID = js.LineID
		}
		e.debugFn("startup: produce payload %d needs empty bin", p.ID)
		e.Events.Emit(Event{Type: EventPayloadNeedsEmptyBin, Payload: PayloadNeedsEmptyBinEvent{
			PayloadID: p.ID, LineID: lineID, JobStyleID: p.JobStyleID,
			Location: p.Location, StagingNode: p.StagingNode, PayloadCode: p.PayloadCode,
		}})
	}
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
				StagingNode: p.StagingNode, Description: p.Description, PayloadCode: p.PayloadCode,
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

	// Check if this payload uses staged hot-swap (auto-remove empties + staging node)
	payload, err := e.db.GetPayload(payloadID)
	if err == nil && payload.AutoRemoveEmpties && payload.StagingNode != "" {
		// Order A: resupply — pickup from storage → stage → dwell → pickup from staging → deliver to line
		resupplySteps := []protocol.ComplexOrderStep{
			{Action: "pickup", NodeGroup: ""}, // core resolves source via payload
			{Action: "dropoff", Node: payload.StagingNode},
			{Action: "wait"},
			{Action: "pickup", Node: payload.StagingNode},
			{Action: "dropoff", Node: payload.Location},
		}
		resupply, err := e.orderMgr.CreateComplexOrder(
			&payloadID,
			int64(reorder.ReorderQty),
			resupplySteps,
		)
		if err != nil {
			log.Printf("create hot-swap resupply for payload %d: %v", reorder.PayloadID, err)
			return
		}
		// Tag resupply with its final destination so handleOrderCompleted
		// can distinguish it from the removal order.
		e.db.UpdateOrderDeliveryNode(resupply.ID, payload.Location)

		// Order B: empty removal — navigate to line → dwell → pickup empty → drive to storage
		removalSteps := []protocol.ComplexOrderStep{
			{Action: "dropoff", Node: payload.Location}, // navigate to line (no cargo — location-only block)
			{Action: "wait"},
			{Action: "pickup", Node: payload.Location},  // pick up the empty bin
			{Action: "dropoff", NodeGroup: ""},           // drive to storage and drop
		}
		_, err = e.orderMgr.CreateComplexOrder(
			&payloadID,
			int64(reorder.ReorderQty),
			removalSteps,
		)
		if err != nil {
			log.Printf("hot-swap: resupply order %d created but removal failed for payload %d: %v",
				resupply.ID, reorder.PayloadID, err)
		}
		return
	}

	// Simple retrieve (existing behavior)
	_, err = e.orderMgr.CreateRetrieveOrder(
		&payloadID,
		reorder.RetrieveEmpty,
		int64(reorder.ReorderQty),
		reorder.Location,
		reorder.StagingNode,
		"standard", "",
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

	payload, err := e.db.GetPayload(*order.PayloadID)
	if err != nil {
		log.Printf("get payload %d for order completion: %v", *order.PayloadID, err)
		return
	}

	switch order.OrderType {
	case "retrieve":
		if payload.Role == "produce" && order.RetrieveEmpty {
			// Empty bin arrived at produce slot — mark awaiting fill
			e.db.UpdatePayloadRemaining(payload.ID, 0, "awaiting")
			e.debugFn("produce payload %d: empty bin delivered, status=awaiting", payload.ID)
			return
		}
		// Consume payload: reset to full
		e.resetPayloadOnRetrieve(payload)

	case "complex":
		// Only reset when the resupply order completes (delivers TO the line).
		// The removal order has no delivery_node set, so it won't match.
		if payload.Role != "produce" && order.DeliveryNode == payload.Location {
			e.resetPayloadOnRetrieve(payload)
		}

	case "ingest":
		// Ingest complete: bin stored, produce slot is now empty and ready for next cycle
		e.db.UpdatePayloadRemaining(payload.ID, 0, "empty")
		e.debugFn("produce payload %d: ingest complete, status=empty", payload.ID)

		// If auto-order empties, request the next empty bin
		if payload.AutoOrderEmpties {
			var lineID int64
			if js, err := e.db.GetJobStyle(payload.JobStyleID); err == nil {
				lineID = js.LineID
			}
			e.Events.Emit(Event{Type: EventPayloadNeedsEmptyBin, Payload: PayloadNeedsEmptyBinEvent{
				PayloadID: payload.ID, LineID: lineID, JobStyleID: payload.JobStyleID,
				Location: payload.Location, StagingNode: payload.StagingNode, PayloadCode: payload.PayloadCode,
			}})
		}
	}
}

// resetPayloadOnRetrieve resets a consume payload to full after a retrieve order delivers.
func (e *Engine) resetPayloadOnRetrieve(payload *store.Payload) {
	resetUnits := payload.ProductionUnits
	// If ProductionUnits not configured, try payload catalog UOPCapacity
	if resetUnits == 0 && payload.PayloadCode != "" {
		if bp, err := e.db.GetPayloadCatalogByCode(payload.PayloadCode); err == nil && bp.UOPCapacity > 0 {
			resetUnits = bp.UOPCapacity
		}
	}
	// Transitional fallback for payloads without payload_code yet
	if resetUnits == 0 && payload.Description != "" {
		if bp, err := e.db.GetPayloadCatalogByName(payload.Description); err == nil && bp.UOPCapacity > 0 {
			resetUnits = bp.UOPCapacity
		}
	}

	if err := e.db.ResetPayload(payload.ID, resetUnits); err != nil {
		log.Printf("reset payload %d: %v", payload.ID, err)
		return
	}

	var lineID int64
	if js, err := e.db.GetJobStyle(payload.JobStyleID); err == nil {
		lineID = js.LineID
	}

	e.Events.Emit(Event{Type: EventPayloadUpdated, Payload: PayloadUpdatedEvent{
		PayloadID: payload.ID, LineID: lineID, JobStyleID: payload.JobStyleID, Location: payload.Location,
		OldRemaining: payload.Remaining, NewRemaining: resetUnits,
		Status: "active",
	}})
}

// handlePayloadNeedsEmptyBin requests an empty bin for a produce payload.
func (e *Engine) handlePayloadNeedsEmptyBin(need PayloadNeedsEmptyBinEvent) {
	// Guard: don't duplicate if supply order already active
	active, _ := e.db.ListActiveOrdersByPayloadAndType(need.PayloadID, "retrieve")
	if len(active) > 0 {
		e.debugFn("produce payload %d: supply order already active, skipping", need.PayloadID)
		return
	}

	// Mark payload as replenishing before creating the order
	e.db.UpdatePayloadRemaining(need.PayloadID, 0, "replenishing")

	e.debugFn("produce payload %d: requesting empty bin at %s", need.PayloadID, need.Location)
	payloadID := need.PayloadID
	_, err := e.orderMgr.CreateRetrieveOrder(
		&payloadID,
		true, // retrieveEmpty
		1,
		need.Location,
		need.StagingNode,
		"standard", "",
		e.cfg.Web.AutoConfirm,
	)
	if err != nil {
		log.Printf("create empty bin supply for payload %d: %v", need.PayloadID, err)
	}
}

// handlePayloadAutoRemove auto-removes empty bins for consume payloads with auto_remove_empties.
func (e *Engine) handlePayloadAutoRemove(empty PayloadEmptyEvent) {
	payload, err := e.db.GetPayload(empty.PayloadID)
	if err != nil {
		return
	}
	if payload.Role != "consume" || !payload.AutoRemoveEmpties {
		return
	}

	// If complex (hot-swap) orders are already active for this payload,
	// removal is handled as part of those orders — skip standalone store.
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

// handleAutoReleaseStagedResupply releases staged resupply orders when the store (empty-removal) order goes in-transit.
//
// CODE REVIEW NOTE: This handler may be dead code as of the complex order rework.
// Simple store orders created by handlePayloadAutoRemove are never auto-submitted
// (they require operator count confirmation), and the two-robot hot-swap creates
// complex orders (not store type), so the OrderType != "store" guard exits early.
// Keeping for now — needs review to determine if this serves any other flow or
// was scaffolding for an incomplete design. Flagged during complex order rework.
func (e *Engine) handleAutoReleaseStagedResupply(changed OrderStatusChangedEvent) {
	if changed.NewStatus != "in_transit" || changed.OrderType != "store" {
		return
	}
	order, err := e.db.GetOrder(changed.OrderID)
	if err != nil || order.PayloadID == nil {
		return
	}
	payload, err := e.db.GetPayload(*order.PayloadID)
	if err != nil || !payload.AutoRemoveEmpties {
		return
	}

	staged, _ := e.db.ListStagedOrdersByPayload(*order.PayloadID)
	for _, s := range staged {
		e.debugFn("auto-releasing staged order %d for payload %d", s.ID, *order.PayloadID)
		if err := e.orderMgr.ReleaseOrder(s.ID); err != nil {
			log.Printf("auto-release staged order %d: %v", s.ID, err)
		}
	}
}

// handleOrderFailed resets produce payloads from "replenishing" back to "empty"
// when their supply order fails (e.g. no empty bins available).
func (e *Engine) handleOrderFailed(failed OrderFailedEvent) {
	order, err := e.db.GetOrder(failed.OrderID)
	if err != nil || order.PayloadID == nil {
		return
	}

	payload, err := e.db.GetPayload(*order.PayloadID)
	if err != nil {
		return
	}

	// Only reset produce payloads that were waiting for an empty bin
	if payload.Role == "produce" && payload.Status == "replenishing" && order.RetrieveEmpty {
		e.db.UpdatePayloadRemaining(payload.ID, 0, "empty")
		log.Printf("produce payload %d: supply order %d failed (%s), reset to empty", payload.ID, order.ID, failed.Reason)
	}
}
