package engine

import (
	"shingo/protocol"

	"shingocore/store/orders"
)

// orderIsTerminal is the ONE spelling of "is this order finished", with the
// fail-closed arm inside it.
//
// ── WHY IT IS A FUNCTION AND NOT A BARE protocol.IsTerminal CALL ──────────
//
// protocol.IsTerminal answers "does this status have outgoing transitions", and
// the EMPTY STRING has none — so a zero-value Status reads as TERMINAL. Whether
// that is safe depends entirely on what the caller does with the answer, and the
// caller here acts on it in a direction where "terminal" is the dangerous
// answer: reapplyRefused SKIPS its teleport check on a terminal order. Skipping
// is the permissive answer, and an order whose state cannot be read is exactly
// when the check should still run.
//
// It had a second caller that wanted the same arm and did not have it: the
// multi-bin completion DELETED the order's junction rows on a terminal order,
// which is precisely what left two specimens unreconstructable after the fact
// (PLAN §R.5, §R.9). That delete is gone: an order's rows are kept after it
// ends.
//
// The arm lives INSIDE the predicate rather than at the call sites — the
// cb7ed41d move. A guard that has to be remembered at each site is one that will
// eventually be forgotten at one of them, which is the history this file is
// written out of: the same predicate has now been corrected four times, every
// time at a call site.
//
// A nil order is not terminal, for the same reason an unset status is not:
// nothing is known about it, so nothing permissive or destructive may follow.
//
// ── WHAT THIS IS NOT ──────────────────────────────────────────────────────
//
// It is not a replacement for protocol.IsTerminal, which stays correct for the
// ~75 call sites that read a status they have just been handed or have already
// validated. This is for the sites where the ANSWER GATES AN IRREVERSIBLE ACT
// and the input is a row that may not have loaded. Those are enumerated in the
// step-1 report rather than converted wholesale, because a sweep that re-points
// 75 readers is A-era work and would bury the two that matter.
func orderIsTerminal(order *orders.Order) bool {
	return order != nil && order.Status != "" && protocol.IsTerminal(order.Status)
}
