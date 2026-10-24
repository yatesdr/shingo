package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"shingo/protocol"
	"shingoedge/domain/flowspec"
)

// recipe_groups.go — a recipe's groups: for each part, the spots it may run on
// and the places that may serve it on each (recipe groups, 2026-10-24).
//
// THE OWNER'S WORDS (SYNTH-round1 §11): an engineer sets groups on a recipe —
// Left Hand and Right Hand on a two-off, the press spots each part may run on,
// the on-deck spots allowed per press spot, Advanced per group — and the
// floor's Spots step offers exactly that and nothing else. "Never
// auto-generate them": a recipe with no groups is spots v0's, lending and all
// (SYNTH-round2 row 5), until an engineer presses Set up groups and saves.
//
// BESIDE THE CELLS, NEVER INSTEAD OF THEM (row 1). flow_json keeps `cells`
// exactly as it was — the ★ defaults the mint lays out, the pre-fill's lay-on,
// drift — and gains `groups`, one per slot. ParsePresetShape reads the cells
// alone and refuses a recipe that has none, so a binary rolled back past this
// file runs a grouped recipe as its cells and takes none off the floor
// (TestFlowMint_ARecipeCarryingGroupsMintsItsCells).
//
// TWO COPIES, SO THE SAVE KEEPS THEM ONE (row 2). A group's first spot is its
// slot's ★ cell's head and its lists hold that cell's own attached nodes; a
// node serves one group only, which is the owner's "an on-deck spot never
// feeds the other side" stored as a fact instead of inferred.
// ValidateRecipeGroups refuses every way the two could disagree, by name.
//
// A GROUP'S ROUTING IS ITS ★ CELL'S (row 3). A group is one slot is one cell,
// so the cell stores the routing and the mint reads it where it always has.

// GroupSpot is one spot a group's part may run on, and the places that may
// serve it there. The list keys are the claim's own field names (owner ruling
// F3), each a LIST: a press spot may take any of its on-deck spots. A
// sequential slot's spot is its pair, {core_node_name: A, paired_core_node:
// [B]}: the floor picks a pair, never half of one.
type GroupSpot struct {
	CoreNodeName         string   `json:"core_node_name"`
	PairedCoreNode       []string `json:"paired_core_node,omitempty"`
	SecondPairedCoreNode []string `json:"second_paired_core_node,omitempty"`
	InboundStaging       []string `json:"inbound_staging,omitempty"`
	OutboundStaging      []string `json:"outbound_staging,omitempty"`
}

// RecipeGroup is one slot's group: the ★ cell it is of, the name the floor
// asks for its part by ("the Left Hand part?"), its spots with the ★ cell's own
// first, and its Advanced.
type RecipeGroup struct {
	Of       string          `json:"of"`
	Name     string          `json:"name"`
	Spots    []GroupSpot     `json:"spots"`
	Advanced *RecipeAdvanced `json:"advanced,omitempty"`
}

// RecipeAdvanced is a group's Advanced: FlowAdvanced less what a recipe may
// not say (SYNTH-round2 row 4). The part fields go, because a recipe carries
// no part (ValidateRecipeGroups refuses them off the raw JSON, since this type
// cannot hold them); so do the press hardware (index_robot_supplies is the
// cell's, the same for every part), the loader board's order, and the reorder
// point's source, which the mint writes. The keys are FlowAdvanced's, so the
// desktop's Advanced sheet reads and writes one shape in either scope.
//
// EVERY FIELD IS OMITTED AT ITS ZERO, the carry-over disposition included:
// zero is the minted default for each of them (blank reads as replace), and a
// group that says nothing about a field says nothing on the wire.
type RecipeAdvanced struct {
	ReorderPoint          int                  `json:"reorder_point,omitempty"`
	AutoReorder           bool                 `json:"auto_reorder,omitempty"`
	LinesideSoftThreshold int                  `json:"lineside_soft_threshold,omitempty"`
	AutoPush              bool                 `json:"auto_push,omitempty"`
	EvacuateOnChangeover  bool                 `json:"evacuate_on_changeover,omitempty"`
	CarryoverDisposition  CarryoverDisposition `json:"changeover_carryover_disposition,omitempty"`
	KeepStagedNode        string               `json:"keep_staged_node,omitempty"`
	AutoConfirm           bool                 `json:"auto_confirm,omitempty"`
	CurtainRequired       bool                 `json:"curtain_required,omitempty"`
	StrictPack            bool                 `json:"strict_pack,omitempty"`
}

// recipePartKeys are the Advanced keys that would put a part on a recipe.
var recipePartKeys = []flowspec.Field{flowspec.AllowedPayloadCodes, flowspec.AutoRequestPayload}

// groupFields are the four lists a group spot carries, each beside the ★ cell
// field it widens.
var groupFields = []struct {
	field flowspec.Field
	cell  func(FlowCell) string
	list  func(GroupSpot) []string
}{
	{flowspec.PairedCoreNode, func(c FlowCell) string { return c.PairedCoreNode }, func(s GroupSpot) []string { return s.PairedCoreNode }},
	{flowspec.SecondPairedCoreNode, func(c FlowCell) string { return c.SecondPairedCoreNode }, func(s GroupSpot) []string { return s.SecondPairedCoreNode }},
	{flowspec.InboundStaging, func(c FlowCell) string { return c.InboundStaging }, func(s GroupSpot) []string { return s.InboundStaging }},
	{flowspec.OutboundStaging, func(c FlowCell) string { return c.OutboundStaging }, func(s GroupSpot) []string { return s.OutboundStaging }},
}

// groupPlaceNoun is what a place in one of a group's lists is, as the
// refusals say it: v0's job nouns, and "a partner" for the B side of a
// sequential pair, which is a spot of its own row and the A side's partner.
func groupPlaceNoun(f flowspec.Field, mode protocol.SwapMode) string {
	switch f {
	case flowspec.PairedCoreNode:
		if mode == protocol.SwapModeSequential {
			return "a partner"
		}
		return jobNoun(SpotJobOnDeck)
	case flowspec.SecondPairedCoreNode:
		return jobNoun(SpotJobThird)
	}
	return jobNoun(SpotJobLane)
}

// bareNoun is a noun without its article: "on-deck spot".
func bareNoun(n string) string { return strings.TrimPrefix(strings.TrimPrefix(n, "an "), "a ") }

// ParseRecipe reads a recipe's flow_json: its cells, exactly as
// ParsePresetShape reads them (the same envelope, refused without a cell), and
// its groups, nil when it has none. ParsePresetShape stays the cells-only
// reader for everything that wants only the shape: the lenders, drift, the
// candidates and the imports.
func ParseRecipe(flowJSON string) ([]FlowCell, []RecipeGroup, error) {
	var env struct {
		Cells  []FlowCell    `json:"cells"`
		Groups []RecipeGroup `json:"groups"`
	}
	if err := json.Unmarshal([]byte(flowJSON), &env); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrFlowPresetMalformed, err)
	}
	if len(env.Cells) == 0 {
		return nil, nil, ErrFlowPresetMalformed
	}
	return env.Cells, env.Groups, nil
}

// RecipeShapeJSON is PresetShapeJSON with the recipe's groups beside its
// cells, WRITTEN AS GIVEN: the cells lose their parts and their Advanced as
// they always have, and the groups are not touched — a group's Advanced is the
// one Advanced a recipe keeps, because the mint lays it on (SYNTH-round2 row 4:
// a draft's Advanced was dropped silently, and must not be here). Absent, null
// or an empty list writes no key, so an ungrouped recipe is stored byte for
// byte as before.
func RecipeShapeJSON(cells []FlowCell, groups json.RawMessage) (string, error) {
	env := struct {
		Cells  []FlowCell      `json:"cells"`
		Groups json.RawMessage `json:"groups,omitempty"`
	}{Cells: StripFlowPayload(cells)}
	if g := strings.TrimSpace(string(groups)); g != "" && g != "null" && g != "[]" {
		env.Groups = groups
	}
	blob, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("marshal preset shape: %w", err)
	}
	return string(blob), nil
}

// StaleGroupNodes is StaleNodes over a recipe's groups: every spot, every node
// of every list and each group's keep-staged node that live does not hold, in
// group, spot, then field order. Group names the group; Cell is 0.
func StaleGroupNodes(groups []RecipeGroup, live map[string]bool) []StaleNode {
	var out []StaleNode
	note := func(g RecipeGroup, field, n string) {
		if name := strings.TrimSpace(n); name != "" && !live[name] {
			out = append(out, StaleNode{Group: g.Name, Field: field, Node: name})
		}
	}
	for _, g := range groups {
		for _, s := range g.Spots {
			note(g, "core_node_name", s.CoreNodeName)
			for _, f := range groupFields {
				for _, n := range f.list(s) {
					note(g, string(f.field), n)
				}
			}
		}
		if g.Advanced != nil {
			note(g, string(flowspec.KeepStagedNode), g.Advanced.KeepStagedNode)
		}
	}
	return out
}

// SlotGroups is each slot's group, in slot order: the group that is of the
// slot's first node, nil for a slot none is of.
func SlotGroups(slots []Slot, groups []RecipeGroup) []*RecipeGroup {
	out := make([]*RecipeGroup, len(slots))
	for i, s := range slots {
		for j := range groups {
			if len(s.Nodes) > 0 && groups[j].Of == s.Nodes[0] {
				out[i] = &groups[j]
				break
			}
		}
	}
	return out
}

// LayOver writes a group's Advanced onto a minted cell's, whose defaults are
// already in a: every field the group carries, the carry-over disposition only
// when it names one, and the reorder point's source "manual" when the group
// sets a point (an engineer set it; the replenishment page and the sheet's pill
// read the source). A point of zero leaves the minted source standing.
func (g RecipeAdvanced) LayOver(a *FlowAdvanced) {
	a.ReorderPoint, a.AutoReorder = g.ReorderPoint, g.AutoReorder
	a.LinesideSoftThreshold, a.AutoPush = g.LinesideSoftThreshold, g.AutoPush
	a.EvacuateOnChangeover, a.KeepStagedNode = g.EvacuateOnChangeover, g.KeepStagedNode
	a.AutoConfirm, a.CurtainRequired, a.StrictPack = g.AutoConfirm, g.CurtainRequired, g.StrictPack
	if g.CarryoverDisposition != "" {
		a.CarryoverDisposition = g.CarryoverDisposition
	}
	if g.ReorderPoint > 0 {
		a.ReorderPointSource = ReorderPointSourceManual
	}
}

// ReorderPointSourceManual is the reorder point's source when a person set it.
const ReorderPointSourceManual = "manual"

// groupRefusal is a save refused for its groups, in the engineer's words.
type groupRefusal string

func (r groupRefusal) Error() string { return string(r) }

func refusef(format string, args ...any) error { return groupRefusal(fmt.Sprintf(format, args...)) }

// groupCheck is ValidateRecipeGroups' working: the recipe, its groups, and
// each slot's ★ cell by its head.
type groupCheck struct {
	cells  []FlowCell
	groups []RecipeGroup
	slots  []Slot
	star   map[string]FlowCell
}

// ValidateRecipeGroups is the save's check of a recipe's groups (SYNTH-round2
// row 2; D8 of DECISIONS-recipe-groups), refusing by name: a group per slot
// and none over; each named, once; each of a slot's ★ head, with that head its
// first spot; the ★ cell's own attached nodes in that spot's lists; a node in
// one group only, with one job in it; no list in a field the mode forbids or
// the ★ cell leaves blank; no part in Advanced (read off the raw JSON, which
// RecipeAdvanced cannot hold) and no Advanced field the mode forbids; every
// node in allowed; and no spot without a
// place for a field the ★ cell fills, which would be a target the floor is
// offered and can never take. Pure; a recipe with no groups passes.
func ValidateRecipeGroups(flowJSON string, allowed map[string]bool) error {
	cells, groups, err := ParseRecipe(flowJSON)
	if err != nil || len(groups) == 0 {
		return err
	}
	c := &groupCheck{cells: cells, groups: groups, slots: PayloadSlots(cells), star: map[string]FlowCell{}}
	for _, cell := range cells {
		if _, ok := c.star[cell.CoreNodeName]; !ok {
			c.star[cell.CoreNodeName] = cell
		}
	}
	for _, check := range []func() error{
		c.onePerSlot, c.named, c.ofTheirSlots, c.holdTheStarCell, c.oneGroupEach,
		c.oneJobEach, c.fieldsTheModeTakes, c.partsFrom(flowJSON), c.advancedTheModeTakes,
	} {
		if err := check(); err != nil {
			return err
		}
	}
	if stale := StaleGroupNodes(groups, allowed); len(stale) > 0 {
		s := stale[0]
		return fmt.Errorf("%s: %s %q %w", s.Group, s.Field, s.Node, ErrFlowPresetUnknownNode)
	}
	return c.noEmptyPlace()
}

func (c *groupCheck) onePerSlot() error {
	if len(c.groups) != len(c.slots) {
		return refusef("this recipe takes %d part%s and has %d group%s — set up one group per part",
			len(c.slots), plural(len(c.slots)), len(c.groups), plural(len(c.groups)))
	}
	return nil
}

func (c *groupCheck) named() error {
	seen := map[string]bool{}
	for _, g := range c.groups {
		name := strings.TrimSpace(g.Name)
		switch {
		case name == "":
			return refusef("the group of %s has no name — the floor asks for its part by it", g.Of)
		case seen[name]:
			return refusef("two groups are called %s — the floor asks for each part by its group's name", name)
		}
		seen[name] = true
	}
	return nil
}

func (c *groupCheck) ofTheirSlots() error {
	heads := map[string]bool{}
	for _, s := range c.slots {
		heads[s.Nodes[0]] = true
	}
	by := map[string]string{}
	for _, g := range c.groups {
		switch {
		case !heads[g.Of]:
			return refusef("%s is of %s, which is not where a part of this recipe runs — a group is of a part's own spot", g.Name, g.Of)
		case by[g.Of] != "":
			return refusef("%s and %s are both of %s — set up one group per part", by[g.Of], g.Name, g.Of)
		case len(g.Spots) == 0:
			return refusef("%s has no spot; its first must be %s, the spot the recipe runs it on", g.Name, g.Of)
		case g.Spots[0].CoreNodeName != g.Of:
			return refusef("%s's first spot is %s; it must be %s, the spot the recipe runs it on", g.Name, g.Spots[0].CoreNodeName, g.Of)
		}
		by[g.Of] = g.Name
		for _, s := range g.Spots {
			if strings.TrimSpace(s.CoreNodeName) == "" {
				return refusef("%s has a spot with no node", g.Name)
			}
		}
	}
	return nil
}

func (c *groupCheck) holdTheStarCell() error {
	for _, g := range c.groups {
		star := c.star[g.Of]
		for _, f := range groupFields {
			n := f.cell(star)
			if n == "" || slices.Contains(f.list(g.Spots[0]), n) {
				continue
			}
			noun := groupPlaceNoun(f.field, star.SwapMode)
			return refusef("%s is %s of %s in the recipe and missing from %s's %ss for %s",
				n, noun, g.Of, g.Name, bareNoun(noun), g.Of)
		}
	}
	return nil
}

func (c *groupCheck) oneGroupEach() error {
	owner := map[string]string{}
	for _, g := range c.groups {
		for _, n := range groupNodes(g) {
			if o := owner[n]; o != "" && o != g.Name {
				return refusef("%s is in both %s and %s — a node serves one group only", n, o, g.Name)
			}
			owner[n] = g.Name
		}
	}
	return nil
}

func (c *groupCheck) oneJobEach() error {
	for _, g := range c.groups {
		spots := map[string]bool{}
		for _, s := range g.Spots {
			spots[s.CoreNodeName] = true
		}
		mode := c.star[g.Of].SwapMode
		for _, s := range g.Spots {
			for _, f := range groupFields {
				for _, n := range f.list(s) {
					if spots[n] {
						return refusef("%s is a spot of %s and one of its %ss too — a node has one job in a group",
							n, g.Name, bareNoun(groupPlaceNoun(f.field, mode)))
					}
				}
			}
		}
	}
	return nil
}

// fieldsTheModeTakes refuses a list in a field the ★ cell's mode forbids
// (flowspec.Steady), and a list in a field the ★ cell leaves blank: the moves
// rename the recipe's own places and add none, so such a list would be a fact
// nothing reads.
func (c *groupCheck) fieldsTheModeTakes() error {
	for _, g := range c.groups {
		star := c.star[g.Of]
		row := flowspec.Steady(star.Role, star.SwapMode)
		for _, s := range g.Spots {
			for _, f := range groupFields {
				list := f.list(s)
				switch {
				case len(list) == 0:
				case row[f.field] == flowspec.Forbidden:
					word := SwapModeWord(star.SwapMode)
					if word == "" {
						word = string(star.SwapMode)
					}
					return refusef("%s lists %s as %s for %s, which a %s does not use",
						g.Name, list[0], strings.ToLower(flowspec.Label(f.field)), s.CoreNodeName, word)
				case f.cell(star) == "":
					return refusef("%s lists %s for %s, and %s has none in the recipe — set one on %s's row first, or take it out",
						g.Name, groupPlaceNoun(f.field, star.SwapMode), s.CoreNodeName, g.Of, g.Of)
				}
			}
		}
	}
	return nil
}

// partsFrom refuses a part in a group's Advanced, read off the raw JSON. An
// empty list is no part: the sheet may send its cleared list.
func (c *groupCheck) partsFrom(flowJSON string) func() error {
	return func() error {
		var raw struct {
			Groups []struct {
				Name     string                     `json:"name"`
				Advanced map[string]json.RawMessage `json:"advanced"`
			} `json:"groups"`
		}
		if err := json.Unmarshal([]byte(flowJSON), &raw); err != nil {
			return fmt.Errorf("%w: %v", ErrFlowPresetMalformed, err)
		}
		for _, g := range raw.Groups {
			for _, key := range recipePartKeys {
				v := g.Advanced[string(key)]
				if rawPresent(v) && strings.TrimSpace(string(v)) != "[]" {
					return fmt.Errorf("%s's Advanced sets %s: %w", g.Name, strings.ToLower(flowspec.Label(key)), ErrFlowPresetHasPayload)
				}
			}
		}
		return nil
	}
}

// advancedFields are a group's Advanced fields by their flowspec word, each
// read as whether the group sets it, the way ClaimInputHas reads a claim's.
var advancedFields = []struct {
	field flowspec.Field
	set   func(RecipeAdvanced) bool
}{
	{flowspec.ReorderPoint, func(a RecipeAdvanced) bool { return a.ReorderPoint != 0 }},
	{flowspec.AutoReorder, func(a RecipeAdvanced) bool { return a.AutoReorder }},
	{flowspec.LinesideSoftThreshold, func(a RecipeAdvanced) bool { return a.LinesideSoftThreshold != 0 }},
	{flowspec.AutoPush, func(a RecipeAdvanced) bool { return a.AutoPush }},
	{flowspec.EvacuateOnChangeover, func(a RecipeAdvanced) bool { return a.EvacuateOnChangeover }},
	{flowspec.ChangeoverCarryoverDisposition, func(a RecipeAdvanced) bool {
		return a.CarryoverDisposition != "" && a.CarryoverDisposition != CarryoverReplace
	}},
	{flowspec.KeepStagedNode, func(a RecipeAdvanced) bool { return a.KeepStagedNode != "" }},
	{flowspec.AutoConfirm, func(a RecipeAdvanced) bool { return a.AutoConfirm }},
	{flowspec.CurtainRequired, func(a RecipeAdvanced) bool { return a.CurtainRequired }},
	{flowspec.StrictPack, func(a RecipeAdvanced) bool { return a.StrictPack }},
}

// advancedTheModeTakes refuses a group's Advanced setting a field its ★ cell's
// mode and role forbid (flowspec.Steady): the mint lays the group's Advanced on
// every claim it makes, and the claim validator would then refuse every style
// made from the recipe — a save that cannot be used, found at the floor's Go
// instead of at the desktop's Save (D26 of DECISIONS-recipe-groups).
func (c *groupCheck) advancedTheModeTakes() error {
	for _, g := range c.groups {
		if g.Advanced == nil {
			continue
		}
		star := c.star[g.Of]
		row := flowspec.Steady(star.Role, star.SwapMode)
		for _, f := range advancedFields {
			if f.set(*g.Advanced) && row[f.field] == flowspec.Forbidden {
				return refusef("%s's Advanced sets %s, which a %s %s spot does not use — clear it",
					g.Name, strings.ToLower(flowspec.Label(f.field)), SwapModeWord(star.SwapMode), star.Role)
			}
		}
	}
	return nil
}

// noEmptyPlace is D8: a spot whose list is empty for a field its ★ cell fills.
func (c *groupCheck) noEmptyPlace() error {
	for _, g := range c.groups {
		star := c.star[g.Of]
		for _, s := range g.Spots {
			for _, f := range groupFields {
				if f.cell(star) != "" && len(f.list(s)) == 0 {
					return refusef("%s has no %s in %s — add one, or take %s out",
						s.CoreNodeName, bareNoun(groupPlaceNoun(f.field, star.SwapMode)), g.Name, s.CoreNodeName)
				}
			}
		}
	}
	return nil
}

// groupNodes is every node a group names, spots and lists, in its order.
func groupNodes(g RecipeGroup) []string {
	var out []string
	for _, s := range g.Spots {
		out = append(out, s.CoreNodeName)
		for _, f := range groupFields {
			out = append(out, f.list(s)...)
		}
	}
	return out
}
