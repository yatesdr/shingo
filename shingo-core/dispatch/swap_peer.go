package dispatch

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store/orders"
)

// Swap-leg terminal-kind tags the engine wiring passes to HandleSwapPeerTerminal
// so the handler can tell a genuine death (fail/cancel) from a moot skip.
const (
	SwapTerminalFailed    = "failed"
	SwapTerminalCancelled = "cancelled"
	SwapTerminalSkipped   = "skipped"
)

// HandleSwapPeerTerminal reacts to a two-robot swap leg reaching a terminal
// state so a half-completed swap can't silently strand the line (evac pulled the
// resident, no replacement) or collide two bins on it (supply drops onto an
// un-cleared line). It is the swap analog of HandleChildOrderFailure — the
// durable sibling link (sibling_order_uuid, made reliable in the durable-link
// commit), NOT a compound parent, identifies the peer.
//
// This closes the ALN_003 POST-DISPATCH window: swapRemovalLegHeld is only a
// dispatch-time admission gate (it stops the evac pulling before the supply has
// claimed) and nothing re-runs it once a leg is in flight. If a leg then dies,
// this handler unwinds the other.
//
// terminalKind is the terminal the dead leg hit (SwapTerminal*). It matters for
// the evac: a SKIPPED (moot) evac means the line's resident was already gone, so
// the supply legitimately proceeds; only a genuine evac failure/cancel leaves the
// resident on the line where the supply's drop would collide.
//
// Guards (per the operator-driven-demand / atomic-transition contract):
//   - re-checks IsTerminal on the peer (mirrors HandleChildOrderFailure) so a
//     near-simultaneous double-terminal never acts on an already-dead peer;
//   - BOUNDED — a single cancel or a single surface, never a re-creation loop;
//   - every state change routes through lifecycle.CancelOrder's atomic
//     transition, so a concurrent DispatchPreparedComplex can't race it (the
//     transition rejects a change out of an already-advanced state).
func (d *Dispatcher) HandleSwapPeerTerminal(deadOrderID int64, terminalKind string) {
	dead, err := d.db.GetOrder(deadOrderID)
	if err != nil || dead == nil {
		return
	}
	sibUUID, err := d.db.OrderSiblingUUID(deadOrderID)
	if err != nil || sibUUID == "" {
		return // not a two-robot swap leg
	}
	peer, err := d.db.GetOrderByUUID(sibUUID)
	if err != nil || peer == nil {
		return
	}

	// Same discriminator as swapRemovalLegHeld — legTakesLineBin: the evac lifts
	// the line's bin and does not put one back; the supply sets one down.
	//
	// This was `DeliveryNode != ProcessNode`, which mis-reads a 3-position
	// press-index R2: it drops a bin on the line and then carries on to re-index
	// the next position, so it ENDS away from the line while being the supply. A
	// SKIPPED supply then took the evac branch below and returned as a "moot
	// evac" no-op — so the real evac proceeded, pulled the line's bin, and nothing
	// was coming to replace it. That is the strand this handler exists to prevent.
	//
	// If the steps can't be read we deliberately do NOT take the evac branch: its
	// skip path is a silent no-op, and a silent no-op is the one outcome that can
	// strand a line. Treating an unknown shape as the supply always resolves the
	// peer, which at worst cancels a swap that could have continued.
	steps, ok := decodeSteps(dead.StepsJSON)
	if !ok {
		log.Printf("dispatch: swap peer-terminal for order %d — cannot read steps; treating as supply (resolve the peer) rather than risk a silent moot-evac no-op", dead.ID)
	}
	deadIsEvac := ok && legTakesLineBin(steps, dead.ProcessNode)

	if deadIsEvac {
		// A moot (skipped) evac is a clean no-op — the line's resident was
		// already gone, so the supply proceeds. Only a genuine evac failure/
		// cancel leaves the resident on the line, where the supply would collide.
		if terminalKind == SwapTerminalSkipped {
			return
		}
		d.resolveSwapPeer(peer, dead,
			fmt.Sprintf("two-robot swap evac (order %d) %s; cancelling supply so it cannot drop onto an un-cleared line", dead.ID, terminalKind))
		return
	}

	// Supply leg died (fail/cancel/skip — a skipped supply is a lost replacement
	// just as much as a failed one). If the evac pulls/pulled the line's resident
	// there is no replacement coming → strand. Cancel the live evac so the line
	// keeps its bin; surface if the evac already delivered.
	d.resolveSwapPeer(peer, dead,
		fmt.Sprintf("two-robot swap supply (order %d) %s; cancelling evac so it cannot strand the line", dead.ID, terminalKind))
}

// resolveSwapPeer cancels the peer if it is still live, or surfaces the
// half-swap if the peer already delivered its bin. A peer that also failed/
// cancelled/skipped is a clean double-abort (or moot) with nothing half-done to
// unwind.
func (d *Dispatcher) resolveSwapPeer(peer, dead *orders.Order, reason string) {
	if !protocol.IsTerminal(peer.Status) {
		d.lifecycle.CancelOrder(peer, peer.StationID, reason)
		return
	}
	// Peer already terminal. If it physically delivered (its bin moved) the
	// half-swap already happened with its sibling now dead — surface so the
	// operator rebalances the line. We deliberately do NOT auto-re-create a
	// replacement supply (give-up / re-issue is operator-driven, and an
	// auto-re-create risks a spin loop against an empty supermarket).
	if peer.Status == protocol.StatusDelivered || peer.Status == protocol.StatusConfirmed {
		log.Printf("dispatch: two-robot swap HALF-COMPLETED — order %d is %s but sibling %d is %s; line needs operator rebalance (%s)",
			peer.ID, peer.Status, dead.ID, dead.Status, reason)
		d.db.AppendAudit("order", peer.ID, "swap_half_completed", "",
			fmt.Sprintf("sibling %d terminal (%s) while this leg reached %s — %s", dead.ID, dead.Status, peer.Status, reason), "system")
	}
}
