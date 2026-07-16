package engine

import (
	"sort"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// ChangeoverSituation classifies what needs to happen at a physical node
// when transitioning between two styles.
type ChangeoverSituation string

const (
	SituationUnchanged ChangeoverSituation = "unchanged" // same payload, no evacuation needed
	SituationEvacuate  ChangeoverSituation = "evacuate"  // same payload, but evacuation required
	SituationSwap      ChangeoverSituation = "swap"      // different payload — old out, new in
	SituationDrop      ChangeoverSituation = "drop"      // old style uses node, new style doesn't
	SituationAdd       ChangeoverSituation = "add"       // new style uses node, old style didn't
)

// ChangeoverNodeDiff represents the material change needed at a single physical
// node during a changeover.
type ChangeoverNodeDiff struct {
	CoreNodeName string
	Situation    ChangeoverSituation
	FromClaim    *processes.NodeClaim // nil for "add" situations
	ToClaim      *processes.NodeClaim // nil for "drop" situations
}

// DiffStyleClaims computes the material changes needed at each physical node
// when transitioning from one style to another.
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

	var diffs []ChangeoverNodeDiff
	for name := range nodeSet {
		from := fromMap[name]
		to := toMap[name]

		var situation ChangeoverSituation
		switch {
		case from == nil && to != nil:
			if to.PayloadCode == "__empty__" {
				situation = SituationUnchanged // node was empty, stays empty
			} else {
				situation = SituationAdd
			}
		case from != nil && to == nil:
			situation = SituationDrop
		case to != nil && to.PayloadCode == "__empty__":
			if from != nil && from.PayloadCode == "__empty__" {
				situation = SituationUnchanged // both empty → nothing to do
			} else {
				situation = SituationDrop // explicitly clear the node
			}
		case from != nil && from.PayloadCode == "__empty__":
			situation = SituationAdd // node was empty, now needs material
		case from.PayloadCode == to.PayloadCode && from.Role == to.Role:
			if to.EvacuateOnChangeover {
				situation = SituationEvacuate
			} else {
				situation = SituationUnchanged
			}
		default:
			situation = SituationSwap
		}

		diffs = append(diffs, ChangeoverNodeDiff{
			CoreNodeName: name,
			Situation:    situation,
			FromClaim:    from,
			ToClaim:      to,
		})
	}
	return diffs
}

// pressPositionSwapMode marks a synthesized per-position press-index
// claim emitted by FanOutPressIndexDifferentBinType. The parent's
// SwapMode (two_robot_press_index) is replaced with this value on each
// synthesized claim so the planner routes them to the simple per-
// position builder rather than back into the press-index branch.
//
// The marker is in-memory only — synthesized claims live in the diff
// list for a single planChangeover call and never get persisted to
// style_node_claims, so the schema's swap_mode column never sees this
// value and steady-state code paths don't need to know it exists.
const pressPositionSwapMode protocol.SwapMode = "press_position"

// FanOutPressIndexDifferentBinType rewrites a press-index Swap/Evacuate
// diff into one per-position diff whenever the from-claim's payload bin
// type differs from the to-claim's. The press's index motion can't shift
// bins of different geometries across positions, so each occupied/needed
// position is independently evac+refill — no coordination across
// positions, three robots fire in parallel because there are three
// independent NodeActions.
//
// Each synthesized diff carries:
//   - CoreNodeName: the per-position node name (front, paired, or
//     second-paired, depending on which slot)
//   - SwapMode: pressPositionSwapMode marker
//   - PayloadCode / InboundSource / OutboundDestination: copied from
//     the parent claim (assumption: same payload across positions
//     during different-bin-type changeover; SME confirmed)
//   - PairedCoreNode / SecondPairedCoreNode / InboundStaging /
//     OutboundStaging: empty (per-position is a single slot, no
//     staging hop, no A/B partner — these fields are press-index-
//     specific and don't apply to a single position)
//
// Position-count change handled implicitly via per-position
// occupied/needed checks:
//   - From-occupied AND to-needed → SituationSwap (or parent's
//     Situation if it was Evacuate): full evac+refill
//   - From-occupied, to-NOT-needed → SituationDrop: evac only
//   - From-NOT-occupied, to-needed → SituationAdd: refill only
//
// binTypes is a map[payloadCode]binTypeCode pre-resolved at plan time.
// nil/missing entries → comparator falls back to "same" → no fan-out,
// parent diff stays untouched. The existing same-bin-type press-index
// dispatch handles those.
func FanOutPressIndexDifferentBinType(diffs []ChangeoverNodeDiff, binTypes map[string]string) []ChangeoverNodeDiff {
	out := make([]ChangeoverNodeDiff, 0, len(diffs))
	for _, d := range diffs {
		if !shouldFanOutPressIndex(d, binTypes) {
			out = append(out, d)
			continue
		}
		out = append(out, fanOutPositions(d)...)
	}
	return out
}

// binTypesDiffer reports whether two claims' payloads resolve to different bin
// type codes. The press's index motion can only shift bins across positions when
// the geometry matches, so this is the predicate that decides whether a position
// can ride the index or must be independently evac+refilled.
//
// An unresolved code on either side ("" — no catalog entry, or Core didn't
// answer) is an UNKNOWN signal, not a mismatch: press geometry rarely changes
// between styles, so missing catalog data is far more likely than an actual
// change. Callers treat unknown as "same" and leave the position alone. That
// fallback is only safe because refusePressIndexWhenCoreUnavailable rejects the
// changeover outright when Core can't answer — without that gate this silently
// strands bins.
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

// shouldFanOutPressIndex reports whether the diff is a press-index
// changeover whose bin types differ between from and to. Only those
// expand into per-position diffs.
func shouldFanOutPressIndex(d ChangeoverNodeDiff, binTypes map[string]string) bool {
	if d.Situation != SituationSwap && d.Situation != SituationEvacuate {
		return false
	}
	if d.FromClaim == nil || d.ToClaim == nil {
		return false
	}
	if d.FromClaim.SwapMode != protocol.SwapModeTwoRobotPressIndex {
		return false
	}
	return binTypesDiffer(d.FromClaim, d.ToClaim, binTypes)
}

// fanOutPositions expands one press-index diff into up to three
// per-position diffs (front, paired, second-paired) based on which
// positions are occupied in from and needed in to.
func fanOutPositions(parent ChangeoverNodeDiff) []ChangeoverNodeDiff {
	from := parent.FromClaim
	to := parent.ToClaim
	// Press-index physical layout: the from-claim's geometry fields
	// define which positions exist (front always; paired iff
	// PairedCoreNode set; second-paired iff SecondPairedCoreNode set).
	// The to-claim may or may not claim each position.
	type slot struct {
		fromName string // empty when from-style doesn't occupy this position
		toName   string // empty when to-style doesn't claim this position
	}
	slots := []slot{
		{fromName: from.CoreNodeName, toName: to.CoreNodeName},
		{fromName: from.PairedCoreNode, toName: to.PairedCoreNode},
		{fromName: from.SecondPairedCoreNode, toName: to.SecondPairedCoreNode},
	}

	var diffs []ChangeoverNodeDiff
	for _, s := range slots {
		if s.fromName == "" && s.toName == "" {
			continue
		}
		// Use the position's own node name as the synthesized
		// CoreNodeName. When from has it but to doesn't, use from's
		// name; vice versa for to-only.
		coreNodeName := s.fromName
		if coreNodeName == "" {
			coreNodeName = s.toName
		}
		switch {
		case s.fromName != "" && s.toName != "":
			// Full per-position swap (both sides occupy).
			diffs = append(diffs, ChangeoverNodeDiff{
				CoreNodeName: coreNodeName,
				Situation:    parent.Situation,
				FromClaim:    synthesizePressPositionClaim(from, coreNodeName),
				ToClaim:      synthesizePressPositionClaim(to, coreNodeName),
			})
		case s.fromName != "" && s.toName == "":
			// Evac only — from-style had this position, to-style
			// doesn't claim it. SituationDrop emits a release order
			// (pickup CoreNodeName → dropoff OutboundDestination)
			// via the existing planner branch, which is exactly the
			// per-position evac-only choreography.
			diffs = append(diffs, ChangeoverNodeDiff{
				CoreNodeName: coreNodeName,
				Situation:    SituationDrop,
				FromClaim:    synthesizePressPositionClaim(from, coreNodeName),
				ToClaim:      nil,
			})
		case s.fromName == "" && s.toName != "":
			// Refill only — to-style claims this position, from
			// didn't have it. SituationAdd routes to
			// planFallbackStagingAction. Synthesized to-claim has
			// InboundStaging empty, so the fallback emits a Retrieve
			// order delivering directly to the position node — the
			// per-position refill-only choreography.
			diffs = append(diffs, ChangeoverNodeDiff{
				CoreNodeName: coreNodeName,
				Situation:    SituationAdd,
				FromClaim:    nil,
				ToClaim:      synthesizePressPositionClaim(to, coreNodeName),
			})
		}
	}
	return diffs
}

// synthesizePressPositionClaim builds a per-position claim from a
// parent press-index claim. CoreNodeName is set to the position's name;
// SwapMode is the press_position marker; press-index-only fields
// (PairedCoreNode, SecondPairedCoreNode) are zeroed; staging fields are
// zeroed (per-position uses direct trips). Other fields (PayloadCode,
// InboundSource, OutboundDestination, Role, UOPCapacity, etc.) are
// copied from the parent.
//
// The synthesized claim's ID is the parent's ID. Per-position node
// tasks reference this ID so wiring lookups can resolve back to the
// real persisted parent claim — the synthesized in-memory object is
// only used by the planner for routing.
func synthesizePressPositionClaim(parent *processes.NodeClaim, coreNodeName string) *processes.NodeClaim {
	c := *parent
	c.CoreNodeName = coreNodeName
	c.SwapMode = pressPositionSwapMode
	c.PairedCoreNode = ""
	c.SecondPairedCoreNode = ""
	c.InboundStaging = ""
	c.OutboundStaging = ""
	// ReuseCompatibleBins is press-index-only; clear it so the
	// reuse-compatible-bins shortcut doesn't try to apply per-position.
	c.ReuseCompatibleBins = false
	// KeepStaged shouldn't trigger inside per-position routing.
	c.KeepStaged = false
	return &c
}

// FanOutPressIndexCrossMode emits per-position Drop or Add diffs for
// press-index positions that aren't covered by any existing diff after
// the same-mode different-bin-type fan-out has run. Handles the cases
// the same-mode fan-out doesn't:
//
//   - Cross-mode: from-claim is press-index, to-claim is some other
//     SwapMode (single_robot, two_robot, sequential). The shared
//     CoreNodeName diff (front position) gets handled normally; the
//     middle/back positions only appear on the press-index claim's
//     PairedCoreNode/SecondPairedCoreNode fields and would otherwise
//     leave bins on the press unmoved.
//   - Mirror cross-mode: to-claim is press-index, from-claim is not.
//     Synthesize Add diffs for the press-index extension positions so
//     the new style gets its bins delivered.
//   - Same-mode position-count delta with same bin types: both sides
//     are press-index, bin types match (so the same-mode fan-out
//     didn't fire), but position counts differ (e.g. 3-pos to 2-pos).
//     Synthesize Drop for the dropped position; Add for the added
//     position.
//   - Shared extension position across a FRONT-NODE MOVE, different bin
//     types: both styles are press-index and both name the same back
//     seat, but their front nodes differ (PLN_01 → PLN_03, both paired
//     to PLN_02). No CoreNodeName is shared, so DiffStyleClaims emits
//     Drop + Add and the same-mode fan-out — which needs Swap/Evacuate
//     and both claims on ONE diff — never fires. posMap is the only
//     place the two sides of that position meet. Synthesize a per-
//     position Swap so it gets evac+refill. (HK #27, 2026-07-16.)
//
// Runs AFTER FanOutPressIndexDifferentBinType. This pass only acts on
// positions NOT in the existing diff list (after the same-mode
// expansion), so there's no precedence conflict — the same-mode pass
// always wins for any position it touches; this pass picks up the
// leftovers.
//
// Synthesized diffs route through the existing SituationDrop,
// SituationAdd and SituationSwap branches in planNodeAction. Drop and
// Add don't read SwapMode (the per-position claim's SwapMode =
// press_position is cosmetic there): Drop calls BuildReleaseSteps using
// OutboundDestination; Add calls planFallbackStagingAction which uses
// InboundSource (and InboundStaging if set; synthesized claims clear
// InboundStaging so the direct-Retrieve fallback fires, delivering to
// the position node directly). Swap DOES read it, routing to
// buildPressIndexPerPositionSwap — the single-order evac+refill.
//
// Diff list ordering: appended after the input diffs in stable order
// (sorted by position name) so test assertions are deterministic and
// log output is predictable.
func FanOutPressIndexCrossMode(diffs []ChangeoverNodeDiff, binTypes map[string]string) []ChangeoverNodeDiff {
	covered := make(map[string]struct{}, len(diffs))
	for _, d := range diffs {
		covered[d.CoreNodeName] = struct{}{}
	}

	// posState tracks, for each press-index extension position, which
	// side(s) reference it and the parent claim that owns it.
	type posState struct {
		fromClaim *processes.NodeClaim
		toClaim   *processes.NodeClaim
	}
	posMap := make(map[string]*posState)
	noteFrom := func(name string, claim *processes.NodeClaim) {
		if name == "" {
			return
		}
		s := posMap[name]
		if s == nil {
			s = &posState{}
			posMap[name] = s
		}
		s.fromClaim = claim
	}
	noteTo := func(name string, claim *processes.NodeClaim) {
		if name == "" {
			return
		}
		s := posMap[name]
		if s == nil {
			s = &posState{}
			posMap[name] = s
		}
		s.toClaim = claim
	}
	for i := range diffs {
		d := &diffs[i]
		if fc := d.FromClaim; fc != nil && fc.SwapMode == protocol.SwapModeTwoRobotPressIndex {
			noteFrom(fc.PairedCoreNode, fc)
			noteFrom(fc.SecondPairedCoreNode, fc)
		}
		if tc := d.ToClaim; tc != nil && tc.SwapMode == protocol.SwapModeTwoRobotPressIndex {
			noteTo(tc.PairedCoreNode, tc)
			noteTo(tc.SecondPairedCoreNode, tc)
		}
	}

	// Sort positions for stable iteration / test determinism.
	names := make([]string, 0, len(posMap))
	for name := range posMap {
		names = append(names, name)
	}
	sort.Strings(names)

	added := make([]ChangeoverNodeDiff, 0, len(names))
	for _, pos := range names {
		if _, alreadyCovered := covered[pos]; alreadyCovered {
			continue
		}
		s := posMap[pos]
		switch {
		case s.fromClaim != nil && s.toClaim != nil:
			// Both styles' press-index extension fields reference this
			// position.
			//
			// Same bin type: the position stays in place and its bin is
			// handled by the press's index motion at the front-position
			// swap. Nothing to do.
			//
			// Different bin type: the position needs its own evac+refill —
			// the index can't shift geometries. It is NOT safe to assume the
			// same-mode fan-out already covered it: that pass only fires on
			// Swap/Evacuate, and a changeover that MOVES the front node
			// between styles (PLN_01 → PLN_03, both paired to PLN_02) diffs
			// as Drop + Add, so no single diff ever holds both sides of this
			// position. THIS pass is the only place the two sides meet, via
			// posMap. Hopkinsville 2026-07-16 (changeover #27, tote →
			// knockdown) stranded both back seats on exactly that assumption.
			if !binTypesDiffer(s.fromClaim, s.toClaim, binTypes) {
				continue
			}
			added = append(added, ChangeoverNodeDiff{
				CoreNodeName: pos,
				Situation:    SituationSwap,
				FromClaim:    synthesizePressPositionClaim(s.fromClaim, pos),
				ToClaim:      synthesizePressPositionClaim(s.toClaim, pos),
			})
		case s.fromClaim != nil:
			// From-side has it; nobody on the to-side claims it.
			// Synthesize Drop — bin leaves to OutboundDestination.
			added = append(added, ChangeoverNodeDiff{
				CoreNodeName: pos,
				Situation:    SituationDrop,
				FromClaim:    synthesizePressPositionClaim(s.fromClaim, pos),
			})
		case s.toClaim != nil:
			// To-side wants it; from-side didn't have it.
			// Synthesize Add — new bin delivered from
			// InboundSource directly to position (Add branch's
			// fallback Retrieve order, since synthesized claim
			// clears InboundStaging).
			added = append(added, ChangeoverNodeDiff{
				CoreNodeName: pos,
				Situation:    SituationAdd,
				ToClaim:      synthesizePressPositionClaim(s.toClaim, pos),
			})
		}
	}
	return append(diffs, added...)
}

// ApplyReuseCompatibleBinsShortcut rewrites Swap / Evacuate diffs to
// Unchanged when the from-claim is press-index, the to-claim shares the
// same payload, the from-claim opted in via ReuseCompatibleBins, AND the
// physical bin at the node is empty (per the runtime check).
//
// Press-index hardware can keep the same bin between styles when the
// next style produces the same payload — no robot trip needed. Lives
// as a post-processor over DiffStyleClaims so the planner stays pure
// (no runtime-state dependency leaking into pure step builders).
//
// isEmpty is a runtime accessor: given a CoreNodeName, returns true if
// the physical bin at the slot is empty. nil isEmpty short-circuits to
// "not empty" → no shortcut applied (defensive default).
func ApplyReuseCompatibleBinsShortcut(diffs []ChangeoverNodeDiff, isEmpty func(coreNodeName string) bool) []ChangeoverNodeDiff {
	if isEmpty == nil {
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
		if d.FromClaim.SwapMode != protocol.SwapModeTwoRobotPressIndex {
			continue
		}
		if !d.FromClaim.ReuseCompatibleBins {
			continue
		}
		if d.FromClaim.PayloadCode != d.ToClaim.PayloadCode {
			continue
		}
		if !isEmpty(d.CoreNodeName) {
			continue
		}
		d.Situation = SituationUnchanged
	}
	return diffs
}
