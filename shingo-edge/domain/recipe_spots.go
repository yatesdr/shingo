package domain

import (
	"maps"
	"slices"
	"strings"

	"shingo/protocol"
	"shingoedge/domain/flowspec"
)

// recipe_spots.go — where each place of a recipe may go on the Spots step
// (spots v0, 2026-10-22).
//
// THE OWNER'S RULE: the floor may pick pairings, but only the ones the
// engineer has set up in recipes, and an on-deck spot never feeds the other
// side. Springfield Press 6 (REPORT-agent-spr-press6-pair-nodes-2026-10-22)
// is why: its on-deck card was its own tap target, a tap on an idle front
// position made it the deck, and the server took any position or routing
// member.
//
// ONE RULE, COMPUTED HERE, READ BY EVERY SURFACE. RecipeSpots gives each place
// its job and its targets; the composer ships them on the card
// (ComposerPreset.Spots), the HMI lights them and restates nothing, the mint
// refuses anything else (RefuseSpotMoves), and the pre-fill goes through them
// before it ships (LegalSpotMoves). TestComposerCard_TargetsAreWhatTheServerAccepts
// holds the card and the server together.
//
// EVERY PLACE HAS A JOB, READ OFF THE RECIPE'S OWN CELLS, never off a
// position's Kind (O-9): a node that owns a row is a spot (both sides of a
// sequential pair are); the paired or second-paired node of a row whose
// partner owns no row is that row's on-deck spot or third position; a staging
// node is a lane. Source, destination, evac and key route are never on the
// Spots step.
//
// A PAIRING IS KNOWN when a recipe of the process names the two together in
// the same swap mode, or a live claim of a style built from THIS recipe does
// (the evidence RecipeLastUsedMoves reads), so the screen's own pre-fill is
// always legal. Press 6: (006,005) and (007,008) from "Two spot press index",
// (009,005) from "One spot press index", (010,005) and (012,008) from the
// style running it. Never (009,008), (007,005), or anything with PLN_013:
// the owner's same-side rule, with no side stored anywhere.

// The jobs a place on the Spots step can have.
const (
	SpotJobSpot   = "spot"
	SpotJobOnDeck = "on_deck"
	SpotJobThird  = "third"
	SpotJobLane   = "lane"
)

// RecipeSpot is one place of a recipe on the Spots step: its node, its job,
// the spot it serves (blank for a spot), and every node it may move to, its
// own included (putting it back is always allowed).
type RecipeSpot struct {
	Node    string   `json:"node"`
	Job     string   `json:"job"`
	Of      string   `json:"of,omitempty"`
	Targets []string `json:"targets"`
}

// SpotRules is the process's facts every recipe's spots are read against,
// gathered once per read: its enabled recipes (EnabledRecipes), its live
// positions, and its routing set's ENABLED staging lanes, the ones the floor
// is offered (R4).
type SpotRules struct {
	Enabled   [][]FlowCell
	Positions []Node
	Lanes     []string
}

// pairing is one known (spot, attached) pair: the two nodes, the swap mode
// that named them together and the field the attached node was in.
type pairing struct {
	mode           protocol.SwapMode
	field          string // "paired" or "second"
	spot, attached string
}

// spotBook is RecipeSpots' working: the recipe's places and the process facts
// a refusal has to explain itself from.
type spotBook struct {
	recipe    []FlowCell
	spots     []RecipeSpot
	at        map[string]int               // node → index into spots
	modeOf    map[string]protocol.SwapMode // node → the mode of the row it serves
	edges     []pairing                    // the recipe's own pairings
	known     map[pairing]bool             // every pairing the recipes and the used claims name
	rowNodes  map[string]bool              // nodes a recipe uses as a spot
	attached  map[string]string            // nodes a recipe uses as an attached place → that job
	named     map[string]bool              // the recipe's own five nodes (Collides' set)
	station   map[string]int64             // live position → its station (0: none)
	order     []string                     // live positions in press order
	lanes     map[string]bool
	laneOrder []string
}

// RecipeSpots is each place of the recipe with its job and its targets (D1 of
// DECISIONS-spots-v0: against the recipe's own layout, so the targets do not
// move as the operator moves things; RefuseSpotMoves' pairing check covers
// what two moves together could do).
//
//   - a spot → a free live position of its own station or of none
//     (positionChoices' station rule), never a staging lane, never a node the
//     recipes use only as an attached place, and only where every pairing it
//     is in stays known (a press with its on-deck spot, a sequential side
//     with its partner);
//   - an on-deck spot or third position → a free live position its spot is
//     known to be paired with in that field;
//   - a lane → the enabled staging lanes, and any position an enabled recipe
//     of the same mode names in that same field, never a node a recipe uses
//     as a spot.
//
// Free is Collides' set: not one of the recipe's own five nodes. used is the
// live claims of every style built from this recipe (RecipeUsedClaims).
func RecipeSpots(recipe []FlowCell, used []NodeClaim, rules SpotRules) []RecipeSpot {
	return readSpots(recipe, used, rules).spots
}

func readSpots(recipe []FlowCell, used []NodeClaim, rules SpotRules) *spotBook {
	b := &spotBook{recipe: recipe, at: map[string]int{}, modeOf: map[string]protocol.SwapMode{},
		known: map[pairing]bool{}, rowNodes: map[string]bool{}, attached: map[string]string{},
		named: map[string]bool{}, station: map[string]int64{}, lanes: map[string]bool{}}
	for _, p := range rules.Positions {
		if _, dup := b.station[p.CoreNodeName]; dup {
			continue
		}
		var st int64
		if p.OperatorStationID != nil {
			st = *p.OperatorStationID
		}
		b.station[p.CoreNodeName] = st
		b.order = append(b.order, p.CoreNodeName)
	}
	for _, n := range rules.Lanes {
		if !b.lanes[n] {
			b.lanes[n] = true
			b.laneOrder = append(b.laneOrder, n)
		}
	}
	b.edges = pairingsOf(recipe)
	for _, cells := range append([][]FlowCell{recipe}, rules.Enabled...) {
		for _, e := range pairingsOf(cells) {
			b.known[e] = true
		}
		b.noteJobs(cells)
	}
	for _, c := range used {
		for _, e := range pairingsOf([]FlowCell{Collapse(c)}) {
			b.known[e] = true
		}
	}
	for _, c := range recipe {
		for _, n := range cellNodes(c) {
			if n != "" {
				b.named[n] = true
			}
		}
	}
	b.places(recipe)
	for i := range b.spots {
		b.spots[i].Targets = b.targets(b.spots[i], rules.Enabled)
	}
	return b
}

// pairingsOf is the pairings a set of cells names.
func pairingsOf(cells []FlowCell) []pairing {
	var out []pairing
	for _, c := range cells {
		if c.PairedCoreNode != "" {
			out = append(out, pairing{c.SwapMode, "paired", c.CoreNodeName, c.PairedCoreNode})
		}
		if c.SecondPairedCoreNode != "" {
			out = append(out, pairing{c.SwapMode, "second", c.CoreNodeName, c.SecondPairedCoreNode})
		}
	}
	return out
}

// noteJobs records which nodes one recipe uses as a spot and which as an
// attached place, so "used only as an attached place" can be asked across
// every recipe at once.
func (b *spotBook) noteJobs(cells []FlowCell) {
	rows := map[string]bool{}
	for _, c := range cells {
		rows[c.CoreNodeName] = true
		b.rowNodes[c.CoreNodeName] = true
	}
	note := func(n, job string) {
		if n != "" && !rows[n] && b.attached[n] == "" {
			b.attached[n] = job
		}
	}
	for _, c := range cells {
		note(c.PairedCoreNode, SpotJobOnDeck)
	}
	for _, c := range cells {
		note(c.SecondPairedCoreNode, SpotJobThird)
	}
	for _, c := range cells {
		note(c.InboundStaging, SpotJobLane)
		note(c.OutboundStaging, SpotJobLane)
	}
}

// places is the recipe's places in job order: the spots in cell order, then
// the on-deck spots, the third positions and the lanes. A node with two jobs
// keeps the first (D8): a move renames every field that names it.
func (b *spotBook) places(recipe []FlowCell) {
	rows := map[string]bool{}
	for _, c := range recipe {
		rows[c.CoreNodeName] = true
	}
	add := func(n, job, of string, mode protocol.SwapMode) {
		if n == "" {
			return
		}
		if _, dup := b.at[n]; dup {
			return
		}
		b.at[n] = len(b.spots)
		b.modeOf[n] = mode
		b.spots = append(b.spots, RecipeSpot{Node: n, Job: job, Of: of})
	}
	for _, c := range recipe {
		add(c.CoreNodeName, SpotJobSpot, "", c.SwapMode)
	}
	for _, c := range recipe {
		if !rows[c.PairedCoreNode] {
			add(c.PairedCoreNode, SpotJobOnDeck, c.CoreNodeName, c.SwapMode)
		}
	}
	for _, c := range recipe {
		if !rows[c.SecondPairedCoreNode] {
			add(c.SecondPairedCoreNode, SpotJobThird, c.CoreNodeName, c.SwapMode)
		}
	}
	for _, c := range recipe {
		add(c.InboundStaging, SpotJobLane, c.CoreNodeName, c.SwapMode)
		add(c.OutboundStaging, SpotJobLane, c.CoreNodeName, c.SwapMode)
	}
}

// attachedOnly reports whether the recipes use n as an attached place and
// never as a spot.
func (b *spotBook) attachedOnly(n string) bool { return b.attached[n] != "" && !b.rowNodes[n] }

func (b *spotBook) isPosition(n string) bool { _, ok := b.station[n]; return ok }

// sameStation is positionChoices' station rule: to is the moving node's own
// station's, or no station's.
func (b *spotBook) sameStation(node, to string) bool {
	st := b.station[to]
	return st == 0 || st == b.station[node]
}

// keepsPairings reports whether node may stand at to with every pairing it is
// in still known, its partners where the recipe has them.
func (b *spotBook) keepsPairings(node, to string) bool {
	return b.brokenPairing(node, to) == nil
}

// brokenPairing is the first of node's pairings that would not be known with
// node at to, or nil.
func (b *spotBook) brokenPairing(node, to string) *pairing {
	for i, e := range b.edges {
		switch {
		case e.spot == node && !b.known[pairing{e.mode, e.field, to, e.attached}]:
			return &b.edges[i]
		case e.attached == node && !b.known[pairing{e.mode, e.field, e.spot, to}]:
			return &b.edges[i]
		}
	}
	return nil
}

// targets is where one place may go: its own node, and every node of its job's
// universe that is free and keeps its pairings.
func (b *spotBook) targets(s RecipeSpot, enabled [][]FlowCell) []string {
	out := []string{}
	own := false
	for _, n := range b.universe(s, enabled) {
		switch {
		case n == s.Node:
			own = true
			out = append(out, n)
		case b.named[n]:
		case b.keepsPairings(s.Node, n):
			out = append(out, n)
		}
	}
	if !own {
		out = append([]string{s.Node}, out...)
	}
	return out
}

// universe is the nodes a place's job may ever stand on, before freeness and
// pairings.
func (b *spotBook) universe(s RecipeSpot, enabled [][]FlowCell) []string {
	var out []string
	switch s.Job {
	case SpotJobSpot:
		for _, n := range b.order {
			if b.sameStation(s.Node, n) && !b.lanes[n] && !b.attachedOnly(n) {
				out = append(out, n)
			}
		}
	case SpotJobOnDeck, SpotJobThird:
		out = b.order
	case SpotJobLane:
		field := b.laneField(s)
		mode := b.modeOf[s.Node]
		named := map[string]bool{}
		for _, cells := range enabled {
			for _, c := range cells {
				if c.SwapMode != mode {
					continue
				}
				if n := field(c); n != "" {
					named[n] = true
				}
			}
		}
		for _, n := range b.laneOrder {
			if !b.rowNodes[n] {
				out = append(out, n)
			}
		}
		for _, n := range b.order {
			if named[n] && !b.lanes[n] && !b.rowNodes[n] {
				out = append(out, n)
			}
		}
	}
	return out
}

// laneField is which staging field a lane place fills on the row it serves.
func (b *spotBook) laneField(s RecipeSpot) func(FlowCell) string {
	for _, c := range b.recipe {
		if c.CoreNodeName == s.Of && c.InboundStaging == s.Node {
			return func(c FlowCell) string { return c.InboundStaging }
		}
	}
	return func(c FlowCell) string { return c.OutboundStaging }
}

// RefuseSpotMoves is the server's check of the Spots step's moves (recipe node
// → the node it should use instead): every move goes to one of its place's
// targets, no two places land on one node (Collides), and no press spot and
// its attached place both moved onto a pairing no recipe set up (D2). The
// refusal is a sentence in the place's own words that names the next act;
// nil when the moves stand.
func RefuseSpotMoves(recipe []FlowCell, used []NodeClaim, rules SpotRules, moves map[string]string) error {
	b := readSpots(recipe, used, rules)
	clean := map[string]string{}
	for _, from := range slices.Sorted(maps.Keys(moves)) {
		to := strings.TrimSpace(moves[from])
		if _, ok := b.at[from]; !ok {
			return spotRefusal(from + " is not in this recipe")
		}
		if to == "" {
			return spotRefusal(from + " has no node to move to")
		}
		clean[from] = to
	}
	at := func(n string) string {
		if to, ok := clean[n]; ok {
			return to
		}
		return n
	}
	for _, s := range b.spots {
		to, ok := clean[s.Node]
		if !ok || to == s.Node || slices.Contains(s.Targets, to) {
			continue
		}
		return spotRefusal(b.whyNot(s, to, at))
	}
	if n := collision(recipe, clean); n != "" {
		return spotRefusal(n + " is already in this recipe")
	}
	if why := b.pairRefusal(at); why != "" {
		return spotRefusal(why)
	}
	return nil
}

// LegalSpotMoves is the pre-fill as the screen may open on it: the moves whose
// targets allow them, and none at all when what is left would collide or pair
// two places no recipe pairs. A suggestion the server would refuse is worse
// than the recipe's own nodes.
func LegalSpotMoves(recipe []FlowCell, used []NodeClaim, rules SpotRules, moves map[string]string) map[string]string {
	if len(moves) == 0 {
		return nil
	}
	b := readSpots(recipe, used, rules)
	out := map[string]string{}
	for from, to := range moves {
		if i, ok := b.at[from]; ok && to != from && slices.Contains(b.spots[i].Targets, to) {
			out[from] = to
		}
	}
	at := func(n string) string {
		if to, ok := out[n]; ok {
			return to
		}
		return n
	}
	if len(out) == 0 || Collides(recipe, out) || b.pairRefusal(at) != "" {
		return nil
	}
	return out
}

// pairRefusal is D2: a pairing of the recipe whose two ends both moved onto a
// pair no recipe or used claim names.
func (b *spotBook) pairRefusal(at func(string) string) string {
	for _, e := range b.edges {
		s, a := at(e.spot), at(e.attached)
		if (s != e.spot || a != e.attached) && !b.known[pairing{e.mode, e.field, s, a}] {
			return s + " and " + a + " are not a pairing your recipes set up — an engineer adds one on the desktop"
		}
	}
	return ""
}

// whyNot says why one place may not go to `to`, in its job's words, naming
// the next act. at gives where every place is after the moves.
func (b *spotBook) whyNot(s RecipeSpot, to string, at func(string) string) string {
	const ask = " — an engineer adds one on the desktop"
	spotWord := "a spot"
	if b.modeOf[s.Node] == protocol.SwapModeTwoRobotPressIndex {
		spotWord = "a press spot"
	}
	switch s.Job {
	case SpotJobSpot:
		switch {
		case b.lanes[to]:
			return to + " is a staging lane; " + spotWord + " cannot move onto it"
		case b.attachedOnly(to):
			return to + " is " + jobNoun(b.attached[to]) + "; " + spotWord + " cannot move onto it"
		case !b.isPosition(to):
			return to + " is not a position of this process"
		case !b.sameStation(s.Node, to):
			return to + " is another station's position; " + spotWord + " moves only to its own station's"
		}
		if e := b.brokenPairing(s.Node, to); e != nil {
			return to + " is not " + spotWord + " your recipes set up with " + partnerWords(*e, s.Node, at) + ask
		}
	case SpotJobOnDeck, SpotJobThird:
		noun := jobNoun(s.Job)
		switch {
		case b.rowNodes[to]:
			return to + " is a spot; " + noun + " cannot move onto it"
		case b.lanes[to]:
			return to + " is a staging lane; " + noun + " cannot move onto it"
		case !b.named[to] || b.brokenPairing(s.Node, to) != nil:
			return to + " is not " + noun + " your recipes set up for " + at(s.Of) + ask
		}
	case SpotJobLane:
		switch {
		case b.named[to]:
		case b.rowNodes[to]:
			return to + " is a spot; a lane moves only to another lane"
		case b.isPosition(to):
			return to + " is not a staging lane; a lane moves only to another lane"
		default:
			return to + " is not a staging lane of this process — an engineer adds one in Settings › Routing"
		}
	}
	return to + " is one of this recipe's own places; tap it to change it instead"
}

// partnerWords names the partner a spot's broken pairing is with, where it is
// after the moves: "PLN_005 on deck", "PLN_014 as its third position",
// "SEQ_B as its partner".
func partnerWords(e pairing, node string, at func(string) string) string {
	other := e.attached
	if e.attached == node {
		other = e.spot
	}
	switch {
	case e.spot == node && e.field == "paired" && e.mode == protocol.SwapModeTwoRobotPressIndex:
		return at(other) + " on deck"
	case e.spot == node && e.field == "second":
		return at(other) + " as its " + strings.ToLower(flowspec.Label(flowspec.SecondPairedCoreNode))
	}
	return at(other) + " as its partner"
}

// jobNoun is a job as the refusals say it. A third position is its field's
// word (flowspec.Label, owner ruling F3: one name for a field).
func jobNoun(job string) string {
	switch job {
	case SpotJobOnDeck:
		return "an on-deck spot"
	case SpotJobThird:
		return "a " + strings.ToLower(flowspec.Label(flowspec.SecondPairedCoreNode))
	case SpotJobLane:
		return "a staging lane"
	}
	return "a spot"
}

// spotRefusal is a refusal of the Spots step's moves; the engine answers it
// as a 422 with the sentence.
type spotRefusal string

func (r spotRefusal) Error() string { return string(r) }

// Collides reports whether the moves put two of the recipe's distinct nodes on
// one node.
func Collides(recipe []FlowCell, moves map[string]string) bool { return collision(recipe, moves) != "" }

// collision is the node two of the recipe's places land on, or "".
func collision(recipe []FlowCell, moves map[string]string) string {
	at := map[string]string{}
	for _, c := range recipe {
		for _, n := range cellNodes(c) {
			if n == "" {
				continue
			}
			to := n
			if m, ok := moves[n]; ok {
				to = m
			}
			if prev, taken := at[to]; taken && prev != n {
				return to
			}
			at[to] = n
		}
	}
	return ""
}

// EnabledRecipes reads a process's live recipe rows (ListFlowPresets without
// the archived ones, name then version DESC) into its enabled recipes, the
// newest non-archived version of each name (pick-a-part O-4), and every live
// version's id by name: a style built from v1 still used the recipe once v2
// is the card. An archived version is in neither, so it grants nothing.
func EnabledRecipes(rows []FlowPreset) (latest []FlowPreset, versions map[string]map[int64]bool) {
	versions = map[string]map[int64]bool{}
	for _, p := range rows {
		if versions[p.Name] == nil {
			versions[p.Name] = map[int64]bool{}
			latest = append(latest, p)
		}
		versions[p.Name][p.ID] = true
	}
	return latest, versions
}

// RecipeUsedClaims is every live claim of every style built from one of ids:
// a style with at least one claim whose provenance is the recipe, read whole,
// as RecipeLastUsedMoves reads its last one.
func RecipeUsedClaims(ids map[int64]bool, claims map[int64][]NodeClaim) []NodeClaim {
	var out []NodeClaim
	for _, rows := range claims {
		for _, c := range rows {
			if c.SourcePresetID != nil && ids[*c.SourcePresetID] {
				out = append(out, rows...)
				break
			}
		}
	}
	return out
}

// StagingLanes is the enabled staging-role members of a routing set: the
// lanes the floor is offered (R4).
func StagingLanes(routing []RoutingNode) []string {
	var out []string
	for _, r := range routing {
		if r.Enabled && r.Role == RoutingRoleStaging {
			out = append(out, r.CoreNodeName)
		}
	}
	return out
}
