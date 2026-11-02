package engine

import (
	"fmt"

	"shingo/protocol"
	"shingocore/kanban"
	"shingocore/store"
)

// unloader_lines.go — an unloader's group line: what the outbound group of an
// unloader wants, and what each cart standing at the unloader's windows is
// asked to go back as.
//
// THE KEEPER'S ARITHMETIC. A level type's shortfall is the keeper's own
// (store.KanbanCounts: the effective want less resident, asked and coming), read
// against the same open kanban episodes KanbanKeeper.Tick reads. Nothing here is
// a fifth term. The walk over the carts is kanban.AssignAcross.
//
// KEYED ON LEVELS, NOT ON TOTE NUMBERS. A group with no levels gets no line and
// costs no statement; a group with levels gets an assignment for every bin
// type, numbers or none.

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
// THREE STATEMENTS PER CALL, however many groups and levels: the open kanban
// episodes with their asks, the types in play (the level types, the sources' and the
// window carts' types, each with its family and the groups' layer wants,
// ListTypesInPlay), and the loops (store.DB.ReadKanbanCounts, whose rows carry what a
// wanted height reads). The sources ride the same reads as their
// kanbans, whether the walk comes to ask them or not, so a whole kanban costs nothing
// more. A STOP in the brief to add one per kanban. Pin: TestKanbanCost_SnapshotBuild.
func (e *Engine) UnloaderGroupLines(asks []store.GroupLineAsk) ([]protocol.NodeBinsGroup, error) {
	ids, codes := groupLineScope(asks)
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
	wantsByGroup := map[int64][]store.KanbanLayerWant{}
	families := map[string]int64{}
	for _, t := range types {
		for _, w := range t.LayerWants {
			wantsByGroup[w.GroupNodeID] = append(wantsByGroup[w.GroupNodeID], w)
		}
		families[t.Type.Code] = t.Type.FamilyID
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
		linesOf := func(groupID int64, group string, levels []store.KanbanLevel) []kanban.Line {
			lines := make([]kanban.Line, 0, len(levels))
			for _, lv := range levels {
				asked := askedByKey[protocol.KanbanEpisodeKey(groupID, lv.BinTypeID)]
				tl, c := counts.TypeLine(lv, asked, wantsByGroup[groupID])
				for _, ib := range c.Inbound {
					inbound[ib.BinID] = true
				}
				lines = append(lines, tl)
			}
			return lines
		}
		lines := linesOf(a.Group.ID, a.Group.Name, a.Levels)
		// A LOOP CART LANDS AT HOME FIRST (plan.Planner.ChoosePush): a source is asked for only
		// with no slot left here, or the board would say a cart is for a source and it would land
		// at home.
		sources := make([]kanban.Source, 0, len(a.Sources))
		for _, s := range a.Sources {
			sources = append(sources, kanban.Source{Name: s.Name, Lines: linesOf(s.ID, s.Name, s.Levels)})
		}
		if counts.SlotsSpoken(a.Group.ID) < a.Slots {
			sources = nil
		}
		out = append(out, protocol.NodeBinsGroup{Group: a.Group.Name, Wants: kanban.Wants(lines),
			Assignments: kanban.AssignAcross(lines, sources, a.Windows, inbound, families)})
	}
	return out, nil
}

// groupLineScope is the groups and types a board's asks name, each type once.
func groupLineScope(asks []store.GroupLineAsk) (ids []int64, codes []string) {
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
	return ids, codes
}
