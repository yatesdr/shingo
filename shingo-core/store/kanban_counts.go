package store

import (
	"fmt"

	"shingo/protocol"
	"shingocore/kanban"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// kanban_counts.go — the keeper's populations for many kanbans at once: what
// CountLevel and TypeLineOf read one (group, type) at a time, in two statements
// whatever the plant's size, so neither the keeper's tick nor a snapshot build
// pays per kanban or per level. Each read composes the single read's fragments
// (bins.ListKanbanResidents, orders.ListKanbanInbound); the arithmetic on top is
// LevelState's and TypeLine's, as it is for the single reads.

type kanbanTypeKey struct {
	kanban int64
	code   string
}

// KanbanCounts is one read of the residents and the comings.
type KanbanCounts struct {
	resident map[kanbanTypeKey]int
	byTotes  map[kanbanTypeKey]map[int]int
	inbound  map[kanbanTypeKey][]orders.InboundCarrier
}

// ReadKanbanCounts reads the populations of the named kanbans (nil: all). The
// comings are read first: a cart landing between the two statements then counts
// in both for this read, which asks one less now and is right on the next read,
// and never in neither, which would ask for a cart that is already there.
func (db *DB) ReadKanbanCounts(ids []int64) (*KanbanCounts, error) {
	inbound, err := orders.ListKanbanInbound(db.DB, ids)
	if err != nil {
		return nil, err
	}
	residents, err := bins.ListKanbanResidents(db.DB, ids)
	if err != nil {
		return nil, err
	}
	c := &KanbanCounts{resident: map[kanbanTypeKey]int{}, byTotes: map[kanbanTypeKey]map[int]int{},
		inbound: map[kanbanTypeKey][]orders.InboundCarrier{}}
	for _, r := range residents {
		k := kanbanTypeKey{r.Kanban, r.Code}
		if r.Stock {
			c.resident[k] += r.N
		}
		if r.Totes != nil {
			if c.byTotes[k] == nil {
				c.byTotes[k] = map[int]int{}
			}
			c.byTotes[k][*r.Totes] += r.N
		}
	}
	for _, in := range inbound {
		k := kanbanTypeKey{in.Kanban, in.Code}
		c.inbound[k] = append(c.inbound[k], in.InboundCarrier)
	}
	return c, nil
}

// Level is one level type's populations, as CountLevel answers them: asked is the
// open episode's, read with the episodes (ListOpenKeeperEpisodes).
func (c *KanbanCounts) Level(lv KanbanLevel, asked int) LevelState {
	k := kanbanTypeKey{lv.GroupNodeID, lv.BinTypeCode}
	inbound := c.inbound[k]
	return LevelState{Level: lv, Have: kanban.Have{Resident: c.resident[k], Asked: asked,
		Coming: orders.LevelComing(inbound)}, Inbound: inbound}
}

// TypeLine is one level type's line, as TypeLineOf answers it.
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

// SourceableTypes is the keeper's forecast for one short kanban: which of codes a
// plant-wide find with this fence would answer with something
// (bins.SourceableTypes).
func (db *DB) SourceableTypes(codes []string, fence bins.EmptyFence) (map[string]bool, error) {
	types, err := bins.SourceableTypes(db.DB, codes, fence)
	if err != nil {
		return nil, fmt.Errorf("keeper forecast: %w", err)
	}
	return types, nil
}

// GroupLineAsk is one outbound group a snapshot build wants the line of: the group,
// its levels (effective, from the picture), and the rows of the bins at its
// unloader's windows in the plant's order.
type GroupLineAsk struct {
	Group   *nodes.Node
	Levels  []KanbanLevel
	Windows []protocol.NodeBinsRow
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
