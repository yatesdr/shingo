package engine

import (
	"fmt"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// unloader_lines.go — an unloader's group line: what the outbound group of an
// unloader wants, and what each cart standing at the unloader's windows is
// asked to go back as.
//
// THE KEEPER'S ARITHMETIC. A level type's shortfall is the keeper's own
// (countLevel: want less resident, asked and coming), read against the same
// open maintain episodes Maintainer.Tick reads. Nothing here is a fifth term.
//
// HEIGHTS, FOR A TYPE WITH LAYER WANTS. When the type states its totes per
// layer, each wanted height is a thing of its own: a cart counts toward a
// height only when its totes_on_cart is exactly that many layers of the type's
// totes per layer. A cart with a remainder, or with no number, counts toward
// its type and toward no height. Asks carry no number, so they count toward
// the type alone. A type with layer wants and no totes per layer has no
// heights: no cart can be counted at one.
//
// KEYED ON LEVELS, NOT ON TOTE NUMBERS. A group with no levels gets no line and
// costs no statement; a group with levels gets an assignment for every bin
// type, numbers or none.

// UnloaderGroupLines answers one outbound group's line. levels are the group's
// levels as ListMaintainLevels returns them (their order breaks ties);
// windows are the rows of the bins at the unloader's windows in the plant's
// order (bin_loader_homes.sort_order), unoccupied windows included or not.
//
// nil, with no statement, for a group with no levels. A failed read is an
// error and no line: never a line built on a count that was not made.
//
// COST per call with levels: the open maintain episodes (one read); per level
// type the keeper's counts (resident, coming, and asked while an episode is
// open: two or three reads); the group's layer wants (one read); per type with
// a wanted height and totes per layer, its residents by totes (one read). The
// comings at a height are read off the keeper's own coming rows.
func (e *Engine) UnloaderGroupLines(group *nodes.Node, levels []store.MaintainLevel,
	windows []protocol.NodeBinsRow) (*protocol.NodeBinsGroup, error) {
	if len(levels) == 0 {
		return nil, nil
	}
	open, err := e.db.ListOpenEpisodesOfKind(protocol.EpisodeKindMaintain)
	if err != nil {
		return nil, fmt.Errorf("group line %s: list open episodes: %w", group.Name, err)
	}
	openByKey := make(map[string]store.DemandOrigin, len(open))
	for _, o := range open {
		openByKey[o.EpisodeKey] = o
	}
	layerWants, err := e.db.ListMaintainLayerWants(group.ID)
	if err != nil {
		return nil, fmt.Errorf("group line %s: %w", group.Name, err)
	}

	lines := make([]typeLine, 0, len(levels))
	inbound := map[int64]bool{}
	for _, lv := range levels {
		c, err := countLevel(e.db, group, lv, openByKey[protocol.MaintainEpisodeKey(group.Name, lv.BinTypeCode)])
		if err != nil {
			return nil, fmt.Errorf("group line: %w", err)
		}
		for _, ib := range c.Inbound {
			inbound[ib.BinID] = true
		}
		tl := typeLine{code: lv.BinTypeCode, want: lv.Want, have: lv.Want - c.Short()}
		for _, w := range layerWants {
			if w.BinTypeID != lv.BinTypeID || w.TotesPerLayer == nil || *w.TotesPerLayer <= 0 {
				continue
			}
			tl.totesPerLayer = *w.TotesPerLayer
			tl.heights = append(tl.heights, heightLine{layers: w.Layers, want: w.Want})
		}
		if len(tl.heights) > 0 {
			if err := e.countHeights(group, &tl, c.Inbound); err != nil {
				return nil, err
			}
		}
		lines = append(lines, tl)
	}

	return &protocol.NodeBinsGroup{Group: group.Name, Wants: wantsOf(lines),
		Assignments: assignWindows(lines, windows, inbound)}, nil
}

// countHeights fills each wanted height's residents and comings: the carts
// whose totes_on_cart is exactly that many layers. The comings are the type's
// coming rows (countLevel's Inbound), so no second read spells "coming".
func (e *Engine) countHeights(group *nodes.Node, tl *typeLine, inbound []orders.InboundCarrier) error {
	resident, err := e.db.CountEmptyBinsOfTypeInGroupByTotes(tl.code, group.ID)
	if err != nil {
		return fmt.Errorf("group line: %w", err)
	}
	coming := map[int]int{}
	for _, ib := range inbound {
		if ib.TotesOnCart != nil {
			coming[*ib.TotesOnCart]++
		}
	}
	for i := range tl.heights {
		totes := tl.heights[i].layers * tl.totesPerLayer
		tl.heights[i].have = resident[totes] + coming[totes]
	}
	return nil
}

// typeLine is one level type as the walk sees it: what the group wants of it,
// what it has (resident, asked and coming: want less the keeper's Short), and
// its wanted heights when the type states its totes per layer.
type typeLine struct {
	code          string
	want, have    int
	totesPerLayer int // 0: the type states none, and has no heights
	heights       []heightLine
}

// heightLine is one wanted height of a type: how many carts standing layers
// high the group wants, and how many it has (resident and coming at exactly
// that many totes).
type heightLine struct {
	layers     int
	want, have int
}

// wantsOf is the line's wants: each level type, then its wanted heights.
func wantsOf(lines []typeLine) []protocol.NodeBinsWant {
	out := []protocol.NodeBinsWant{}
	for _, tl := range lines {
		out = append(out, protocol.NodeBinsWant{BinType: tl.code, Want: tl.want})
		for _, h := range tl.heights {
			layers := h.layers
			out = append(out, protocol.NodeBinsWant{BinType: tl.code, Layers: &layers, Want: h.want})
		}
	}
	return out
}

// thing is what a cart can be sent back as: a level type (layers 0: any
// height) or a wanted height of one.
type thing struct {
	code   string
	layers int
}

// assignWindows is the walk: what each cart at a window is asked to go back as.
//
//   - A running shortfall per thing: want less what the group has, less what
//     this walk has assigned. A height's shortfall is also held under its
//     type's, since a cart at a height is a cart of the type. A type's
//     any-height shortfall is what its wanted heights do not speak for.
//   - A window whose cart is already on its way into the group (inbound: its
//     empty-out names it, so the keeper's coming counts it) is not walked. Its
//     choice was made at its CLEAR, and walking it would count it twice. Pin:
//     TestUnloaderGroupLines_AClearedCartIsCountedOnce.
//   - Windows holding an empty are walked first (it is already what it is;
//     sending it as it stands is no work), then windows holding a full, each
//     in the order given (the plant's).
//   - A cart is assigned its own thing while the group is still short of it:
//     its height, when its type has heights and its number is an exact
//     multiple at a wanted height; otherwise its type. Otherwise the thing with
//     the largest remaining shortfall, ties by the levels' order (within a
//     type, its heights from the lowest, then the type at any height). Nothing
//     short: no assignment.
//
// The answer is in the windows' order. Pins: TestAssignWindows_*.
func assignWindows(lines []typeLine, windows []protocol.NodeBinsRow, inbound map[int64]bool) []protocol.NodeBinsAssignment {
	assignedType := map[string]int{}
	assignedHeight := map[thing]int{}

	typeShort := func(tl typeLine) int { return tl.want - tl.have - assignedType[tl.code] }
	heightShort := func(tl typeLine, h heightLine) int {
		return min(h.want-h.have-assignedHeight[thing{tl.code, h.layers}], typeShort(tl))
	}
	anyShort := func(tl typeLine) int {
		s := typeShort(tl)
		for _, h := range tl.heights {
			s -= max(0, heightShort(tl, h))
		}
		return s
	}
	lineOf := func(code string) (typeLine, bool) {
		for _, tl := range lines {
			if tl.code == code {
				return tl, true
			}
		}
		return typeLine{}, false
	}
	// own is the cart's own thing and whether the group is still short of it.
	own := func(row protocol.NodeBinsRow) (thing, bool) {
		tl, ok := lineOf(row.BinTypeCode)
		if !ok {
			return thing{code: row.BinTypeCode}, false
		}
		if tl.totesPerLayer > 0 && row.TotesOnCart != nil && *row.TotesOnCart%tl.totesPerLayer == 0 {
			layers := *row.TotesOnCart / tl.totesPerLayer
			for _, h := range tl.heights {
				if h.layers == layers {
					return thing{tl.code, layers}, heightShort(tl, h) > 0
				}
			}
		}
		// No height of its own (a type with no heights, a remainder, or no
		// number): its own thing is the type at any height, short only by
		// what the wanted heights do not speak for. Pinned by
		// TestAssignWindows_NoHeightOfItsOwnTakesTheLargestHeight.
		return thing{code: tl.code}, anyShort(tl) > 0
	}
	// largest is the thing with the largest remaining shortfall, first in the
	// levels' order on a tie.
	largest := func() (thing, bool) {
		var best thing
		bestShort := 0
		consider := func(t thing, short int) {
			if short > bestShort {
				best, bestShort = t, short
			}
		}
		for _, tl := range lines {
			for _, h := range tl.heights {
				consider(thing{tl.code, h.layers}, heightShort(tl, h))
			}
			consider(thing{code: tl.code}, anyShort(tl))
		}
		return best, bestShort > 0
	}

	byNode := map[string]protocol.NodeBinsAssignment{}
	walk := func(empty bool) {
		for _, row := range windows {
			if !row.Occupied || (row.PayloadCode == "") != empty || inbound[row.BinID] {
				continue
			}
			t, short := own(row)
			if !short {
				if t, short = largest(); !short {
					continue
				}
			}
			assignedType[t.code]++
			a := protocol.NodeBinsAssignment{Node: row.Node, BinType: t.code}
			if t.layers > 0 {
				assignedHeight[t]++
				layers := t.layers
				a.Layers = &layers
			}
			byNode[row.Node] = a
		}
	}
	walk(true)
	walk(false)
	// Answered in the windows' order, whatever order they were walked in.
	out := []protocol.NodeBinsAssignment{}
	for _, row := range windows {
		if a, ok := byNode[row.Node]; ok {
			out = append(out, a)
		}
	}
	return out
}
