package store

// migrations_curtain.go - Edge v6, the FG light-curtain interlock moves from
// the process to the node.
//
// The interlock shipped on four processes columns (curtain_enabled,
// curtain_plc_name, curtain_tag_name, curtain_safe_value). One tag per process
// cannot describe a cell with several screens: combined as "all muted" it holds
// a release at one screen until every screen is muted, and combined as "any
// muted" it lets a robot in past a live screen because another is bypassed. So
// the settings move to process_nodes, and the process columns go.
//
// THE INTERLOCK IS LIVE ON THE PROCESS COLUMNS AT A PLANT, so the move carries
// it rather than resetting it. The per-process gate checked the releases whose
// claim is a produce claim, so each process's settings land on every node of
// that process that has a produce claim in any live style - the same set of
// releases, now keyed where the screen is.
//
// Re-run safe, twice over: the copy writes only nodes whose curtain columns
// are still untouched, and once the process columns are dropped there is
// nothing left to copy from.

import (
	"database/sql"
	"fmt"
	"log"
)

// processCurtainColumns are the retired per-process columns, in the order the
// old ALTERs added them.
var processCurtainColumns = []string{"curtain_enabled", "curtain_plc_name", "curtain_tag_name", "curtain_safe_value"}

// nodeCurtainColumns are the node columns, with the baseline CREATE's
// definitions. curtain_safe_value is nullable on purpose: NULL is "polarity
// not chosen", and no default is safe.
var nodeCurtainColumns = []struct{ name, decl string }{
	{"curtain_enabled", "INTEGER NOT NULL DEFAULT 0"},
	{"curtain_plc_name", "TEXT NOT NULL DEFAULT ''"},
	{"curtain_tag_name", "TEXT NOT NULL DEFAULT ''"},
	{"curtain_safe_value", "INTEGER"},
}

// txHasColumn is schema.TableHasColumn inside the migration's transaction.
func txHasColumn(tx *sql.Tx, table, column string) (bool, error) {
	var n int
	err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('`+table+`') WHERE name = ?`, column).Scan(&n)
	return n > 0, err
}

// moveCurtainToNodes is Edge v6's Fn. It adds the node columns where they
// are missing, copies each process's curtain settings onto its produce nodes,
// and drops the process columns, all in the runner's one transaction. A
// failure rolls back every part, so the next boot finds the process columns
// intact and tries again - never a half-dropped table the copy can no longer
// read.
//
// WHICH PROCESSES COPY. One whose interlock is on, or whose PLC or tag pointer
// was filled in. A process nobody configured copies nothing: its
// curtain_safe_value is the old column DEFAULT 1, which nobody chose, and
// writing it onto a node would turn a default into what looks like a decision.
//
// WHAT A DISABLED PROCESS CARRIES. Its pointers, disabled, so switching the
// node on later is not a re-typing of the addresses. Not its polarity: the
// old settings sheet started every process at TRUE, so a stored TRUE on a
// process that was never switched on cannot be told apart from a value
// nobody tested, and TRUE is the inverted reading at the plant that first
// wired one. The node's polarity stays unset, and the setter makes whoever
// switches it on choose. An ENABLED process's polarity is the one the gate
// has been enforcing, and it is carried exactly.
func moveCurtainToNodes(tx *sql.Tx) error {
	for _, col := range nodeCurtainColumns {
		has, err := txHasColumn(tx, "process_nodes", col.name)
		if err != nil {
			return fmt.Errorf("curtain move: probe process_nodes.%s: %w", col.name, err)
		}
		if !has {
			if _, err := tx.Exec(`ALTER TABLE process_nodes ADD COLUMN ` + col.name + ` ` + col.decl); err != nil {
				return fmt.Errorf("curtain move: add process_nodes.%s: %w", col.name, err)
			}
		}
	}

	present := 0
	for _, col := range processCurtainColumns {
		has, err := txHasColumn(tx, "processes", col)
		if err != nil {
			return fmt.Errorf("curtain move: probe processes.%s: %w", col, err)
		}
		if has {
			present++
		}
	}
	if present == 0 {
		return nil // a fresh database, or one this already ran on
	}
	if present != len(processCurtainColumns) {
		// The old pass added all four together and this pass drops all four
		// together, so a partial set means something else touched the table.
		// Refuse rather than guess which settings survived.
		return fmt.Errorf("curtain move: processes carries %d of the %d curtain columns - refusing to copy a partial interlock configuration",
			present, len(processCurtainColumns))
	}

	type copyRow struct {
		nodeID             int64
		nodeName, coreNode string
		procID             int64
		procName           string
		enabled            bool
		plcName, tagName   string
		safe               sql.NullBool
	}
	rows, err := tx.Query(`
		SELECT n.id, n.name, n.core_node_name, p.id, p.name,
		       p.curtain_enabled, p.curtain_plc_name, p.curtain_tag_name, p.curtain_safe_value
		FROM process_nodes n
		JOIN processes p ON p.id = n.process_id
		WHERE (p.curtain_enabled = 1 OR p.curtain_plc_name <> '' OR p.curtain_tag_name <> '')
		  AND n.curtain_enabled = 0 AND n.curtain_plc_name = '' AND n.curtain_tag_name = ''
		  AND n.curtain_safe_value IS NULL
		  AND EXISTS (
		      SELECT 1 FROM style_node_claims c
		      JOIN styles s ON s.id = c.style_id
		      WHERE s.process_id = n.process_id
		        AND c.core_node_name = n.core_node_name
		        AND c.role = 'produce'
		        AND c.retired_at IS NULL
		        AND s.deleted_at IS NULL)
		ORDER BY p.id, n.id`)
	if err != nil {
		return fmt.Errorf("curtain move: read: %w", err)
	}
	var todo []copyRow
	for rows.Next() {
		var r copyRow
		if err := rows.Scan(&r.nodeID, &r.nodeName, &r.coreNode, &r.procID, &r.procName,
			&r.enabled, &r.plcName, &r.tagName, &r.safe); err != nil {
			rows.Close()
			return fmt.Errorf("curtain move: scan: %w", err)
		}
		todo = append(todo, r)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("curtain move: read: %w", err)
	}

	// Logged as the rows are written. The runner commits this transaction
	// after Fn returns; if the commit fails, the boot fails with it, so a
	// logged copy is never silently undone.
	for _, r := range todo {
		safe := sql.NullBool{}
		if r.enabled {
			safe = r.safe
		}
		if _, err := tx.Exec(`UPDATE process_nodes SET curtain_enabled=?, curtain_plc_name=?, curtain_tag_name=?,
			curtain_safe_value=? WHERE id=?`, r.enabled, r.plcName, r.tagName, safe, r.nodeID); err != nil {
			return fmt.Errorf("curtain move: node %d: %w", r.nodeID, err)
		}
		polarity := "unset"
		if safe.Valid {
			polarity = "FALSE"
			if safe.Bool {
				polarity = "TRUE"
			}
		}
		log.Printf("migrate: curtain interlock copied from process %d (%s) to node %d (%s, core node %s): enabled=%t plc=%q tag=%q release value=%s",
			r.procID, r.procName, r.nodeID, r.nodeName, r.coreNode, r.enabled, r.plcName, r.tagName, polarity)
	}

	for _, col := range processCurtainColumns {
		if _, err := tx.Exec(`ALTER TABLE processes DROP COLUMN ` + col); err != nil {
			return fmt.Errorf("curtain move: drop processes.%s: %w", col, err)
		}
	}
	return nil
}
