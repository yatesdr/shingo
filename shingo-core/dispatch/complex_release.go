package dispatch

import (
	"encoding/json"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store/orders"
)

// HandleOrderRelease processes a release request for a staged (dwelling) order.
// Multi-wait support: the order's WaitIndex tracks how many wait points have
// been consumed. Each release emits only the next segment (steps between
// consecutive waits) and increments the index. The fleet order stays staged
// (complete=false) until the final segment is released.
func (d *Dispatcher) HandleOrderRelease(env *protocol.Envelope, p *protocol.OrderRelease) {
	d.dbg("order release: station=%s uuid=%s", env.Src.Station, p.OrderUUID)

	order, ok := d.getOwnedOrder(env, p.OrderUUID)
	if !ok {
		d.sendError(env, p.OrderUUID, "not_found", "order not found or access denied")
		return
	}

	// Precondition: order must be staged or in_transit. InTransit is accepted
	// for duplicate fan-out from Edge's consolidated two-robot release and for
	// multi-wait re-release. The real duplicate gate is splitSegment returning nil.
	if order.Status != StatusStaged && order.Status != StatusInTransit {
		d.sendError(env, p.OrderUUID, "invalid_state",
			fmt.Sprintf("order must be staged or in_transit to release, got %s", order.Status))
		return
	}

	if err := d.syncManifestForRelease(env, order, p); err != nil {
		return
	}

	var steps []resolvedStep
	if err := json.Unmarshal([]byte(order.StepsJSON), &steps); err != nil {
		d.sendError(env, p.OrderUUID, "internal_error", "failed to parse stored steps")
		return
	}

	segment, moreWaits, blockOffset := splitSegment(steps, order.WaitIndex)
	if segment == nil {
		if order.Status == StatusInTransit {
			d.dbg("complex release: order %d already in_transit with wait_index %d past final wait — no-op",
				order.ID, order.WaitIndex)
			return
		}
		d.sendError(env, p.OrderUUID, "invalid_state",
			fmt.Sprintf("wait_index %d exceeds number of waits in order", order.WaitIndex))
		return
	}

	d.patchRedirectSegments(segment, order, moreWaits)
	d.dispatchFleetRelease(env, order, segment, moreWaits, blockOffset)
}

// syncManifestForRelease performs the late-bind bin manifest sync at release
// time. p.RemainingUOP carries the operator's intent: nil = no change
// (legacy/Order-A path), 0 = bin empty (NOTHING PULLED), >0 = partial
// (SEND PARTIAL BACK). Must run before backend.ReleaseOrder so the fleet
// doesn't proceed against an inconsistent manifest.
//
// When order.BinID is nil (claimComplexBins missed), falls back to locating
// the bin at order.ProcessNode/SourceNode — the ALN_002 incident path.
func (d *Dispatcher) syncManifestForRelease(env *protocol.Envelope, order *orders.Order, p *protocol.OrderRelease) error {
	if p.Disposition != nil {
		var binIDForAudit int64
		if order.BinID != nil {
			binIDForAudit = *order.BinID
		} else if id, ok := d.findFallbackBinAtSource(order); ok {
			binIDForAudit = id
		}
		if binIDForAudit != 0 {
			if err := d.binManifest.AuditReleaseOverride(binIDForAudit, order.ID, p.Disposition, p.CalledBy); err != nil {
				log.Printf("dispatch: release override audit for order %d (bin %d): %v",
					order.ID, binIDForAudit, err)
			}
		}
	}

	if p.RemainingUOP == nil {
		return nil
	}

	var kind protocol.UOPDispositionKind
	if p.Disposition != nil {
		kind = p.Disposition.Kind
	}

	if order.BinID != nil {
		if err := d.binManifest.SyncOrClearForReleased(*order.BinID, order.ID, p.RemainingUOP, kind, p.CalledBy); err != nil {
			log.Printf("dispatch: manifest sync on release for order %d: %v", order.ID, err)
			d.sendError(env, p.OrderUUID, "manifest_sync_failed", err.Error())
			return err
		}
		return nil
	}

	if order.ProcessNode == "" && order.SourceNode == "" {
		log.Printf("dispatch: release for order %d had nil BinID and no ProcessNode/SourceNode — manifest will not clear",
			order.ID)
		return nil
	}

	fallbackLookup := order.ProcessNode
	if fallbackLookup == "" {
		fallbackLookup = order.SourceNode
	}

	binID, ok := d.findFallbackBinAtSource(order)
	if !ok {
		log.Printf("dispatch: release for order %d had nil BinID and no fallback bin at %s — manifest will not clear",
			order.ID, fallbackLookup)
		return nil
	}

	log.Printf("dispatch: release for order %d had nil BinID; fallback located bin %d at %s",
		order.ID, binID, fallbackLookup)

	if err := d.binManifest.SyncOrClearForReleasedNoOwner(binID, order.ID, p.RemainingUOP, p.CalledBy); err != nil {
		log.Printf("dispatch: fallback manifest sync on release for order %d (bin %d): %v", order.ID, binID, err)
		d.sendError(env, p.OrderUUID, "manifest_sync_failed", err.Error())
		return err
	}
	return nil
}

// patchRedirectSegments replaces the last dropoff node in the segment with
// the order's current DeliveryNode when the order was redirected while staged.
// Only patches the final segment — intermediate segments have legitimate
// dropoffs that differ from the final destination.
func (d *Dispatcher) patchRedirectSegments(segment []resolvedStep, order *orders.Order, moreWaits bool) {
	if order.DeliveryNode == "" || moreWaits {
		return
	}
	for i := len(segment) - 1; i >= 0; i-- {
		if segment[i].Action == protocol.ActionDropoff {
			if segment[i].Node != order.DeliveryNode {
				d.dbg("complex release: patching segment dropoff %s -> %s (redirect)", segment[i].Node, order.DeliveryNode)
				segment[i].Node = order.DeliveryNode
			}
			break
		}
	}
}

// dispatchFleetRelease converts the segment to fleet blocks, submits them to
// the fleet backend, advances the wait index, and transitions the order
// lifecycle. Called after manifest sync and segment extraction have succeeded.
func (d *Dispatcher) dispatchFleetRelease(env *protocol.Envelope, order *orders.Order, segment []resolvedStep, moreWaits bool, blockOffset int) {
	blocks := stepsToBlocks(order.VendorOrderID, segment, blockOffset)
	complete := !moreWaits

	d.dbg("complex release: order=%d vendor=%s wait_index=%d adding %d blocks complete=%v",
		order.ID, order.VendorOrderID, order.WaitIndex, len(blocks), complete)

	if err := d.backend.ReleaseOrder(order.VendorOrderID, blocks, complete); err != nil {
		log.Printf("dispatch: fleet release order failed: %v", err)
		d.sendError(env, order.EdgeUUID, "fleet_failed", err.Error())
		return
	}

	newWaitIndex := order.WaitIndex + 1
	if err := d.db.UpdateOrderWaitIndex(order.ID, newWaitIndex); err != nil {
		log.Printf("dispatch: update order %d wait_index to %d: %v", order.ID, newWaitIndex, err)
	}

	if err := d.lifecycle.Release(order, "dispatcher"); err != nil {
		if IsIllegalTransition(err) {
			log.Printf("dispatch: order %d became un-releasable mid-flight (status=%s): %v", order.ID, order.Status, err)
		} else {
			log.Printf("dispatch: release order %d from staging: %v", order.ID, err)
		}
	}
	log.Printf("dispatch: complex order %d released with %d additional blocks (wait %d, complete=%v)",
		order.ID, len(blocks), order.WaitIndex, complete)
}

// findFallbackBinAtSource locates a bin to manifest-sync when the
// caller's order.BinID is nil at release time. Returns (binID, true)
// on success.
//
// Lookup order:
//
//  1. **Claim-first** (Phase 3 of bin-transit-state): query bins where
//     claimed_by = order.ID. The claim is the canonical "this order's
//     bin(s)" pointer, independent of where the bin physically sits.
//     Critical under transit semantics — a bin mid-flight has
//     node_id=_TRANSIT, not its original source, so a node-only lookup
//     would miss it. Multi-bin orders may return several rows; if
//     ProcessNode is set we prefer the bin currently at the line node
//     (the operator's release target), else the first by ID.
//
//  2. **Node fallback**: search bins physically at ProcessNode (the
//     line) or SourceNode (the first pickup) for orders without an
//     active claim — pre-existing behavior. Selects payload-matching
//     bin first, then any non-empty bin at the node.
//
// Pre-Phase-3 this was node-only and would silently miss bins that
// claimComplexBins HAD claimed but UpdateOrderBinID failed to persist
// (DB-write race), and miss any in-transit bin during the rare case
// where release fires after pickup has already happened.
func (d *Dispatcher) findFallbackBinAtSource(order *orders.Order) (int64, bool) {
	// 1) Claim-first.
	claimed, err := d.db.ListBinsByClaim(order.ID)
	if err == nil && len(claimed) > 0 {
		// Multi-bin orders: prefer the bin at ProcessNode (the line —
		// where the operator's release intent applies). Falls back to
		// the first by ID if no per-line preference resolves.
		if order.ProcessNode != "" && len(claimed) > 1 {
			if procNode, perr := d.db.GetNodeByDotName(order.ProcessNode); perr == nil && procNode != nil {
				for _, b := range claimed {
					if b.NodeID != nil && *b.NodeID == procNode.ID {
						return b.ID, true
					}
				}
			}
		}
		return claimed[0].ID, true
	}

	// 2) Node fallback — only reached when no bin is claimed by this
	// order at all (claimComplexBins missed entirely, or order is in
	// a partial-state we can't reason about from claims).
	lookupNode := order.ProcessNode
	if lookupNode == "" {
		lookupNode = order.SourceNode
	}
	srcNode, err := d.db.GetNodeByDotName(lookupNode)
	if err != nil || srcNode == nil {
		return 0, false
	}
	bins, err := d.db.ListBinsByNode(srcNode.ID)
	if err != nil || len(bins) == 0 {
		return 0, false
	}
	// Prefer a payload-matching bin (correct in the multi-bin storage case).
	if order.PayloadCode != "" {
		for _, b := range bins {
			if b.PayloadCode == order.PayloadCode {
				return b.ID, true
			}
		}
	}
	// No payload match — fall back to the first bin with a non-empty
	// manifest. Skip already-cleared bins to avoid double-clearing a
	// stale empty.
	for _, b := range bins {
		if b.PayloadCode != "" {
			return b.ID, true
		}
	}
	return 0, false
}
