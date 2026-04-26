package service

import (
	"fmt"

	"shingoedge/store"
	"shingoedge/store/processes"
)

// ChangeoverService owns the cross-aggregate changeover orchestration
// (Create — spans processes + changeovers + style_node_claims +
// process_nodes + process_node_runtime_states) plus the changeover
// query surface used by handlers and engine code.
//
// Phase 6.1 introduced Create; Phase 6.2′ added the read methods that
// previously sat as named *engine.Engine passthroughs. State-mutation
// methods on changeover/station-task/node-task rows are still called
// directly from engine business logic via *store.DB and don't go
// through this service — they're internal orchestration plumbing,
// not handler-facing.
type ChangeoverService struct {
	db *store.DB
}

// NewChangeoverService constructs a ChangeoverService wrapping the
// shared *store.DB.
func NewChangeoverService(db *store.DB) *ChangeoverService {
	return &ChangeoverService{db: db}
}

// Create atomically creates a changeover with its station and node
// tasks. Cross-aggregate: also flips the owning process into the
// changeover state (target_style_id + production_state) and backfills
// process_nodes / process_node_runtime_states for any core nodes that
// didn't have a row yet. Returns the new changeover id.
//
// Phase 6.1 introduced this method as a thin delegate; Phase 6.4a
// moved the transaction body in from the (now-deleted) outer
// store/process_changeovers.go::CreateChangeover.
func (s *ChangeoverService) Create(processID int64, fromStyleID *int64, toStyleID int64, calledBy, notes string,
	stationIDs []int64, nodeTasks []processes.NodeTaskInput, existingNodes []processes.Node) (int64, error) {

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`INSERT INTO process_changeovers (process_id, from_style_id, to_style_id, state, called_by, notes)
		VALUES (?, ?, ?, 'active', ?, ?)`, processID, fromStyleID, toStyleID, calledBy, notes)
	if err != nil {
		return 0, err
	}
	changeoverID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE processes SET target_style_id=? WHERE id=?`, toStyleID, processID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE processes SET production_state='changeover_active' WHERE id=?`, processID); err != nil {
		return 0, err
	}

	for _, sid := range stationIDs {
		if _, err := tx.Exec(`INSERT INTO changeover_station_tasks (
			process_changeover_id, operator_station_id, state
		) VALUES (?, ?, 'waiting')`, changeoverID, sid); err != nil {
			return 0, err
		}
	}

	for _, nt := range nodeTasks {
		// Find existing process node by core_node_name.
		var processNodeID *int64
		for i := range existingNodes {
			if existingNodes[i].CoreNodeName == nt.CoreNodeName {
				id := existingNodes[i].ID
				processNodeID = &id
				break
			}
		}
		if processNodeID == nil {
			// Auto-create process node for this claimed core node.
			res, err := tx.Exec(`INSERT INTO process_nodes (process_id, core_node_name, code, name) VALUES (?, ?, ?, ?)`,
				nt.ProcessID, nt.CoreNodeName, nt.CoreNodeName, nt.CoreNodeName)
			if err != nil {
				return 0, fmt.Errorf("auto-create process node for %s: %w", nt.CoreNodeName, err)
			}
			id, _ := res.LastInsertId()
			processNodeID = &id
		}

		if _, err := tx.Exec(`INSERT INTO changeover_node_tasks (
			process_changeover_id, process_node_id, from_claim_id, to_claim_id, situation, state
		) VALUES (?, ?, ?, ?, ?, ?)`, changeoverID, *processNodeID, nt.FromClaimID, nt.ToClaimID, nt.Situation, nt.State); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO process_node_runtime_states (process_node_id) VALUES (?)`, *processNodeID); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changeoverID, nil
}

// GetActive returns the active (non-completed, non-cancelled)
// changeover for a process, if any.
func (s *ChangeoverService) GetActive(processID int64) (*processes.Changeover, error) {
	return s.db.GetActiveProcessChangeover(processID)
}

// List returns every process_changeover for a process, newest first.
func (s *ChangeoverService) List(processID int64) ([]processes.Changeover, error) {
	return s.db.ListProcessChangeovers(processID)
}

// ListStationTasks returns every changeover_station_task for one
// changeover.
func (s *ChangeoverService) ListStationTasks(changeoverID int64) ([]processes.StationTask, error) {
	return s.db.ListChangeoverStationTasks(changeoverID)
}

// ListNodeTasks returns every changeover_node_task for a changeover.
func (s *ChangeoverService) ListNodeTasks(changeoverID int64) ([]processes.NodeTask, error) {
	return s.db.ListChangeoverNodeTasks(changeoverID)
}
