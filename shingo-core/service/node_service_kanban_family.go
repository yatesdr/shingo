package service

import (
	"fmt"
	"sort"
	"strings"

	"shingocore/domain"
	"shingocore/store"
)

// node_service_kanban_family.go — what the save says about a kanban's carrier families. All of it
// is a warning but one: a plant may mean any of it, and the card shows it. Two kanbans levelling
// one family on one support are refused, because both would count the same line carts and
// neither count would mean anything.

// KanbanFamilyRow is one (kanban, family) as the card and the save read it: the pool, the
// minimum, and the room that admits it.
type KanbanFamilyRow struct {
	FamilyID     int64  `json:"family_id"`
	Family       string `json:"family"`
	Pool         int    `json:"pool"`
	PoolDeclared bool   `json:"pool_declared"`
	Minimum      int    `json:"minimum"`
	Room         int    `json:"room"`
	// CannotCover is pool less room: carts with no slot to come back to at the worst mix.
	CannotCover int `json:"cannot_cover,omitempty"`
	// Dips is how far the running type's level dips in the kanban during an unload: the carts
	// on the line that can stand at the windows, less what the pool holds above its minimum. The
	// types a change draws on never dip.
	Dips int `json:"dips,omitempty"`
}

// KanbanRoom is the one room function: how many of the kanban's enabled slots admit each family
// (bins.EffectiveTypesOfChildren). A slot with no list takes anything, so it admits them all,
// and a slot admitting any type of a family is room for that family. One statement, and the
// children the caller already holds.
func (s *NodeService) KanbanRoom(groupNodeID int64, children []*domain.Node, families map[int64]bool) (map[int64]int, error) {
	types, err := s.db.EffectiveTypesOfChildren(groupNodeID)
	if err != nil {
		return nil, err
	}
	room := map[int64]int{}
	for _, c := range children {
		if !c.Enabled {
			continue
		}
		list, typed := types[c.ID]
		if !typed {
			for f := range families {
				room[f]++
			}
			continue
		}
		seen := map[int64]bool{}
		for _, t := range list {
			if !seen[t.FamilyID] {
				seen[t.FamilyID] = true
				room[t.FamilyID]++
			}
		}
	}
	return room, nil
}

// ListKanbanFamilies is the card's family rows: pool, minimum and room per family the kanban
// levels or a style of its supports runs.
func (s *NodeService) ListKanbanFamilies(groupNodeID int64) ([]KanbanFamilyRow, error) {
	mins, err := s.db.ListKanbanMinimums([]int64{groupNodeID})
	if err != nil {
		return nil, err
	}
	if len(mins) == 0 {
		return nil, nil
	}
	pools, err := s.db.ListKanbanPools(groupNodeID)
	if err != nil {
		return nil, err
	}
	children, err := s.db.ListChildNodes(groupNodeID)
	if err != nil {
		return nil, fmt.Errorf("read children of %d: %w", groupNodeID, err)
	}
	families := map[int64]bool{}
	for _, m := range mins {
		families[m.FamilyID] = true
	}
	room, err := s.KanbanRoom(groupNodeID, children, families)
	if err != nil {
		return nil, err
	}
	out := make([]KanbanFamilyRow, 0, len(mins))
	for _, m := range mins {
		r := KanbanFamilyRow{FamilyID: m.FamilyID, Family: m.FamilyCode, Minimum: m.Minimum(), Room: room[m.FamilyID]}
		r.Pool, r.PoolDeclared = pools[m.FamilyID]
		if !r.PoolDeclared {
			r.Pool = r.Minimum
		}
		r.CannotCover = max(0, r.Pool-r.Room)
		r.Dips = min(m.Line, max(0, m.Line-(r.Pool-r.Minimum)))
		out = append(out, r)
	}
	return out, nil
}

// familyRefusal refuses the save that would leave two kanbans levelling one family on one
// support. Read before anything is written, over the configuration the save would leave.
func (s *NodeService) familyRefusal(cfg KanbanConfig, refuse func(string, ...any)) error {
	if len(cfg.Levels) == 0 || len(cfg.Supports) == 0 {
		return nil
	}
	types := make([]int64, 0, len(cfg.Levels))
	for _, l := range cfg.Levels {
		types = append(types, l.BinTypeID)
	}
	supports := make([]int64, 0, len(cfg.Supports))
	for _, sp := range cfg.Supports {
		supports = append(supports, sp.ProcessNodeID)
	}
	clashes, err := s.db.KanbansLevellingAFamilyOn(cfg.GroupNodeID, types, supports)
	if err != nil {
		return err
	}
	for _, c := range clashes {
		refuse("%s already levels %s on %s: two kanbans on one support would both count the same carts on the line", c.Kanban, c.Family, c.Support)
	}
	return nil
}

// familyWarnings is what the save says once its configuration stands: a pool written at its
// minimum, room under the pool, a pool under the minimum, a slot admitting part of a family, a
// claimed type nothing levels, and members of a family that disagree. Only a kanban levelling
// something pays it, and only at a save.
func (s *NodeService) familyWarnings(groupNodeID int64, actor string) ([]string, error) {
	levels, err := s.db.ListDeclaredLevels(groupNodeID)
	if err != nil {
		return nil, err
	}
	if len(levels) == 0 {
		return nil, nil
	}
	group, err := s.db.GetNode(groupNodeID)
	if err != nil {
		return nil, err
	}
	rows, err := s.ListKanbanFamilies(groupNodeID)
	if err != nil {
		return nil, err
	}
	var out []string
	missing := map[int64]int{}
	for _, r := range rows {
		if !r.PoolDeclared {
			missing[r.FamilyID] = r.Minimum
			out = append(out, fmt.Sprintf("%s holds a pool of %d of %s, its minimum: nobody has said otherwise", group.Name, r.Minimum, r.Family))
		}
		if r.Room < r.Pool {
			out = append(out, fmt.Sprintf("%s has room for %d of %s's %d carts: %d cannot come back while the rest stand there", group.Name, r.Room, r.Family, r.Pool, r.Pool-r.Room))
		}
		if r.Pool < r.Minimum {
			out = append(out, fmt.Sprintf("%s's pool of %d of %s is under its minimum of %d: the line eats the mix", group.Name, r.Pool, r.Family, r.Minimum))
		}
	}
	if len(missing) > 0 {
		if err := s.db.SaveKanban(store.KanbanChange{NodeID: groupNodeID, Pools: missing}, actor); err != nil {
			return out, err
		}
	}
	types, err := s.db.ListBinTypes()
	if err != nil {
		return out, err
	}
	partial, err := s.partialSlotWarnings(groupNodeID, levels, types)
	if err != nil {
		return out, err
	}
	out = append(out, partial...)
	out = append(out, memberWarnings(levels, types)...)
	unlevelled, err := s.db.ClaimedTypesNotLevelled(groupNodeID)
	if err != nil {
		return out, err
	}
	if len(unlevelled) > 0 {
		out = append(out, fmt.Sprintf("%s supports styles running %s, which it does not level", group.Name, strings.Join(unlevelled, ", ")))
	}
	return out, nil
}

// partialSlotWarnings names each slot admitting some of a levelled family's types and not the
// rest: a slot under a mezzanine may mean it, and a typed list may not.
func (s *NodeService) partialSlotWarnings(groupNodeID int64, levels []store.KanbanLevel, types []*domain.BinType) ([]string, error) {
	family := map[int64]int64{}
	code := map[int64]string{}
	for _, t := range types {
		family[t.ID] = t.FamilyID
		code[t.ID] = t.Code
	}
	levelled := map[int64]bool{}
	for _, l := range levels {
		levelled[family[l.BinTypeID]] = true
	}
	levelledByFamily := map[int64][]int64{}
	for _, t := range types {
		if levelled[t.FamilyID] && !t.Bare {
			levelledByFamily[t.FamilyID] = append(levelledByFamily[t.FamilyID], t.ID)
		}
	}
	children, err := s.db.ListChildNodes(groupNodeID)
	if err != nil {
		return nil, err
	}
	effective, err := s.db.EffectiveTypesOfChildren(groupNodeID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range children {
		list, typed := effective[c.ID]
		if !typed || !c.Enabled {
			continue
		}
		admits := map[int64]bool{}
		for _, t := range list {
			admits[t.ID] = true
		}
		for _, members := range levelledByFamily {
			var has, lacks []string
			for _, id := range members {
				if admits[id] {
					has = append(has, code[id])
				} else {
					lacks = append(lacks, code[id])
				}
			}
			if len(has) > 0 && len(lacks) > 0 {
				sort.Strings(lacks)
				out = append(out, fmt.Sprintf("%s admits part of a family this kanban levels: not %s", c.Name, strings.Join(lacks, ", ")))
			}
		}
	}
	return out, nil
}

// memberWarnings names a levelled family whose members disagree on footprint or robot group.
// Neither is a decision: a footprint is a label and a check, and a robot group is the type's.
func memberWarnings(levels []store.KanbanLevel, types []*domain.BinType) []string {
	levelled := map[int64]bool{}
	byID := map[int64]*domain.BinType{}
	for _, t := range types {
		byID[t.ID] = t
	}
	for _, l := range levels {
		if t := byID[l.BinTypeID]; t != nil {
			levelled[t.FamilyID] = true
		}
	}
	type seen struct {
		code, footprints, groups map[string]bool
	}
	families := map[int64]*seen{}
	for _, t := range types {
		if t.Bare || !levelled[t.FamilyID] {
			continue
		}
		f := families[t.FamilyID]
		if f == nil {
			f = &seen{code: map[string]bool{t.FamilyCode: true}, footprints: map[string]bool{}, groups: map[string]bool{}}
			families[t.FamilyID] = f
		}
		if fp, ok := t.Footprint(); ok {
			f.footprints[fp.String()] = true
		}
		f.groups[t.RequiredRobotGroup] = true
	}
	var out []string
	for _, f := range families {
		name := ""
		for c := range f.code {
			name = c
		}
		if len(f.footprints) > 1 {
			out = append(out, fmt.Sprintf("the types of %s disagree on their footprint: %s", name, joinKeys(f.footprints)))
		}
		if len(f.groups) > 1 {
			out = append(out, fmt.Sprintf("the types of %s disagree on their robot group: %s", name, joinKeys(f.groups)))
		}
	}
	sort.Strings(out)
	return out
}

func joinKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		if k == "" {
			k = "none"
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// RenameWarnings says what joins a node by its name, before a save renames it. A rename is the
// fleet's identity moving, and each of these stops matching with no error of its own: a loader's
// outbound or inbound, a claim's position. Empty with no rename or nothing joined.
func (s *NodeService) RenameWarnings(n *domain.Node, newName string) ([]string, error) {
	if n == nil || newName == n.Name {
		return nil, nil
	}
	refs, err := s.db.ReferencesToNodeName(n.Name)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, fmt.Sprintf("renaming %s leaves %s pointing at the old name", n.Name, r))
	}
	return out, nil
}
