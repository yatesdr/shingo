package dispatch

import (
	"log"

	"shingo/protocol"
	"shingocore/store/loaders"
	"shingocore/store/orders"
)

// loader_place.go — the PARK side of the dedicated home loader, the inverse of the
// source side (loader_source.go). When a dedicated-loader changeover returns a bin
// from a home position, Core decides where it lands: its HOME if provably free,
// else a free buffer slot (home_kind='buffer', the M1 representation the source side
// already pools), else drain (the configured outbound — unchanged). Source and park
// use the SAME Core representation, so a parked partial is re-sourced by the same
// pool and the loop closes end-to-end.
//
// LOCUS — Core is the single authority. The Edge ships the evac order with
// DeliveryNode="" and holds no authoritative bin-landing record; Core resolves the
// dropoff here and the existing release-time redirect overlay (patchRedirectSegments)
// carries it to the fleet. Divergence-free.
//
// NEVER-2N — the park's placement MUST consult the Core in-flight authority (the
// SAME order-truth restock gates on, CountInFlightOrdersByDeliveryNode /
// planning_service.go's CheckDropoffCapacity) — NEVER a bespoke count. Committing
// DeliveryNode=home makes this order in-flight to the home, so a later restock's
// own gate sees it and yields; and if a restock got there first, this read sees it
// and yields to buffer. A lost race requeues (scanner replay), the same contract
// every dropoff has. Do NOT route through the Edge reserveLoaderBins seam (wrong
// store, re-introduces divergence) and do NOT add ClaimSlot here.
//
// The home occupancy read is IN-FLIGHT ONLY by design: the only bin physically at
// the home is the evac's own, which is leaving, so CheckDropoffCapacity's bin-count
// half would force buffer every time (the physical half is vacuous for the park).
// The buffer read is FULL CheckDropoffCapacity — a buffer legitimately holds a
// parked partial, so its physical occupancy is real and must block.
func (d *Dispatcher) placeForDedicatedLoader(order *orders.Order, steps []resolvedStep) {
	// Pattern A: SourceNode is a home position (produce-side return).
	// Pattern B: DeliveryNode is a home position (consume-side removal leg).
	// Both route to the same home/buffer/drain logic; the only structural
	// difference is that Pattern A guards against same-order double-commit
	// (orderDeliversTo) while Pattern B does not — delivering to the home IS
	// the intent for the removal leg, and the single-robot-swap shape can't
	// produce a same-order conflict here.

	if order.SourceNode != "" && order.DeliveryNode == "" {
		// Pattern A: pick up FROM a home with no explicit delivery yet → route the
		// return. Guard on DeliveryNode=="" because supply legs also pick from a
		// home node but carry an explicit delivery (the line node); without the guard
		// Pattern A overwrites their delivery with the home, making the order
		// circular (pickup=SMN_014, deliver=SMN_014) and Core skips it.
		srcNode, err := d.db.GetNodeByDotName(order.SourceNode)
		if err != nil || srcNode == nil {
			return
		}
		home, err := d.db.GetLoaderHomeByPositionNode(srcNode.ID)
		if err != nil || home == nil {
			return
		}
		loader, err := d.db.GetLoader(home.LoaderID)
		if err != nil || loader == nil || loader.Layout != loaders.LayoutDedicatedPositions {
			return
		}
		homeName := srcNode.Name
		if !orderDeliversTo(steps, homeName) {
			inFlight, ierr := d.db.CountInFlightOrdersByDeliveryNodeExcluding(homeName, order.ID)
			if ierr == nil && inFlight == 0 {
				d.setParkDestination(order, homeName, "home")
				return
			}
		}
		d.placeForLoader(order, home.LoaderID, homeName)
		return
	}

	if order.DeliveryNode != "" {
		// Pattern B: explicit DeliveryNode is a home position (consume-side removal
		// leg where outbound_destination = inbound_source). The home is where the
		// removal robot is returning the evac bin; route home-first, else buffer.
		destNode, err := d.db.GetNodeByDotName(order.DeliveryNode)
		if err != nil || destNode == nil {
			return
		}
		home, err := d.db.GetLoaderHomeByPositionNode(destNode.ID)
		if err != nil || home == nil {
			return
		}
		loader, err := d.db.GetLoader(home.LoaderID)
		if err != nil || loader == nil || loader.Layout != loaders.LayoutDedicatedPositions {
			return
		}
		homeName := destNode.Name
		// No same-order guard here: delivering to the home is the intent.
		inFlight, ierr := d.db.CountInFlightOrdersByDeliveryNodeExcluding(homeName, order.ID)
		if ierr == nil && inFlight == 0 {
			d.setParkDestination(order, homeName, "home")
			return
		}
		d.placeForLoader(order, home.LoaderID, homeName)
	}
}

// placeForLoader routes to a free buffer slot for the given loader, or drains.
// Shared by Pattern A and Pattern B after the home-first check fails.
func (d *Dispatcher) placeForLoader(order *orders.Order, loaderID int64, homeName string) {
	members, merr := d.db.ListLoaderHomes(loaderID)
	if merr != nil {
		log.Printf("dispatch: place loader %d members: %v — draining order %d", loaderID, merr, order.ID)
		return
	}
	for _, m := range members {
		if m.Kind != loaders.HomeKindBuffer {
			continue
		}
		bn, nerr := d.db.GetNode(m.PositionNodeID)
		if nerr != nil || bn == nil {
			continue
		}
		if blocked, _ := CheckDropoffCapacity(d.db, bn.Name, order.ID); blocked {
			continue
		}
		d.setParkDestination(order, bn.Name, "buffer")
		return
	}
	d.dbg("place: loader home %s not free and no free buffer — draining order %d", homeName, order.ID)
}

// orderDeliversTo reports whether any dropoff step in this order targets node. Used
// to catch the single-robot-swap case where the SAME order delivers the new style to
// the home — that bin claims the home, so the returning partial must go to buffer.
func orderDeliversTo(steps []resolvedStep, node string) bool {
	for _, s := range steps {
		if s.Action == protocol.ActionDropoff && s.Node == node {
			return true
		}
	}
	return false
}

// setParkDestination commits the chosen dropoff onto the order header. The release-
// time patchRedirectSegments overlays this onto the final dropoff step, so the fleet
// follows it; persisting it now also makes the order in-flight to the chosen node so
// a concurrent restock's gate observes it (the never-2N handshake).
func (d *Dispatcher) setParkDestination(order *orders.Order, node, kind string) {
	if order.DeliveryNode == node {
		return // already there — idempotent across scanner replays
	}
	if err := d.db.UpdateOrderDeliveryNode(order.ID, node); err != nil {
		log.Printf("dispatch: place park dest order %d → %s: %v", order.ID, node, err)
		return
	}
	order.DeliveryNode = node
	d.dbg("place: order %d returning partial → %s (%s)", order.ID, node, kind)
}
