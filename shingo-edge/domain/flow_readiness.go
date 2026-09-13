package domain

import (
	"fmt"
	"sort"
	"strings"

	"shingoedge/domain/flowspec"
)

// NodeFinding is one thing a changeover between two styles cannot do at one
// node: a field the outgoing mode's builder will read that is blank on the
// side it reads it from.
type NodeFinding struct {
	CoreNodeName string         `json:"core_node_name"`
	Side         flowspec.Side  `json:"side"`
	Field        flowspec.Field `json:"field"`
	Severity     string         `json:"severity"`
	Message      string         `json:"message"`
}

// ValidateFlowChangeoverReadiness answers, before anyone presses START, the
// question the changeover planner answers after: for a changeover from the
// claims of one style to the claims of another, which node would the planner
// refuse, and on which field.
//
// PURE, and pairwise on purpose. The save-time validator is unary — one claim,
// one mode — and the planner switches on the OUTGOING claim's mode while
// reading fields off both sides (requiredChangeoverFields), so a claim that
// saved clean can still be the wrong half of a pair. This is the pairwise
// check, at preview time, for every writer: the editor, the compare grid, the
// composer. No caller yet; the composer's preview calls it, the desktop
// renderer calls it.
//
// MATCHING MIRRORS engine.DiffStyleClaims without importing it: claims pair by
// CoreNodeName across the union of both sides. A node on one side only is an
// add or a drop, and the planner consults no field registry for those, so
// they produce no finding here either. A pair whose payload and role are
// unchanged and that evacuates nothing builds no orders, so it produces none
// either; anything else is a swap or an evacuation and the planner will read
// flowspec.Changeover(from.SwapMode) for it, so this does too, in the same
// order, and reports each blank Required entry as an error naming the node,
// the side and the field.
//
// nil, nil is a valid input and yields nil: two styles with no claims have
// nothing to be unready about. Findings are ordered by node name, then in
// flowspec.ChangeoverOrder, so two runs over the same claims render the same
// list.
func ValidateFlowChangeoverReadiness(from, to []NodeClaim) []NodeFinding {
	fromByNode := make(map[string]*NodeClaim, len(from))
	for i := range from {
		fromByNode[from[i].CoreNodeName] = &from[i]
	}
	toByNode := make(map[string]*NodeClaim, len(to))
	for i := range to {
		toByNode[to[i].CoreNodeName] = &to[i]
	}
	names := make([]string, 0, len(fromByNode)+len(toByNode))
	seen := map[string]bool{}
	for _, c := range from {
		if !seen[c.CoreNodeName] {
			seen[c.CoreNodeName] = true
			names = append(names, c.CoreNodeName)
		}
	}
	for _, c := range to {
		if !seen[c.CoreNodeName] {
			seen[c.CoreNodeName] = true
			names = append(names, c.CoreNodeName)
		}
	}
	sort.Strings(names)

	var out []NodeFinding
	for _, name := range names {
		f, t := fromByNode[name], toByNode[name]
		if !changeoverBuildsOrders(f, t) {
			continue
		}
		spec := flowspec.Changeover(f.SwapMode)
		for _, sf := range flowspec.ChangeoverOrder() {
			if spec[sf] != flowspec.Required {
				continue
			}
			claim := f
			if sf.Side == flowspec.SideTo {
				claim = t
			}
			if ClaimHas(claim, sf.Field) {
				continue
			}
			out = append(out, NodeFinding{
				CoreNodeName: name,
				Side:         sf.Side,
				Field:        sf.Field,
				Severity:     SeverityError,
				Message: fmt.Sprintf("%s: the %s-claim has no %s, and a %s changeover cannot be built without one",
					name, sf.Side, flowspec.Label(sf.Field), f.SwapMode),
			})
		}
	}
	return out
}

// changeoverBuildsOrders mirrors the arms of engine.DiffStyleClaims that lead
// to a swap or an evacuation — the two situations for which the planner
// consults the field registry. Everything else (an add, a drop, an unchanged
// node, a node empty on both sides) builds no per-mode order and so cannot be
// unready in the sense this validator measures.
func changeoverBuildsOrders(from, to *NodeClaim) bool {
	if from == nil || to == nil {
		return false // add or drop: no registry consulted
	}
	const empty = "__empty__"
	if to.PayloadCode == empty || from.PayloadCode == empty {
		return false // explicit clear, or a node that was empty: drop / add
	}
	if ToolingClearanceApplies(from, to) {
		return true // staged tooling evacuation, whatever the payloads do
	}
	if from.PayloadCode == to.PayloadCode && from.Role == to.Role {
		return from.EvacuateOnChangeover // same part: only a whole-node evacuation builds orders
	}
	return true // swap
}

// ValidateFlowPartsPlaced answers a question the per-cell validator cannot:
// does every part this style runs have a position to run on? (Owner ruling
// R8, 2026-09-12.)
//
// WHY IT IS A FINDING AND NOT A REFUSAL. An apply that leaves parts unplaced
// SAVES — the engineer is mid-way through moving a part's flow onto a new
// shape and a half-finished flow on a screen is not a reason to lose the
// work. What must not happen is a CHANGEOVER the robots cannot serve: a
// changeover to a style with a part no position carries would deliver nothing
// for that part and the line would run dry on it. So it reports as an error
// finding, the bar and the set-up card show it, and START is what it blocks.
//
// stored is the style's flow as the database holds it; draft is the flow the
// request would leave behind. A payload code that stored names and draft does
// not is a part the flow no longer carries. Read that way round on purpose:
// the style's parts are its claims — there is no other record of what a style
// runs — so the only honest "this part lost its position" is the comparison
// with what the style had a moment ago.
//
// __empty__ is not a part. It is how a flow says a position is deliberately
// clear, and a finding about it would be a finding about nothing.
//
// One finding for all of them, with no node: the answer to "which position?"
// is the whole point of the finding, so naming one would be inventing it.
func ValidateFlowPartsPlaced(stored, draft []NodeClaim) []NodeFinding {
	const empty = "__empty__"
	// A FLOW WITH NO CELLS LEAVES NOTHING UNPLACED. An empty draft is a flow
	// being torn down or not yet built, and the preview already answers that
	// with "this flow fires no orders" — the same rule the model follows, so
	// the two surfaces raise this on the same drafts.
	if len(draft) == 0 {
		return nil
	}
	placed := map[string]bool{}
	for _, c := range draft {
		if c.PayloadCode != "" {
			placed[c.PayloadCode] = true
		}
	}
	var lost []string
	seen := map[string]bool{}
	for _, c := range stored {
		code := c.PayloadCode
		if code == "" || code == empty || placed[code] || seen[code] {
			continue
		}
		seen[code] = true
		lost = append(lost, code)
	}
	if len(lost) == 0 {
		return nil
	}
	sort.Strings(lost)
	msg := fmt.Sprintf("%d parts need a position", len(lost))
	if len(lost) == 1 {
		msg = "1 part needs a position"
	}
	return []NodeFinding{{
		Side:     flowspec.SideTo,
		Field:    flowspec.PayloadCode,
		Severity: SeverityError,
		Message:  msg + ": " + strings.Join(lost, ", "),
	}}
}
