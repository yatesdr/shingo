package engine

import (
	"slices"
	"sort"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// ChangeoverSituation classifies what needs to happen at a physical node
// when transitioning between two styles: domain.Situation, decided by
// domain.ChangeoverSituation for the planner and the readiness card alike.
type ChangeoverSituation = domain.Situation

const (
	SituationUnchanged = domain.SituationUnchanged
	SituationEvacuate  = domain.SituationEvacuate
	SituationSwap      = domain.SituationSwap
	SituationDrop      = domain.SituationDrop
	SituationAdd       = domain.SituationAdd
)

// ChangeoverNodeDiff represents the material change needed at a single physical
// node during a changeover.
type ChangeoverNodeDiff struct {
	CoreNodeName string
	Situation    ChangeoverSituation
	FromClaim    *processes.NodeClaim // nil for "add" situations
	ToClaim      *processes.NodeClaim // nil for "drop" situations
	// AlreadyDone marks a node the claims say changes but which already holds
	// the incoming style's part (ApplyNodeState): planned Unchanged, so it
	// moves nothing, yet still a CHANGING node to the start's gates, so an
	// order the outgoing style left at it is cancelled or blocks the start
	// rather than delivering the old part onto a node the new style runs.
	AlreadyDone bool
}

// DiffStyleClaims computes the material changes needed at each physical node
// when transitioning from one style to another: one diff per node either style
// claims, its situation domain.ChangeoverSituation's.
func DiffStyleClaims(fromClaims, toClaims []processes.NodeClaim) []ChangeoverNodeDiff {
	fromMap := make(map[string]*processes.NodeClaim, len(fromClaims))
	for i := range fromClaims {
		fromMap[fromClaims[i].CoreNodeName] = &fromClaims[i]
	}
	toMap := make(map[string]*processes.NodeClaim, len(toClaims))
	for i := range toClaims {
		toMap[toClaims[i].CoreNodeName] = &toClaims[i]
	}

	// Collect all unique node names
	nodeSet := make(map[string]bool)
	for name := range fromMap {
		nodeSet[name] = true
	}
	for name := range toMap {
		nodeSet[name] = true
	}

	// Sorted, for the same reason the fan-out below sorts its positions: a
	// plan built twice over the same rows must read the same — the seam's
	// agreement pin compares two plans whole, and the composer's preview is
	// compared byte for byte against the preview of the saved style. Map order
	// was never a contract any caller could hold.
	names := make([]string, 0, len(nodeSet))
	for name := range nodeSet {
		names = append(names, name)
	}
	sort.Strings(names)

	var diffs []ChangeoverNodeDiff
	for _, name := range names {
		from, to := fromMap[name], toMap[name]
		diffs = append(diffs, ChangeoverNodeDiff{
			CoreNodeName: name,
			Situation:    domain.ChangeoverSituation(from, to),
			FromClaim:    from,
			ToClaim:      to,
		})
	}
	return diffs
}

// pressPositionSwapMode is the planner's local name for
// domain.SwapModePressPosition — the marker stamped on every synthesized
// per-position claim so the planner routes it to the simple per-position
// builder rather than back into the press-index branch.
//
// The definition moved to domain (2026-08-05) because the station view has
// to derive the same claim: a fanned-out position owns a task and an order but
// has no style_node_claims row, so the view must synthesize one to render it
// as claimed. See domain.SynthesizePositionClaim for that scar.
//
// Still never persisted — UpsertClaim rejects it — so steady-state DB rows
// never carry this value. What changed is that VIEW code now does need to
// know it exists; the old comment here claimed it never would.
const pressPositionSwapMode = domain.SwapModePressPosition

// PlanPressPositions is the one pass that decides every press position either
// style names: outgoing or incoming, front or back, same mode or cross mode. A
// press position is a node a press-index claim names (NodeClaim.Positions), and
// each gets at most one diff, decided by one rule over its two claims
// (pressPosition), the situation domain.ChangeoverSituation's over the position
// claims:
//
//   - both styles use it: its marked tool-change treatment when the outgoing
//     claim marks it; its own swap when the carrier type changes
//     (binTypesDiffer); else, when the cell is planned position by position,
//     its own situation (the same part is unchanged); else nothing, because
//     the index handles it: the press's own swap indexes the position, or,
//     across a front that moves, the incoming press indexes the carrier of its
//     own type standing there;
//   - only the outgoing style uses it: a drop;
//   - only the incoming style uses it: an add, a plain delivery, unless its seat
//     says a bin is bound there (SeatBin), when it is the per-position swap,
//     both claims the incoming claim's: whatever stands there goes to the
//     incoming claim's outbound and the new carrier comes.
//
// A cell is planned position by position (splitsPerPosition) when one diff
// holds both styles' claims of the press and the press's own swap cannot carry
// it: the carrier type changes, the outgoing claim marks positions (a tool
// change never indexes), or a position the outgoing style stands on holds no
// bin (vacantPositions, the answer REQUEST's planVacantLine reads). Otherwise
// the diff stays whole, and every position a claim names that no diff stands on
// (a press dropped, added, moved, resized or changed to another mode) is decided
// by the same rule, appended in name order.
//
// AN UNKNOWN SEAT AT AN INCOMING-ONLY POSITION IS AN ADD. The Edge knows a
// position is empty only once a departure was recorded there (SeatEmpty), and a
// position no style has used yet has had none. A swap there lifts before it
// sets down, and at Core a lift at a node with nothing on it holds the swap's
// reserve with no limit (reserve-holding), which held a changeover's cutover
// until its leg was aborted. A carrier standing there that the Edge does not
// know of is Core's to see: the delivery's drop meets Core's capacity check for
// the node.
//
// ApplyNodeState, which runs next with the same seats, keeps the done rule and
// the vacant-seat rule for a position an outgoing claim stands on. state nil
// reads no seat: every seat unknown.
func PlanPressPositions(diffs []ChangeoverNodeDiff, binTypes map[string]string, state func(coreNode string) NodeState) []ChangeoverNodeDiff {
	seat := func(n string) Seat {
		if state == nil {
			return SeatUnknown
		}
		return state(n).Seat
	}
	out := make([]ChangeoverNodeDiff, 0, len(diffs))
	for _, d := range diffs {
		switch {
		case splitsPerPosition(d, binTypes, state):
			for _, pos := range cellPositions(d.FromClaim, d.ToClaim) {
				if pd, ok := pressPosition(pos, claimNaming(d.FromClaim, pos), claimNaming(d.ToClaim, pos), true,
					binTypes, seat); ok {
					out = append(out, pd)
				}
			}
		case d.Situation == SituationAdd && d.FromClaim == nil && d.ToClaim.IsPressIndex() && seat(d.CoreNodeName) == SeatBin:
			// The incoming press's own front, which no outgoing claim stands
			// on: DiffStyleClaims made it the Add, and a bin bound there makes
			// it the swap, as at every incoming-only position.
			out = append(out, incomingSwap(d.ToClaim, d.CoreNodeName))
		default:
			out = append(out, d)
		}
	}
	return append(out, unplannedPositions(out, diffs, binTypes, seat)...)
}

// splitsPerPosition reports whether a diff's cell is planned position by
// position rather than by the press's own swap (PlanPressPositions).
func splitsPerPosition(d ChangeoverNodeDiff, binTypes map[string]string, state func(coreNode string) NodeState) bool {
	return shouldFanOutPressIndex(d, binTypes) || pressHasVacantPosition(d, state)
}

// pressHasVacantPosition reports whether a press-index Swap's or tooling
// Evacuate's outgoing positions include one whose seat says its carrier has
// gone. An unknown seat reads occupied, as a missing occupancy answer does on
// REQUEST.
func pressHasVacantPosition(d ChangeoverNodeDiff, state func(coreNode string) NodeState) bool {
	if state == nil || (d.Situation != SituationSwap && d.Situation != SituationEvacuate) ||
		!d.FromClaim.IsPressIndex() || !d.ToClaim.IsPressIndex() {
		return false
	}
	occupancy := map[string]bool{}
	for _, n := range d.FromClaim.Positions() {
		occupancy[n] = state(n).Seat != SeatEmpty
	}
	lineVacant, paired := vacantPositions(d.FromClaim, occupancy)
	return lineVacant || len(paired) > 0
}

// binTypesDiffer reports whether two claims' payloads resolve to different bin
// type codes. The press's index motion can only shift bins across positions when
// the geometry matches, so this is the predicate that decides whether a position
// can ride the index or must be independently evac+refilled.
//
// An unresolved code on either side ("" — the Edge's synced bin-type catalog
// has no row for the payload) is an UNKNOWN signal, not a mismatch: press
// geometry rarely changes between styles, so missing catalog data is far more
// likely than an actual change. Callers treat unknown as "same" and leave the
// position alone. refusePressIndexWhenCoreUnavailable refuses a press-index
// changeover only before Core's catalog has arrived; once it has, a payload
// the catalog has no row for reads as "same", and if its carrier really changes
// at the index the position keeps the old carrier type. That is the known
// exposure of reading bin types from the catalog rather than asking Core.
//
// Both fan-out passes share this predicate; it must not be reimplemented at
// either call site.
func binTypesDiffer(from, to *processes.NodeClaim, binTypes map[string]string) bool {
	if from == nil || to == nil {
		return false
	}
	fromBT, toBT := binTypes[from.PayloadCode], binTypes[to.PayloadCode]
	if fromBT == "" || toBT == "" {
		return false
	}
	return fromBT != toBT
}

// binTypesKnownSame is binTypesDiffer's POSITIVE twin: both codes resolved AND
// equal. It reads the same map, computed by the same binTypeSnapshot call, so
// the sequential reuse-skip and the press-index fan-out can never disagree about
// what a style rides.
//
// ── IT IS NOT !binTypesDiffer, AND THE UNKNOWN CASE IS WHY ────────────────
//
// binTypesDiffer answers "same" when either code is unresolved. Nothing refuses
// that case once Core's catalog has arrived (refusePressIndexWhenCoreUnavailable
// refuses only before it has), so for press-index unknown is read as "same" and
// the position is left alone: the lesser error there.
//
// The sequential skip has the opposite exposure. It has no such gate, and acting
// on unknown means SKIPPING a side — not swapping a carrier that may be wrong.
// So it demands a positive answer: no catalog, no skip, build both orders. The
// degraded direction is a redundant carrier swap, which is still a swap: on a
// parked side whose seat is unknown and that turns out empty, its lift holds at
// Core (see ApplyNodeState's done rule).
func binTypesKnownSame(from, to *processes.NodeClaim, binTypes map[string]string) bool {
	if from == nil || to == nil {
		return false
	}
	fromBT, toBT := binTypes[from.PayloadCode], binTypes[to.PayloadCode]
	if fromBT == "" || toBT == "" {
		return false // no catalog answer — do not skip
	}
	return fromBT == toBT
}

// ApplySequentialReuseShortcut turns a produce sequential press's PARKED-side
// Swap into SituationUnchanged when the two styles ride the SAME carrier.
//
// ── THE INVARIANT CARRIES THE KNOWLEDGE, NOT AN INVENTORY READ ────────────
//
// This is press-index's answer to the identical question. A same-carrier
// press-index produce changeover never asks the floor what is on deck: it asks
// the CATALOG and leans on the mode's steady-state invariant — on-deck positions
// hold empties of the running style's carrier, because steady state put them
// there. Sequential has that invariant exactly: a produce press's parked side
// holds an empty of the running style's carrier.
//
// So if the carrier does not change, the empty already standing on the parked
// side is the empty the new style wants. Swapping it out and fetching an
// identical one back is a robot trip that moves nothing. No order, no button,
// no release for that side.
//
// PRODUCE ONLY (owner ruling 2026-08-28). A consume press's parked side holds a
// FULL standby of the OUTGOING style — material, not an empty carrier — and
// material is material: it has to leave and be replaced whatever the carrier
// does. Consume always changes over.
//
// Keyed on bin-type fit and never on same-payload: empties have no style, so a
// payload compare would answer "different" for two styles that ride one carrier,
// which is the whole population this skips.
func ApplySequentialReuseShortcut(
	diffs []ChangeoverNodeDiff, binTypes map[string]string, activePull map[string]bool,
) []ChangeoverNodeDiff {
	out := make([]ChangeoverNodeDiff, 0, len(diffs))
	for _, d := range diffs {
		if !sequentialParkedSideReuses(d, binTypes, activePull) {
			out = append(out, d)
			continue
		}
		d.Situation = SituationUnchanged
		out = append(out, d)
	}
	return out
}

// sequentialParkedSideReuses is the predicate above, as one readable question.
func sequentialParkedSideReuses(
	d ChangeoverNodeDiff, binTypes map[string]string, activePull map[string]bool,
) bool {
	if d.Situation != SituationSwap || d.FromClaim == nil || d.ToClaim == nil {
		return false
	}
	if d.FromClaim.SwapMode != protocol.SwapModeSequential {
		return false
	}
	if d.FromClaim.Role != protocol.ClaimRoleProduce {
		return false // consume always changes over
	}
	if !binTypesKnownSame(d.FromClaim, d.ToClaim, binTypes) {
		return false
	}
	inactive, _ := resolveSequentialActivePull(d.FromClaim, activePull)
	return inactive != "" && d.CoreNodeName == inactive
}

// shouldFanOutPressIndex reports whether the diff is a press-index
// changeover that is planned position by position: its bin types differ
// between from and to, or it is a tool change (the outgoing claim marks a
// position). A tool change never indexes: each marked position is cleared on
// its own and each unmarked one gets its own ordinary changeover, so the
// whole-cell index pair is never built for a marked press.
func shouldFanOutPressIndex(d ChangeoverNodeDiff, binTypes map[string]string) bool {
	if d.Situation != SituationSwap && d.Situation != SituationEvacuate {
		return false
	}
	if d.FromClaim == nil || d.ToClaim == nil {
		return false
	}
	if !d.FromClaim.IsPressIndex() {
		return false
	}
	return binTypesDiffer(d.FromClaim, d.ToClaim, binTypes) || domain.StagedToolingChangeover(d.FromClaim)
}

// cellPositions is every position a split cell's two claims name, the outgoing
// claim's first (front, paired, second paired), then those only the incoming
// claim names, in its order.
func cellPositions(from, to *processes.NodeClaim) []string {
	out := from.Positions()
	for _, pos := range to.Positions() {
		if !slices.Contains(out, pos) {
			out = append(out, pos)
		}
	}
	return out
}

// claimNaming is c when it names pos among its positions, else nil.
func claimNaming(c *processes.NodeClaim, pos string) *processes.NodeClaim {
	if slices.Contains(c.Positions(), pos) {
		return c
	}
	return nil
}

// pressPosition is PlanPressPositions' one rule for one press position: from
// and to are the outgoing and incoming claims that name it, nil for a style
// that does not, and split says the cell is planned position by position. ok
// false plans nothing there: the index handles it, or, on a cell that is not
// split, the claims leave it unchanged.
func pressPosition(pos string, from, to *processes.NodeClaim, split bool, binTypes map[string]string,
	seat func(string) Seat) (ChangeoverNodeDiff, bool) {
	pf, pt := synthesizePressPositionClaim(from, pos), synthesizePressPositionClaim(to, pos)
	var d ChangeoverNodeDiff
	switch {
	case from != nil && to != nil:
		switch {
		case split || domain.MarksNode(from, pos):
			d = ChangeoverNodeDiff{CoreNodeName: pos, Situation: domain.ChangeoverSituation(pf, pt), FromClaim: pf, ToClaim: pt}
		case binTypesDiffer(from, to, binTypes):
			d = ChangeoverNodeDiff{CoreNodeName: pos, Situation: SituationSwap, FromClaim: pf, ToClaim: pt}
		default:
			return ChangeoverNodeDiff{}, false // the index handles it
		}
	case from != nil:
		d = ChangeoverNodeDiff{CoreNodeName: pos, Situation: domain.ChangeoverSituation(pf, nil), FromClaim: pf}
	case to != nil:
		d = ChangeoverNodeDiff{CoreNodeName: pos, Situation: domain.ChangeoverSituation(nil, pt), ToClaim: pt}
		if d.Situation == SituationAdd && seat(pos) == SeatBin {
			d = incomingSwap(to, pos)
		}
	default:
		return ChangeoverNodeDiff{}, false
	}
	if !split && d.Situation == SituationUnchanged {
		return ChangeoverNodeDiff{}, false
	}
	return d, true
}

// incomingSwap is the per-position swap at a position only the incoming style
// uses, with a bin bound there: both claims the incoming claim's at pos
// (buildPressIndexPerPositionSwap builds nothing for a nil from-claim), so the
// bound carrier goes to the incoming claim's outbound and the new one comes.
func incomingSwap(to *processes.NodeClaim, pos string) ChangeoverNodeDiff {
	return ChangeoverNodeDiff{CoreNodeName: pos, Situation: SituationSwap,
		FromClaim: synthesizePressPositionClaim(to, pos), ToClaim: synthesizePressPositionClaim(to, pos)}
}

// synthesizePressPositionClaim is the planner's local name for
// domain.SynthesizePositionClaim. The body moved to domain so the
// station view derives per-position claims through the SAME function —
// see that doc comment for why (Hopkinsville 2026-08-05).
func synthesizePressPositionClaim(parent *processes.NodeClaim, coreNodeName string) *processes.NodeClaim {
	return domain.SynthesizePositionClaim(parent, coreNodeName)
}

// unplannedPositions is PlanPressPositions' rule over every position a press
// claim names beyond its own node (DependentPositions) that no planned diff
// stands on: the extension positions of a press dropped, added, resized, moved
// to another front or changed to another mode, which DiffStyleClaims, keyed on
// each claim's own node, never sees. It is the only place the two sides of a
// back shared across a front that moves meet (Hopkinsville #27, 2026-07-16: the
// tote-to-knockdown changeover stranded both back positions when nothing
// planned them). In name order, so a plan built twice reads the same.
func unplannedPositions(planned, diffs []ChangeoverNodeDiff, binTypes map[string]string, seat func(string) Seat) []ChangeoverNodeDiff {
	covered := make(map[string]bool, len(planned))
	for _, d := range planned {
		covered[d.CoreNodeName] = true
	}
	from, to := map[string]*processes.NodeClaim{}, map[string]*processes.NodeClaim{}
	for i := range diffs {
		for _, pos := range diffs[i].FromClaim.DependentPositions() {
			from[pos] = diffs[i].FromClaim
		}
		for _, pos := range diffs[i].ToClaim.DependentPositions() {
			to[pos] = diffs[i].ToClaim
		}
	}
	names := make([]string, 0, len(from)+len(to))
	for pos := range from {
		names = append(names, pos)
	}
	for pos := range to {
		if from[pos] == nil {
			names = append(names, pos)
		}
	}
	sort.Strings(names)
	var out []ChangeoverNodeDiff
	for _, pos := range names {
		if covered[pos] {
			continue
		}
		if d, ok := pressPosition(pos, from[pos], to[pos], false, binTypes, seat); ok {
			out = append(out, d)
		}
	}
	return out
}

// DrainState is what the runtime can say about the bin at a position, and it
// has THREE answers rather than two.
//
// The predicate behind it used to be a bool, so "the counter says no parts
// remain" and "there is no counter to ask" were the same answer — and at a
// press whose counter is not wired, RemainingUOPCached reads 0 forever, so that
// answer was "drained" at every position of that press. The shortcut below
// would then skip every same-part swap there and leave a partly-full bin at the
// line, silently. The opt-in flag was what stopped that from happening in
// practice, which made a per-claim policy switch the only thing standing
// between an unwired counter and a missed swap.
//
// DrainUnknown IS THE ZERO VALUE, deliberately: a closure that falls through a
// case, or a caller that has not wired one, answers "I do not know", and the
// shortcut never skips on that.
type DrainState int

const (
	// DrainUnknown: there is no counter reading to trust — no counter wired on
	// the process, no process_nodes row, no runtime row, or no bin bound at the
	// position, so "is the bin drained" has no answer.
	DrainUnknown DrainState = iota
	// Drained: a wired counter says no parts remain in the bin at this position.
	Drained
	// NotDrained: a wired counter says parts remain.
	NotDrained
)

// ApplyReuseCompatibleBinsShortcut rewrites Swap / Evacuate diffs to
// Unchanged when the from-claim is press-index, both claims are PRODUCE, the
// to-claim shares the same payload, AND the physical bin at the node is
// DRAINED (per the runtime check).
//
// Press-index hardware can keep the same bin between styles when the
// next style produces the same payload — no robot trip needed. Lives
// as a post-processor over DiffStyleClaims so the planner stays pure
// (no runtime-state dependency leaking into pure step builders).
//
// THERE IS NO PER-CLAIM OPT-IN. It used to also require the from-claim's
// ReuseCompatibleBins flag. Ruled 2026-09-09: the plant's part counters are
// trusted, so the shortcut is the behaviour rather than a switch. The seven
// live Hopkinsville claims that had it set are all produce positions on P400's
// PLN_01, and Springfield had none, so no cell at either plant loses a skip —
// what changes is that the other same-part press-index changeovers stop making
// a robot trip that takes a drained bin out and puts an equivalent one back.
//
// What the flag was really standing in front of is guarded properly now.
// drainState answers three ways, not two: an unwired press counter reads zero
// forever and used to be indistinguishable from a genuinely drained bin, so
// removing the opt-in without that would have skipped every same-part swap at
// such a press, silently. Unknown never skips, and a nil drainState
// short-circuits to no shortcut at all.
//
// DRAINED IS NOT EMPTY. Core's "empty" means a carrier with no payload code;
// this reads an Edge counter on a bin that still carries its payload and its
// manifest. See binDrainedAtCoreNode.
func ApplyReuseCompatibleBinsShortcut(diffs []ChangeoverNodeDiff, drainState func(coreNodeName string) DrainState) []ChangeoverNodeDiff {
	if drainState == nil {
		return diffs
	}
	for i := range diffs {
		d := &diffs[i]
		if d.FromClaim == nil || d.ToClaim == nil {
			continue
		}
		if d.Situation != SituationSwap && d.Situation != SituationEvacuate {
			continue
		}
		if !d.FromClaim.IsPressIndex() {
			continue
		}
		// PRODUCE ONLY. Empty is the desired state at a produce position — the
		// press is what fills it — so a drained bin there is one the next style
		// can go on filling. At a CONSUME position a drained bin is a starved
		// line: the changeover swap is what puts a full bin there for the start
		// of the new style, and nothing automatic would catch the miss
		// (auto_reorder is 0 on every live claim at both plants, and
		// Hopkinsville sets no reorder point at all). Ruled 2026-09-09, the same
		// answer the owner gave for the sequential reuse-skip on 2026-08-28.
		if d.FromClaim.Role != protocol.ClaimRoleProduce || d.ToClaim.Role != protocol.ClaimRoleProduce {
			continue
		}
		if d.FromClaim.PayloadCode != d.ToClaim.PayloadCode {
			continue
		}
		if drainState(d.CoreNodeName) != Drained {
			continue
		}
		d.Situation = SituationUnchanged
	}
	return diffs
}

// NodeState is what the changeover planner reads about a node before it
// plans it: the seat (a bin bound, none, or unknown), the part the carrier on
// it is (the lineside identity, written only through recordLinesideCarrier),
// and the counter's word on whether that carrier is drained. One reader
// (Engine.nodeStateAt) serves every rule below and the start gate's seat read.
type NodeState struct {
	Seat      Seat
	Part      string
	PartKnown bool
	Drain     DrainState
	// runtime is the row the rest was read from, for the old carrier's aim
	// (residentEvacDest), which asks of the same row.
	runtime *processes.RuntimeState
}

// ApplyNodeState plans each changing node from what stands on it. Two rules,
// one pass, one reader:
//
//   - EMPTY IS ADD. A Swap whose seat says its carrier has gone becomes
//     SituationAdd. A swap's legs lift the node's bin before they set the new
//     one down, and a leg that lifts at an empty node waits at Core for a bin
//     that is not coming (the allocator holds it, with no limit). There is
//     nothing to take away, so the new style's bin is simply brought. A
//     position no outgoing claim stands on never reaches this rule as a Swap
//     with an empty seat: PlanPressPositions planned it from its seat already.
//     The same holds for ONE press position's tooling Evacuate (a split
//     press's per-position diff): its clearance lifts first too, and an empty
//     position is already clear (SEND BACK dry, then START, left the outside so:
//     the press tranche's sim, 2026-10-23). The tooling decorator holds the
//     Add's carrier at staging until the tooling is done, as it holds every
//     inbound leg of the cell, and gives it no carry-over round trip.
//
//   - ALREADY THE NEW PART IS DONE. A Swap or Add whose node already holds the
//     incoming style's part is planned Unchanged and marked AlreadyDone. The
//     style only flips at cutover, so a changeover planned again before it —
//     the restart after a cancel — used to swap again every node the first
//     attempt finished, pulling a carrier that had just been delivered (SPR
//     2026-10-14, ALN_003, a 767D2 carrier 42 minutes on the line).
//
// THE DONE RULE IS NARROW ON PURPOSE, and every arm of it is a reason:
//
//   - CONSUME ON BOTH SIDES. A consume carrier is labelled with its part on
//     delivery, so "holds the new part" is a fact. A produce position holds an
//     empty, and an empty is not the new style's more than the old one's.
//   - ONE POSITION. A claim with paired positions (sequential A/B, an unsplit
//     indexing press) is decided for the cell, and one node's carrier says
//     nothing about the others; those plan exactly as before. A split press
//     is one-position claims, each judged by its own seat.
//   - KNOWN, AND NOT DRAINED. An unknown carrier plans the swap. That is not
//     free: on a node that turns out to be empty, the swap's lift finds no bin
//     at Core and the order holds its reserve there with no limit
//     (reserve-holding) until someone clears it, holding the changeover's
//     cutover with it. It is kept because a done node wrongly left Unchanged
//     would keep the old part on the line. A drained consume carrier is a
//     starved line, the same ruling as the reuse shortcut's.
//
// Evacuate is the tooling choreography, whose clearance does not depend on
// what the carrier is: the done rule never touches it, and a whole press's
// Evacuate is left as it is (PlanPressPositions has already split one with a
// vacant position). A nil state changes nothing.
func ApplyNodeState(diffs []ChangeoverNodeDiff, state func(coreNodeName string) NodeState) []ChangeoverNodeDiff {
	if state == nil {
		return diffs
	}
	for i := range diffs {
		d := &diffs[i]
		if d.ToClaim == nil {
			continue
		}
		// One press position's tooling Evacuate takes the empty rule and never
		// the done rule.
		positionEvacuate := d.Situation == SituationEvacuate && d.ToClaim.SwapMode == pressPositionSwapMode
		if d.Situation != SituationSwap && d.Situation != SituationAdd && !positionEvacuate {
			continue
		}
		st := state(d.CoreNodeName)
		if st.Seat == SeatEmpty && (d.Situation == SituationSwap || positionEvacuate) {
			d.Situation = SituationAdd
			continue
		}
		if !positionEvacuate && holdsTheIncomingPart(*d, st) {
			d.Situation = SituationUnchanged
			d.AlreadyDone = true
		}
	}
	return diffs
}

func holdsTheIncomingPart(d ChangeoverNodeDiff, st NodeState) bool {
	onePositionConsume := func(c *processes.NodeClaim) bool {
		return c.Role == protocol.ClaimRoleConsume && c.PairedCoreNode == "" && c.SecondPairedCoreNode == ""
	}
	if !onePositionConsume(d.ToClaim) || (d.FromClaim != nil && !onePositionConsume(d.FromClaim)) {
		return false
	}
	return st.Seat == SeatBin && st.PartKnown && st.Part != "" &&
		st.Part == d.ToClaim.PayloadCode && st.Drain != Drained
}
