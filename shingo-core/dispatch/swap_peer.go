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
	// SwapTerminalAbandoned is a supply cancel where the operator explicitly
	// accepted the half-swap (protocol.CancelReasonAcceptHalfSwap). It is a
	// DISTINCT kind rather than a flag on Cancelled because the two arms have
	// opposite post-conditions — Cancelled cancels the partner fail-closed,
	// Abandoned deliberately leaves it flying — and overloading one constant
	// with both would force a suppression boolean through this handler, which
	// is exactly the two-definitions drift the constants exist to prevent.
	SwapTerminalAbandoned = "abandoned"
)

// IsOperatorGatedStaging reports whether an order is a coordinated swap leg
// parked at its wait point waiting for an OPERATOR to press RELEASE — as
// opposed to a robot the system has forgotten about.
//
// This is the abandon sweep's second exemption, alongside IsGateStaged, and the
// two are deliberately separate because they are held by different parties: a
// gate-staged leg waits on CORE to append its tail, and this one waits on a
// HUMAN. Neither is "stuck", and neither should be cancelled on a timer whose
// premise is that it has been forgotten.
//
// SPRINGFIELD ALN_003, 2026-07-31. The evac leg staged at 15:00:07 and its
// supply sibling reached staged at 15:32:57 after three transient fleet faults
// (AMR-11, laser-reflector warnings on the SMN_033→ALN_003 path). The operator
// could not release, and at 16:00:22 — exactly 1h after the evac staged — the
// sweep abandoned the evac and cascade-cancelled the supply, destroying both
// legs of a live changeover. 1h is the right bound for a forgotten robot and
// far too short for a swap a human still has to authorise.
//
// WHY `Coordinated` AND NOT "is this a changeover". Core cannot see changeovers:
// process_changeovers is an Edge-local SQLite table and no changeover marker
// reaches the Core order row (origin_class is a closed enum of
// attached/orphan/no_demand). `Coordinated` is the Core-side category that
// actually carries the property we care about. A mid-cycle two-robot swap has
// the identical hazard (both legs cancelled, line left un-cleared) and the
// identical operator dependency, so covering it too is intended, not overreach.
//
// ⛔ THIS IS NO LONGER "the SAME category IsGateStaged excludes", which is what
// this note used to say. IsGateStaged stopped being a statement about order
// class: it asks which WAIT the order is parked at, so a coordinated order can
// satisfy both predicates. This one is deliberately still coarse — Coordinated
// AND staged — and it stays coarse safely only because the abandon sweep asks
// IsGateStaged FIRST (reconciliation_service.go). A coordinated order parked at
// a LANE wait is claimed there and never reaches here; what reaches here is a
// coordinated order parked on a wait Core does not own, which is exactly the
// population this bound is for. Read the two together; the precedence is
// load-bearing.
func IsOperatorGatedStaging(order *orders.Order) bool {
	if order == nil {
		return false
	}
	return order.Coordinated && order.Status == protocol.StatusStaged
}

// HandleSwapPeerTerminal reacts to a two-robot swap leg reaching a terminal
// state so a half-completed swap can't silently strand the line (evac pulled the
// resident, no replacement) or collide two bins on it (supply drops onto an
// un-cleared line). It is the swap analog of HandleChildOrderFailure — the
// durable sibling link (sibling_order_uuid, made reliable in the durable-link
// commit), NOT a compound parent, identifies the peer.
//
// This closes the POST-DISPATCH window. The pair rule (complex_pair.go) is a
// dispatch-time admission decision: it guarantees both legs were committed in
// the same pass, and nothing re-evaluates it once they are in flight. If a leg
// then dies, this handler unwinds the other — which is the same rule carried
// past the moment dispatch can enforce it.
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

	// Same discriminator as swapLegHeld — legTakesLineBin: the evac lifts
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

	// Accept-half-swap: the operator explicitly chose to let the committed
	// partner finish. Record it loudly; touch NOTHING. The fail-closed peer
	// cancel below is the default for every other terminal kind.
	if terminalKind == SwapTerminalAbandoned {
		d.db.AppendAudit("order", peer.ID, "swap_half_accepted", "",
			fmt.Sprintf("supply (order %d) abandoned by operator accepting the half-swap; partner left to complete", dead.ID), "system")
		log.Printf("dispatch: swap supply %d abandoned (accept-half-swap) — partner %d left to complete", dead.ID, peer.ID)
		return
	}

	// ── A MOOT SKIP IS NOT A DEATH, AND THAT IS THE ONE ROLE READ LEFT ────
	//
	// A skipped evac found NO BIN to clear: the line's resident was already gone,
	// so there is nothing for the supply to collide with and the supply is the
	// thing that should put a carrier back. Every other terminal on either side
	// is a death.
	//
	// It survives the death rule deliberately. The rule is "if either leg DIES,
	// both die", and a leg whose work was found unnecessary did not die — the
	// distinction is physical, not bookkeeping. Killing the supply here would
	// leave the line empty and make the level keeper re-ask for the same carrier
	// one cycle later, through consume_plan's node-empty downgrade. Same carrier,
	// later, plus a cancelled order on the board.
	if deadIsEvac && terminalKind == SwapTerminalSkipped {
		return
	}

	// ── THE DEATH RULE, WHOLE ─────────────────────────────────────────────
	//
	// A leg that goes terminal takes its sibling with it. ONE action, taken the
	// same way whichever leg died and whatever mode the swap is — the role below
	// chooses the SENTENCE, never the outcome, because the two hazards are
	// genuinely different things to tell an operator and the same thing to do
	// about them.
	//
	// THE SPARE IS GONE. It used to keep alive a supply parked on a dry source
	// whose evac had died, so an operator could stock the payload and let the
	// supply resume. That was a workaround for a half-dispatched pair: the legs
	// were two independently-dispatched orders, so one could be mid-wait while
	// the other died, and cancelling the survivor drove the Springfield
	// 2026-07-21 re-arm churn (the monitor saw the cancel move the in-loop UOP,
	// re-armed the changeover, the planner rebuilt the pair, the supply parked
	// again, the evac died again — hundreds of doomed swaps per changeover,
	// 74577-6SA0A.06, zero system stock).
	//
	// There is no such thing as a half-dispatched pair now: both legs dispatch in
	// one pass or neither does. And the churn's actual cause was never this
	// cancellation — it was the planner RE-ARMING into a source it already knew
	// was dry. That is fixed where the pair is armed (guardSourceKnownDry,
	// shingo-edge/engine/operator_guards.go), which is the only place that can
	// stop a doomed pair from being created at all. A wait that survives its
	// partner's death is a leg holding resources for a job that cannot happen.
	if deadIsEvac {
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
		// TermPeerTerminal is declared for exactly this and had no producer:
		// "a swap sibling died and this leg was unwound with it". It is already
		// in ClassifyTermCode's deliberate bucket, so coding it moves nothing —
		// it just stops this cascade being indistinguishable from a person, which
		// is the confusion that made a machine-vs-human retry exemption look
		// implementable.
		d.lifecycle.CancelOrder(peer, peer.StationID, reason,
			CancelCause{Code: protocol.TermPeerTerminal})
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
