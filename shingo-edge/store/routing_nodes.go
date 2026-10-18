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
// per-role style counts filled in. Unused: the panel reads RoutingSet, which
// makes the same two reads.
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

// RoutingSet is the Routing tab's answer: the process's rows, each with its
// live-style count. The process must exist (a missing one is the GET's 404).
func (db *DB) RoutingSet(processID int64) ([]processes.RoutingNode, error) {
	if _, err := processes.Get(db.DB, processID); err != nil {
		return nil, err
	}
	return processes.RoutingSet(db.DB, processID)
}

// PutRoutingNodes writes a whole list of (name, role) rows in the order given,
// adopting rows that already exist and NEVER deleting one the list omits.
//
// ADD-AND-ADOPT, NOT A SET-TO, and the reason is the delete guard. Taking a
// routing name out is refused while a live claim still routes through it
// (ErrRoutingNodeInUse) — a refusal an engineer is meant to see, by name, so
// they can move the flow first. A set-to could only either fail the whole
// write for a row nobody touched or drop a row a flow depends on. Add Process
// only ever adds; the Edit sheet keeps its diff, where a removal is a DELETE.
//
// THE INDEX IS THE SEQUENCE. composer-model's defaultRouting opens a new
// position on the LOWEST sequence of each role, so the order the engineer
// picked names in is the order that decides their defaults.
//
// kinds is Core's lane set; a row naming a lane is refused by the store.
func (db *DB) PutRoutingNodes(kinds domain.CoreNodeKinds, processID int64, rows []domain.RoutingNodeInput, calledBy string) error {
	seq := map[string]int{}
	for i := range rows {
		rows[i].ProcessID = processID
		rows[i].Origin = domain.RoutingOriginEngineer
		rows[i].CalledBy = calledBy
		rows[i].Enabled = true
		rows[i].Sequence = seq[rows[i].Role]
		seq[rows[i].Role]++
	}
	// NAMING A ROW IN THIS LIST IS ADOPTING IT, and the upsert already says so:
	// its ON CONFLICT writes enabled, origin and called_by from the input, so a
	// backfilled row named here comes back on, stamped 'engineer', with the
	// session user on it — the same three columns SetRoutingNodeEnabled would
	// have written, in the statement that is already running. A second write
	// per row would be a second statement on the one SQLite connection for a
	// value the first one just set.
	for _, in := range rows {
		if _, err := db.UpsertRoutingNode(kinds, in); err != nil {
			return err
		}
	}
	return nil
}
