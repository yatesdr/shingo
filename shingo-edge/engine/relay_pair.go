package engine

import (
	"fmt"

	"shingo/protocol"
	"shingoedge/orders"
)

// ── A RELAY IS NOT A PAIR ────────────────────────────────────────────────────
//
// Core runs two legs that name each other as one job: both dispatch in the same
// pass or neither does. That is right for a clear-then-fill swap, where each leg
// sources on its own and the danger is one robot going without the other. It is
// wrong for a relay, where one leg's source IS the other leg's delivery: the
// collector cannot source until the feeder has delivered, the feeder may not go
// without the collector, and the pair waits on itself for ever.
//
// buildSingleRobotChangeoverSwap is the relay today — its stage leg brings the
// new carrier to inbound staging, and its swap leg collects it from there.
//
// legsRelay reads the steps, not the mode. A leg FEEDS the other when it sets a
// carrier down at a one-bin node before its first wait, and the other leg picks
// up at that node. Both narrowings of "picks up where the other drops" carry
// weight:
//
//   - BEFORE ITS FIRST WAIT, so the carrier is down before the operator releases
//     anything. In a clear-then-fill swap the partner also picks up where the
//     filler drops — the evac lifts the line bin the supply later replaces — but
//     every filler drop onto a shared node comes after its release wait, because
//     the partner has to clear the node first.
//   - AT A ONE-BIN NODE (ExclusiveSlot, which stagingDropoff declares). A drop at
//     a market is not a hand-off: a partner fetching from the same market is
//     fetching, not collecting.
//
// A relay's legs stay linked at the Edge — the RELEASE button, the supply-bin
// guard and handleRelayPartnerDeath read LinkOrderSiblings — and go to Core
// naming no sibling, so Core runs them one after the other.
func legsRelay(a, b []protocol.ComplexOrderStep) bool {
	return feeds(a, b) || feeds(b, a)
}

// feeds reports whether collector picks up at a one-bin node that feeder sets a
// carrier down at before its first wait.
func feeds(feeder, collector []protocol.ComplexOrderStep) bool {
	handOff := map[string]bool{}
	for _, s := range feeder {
		if s.Action == protocol.ActionWait {
			break
		}
		if s.Action == protocol.ActionDropoff && s.ExclusiveSlot && s.Node != "" {
			handOff[s.Node] = true
		}
	}
	for _, s := range collector {
		if s.Action == protocol.ActionPickup && handOff[s.Node] {
			return true
		}
	}
	return false
}

// coreSiblings returns the sibling uuid each of two legs names to Core: its
// partner's, or none for either leg of a relay.
func coreSiblings(stepsA, stepsB []protocol.ComplexOrderStep, uuidA, uuidB string) (sibA, sibB string) {
	if legsRelay(stepsA, stepsB) {
		return "", ""
	}
	return uuidB, uuidA
}

// handleRelayPartnerDeath cancels the live leg of a relay when the other leg
// ends without finishing — failed, cancelled or skipped.
//
// Core cannot: a relay goes to Core unpaired, so HandleSwapPeerTerminal never
// hears of the partner. That rule covered this pair while the Edge sent it as
// siblings, and without it the collector waits for a carrier that is not coming,
// seen only on the anomaly board, or the feeder stages a carrier nobody will
// collect.
//
// Only relays. A clear-then-fill pair is Core's: its rule spares a moot evac's
// supply and a supply parked on material, which the Edge cannot tell apart, so
// an Edge cancel there would override a choice Core made on purpose.
//
// No exception for accept-half-swap, unlike Core's arm: a relay's collector
// cannot finish without its feeder's carrier, so there is no half to keep.
func (e *Engine) handleRelayPartnerDeath(changed OrderStatusChangedEvent) {
	status := protocol.Status(changed.NewStatus)
	if !protocol.IsTerminal(status) || orders.IsTerminalSuccess(status) {
		return
	}
	order, err := e.db.GetOrder(changed.OrderID)
	if err != nil || order.SiblingOrderID == nil {
		return
	}
	partner, err := e.db.GetOrder(*order.SiblingOrderID)
	if err != nil || protocol.IsTerminal(partner.Status) {
		return
	}
	if !e.storedLegsRelay(order.ID, partner.ID) {
		return
	}
	reason := fmt.Sprintf("relay partner order %d %s — this leg cannot complete without it", order.ID, status)
	if err := e.orderMgr.AbortOrderWithReason(partner.ID, reason); err != nil {
		e.logFn("relay partner death: order %d %s, cancelling partner %d: %v", order.ID, status, partner.ID, err)
		return
	}
	e.logFn("relay partner death: order %d %s — cancelled its relay partner %d", order.ID, status, partner.ID)
}

// storedLegsRelay applies legsRelay to two stored orders. Steps that cannot be
// read — a retrieve leg has none — do not make a relay.
func (e *Engine) storedLegsRelay(a, b int64) bool {
	sa, err := e.storedStepsOf(a)
	if err != nil {
		return false
	}
	sb, err := e.storedStepsOf(b)
	if err != nil {
		return false
	}
	return legsRelay(sa, sb)
}

func (e *Engine) storedStepsOf(orderID int64) ([]protocol.ComplexOrderStep, error) {
	raw, err := e.db.GetOrderStepsJSON(orderID)
	if err != nil {
		return nil, err
	}
	return decodeSteps(raw)
}
