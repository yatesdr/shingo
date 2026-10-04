package store

// Delegate file: the process routing set lives in store/processes/. This
// preserves the *store.DB method surface the services and tests use.

import (
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// ListRoutingNodes returns a process's routing set in source / staging /
// destination order, without style counts.
func (db *DB) ListRoutingNodes(processID int64) ([]processes.RoutingNode, error) {
	return processes.ListRoutingNodes(db.DB, processID)
}

// ListRoutingNodesWithCounts is ListRoutingNodes with the Routing panel's
// per-role style counts filled in. The panel's read, and only the panel's.
func (db *DB) ListRoutingNodesWithCounts(processID int64) ([]processes.RoutingNode, error) {
	return processes.ListRoutingNodesWithCounts(db.DB, processID)
}

// UpsertRoutingNode inserts or updates one (process, node, role) row,
// refusing a lane (kinds, Core's lane set).
func (db *DB) UpsertRoutingNode(kinds domain.CoreNodeKinds, in processes.RoutingNodeInput) (int64, error) {
	return processes.UpsertRoutingNode(db.DB, kinds, in)
}

// SetRoutingNodeEnabled flips one row's enabled flag (adopt = true),
// refusing to switch a lane on.
func (db *DB) SetRoutingNodeEnabled(kinds domain.CoreNodeKinds, processID, id int64, enabled bool, calledBy string) error {
	return processes.SetRoutingNodeEnabled(db.DB, kinds, processID, id, enabled, calledBy)
}

// DeleteRoutingNode removes one row, refusing while a live claim names it.
func (db *DB) DeleteRoutingNode(processID, id int64) error {
	return processes.DeleteRoutingNode(db.DB, processID, id)
}

// DeriveRoutingNodes runs the routing-set backfill for every process whose
// flow composer is still off. Called once at boot, after migrate. A claim
// leg naming a lane in kinds is skipped and logged.
func (db *DB) DeriveRoutingNodes(kinds domain.CoreNodeKinds, isUnknown func(name string) bool) ([]processes.RoutingDeriveReport, error) {
	return processes.DeriveRoutingNodes(db.DB, kinds, isUnknown)
}

// DeriveRoutingNodesForProcess re-derives one process's routing set (a no-op
// once its flow composer is enabled), skipping a lane in kinds.
func (db *DB) DeriveRoutingNodesForProcess(kinds domain.CoreNodeKinds, processID int64, isUnknown func(name string) bool) (processes.RoutingDeriveReport, error) {
	return processes.DeriveRoutingNodesForProcess(db.DB, kinds, processID, isUnknown)
}

// RoutingSetReport counts a process's routing set without deriving.
func (db *DB) RoutingSetReport(processID int64, isUnknown func(name string) bool) (processes.RoutingDeriveReport, error) {
	rep, _, err := db.RoutingSet(processID, isUnknown)
	return rep, err
}

// RoutingSet is the Routing tab's whole answer in one pass: the rows, and the
// report that describes them.
//
// GET …/routing-nodes used to ask for the report and then ask for the list,
// and the report re-read the same table the list was about — see
// processes.RoutingSetReportFrom (since deleted). The route reads process, rows and the live
// claim count now, and nothing else.
func (db *DB) RoutingSet(processID int64, isUnknown func(name string) bool) (processes.RoutingDeriveReport, []processes.RoutingNode, error) {
	p, err := processes.Get(db.DB, processID)
	if err != nil {
		return processes.RoutingDeriveReport{}, nil, err
	}
	return processes.RoutingSet(db.DB, *p, isUnknown)
}
