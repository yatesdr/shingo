package dispatch

// source_finder_want.go — which carrier TYPE an empty should be.
//
// MOVED OUT OF source_finder.go UNCHANGED. One function, in its own file, because
// it is about to grow: the maintained-group work adds a first arm here (an ask
// whose origin names an open maintain episode returns that episode's type and
// never re-derives), and that edit reads better against 100 lines than against
// 900. Its sibling requiresFullCarrier stays in source_finder.go — it answers
// full-vs-partial, not which type, and moving it would be a judgement rather than
// a move.

// wantedBinType decides WHICH carrier type an empty going to a loader window
// should be, from the loader's declared carrier mix. Returns "" when the loader
// has declared no mix — which is every loader today, and means "first come,
// first served": take whatever compatible empty is available, exactly as before.
//
// The rule composes two facts that answer different questions:
//
//   - QUOTA is the loader's intent: three 45x48, one 32x32, one tote. It decides
//     what to ask for — whatever the loader is most short of.
//   - CAPABILITY is the window's physical fact: this slot fits a 45x48 or a tote.
//     It filters what may be asked for AT THIS WINDOW. It is HARD; a carrier that
//     does not fit is not a carrier, whatever the mix says.
//
// Shortfall is measured against what is at the loader's windows RIGHT NOW,
// counted by type, which Core reads directly — it owns the bins. That is also
// why this is derived here rather than carried on the order: the Edge would have
// to be told, and a rule a caller has to remember is one that gets forgotten.
//
// A declared mix is HONOURED, NOT APPROXIMATED. If the
// type the loader is short of is not available anywhere, the pull WAITS. It does
// not substitute another type — declaring a mix and then ignoring it when it is
// inconvenient makes declaring it pointless. A loader that wants any-type
// behaviour says so by declaring no mix.
func (f *SourceFinder) wantedBinType(need SourceNeed) string {
	if need.Intent != IntentEmpty {
		return ""
	}

	// ── FIRST ARM: THE TYPE IS PINNED, NOT DERIVED ──────────────────────────
	//
	// An ask carrying an origin that names an OPEN maintained-group episode
	// sources the type that episode is short of, full stop. It does not fall
	// through to the loader derivation below and it never re-derives.
	//
	// PINNING AT MINT IS SOURCING CORRECTNESS, NOT BOOKKEEPING, and this is the
	// argument for it (SYNTH round 2 §1d): the keeper decided it was short of
	// 45x58x32 and is counting that type. If a replayed ask re-derived its own
	// shortfall — on a tick where the level had moved, or where a push had just
	// landed — it could source 45x48x24 instead. The carrier arrives, the count
	// the keeper is watching does not move, and it asks again. The level never
	// converges, and nothing anywhere reports an error.
	//
	// BEFORE THE DeliveryNode GUARD, deliberately. The old first line folded
	// Intent and DeliveryNode together, but a keeper ask's DeliveryNode is a
	// concrete child slot chosen by pre-resolve — it is not a loader window, so
	// the derivation below would find no home and return "" for an ask whose
	// type is already known. The DeliveryNode requirement belongs to the LOADER
	// derivation, which is why it moved down to it.
	//
	// A read failure returns "" rather than guessing, and that is the safe
	// direction here: an untyped ask sources any compatible empty, which the
	// keeper will then not count — one wasted carrier and a retry next tick,
	// versus a wrong-typed carrier delivered as though it were right.
	if need.OriginID != "" {
		if _, code, err := f.db.MaintainedEpisodeForOrigin(need.OriginID); err == nil && code != "" {
			return code
		} else if err != nil {
			f.debug("wantedBinType: maintained episode for origin %s: %v", need.OriginID, err)
		}
	}

	// ── The loader derivation. Needs a destination WINDOW to derive from. ────
	if need.DeliveryNode == "" {
		return ""
	}
	dest, err := f.db.GetNodeByDotName(need.DeliveryNode)
	if err != nil || dest == nil {
		return ""
	}
	home, err := f.db.GetLoaderHomeByPositionNode(dest.ID)
	if err != nil || home == nil {
		return ""
	}
	quotas, err := f.db.ListLoaderQuotas(home.LoaderID)
	if err != nil || len(quotas) == 0 {
		return "" // no declared mix — first come, first served
	}
	homes, err := f.db.ListLoaderHomes(home.LoaderID)
	if err != nil {
		return ""
	}
	nodeIDs := make([]int64, 0, len(homes))
	for _, h := range homes {
		nodeIDs = append(nodeIDs, h.PositionNodeID)
	}
	resident, err := f.db.ListBinsByNodes(nodeIDs)
	if err != nil {
		return ""
	}
	have := map[string]int{}
	for _, b := range resident {
		have[b.BinTypeCode]++
	}
	// What this window can physically take. Absent = anything.
	caps, err := f.db.ListLoaderHomeBinTypes(home.LoaderID)
	if err != nil {
		return ""
	}
	allowed := caps[dest.ID]
	fits := func(code string) bool {
		if len(allowed) == 0 {
			return true
		}
		for _, a := range allowed {
			if a == code {
				return true
			}
		}
		return false
	}
	// The most starved type this window can hold, measured as a PROPORTION of
	// what was asked for rather than as a raw count.
	//
	// Raw count gets this wrong in the case that matters most. Wanting 3 of one
	// type and 1 of another, holding 2 and 0, leaves both one short — and a
	// count-based rule picks whichever sorts first. But holding none of a type
	// means that part cannot run at all, while being one short of three means it
	// can. The emptier line is the more urgent one.
	//
	// Compared by cross-multiplication rather than a ratio so the arithmetic
	// stays integral and exact. Ties break on the type code, so the answer is
	// stable rather than map-order.
	var best string
	var bestGap, bestWant int
	for _, q := range quotas {
		gap := q.Want - have[q.BinTypeCode]
		if gap <= 0 || q.Want <= 0 || !fits(q.BinTypeCode) {
			continue
		}
		switch {
		case best == "",
			gap*bestWant > bestGap*q.Want,
			gap*bestWant == bestGap*q.Want && q.BinTypeCode < best:
			best, bestGap, bestWant = q.BinTypeCode, gap, q.Want
		}
	}
	return best
}

// maintainedGroupExclusion answers "which subtree must this need NOT source
// from", and it is non-zero for exactly one caller: a level keeper's top-off ask.
//
// THE DEFECT IT CLOSES, measured rather than reasoned. A keeper ask delivering
// into a maintained group is free to source an empty ALREADY STANDING IN THAT
// GROUP and carry it to another position in the same group. On a six-position
// group standing at 2 of a level of 4, both remaining carriers were claimed by
// the group's own top-off asks inside one tick — P03 to P01 and P04 to P02.
//
// It is not just two wasted trips. A claimed carrier stops counting as
// `resident`, so the gap re-opens, so the keeper asks again: the group shuffles
// itself and never reaches its level. The design named the rule before the
// keeper was built — "never from the group itself" — and this is that rule.
//
// ZERO FOR EVERY OTHER NEED, which keeps the change scoped to the path that
// demonstrated the problem. A plain retrieve_empty into an ordinary market lane
// sourcing from the same market may well be pointless too, but that is a
// different claim with no evidence behind it here, and widening a fix into a
// behaviour change is how a fix stops being reviewable.
//
// A READ FAILURE RETURNS ZERO — no exclusion — rather than refusing to source.
// The failure mode it restores is a wasted trip the next tick corrects; the
// alternative (treat an unreadable episode as "exclude everything") would park
// the keeper's asks on a database blip.
func (f *SourceFinder) maintainedGroupExclusion(need SourceNeed) int64 {
	if need.OriginID == "" {
		return 0
	}
	group, _, err := f.db.MaintainedEpisodeForOrigin(need.OriginID)
	if err != nil {
		f.debug("maintainedGroupExclusion: origin %s: %v", need.OriginID, err)
		return 0
	}
	if group == "" {
		return 0
	}
	node, err := f.db.GetNodeByDotName(group)
	if err != nil || node == nil {
		f.debug("maintainedGroupExclusion: group %q for origin %s not found: %v",
			group, need.OriginID, err)
		return 0
	}
	return node.ID
}
