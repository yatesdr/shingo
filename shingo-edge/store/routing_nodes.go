package store

// The process routing set lives in store/processes/; these keep the *store.DB
// method surface the services and tests use. PutRoutingNodes is the set's one
// transaction.

import (
	"database/sql"
	"errors"
	"strings"

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

// DeleteRoutingNode removes one row and says what it was and which live styles
// still name it.
func (db *DB) DeleteRoutingNode(processID, id int64) (name, role string, styles []string, err error) {
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

// PutRoutingNodes is the routing set's one write: it adds or adopts every row
// of rows, in the order given, and takes out every row of remove (by name and
// role), in ONE transaction. All of it lands or none of it does: a row refused
// halfway (a lane, or a name that is one of the process's own positions)
// leaves the set as it was, removals included. It returns what each removal
// leaves behind; a removal of a row that is not there is nothing to do.
//
// ADD-AND-ADOPT PLUS NAMED REMOVALS, NOT A SET-TO, because a removal warns
// what it orphans, by name. A set-to would drop a row nobody touched and warn
// about a name the engineer never asked to take out.
//
// THE INDEX IS THE SEQUENCE. composer-model's defaultRouting opens a new
// position on the LOWEST sequence of each role, so the order the engineer
// picked names in is the order that decides their defaults.
//
// kinds is Core's lane set; a row naming a lane is refused by the store.
func (db *DB) PutRoutingNodes(kinds domain.CoreNodeKinds, processID int64, rows, remove []domain.RoutingNodeInput, calledBy string) ([]processes.RoutingDeletion, error) {
	seq := map[string]int{}
	for i := range rows {
		rows[i].ProcessID = processID
		rows[i].Origin = domain.RoutingOriginEngineer
		rows[i].CalledBy = calledBy
		rows[i].Enabled = true
		rows[i].Sequence = seq[rows[i].Role]
		seq[rows[i].Role]++
	}
	var gone []processes.RoutingDeletion
	err := db.InTx(func(tx *sql.Tx) error {
		// NAMING A ROW IN THIS LIST IS ADOPTING IT, and the upsert already says
		// so: its ON CONFLICT writes enabled, origin and called_by from the
		// input, so a backfilled row named here comes back on, stamped
		// 'engineer', with the session user on it.
		for _, in := range rows {
			if _, err := processes.UpsertRoutingNode(tx, kinds, in); err != nil {
				return err
			}
		}
		for _, r := range remove {
			var id int64
			switch err := tx.QueryRow(`SELECT id FROM process_routing_nodes WHERE process_id = ? AND core_node_name = ? AND role = ?`,
				processID, strings.TrimSpace(r.CoreNodeName), r.Role).Scan(&id); {
			case errors.Is(err, sql.ErrNoRows):
				continue
			case err != nil:
				return err
			}
			name, role, styles, err := processes.DeleteRoutingNode(tx, processID, id)
			if err != nil {
				return err
			}
			gone = append(gone, processes.RoutingDeletion{Name: name, Role: role, Styles: styles})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return gone, nil
}
