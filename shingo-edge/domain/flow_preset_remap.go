package domain

import "sort"

// flow_preset_remap.go — a recipe drawn on one cell, laid onto another
// (pick-a-part U7, R-J).
//
// IMPORT IS A DRAFT, NOT A SAVE. What comes out of here is handed to the
// desktop as a shape draft and saved through the same create path a drawn
// shape takes (ProcessService.CreateFromCells), so the store's validation
// against this cell's positions and routing set is the only gate. This
// function's job is to get as close as the two cells allow and to say,
// cell by cell, where it could not — never to guess.

// ShapeFrame is a cell as a recipe sees it: its positions in picture order and
// its routing rows.
type ShapeFrame struct {
	Positions []string
	Routing   []RoutingNode
}

// RemapShape lays src, drawn on the cell from describes, onto the cell to
// describes. The part and the Advanced block are dropped (StripFlowPayload).
//
//   - POSITIONS BY PICTURE ORDER: from's i-th position is to's i-th. A source
//     cell past to's last position has nowhere to go; it is dropped and its
//     source name listed.
//   - PARTNER AND STAGING FOLLOW THE PAIRING: a field naming a position maps
//     with the positions; one naming a routing row maps to to's row of the
//     same role at the same rank by sequence.
//   - SOURCE AND DESTINATION BY ROLE: to's lowest-sequence enabled row of the
//     role the value held in from's routing set.
//   - key_route IS ALWAYS BLANKED when set: its waypoints are vendor-map names
//     drawn for the other cell's aisles, and nothing here can check them
//     without a round trip. Evac nodes map with the positions or are blanked.
//
// unmapped is the dropped source positions, then the target positions whose
// cells had a field blanked, each sorted — the cells the draft flags.
func RemapShape(src []FlowCell, from, to ShapeFrame) (cells []FlowCell, unmapped []string) {
	pos := map[string]string{}
	for i, p := range from.Positions {
		if i < len(to.Positions) {
			pos[p] = to.Positions[i]
		}
	}
	m := remapper{pos: pos, from: from.Routing, to: to.Routing}
	var dropped, flagged []string
	for _, c := range StripFlowPayload(src) {
		target, ok := pos[c.CoreNodeName]
		if !ok {
			dropped = append(dropped, c.CoreNodeName)
			continue
		}
		m.blanked = false
		c.CoreNodeName = target
		c.PairedCoreNode = m.position(c.PairedCoreNode)
		c.SecondPairedCoreNode = m.position(c.SecondPairedCoreNode)
		c.InboundStaging = m.paired(c.InboundStaging)
		c.OutboundStaging = m.paired(c.OutboundStaging)
		c.InboundSource = m.byRole(c.InboundSource)
		c.OutboundDestination = m.byRole(c.OutboundDestination)
		c.ChangeoverEvacDestination = m.byRole(c.ChangeoverEvacDestination)
		c.PartialDestination = m.byRole(c.PartialDestination)
		c.ChangeoverEvacNodes = m.positions(c.ChangeoverEvacNodes)
		if len(c.KeyRoute) > 0 {
			c.KeyRoute, m.blanked = nil, true
		}
		if m.blanked {
			flagged = append(flagged, target)
		}
		cells = append(cells, c)
	}
	sort.Strings(dropped)
	sort.Strings(flagged)
	return cells, append(dropped, flagged...)
}

// remapper carries one RemapShape's maps; blanked says the cell in hand lost
// a field.
type remapper struct {
	pos      map[string]string
	from, to []RoutingNode
	blanked  bool
}

func (m *remapper) blank() string {
	m.blanked = true
	return ""
}

func (m *remapper) position(name string) string {
	if name == "" {
		return ""
	}
	if p, ok := m.pos[name]; ok {
		return p
	}
	return m.blank()
}

func (m *remapper) positions(names []string) []string {
	var out []string
	for _, n := range names {
		p, ok := m.pos[n]
		if !ok {
			m.blanked = true
			return nil
		}
		out = append(out, p)
	}
	return out
}

// paired maps a staging field: a position with the positions, a routing row
// to the same role and rank.
func (m *remapper) paired(name string) string {
	if name == "" {
		return ""
	}
	if p, ok := m.pos[name]; ok {
		return p
	}
	role, rank, ok := routingRank(m.from, name)
	if !ok {
		return m.blank()
	}
	rows := rankedOfRole(m.to, role)
	switch {
	case rank < len(rows):
		return rows[rank]
	case len(rows) > 0:
		return rows[0]
	}
	return m.blank()
}

func (m *remapper) byRole(name string) string {
	if name == "" {
		return ""
	}
	role, _, ok := routingRank(m.from, name)
	if !ok {
		return m.blank()
	}
	if n := lowestOfRole(m.to, role); n != "" {
		return n
	}
	return m.blank()
}

// routingRank finds name among the enabled routing rows: its role and its
// place among that role's rows by sequence.
func routingRank(rows []RoutingNode, name string) (string, int, bool) {
	for _, r := range rows {
		if r.CoreNodeName == name && r.Enabled {
			for i, n := range rankedOfRole(rows, r.Role) {
				if n == name {
					return r.Role, i, true
				}
			}
		}
	}
	return "", 0, false
}

// rankedOfRole is one role's enabled rows by sequence, ties by name — the
// order lowestOfRole picks the first of.
func rankedOfRole(rows []RoutingNode, role string) []string {
	var of []RoutingNode
	for _, r := range rows {
		if r.Role == role && r.Enabled {
			of = append(of, r)
		}
	}
	sort.Slice(of, func(i, j int) bool {
		if of[i].Sequence != of[j].Sequence {
			return of[i].Sequence < of[j].Sequence
		}
		return of[i].CoreNodeName < of[j].CoreNodeName
	})
	out := make([]string, len(of))
	for i, r := range of {
		out[i] = r.CoreNodeName
	}
	return out
}
