package service

import (
	"fmt"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// RemovalImpact is the screen sheet's question before a save that takes
// positions off a station: what still names them, and what a person set at
// them that the take-off clears (positionSettings). nodeNames is the list the
// save will send, read as SetNodes reads it; the answer is
// domain.PositionRemovalWarning over the positions the station owns that the
// list leaves out. Nothing is written.
//
// ONLY ON THE REMOVE PATH. The sheet asks only when its list drops a position,
// and the save itself (SetNodes) reads nothing more than it did. A list that
// removes nothing costs two reads, the station and its positions; one that
// removes a position adds five, flat in the style count: the recipes, the live
// claims, the styles, the process and the removed positions' runtime rows; a
// sixth, the process's station counters, when a removed position is counted
// at one. TestRemovalImpact_Statements pins all three.
func (s *StationService) RemovalImpact(stationID int64, nodeNames []string) (string, error) {
	station, err := s.db.GetOperatorStation(stationID)
	if err != nil {
		return "", err
	}
	return s.removalImpact(station.ProcessID, stationID, nodeNames)
}

// LooseRemovalImpact is RemovalImpact for the list SetLoosePositions takes:
// the warning over the process's positions on no screen that nodeNames
// leaves out. A position on a screen is not that list's to remove, so it is
// never named. Nothing is written; one read when nothing is removed, the
// same five or six more when something is.
func (s *StationService) LooseRemovalImpact(processID int64, nodeNames []string) (string, error) {
	return s.removalImpact(processID, 0, nodeNames)
}

// removalImpact is the warning both share, as setPositions is the save both
// share: the positions the write owns (the station's, or with stationID 0
// the process's on no screen) that nodeNames, read as the save reads it,
// leaves out, what still names them, and what they take with them.
func (s *StationService) removalImpact(processID, stationID int64, nodeNames []string) (string, error) {
	var owned []processes.Node
	var err error
	if stationID != 0 {
		owned, err = s.db.ListProcessNodesByStation(stationID)
	} else {
		var nodes []processes.Node
		nodes, err = s.db.ListProcessNodesByProcess(processID)
		owned = onNoScreen(nodes)
	}
	if err != nil {
		return "", err
	}
	_, desired := s.wantedNames(nodeNames)
	var removed []processes.Node
	var names []string
	for _, n := range owned {
		if !desired[n.CoreNodeName] {
			removed = append(removed, n)
			names = append(names, n.CoreNodeName)
		}
	}
	if len(removed) == 0 {
		return "", nil
	}
	claims, err := s.positionClaims(processID, names)
	if err != nil {
		return "", err
	}
	if err := s.positionSettings(processID, removed, claims); err != nil {
		return "", err
	}
	return domain.PositionRemovalWarning(claims), nil
}

// positionSettings fills in, on claims (one per position of removed, in the
// same order), what a person set at each position that the save's retire
// clears for good: its light curtain, the station counter it is counted at,
// and a person's auto-reorder off (domain.PositionClaims says why each is
// lost and not moved).
//
// The curtain and the counter binding are on the rows already in hand; a
// person's off is on the runtime row, one read for every removed position;
// a counter's name is one read of the process's counters, made only when a
// removed position is counted at one.
//
// A position the save finds an order still running at is kept, disabled,
// and loses nothing; this read cannot know that ahead of the save, and the
// save's reply names it in kept.
func (s *StationService) positionSettings(processID int64, removed []processes.Node, claims []domain.PositionClaims) error {
	ids := make([]int64, len(removed))
	counted := false
	for i, n := range removed {
		ids[i] = n.ID
		counted = counted || n.LineCounterID != nil
	}
	runtimes, err := s.db.ProcessNodeRuntimes(ids)
	if err != nil {
		return err
	}
	counterName := map[int64]string{}
	if counted {
		lcs, err := s.db.ListLineCounters(processID)
		if err != nil {
			return err
		}
		for _, lc := range lcs {
			counterName[lc.ID] = lc.Name()
		}
	}
	for i, n := range removed {
		c := &claims[i]
		c.Curtain = n.CurtainEnabled || n.CurtainPLCName != "" || n.CurtainTagName != ""
		if n.LineCounterID != nil {
			c.CountedAt = counterName[*n.LineCounterID]
			if c.CountedAt == "" {
				c.CountedAt = fmt.Sprintf("station counter %d", *n.LineCounterID)
			}
		}
		if rt := runtimes[n.ID]; rt != nil && rt.ReorderHold == domain.ReorderHoldPersonOff {
			c.ReorderOff = true
		}
	}
	return nil
}

// positionClaims is, for each position in removed, the recipes and styles of
// the process that name it — as a cell's own position or as its partner, the
// two ways a shape stands on a position.
func (s *StationService) positionClaims(processID int64, removed []string) ([]domain.PositionClaims, error) {
	presets, err := s.db.ListFlowPresets(processID, false)
	if err != nil {
		return nil, err
	}
	rows, err := processes.ListLiveClaimsByProcess(s.db.DB, processID)
	if err != nil {
		return nil, err
	}
	styles, err := s.db.ListStylesByProcess(processID)
	if err != nil {
		return nil, err
	}
	proc, err := s.db.GetProcess(processID)
	if err != nil {
		return nil, err
	}
	recipes := map[string]map[string]bool{}
	// The enabled recipe of each name: an older version is on no card, so it
	// is orphaned by nothing.
	latest, _ := domain.EnabledRecipes(presets)
	for _, p := range latest {
		cells, err := domain.ParsePresetShape(p.FlowJSON)
		if err != nil {
			continue // a recipe that does not parse names nothing the floor can run
		}
		for _, c := range cells {
			addNamer(recipes, p.Name, c.CoreNodeName, c.PairedCoreNode, c.SecondPairedCoreNode)
		}
	}
	styleName := map[int64]string{}
	active := ""
	for _, st := range styles {
		styleName[st.ID] = st.Name
		if proc.ActiveStyleID != nil && *proc.ActiveStyleID == st.ID {
			active = st.Name
		}
	}
	byStyle := map[string]map[string]bool{}
	for _, c := range rows {
		if name, live := styleName[c.StyleID]; live {
			addNamer(byStyle, name, c.CoreNodeName, c.PairedCoreNode, c.SecondPairedCoreNode)
		}
	}
	out := make([]domain.PositionClaims, 0, len(removed))
	for _, pos := range removed {
		pc := domain.PositionClaims{Position: pos, Recipes: setNames(recipes[pos]), Styles: setNames(byStyle[pos])}
		if byStyle[pos][active] {
			pc.Active = active
		}
		out = append(out, pc)
	}
	return out, nil
}

// addNamer records that namer names each non-blank position given.
func addNamer(by map[string]map[string]bool, namer string, positions ...string) {
	for _, p := range positions {
		if p == "" {
			continue
		}
		if by[p] == nil {
			by[p] = map[string]bool{}
		}
		by[p][namer] = true
	}
}

func setNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	return out
}
