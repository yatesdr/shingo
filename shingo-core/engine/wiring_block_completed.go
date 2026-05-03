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
//   - dropoff-shaped: no-op here. Delivery arrival is still driven by
//     EventOrderStatusChanged → handleOrderDelivered → applyBinArrival
//     when the whole order reaches FINISHED. We could fire arrival per
//     dropoff block here for tighter latency on multi-bin complex
//     orders, but the current arrival path is robust and idempotent —
//     adding a per-block path doubles the surface for no observable
//     win. Reconsider if a future use case needs sub-order arrival.
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
)

// handleBlockCompleted is called from wiring.go's EventBlockCompleted
// subscription. Routes the block by kind and drives the corresponding
// bin lifecycle transition.
func (e *Engine) handleBlockCompleted(ev BlockCompletedEvent) {
	if !isPickupBlock(ev.BinTask) {
		return
	}

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
	// Multi-bin path: junction table.
	rows, err := e.db.ListOrderBins(orderID)
	if err == nil && len(rows) > 0 {
		for _, ob := range rows {
			if ob.Action != "pickup" {
				continue
			}
			if ob.NodeName != location {
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
