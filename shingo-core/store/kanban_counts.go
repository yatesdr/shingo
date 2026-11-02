package store

import (
	"fmt"

	"shingo/protocol"
	"shingocore/kanban"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// kanban_counts.go — the keeper's populations for many kanbans at once, from one read of
// their loops (DB.ListKanbanLoop), so neither the keeper's tick nor a snapshot build pays per
// kanban or per level. The arithmetic on top is LevelState's and kanban.Line's.

type kanbanTypeKey struct {
	kanban int64
	code   string
}

type kanbanFamilyKey struct {
	kanban int64
	family int64
}

// FamilyFound is where one family's carts are, per place of the loop. A keeper's own asks are
// not in it: they are counted as asked, once.
type FamilyFound struct {
	Slot, Line, Window, Flight int
	// Unowned is how many of Flight stand on a robot with no live order.
	Unowned int
}

// Total is everything found.
func (f FamilyFound) Total() int { return f.Slot + f.Line + f.Window + f.Flight }

// KanbanCounts is one read of the loops.
type KanbanCounts struct {
	resident map[kanbanTypeKey]int
	byTotes  map[kanbanTypeKey]map[int]int
	inbound  map[kanbanTypeKey][]orders.InboundCarrier
	toLine   map[kanbanTypeKey]int
	held     map[kanbanTypeKey]int
	windows  map[int64][]protocol.NodeBinsRow
	found    map[kanbanFamilyKey]*FamilyFound
	families map[string]int64
	carts    map[int64][]LoopCart
}

// ReadKanbanCounts reads the populations of the named kanbans (nil: all), in one statement.
func (db *DB) ReadKanbanCounts(ids []int64) (*KanbanCounts, error) {
	carts, err := db.ListKanbanLoop(ids)
	if err != nil {
		return nil, err
	}
	return countsOfLoop(carts), nil
}

// countsOfLoop sorts each cart into what it is to its kanban.
//
//	Standing in the kanban, an empty the finder would take: resident.
//	Standing on a supported position, not spoken for: held.
//	Carried into the kanban: coming (and its row, for the walk's "already on its way").
//	Carried onto a supported position: coming.
//	At a window or carried to one: the walk's, by the type it lights.
func countsOfLoop(carts []LoopCart) *KanbanCounts {
	c := &KanbanCounts{resident: map[kanbanTypeKey]int{}, byTotes: map[kanbanTypeKey]map[int]int{},
		inbound: map[kanbanTypeKey][]orders.InboundCarrier{}, toLine: map[kanbanTypeKey]int{}, held: map[kanbanTypeKey]int{},
		windows: map[int64][]protocol.NodeBinsRow{}, found: map[kanbanFamilyKey]*FamilyFound{}, families: map[string]int64{},
		carts: map[int64][]LoopCart{}}
	for _, lc := range carts {
		c.families[lc.Code] = lc.FamilyID
		c.carts[lc.Kanban] = append(c.carts[lc.Kanban], lc)
		k := kanbanTypeKey{lc.Kanban, lc.Code}
		if !lc.Asked {
			f := c.found[kanbanFamilyKey{lc.Kanban, lc.FamilyID}]
			if f == nil {
				f = &FamilyFound{}
				c.found[kanbanFamilyKey{lc.Kanban, lc.FamilyID}] = f
			}
			switch lc.Place {
			case LoopPlaceSlot:
				f.Slot++
			case LoopPlaceLine:
				f.Line++
			case LoopPlaceWindow:
				f.Window++
			default:
				f.Flight++
				if lc.Unowned {
					f.Unowned++
				}
			}
		}
		if lc.Resident && lc.Into == "" {
			if lc.Stock {
				c.resident[k]++
			}
			if lc.TotesOnCart != nil {
				if c.byTotes[k] == nil {
					c.byTotes[k] = map[int]int{}
				}
				c.byTotes[k][*lc.TotesOnCart]++
			}
		}
		switch {
		case lc.Asked || lc.Unowned || lc.Bare:
		case lc.Into == LoopPlaceSlot:
			if lc.PayloadCode == "" {
				c.inbound[k] = append(c.inbound[k], orders.InboundCarrier{BinID: lc.BinID, TotesOnCart: lc.TotesOnCart, State: lc.State})
			}
		case lc.Into == LoopPlaceLine:
			if lc.Stock {
				c.toLine[k]++
			}
		case lc.Into == LoopPlaceWindow || lc.Place == LoopPlaceWindow:
			c.windows[lc.Kanban] = append(c.windows[lc.Kanban], protocol.NodeBinsRow{Node: fmt.Sprintf("cart-%d", lc.BinID), BinID: lc.BinID,
				BinLabel: lc.Label, BinTypeCode: lc.Code, PayloadCode: lc.PayloadCode, TotesOnCart: lc.TotesOnCart,
				TotesPerLayer: lc.TotesPerLayer, Occupied: true})
		case lc.Place == LoopPlaceLine:
			c.held[k]++
		}
	}
	return c
}

// Level is one level type's populations, against the open episode's asks (the keeper's own
// read, ListOpenKeeperEpisodes).
func (c *KanbanCounts) Level(lv KanbanLevel, asked int) LevelState {
	k := kanbanTypeKey{lv.GroupNodeID, lv.BinTypeCode}
	inbound := c.inbound[k]
	return LevelState{Level: lv, Have: kanban.Have{Resident: c.resident[k], Asked: asked,
		Coming: orders.LevelComing(inbound) + c.toLine[k], Held: c.held[k]}, Inbound: inbound}
}

// Found is one (kanban, family)'s carts by place.
func (c *KanbanCounts) Found(kanbanID, familyID int64) FamilyFound {
	if f := c.found[kanbanFamilyKey{kanbanID, familyID}]; f != nil {
		return *f
	}
	return FamilyFound{}
}

// WindowCarts is the carts at a kanban's windows and on their way to one, as the walk reads
// them. Their Node is a key of the walk's alone.
func (c *KanbanCounts) WindowCarts(kanbanID int64) []protocol.NodeBinsRow { return c.windows[kanbanID] }

// SlotsSpoken is the kanban's slots with a cart on them or an order delivering one, for the
// walk's word on whether a return lands at home. Admits are not read: it is a word, and the door
// repeats the choice on the slots themselves.
func (c *KanbanCounts) SlotsSpoken(kanbanID int64) int {
	n := 0
	for _, lc := range c.carts[kanbanID] {
		if lc.Place == LoopPlaceSlot || lc.Into == LoopPlaceSlot {
			n++
		}
	}
	return n
}

// Carts is every cart of a kanban's loop.
func (c *KanbanCounts) Carts(kanbanID int64) []LoopCart { return c.carts[kanbanID] }

// Lit is the type each cart at the kanban's windows would come back as, by type: the walk
// over the lines given, which are the kanban's with its windows left out. A cart the walk
// lights nothing keeps its own type and is nobody's shortfall.
func (c *KanbanCounts) Lit(kanbanID int64, lines []kanban.Line, families map[string]int64) map[string]int {
	carts := c.windows[kanbanID]
	if len(carts) == 0 {
		return nil
	}
	for code, family := range c.families {
		if _, ok := families[code]; !ok {
			families[code] = family
		}
	}
	out := map[string]int{}
	for _, a := range kanban.Assign(lines, carts, nil, families) {
		out[a.BinType]++
	}
	return out
}

// TypeLine is one level type's line.
func (c *KanbanCounts) TypeLine(lv KanbanLevel, asked int, layerWants []KanbanLayerWant) (kanban.Line, LevelState) {
	s := c.Level(lv, asked)
	return typeLineOf(s, layerWants, c.byTotes[kanbanTypeKey{lv.GroupNodeID, lv.BinTypeCode}]), s
}

// typeLineOf composes a level type's line from its count and, for a type with
// wanted heights, its residents by totes. The comings at a height are the count's
// coming rows (LevelState.Inbound), so no second read spells "coming".
func typeLineOf(c LevelState, layerWants []KanbanLayerWant, residentByTotes map[int]int) kanban.Line {
	lv := c.Level
	tl := kanban.LineOf(lv.BinTypeCode, lv.Want, c.Have)
	for _, w := range layerWants {
		if w.BinTypeID != lv.BinTypeID || !protocol.IsToteType(w.TotesPerLayer) {
			continue
		}
		tl.TotesPerLayer = *w.TotesPerLayer
		tl.Heights = append(tl.Heights, kanban.HeightLine{Layers: w.Layers, Want: w.Want})
	}
	coming := map[int]int{}
	for _, ib := range c.Inbound {
		if ib.TotesOnCart != nil {
			coming[*ib.TotesOnCart]++
		}
	}
	for i := range tl.Heights {
		totes := tl.Heights[i].Layers * tl.TotesPerLayer
		tl.Heights[i].Have = residentByTotes[totes] + coming[totes]
	}
	return tl
}

// A level is of EMPTIES: the forecast counts empties, one of the five fragments a kanban of fulls
// would widen.
// SourceableCounts is the keeper's forecast for one short kanban: how many empties of each of
// codes a plant-wide find with this fence would answer with, so asks never pass the carts
// that exist (bins.SourceableCounts).
func (db *DB) SourceableCounts(codes []string, fence bins.EmptyFence) (map[string]int, error) {
	counts, err := bins.SourceableCounts(db.DB, codes, fence)
	if err != nil {
		return nil, fmt.Errorf("keeper forecast: %w", err)
	}
	return counts, nil
}

// GroupLineAsk is one outbound group a snapshot build wants the line of: the group,
// its levels (effective, from the picture), and the rows of the bins at its
// unloader's windows in the plant's order.
type GroupLineAsk struct {
	Group   *nodes.Node
	Levels  []KanbanLevel
	Windows []protocol.NodeBinsRow
	// Slots is the kanban's enabled children.
	Slots int
	// Sources are the group's sources in rank order, each with its levels: none for
	// one that declares none, which is short of nothing.
	Sources []GroupLineSource
}

// GroupLineSource is one source of a kanban an unloader's walk asks.
type GroupLineSource struct {
	ID     int64
	Name   string
	Levels []KanbanLevel
}
