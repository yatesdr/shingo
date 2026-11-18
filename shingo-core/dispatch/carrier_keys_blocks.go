package dispatch

import (
	"shingo/protocol"
	"shingocore/domain"
)

// ── CARRIER KEYS ON THE BLOCKS: WHICH STEP SENDS ITS FAMILY'S KEY ────────────
//
// A family's lift key goes on the lift of a cart of that family, and its wait
// key on a wait that stands directly in front of that lift at the same node:
// the one place the robot stands at a cart it is about to recognize. Every
// other step sends the default (seerrds.BinTaskForAction): a set-down, and a
// wait made while carrying a cart (a lane gate point, a dig dwell, a turn wait
// in front of a set-down), which sends what it sends today.

// carrierKeyNames is that rule, written once: the key each step of seg sends,
// by step ("" for the default), or nil when every step sends the default.
//
// seg is a run of the order's plan as it is sent, and start is its first step's
// position in plan. The rule reads seg's own steps (a redirect may have
// re-pointed a node in it: patchRedirectSegments) and, for the wait rule's
// look-ahead past seg's last step, the plan's next. rows are the order's carts
// (order_bins), familyOf a cart's carrier family by bin id, asked only for a
// cart a step lifts, and keys the families that name a key.
//
// It keys on the pick step, never on the bin: one cart can be lifted twice in
// one order (a relay, the carryover round trip), each lift its own row. A step
// no row lifts at, a cart with no family, a blank key, and rows at one step
// whose keys disagree all send the default. Pinned by
// TestCarrierKeyNames_TheRule.
func carrierKeyNames(plan []resolvedStep, start int, seg []resolvedStep, rows []domain.Cart,
	familyOf func(binID int64) (int64, bool), keys map[int64]familyKeys) []string {
	var names []string
	for k, s := range seg {
		var name string
		switch {
		case s.Action == protocol.ActionPickup:
			name = keyLiftedAt(start+k, rows, familyOf, keys, func(f familyKeys) string { return f.lift })
		case s.Action == protocol.ActionWait && s.Node != "":
			next, ok := stepAfter(plan, start, seg, k)
			if ok && next.Action == protocol.ActionPickup && next.Node == s.Node {
				name = keyLiftedAt(start+k+1, rows, familyOf, keys, func(f familyKeys) string { return f.wait })
			}
		}
		if name == "" {
			continue
		}
		if names == nil {
			names = make([]string, len(seg))
		}
		names[k] = name
	}
	return names
}

// stepAfter is the step after seg[k]: seg's next, or past seg's last, the plan's
// step after it (seg starts at plan position start).
func stepAfter(plan []resolvedStep, start int, seg []resolvedStep, k int) (resolvedStep, bool) {
	if k+1 < len(seg) {
		return seg[k+1], true
	}
	if p := start + len(seg); p >= 0 && p < len(plan) {
		return plan[p], true
	}
	return resolvedStep{}, false
}

// keyLiftedAt is the key (key picks the lift's or the wait's) of the carts the
// order lifts at plan step p, when they agree on one; "" when no row lifts
// there, a cart has no family, or two carts' keys differ.
func keyLiftedAt(p int, rows []domain.Cart, familyOf func(int64) (int64, bool), keys map[int64]familyKeys,
	key func(familyKeys) string) string {
	name, seen := "", false
	for _, r := range rows {
		if r.PickStep == nil || *r.PickStep != p {
			continue
		}
		k := ""
		if fam, ok := familyOf(r.BinID); ok {
			k = key(keys[fam])
		}
		if seen && k != name {
			return ""
		}
		name, seen = k, true
	}
	return name
}
