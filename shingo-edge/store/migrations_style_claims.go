package store

import (
	"database/sql"
	"fmt"

	"shingoedge/store/schema"
)

// rebuildStyleNodeClaims normalises style_node_claims on databases that predate
// the current column defaults.
//
// THREE CHANGES, ONE REBUILD. SQLite has no ALTER COLUMN DROP DEFAULT, so each
// of these needs the table recreated — and rebuilding the same table twice in a
// week is how a plant deploy goes wrong:
//
//	auto_reorder  DEFAULT 1 -> none. b8836528 changed the baseline ("stop the
//	              claim editor force-arming auto_reorder on every save") and the
//	              change reached NO plant, because CREATE TABLE IF NOT EXISTS
//	              no-ops on a table that already exists. Every plant edge in the
//	              field still defaults a new claim's autoreorder to ON — on the
//	              column behind the 2026-07-21 cascade.
//	swap_mode     DEFAULT 'simple' -> none. Same shape, older.
//	below_reorder_since  added by the ALTER in migrate(); the rebuild carries it
//	              so the fresh and upgraded paths land on ONE shape.
//
// Both defaults are INERT today — every writer (UpsertStyleNodeClaim,
// cloneClaimColumns, seed_edge) names its column — so nothing behaves
// differently after this runs. What changes is that a hand-written INSERT that
// omits auto_reorder now lands OFF, which is the value a claim nobody armed
// should have.
//
// FOUND BY TestSchemaConvergesAcrossVintages, which is also what tells you it
// worked: the two entries recorded for these in known.go are deleted in the
// same change, so if the rebuild silently does not take, the test goes red
// rather than the allowlist quietly covering for it.
//
// PRAGMA legacy_alter_table IS LOAD-BEARING, not a precaution. Three columns
// across two tables carry REFERENCES style_node_claims(id) —
// changeover_node_tasks.from_claim_id / .to_claim_id and
// process_node_runtime_states.active_claim_id. Since SQLite 3.25 an
// ALTER TABLE ... RENAME rewrites those clauses in the OTHER tables' stored
// schemas, so the rename would silently re-point all three at
// style_node_claims_legacy and the DROP below would leave them dangling
// (invisibly, because Edge runs with foreign_keys OFF — see store.Open). Legacy
// mode restores the old behaviour: rename the table, touch nothing else, which
// is exactly what a rename/create/copy/drop rebuild needs.
//
// Idempotent: it runs only while a default is still present, and after it runs
// none is.
func (db *DB) rebuildStyleNodeClaims() error {
	// demand_episodes was this table's name for exactly one commit on this
	// branch, before it was renamed to demand_origins_open so the two services
	// use ONE noun for one concept. Nothing was ever deployed carrying it, but
	// the branch was pushed, so anyone who ran it has an empty stray table.
	// One line, and the alternative is a name nobody can explain.
	db.Exec(`DROP TABLE IF EXISTS demand_episodes`)

	// demand_origins_open was four columns for two commits on this branch,
	// before the seam changed from event pairs to STATE TRANSFER and the table
	// had to be able to assemble its own close message. Nothing is deployed, so
	// the honest fix is to drop and let schema.Apply rebuild the current shape
	// rather than write an ALTER chain for a shape that never reached a plant.
	// The table holds only OPEN episodes, so there is nothing to preserve.
	if hasNarrowDemandOrigins(db.DB) {
		db.Exec(`DROP TABLE IF EXISTS demand_origins_open`)
	}

	present, err := schema.TableExists(db.DB, "style_node_claims")
	if err != nil || !present {
		return err
	}
	// The target is not "no default" — it is the shape sqlite_ddl.go declares:
	// auto_reorder keeps DEFAULT 0 (a claim nobody armed lands OFF), and
	// swap_mode has none. Branching on PRESENCE rather than VALUE would find
	// auto_reorder's DEFAULT 0 on a correct database and rebuild the table on
	// every single Edge startup.
	autoDflt, _, err := schema.ColumnDefault(db.DB, "style_node_claims", "auto_reorder")
	if err != nil {
		return err
	}
	_, swapHasDflt, err := schema.ColumnDefault(db.DB, "style_node_claims", "swap_mode")
	if err != nil {
		return err
	}
	if autoDflt != "1" && !swapHasDflt {
		return nil // already the current shape: a fresh install, or already run
	}

	// Make every column the copy names exist before naming it.
	//
	// The INSERT ... SELECT below enumerates the full column list, so a single
	// column missing from THIS database's table fails the whole rebuild — and
	// migrate() would return the error, so Edge would not start. Not
	// hypothetical: the first run of the rebuild test died on `sequence`,
	// because some columns only ever arrived through the baseline CREATE, which
	// is a no-op on a table that already exists. Which columns a given plant
	// has therefore depends on when it was installed, which is the whole reason
	// this class of bug exists.
	//
	// Duplicate ADD COLUMN fails silently in SQLite and is ignored here, the
	// same convention migrate() uses throughout. Defaults are only needed to
	// satisfy NOT NULL on a populated table — the rebuild's own CREATE is what
	// decides the final shape.
	for _, ddl := range styleNodeClaimsColumnGuards {
		db.Exec(ddl)
	}

	if _, err := db.Exec(`PRAGMA legacy_alter_table = ON`); err != nil {
		return fmt.Errorf("style_node_claims rebuild: enable legacy_alter_table: %w", err)
	}
	// Restored whatever happens below. Leaving it on would change the behaviour
	// of every later ALTER in the process.
	defer db.Exec(`PRAGMA legacy_alter_table = OFF`)

	// One statement string in one call: a failure part-way cannot leave the
	// database with the legacy table renamed and no replacement.
	if _, err := db.Exec(styleNodeClaimsRebuildSQL); err != nil {
		return fmt.Errorf("style_node_claims rebuild: %w", err)
	}
	return nil
}

// styleNodeClaimsColumnGuards makes the copy's column list safe to name on a
// database of any age. One entry per column the rebuild copies, except id /
// style_id / core_node_name, which every version of this table has had.
var styleNodeClaimsColumnGuards = []string{
	`ALTER TABLE style_node_claims ADD COLUMN role TEXT NOT NULL DEFAULT 'consume'`,
	`ALTER TABLE style_node_claims ADD COLUMN swap_mode TEXT NOT NULL DEFAULT 'simple'`,
	`ALTER TABLE style_node_claims ADD COLUMN payload_code TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE style_node_claims ADD COLUMN uop_capacity INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE style_node_claims ADD COLUMN reorder_point INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE style_node_claims ADD COLUMN auto_reorder INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE style_node_claims ADD COLUMN inbound_staging TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE style_node_claims ADD COLUMN outbound_staging TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE style_node_claims ADD COLUMN inbound_source TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE style_node_claims ADD COLUMN outbound_destination TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE style_node_claims ADD COLUMN allowed_payload_codes TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE style_node_claims ADD COLUMN auto_request_payload TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE style_node_claims ADD COLUMN keep_staged INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE style_node_claims ADD COLUMN evacuate_on_changeover INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE style_node_claims ADD COLUMN paired_core_node TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE style_node_claims ADD COLUMN auto_confirm INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE style_node_claims ADD COLUMN sequence INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE style_node_claims ADD COLUMN lineside_soft_threshold INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE style_node_claims ADD COLUMN reuse_compatible_bins INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE style_node_claims ADD COLUMN auto_push INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE style_node_claims ADD COLUMN reorder_point_source TEXT NOT NULL DEFAULT 'legacy'`,
	`ALTER TABLE style_node_claims ADD COLUMN below_reorder_since TEXT`,
	`ALTER TABLE style_node_claims ADD COLUMN created_at TEXT NOT NULL DEFAULT (datetime('now'))`,
	`ALTER TABLE style_node_claims ADD COLUMN staging_node TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE style_node_claims ADD COLUMN release_node TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE style_node_claims ADD COLUMN inbound_source_node TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE style_node_claims ADD COLUMN inbound_source_node_group TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE style_node_claims ADD COLUMN outbound_source_node TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE style_node_claims ADD COLUMN outbound_source_node_group TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE style_node_claims ADD COLUMN outbound_source TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE style_node_claims ADD COLUMN mode TEXT NOT NULL DEFAULT 'loader'`,
	`ALTER TABLE style_node_claims ADD COLUMN second_paired_core_node TEXT NOT NULL DEFAULT ''`,
}

// styleNodeClaimsRebuildSQL must produce the SAME shape as sqlite_ddl.go's
// CREATE TABLE for this table. TestSchemaConvergesAcrossVintages is what holds
// the two together: it builds a database each way and compares them.
const styleNodeClaimsRebuildSQL = `
ALTER TABLE style_node_claims RENAME TO style_node_claims_legacy;
CREATE TABLE style_node_claims (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    style_id                INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    core_node_name          TEXT NOT NULL,
    role                    TEXT NOT NULL DEFAULT 'consume',
    swap_mode               TEXT NOT NULL,
    payload_code            TEXT NOT NULL DEFAULT '',
    uop_capacity            INTEGER NOT NULL DEFAULT 0,
    reorder_point           INTEGER NOT NULL DEFAULT 0,
    auto_reorder            INTEGER NOT NULL DEFAULT 0,
    inbound_staging         TEXT NOT NULL DEFAULT '',
    outbound_staging        TEXT NOT NULL DEFAULT '',
    inbound_source          TEXT NOT NULL DEFAULT '',
    outbound_destination    TEXT NOT NULL DEFAULT '',
    allowed_payload_codes   TEXT NOT NULL DEFAULT '',
    auto_request_payload    TEXT NOT NULL DEFAULT '',
    keep_staged             INTEGER NOT NULL DEFAULT 0,
    evacuate_on_changeover  INTEGER NOT NULL DEFAULT 0,
    paired_core_node        TEXT NOT NULL DEFAULT '',
    auto_confirm            INTEGER NOT NULL DEFAULT 0,
    sequence                INTEGER NOT NULL DEFAULT 0,
    lineside_soft_threshold INTEGER NOT NULL DEFAULT 0,
    reuse_compatible_bins   INTEGER NOT NULL DEFAULT 0,
    auto_push               INTEGER NOT NULL DEFAULT 0,
    reorder_point_source    TEXT NOT NULL DEFAULT 'legacy',
    below_reorder_since     TEXT,
    created_at              TEXT NOT NULL DEFAULT (datetime('now')),
    staging_node            TEXT NOT NULL DEFAULT '',
    release_node            TEXT NOT NULL DEFAULT '',
    inbound_source_node     TEXT NOT NULL DEFAULT '',
    inbound_source_node_group TEXT NOT NULL DEFAULT '',
    outbound_source_node    TEXT NOT NULL DEFAULT '',
    outbound_source_node_group TEXT NOT NULL DEFAULT '',
    outbound_source         TEXT NOT NULL DEFAULT '',
    mode                    TEXT NOT NULL DEFAULT 'loader',
    second_paired_core_node TEXT NOT NULL DEFAULT '',
    UNIQUE(style_id, core_node_name)
);
INSERT INTO style_node_claims (
    id, style_id, core_node_name, role, swap_mode, payload_code, uop_capacity,
    reorder_point, auto_reorder, inbound_staging, outbound_staging, inbound_source,
    outbound_destination, allowed_payload_codes, auto_request_payload, keep_staged,
    evacuate_on_changeover, paired_core_node, auto_confirm, sequence,
    lineside_soft_threshold, reuse_compatible_bins, auto_push, reorder_point_source,
    below_reorder_since, created_at, staging_node, release_node, inbound_source_node,
    inbound_source_node_group, outbound_source_node, outbound_source_node_group,
    outbound_source, mode, second_paired_core_node
)
SELECT
    id, style_id, core_node_name, role, swap_mode, payload_code, uop_capacity,
    reorder_point, auto_reorder, inbound_staging, outbound_staging, inbound_source,
    outbound_destination, allowed_payload_codes, auto_request_payload, keep_staged,
    evacuate_on_changeover, paired_core_node, auto_confirm, sequence,
    lineside_soft_threshold, reuse_compatible_bins, auto_push, reorder_point_source,
    below_reorder_since, created_at, staging_node, release_node, inbound_source_node,
    inbound_source_node_group, outbound_source_node, outbound_source_node_group,
    outbound_source, mode, second_paired_core_node
FROM style_node_claims_legacy;
DROP TABLE style_node_claims_legacy;
`

// hasNarrowDemandOrigins reports whether demand_origins_open exists in the
// pre-state-transfer four-column shape. Keyed on `revision`, which arrived with
// the widening and is the one column that cannot be present in the old shape.
func hasNarrowDemandOrigins(db *sql.DB) bool {
	present, err := schema.TableExists(db, "demand_origins_open")
	if err != nil || !present {
		return false
	}
	hasRevision, err := schema.TableHasColumn(db, "demand_origins_open", "revision")
	if err != nil {
		return false
	}
	return !hasRevision
}
