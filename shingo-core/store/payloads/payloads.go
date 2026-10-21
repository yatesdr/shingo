// Package payloads holds payload-aggregate persistence for shingo-core.
//
// Stage 2D of the architecture plan moved payload CRUD + manifest
// templates out of the flat store/ package and into this sub-package.
// The outer store/ keeps one-line delegate methods on *store.DB; callers
// name payloads.Payload and its siblings directly. Cross-aggregate methods (those that span payloads
// and bins/nodes) stay at the outer store/ level.
package payloads

import (
	"database/sql"
	"fmt"

	"shingocore/domain"
	"shingocore/store/internal/helpers"
)

// Payload is the payload-template domain entity. The struct lives in
// shingocore/domain (Stage 2A); this alias keeps the payloads.Payload
// name used by ScanPayload, Create/Update, and the outer store/
// payloads.go re-export (store.Payload).
type Payload = domain.Payload

// SelectCols is exported so cross-aggregate readers (e.g. ListPayloadsForNode
// at the outer store/ level) can reuse the column list.
const SelectCols = `id, code, description, uop_capacity, robot_group, advanced_load_sequence, near_empty_enabled, near_empty_robot_group, near_empty_threshold_pct, tote_layers, uop_per_tote, created_at, updated_at`

// ScanPayload reads a single payloads row. Exported for cross-aggregate
// readers at the outer store/ level.
func ScanPayload(row interface{ Scan(...any) error }) (*Payload, error) {
	var p Payload
	err := row.Scan(&p.ID, &p.Code, &p.Description,
		&p.UOPCapacity, &p.RobotGroup, &p.AdvancedLoadSequence,
		&p.NearEmptyEnabled, &p.NearEmptyRobotGroup, &p.NearEmptyThresholdPct,
		&p.ToteLayers, &p.UOPPerTote, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ScanPayloads reads all payloads rows from a *sql.Rows.
func ScanPayloads(rows *sql.Rows) ([]*Payload, error) {
	var payloads []*Payload
	for rows.Next() {
		p, err := ScanPayload(rows)
		if err != nil {
			return nil, err
		}
		payloads = append(payloads, p)
	}
	return payloads, rows.Err()
}

// PayloadCATIDs returns payload id → the DISTINCT cat ids its manifest lines
// resolve to, comma-joined in order. A single-part payload yields that one
// value; a kit yields the full list, which the edge splits back into the
// style's part-identity SET (membership semantics, so multi-part is
// unambiguous by design: the guard accepts any of them). Payloads whose lines
// resolve to no cat id are omitted, so the edge derives nothing from them.
//
// IT READS THE PART'S CAT ID, NOT THE MANIFEST LINE (v107/v108). A cat id is a
// property of the PART — a consume cell combining four parts mints a new one
// for what it outputs, and the four inputs each carry their own — so it lives
// on the parts row beside the part number. This used to read the manifest
// line's own value, which held a cat id because the entry form asked for one.
//
// THE PART NUMBER ARM IS THE CORRECTION WINDOW, AND ONLY FOR AN UNLINKED LINE.
// v108 re-points every single-line payload; a KIT's lines are typed in by hand
// afterwards, and until they are, the line (part_id NULL) still carries the cat
// id it always did, so its part_number is read as the cat id. The arm retires
// itself: when the last kit line has a part, nothing reaches it.
//
// A LINKED part with a blank cat id contributes NOTHING. It used to fall
// through to its part number, which is a part number, not a cat id: the Edge
// took it as the style's identity, and the process's numeric cat id could never
// match it, so the wrong-part guard refused every request on that process and
// the post-cutover check flagged a mismatch on every changeover (SPR SNF3,
// 2026-10-14). An empty set leaves the guard inert, which is
// what "no cat id configured" means.
// lineCATID is one manifest line's cat id: the linked part's, or for an
// unlinked line (the v108 window) the line's own part_number.
const lineCATID = `CASE WHEN pm.part_id IS NULL THEN pm.part_number ELSE COALESCE(pt.catid, '') END`

func PayloadCATIDs(db *sql.DB) (map[int64]string, error) {
	rows, err := db.Query(`SELECT pm.payload_id,
			string_agg(DISTINCT ` + lineCATID + `, ',' ORDER BY ` + lineCATID + `)
		FROM payload_manifest pm
		LEFT JOIN parts pt ON pt.id = pm.part_id
		WHERE ` + lineCATID + ` <> ''
		GROUP BY pm.payload_id`)
	if err != nil {
		return nil, fmt.Errorf("payload catids: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]string)
	for rows.Next() {
		var payloadID int64
		var joined string
		if err := rows.Scan(&payloadID, &joined); err != nil {
			return nil, fmt.Errorf("scan payload catid: %w", err)
		}
		out[payloadID] = joined
	}
	return out, rows.Err()
}

// DescriptionsByCode returns a payload_code → description map for every payload
// template. Used by the inventory Replenishment Health rollup to show a catalog
// description beside each payload code.
func DescriptionsByCode(db *sql.DB) (map[string]string, error) {
	rows, err := db.Query(`SELECT code, COALESCE(description, '') FROM payloads`)
	if err != nil {
		return nil, fmt.Errorf("payload descriptions: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var code, desc string
		if err := rows.Scan(&code, &desc); err != nil {
			return nil, fmt.Errorf("scan payload description: %w", err)
		}
		out[code] = desc
	}
	return out, rows.Err()
}

// Create inserts a new payload template and sets p.ID on success.
func Create(db *sql.DB, p *Payload) error {
	return createRow(db, p)
}

// createRow is Create's INSERT, on a database or inside a transaction.
func createRow(db helpers.QueryRower, p *Payload) error {
	id, err := helpers.InsertID(db, `INSERT INTO payloads (code, description, uop_capacity, robot_group, advanced_load_sequence,
		near_empty_enabled, near_empty_robot_group, near_empty_threshold_pct, tote_layers, uop_per_tote)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		p.Code, p.Description, p.UOPCapacity, p.RobotGroup, p.AdvancedLoadSequence,
		p.NearEmptyEnabled, p.NearEmptyRobotGroup, p.NearEmptyThresholdPct, p.ToteLayers, p.UOPPerTote)
	if err != nil {
		return fmt.Errorf("create payload: %w", err)
	}
	p.ID = id
	return nil
}

// Update writes all payload columns by primary key, the tote numbers among
// them: every caller reads the row (Get) and lays its edit over it, so a door
// that does not edit a column writes back what it read. Pin:
// TestPayloadDoors_UpdateRoundTrip.
func Update(db *sql.DB, p *Payload) error {
	return updateRow(db, p)
}

// execer is the write surface updateRow and the set writers need: a database
// or a transaction.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// updateRow is Update's UPDATE, on a database or inside a transaction.
func updateRow(db execer, p *Payload) error {
	_, err := db.Exec(`UPDATE payloads SET code=$1, description=$2, uop_capacity=$3, robot_group=$4,
		advanced_load_sequence=$5, near_empty_enabled=$6, near_empty_robot_group=$7,
		near_empty_threshold_pct=$8, tote_layers=$9, uop_per_tote=$10, updated_at=NOW() WHERE id=$11`,
		p.Code, p.Description, p.UOPCapacity, p.RobotGroup, p.AdvancedLoadSequence,
		p.NearEmptyEnabled, p.NearEmptyRobotGroup, p.NearEmptyThresholdPct, p.ToteLayers, p.UOPPerTote, p.ID)
	return err
}

// Sets is what a save states about a payload's two sets: its carrier types and
// its manifest. A nil field is a set the save did not state, and the stored
// one stands; a non-nil one, empty included, replaces it.
type Sets struct {
	BinTypeIDs *[]int64
	Manifest   *[]Line
}

// CreateWithSets inserts a payload and the sets the save states, in one
// transaction: a refusal from any part of it (a cat-id conflict in the
// manifest, which only the manifest write can see) leaves no payload behind.
// p.ID is set only when the whole save committed. Pin:
// TestPayloadSave_CreateCommitsTogetherOrNotAtAll.
func CreateWithSets(db *sql.DB, p *Payload, sets Sets) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	row := *p
	if err := createRow(tx, &row); err != nil {
		return err
	}
	if err := writeSetsTx(tx, row.ID, sets); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	p.ID = row.ID
	return nil
}

// UpdateWithSets writes a payload's row and the sets the save states, in one
// transaction: the row, its carrier types and its manifest change together or
// not at all. Pin: TestPayloadSave_UpdateCommitsTogetherOrNotAtAll.
func UpdateWithSets(db *sql.DB, p *Payload, sets Sets) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if err := updateRow(tx, p); err != nil {
		return err
	}
	if err := writeSetsTx(tx, p.ID, sets); err != nil {
		return err
	}
	return tx.Commit()
}

// writeSetsTx replaces the sets the save states, carrier types first.
func writeSetsTx(tx *sql.Tx, payloadID int64, sets Sets) error {
	if sets.BinTypeIDs != nil {
		if err := setBinTypesTx(tx, payloadID, *sets.BinTypeIDs); err != nil {
			return err
		}
	}
	if sets.Manifest != nil {
		if err := replaceManifestTx(tx, payloadID, *sets.Manifest); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes a payload template.
func Delete(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM payloads WHERE id=$1`, id)
	return err
}

// Get fetches a payload by ID.
func Get(db *sql.DB, id int64) (*Payload, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM payloads WHERE id=$1`, SelectCols), id)
	return ScanPayload(row)
}

// GetByCode fetches a payload by its unique code.
func GetByCode(db *sql.DB, code string) (*Payload, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM payloads WHERE code=$1`, SelectCols), code)
	return ScanPayload(row)
}

// List returns every payload ordered by code.
func List(db *sql.DB) ([]*Payload, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM payloads ORDER BY code`, SelectCols))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanPayloads(rows)
}

// SetBinTypes replaces all bin type associations for a payload template.
// Runs as a single transaction. The INSERT is ON CONFLICT DO NOTHING: two
// concurrent SetBinTypes calls for the same payload can interleave their
// DELETE/INSERT phases, and without it one transaction's INSERT trips over
// the other's committed rows on payload_bin_types_pkey and the loser returns
// an error for a write whose end state is exactly what it asked for.
//
// ── THE FINDINGS ARE RECOMPUTED IN THIS TRANSACTION ─────────────────────────
//
// bins.undeclared_carrier_at records "the payload this carrier holds is not
// declared to travel in it" — a statement about THIS TABLE, so editing this
// table is the moment it can stop being true in either direction. A bin that
// now passes loses its flag in the same transaction that made it pass, and a
// bin that no longer passes gains one, so the flag never disagrees with the
// rule it is derived from. Without this, widening a rule would leave carriers
// on an operator's list with nothing left to fix, which is the fastest way to
// teach people to ignore a list.
//
// ONE UPDATE, over the bins carrying this payload only. This is an admin
// action — somebody editing a payload template — not a tick, and the statement
// costs nothing on a payload no bin carries.
func SetBinTypes(db *sql.DB, payloadID int64, binTypeIDs []int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if err := setBinTypesTx(tx, payloadID, binTypeIDs); err != nil {
		return err
	}
	return tx.Commit()
}

// setBinTypesTx is SetBinTypes inside the caller's transaction.
func setBinTypesTx(tx *sql.Tx, payloadID int64, binTypeIDs []int64) error {
	if _, err := tx.Exec(`DELETE FROM payload_bin_types WHERE payload_id=$1`, payloadID); err != nil {
		return err
	}
	for _, btID := range binTypeIDs {
		if _, err := tx.Exec(`INSERT INTO payload_bin_types (payload_id, bin_type_id) VALUES ($1, $2)
			ON CONFLICT (payload_id, bin_type_id) DO NOTHING`, payloadID, btID); err != nil {
			return err
		}
	}
	return recomputeUndeclaredCarriersTx(tx, payloadID)
}

// recomputeUndeclaredCarriersTx re-derives bins.undeclared_carrier_at for every
// bin carrying the payload whose rule just changed.
//
// It runs AFTER the DELETE/INSERT so it judges the new rule, and it is scoped
// by payload_code rather than sweeping the table: a rule edit can only change
// the answer for bins carrying that payload.
//
// COALESCE preserves an existing stamp so a carrier that was already wrong is
// not re-dated by an unrelated edit to the same payload's rule — the timestamp
// answers "how long has this been wrong", and a bin whose rule was edited twice
// has been wrong since the first time.
//
// A payload left with NO rows takes the ELSE arm for every one of its bins,
// which is the sparse-table ruling arriving from the clearing side: coverage is
// a gap in the data, so removing the last rule un-flags rather than flags.
func recomputeUndeclaredCarriersTx(tx *sql.Tx, payloadID int64) error {
	_, err := tx.Exec(`
		UPDATE bins b SET undeclared_carrier_at = CASE
		    WHEN `+helpers.UndeclaredCarrierRuleSQL("b.payload_code", "b.bin_type_id")+`
		    THEN COALESCE(b.undeclared_carrier_at, NOW())
		    ELSE NULL END
		WHERE b.payload_code <> ''
		  AND b.payload_code = (SELECT code FROM payloads WHERE id = $1)`, payloadID)
	if err != nil {
		return fmt.Errorf("recompute undeclared carriers for payload %d: %w", payloadID, err)
	}
	return nil
}
