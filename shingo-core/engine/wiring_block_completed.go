// wiring_block_completed.go — Phase 2 of the bin-transit-state project.
//
// Engine handler for EventBlockCompleted (fired by the rds.Poller when a
// per-block state transitions to FINISHED while the parent order is
// still mid-flight). For pickup blocks, drives the bin claimed at that
// step onto the synthetic _TRANSIT node so the source slot is freed
// immediately — the slot-vacancy signal queued orders need to unblock.
//
// Block-kind routing:
//
//   - pickup-shaped (BinTask=Load, "pickup", or any operation that
//     loads a goods onto the robot): bin transitions to _TRANSIT.
//   - dropoff-shaped: INTERMEDIATE dropoffs (a midway storage slot, not the
//     order's final delivery) fire bin arrival immediately via
//     handleStoreBlockCompleted, so the slot reflects the physical bin the
//     moment the store completes — and a mid-flight cancel leaves the bin at
//     its slot instead of stranded at _TRANSIT (with the slot reading empty,
//     a double-store hazard). The FINAL delivery is still driven by
//     handleOrderDelivered when the whole order reaches FINISHED (that path
//     is robust, idempotent, and also sends the Edge OrderDelivered
//     notification), so we deliberately do not race it here.
//   - waits, scripts, navigation-only: no-op.
//
// Idempotence: BinService.MoveToTransit is a no-op when the bin is
// already at _TRANSIT, so duplicate or replayed events are safe.
//
// Failure mode: if order_bins lookup misses for the block's location
// (concrete bin couldn't be claimed at order-creation time, or the
// junction row was never written for a single-bin complex order), we
// fall back to order.BinID. That covers the simpler case where there's
// only one bin per order. If neither path resolves a bin, we log and
// drop — the bin's source slot stays nominally occupied until delivery,
// which is the pre-Phase-2 behavior. Acceptable degradation.

package engine

import (
	"strings"
	"time"

	"shingo/protocol"
	"shingocore/store/orders"
)

// handleBlockCompleted is called from wiring.go's EventBlockCompleted
// subscription. Routes the block by kind and drives the corresponding
// bin lifecycle transition: pickups free the source slot (→ _TRANSIT),
// intermediate storage dropoffs record the bin at its slot immediately.
func (e *Engine) handleBlockCompleted(ev BlockCompletedEvent) {
	switch {
	case isPickupBlock(ev.BinTask):
		e.handlePickupBlockCompleted(ev)
	case isDropoffBlock(ev.BinTask):
		e.handleStoreBlockCompleted(ev)
	}
}

// handlePickupBlockCompleted drives the bin claimed at a pickup block onto
// the synthetic _TRANSIT node so the source slot frees immediately — the
// slot-vacancy signal queued orders need to unblock.
func (e *Engine) handlePickupBlockCompleted(ev BlockCompletedEvent) {
	binID, stepIndex, fromNodeID, ok := e.resolvePickupBin(ev.OrderID, ev.Location)
	if !ok {
		e.logFn("transit: order %d block %s @ %s — no claimed bin matched; bin will move to dest at delivery (pre-Phase-2 behavior)",
			ev.OrderID, ev.BlockID, ev.Location)
		return
	}

	if err := e.binService.MoveToTransit(binID); err != nil {
		e.logFn("transit: MoveToTransit bin %d for order %d: %v", binID, ev.OrderID, err)
		return
	}

	e.dbg("transit: bin %d entered _TRANSIT (order %d, block %s @ %s, step %d)",
		binID, ev.OrderID, ev.BlockID, ev.Location, stepIndex)

	// Item 11: notify Edge that the bin was physically picked up. The
	// SEND PARTIAL BACK flow needs this signal to flush the released
	// bin's delta accumulator and advance the active claim. We publish
	// for every pickup (not just partial-back) — the Edge handler
	// no-ops gracefully when the order doesn't match a tracked bin.
	if order, err := e.db.GetOrder(ev.OrderID); err == nil && order != nil && order.StationID != "" {
		if err := e.SendDataToEdge(protocol.SubjectBinPickedUp, order.StationID, &protocol.BinPickedUp{
			OrderUUID:  order.EdgeUUID,
			BinID:      binID,
			Location:   ev.Location,
			PickedUpAt: time.Now().UTC(),
		}); err != nil {
			e.logFn("transit: send BinPickedUp bin %d order %d: %v", binID, ev.OrderID, err)
		}
	}

	e.Events.Emit(Event{Type: EventBinEnteredTransit, Payload: BinEnteredTransitEvent{
		BinID:      binID,
		OrderID:    ev.OrderID,
		FromNodeID: fromNodeID,
		StepIndex:  stepIndex,
	}})
}

// resolvePickupBin finds the bin claimed at the given pickup-block
// location. Returns binID, stepIndex, fromNodeID (the source node ID
// the bin is leaving), and ok.
//
// Lookup order:
//  1. Multi-bin complex order: order_bins junction. Match by
//     NodeName == location AND Action == "pickup". When multiple
//     pickups share a location (rare — same supermarket lane twice
//     in one swap), pick the earliest unmoved one (lowest step_index
//     whose bin's NodeID still equals the source node — others have
//     already transitioned).
//  2. Single-bin order fallback: order.BinID.
func (e *Engine) resolvePickupBin(orderID int64, location string) (binID int64, stepIndex int, fromNodeID int64, ok bool) {
	// LOAD-BEARING (same contract as shingo-edge/engine/handler_bin_picked_up.go):
	// `location` arrives from BlockCompletedEvent.Location, originally
	// an un-normalized RDS vendor string. `ob.NodeName` comes from
	// the order_bins junction, populated at order-creation time from
	// nodes.name. All Core write paths trim on write today, but
	// tainted rows from a pre-trim install (or any future write path
	// that bypasses the trim) would silently break this filter,
	// leaving the bin nominally occupied at source.
	//
	// Defensive TrimSpace on both sides. Trim only — NOT case-fold;
	// case mismatch is a real config error.
	locationTrimmed := strings.TrimSpace(location)
	// Multi-bin path: junction table.
	rows, err := e.db.ListOrderBins(orderID)
	if err == nil && len(rows) > 0 {
		for _, ob := range rows {
			if ob.Action != "pickup" {
				continue
			}
			if strings.TrimSpace(ob.NodeName) != locationTrimmed {
				continue
			}
			bin, err := e.db.GetBin(ob.BinID)
			if err != nil || bin == nil {
				continue
			}
			// Skip bins that have already transitioned (their NodeID
			// is no longer at the source). This handles duplicate
			// pickup events for repeated-location orders.
			srcNode, srcErr := e.db.GetNodeByDotName(ob.NodeName)
			if srcErr != nil || srcNode == nil {
				continue
			}
			if bin.NodeID == nil || *bin.NodeID != srcNode.ID {
				continue
			}
			return ob.BinID, ob.StepIndex, srcNode.ID, true
		}
	}

	// Single-bin fallback.
	order, err := e.db.GetOrder(orderID)
	if err != nil || order == nil || order.BinID == nil {
		return 0, 0, 0, false
	}
	bin, err := e.db.GetBin(*order.BinID)
	if err != nil || bin == nil {
		return 0, 0, 0, false
	}
	from := int64(0)
	if bin.NodeID != nil {
		from = *bin.NodeID
	}
	return *order.BinID, 0, from, true
}

// handleStoreBlockCompleted records a bin at its destination slot the moment
// an INTERMEDIATE dropoff block finishes — the store dual of the
// pickup→_TRANSIT transition. Without it, a bin dropped at a midway storage
// slot (the "store the full bin, then go retrieve" leg of a complex swap)
// stays recorded at _TRANSIT until the WHOLE order reaches FINISHED. For the
// duration the slot reads empty, and if the order is cancelled mid-flight the
// bin is stranded at _TRANSIT while a downstream order can dispatch a second
// bin into the physically-occupied slot (the Hopkinsville #130/#132 divergence).
//
// Scope is deliberately narrow:
//   - Only multi-bin complex orders (order_bins junction populated) have an
//     intermediate dropoff; resolveDropoffBin no-ops for single-bin orders
//     and compound children (no junction rows), leaving their well-tested
//     completion path untouched.
//   - The FINAL delivery (location == order.DeliveryNode) is skipped — it is
//     driven by handleOrderDelivered at whole-order FINISHED, which also ships
//     the Edge OrderDelivered notification; racing it here would buy nothing.
//
// Idempotent: resolveDropoffBin returns only a bin still claimed by this
// order, so an already-delivered (unclaimed) bin or a replayed block event is
// a no-op.
func (e *Engine) handleStoreBlockCompleted(ev BlockCompletedEvent) {
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil || order == nil {
		return
	}
	location := strings.TrimSpace(ev.Location)
	if location == "" || location == strings.TrimSpace(order.DeliveryNode) {
		return // final delivery is recorded at whole-order FINISHED
	}

	binID, ok := e.resolveDropoffBin(order, location)
	if !ok {
		e.dbg("transit: order %d dropoff block %s @ %s — no in-flight claimed bin matched; store recorded at order FINISHED instead",
			ev.OrderID, ev.BlockID, ev.Location)
		return
	}

	destNode, err := e.db.GetNodeByDotName(location)
	if err != nil || destNode == nil {
		e.logFn("transit: order %d store dropoff @ %s — dest node lookup failed: %v", ev.OrderID, ev.Location, err)
		return
	}

	staged, expiresAt := e.resolveNodeStaging(destNode)
	if err := e.binService.ApplyArrival(binID, destNode.ID, staged, expiresAt); err != nil {
		e.logFn("transit: order %d intermediate store arrival bin %d -> %s: %v", order.ID, binID, ev.Location, err)
		return
	}

	e.dbg("transit: bin %d stored at %s on dropoff (order %d, block %s) — slot now reflects the physical bin",
		binID, ev.Location, order.ID, ev.BlockID)

	updated, uerr := e.db.GetBin(binID)
	if uerr != nil {
		e.logFn("transit: get bin %d after intermediate store arrival: %v", binID, uerr)
	}
	if updated != nil {
		e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
			Action:      "moved",
			BinID:       updated.ID,
			PayloadCode: updated.PayloadCode,
			// FromNodeID intentionally 0: the bin arrives from _TRANSIT, not a
			// real slot, so kanban's produce-on-storage-exit check must not fire.
			ToNodeID: destNode.ID,
			NodeID:   destNode.ID,
		}})
	}
}

// resolveDropoffBin finds the bin this order dropped at `location` via the
// order_bins junction (dest_node == location). Among matching rows it returns
// the bin still claimed by the order — the one actually in flight for this
// leg; a bin already delivered to the same dest is unclaimed and skipped,
// which makes the caller idempotent against duplicate/replayed block events.
// Returns false when no junction rows exist (single-bin orders, compound
// children) or none match.
func (e *Engine) resolveDropoffBin(order *orders.Order, location string) (int64, bool) {
	rows, err := e.db.ListOrderBins(order.ID)
	if err != nil || len(rows) == 0 {
		return 0, false
	}
	for _, ob := range rows {
		if strings.TrimSpace(ob.DestNode) != location {
			continue
		}
		bin, err := e.db.GetBin(ob.BinID)
		if err != nil || bin == nil {
			continue
		}
		if bin.ClaimedBy != nil && *bin.ClaimedBy == order.ID {
			return ob.BinID, true
		}
	}
	return 0, false
}

// isPickupBlock returns true when a block's BinTask designates a
// pickup-shaped operation. The vendor's BinTask vocabulary is
// roboshop-configurable (the storage-bin-location action key), so we
// match on common patterns rather than an exact set.
func isPickupBlock(binTask string) bool {
	if binTask == "" {
		return false
	}
	t := strings.ToLower(binTask)
	switch t {
	case "load", "pickup", "pick", "jackload", "jack_load", "fork_load", "rollerload":
		return true
	}
	// Substring fallback: any binTask containing "load" or "pick" but
	// NOT "unload" / "drop" / "release" is treated as pickup-shaped.
	if strings.Contains(t, "unload") || strings.Contains(t, "drop") || strings.Contains(t, "release") {
		return false
	}
	if strings.Contains(t, "load") || strings.Contains(t, "pick") {
		return true
	}
	return false
}

// isDropoffBlock returns true when a block's BinTask designates a
// dropoff-shaped operation — the store/deliver dual of isPickupBlock. Same
// roboshop-configurable-vocabulary caveat, so it mixes exact-match with a
// substring fallback on "unload"/"drop"/"release".
func isDropoffBlock(binTask string) bool {
	if binTask == "" {
		return false
	}
	t := strings.ToLower(binTask)
	switch t {
	case "unload", "dropoff", "drop", "jackunload", "jack_unload", "fork_unload", "rollerunload", "release":
		return true
	}
	if strings.Contains(t, "unload") || strings.Contains(t, "drop") || strings.Contains(t, "release") {
		return true
	}
	return false
}
