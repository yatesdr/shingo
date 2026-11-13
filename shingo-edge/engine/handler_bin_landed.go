// handler_bin_landed.go — the landing (Lane H, H.1, H.4, H.5, H.7).
//
// Core announces that a carrier came to rest at a node on
// transit.bin_landed, as an outbox row in the transaction that moved it
// (shingo-core service.BinService.announceLanding), addressed to the station
// that binds there. This is the Edge's ONE door for binding a carrier Core
// placed: order.delivered closes the order and binds nothing (H.5), and the
// Bound adjustment is an older Core's (H.7), routed through the same verb.

package engine

import (
	"log"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// HandleBinLanded binds the carrier a landing names at the node it names, if
// that node is one of this Edge's process nodes. The bind's refusals are its
// statement's WHERE (processes.BindLanding): a landing whose placement stamp
// is not newer than the seat's last is refused whole, so a redelivered or
// overtaken landing binds nothing (D10); the same bin already bound at an
// equal or newer generation keeps its count (H.4).
//
// THE SEAT RECORDS ITS CART'S PART AT THE LANDING (decision 57), in this
// order: the part the landing's order delivered to the node (orderedPart,
// source delivery): the order knows what it brought; else Core's part for the
// bin, with its known flag (source landing, H.1); else, for a cart neither
// names, the part the position's active claim names, assumed
// (recordBoundCarrier, source assumed). Where Core names a part the order did
// not, Core's stands: it is the bin's record, and the one Core checks every
// count against. A carrier is never recorded as the previous occupant's part.
//
// THE HOLD LANDS ONLY ON A CART AN ORDER OF THIS EDGE'S DELIVERED (D4). The
// landing is ours when it names an order this Edge holds (one it authored, or
// one Core authored and projected to it) that places a bin at this node
// (strangerBy); the seat's hold then replays onto the cart as always. Any
// other landing of another carrier, a cart placed or recorded by hand or a
// recovery set-down naming an order this Edge does not have, drops the
// standing hold in the bind's own transaction (bindEndingHold). That is not
// orderedPart's question: a swap's supply leg brings an empty, for which
// orderedPart answers "" and strangerBy answers ours.
//
// Pi cost per landing at a seat: the node read, the order read (a landing
// naming one), the runtime row's ensure, the runtime read under the count lock
// (bindEndingHold), the bind, the identity write and the placement settle
// (settleCellPlacement's reads); a landing that names no part adds the runtime
// row and its claim (the assumed part).
func (e *Engine) HandleBinLanded(l protocol.BinLanded) {
	node, err := e.db.GetProcessNodeByCoreNodeName(l.CoreNodeName)
	if err != nil || node == nil {
		// Not a seat of this station's: a landing is addressed to the station
		// whose processes claim the node, or the order's station, which may
		// not bind there (a staging node, a market). Nothing to bind.
		e.debugFn("bin_landed: %s is not a process node here — nothing to bind (bin %d)", l.CoreNodeName, l.BinID)
		return
	}
	seq := l.PlacedSeq
	order := e.landingOrder(l.OrderUUID)
	carrier, src := domain.UnknownCarrier(), domain.CarrierFromLanding
	switch part := orderedPart(order, l); {
	case part != "" && (!l.PayloadKnown || l.PayloadCode == part):
		carrier, src = domain.KnownCarrier(domain.LinesidePayloadCode(part)), domain.CarrierFromDelivery
	case l.PayloadKnown:
		carrier = domain.KnownCarrier(domain.LinesidePayloadCode(l.PayloadCode))
	}
	e.landCarrier(node, processes.LandingBind{
		BinID: l.BinID, Epoch: l.Epoch, Count: l.Count, PlacedSeq: &seq,
	}, carrier, src, "landing "+l.OrderUUID, e.strangerBy(order, l.CoreNodeName))
}

// landingOrder is the order a landing names, when this Edge holds it; nil for
// none, an order this Edge does not have, or one it cannot read.
func (e *Engine) landingOrder(orderUUID string) *orders.Order {
	if orderUUID == "" {
		return nil
	}
	o, err := e.db.GetOrderByUUID(orderUUID)
	if err != nil {
		return nil
	}
	return o
}

// orderedPart is the part the landing's order delivered to the node it
// landed at; "" when the landing names no order, the order is not this
// Edge's, it delivers elsewhere (the evacuation leg of a swap) or it brought
// an empty.
func orderedPart(o *orders.Order, l protocol.BinLanded) string {
	if o == nil || o.RetrieveEmpty || o.DeliveryNode != l.CoreNodeName {
		return ""
	}
	return o.PayloadCode
}

// strangerBy answers D4 for a bind at coreNode by order o: "" when the cart is
// ours (an order this Edge holds, which places a bin at the node:
// orderPlacesBinAtAny, which fails closed to ours on steps it cannot read),
// else the actor a dropped hold's row names (holdDropByLanding).
func (e *Engine) strangerBy(o *orders.Order, coreNode string) string {
	if o != nil && e.orderPlacesBinAtAny(o, []string{coreNode}) {
		return ""
	}
	return holdDropByLanding
}

// The actors a hold_dropped row names for a bind that ends a hold (D4).
const (
	holdDropByLanding    = "edge:landing"
	holdDropByManualLoad = "edge:manual-load"
)

// bindEndingHold runs a bind at the seat with what it ends, in the bind's own
// transaction (writeSeatWithDrops; no transaction when it ends nothing):
//
//   - a bind of another carrier than the one bound by a stranger (by names
//     the actor; "" for a cart an order of this Edge's delivered) drops the
//     seat's standing hold (processes.DropSeatHold) with one hold_dropped
//     (no_carrier) row naming by (D4);
//   - any bind of another carrier ends the no-cart stretch: its rows ride the
//     same transaction, naming stretchBy (D6);
//   - with liftStop, a stopped seat's stop is lifted in the same transaction
//     (U1: a hand load, whatever cart it names, and a person's staged bind).
//     A landing passes false: its statement lifts the stop for another cart
//     and keeps it for a re-landing of the cart bound.
//
// binID is the carrier the bind names (nil: none). bind is the bind statement
// on ex and reports whether it landed; a bind that does not land writes
// nothing, row, hold or stop. One runtime read, under the lock. A re-landing of
// the bin already bound (H.4) ends nothing. Caller holds countMu.
func (e *Engine) bindEndingHold(node *processes.Node, binID *int64, by, stretchBy string, liftStop bool,
	bind func(processes.DBTX) (bool, error)) (bool, error) {
	rt, err := e.db.GetProcessNodeRuntime(node.ID)
	if err != nil {
		rt = nil
	}
	another := rt == nil || rt.ActiveBinID == nil || binID == nil || *rt.ActiveBinID != *binID
	var rows, noBin []protocol.HoldDropped
	var pile int64
	if another && by != "" && rt != nil && rt.PendingUOPDelta != 0 {
		pile = rt.PendingUOPDelta
		rows = append(rows, protocol.HoldDropped{PayloadCode: rt.PendingUOPPayload, Qty: pile,
			Reason: protocol.HoldDropNoCarrier, CalledBy: by})
	}
	if another {
		noBin = e.noBinRows(node.ID)
		for i := range noBin {
			noBin[i].CalledBy = stretchBy
		}
	}
	lift := liftStop && rt != nil && rt.HoldStopped
	ok, err := e.writeEndingHold(node, append(rows, noBin...), lift, func(ex processes.DBTX) (bool, error) {
		if ok, err := bind(ex); err != nil || !ok || pile == 0 {
			return ok, err
		}
		return processes.DropSeatHold(ex, node.ID, pile)
	})
	if err == nil && ok {
		e.endNoBinEpisode(node.ID, noBin)
		if pile != 0 {
			e.endUnheldEpisode(node.ID)
		}
	}
	return ok, err
}

// landCarrier is the landing verb's engine half: the bind (uop.Mutator.Land,
// whose refusals are its WHERE), then, only if it landed, the relight of a
// dark sequential pair (lightDarkSequentialPair, on a seat the bind left
// dark), the identity through the lineside doorway (source src, or assumed
// where the bind could not say: recordBoundCarrier) and the
// placement's settle. Reports whether it landed.
//
// THE SETTLE IS LOAD-BEARING. A bind here is the record that a bin is on the
// cell, which a leg that has already left the cell's nodes waits on; whichever
// of the pickup and the placement lands second completes the departure
// (leg_departure.go). The Bound arm fired it; the landing now does.
//
// by is strangerBy's answer for the cart: "" when an order of this Edge's
// delivered it, else the actor of the hold drop the bind writes (D4,
// bindEndingHold).
//
// Callers: HandleBinLanded (stamped), and for one release an older Core's
// Bound (bindAdjustmentFromCore) and delivered (landFromOldCore), unstamped
// (H.7).
func (e *Engine) landCarrier(node *processes.Node, l processes.LandingBind, carrier domain.LinesideCarrier,
	src domain.CarrierSource, why, by string) bool {
	if e.inventoryDelta == nil {
		return false
	}
	// A never-active seat may have no runtime row yet.
	if _, err := e.db.EnsureProcessNodeRuntime(node.ID); err != nil {
		log.Printf("bin_landed: bind bin %d — ensure runtime for node %s: %v", l.BinID, node.CoreNodeName, err)
		return false
	}
	// Under countMu, as every write of a seat's count: a tick that read the
	// seat before the bind is refused by its own WHERE and re-applied to the
	// carrier that landed (applyHoldAndReplay).
	e.countMu.Lock()
	var activePull bool
	landed, err := e.bindEndingHold(node, &l.BinID, by, holdDropByLanding, false, func(ex processes.DBTX) (bool, error) {
		landed, ap, err := e.inventoryDelta.Land(ex, node.ID, l)
		activePull = ap
		return landed, err
	})
	if landed && !activePull {
		e.lightDarkSequentialPair(node)
	}
	e.countMu.Unlock()
	if err != nil {
		log.Printf("bin_landed: bind bin %d at %s (%s): %v", l.BinID, node.CoreNodeName, why, err)
		return false
	}
	if !landed {
		log.Printf("bin_landed: bin %d at %s epoch %d (%s) refused by the seat — a landing not newer than the "+
			"one it holds, a carrier that has left, or an unstamped bind onto a different carrier; nothing written",
			l.BinID, node.CoreNodeName, l.Epoch, why)
		return false
	}
	e.recordBoundCarrier(node, carrier, src)
	// Debug, not info: the landing itself is the record, and one line per
	// carrier at every seat was the Pi's only log-line rise under Lane H
	// (RULINGS-C2c-run-through §3.4; DECISIONS D35). Refusals stay at info.
	e.debugFn("bin_landed: bound bin %d at %s (count=%d epoch=%d, %s)", l.BinID, node.CoreNodeName, l.Count, l.Epoch, why)
	e.settleCellPlacement(node.ID)
	e.Events.Emit(Event{Type: EventUOPAdjusted, Payload: UOPAdjustedEvent{
		ProcessNodeID: node.ID,
		CoreNodeName:  node.CoreNodeName,
		BinID:         l.BinID,
		NewRemaining:  l.Count,
		Actor:         protocol.ActorCoreLifecycle,
	}})
	return true
}

// lightDarkSequentialPair puts the line on the position a carrier just landed
// on when it is one of a sequential pair with BOTH positions dark: the state
// a tooling evacuate leaves (clearActivePullForEvacuate), which no release
// can leave, because the flip moves the line only from a side that is
// pulling. The landed side takes the line through the pair's one writer
// (writePullSide); its partner stays dark until a release flips the line to
// it. A pair with either side lit is never changed: a carrier landing on the
// parked side of a running pair is the pair working. Press-index pairs never
// read the bit (isInactivePairedNode) and are not lit here.
//
// Cost: nothing on a seat the bind left lit (the bind returns the bit). On a
// dark seat, the claim and the partner (claimAtNode, pairedNodeOf) and the
// partner's runtime row; the two-statement write only when both are dark.
// Caller holds countMu, as the tick that reads the bit does.
func (e *Engine) lightDarkSequentialPair(node *processes.Node) {
	claim := e.claimAtNode(node)
	if claim == nil || claim.PairedCoreNode == "" || claim.SwapMode != protocol.SwapModeSequential {
		return
	}
	partner, err := e.pairedNodeOf(node)
	if err != nil {
		log.Printf("bin_landed: node %s landed dark on a sequential pair, and its partner cannot be read: %v",
			node.CoreNodeName, err)
		return
	}
	prt, err := e.db.GetProcessNodeRuntime(partner.ID)
	if err != nil || prt == nil || prt.ActivePull {
		return
	}
	if err := e.writePullSide(node.ID, partner.ID); err != nil {
		return
	}
	log.Printf("bin_landed: sequential pair %s/%s was dark on both sides; the line is on %s, where a carrier landed",
		node.CoreNodeName, partner.CoreNodeName, node.CoreNodeName)
}
