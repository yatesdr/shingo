// wiring_completion.go — Order delivery and completion handling.
//
// handleOrderDelivered runs when fleet reports FINISHED. It sends the
// delivered notification and moves the bin(s) to their destination
// immediately so telemetry is accurate. handleOrderCompleted runs when
// Edge confirms receipt and advances compound orders. Both paths use
// applyBinArrivalForOrder / applyMultiBinArrivalForOrder for the bin
// move; the completion path is idempotent (skips bins already at dest).

package engine

import (
	"fmt"
	"time"

	"shingo/protocol"
	"shingo/shared/clock"
	"shingocore/store/orders"
)

// handleOrderDelivered runs on fleet-reported FINISHED. Notifies Edge
// and moves the bin to its destination immediately so subsequent orders
// see accurate occupancy.
func (e *Engine) handleOrderDelivered(order *orders.Order) {
	// Resolve staged expiry for the delivered message. Only ship a countdown
	// when the bin will actually arrive `staged` — for storage destinations
	// (LANE/NGRP roots and their children) the bin lands `available`
	// and an expiry on the order envelope is misleading to the operator UI.
	var stagedExpireAt *time.Time
	if order.DeliveryNode != "" {
		if destNode, err := e.db.GetNodeByDotName(order.DeliveryNode); err == nil {
			if staged, ea := e.resolveNodeStaging(destNode); staged && ea != nil {
				stagedExpireAt = ea
			}
		}
	}

	// Apply bin arrival FIRST so telemetry is accurate immediately. The
	// previous order — sendToEdge then applyBinArrivalForOrder — let
	// AutoConfirm Edge orders auto-confirm before the bin-arrival
	// commit landed.
	e.applyBinArrivalForOrder(order)

	// Ship the bin ID so Edge can attribute PLC tick deltas to the
	// right bin. Single-bin orders carry BinID; multi-bin orders leave
	// it nil and rely on bucket deltas instead. Edge's bin-ownership
	// flip means active_bin_id at the runtime row is now sourced from
	// this envelope — without it, Edge can't track tick attribution
	// for the duration the bin sits at the slot.
	var binID *int64
	multiBin := false
	if order.BinID != nil {
		orderBins, _ := e.db.ListOrderBins(order.ID)
		if len(orderBins) == 0 {
			v := *order.BinID
			binID = &v
		} else {
			multiBin = true
		}
	}

	// Diagnostic: surface the missing-bin case so a future "Edge isn't
	// tracking ticks" investigation can grep for the cause. order.BinID
	// nil on a single-bin order means planMove never persisted the bin
	// reference (a known failure mode the bin-stuck-at-source log at
	// applyBinArrivalForOrder also tracks).
	if binID == nil && !multiBin {
		e.logFn("engine: order=%d type=%s shipped order.delivered without bin_id (order.BinID nil at delivery — Edge tick attribution will be silent until next order)",
			order.ID, order.OrderType)
	}

	// Snapshot the just-arrived bin's authoritative count + load-lifecycle
	// epoch onto the envelope so Edge seeds its runtime cache and stamps
	// outgoing BinUOPDeltas from these — no separate HTTP pull. Read AFTER
	// applyBinArrivalForOrder above so the bin row reflects the arrival.
	// Single-bin only; multi-bin (binID nil) uses bucket deltas.
	var uopRemaining *int
	var deltaEpoch int64
	if binID != nil {
		if bin, binErr := e.db.GetBin(*binID); binErr == nil {
			u := bin.UOPRemaining
			uopRemaining = &u
			deltaEpoch = bin.DeltaEpoch
		} else {
			e.logFn("engine: order=%d delivered: bin %d uop/epoch lookup failed: %v (Edge falls back to role default)",
				order.ID, *binID, binErr)
		}
	}

	// Core-admin (manual move) orders have no station; broadcast to all edges so
	// each can attempt the bin binding via DeliveryNode fallback.
	stationID := order.StationID
	if stationID == "" {
		stationID = protocol.StationBroadcast
	}
	if err := e.sendToEdge(protocol.TypeOrderDelivered, stationID, &protocol.OrderDelivered{
		OrderUUID:      order.EdgeUUID,
		DeliveredAt:    clock.Now().UTC(),
		StagedExpireAt: stagedExpireAt,
		BinID:          binID,
		UOPRemaining:   uopRemaining,
		DeltaEpoch:     deltaEpoch,
		DeliveryNode:   order.DeliveryNode,
	}); err != nil {
		e.logFn("engine: delivered notification: %v", err)
	}
}

// applyBinArrivalForOrder moves the order's bin(s) to the delivery node.
// Called from handleOrderDelivered (on fleet FINISHED) so that telemetry
// is accurate immediately. handleOrderCompleted still runs on confirmation
// but is idempotent — it skips the bin move if already at the destination.
func (e *Engine) applyBinArrivalForOrder(order *orders.Order) {
	if order.SourceNode == "" || order.DeliveryNode == "" {
		// Bin-stuck-at-source diagnostic: previously a silent skip. Move-order
		// post-mortem 2026-04-28 traced "delivered but bin still at source"
		// scenarios that left no log line at all.
		e.logFn("delivery: order=%d type=%s bin=%v skipped arrival: missing source/delivery (source=%q delivery=%q)",
			order.ID, order.OrderType, order.BinID, order.SourceNode, order.DeliveryNode)
		return
	}

	// Multi-bin path
	orderBins, _ := e.db.ListOrderBins(order.ID)
	if len(orderBins) > 0 {
		e.logFn("delivery: order=%d type=%s taking multi-bin arrival path (%d junction rows)",
			order.ID, order.OrderType, len(orderBins))
		e.applyMultiBinArrivalForOrder(order, orderBins)
		return
	}

	// Single-bin path
	if order.BinID == nil {
		// Bin-stuck-at-source diagnostic: this is the failure mode where
		// planMove's UpdateOrderBinID didn't persist (or was never called)
		// but the order still progressed to FINISHED. Without a log here,
		// the bin silently stays at source and the symptom shows up downstream.
		e.logFn("delivery: order=%d type=%s skipped arrival: order.BinID is nil (source=%s delivery=%s) — planMove may have failed to persist BinID",
			order.ID, order.OrderType, order.SourceNode, order.DeliveryNode)
		return
	}

	destNode, err := e.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		e.logFn("engine: dest node %s not found for delivery arrival: %v", order.DeliveryNode, err)
		return
	}

	sourceNode, _ := e.db.GetNodeByDotName(order.SourceNode)
	sourceNodeID := int64(0)
	if sourceNode != nil {
		sourceNodeID = sourceNode.ID
	}

	// Claim-based teleport guard (#7): a late-arriving FINISHED for an
	// order that was meanwhile failed/cancelled (releasing the claim)
	// or whose bin was reclaimed by a newer order would, without this
	// guard, move the bin to a stale destination — the same teleport
	// shape SMN_001 / SMN_002 produced in the completion path. The
	// completion-time guard at handleOrderCompleted already protects
	// the safety-net call; this matches the predicate at delivery time.
	//
	// Skip the guard for compound order children (ParentOrderID != nil):
	// a multi-step reshuffle plan intentionally overlaps bin claims —
	// when CreateCompoundChildren writes claims for all steps in one
	// transaction, the LAST step's UPDATE wins for any bin that appears
	// in multiple steps (e.g. an unbury followed by a restock both
	// touching the same blocker bin). Interim child orders need to move
	// the bin even though claimed_by points at a sibling child. The
	// compound dispatcher serializes children sequentially, so the
	// teleport class this guard prevents (concurrent reclaim) doesn't
	// apply within a compound family.
	if order.ParentOrderID == nil {
		bin, binErr := e.db.GetBin(*order.BinID)
		if binErr != nil {
			e.logFn("engine: get bin %d for delivery arrival guard: %v", *order.BinID, binErr)
			return
		}
		if bin.ClaimedBy == nil || *bin.ClaimedBy != order.ID {
			claimedDesc := "nil"
			if bin.ClaimedBy != nil {
				claimedDesc = fmt.Sprintf("%d", *bin.ClaimedBy)
			}
			e.logFn("delivery: order=%d bin=%d not claimed by this order (claimed_by=%s) — skipping arrival to avoid teleport",
				order.ID, *order.BinID, claimedDesc)
			return
		}
	}

	staged, expiresAt := e.resolveNodeStaging(destNode)

	// Note: previously this path forced staged=false for complex orders with
	// WaitIndex > 0 and for retrieve_empty deliveries. Both overrides removed
	// 2026-04-14 — they bypassed the FindSourceBinFIFO staged exclusion and
	// allowed unloader/loader auto-requests to poach lineside bins. With the
	// overrides gone, lineside deliveries arrive `staged` and stay protected
	// until the next claim or operator action.

	e.logFn("delivery: order=%d type=%s bin=%d arriving %s -> %s (staged=%v)",
		order.ID, order.OrderType, *order.BinID, order.SourceNode, order.DeliveryNode, staged)
	evicted, err := e.binService.ApplyArrival(*order.BinID, destNode.ID, staged, expiresAt)
	if err != nil {
		e.logFn("engine: apply bin arrival on delivery for order %d bin %d: %v", order.ID, *order.BinID, err)
		return
	}
	if evicted {
		e.logFn("WARN: delivery of bin %d to %s evicted a stale bin record there — a successful delivery proved the slot empty; the stale bin is at _TRANSIT, recover via the anomalies page", *order.BinID, order.DeliveryNode)
	}

	// Re-read bin for the event payload (post-ApplyArrival state). The
	// guard's earlier read is pre-arrival; the event needs the new node
	// and any side-effects from ApplyArrival (e.g. anomaly_at clear).
	updatedBin, updatedErr := e.db.GetBin(*order.BinID)
	if updatedErr != nil {
		e.logFn("engine: get bin %d for delivery arrival event: %v", *order.BinID, updatedErr)
	}
	if updatedBin != nil {
		e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
			Action:      "moved",
			BinID:       updatedBin.ID,
			PayloadCode: updatedBin.PayloadCode,
			FromNodeID:  sourceNodeID,
			ToNodeID:    destNode.ID,
			NodeID:      destNode.ID,
		}})
	}
}

// applyMultiBinArrivalForOrder handles the multi-bin case at delivery time.
//
// Note: previously this path forced staged=false for complex orders with
// WaitIndex > 0 ("operatorConfirmed"). Override removed 2026-04-14 — bins
// arriving at lineside via complex orders now stage like simple orders do.
// See applyBinArrivalForOrder for full context.
func (e *Engine) applyMultiBinArrivalForOrder(order *orders.Order, orderBins []*orders.OrderBin) {
	var instructions []orders.BinArrivalInstruction
	// fromNodeIDs[i] is the source node of instructions[i]. Captured here
	// so the post-arrival BinUpdatedEvent can carry FromNodeID — without it
	// handleKanbanDemand cannot fire produce signals on storage-slot exit.
	var fromNodeIDs []int64

	for _, ob := range orderBins {
		if ob.DestNode == "" {
			continue
		}
		// Claim-based teleport guard (#8): per-bin variant of the same
		// predicate applyBinArrivalForOrder uses for single-bin orders.
		// A late-arriving FINISHED on a stale order, or a bin reclaimed
		// between FINISHED and the engine's processing of it, must NOT
		// be teleported to the junction-table destination. The
		// completion-time path (handleMultiBinCompleted) has the same
		// guard; this matches it at delivery time.
		//
		// Compound children (ParentOrderID != nil) skip the guard for
		// the same overlapping-claim reason documented in
		// applyBinArrivalForOrder.
		if order.ParentOrderID == nil {
			guardBin, err := e.db.GetBin(ob.BinID)
			if err != nil {
				e.logFn("engine: order %d bin %d get for delivery guard: %v", order.ID, ob.BinID, err)
				continue
			}
			if guardBin.ClaimedBy == nil || *guardBin.ClaimedBy != order.ID {
				claimedDesc := "nil"
				if guardBin.ClaimedBy != nil {
					claimedDesc = fmt.Sprintf("%d", *guardBin.ClaimedBy)
				}
				e.logFn("delivery: order=%d bin=%d not claimed by this order (claimed_by=%s) — skipping multi-bin arrival to avoid teleport",
					order.ID, ob.BinID, claimedDesc)
				continue
			}
		}
		destNode, err := e.db.GetNodeByDotName(ob.DestNode)
		if err != nil {
			e.logFn("engine: order %d bin %d dest node %q not found on delivery: %v", order.ID, ob.BinID, ob.DestNode, err)
			continue
		}
		staged, expiresAt := e.resolveNodeStaging(destNode)
		instructions = append(instructions, orders.BinArrivalInstruction{
			BinID:     ob.BinID,
			ToNodeID:  destNode.ID,
			Staged:    staged,
			ExpiresAt: expiresAt,
		})

		// Resolve the per-bin source node (the OrderBin.NodeName is the dot-path
		// of the pickup step). 0 means "unknown source" — kanban will simply not
		// fire the FROM-side check, which is the correct degradation.
		fromNodeID := int64(0)
		if ob.NodeName != "" {
			if srcNode, err := e.db.GetNodeByDotName(ob.NodeName); err == nil && srcNode != nil {
				fromNodeID = srcNode.ID
			}
		}
		fromNodeIDs = append(fromNodeIDs, fromNodeID)
	}

	if len(instructions) == 0 {
		return
	}

	if err := e.db.ApplyMultiBinArrival(instructions); err != nil {
		e.logFn("engine: multi-bin delivery arrival for order %d: %v", order.ID, err)
		return
	}

	for i, inst := range instructions {
		bin, err := e.db.GetBin(inst.BinID)
		if err != nil {
			continue
		}
		e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
			Action:      "moved",
			BinID:       bin.ID,
			PayloadCode: bin.PayloadCode,
			FromNodeID:  fromNodeIDs[i],
			ToNodeID:    inst.ToNodeID,
			NodeID:      inst.ToNodeID,
		}})
	}
}

// handleOrderCompleted runs when Edge confirms receipt. Bin movement already
// happened in handleOrderDelivered, so this is mostly paperwork (compound
// order advancement, cleanup). The bin arrival call is kept as an idempotent
// safety net — if the bin is already at dest, ApplyBinArrival is a no-op.
func (e *Engine) handleOrderCompleted(ev OrderCompletedEvent) {
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil {
		e.logFn("engine: get order %d for completion: %v", ev.OrderID, err)
		return
	}

	// If this is a child of a compound order, advance the parent
	if order.ParentOrderID != nil && e.dispatcher != nil {
		e.dispatcher.HandleChildOrderComplete(order)
	}

	if order.SourceNode == "" || order.DeliveryNode == "" {
		return
	}

	// Check for multi-bin junction table rows (populated by ApplyComplexPlan
	// for orders with 2+ pickup steps). If present, each bin has a per-step
	// destination — use the junction table path instead of the legacy single-bin path.
	orderBins, _ := e.db.ListOrderBins(order.ID)
	if len(orderBins) > 0 {
		e.handleMultiBinCompleted(order, orderBins)
		return
	}

	// Legacy single-bin path: idempotent safety net — bin should already be at
	// dest from handleOrderDelivered, but re-apply in case delivery arrival failed.
	if order.BinID == nil {
		return
	}

	bin, err := e.db.GetBin(*order.BinID)
	if err != nil {
		e.logFn("engine: get bin %d for completion: %v", *order.BinID, err)
		return
	}
	destNode, err := e.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		e.logFn("engine: dest node %s not found for completion: %v", order.DeliveryNode, err)
		return
	}
	sourceNode, _ := e.db.GetNodeByDotName(order.SourceNode)
	sourceNodeID := int64(0)
	if sourceNode != nil {
		sourceNodeID = sourceNode.ID
	}

	// Safety-net invariant: only re-apply this order's arrival if the bin
	// is STILL claimed by THIS order. claimed_by is the canonical "this
	// order owns the bin" pointer; it is cleared atomically in
	// ApplyArrival (normal post-FINISH state) and in
	// FailOrderAtomic / CancelOrderAtomic. So:
	//
	//   - claimed_by == nil   → ApplyArrival already ran (or order
	//                           failed/cancelled); arrival happened or
	//                           is no longer wanted. Skip.
	//   - claimed_by == other → re-claimed by a newer order during the
	//                           FINISH → CONFIRM window. Skip — re-
	//                           applying would clobber the new order
	//                           (the SMN_001 / SMN_002 teleport bug
	//                           originally fixed by checking node_id).
	//   - claimed_by == this  → re-apply. The bin is somewhere
	//                           (source, _TRANSIT, or stale dest), but
	//                           it's still ours, and ApplyArrival is
	//                           idempotent across all of those.
	//
	// Pre-Phase-2 this used `bin.NodeID == sourceNode.ID` as a proxy
	// for "still ours" — true because the bin physically stayed at
	// source until FINISH. Phase 2 transit semantics break that proxy
	// (the bin is at _TRANSIT during in-flight, not at source), so the
	// guard now reads claimed_by directly. Same intent, narrower
	// invariant — also correctly handles the rare case where the bin
	// happens to still be at the same source node but has been re-
	// claimed by another order (which the node-based predicate
	// would have falsely accepted).
	//
	// Compound children (ParentOrderID != nil) skip the guard: the
	// same multi-step plan that touches a bin in multiple legs claims
	// it for the LAST leg only, so interim children's safety-net runs
	// must not check claimed_by. See applyBinArrivalForOrder for the
	// long-form rationale.
	if order.ParentOrderID == nil {
		if bin.ClaimedBy == nil || *bin.ClaimedBy != order.ID {
			e.dbg("completion: bin %d not claimed by order %d — skipping safety-net arrival", *order.BinID, order.ID)
			return
		}
	}

	// Bin still at source — apply arrival as recovery from a missed FINISH

	staged, expiresAt := e.resolveNodeStaging(destNode)

	// Note: see applyBinArrivalForOrder for the override-removal context.
	// Same overrides existed here in the safety-net path and were removed
	// for the same reason.

	evicted, err := e.binService.ApplyArrival(*order.BinID, destNode.ID, staged, expiresAt)
	if err != nil {
		e.logFn("engine: apply bin arrival for order %d bin %d: %v", order.ID, *order.BinID, err)
		return
	}
	if evicted {
		e.logFn("WARN: delivery of bin %d to %s evicted a stale bin record there — a successful delivery proved the slot empty; the stale bin is at _TRANSIT, recover via the anomalies page", *order.BinID, order.DeliveryNode)
	}

	// Emit bin contents changed
	updatedBin, binErr := e.db.GetBin(*order.BinID)
	if binErr != nil {
		e.logFn("engine: get bin %d for completion event: %v", *order.BinID, binErr)
	}
	if updatedBin != nil {
		e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
			Action:      "moved",
			BinID:       updatedBin.ID,
			PayloadCode: updatedBin.PayloadCode,
			FromNodeID:  sourceNodeID,
			ToNodeID:    destNode.ID,
			NodeID:      destNode.ID,
		}})
	}
}

// handleMultiBinCompleted processes completion for orders with multiple claimed bins.
// Each bin is moved to its per-step destination (from the order_bins junction table)
// in a single atomic transaction. Idempotent — skips bins already at their destination
// (normal case when applyMultiBinArrivalForOrder already ran at delivery time).
func (e *Engine) handleMultiBinCompleted(order *orders.Order, orderBins []*orders.OrderBin) {
	var instructions []orders.BinArrivalInstruction
	// fromNodeIDs[i] is the source node of instructions[i] — same purpose as
	// in applyMultiBinArrivalForOrder: keep FromNodeID intact so kanban can
	// fire on storage-slot exit when this safety-net path actually moves a bin.
	var fromNodeIDs []int64

	// Note: previously had an "operatorConfirmed" override forcing staged=false
	// for complex orders with WaitIndex > 0. Removed 2026-04-14 — see
	// applyBinArrivalForOrder for context.

	for _, ob := range orderBins {
		if ob.DestNode == "" {
			e.logFn("engine: order %d bin %d has no dest_node in order_bins — skipping", order.ID, ob.BinID)
			continue
		}
		destNode, err := e.db.GetNodeByDotName(ob.DestNode)
		if err != nil {
			e.logFn("engine: order %d bin %d dest node %q not found: %v", order.ID, ob.BinID, ob.DestNode, err)
			continue
		}

		// Safety-net invariant: only re-apply this leg's arrival if the
		// bin is STILL claimed by THIS order. Same predicate as the
		// single-bin path — see the long comment there for the
		// SMN_001 / Phase 2 transit-semantics rationale. Compound
		// children skip the guard (overlapping claims by design).
		if order.ParentOrderID == nil {
			bin, err := e.db.GetBin(ob.BinID)
			if err != nil {
				e.logFn("engine: order %d bin %d get for safety-net guard: %v", order.ID, ob.BinID, err)
				continue
			}
			if bin.ClaimedBy == nil || *bin.ClaimedBy != order.ID {
				e.dbg("multi-bin completion: bin %d not claimed by order %d — skipping safety-net arrival", ob.BinID, order.ID)
				continue
			}
		}

		staged, expiresAt := e.resolveNodeStaging(destNode)
		instructions = append(instructions, orders.BinArrivalInstruction{
			BinID:     ob.BinID,
			ToNodeID:  destNode.ID,
			Staged:    staged,
			ExpiresAt: expiresAt,
		})

		// Capture the per-bin source node before we move it so the post-arrival
		// event still has it. The OrderBin.NodeName is the pickup step's dot-path.
		fromNodeID := int64(0)
		if ob.NodeName != "" {
			if srcNode, err := e.db.GetNodeByDotName(ob.NodeName); err == nil && srcNode != nil {
				fromNodeID = srcNode.ID
			}
		}
		fromNodeIDs = append(fromNodeIDs, fromNodeID)
	}

	// Junction rows are deleted only when the order has reached a terminal
	// status (confirmed / failed / cancelled). The Stage 10 action map
	// fires fireCompleted on (X, delivered) AND on (delivered, confirmed),
	// so this handler runs twice per order. Deleting on the first
	// (delivered) call would lose the per-bin destination data the
	// sibling handleOrderDelivered path needs on the same status change;
	// keeping the rows alive until terminal lets every completion firing
	// take the multi-bin idempotent path consistently. The terminal
	// transition (handled by HandleOrderReceipt's MarkConfirmed) is the
	// natural cleanup point — by then no more re-runs of this handler
	// will fire for the order.
	if protocol.IsTerminal(order.Status) {
		defer e.db.DeleteOrderBins(order.ID)
	}

	if len(instructions) == 0 {
		e.dbg("multi-bin completion: order %d all bins already at dest — skipping arrival", order.ID)
		return
	}

	if err := e.db.ApplyMultiBinArrival(instructions); err != nil {
		e.logFn("engine: multi-bin arrival for order %d: %v", order.ID, err)
		return
	}

	// Emit BinUpdatedEvent only for bins that actually moved
	for i, inst := range instructions {
		bin, err := e.db.GetBin(inst.BinID)
		if err != nil {
			e.logFn("engine: get bin %d for multi-bin event: %v", inst.BinID, err)
			continue
		}
		e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
			Action:      "moved",
			BinID:       bin.ID,
			PayloadCode: bin.PayloadCode,
			FromNodeID:  fromNodeIDs[i],
			ToNodeID:    inst.ToNodeID,
			NodeID:      inst.ToNodeID,
		}})
	}

	e.logFn("engine: order %d multi-bin completion: %d bins moved", order.ID, len(instructions))
}
