package engine

import (
	"fmt"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/nodes"
)

// unloader_lines.go — an unloader's group line: what the outbound group of an
// unloader wants, and what each cart standing at the unloader's windows is
// asked to go back as.
//
// THE KEEPER'S ARITHMETIC. A level type's shortfall is the keeper's own
// (store.CountLevel: want less resident, asked and coming), read against the
// same open maintain episodes Maintainer.Tick reads. Nothing here is a fifth
// term. The line's model (store.TypeLine) lives in store beside it.
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

	lines := make([]store.TypeLine, 0, len(levels))
	inbound := map[int64]bool{}
	for _, lv := range levels {
		tl, c, err := e.db.TypeLineOf(group, lv, openByKey[protocol.MaintainEpisodeKey(group.Name, lv.BinTypeCode)], layerWants)
		if err != nil {
			return nil, fmt.Errorf("group line: %w", err)
		}
		for _, ib := range c.Inbound {
			inbound[ib.BinID] = true
		}
		lines = append(lines, tl)
	}

	return &protocol.NodeBinsGroup{Group: group.Name, Wants: wantsOf(lines),
		Assignments: assignWindows(lines, windows, inbound)}, nil
}

// wantsOf is the line's wants: each level type, then its wanted heights.
func wantsOf(lines []store.TypeLine) []protocol.NodeBinsWant {
	out := []protocol.NodeBinsWant{}
	for _, tl := range lines {
		out = append(out, protocol.NodeBinsWant{BinType: tl.Code, Want: tl.Want})
		for _, h := range tl.Heights {
			layers := h.Layers
			out = append(out, protocol.NodeBinsWant{BinType: tl.Code, Layers: &layers, Want: h.Want})
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
func assignWindows(lines []store.TypeLine, windows []protocol.NodeBinsRow, inbound map[int64]bool) []protocol.NodeBinsAssignment {
	w := &windowWalk{lines: lines, assignedType: map[string]int{}, assignedHeight: map[thing]int{}}
	byNode := map[string]protocol.NodeBinsAssignment{}
	pass := func(empty bool) {
		for _, row := range windows {
			if !row.Occupied || (row.PayloadCode == "") != empty || inbound[row.BinID] {
				continue
			}
			t, short := w.own(row)
			if !short {
				if t, short = w.largest(); !short {
					continue
				}
			}
			byNode[row.Node] = w.assign(row.Node, t)
		}
	}
	pass(true)
	pass(false)
	// Answered in the windows' order, whatever order they were walked in.
	out := []protocol.NodeBinsAssignment{}
	for _, row := range windows {
		if a, ok := byNode[row.Node]; ok {
			out = append(out, a)
		}
	}
	return out
}

// windowWalk is the walk's running state: the group's lines, and what the walk
// has assigned of each thing so far, subtracted from each line's shortfall.
type windowWalk struct {
	lines          []store.TypeLine
	assignedType   map[string]int
	assignedHeight map[thing]int
}

func (w *windowWalk) typeShort(tl store.TypeLine) int { return tl.Short() - w.assignedType[tl.Code] }

func (w *windowWalk) heightShort(tl store.TypeLine, h store.HeightLine) int {
	return min(h.Want-h.Have-w.assignedHeight[thing{tl.Code, h.Layers}], w.typeShort(tl))
}

func (w *windowWalk) anyShort(tl store.TypeLine) int {
	s := w.typeShort(tl)
	for _, h := range tl.Heights {
		s -= max(0, w.heightShort(tl, h))
	}
	return s
}

func (w *windowWalk) lineOf(code string) (store.TypeLine, bool) {
	for _, tl := range w.lines {
		if tl.Code == code {
			return tl, true
		}
	}
	return store.TypeLine{}, false
}

// own is the cart's own thing and whether the group is still short of it.
func (w *windowWalk) own(row protocol.NodeBinsRow) (thing, bool) {
	tl, ok := w.lineOf(row.BinTypeCode)
	if !ok {
		return thing{code: row.BinTypeCode}, false
	}
	if layers, ok := protocol.ToteLayers(row.TotesOnCart, &tl.TotesPerLayer); ok {
		for _, h := range tl.Heights {
			if h.Layers == layers {
				return thing{tl.Code, layers}, w.heightShort(tl, h) > 0
			}
		}
	}
	// No height of its own (a type with no heights, a remainder, or no
	// number): its own thing is the type at any height, short only by
	// what the wanted heights do not speak for. Pinned by
	// TestAssignWindows_NoHeightOfItsOwnTakesTheLargestHeight.
	return thing{code: tl.Code}, w.anyShort(tl) > 0
}

// largest is the thing with the largest remaining shortfall, first in the
// levels' order on a tie.
func (w *windowWalk) largest() (thing, bool) {
	var best thing
	bestShort := 0
	consider := func(t thing, short int) {
		if short > bestShort {
			best, bestShort = t, short
		}
	}
	for _, tl := range w.lines {
		for _, h := range tl.Heights {
			consider(thing{tl.Code, h.Layers}, w.heightShort(tl, h))
		}
		consider(thing{code: tl.Code}, w.anyShort(tl))
	}
	return best, bestShort > 0
}

// assign counts t against the walk and answers the window's assignment.
func (w *windowWalk) assign(node string, t thing) protocol.NodeBinsAssignment {
	w.assignedType[t.code]++
	a := protocol.NodeBinsAssignment{Node: node, BinType: t.code}
	if t.layers > 0 {
		w.assignedHeight[t]++
		layers := t.layers
		a.Layers = &layers
	}
	return a
}
