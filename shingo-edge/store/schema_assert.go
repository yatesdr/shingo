package store

import (
	"fmt"
	"strings"

	"shingoedge/store/schema"
)

// Schema verification — the backstop for the ignored-error ALTER pattern.
//
// migrate() adds columns with `db.Exec("ALTER TABLE ... ADD COLUMN ...")` and
// deliberately discards the error, because re-adding an existing column is the
// normal idempotent path on every already-migrated database and SQLite gives no
// cheap way to tell that apart from a real failure. The cost of that trade is
// that a genuine failure looks exactly like a successful no-op, and migrate()
// returns nil either way.
//
// This is the backstop. After migrate() runs, assert that the objects the
// CURRENT build actually depends on are present, and fail startup loudly —
// listing every missing object at once — if they are not.
//
// It also catches a failure mode that has nothing to do with SQL. On
// 2026-07-20 a Springfield deploy left the OLD edge binary running while the
// new one sat on disk: systemd's Restart=always relaunched the service ~1s
// before the installer swapped the binary. Every health signal looked correct
// (service active, HMI 200, Kafka connected, zero errors) and the mismatch was
// only found by querying the schema by hand. A stale binary migrates to its
// own older schema, so its result cannot satisfy this manifest — the assertion
// turns that silent condition into a startup failure that names the gap.
//
// POLICY: when you add a migration that the code then depends on, add it here.
// Keep this list to UNCONDITIONAL migrations — the ones that run on every
// database. Migrations guarded by a conditional (legacy-shape fixups, one-time
// data repairs) must NOT be listed: they legitimately do not apply everywhere,
// and asserting them would refuse to start an otherwise healthy plant.

// requiredTables are tables every migrated edge database must have.
var requiredTables = []string{
	"orders",
	"processes",
	"styles",
	"style_node_claims",
	"process_changeovers",
	"loader_payload_thresholds",
	"changeover_node_tasks",
	"process_node_runtime_states",
	"sourcing_state",
}

// requiredColumn is one (table, column) pair added by an unconditional
// top-level migration in migrate().
type requiredColumn struct{ table, column string }

var requiredColumns = []requiredColumn{
	{"orders", "payload_code"},
	{"orders", "sibling_order_id"},
	{"orders", "queue_reason"},
	{"orders", "queue_code"},
	{"style_node_claims", "mode"},
	{"style_node_claims", "lineside_soft_threshold"},
	{"style_node_claims", "second_paired_core_node"},
	{"style_node_claims", "reuse_compatible_bins"},
	{"style_node_claims", "auto_push"},
	{"style_node_claims", "reorder_point_source"},
	{"processes", "auto_cutover_enabled"},
	{"process_changeovers", "triggered_by"},
	{"loader_payload_thresholds", "overridden_inputs"},
	{"changeover_node_tasks", "skip_note"},
	{"process_node_runtime_states", "remaining_uop_cached"},
}

// verifySchema reports every required table and column that is missing. It
// collects the full list rather than failing on the first gap: when this fires
// during a deploy, the operator needs to see the whole picture at once to tell
// a stale binary (many objects missing) from a single failed migration (one).
func (db *DB) verifySchema() error {
	var missing []string

	for _, t := range requiredTables {
		ok, err := schema.TableExists(db.DB, t)
		if err != nil {
			return fmt.Errorf("schema verify: checking table %s: %w", t, err)
		}
		if !ok {
			missing = append(missing, "table  "+t)
		}
	}

	for _, rc := range requiredColumns {
		// A missing table already reported above also reports (false, nil)
		// here; that is fine, both facts are useful in the failure output.
		ok, err := schema.TableHasColumn(db.DB, rc.table, rc.column)
		if err != nil {
			return fmt.Errorf("schema verify: checking %s.%s: %w", rc.table, rc.column, err)
		}
		if !ok {
			missing = append(missing, "column "+rc.table+"."+rc.column)
		}
	}

	if len(missing) == 0 {
		return nil
	}

	return fmt.Errorf(
		"schema verification failed — %d required object(s) missing after migration:\n  %s\n\n"+
			"This means the database does not match the code that is running.\n"+
			"Most common cause: the service is running an OLD binary (a deploy that\n"+
			"swapped the file but never restarted the process onto it). Check with:\n"+
			"  readlink /proc/$(systemctl show shingo-edge -p MainPID --value)/exe\n"+
			"A trailing \"(deleted)\" there confirms it; `systemctl restart shingo-edge` fixes it.",
		len(missing), strings.Join(missing, "\n  "))
}
