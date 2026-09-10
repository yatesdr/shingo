// claim_quarantine.go — where a stored manual_swap claim goes when Core's
// loader set says it should not exist.
//
// WHY A MOVE AND NOT A DELETE. Core owns a loader's role, payload set, inbound
// source, outbound destination and auto-confirm (domain.Loader.SynthClaim names
// exactly those six), and it serves them to the Edge on every node-list sync. A
// style_node_claims row carrying swap_mode='manual_swap' supplies the same six
// from the Edge's own database, so it is a SECOND AUTHORITY: whichever reader
// reaches the stored row first wins, and the two disagree the moment a loader is
// edited, retired, or recreated in Core. The ownership rule says a migration
// that moves ownership of a fact deletes or neutralises the old copy in the same
// operation; this is that operation, arriving late.
//
// It is a move because the rows are OPERATOR-AUTHORED CONFIGURATION and there is
// no unarchive. Nothing reads the quarantine table, so a row stops voting the
// instant it lands there — the neutralisation is complete on the write — while
// recovery stays an INSERT ... SELECT away:
//
//	INSERT INTO style_node_claims (<the claim columns>)
//	SELECT <the claim columns> FROM style_node_claims_quarantine WHERE id = ?;
//
// which is why the quarantine table is a column-for-column MIRROR of
// style_node_claims rather than a summary or a JSON blob.
//
// AT THE OUTER store/ LEVEL, not in store/processes/, because the move is
// cross-aggregate: it archives a claim, clears the runtime pointer at
// process_node_runtime_states, clears both changeover_node_tasks pointers, and
// reads the schema through store/schema to derive the mirror. A store
// sub-package may import none of that (the store-sub-pkg-isolation depguard
// rule), and it should not — one atomic operation across three tables is exactly
// what the outer level is for.
package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"shingo/protocol"
	"shingoedge/store/schema"
)

// ClaimQuarantineTable is the archive. Nothing in the codebase reads it — that
// is its whole design — so it is named here for the migration, the mover, and
// the person writing the recovery statement by hand.
const ClaimQuarantineTable = "style_node_claims_quarantine"

// ClaimQuarantineReason is stamped on every moved row and repeated in every log
// line. One predicate, one reason: there is no per-row judgement to record.
const ClaimQuarantineReason = "Core owns loader configuration; a stored manual_swap claim is a second authority"

// quarantineMeta are the four columns the archive adds to its mirror of
// style_node_claims: when the row moved, why, and enough of the sync that
// displaced it to judge the move afterwards. The loader KEYS rather than a count
// alone, because "which sync" is answered by what the sync carried.
//
// Every name is prefixed so it can never collide with a column style_node_claims
// grows later — the mirror below copies by name.
var quarantineMeta = []schema.Column{
	{Name: "quarantined_at", Type: "TEXT NOT NULL DEFAULT ''"},
	{Name: "quarantined_reason", Type: "TEXT NOT NULL DEFAULT ''"},
	{Name: "quarantined_sync_loader_count", Type: "INTEGER NOT NULL DEFAULT 0"},
	{Name: "quarantined_sync_loader_keys", Type: "TEXT NOT NULL DEFAULT ''"},
}

// QuarantinedClaim identifies one moved row: enough to name it in a log line a
// person reads on the floor, and enough to find it again in the archive.
type QuarantinedClaim struct {
	ID           int64
	StyleID      int64
	CoreNodeName string
	PayloadCode  string
}

// EnsureClaimQuarantine creates the archive and keeps its shape a mirror of
// style_node_claims. Idempotent, and safe on a database of any age.
//
// THE SHAPE IS DERIVED, NOT WRITTEN DOWN, and that is deliberate.
// style_node_claims reaches its current shape by two routes — the baseline
// CREATE in sqlite_ddl.go and nine ALTER ADD COLUMNs in migrate() — so a
// hand-copied column list in the DDL would be wrong on the day it was written
// and wrong again after the next ALTER. A mirror that copies the live column
// list by name cannot drift, and a row that lands here is therefore the row in
// full whatever vintage the database is.
//
// Must run AFTER every style_node_claims migration, which is why migrate() calls
// it last: the mirror can only copy the columns that exist when it looks. A
// column that arrives later is added on the next startup by the same pass.
func (db *DB) EnsureClaimQuarantine() error {
	live, err := schema.Columns(db.DB, "style_node_claims")
	if err != nil {
		return fmt.Errorf("quarantine mirror: read style_node_claims columns: %w", err)
	}
	if len(live) == 0 {
		// No claims table means a database schema.Apply has not touched. Nothing
		// to mirror and nothing to quarantine; verifySchema reports the real
		// problem.
		return nil
	}

	exists, err := schema.TableExists(db.DB, ClaimQuarantineTable)
	if err != nil {
		return fmt.Errorf("quarantine mirror: %w", err)
	}
	if !exists {
		// CREATE TABLE AS SELECT copies the column names and their affinities and
		// drops the constraints, which is exactly what an archive wants: no
		// AUTOINCREMENT (the id is the ORIGINAL row's, carried so a restore puts
		// it back where the runtime pointers expect it), no UNIQUE (the same
		// (style, node) pair may be quarantined, restored, and quarantined again),
		// and no foreign key into styles (a style deleted afterwards must not
		// cascade the archive away).
		if _, err := db.Exec(`CREATE TABLE ` + ClaimQuarantineTable + ` AS SELECT * FROM style_node_claims WHERE 0`); err != nil {
			return fmt.Errorf("quarantine mirror: create %s: %w", ClaimQuarantineTable, err)
		}
	}

	have, err := schema.Columns(db.DB, ClaimQuarantineTable)
	if err != nil {
		return fmt.Errorf("quarantine mirror: read %s columns: %w", ClaimQuarantineTable, err)
	}
	present := make(map[string]bool, len(have))
	for _, c := range have {
		present[c.Name] = true
	}

	for _, c := range live {
		if present[c.Name] {
			continue
		}
		decl := c.Type
		if decl == "" {
			decl = "TEXT" // an untyped column; BLOB affinity either way
		}
		// No NOT NULL and no default: a column added after rows are already in the
		// archive leaves NULL on those rows, which is the honest value for "this
		// claim predates that column".
		if _, err := db.Exec(`ALTER TABLE ` + ClaimQuarantineTable + ` ADD COLUMN ` + c.Name + ` ` + decl); err != nil {
			return fmt.Errorf("quarantine mirror: add %s.%s: %w", ClaimQuarantineTable, c.Name, err)
		}
	}
	for _, m := range quarantineMeta {
		if present[m.Name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE ` + ClaimQuarantineTable + ` ADD COLUMN ` + m.Name + ` ` + m.Type); err != nil {
			return fmt.Errorf("quarantine mirror: add %s.%s: %w", ClaimQuarantineTable, m.Name, err)
		}
	}
	return nil
}

// CountLoaderClaims reports how many stored manual_swap claims exist. It is the
// question the empty-loader-set guard asks before deciding whether its refusal
// is worth a log line.
func (db *DB) CountLoaderClaims() (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM style_node_claims WHERE swap_mode = ?`,
		string(protocol.SwapModeManualSwap)).Scan(&n)
	return n, err
}

// QuarantineLoaderClaims moves every stored manual_swap claim into the archive
// and returns what it moved. Empty result, no error, nothing written when there
// is nothing to move — so a second sync carrying the same loader set is a no-op.
//
// THE PREDICATE IS EVERY STORED manual_swap ROW, not just the ones whose node is
// absent from syncLoaderKeys. Core owns loaders and SynthClaim serves the live
// ones from the aggregate, so no manual_swap row should be stored at all; the
// broad predicate states that rule instead of describing today's exceptions. At
// both plants the two predicates select the same rows anyway — every live loader
// at Springfield and Hopkinsville was authored in Core and has no stored claim.
//
// syncLoaderKeys IS NOT THE PREDICATE. It is the guardrail and the provenance:
// an empty set is refused outright, and the keys are recorded on each moved row
// so the sync that displaced it can be identified afterwards. A short or empty
// loader list is a FAILED PROJECTION, not a retired plant —
// shingo-core/store/loaders_sync.go fails the whole node list rather than ship
// one position short, precisely so the Edge stays on last-known-good — and this
// refuses to turn that failure into lost configuration.
//
// ON DELETE SET NULL IS HONOURED BY HAND. Three columns across two tables
// reference style_node_claims(id) and all three declare ON DELETE SET NULL, but
// the Edge opens SQLite with foreign_keys OFF (store.Open), so the declared
// action never runs. Doing it here in the same transaction keeps the database
// behaving the way its own schema says it does. Every reader of all three
// already treats NULL as "no opinion" and fails open; a DANGLING id is the state
// none of them expects.
func (db *DB) QuarantineLoaderClaims(syncLoaderKeys []string) ([]QuarantinedClaim, error) {
	if len(syncLoaderKeys) == 0 {
		return nil, fmt.Errorf("quarantine refused: the incoming loader set is empty, which is a failed projection rather than a plant with no loaders")
	}
	cols, err := schema.Columns(db.DB, "style_node_claims")
	if err != nil {
		return nil, fmt.Errorf("quarantine: read claim columns: %w", err)
	}
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	keysJSON, err := json.Marshal(syncLoaderKeys)
	if err != nil {
		return nil, fmt.Errorf("quarantine: encode loader keys: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	mode := string(protocol.SwapModeManualSwap)

	// The report is read inside the transaction so it names exactly the rows the
	// DELETE below removes, rather than a set that may have changed since.
	rows, err := tx.Query(`SELECT id, style_id, core_node_name, payload_code
		FROM style_node_claims WHERE swap_mode = ? ORDER BY id`, mode)
	if err != nil {
		return nil, err
	}
	var moved []QuarantinedClaim
	for rows.Next() {
		var q QuarantinedClaim
		if err := rows.Scan(&q.ID, &q.StyleID, &q.CoreNodeName, &q.PayloadCode); err != nil {
			rows.Close()
			return nil, err
		}
		moved = append(moved, q)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(moved) == 0 {
		return nil, nil
	}

	list := strings.Join(names, ", ")
	if _, err := tx.Exec(`INSERT INTO `+ClaimQuarantineTable+` (`+list+`,
			quarantined_at, quarantined_reason, quarantined_sync_loader_count, quarantined_sync_loader_keys)
		SELECT `+list+`, ?, ?, ?, ? FROM style_node_claims WHERE swap_mode = ?`,
		time.Now().UTC().Format(time.RFC3339), ClaimQuarantineReason, len(syncLoaderKeys), string(keysJSON), mode); err != nil {
		return nil, fmt.Errorf("quarantine: archive rows: %w", err)
	}

	const doomed = `SELECT id FROM style_node_claims WHERE swap_mode = ?`
	for _, stmt := range []string{
		`UPDATE process_node_runtime_states SET active_claim_id = NULL WHERE active_claim_id IN (` + doomed + `)`,
		`UPDATE changeover_node_tasks SET from_claim_id = NULL WHERE from_claim_id IN (` + doomed + `)`,
		`UPDATE changeover_node_tasks SET to_claim_id = NULL WHERE to_claim_id IN (` + doomed + `)`,
	} {
		if _, err := tx.Exec(stmt, mode); err != nil {
			return nil, fmt.Errorf("quarantine: clear claim references: %w", err)
		}
	}

	if _, err := tx.Exec(`DELETE FROM style_node_claims WHERE swap_mode = ?`, mode); err != nil {
		return nil, fmt.Errorf("quarantine: remove archived rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return moved, nil
}
