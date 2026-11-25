package dispatch

import (
	"fmt"
	"log"
	"strings"

	"shingo/protocol"
	"shingocore/domain"
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
//     transition. Its ending is a compare-and-swap on "still live", not on
//     the peer as read here, so a concurrent DispatchPreparedComplex can land
//     its yes between this read and the ending; the ending then finds the
//     job on the row and the transition stops it (docket B20; the mechanism's
//     pin is TestHandover_ACancelQueuedBehindTheYesStopsTheFleetJob, and this
//     door's own read is not pinned). This said a concurrent dispatch "can't
//     race it" because the transition rejects a change out of an
//     already-advanced state; an ending never did, and that was not checked.
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

	// legTakesLineBin: the evac lifts the line's bin and does not put one back;
	// the supply sets one down. The same discriminator the deleted swap-hold gate
	// used; the allocator's moot-reserve arm and loader_place.go read it too.
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

	// A HOME CLEAR'S TRADE LEG WHOSE DROP IS ALREADY APPENDED FINISHES. Its drop
	// was held on the other bin's lift (a stored vacate wait) and Core appended
	// it only once that bin had left: the place it drops on is empty, so neither
	// hazard below can happen, and cancelling it would only leave its bin on a
	// robot. No Edge-authored leg carries a stored vacate wait (holdsAfterLift),
	// so every other pair takes the rule whole.
	if !protocol.IsTerminal(peer.Status) && peer.WaitIndex > 0 && holdsAfterLift(peer) {
		log.Printf("dispatch: trade leg %d %s; its partner %d keeps going — its drop was appended once the other bin had left",
			dead.ID, terminalKind, peer.ID)
		d.db.AppendAudit("order", peer.ID, "trade_leg_kept", "",
			fmt.Sprintf("partner order %d ended %s after this leg's drop was appended; this leg finishes", dead.ID, terminalKind), "system")
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
	// SYN-PART07A.06, zero system stock).
	//
	// Among legs that name each other there is no such thing as a half-dispatched
	// pair now: both dispatch in one pass or neither does, and every Edge door
	// that makes a Core pair names both legs. And the churn's actual cause was never this
	// cancellation — it was the planner RE-ARMING into a source it already knew
	// was dry. That is fixed where the pair is armed (guardSourceKnownDry,
	// shingo-edge/engine/operator_guards.go), which is the only place that can
	// stop a doomed pair from being created at all. A wait that survives its
	// partner's death is a leg holding resources for a job that cannot happen.
	//
	// ONE CALL, because it is one rule. This was two branches that differed only
	// in their prose, and two calls to the same function under an if/else is how
	// a rule stops being one: the next person adding a condition adds it to one
	// arm. The hazard sentence is chosen; the action is not.
	//
	//	dead evac    the resident it never lifted is still on the line, and the
	//	             supply would drop a second bin onto it.
	//	dead supply  no replacement is coming, and the evac would lift the
	//	             line's bin with nothing behind it (a skipped supply is a
	//	             lost replacement just as much as a failed one).
	hazard := "cancelling evac so it cannot strand the line"
	role := "supply"
	if deadIsEvac {
		hazard = "cancelling supply so it cannot drop onto an un-cleared line"
		role = "evac"
	}
	why := deadLegWhy(dead)
	reason := fmt.Sprintf("coordinated swap %s (order %d) %s%s; %s", role, dead.ID, terminalKind, why, hazard)
	if holdsAfterLift(dead) {
		// A home clear's trade leg: the same action, its own sentence — there is
		// no line, and the partner's drop waits on a lift that will not come.
		reason = fmt.Sprintf("the other leg of this trade (order %d) %s%s; cancelling this one, whose drop waits on that leg's lift",
			dead.ID, terminalKind, why)
	}
	d.resolveSwapPeer(peer, dead, reason)
}

// deadLegWhy is the dead leg's own terminal reason, in parentheses, for the
// partner's sentence — or "" when the dead leg's row carries none.
//
// The partner is the leg an operator is most likely to open: it is the one that
// was standing at the line, and its row said only that its sibling had died.
// HK 400t, 2026-11-04: every supply read "coordinated swap evac (order 3166)
// cancelled; cancelling supply…" while the evac's row held the cause (RDS 60009,
// no path to PLN_05), and nobody went one order over to read it. The dead leg is
// re-read from the DB by HandleSwapPeerTerminal after its terminal write
// commits, so ErrorDetail is the reason that write stored.
//
// A fleet stop's "fleet order stopped — " is dropped when the fleet's words
// follow it: "cancelled (RDS error 60009: …)" already says who stopped it, and
// this sentence is shown on the order page, where the owner's rule is short.
//
// The sentence still opens "coordinated swap " (domain.coreCancelPrefixes keys
// pre-code rows on it) and the peer row is coded peer_terminal, so nothing in
// the parentheses classifies anything.
func deadLegWhy(dead *orders.Order) string {
	why := dead.ErrorDetail
	if rest, ok := strings.CutPrefix(why, domain.FleetStopDetail+" — "); ok && rest != "" {
		why = rest
	}
	if why == "" {
		return ""
	}
	return " (" + why + ")"
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
		d.lifecycle.CancelOrder(peer, peer.Requester(), reason,
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

// swapTerminalKind maps a terminal order status to the SwapTerminal* kind
// HandleSwapPeerTerminal expects, or "" when the status is not a swap-relevant
// terminal — the surviving-side race check skips the unwind for a non-terminal
// or unmapped sibling.
func swapTerminalKind(status protocol.Status) string {
	switch status {
	case StatusSkipped:
		return SwapTerminalSkipped
	case StatusFailed:
		return SwapTerminalFailed
	case StatusCancelled:
		return SwapTerminalCancelled
	default:
		return ""
	}
}
