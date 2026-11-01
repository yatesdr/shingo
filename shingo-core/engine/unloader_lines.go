package engine

import (
	"fmt"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/store"
)

// unloader_lines.go — an unloader's group line: what the outbound group of an
// unloader wants, and what each cart standing at the unloader's windows is
// asked to go back as.
//
// THE KEEPER'S ARITHMETIC. A level type's shortfall is the keeper's own
// (store.KanbanCounts: the effective want less resident, asked and coming), read
// against the same open maintain episodes Maintainer.Tick reads. Nothing here is
// a fifth term. The line's model (store.TypeLine) lives in store beside it.
//
// HEIGHTS, FOR A TYPE WITH LAYER WANTS. When the type states its totes per
// layer, each wanted height is a thing of its own: a cart counts toward a
// height only when its totes_on_cart is exactly that many layers of the type's
// totes per layer. A leftover cart (a remainder) or an unverified one (no
// number) counts toward its type and toward no height. Asks carry no number,
// so they count toward the type alone. A type with layer wants and no totes
// per layer has no heights: no cart can be counted at one.
//
// TOPS & BOTTOMS IS A LINE OF ITS OWN. The wanted height at 0 layers is the
// type's tops & bottoms row: its shortfall is its own (want, less the carts
// at 0 standing and coming, less the walk's), never capped by the type's,
// never taken from the type's any-height shortfall, and an assignment to it
// is not one of the type's.
//
// KEYED ON LEVELS, NOT ON TOTE NUMBERS. A group with no levels gets no line and
// costs no statement; a group with levels gets an assignment for every bin
// type, numbers or none.
//
// A WHOLE KANBAN REPAYS ITS SOURCES. The kanban's walk runs over every window; each
// source, in rank order, then walks what is left (walkGroups). An assignment for a
// source names it (Group); a cart nothing is short of says so (NoneShort). The door
// repeats the choice on the real count (plan.Planner.ChoosePush): a stale card sends a
// cart to the door, never past it.

// UnloaderGroupLines answers the lines of a snapshot build's outbound groups, in
// the order asked. An ask's Levels are as the picture returns them (their order
// breaks ties); its Windows are the rows of the bins at the unloader's windows in
// the plant's order (bin_loader_homes.sort_order), unoccupied windows included or
// not.
//
// A group with no levels has no line, and with none anywhere there is no statement.
// A failed read is an error and no line: never a line built on a count that was not
// made.
//
// FOUR STATEMENTS PER CALL, however many groups and levels: the open maintain
// episodes with their asks, the types in play (the level types, the sources' and the
// window carts' types, each with its floor size and the groups' layer wants,
// ListTypesInPlay), the comings and the residents (store.DB.ReadKanbanCounts, whose
// rows carry what a wanted height reads). The sources ride the same reads as their
// kanbans, whether the walk comes to ask them or not, so a whole kanban costs nothing
// more. A STOP in the brief to add one per kanban. Pin: TestKanbanCost_SnapshotBuild.
func (e *Engine) UnloaderGroupLines(asks []store.GroupLineAsk) ([]protocol.NodeBinsGroup, error) {
	var ids []int64
	var codes []string
	seen := map[string]bool{}
	addCode := func(code string) {
		if code != "" && !seen[code] {
			seen[code] = true
			codes = append(codes, code)
		}
	}
	for _, a := range asks {
		if len(a.Levels) == 0 {
			continue
		}
		ids = append(ids, a.Group.ID)
		for _, lv := range a.Levels {
			addCode(lv.BinTypeCode)
		}
		for _, s := range a.Sources {
			ids = append(ids, s.ID)
			for _, lv := range s.Levels {
				addCode(lv.BinTypeCode)
			}
		}
		for _, w := range a.Windows {
			addCode(w.BinTypeCode)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	open, err := e.db.ListOpenKeeperEpisodes()
	if err != nil {
		return nil, fmt.Errorf("group lines: list open episodes: %w", err)
	}
	askedByKey := make(map[string]int, len(open))
	for _, o := range open {
		askedByKey[o.EpisodeKey] = o.Asked
	}
	types, err := e.db.ListTypesInPlay(ids, codes)
	if err != nil {
		return nil, fmt.Errorf("group lines: %w", err)
	}
	wantsByGroup := map[int64][]store.MaintainLayerWant{}
	sizes := map[string]domain.FloorSize{}
	for _, t := range types {
		for _, w := range t.LayerWants {
			wantsByGroup[w.GroupNodeID] = append(wantsByGroup[w.GroupNodeID], w)
		}
		if fs, ok := t.Type.FloorSize(); ok {
			sizes[t.Type.Code] = fs
		}
	}
	counts, err := e.db.ReadKanbanCounts(ids)
	if err != nil {
		return nil, fmt.Errorf("group lines: %w", err)
	}

	var out []protocol.NodeBinsGroup
	for _, a := range asks {
		if len(a.Levels) == 0 {
			continue
		}
		inbound := map[int64]bool{}
		linesOf := func(groupID int64, group string, levels []store.MaintainLevel) []store.TypeLine {
			lines := make([]store.TypeLine, 0, len(levels))
			for _, lv := range levels {
				asked := askedByKey[protocol.MaintainEpisodeKey(group, lv.BinTypeCode)]
				tl, c := counts.TypeLine(lv, asked, wantsByGroup[groupID])
				for _, ib := range c.Inbound {
					inbound[ib.BinID] = true
				}
				lines = append(lines, tl)
			}
			return lines
		}
		lines := linesOf(a.Group.ID, a.Group.Name, a.Levels)
		sources := make([]sourceLines, 0, len(a.Sources))
		for _, s := range a.Sources {
			sources = append(sources, sourceLines{name: s.Name, lines: linesOf(s.ID, s.Name, s.Levels)})
		}
		out = append(out, protocol.NodeBinsGroup{Group: a.Group.Name, Wants: wantsOf(lines),
			Assignments: walkGroups(lines, sources, a.Windows, inbound, sizes)})
	}
	return out, nil
}

// sourceLines is one of a kanban's sources as its walk reads it, in rank order. One
// that declares no level has no lines and is short of nothing: it is reached only as
// a type's overflow (the save warns, "repaid only as T's overflow").
type sourceLines struct {
	name  string
	lines []store.TypeLine
}

// walkGroups is what every cart at a kanban's windows is asked: the kanban's walk over
// all of them, then each source's, in rank order, over the carts still left. Sequential
// walks, each with its own running counts: the walk keys what it has assigned by type
// code, and a source's line of a type is not the kanban's. inbound is every group's, so
// a cart already on its way into any of them is walked by none. The same floor rule
// holds in each (windowWalk.fits): a 45 x 48 cart is never asked for a source of 32 x 30
// alone. A cart nothing is short of answers NoneShort. In the windows' order.
func walkGroups(lines []store.TypeLine, sources []sourceLines, windows []protocol.NodeBinsRow,
	inbound map[int64]bool, sizes map[string]domain.FloorSize) []protocol.NodeBinsAssignment {
	byNode := map[string]protocol.NodeBinsAssignment{}
	take := func(as []protocol.NodeBinsAssignment, group string) {
		for _, a := range as {
			a.Group = group
			byNode[a.Node] = a
		}
	}
	take(assignWindows(lines, windows, inbound, sizes), "")
	left := func() []protocol.NodeBinsRow {
		var out []protocol.NodeBinsRow
		for _, row := range windows {
			if _, done := byNode[row.Node]; !done && walked(row, inbound) {
				out = append(out, row)
			}
		}
		return out
	}
	for _, s := range sources {
		carts := left()
		if len(carts) == 0 {
			break
		}
		take(assignWindows(s.lines, carts, inbound, sizes), s.name)
	}
	for _, row := range left() {
		byNode[row.Node] = protocol.NodeBinsAssignment{Node: row.Node, NoneShort: true}
	}
	out := []protocol.NodeBinsAssignment{}
	for _, row := range windows {
		if a, ok := byNode[row.Node]; ok {
			out = append(out, a)
		}
	}
	return out
}

// walked reports whether the walk asks anything of the cart at a window: a cart on
// its way already chose at its CLEAR, and a bare cart is nobody's empty.
func walked(row protocol.NodeBinsRow, inbound map[int64]bool) bool {
	return row.Occupied && !inbound[row.BinID] && row.CarrierState() != protocol.CarrierBare
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

// thing is what a cart can be sent back as: a level type at any height (the
// zero Height) or at a wanted height of it, tops & bottoms (Layers(0))
// included. A comparable value, so it keys the walk's counts.
type thing struct {
	code   string
	height domain.Height
}

// layers is the height a thing ships as (protocol.NodeBinsAssignment.Layers):
// n for a wanted height, 0 included, nil for any height.
func (t thing) layers() *int {
	if n, ok := t.height.Stated(); ok {
		return &n
	}
	return nil
}

// assignWindows is the walk: what each cart at a window is asked to go back as.
//
//   - A running shortfall per thing: want less what the group has, less what
//     this walk has assigned. A height's shortfall is also held under its
//     type's, since a cart at a height is a cart of the type. A type's
//     any-height shortfall is what its wanted heights do not speak for. The
//     tops & bottoms row is its own line: held under nothing, taken from
//     nothing.
//   - A window whose cart is already on its way into the group (inbound: its
//     empty-out names it, so the keeper's coming counts it) is not walked. Its
//     choice was made at its CLEAR, and walking it would count it twice. Pin:
//     TestUnloaderGroupLines_AClearedCartIsCountedOnce.
//   - Windows holding an empty are walked first (it is already what it is;
//     sending it as it stands is no work), then windows holding a full, each
//     in the order given (the plant's).
//   - A cart is assigned its own thing while the group is still short of it.
//     A cart is a tote cart by its own row (its type's totes per layer and its
//     carrier state), never by whether the group wants heights of its type.
//     Tops & bottoms: the row. An empty at whole layers: its height when that
//     height is wanted, else its type. A full tote cart has none: the stack
//     leaves with the parts. A full knockdown, a leftover and an unverified
//     cart: the type at any height. Otherwise the thing with the largest
//     remaining shortfall, ties by the levels' order (within a type, its
//     heights from the lowest, then the type at any height). Nothing short:
//     no assignment. A bare cart is not walked: it is nobody's empty.
//   - THE CART'S FLOOR SIZE BOUNDS THE ASK. A cart whose type has its width and
//     length entered (sizes, domain.BinType.FloorSize) is asked only for types
//     of the same floor size, orientation-blind, and never for a type with no
//     size: a 45 × 48 cart comes back as a 45 × 48 knockdown, tote cart or
//     basket, never as a 36 × 36. A cart whose type has no size is asked as
//     before. Tops & bottoms is a height of its tote type, inside its size.
//
// The answer is in the windows' order. Pins: TestAssignWindows_*.
func assignWindows(lines []store.TypeLine, windows []protocol.NodeBinsRow, inbound map[int64]bool,
	sizes map[string]domain.FloorSize) []protocol.NodeBinsAssignment {
	w := &windowWalk{lines: lines, sizes: sizes, assignedType: map[string]int{}, assignedHeight: map[thing]int{}}
	byNode := map[string]protocol.NodeBinsAssignment{}
	pass := func(empty bool) {
		for _, row := range windows {
			if !walked(row, inbound) || (row.PayloadCode == "") != empty {
				continue
			}
			t, short := w.own(row)
			if !short {
				if t, short = w.largest(w.fits(row)); !short {
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

// windowWalk is the walk's running state: the group's lines, the floor size of
// each type in play that has one, and what the walk has assigned of each thing
// so far, subtracted from each line's shortfall.
type windowWalk struct {
	lines          []store.TypeLine
	sizes          map[string]domain.FloorSize
	assignedType   map[string]int
	assignedHeight map[thing]int
}

// fits is the types a cart may be asked for: every type for a cart whose type
// has no floor size, else the types of its own floor size only.
func (w *windowWalk) fits(row protocol.NodeBinsRow) func(code string) bool {
	own, ok := w.sizes[row.BinTypeCode]
	if !ok {
		return func(string) bool { return true }
	}
	return func(code string) bool {
		fs, ok := w.sizes[code]
		return ok && fs == own
	}
}

func (w *windowWalk) typeShort(tl store.TypeLine) int { return tl.Short() - w.assignedType[tl.Code] }

// heightShort is a wanted height's running shortfall: held under its type's,
// but for the tops & bottoms row (0 layers), which is its own.
func (w *windowWalk) heightShort(tl store.TypeLine, h store.HeightLine) int {
	own := h.Want - h.Have - w.assignedHeight[heightOf(tl, h)]
	if h.Layers == 0 {
		return own
	}
	return min(own, w.typeShort(tl))
}

// anyShort is the type's any-height shortfall: what its wanted stack heights
// do not speak for. The tops & bottoms row speaks for none of it.
func (w *windowWalk) anyShort(tl store.TypeLine) int {
	s := w.typeShort(tl)
	for _, h := range tl.Heights {
		if h.Layers > 0 {
			s -= max(0, w.heightShort(tl, h))
		}
	}
	return s
}

func heightOf(tl store.TypeLine, h store.HeightLine) thing {
	return thing{tl.Code, protocol.Layers(h.Layers)}
}

// wanted is the type's wanted height at layers, if it has one.
func wanted(tl store.TypeLine, layers int) (store.HeightLine, bool) {
	for _, h := range tl.Heights {
		if h.Layers == layers {
			return h, true
		}
	}
	return store.HeightLine{}, false
}

func (w *windowWalk) lineOf(code string) (store.TypeLine, bool) {
	for _, tl := range w.lines {
		if tl.Code == code {
			return tl, true
		}
	}
	return store.TypeLine{}, false
}

// own is the cart's own thing and whether the group is still short of it;
// false with no thing for a cart that has none.
func (w *windowWalk) own(row protocol.NodeBinsRow) (thing, bool) {
	tl, ok := w.lineOf(row.BinTypeCode)
	if !ok {
		return thing{}, false
	}
	tote := protocol.IsToteType(row.TotesPerLayer)
	switch row.CarrierState() {
	case protocol.CarrierTopsAndBottoms:
		// The row while the group is short of it; never its type at any height.
		if h, ok := wanted(tl, 0); ok {
			return heightOf(tl, h), w.heightShort(tl, h) > 0
		}
		return thing{}, false
	case protocol.CarrierLoaded:
		if tote {
			// A full tote cart: the stack leaves with the parts.
			return thing{}, false
		}
	case protocol.CarrierEmpty:
		if layers, ok := row.Layers(); ok && layers > 0 {
			if h, ok := wanted(tl, layers); ok {
				return heightOf(tl, h), w.heightShort(tl, h) > 0
			}
		}
	}
	// No height of its own (a full knockdown, a height not wanted, a leftover
	// or an unverified cart): its own thing is the type at any height, short
	// only by what the wanted heights do not speak for. Pinned by
	// TestAssignWindows_NoHeightOfItsOwnTakesTheLargestHeight.
	return thing{code: tl.Code}, w.anyShort(tl) > 0
}

// largest is the thing with the largest remaining shortfall among the types
// fits admits, first in the levels' order on a tie.
func (w *windowWalk) largest(fits func(code string) bool) (thing, bool) {
	var best thing
	bestShort := 0
	consider := func(t thing, short int) {
		if short > bestShort {
			best, bestShort = t, short
		}
	}
	for _, tl := range w.lines {
		if !fits(tl.Code) {
			continue
		}
		for _, h := range tl.Heights {
			consider(heightOf(tl, h), w.heightShort(tl, h))
		}
		consider(thing{code: tl.Code}, w.anyShort(tl))
	}
	return best, bestShort > 0
}

// assign counts t against the walk and answers the window's assignment: a
// wanted height ships its layers, 0 included, and any height ships none. Tops
// & bottoms is not one of its type's.
func (w *windowWalk) assign(node string, t thing) protocol.NodeBinsAssignment {
	a := protocol.NodeBinsAssignment{Node: node, BinType: t.code, Layers: t.layers()}
	if t.height != (domain.Height{}) {
		w.assignedHeight[t]++
	}
	if n, ok := t.height.Stated(); !ok || n > 0 {
		w.assignedType[t.code]++
	}
	return a
}
