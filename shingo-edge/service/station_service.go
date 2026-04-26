package service

import (
	"strings"

	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// StationService owns operator-station CRUD and the two cross-aggregate
// orchestrations that span stations + processes + orders + lineside:
// SetNodes (sync process_nodes for a station) and BuildView (the
// operator HMI projection). Phase 6.1 introduced the cross-aggregate
// methods; Phase 6.2′ folded in the per-station CRUD that previously
// sat as named methods on *engine.Engine.
type StationService struct {
	db *store.DB
}

// NewStationService constructs a StationService wrapping the shared
// *store.DB.
func NewStationService(db *store.DB) *StationService {
	return &StationService{db: db}
}

// ── Cross-aggregate orchestrations ──────────────────────────────────

// SetNodes syncs process_nodes for a station to match the given core
// node names. Cross-aggregate: stations + processes + orders. Nodes
// with active orders are disabled rather than deleted to preserve
// referential integrity for downstream telemetry.
//
// Phase 6.1 introduced this method as a thin delegate; Phase 6.4a
// moved the body in from the (now-deleted) outer
// store/station_nodes.go::SetStationNodes.
func (s *StationService) SetNodes(stationID int64, nodeNames []string) error {
	station, err := s.db.GetOperatorStation(stationID)
	if err != nil {
		return err
	}

	existing, err := s.db.ListProcessNodesByStation(stationID)
	if err != nil {
		return err
	}

	existingMap := map[string]processes.Node{}
	for _, n := range existing {
		existingMap[n.CoreNodeName] = n
	}

	// Normalize input: trim and deduplicate, preserving order.
	clean := make([]string, 0, len(nodeNames))
	desired := map[string]bool{}
	for _, name := range nodeNames {
		name = strings.TrimSpace(name)
		if name != "" && !desired[name] {
			desired[name] = true
			clean = append(clean, name)
		}
	}

	for i, name := range clean {
		if _, exists := existingMap[name]; exists {
			if _, err := s.db.Exec(`UPDATE process_nodes SET sequence=?, enabled=1, updated_at=datetime('now')
				WHERE operator_station_id=? AND core_node_name=?`, i+1, stationID, name); err != nil {
				return err
			}
			continue
		}
		id, err := s.db.CreateProcessNode(processes.NodeInput{
			ProcessID:         station.ProcessID,
			OperatorStationID: &stationID,
			CoreNodeName:      name,
			Name:              name,
			Sequence:          i + 1,
			Enabled:           true,
		})
		if err != nil {
			return err
		}
		if _, err := s.db.EnsureProcessNodeRuntime(id); err != nil {
			return err
		}
	}

	for _, n := range existing {
		if desired[n.CoreNodeName] {
			continue
		}
		active, err := s.db.ListActiveOrdersByProcessNode(n.ID)
		if err != nil {
			return err
		}
		if len(active) > 0 {
			if _, err := s.db.Exec(`UPDATE process_nodes SET enabled=0, updated_at=datetime('now') WHERE id=?`, n.ID); err != nil {
				return err
			}
			continue
		}
		if err := s.db.DeleteProcessNode(n.ID); err != nil {
			return err
		}
	}

	return nil
}

// BuildView returns the operator station view used by the operator
// HMI. Cross-aggregate: stations + processes + claims + lineside.
//
// Phase 6.1 introduced this method as a thin delegate; Phase 6.4a
// moved the body in from the (now-deleted) outer
// store/station_views.go::BuildOperatorStationView. The two helpers
// (ComputeSwapReady, LookupLastReleaseError) stay in store/ so the
// existing test file station_views_test.go can continue exercising
// them directly without import-cycle gymnastics.
func (s *StationService) BuildView(stationID int64) (*store.OperatorStationView, error) {
	station, err := s.db.GetOperatorStation(stationID)
	if err != nil {
		return nil, err
	}
	process, err := s.db.GetProcess(station.ProcessID)
	if err != nil {
		return nil, err
	}

	view := &store.OperatorStationView{
		Station: *station,
		Process: *process,
	}
	if process.ActiveStyleID != nil {
		if st, err := s.db.GetStyle(*process.ActiveStyleID); err == nil {
			view.CurrentStyle = st
		}
	}
	if process.TargetStyleID != nil {
		if st, err := s.db.GetStyle(*process.TargetStyleID); err == nil {
			view.TargetStyle = st
		}
	}
	view.AvailableStyles, _ = s.db.ListStylesByProcess(process.ID)
	if co, err := s.db.GetActiveProcessChangeover(process.ID); err == nil {
		view.ActiveChangeover = co
		if stationTask, err := s.db.GetChangeoverStationTaskByStation(co.ID, stationID); err == nil {
			view.StationTask = stationTask
		}
	}

	nodes, err := s.db.ListProcessNodesByStation(stationID)
	if err != nil {
		return nil, err
	}
	nodeTaskMap := map[int64]processes.NodeTask{}
	if view.StationTask != nil {
		nodeTasks, _ := s.db.ListChangeoverNodeTasksByStation(view.ActiveChangeover.ID, stationID)
		for _, nodeTask := range nodeTasks {
			nodeTaskMap[nodeTask.ProcessNodeID] = nodeTask
		}
	}
	for _, node := range nodes {
		nodeView := store.StationNodeView{Node: node}
		runtime, _ := s.db.EnsureProcessNodeRuntime(node.ID)
		nodeView.Runtime = runtime
		if process.ActiveStyleID != nil && node.CoreNodeName != "" {
			nodeView.ActiveClaim, _ = s.db.GetStyleNodeClaimByNode(*process.ActiveStyleID, node.CoreNodeName)
		}
		if process.TargetStyleID != nil && node.CoreNodeName != "" {
			nodeView.TargetClaim, _ = s.db.GetStyleNodeClaimByNode(*process.TargetStyleID, node.CoreNodeName)
		}
		if nodeTask, ok := nodeTaskMap[node.ID]; ok {
			taskCopy := nodeTask
			nodeView.ChangeoverTask = &taskCopy
		}
		nodeView.Orders, _ = s.db.ListActiveOrdersByProcessNode(node.ID)
		nodeView.SwapReady = store.ComputeSwapReady(s.db, nodeView.ActiveClaim, runtime)
		// Lineside buckets power the active-bar and stranded-chip UI on
		// the operator station modal. Best-effort — absence of buckets
		// just means the node has nothing pulled to lineside yet.
		nodeView.LinesideActive, _ = s.db.ListActiveLinesideBuckets(node.ID)
		nodeView.LinesideInactive, _ = s.db.ListInactiveLinesideBuckets(node.ID)
		// Surface any pending release-time error that's been rolled back to
		// Staged for the operator to retry.
		nodeView.LastReleaseError = store.LookupLastReleaseError(s.db, runtime)
		view.Nodes = append(view.Nodes, nodeView)
	}
	return view, nil
}

// ── Per-station CRUD ────────────────────────────────────────────────

// List returns every operator_stations row.
func (s *StationService) List() ([]stations.Station, error) {
	return s.db.ListOperatorStations()
}

// ListByProcess returns operator_stations rows for one process.
func (s *StationService) ListByProcess(processID int64) ([]stations.Station, error) {
	return s.db.ListOperatorStationsByProcess(processID)
}

// Get returns one operator_station by id.
func (s *StationService) Get(id int64) (*stations.Station, error) {
	return s.db.GetOperatorStation(id)
}

// Create inserts a station, generating code and sequence when not
// supplied.
func (s *StationService) Create(in stations.Input) (int64, error) {
	return s.db.CreateOperatorStation(in)
}

// Update modifies an operator_station.
func (s *StationService) Update(id int64, in stations.Input) error {
	return s.db.UpdateOperatorStation(id, in)
}

// Delete removes an operator_station.
func (s *StationService) Delete(id int64) error {
	return s.db.DeleteOperatorStation(id)
}

// Touch updates last_seen_at and health_status.
func (s *StationService) Touch(id int64, healthStatus string) error {
	return s.db.TouchOperatorStation(id, healthStatus)
}

// Move swaps the sequence of a station with its neighbour in the
// given direction ("up" or "down").
func (s *StationService) Move(id int64, direction string) error {
	return s.db.MoveOperatorStation(id, direction)
}

// GetNodeNames returns the core_node_name list for a station.
func (s *StationService) GetNodeNames(stationID int64) ([]string, error) {
	return s.db.GetStationNodeNames(stationID)
}
