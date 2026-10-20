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
	"shingoedge/store/processes"
)

// HandleBinLanded binds the carrier a landing names at the node it names, if
// that node is one of this Edge's process nodes. The bind's refusals are its
// statement's WHERE (processes.BindLanding): a landing whose placement stamp
// is not newer than the seat's last is refused whole, so a redelivered or
// overtaken landing binds nothing (D10); the same bin already bound at an
// equal or newer generation keeps its count (H.4).
//
// The landing's part, with its known flag, is the seat's identity (H.1): a
// carrier Core does not know is recorded UNKNOWN, never as the previous
// occupant's part.
//
// Pi cost per landing at a seat: the node read, the runtime row's ensure, the
// bind, the identity write and the placement settle (settleCellPlacement's
// reads), the same reads the Bound arm and the delivered bind paid for the
// same carrier, now paid once instead of twice.
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
	carrier := domain.UnknownCarrier()
	if l.PayloadKnown {
		carrier = domain.KnownCarrier(domain.LinesidePayloadCode(l.PayloadCode))
	}
	e.landCarrier(node, processes.LandingBind{
		BinID: l.BinID, Epoch: l.Epoch, Count: l.Count, PlacedSeq: &seq,
	}, carrier, "landing "+l.OrderUUID)
}

// landCarrier is the landing verb's engine half: the bind (uop.Mutator.Land,
// whose refusals are its WHERE), then, only if it landed, the relight of a
// dark sequential pair (lightDarkSequentialPair, on a seat the bind left
// dark), the identity through the lineside doorway and the placement's
// settle. Reports whether it landed.
//
// THE SETTLE IS LOAD-BEARING. A bind here is the record that a bin is on the
// cell, which a leg that has already left the cell's nodes waits on; whichever
// of the pickup and the placement lands second completes the departure
// (leg_departure.go). The Bound arm fired it; the landing now does.
//
// Callers: HandleBinLanded (stamped), and for one release an older Core's
// Bound (bindAdjustmentFromCore) and delivered (landFromOldCore), unstamped
// (H.7).
func (e *Engine) landCarrier(node *processes.Node, l processes.LandingBind, carrier domain.LinesideCarrier, why string) bool {
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
	landed, activePull, err := e.inventoryDelta.Land(node.ID, l)
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
	e.recordLinesideCarrier(node.ID, node.CoreNodeName, carrier, domain.CarrierFromLanding)
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
