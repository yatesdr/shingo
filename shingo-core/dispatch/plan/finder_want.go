package plan

import (
	"shingocore/domain"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// finder_want.go — which carrier TYPE an empty should be: the maintained-group
// episode's type, else the loader window's derivation, else what fits where the
// carrier is set down. Its sibling requiresFullCarrier (finder.go) answers
// full-vs-partial, not which type.

// WantedBinType decides WHICH carrier type an empty going to a loader window
// should be, from the loader's declared carrier mix. A loader that has declared
// no mix — every loader today — is "first come, first served" among the carriers
// its own payloads may ride (see the no-mix arm in wantedEmpty): this returns
// that type when there is exactly one, and "" when there are several or the rule
// admits any carrier. The finder itself searches by the set (wantedEmpty);
// this single-code form is what the mix tests read.
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
func (f *Finder) WantedBinType(need Need) string {
	if types, _ := f.wantedEmpty(need); len(types) == 1 {
		return types[0]
	}
	return ""
}

// oneType is a single derived code as a set: "" (nothing said) is nil, any.
func oneType(code string) []string {
	if code == "" {
		return nil
	}
	return []string{code}
}

// wantedEmpty is what an empty should be. types is WantedBinType's answer as a
// set, which is what the finder searches by: nil is any carrier, one code is
// that type, several are any of them. Only the no-mix loader arm below ever
// answers with more than one.
//
// height is the height, in layers of totes, the empty is wanted at where it is
// set down (store.DB.GetSetDownNode): 0 at a fed press cell's core node, else the
// payload's tote layers, none (the zero Height) when the payload states none or
// nothing names where the carrier goes. The finders rank by it, and it decides
// one thing they keep out: tops & bottoms answers Layers(0) only (bins.HeightArm). It rides the
// destination read this function already made, so it costs no statement; a
// keeper ask (the origin arm) reads no destination and wants no height.
// Pins: TestToteWant_TheShape, TestToteWant_ReachesTheFinder,
// TestToteWant_NoNumbersAddNoStatement.
func (f *Finder) wantedEmpty(need Need) (types []string, height domain.Height) {
	if need.Intent != IntentEmpty {
		return nil, domain.Height{}
	}
	// A STATED WANT IS THE ANSWER (Need.Want), before any derivation: the
	// caller has already decided the type and the height.
	if need.Exact() {
		return []string{need.Want.Type}, need.Want.Height
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
			return []string{code}, domain.Height{}
		} else if err != nil {
			f.debug("WantedBinType: maintained episode for origin %s: %v", need.OriginID, err)
		}
	}

	// ── The loader derivation. Needs a destination WINDOW to derive from. ────
	if need.DeliveryNode == "" {
		code, height := f.typeFromDestinationPosition(need, nil)
		return oneType(code), height
	}
	dest, destHeight, err := f.db.GetSetDownNode(need.DeliveryNode, need.PayloadCode)
	if err != nil || dest == nil {
		code, height := f.typeFromDestinationPosition(need, nil)
		return oneType(code), height
	}
	// The want is the set-down node's; a delivery node that is not where the
	// carrier is set down says nothing about its height.
	if need.SetDownNode == need.DeliveryNode {
		height = destHeight
	}
	// The home and, in the same read, the carrier types the window's payloads
	// may ride — so the no-mix arm below costs no statement the quota read did
	// not already pay for.
	home, carriers, err := f.db.GetLoaderHomeCarriers(dest.ID)
	if err != nil || home == nil {
		code, height := f.typeFromDestinationPosition(need, &setDown{dest, destHeight})
		return oneType(code), height
	}
	quotas, err := f.db.ListLoaderQuotas(home.LoaderID)
	if err != nil {
		return nil, height
	}
	if len(quotas) == 0 {
		// ── NO DECLARED MIX: THE CARRIERS THE LOADER'S PARTS MAY RIDE ───────
		//
		// No mix means "first come, first served" among the carriers this
		// loader can USE — not among every carrier in the plant. The empty is
		// generic, but the loader will fill it with one of its own payloads, and
		// where payload_bin_types lists a payload's carriers the rule is hard:
		// a full on any other type is refused as a source for good
		// (helpers.PayloadBinTypeRuleArm). Fetching one anyway delivers a
		// carrier that, once loaded, can never leave as stock.
		//
		// r7 demo, 2026-10-10: 2 of 23 PLK pulls brought a STANDARD-SM empty to
		// a STUD / BRKT loader (both STANDARD-only); each stranded its outbound
		// and held its loader's threshold satisfied (TRACE-r7-demo-before-
		// stranded.md). Pins: TestReplenishAsk_NoMixTakesACarrierItsPartMayRide,
		// TestReplenishAsk_NoMixWaitsRatherThanTakeTheWrongCarrier.
		//
		// nil — any carrier, today's behaviour — when the window stages no
		// payload or any of its payloads has no rows: that payload constrains
		// nothing, so any carrier is one the loader can fill. Otherwise the
		// union of its payloads' types, and a group with none of them WAITS
		// under the typed cause rather than taking another.
		return carriers, height
	}
	homes, err := f.db.ListLoaderHomes(home.LoaderID)
	if err != nil {
		return nil, height
	}
	nodeIDs := make([]int64, 0, len(homes))
	for _, h := range homes {
		nodeIDs = append(nodeIDs, h.PositionNodeID)
	}
	resident, err := f.db.ListBinsByNodes(nodeIDs)
	if err != nil {
		return nil, height
	}
	have := map[string]int{}
	for _, b := range resident {
		have[b.BinTypeCode]++
	}
	// What this window can physically take. Absent = anything.
	caps, err := f.db.ListLoaderHomeBinTypes(home.LoaderID)
	if err != nil {
		return nil, height
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
	return oneType(best), height
}

// emptyFenceFor builds the maintained-group fence for one need.
//
// TWO INPUTS, TWO RULES. ProcessNode decides rule (i) — whether this asker is
// supported at a strict group — and OriginGroup decides rule (ii), whether the
// ask is filling a group it may therefore not source from. Both come straight
// off the order; neither is re-derived.
//
// SHARING IS THE DEFAULT, so a need with neither returns the zero fence and the
// query renders without a CTE or an arm — byte-for-byte what it was before
// MG3-1. Only an ask that names a process or carries a maintain origin can be
// fenced at all.
//
// A READ FAILURE RETURNS THE ZERO FENCE rather than refusing to source, and the
// direction is chosen: the failure it restores is a carrier taken from a fenced
// group, which the next tick corrects, against parking every empty pull in the
// plant on a database blip. The alternative — treat an unreadable episode as
// "fence everything" — would turn one bad read into a plant-wide stall.
func (f *Finder) emptyFenceFor(need Need) bins.EmptyFence {
	fence := bins.EmptyFence{ProcessNode: need.ProcessNode}
	if need.OriginID == "" {
		return fence
	}
	group, _, err := f.db.MaintainedEpisodeForOrigin(need.OriginID)
	if err != nil {
		f.debug("emptyFenceFor: maintained episode for origin %s: %v", need.OriginID, err)
		return fence
	}
	fence.OriginGroup = group
	return fence
}

// typeFromDestinationPosition is MG3-4's half of the answer: OUTFLOW TYPING.
//
// A press empty pull has no payload to reason from — it wants an empty carrier,
// and "empty" says nothing about which type. The only fact available at
// selection time is the POSITION the carrier is going to, and that position's
// effective bin types say what physically fits there.
//
// ── EXACTLY ONE, OR NOTHING ─────────────────────────────────────────────────
//
// One effective type is a statement: this slot takes 45x58 and nothing else, so
// asking for anything else produces a carrier that cannot be set down.
//
// ZERO OR MANY LEAVES TODAY'S BEHAVIOUR UNTOUCHED, and the two cases are
// different facts with the same correct handling. Zero means nobody has said
// what fits — the overwhelmingly common state, and narrowing on an operator's
// behalf is making a decision for them. Many means several types fit, and
// picking one of them here would be arbitrary: the cascade's ordering already
// decides which carrier costs least to fetch, and it should keep deciding.
//
// AFTER THE LOADER DERIVATION, DELIBERATELY. A loader window with a declared mix
// has a richer answer — which type it is most STARVED of, proportionally — and
// that must not be overridden by the narrower physical fact. This runs only
// where the loader derivation had nothing to say.
//
// INERT ON EMPTY CONFIG. A plant with no per-position bin types gets "" from
// every call, which is what it got before MG3-4 existed. That is why the wire
// lands regardless of how many positions are typed today: the config is not
// worth populating until something reads it.
//
// THE DESTINATION, NOT THE ASKER. It reads need.SetDownNode, where the carrier
// is put down next; need.ProcessNode is who asks, which the strict fence reads.
// A plain retrieve carries no process node at all, and a complex leg's asker is
// its press while its empty goes to the paired node. Pins:
// TestPlainRetrieveEmpty_TypedByItsDestinationsOneAdmittedType,
// TestOutflowTyping_TheAskerIsNotTheDestination.
//
// delivery is the need's delivery node, with its wanted height, when the caller
// has already read it, so a simple order's destination costs no second lookup;
// nil otherwise. It also answers the height an empty is wanted at on the
// set-down node (wantedEmpty), from the same read.
func (f *Finder) typeFromDestinationPosition(need Need, delivery *setDown) (string, domain.Height) {
	if need.SetDownNode == "" {
		return "", domain.Height{}
	}
	at := delivery
	if at == nil || need.SetDownNode != need.DeliveryNode {
		n, height, err := f.db.GetSetDownNode(need.SetDownNode, need.PayloadCode)
		if err != nil || n == nil {
			return "", domain.Height{}
		}
		at = &setDown{n, height}
	}
	types, err := f.db.GetEffectiveBinTypes(at.node.ID)
	if err != nil || len(types) != 1 {
		return "", at.height
	}
	return types[0].Code, at.height
}

// setDown is a destination read: the node, and the height an empty is wanted
// at there (the zero Height for none).
type setDown struct {
	node   *nodes.Node
	height domain.Height
}
