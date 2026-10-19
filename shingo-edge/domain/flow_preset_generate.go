package domain

import (
	"fmt"
	"sort"

	"shingo/protocol"
	"shingoedge/domain/flowspec"
)

// flow_preset_generate.go — "New shape": the recipes a cell COULD run,
// proposed from its positions and routing set, for an engineer to confirm and
// name on the desktop (pick-a-part brief, unit U2).
//
// WHAT IT READS AND WHAT IT MUST NOT. It reads the cell's positions in picture
// order, the routing set, the process's prior claims (for role only) and the
// engineer's picks. It does not read payloads or the catalog (a recipe is a
// shape, never a part), a position's Kind (Kind marks both sides of a
// sequential pair `back`, so it cannot say which side owns a row — the
// engineer's pairing does), routing `sequence` as a pairing (it is typing
// order; it is read only as "the lowest of a role is the default source or
// destination"), the flow-composer gate, or the database.
//
// TABLE-DRIVEN OVER flowspec.Steady, NO CASE PER MODE. Which fields a mode
// needs is flowspec's answer: a Required field is filled or the mode is not
// offered, a Forbidden or Unused one is left blank, a Used one is filled only
// from the engineer's picks or, for source and destination, from the routing
// set's lowest row of that role. TestGenerateShapes_ARequiredFieldNobodyFillsIsNotOffered
// swaps in a fake Required field to pin that the table, not a mode name, is
// what decides.

// ShapePicks are the engineer's answers to the questions a cell cannot answer
// for itself. Every field is optional: a nil map means "propose".
type ShapePicks struct {
	// Mode is the one choreography to generate; blank generates every mode the
	// cell can satisfy.
	Mode protocol.SwapMode
	// Pairs maps a position to its partner. The KEY owns the row: in a mode
	// whose partner is a satellite of one claim (it can carry a second
	// partner — press-index) only the key gets a cell; in a mode where both
	// sides run (sequential) both get one, each naming the other.
	Pairs map[string]string
	// Produce marks the positions that make parts. Nil means "derive from the
	// process's claims"; when there are none every position is consume and the
	// candidate carries a note asking the question.
	Produce map[string]bool
	// Staging is a position's [inbound, outbound] staging lane.
	Staging map[string][2]string
	// Back is the positions the process's claims already use as a partner or
	// a staging place (BackPositionNames) — the on-deck spots. In a mode whose
	// partner is a satellite (press index) a back position is proposed only
	// as a partner, never as the position that owns the row. Nil: nothing
	// known, and the proposal pairs neighbours.
	Back map[string]bool
}

// ShapeCandidate is one generated recipe: the mode, its cells (payload-free),
// and the notes an engineer should read before naming it. Notes are never
// refusals: a shared staging lane is legal (10-05 F11) and says so.
type ShapeCandidate struct {
	Mode  protocol.SwapMode `json:"mode"`
	Cells []FlowCell        `json:"cells"`
	Notes []string          `json:"notes,omitempty"`
}

// steadyFor is flowspec.Steady, as a variable so the fake-Required pin can
// swap the table it reads. Nothing else assigns it.
var steadyFor = flowspec.Steady

// GenerateShapes proposes one candidate per mode the cell can satisfy (or for
// picks.Mode alone). positions are the cell's positions in picture order —
// the engineer's listing order, then name.
func GenerateShapes(positions []string, routing []RoutingNode, prior []NodeClaim, picks ShapePicks) []ShapeCandidate {
	modes := protocol.ConfigurableSwapModes()
	if picks.Mode != "" {
		modes = []protocol.SwapMode{picks.Mode}
	}
	var out []ShapeCandidate
	for _, m := range modes {
		// A mode with no part is a loader card, not a recipe the floor fills.
		if steadyFor(protocol.ClaimRoleConsume, m)[flowspec.PayloadCode] == flowspec.Forbidden {
			continue
		}
		if c, ok := generateOne(m, positions, routing, prior, picks); ok {
			out = append(out, c)
		}
	}
	return out
}

func generateOne(mode protocol.SwapMode, positions []string, routing []RoutingNode, prior []NodeClaim, picks ShapePicks) (ShapeCandidate, bool) {
	cand := ShapeCandidate{Mode: mode}
	row := steadyFor(protocol.ClaimRoleConsume, mode)
	paired := row[flowspec.PairedCoreNode] != flowspec.Forbidden && row[flowspec.PairedCoreNode] != flowspec.Unused
	// A mode that can carry a second partner treats the partner as a
	// satellite of one claim (press-index: the back position has no row).
	satellite := row[flowspec.SecondPairedCoreNode] != flowspec.Forbidden && row[flowspec.SecondPairedCoreNode] != flowspec.Unused

	owners, partner := rowsFor(positions, picks.Pairs, picks.Back, paired, satellite, &cand.Notes)
	if len(owners) == 0 {
		return cand, false
	}

	roles, asked := rolesFor(owners, prior, picks.Produce)
	if asked {
		cand.Notes = append(cand.Notes, "Role: no claim on this process says which positions make parts — mark them before naming")
	}

	lanes := enabledOfRole(routing, RoutingRoleStaging)
	sort.Strings(lanes)
	source := lowestOfRole(routing, RoutingRoleSource)
	dest := lowestOfRole(routing, RoutingRoleDestination)
	next, shared := 0, 0
	nextLane := func() string {
		if len(lanes) == 0 {
			return ""
		}
		if next >= len(lanes) {
			shared++
		}
		l := lanes[next%len(lanes)]
		next++
		return l
	}

	for _, n := range owners {
		role := roles[n]
		need := steadyFor(role, mode)
		c := FlowCell{CoreNodeName: n, Role: role, SwapMode: mode}
		fill := func(f flowspec.Field, set func(string), picked string, propose func() string) {
			switch need[f] {
			case flowspec.Required:
				if picked == "" && propose != nil {
					picked = propose()
				}
				set(picked)
			case flowspec.Used:
				set(picked)
			}
		}
		fill(flowspec.PairedCoreNode, func(v string) { c.PairedCoreNode = v }, partner[n], nil)
		st := picks.Staging[n]
		fill(flowspec.InboundStaging, func(v string) { c.InboundStaging = v }, st[0], nextLane)
		fill(flowspec.OutboundStaging, func(v string) { c.OutboundStaging = v }, st[1], nextLane)
		// Source and destination take the routing set's lowest row of their
		// role whenever the mode reads them at all — Used included. That is
		// the one default the routing order is allowed to decide.
		if need[flowspec.InboundSource] == flowspec.Required || need[flowspec.InboundSource] == flowspec.Used {
			c.InboundSource = source
		}
		if need[flowspec.OutboundDestination] == flowspec.Required || need[flowspec.OutboundDestination] == flowspec.Used {
			c.OutboundDestination = dest
		}
		for _, f := range flowspec.RequiredFields(need) {
			if f == flowspec.PayloadCode {
				continue // a recipe never carries a part; the mint fills it
			}
			if cellField(c, f) == "" {
				return cand, false
			}
		}
		cand.Cells = append(cand.Cells, c)
	}
	if shared > 0 {
		cand.Notes = append(cand.Notes, fmt.Sprintf("pairing shares %d staging lane%s", shared, plural(shared)))
	}
	return cand, true
}

// rowsFor decides which positions own a cell and who each one's partner is.
func rowsFor(positions []string, picks map[string]string, back map[string]bool, paired, satellite bool, notes *[]string) ([]string, map[string]string) {
	partner := map[string]string{}
	if !paired {
		return append([]string(nil), positions...), partner
	}
	pairs := picks
	if len(pairs) == 0 && satellite {
		pairs = satellitePairs(positions, back)
	}
	if len(pairs) == 0 {
		// The proposal: neighbours in picture order, first owns the row. The
		// engineer confirms or re-pairs in the positions table.
		pairs = map[string]string{}
		for i := 0; i+1 < len(positions); i += 2 {
			pairs[positions[i]] = positions[i+1]
		}
	}
	var owners []string
	inPair := map[string]bool{}
	for _, n := range positions {
		p, ok := pairs[n]
		if !ok || p == "" {
			continue
		}
		owners = append(owners, n)
		partner[n] = p
		inPair[n], inPair[p] = true, true
		if !satellite {
			if _, back := pairs[p]; !back {
				owners = append(owners, p)
				partner[p] = n
			}
		}
	}
	// Owners in picture order, whichever side the pick was written from.
	at := map[string]int{}
	for i, n := range positions {
		at[n] = i
	}
	sort.SliceStable(owners, func(i, j int) bool { return at[owners[i]] < at[owners[j]] })
	for _, n := range positions {
		if !inPair[n] {
			*notes = append(*notes, n+" has no partner and is left out")
		}
	}
	return owners, partner
}

// satellitePairs proposes a satellite mode's pairs from the known back
// positions: each front position, in picture order, takes the nearest back
// position nobody has taken. A back position never owns a row — that is what
// put an on-deck spot in front of a press. Nil when the cell has no known
// back position or no front one, and the caller pairs neighbours.
func satellitePairs(positions []string, back map[string]bool) map[string]string {
	var fronts, backs []int
	for i, n := range positions {
		if back[n] {
			backs = append(backs, i)
		} else {
			fronts = append(fronts, i)
		}
	}
	if len(fronts) == 0 || len(backs) == 0 {
		return nil
	}
	taken := map[int]bool{}
	out := map[string]string{}
	for _, f := range fronts {
		best := -1
		for _, b := range backs {
			if taken[b] {
				continue
			}
			if best < 0 || abs(b-f) < abs(best-f) {
				best = b
			}
		}
		if best < 0 {
			break
		}
		taken[best] = true
		out[positions[f]] = positions[best]
	}
	return out
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// rolesFor is each owner's role: the engineer's pick, else the role the
// process's claims give that position (its own claims first, then the
// process's majority), else consume — and `asked` says the last case happened.
func rolesFor(owners []string, prior []NodeClaim, produce map[string]bool) (map[string]protocol.ClaimRole, bool) {
	out := map[string]protocol.ClaimRole{}
	if produce != nil {
		for _, n := range owners {
			out[n] = protocol.ClaimRoleConsume
			if produce[n] {
				out[n] = protocol.ClaimRoleProduce
			}
		}
		return out, false
	}
	majority := func(match func(NodeClaim) bool) protocol.ClaimRole {
		tally := map[protocol.ClaimRole]int{}
		for _, c := range prior {
			if match(c) && c.Role != "" {
				tally[c.Role]++
			}
		}
		var best protocol.ClaimRole
		for r, n := range tally {
			if n > tally[best] || (n == tally[best] && r < best) {
				best = r
			}
		}
		return best
	}
	whole := majority(func(NodeClaim) bool { return true })
	for _, n := range owners {
		node := n
		if r := majority(func(c NodeClaim) bool { return c.CoreNodeName == node }); r != "" {
			out[n] = r
		} else if whole != "" {
			out[n] = whole
		} else {
			out[n] = protocol.ClaimRoleConsume
		}
	}
	return out, whole == ""
}

func enabledOfRole(routing []RoutingNode, role string) []string {
	var out []string
	for _, r := range routing {
		if r.Role == role && r.Enabled {
			out = append(out, r.CoreNodeName)
		}
	}
	return out
}

// lowestOfRole is the default source or destination: the enabled row of that
// role with the lowest sequence, ties by name.
func lowestOfRole(routing []RoutingNode, role string) string {
	best := -1
	for i, r := range routing {
		if r.Role != role || !r.Enabled {
			continue
		}
		if best < 0 || r.Sequence < routing[best].Sequence ||
			(r.Sequence == routing[best].Sequence && r.CoreNodeName < routing[best].CoreNodeName) {
			best = i
		}
	}
	if best < 0 {
		return ""
	}
	return routing[best].CoreNodeName
}

// cellField reads one shape field off a cell by its flowspec name, for the
// Required check. A field the cell has no column for reads blank, which is
// exactly what makes a Required field nobody fills refuse the mode.
func cellField(c FlowCell, f flowspec.Field) string {
	for _, sf := range shapeFields {
		if sf.field == f {
			return sf.of(c)
		}
	}
	return ""
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
