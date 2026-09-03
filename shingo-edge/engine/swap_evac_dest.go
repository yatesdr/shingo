package engine

import (
	"log"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// swap_evac_dest.go — WHOSE carrier is leaving, and therefore where it goes.
//
// A swap has two legs and they are not symmetric. The supply leg fetches the
// style the operator ASKED FOR; the evac leg lifts whatever is PHYSICALLY ON THE
// CELL. For an ordinary refill those are the same part and the distinction never
// surfaces, which is why one claim drove both legs for as long as it did.
//
// They come apart whenever a cell is left holding a style other than the one next
// requested. Springfield, 2026-09-02: an operator cancelled the ALN_006 leg of a
// changeover; the Edge stamped that node `abandoned` (terminal, deliberately —
// see wiring_completion.go, it is the fix for the Hopkinsville 2026-07-28 wedge)
// and the changeover completed 73 s later. The cell was then recorded as running
// 63145-6TA1B.10 while a 74871-6SA0A.06 carrier still stood on it. Eighteen
// minutes after that an operator requested 63145, the Edge built an ordinary
// same-style swap, and the evac leg took its destination from the REQUESTED
// style's claim — 63145's dedicated home, SMN_029. A 74871 carrier was driven
// onto 63145's home and held it for 12h35m; the next morning the 63145 supply
// order failed to source 92 times over 72 minutes because the only home it draws
// from was occupied by the wrong part.
//
// The first attempt of that same swap got it right, because a CHANGEOVER swap
// carries two claims (buildTwoRobotChangeoverSwap takes fromClaim AND toClaim)
// and the evac leg used the outgoing one: SMN_031, 74871's own home. Only the
// routine single-claim path is blind.
//
// ── WHY THE CLAIM AND NOT THE PAYLOAD ─────────────────────────────────────
//
// The obvious fix is "read the carrier's payload and look up its home", and the
// Edge cannot do it: process_node_runtime stores active_bin_id, an opaque Core
// id, and there is no bins table on this side. That gap is the same one that
// made the Edge stamp its consume tick with the claim's payload rather than the
// bin's, which is how Core came to log payload_mismatch_dropped on this carrier
// 36 minutes before the robot moved.
//
// It does not need the payload. active_claim_id already answers "whose" — it
// names the claim the resident bin was delivered under — and a claim carries its
// own outbound destination. One lookup, no new wire field, no staleness question
// on a value Core owns.
//
// Core's park-side guard (dispatch/loader_place.go) refuses a carrier that does
// not match a pinned home and reroutes it. This is the other half: it stops the
// leg being AIMED wrong in the first place, so the guard is a backstop rather
// than the only thing standing between a carrier and the wrong slot.

// residentEvacDest returns where the carrier CURRENTLY ON THIS CELL belongs, or
// "" when the question cannot be answered or the answer is the one the caller
// already has.
//
// "" IS THE COMMON RETURN AND MEANS "no override", not "no destination". Every
// caller falls back to the requested claim's own OutboundDestination, which is
// today's behaviour exactly — so an ordinary same-style swap, a cell with no
// runtime row, a cell whose resident claim IS the requested claim, and every
// read failure all produce byte-identical orders to before this existed.
//
// FAILS OPEN ON EVERY UNKNOWN, deliberately. A wrong-but-plausible override
// would send a healthy carrier somewhere it does not belong on every ordinary
// swap in the plant, which is a far worse failure than the one being prevented:
// this path is reached on every consume and produce cycle, and the condition it
// guards against needs a cell to have been left holding a foreign style.
func (e *Engine) residentEvacDest(runtime *processes.RuntimeState, claim *processes.NodeClaim) string {
	if runtime == nil || claim == nil || runtime.ActiveClaimID == nil {
		return ""
	}
	// The resident IS the requested style — the overwhelmingly common case, and
	// the one where an override would be a no-op anyway. Return early so the
	// lookup below only runs when the two genuinely disagree.
	if *runtime.ActiveClaimID == claim.ID {
		return ""
	}
	resident, err := e.db.GetStyleNodeClaim(*runtime.ActiveClaimID)
	if err != nil || resident == nil {
		// A missing or unreadable claim row is not a reason to divert a carrier.
		return ""
	}
	dest := domain.EvacDestinationFor(resident)
	if dest == "" || dest == claim.OutboundDestination {
		return ""
	}
	// LOUD, because this only fires when the cell's resident style and the
	// requested style disagree — which means something upstream completed a
	// changeover over this node, or a carrier was placed here by hand. The swap
	// is corrected either way, but the disagreement is worth a line: it is the
	// same fact Core's payloadMismatch detector logs, arriving earlier and from
	// the side that can still act on it.
	log.Printf("swap evac: node %s holds claim %d (%s) but %s was requested — routing the outgoing "+
		"carrier to %s, its own home, not %s",
		claim.CoreNodeName, resident.ID, resident.PayloadCode, claim.PayloadCode, dest, claim.OutboundDestination)
	return dest
}

// withResidentEvacDest returns claim unchanged when dest is "", else a COPY
// whose OutboundDestination names dest.
//
// A COPY, AND ONLY THIS ONE FIELD. Inside buildSwapDispatch, OutboundDestination
// is read at exactly four places and every one of them is the evac leg's dropoff
// — BuildSequentialRemovalSteps step 3, BuildSingleSwapSteps step 9,
// BuildTwoRobotSwapSteps orderB, BuildTwoRobotPressIndexSwapSteps R1. The supply
// side reads InboundSource. So overriding this field is precisely and only
// "where the outgoing carrier goes", which is the question being answered.
//
// WHY A COPY RATHER THAN A PARAMETER THREADED THROUGH THE BUILDERS. The honest
// alternative is an explicit evacDest argument on all four builders plus
// BuildSwapDispatch, which is ~60 call sites, almost all of them tests. That is
// a large, noisy diff whose risk lives in the edits themselves rather than in
// the change, for a decision that is already fully expressed by one field. The
// copy keeps every existing signature and every existing test honest, and the
// override is named at the one place it happens.
//
// The claim is never persisted from here and the copy does not escape the
// dispatch call, so no caller can observe a claim that disagrees with its row.
func withResidentEvacDest(claim *processes.NodeClaim, dest string) *processes.NodeClaim {
	if dest == "" || claim == nil {
		return claim
	}
	adjusted := *claim
	adjusted.OutboundDestination = dest
	return &adjusted
}
