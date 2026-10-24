package domain

import (
	"maps"
	"slices"

	"shingo/protocol"
	"shingoedge/domain/flowspec"
)

// recipe_spots_grouped.go — the Spots step for a recipe with groups
// (SYNTH-round2 rows 7 and 8, §2).
//
// THE GROUP IS THE WHOLE ANSWER. A spot moves only to its group's spots; an
// attached place (on deck, third, lane, a sequential pair's B side) only
// within the list its spot's stand has; nothing is lent from another recipe or
// a style, which is what "nothing else is offered" means on the mock. v0's
// rule stays exactly what it was for a recipe with no groups.
//
// AN ON-DECK SPOT FOLLOWS ITS SPOT (row 8). A tied list — the owner's example,
// "5 feeds 6 9 10 and 11 feeds 13 15 19", decks each feeding their own spots —
// is walkable only if a spot's move brings its deck along, so the card ships the rule's answer and
// the HMI looks it up: at[stand], the nodes each attached place may stand on
// when its spot stands there, and carry[target], the attached moves a spot's
// move to target brings. The Spots step's body carries those moves with the
// spot's, and the server checks the state they leave.
//
// THE SERVER CHECKS THE FINAL STATE. After the moves every spot stands on one
// of its group's spots and every attached place on a node its field's list
// holds at that stand (a lane also on an enabled routing lane, D7), nothing
// collides, and v0's job refusals still say why a spot cannot stand on a lane,
// an attached place or another station's position.

// groupSlot is one slot's group as the book reads it: its ★ head, the mode of
// its ★ row, its stands (each spot, by node, the first entry winning) in group
// order, and the ★ row's attached places.
type groupSlot struct {
	group    RecipeGroup
	head     string
	mode     protocol.SwapMode
	stands   map[string]GroupSpot
	order    []string
	attached []attachedPlace
}

func (s *groupSlot) standsOn(n string) bool { _, ok := s.stands[n]; return ok }

// attachedPlace is one of a ★ row's attached nodes and the group field
// (groupFields' index) its list widens.
type attachedPlace struct {
	node  string
	field int
}

// groupBook is the grouped rule's working.
type groupBook struct {
	spotFacts
	recipe   []FlowCell
	spots    []RecipeSpot
	slots    []groupSlot
	slotOf   map[string]int // grouped place's recipe node → index into slots
	place    map[string]int // attached place's recipe node → index into its slot's attached
	named    map[string]bool
	attached map[string]string // a node an attached place or a group list holds → the job noun
}

func readGroupSpots(recipe []FlowCell, groups []RecipeGroup, rules SpotRules) *groupBook {
	b := &groupBook{spotFacts: readSpotFacts(rules), recipe: recipe, slotOf: map[string]int{},
		place: map[string]int{}, named: recipeNamed(recipe), attached: map[string]string{}}
	star := map[string]FlowCell{}
	for _, c := range recipe {
		if _, ok := star[c.CoreNodeName]; !ok {
			star[c.CoreNodeName] = c
		}
	}
	slots := PayloadSlots(recipe)
	for i, g := range SlotGroups(slots, groups) {
		if g == nil {
			continue
		}
		cell := star[slots[i].Nodes[0]]
		s := groupSlot{group: *g, head: cell.CoreNodeName, mode: cell.SwapMode, stands: map[string]GroupSpot{}}
		for _, sp := range g.Spots {
			if _, dup := s.stands[sp.CoreNodeName]; !dup {
				s.stands[sp.CoreNodeName] = sp
				s.order = append(s.order, sp.CoreNodeName)
			}
			for _, f := range groupFields {
				for _, n := range f.list(sp) {
					b.noteAttached(n, f.field, s.mode)
				}
			}
		}
		for fi, f := range groupFields {
			// A node in two of the ★ row's fields keeps the first, as a place
			// with two jobs does (D8 of DECISIONS-spots-v0).
			n := f.cell(cell)
			if n == "" || slices.ContainsFunc(s.attached, func(a attachedPlace) bool { return a.node == n }) {
				continue
			}
			s.attached = append(s.attached, attachedPlace{node: n, field: fi})
			b.noteAttached(n, f.field, s.mode)
		}
		b.slots = append(b.slots, s)
	}
	for si, s := range b.slots {
		b.slotOf[s.head] = si
		for ai, a := range s.attached {
			if _, taken := b.slotOf[a.node]; !taken {
				b.slotOf[a.node], b.place[a.node] = si, ai
			}
		}
	}
	places, _ := recipePlaces(recipe)
	for _, p := range places {
		b.spots = append(b.spots, b.placeOf(p))
	}
	return b
}

func (b *groupBook) noteAttached(n string, f flowspec.Field, mode protocol.SwapMode) {
	if b.attached[n] == "" {
		b.attached[n] = groupPlaceNoun(f, mode)
	}
}

// placeOf is one place of the recipe on the grouped Spots step: a group's
// spot, one of its attached places, or a place no group holds, which stays
// where it is.
func (b *groupBook) placeOf(p RecipeSpot) RecipeSpot {
	si, grouped := b.slotOf[p.Node]
	if !grouped {
		p.Targets = []string{p.Node}
		return p
	}
	s := &b.slots[si]
	p.Group = s.group.Name
	if p.Node == s.head {
		p.Targets, p.Carry = b.spotTargets(s)
		return p
	}
	a := s.attached[b.place[p.Node]]
	p.Of = s.head
	p.At = map[string][]string{}
	for _, st := range s.order {
		p.At[st] = b.offered(s, a, st)
	}
	p.Targets = []string{p.Node}
	for _, n := range p.At[s.head] {
		if n != p.Node {
			p.Targets = append(p.Targets, n)
		}
	}
	return p
}

// spotTargets is where a group's spot may go, and the attached moves each
// target brings: its own node, then every stand of its group that is a live
// position of its own station or of none, not a lane, not a node another place
// of the recipe or the groups uses, and that every attached place can follow
// it to.
func (b *groupBook) spotTargets(s *groupSlot) ([]string, map[string]map[string]string) {
	targets := []string{s.head}
	var carry map[string]map[string]string
	for _, st := range s.order {
		if st == s.head || !b.isPosition(st) || !b.sameStation(s.head, st) || b.lanes[st] ||
			b.named[st] || b.attached[st] != "" {
			continue
		}
		moves, ok := b.carryTo(s, st)
		if !ok {
			continue
		}
		targets = append(targets, st)
		if len(moves) > 0 {
			if carry == nil {
				carry = map[string]map[string]string{}
			}
			carry[st] = moves
		}
	}
	return targets, carry
}

// carryTo is the attached moves a spot's move to stand brings: each attached
// place whose recipe node the stand's list does not hold goes to the first node
// the list offers it (offered: free of the recipe's other places) that no
// earlier carry took. Not ok when one has nowhere to go.
func (b *groupBook) carryTo(s *groupSlot, stand string) (map[string]string, bool) {
	moves := map[string]string{}
	taken := map[string]bool{}
	for _, a := range s.attached {
		if slices.Contains(b.allowed(s, a, stand), a.node) {
			continue
		}
		to := ""
		for _, n := range b.offered(s, a, stand) {
			if !taken[n] {
				to = n
				break
			}
		}
		if to == "" {
			return nil, false
		}
		moves[a.node], taken[to] = to, true
	}
	return moves, true
}

// allowed is every node an attached place may stand on when its spot stands
// on stand: the stand's list for its field, and for a lane the enabled routing
// lanes too (D7 of DECISIONS-recipe-groups: SYNTH row 7 keeps the conveyor
// change). The server's check reads this.
func (b *groupBook) allowed(s *groupSlot, a attachedPlace, stand string) []string {
	sp, ok := s.stands[stand]
	if !ok {
		return nil
	}
	out := slices.Clone(groupFields[a.field].list(sp))
	if f := groupFields[a.field].field; f == flowspec.InboundStaging || f == flowspec.OutboundStaging {
		for _, n := range b.laneOrder {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	return out
}

// offered is allowed less the nodes the recipe's other places use: what the
// card lights, against the recipe's own layout (D1 of DECISIONS-spots-v0).
func (b *groupBook) offered(s *groupSlot, a attachedPlace, stand string) []string {
	out := []string{}
	for _, n := range b.allowed(s, a, stand) {
		if n == a.node || !b.named[n] {
			out = append(out, n)
		}
	}
	return out
}

// refuse is RefuseSpotMoves for a grouped recipe, over clean moves.
func (b *groupBook) refuse(moves map[string]string) string {
	for _, p := range b.spots {
		if _, grouped := b.slotOf[p.Node]; !grouped {
			if to, moved := moves[p.Node]; moved && to != p.Node {
				return p.Node + " is in no group of this recipe; it stays where it is"
			}
		}
	}
	for si := range b.slots {
		if why := b.slotRefusal(&b.slots[si], moves); why != "" {
			return why
		}
	}
	if n := collision(b.recipe, moves); n != "" {
		return n + " is already in this recipe"
	}
	return ""
}

// slotRefusal is one slot's final state after the moves: its spot on one of
// its group's spots, by v0's job rules, and every attached place on a node its
// list holds at that stand. "" when it stands.
func (b *groupBook) slotRefusal(s *groupSlot, moves map[string]string) string {
	at := func(n string) string {
		if to, ok := moves[n]; ok {
			return to
		}
		return n
	}
	spotWord := "a spot"
	if s.mode == protocol.SwapModeTwoRobotPressIndex {
		spotWord = "a press spot"
	}
	stand := at(s.head)
	if stand != s.head {
		switch {
		case b.lanes[stand]:
			return stand + " is a staging lane; " + spotWord + " cannot move onto it"
		case b.attached[stand] != "":
			return stand + " is " + b.attached[stand] + "; " + spotWord + " cannot move onto it"
		case !b.isPosition(stand):
			return stand + " is not a position of this process"
		case !b.sameStation(s.head, stand):
			return stand + " is another station's position; " + spotWord + " moves only to its own station's"
		case !s.standsOn(stand):
			return stand + " is not " + spotWord + " of " + s.group.Name + " — an engineer adds it on the desktop"
		}
	}
	for _, a := range s.attached {
		if n := at(a.node); !slices.Contains(b.allowed(s, a, stand), n) {
			return n + " is not " + groupPlaceNoun(groupFields[a.field].field, s.mode) + " " + s.group.Name +
				" has for " + stand + " — an engineer adds one on the desktop"
		}
	}
	return ""
}

// legal is LegalSpotMoves for a grouped recipe: each slot's pre-fill kept only
// when that slot's final state stands and it collides with nothing kept
// before it, so one slot's stale suggestion does not cost the other its own.
func (b *groupBook) legal(moves map[string]string) map[string]string {
	out := map[string]string{}
	for si := range b.slots {
		s := &b.slots[si]
		trial := maps.Clone(out)
		mine := false
		for from, to := range moves {
			if i, ok := b.slotOf[from]; ok && i == si && to != from {
				trial[from], mine = to, true
			}
		}
		if mine && b.slotRefusal(s, trial) == "" && !Collides(b.recipe, trial) {
			out = trial
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
