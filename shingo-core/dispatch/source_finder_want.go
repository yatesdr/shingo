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
	if need.Intent != IntentEmpty || need.DeliveryNode == "" {
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
