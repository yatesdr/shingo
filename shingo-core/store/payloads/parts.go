package payloads

import (
	"database/sql"
	"errors"
	"fmt"

	"shingocore/domain"
)

// Part is the parts-table entity — see domain.Part for what the two columns
// are and why they share a row.
//
// IT LIVES IN THIS PACKAGE, NOT ONE OF ITS OWN, for a plain reason: store/parts
// is already the Operations dashboard's per-part METRICS queries, and two
// packages cannot share a directory name. The parts table is the payload
// template's identity partner — every read of it joins payload_manifest and
// every write happens on the payloads page — so it sits beside the manifest it
// serves rather than taking a name that would have to be qualified everywhere.
type Part = domain.Part

// ErrCATIDConflict is returned when a caller supplies a cat id for a part that
// already has a DIFFERENT one. It carries both values so the caller can put the
// question to a person instead of choosing.
//
// THIS IS THE STANDING DRIFT-CATCHER. Silently overwriting is how one part ends
// up with two controls identities and how a style's guard starts accepting the
// wrong one; silently keeping is how a genuine correction gets swallowed. The
// only honest response is "this part exists with cat id X — same part or typo?",
// which is a question, so this is an error rather than a policy.
type ErrCATIDConflict struct {
	PartNumber string
	Existing   string
	Incoming   string
}

func (e *ErrCATIDConflict) Error() string {
	return fmt.Sprintf("part %s already carries cat id %s, and %s was entered — "+
		"same part or a typo? Nothing was changed",
		e.PartNumber, e.Existing, e.Incoming)
}

const partSelectCols = `id, part_number, catid, description, created_at, updated_at`

func scanPart(row interface{ Scan(...any) error }) (*Part, error) {
	var p Part
	if err := row.Scan(&p.ID, &p.PartNumber, &p.CATID, &p.Description, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// GetPartByNumber returns the part with this number, or sql.ErrNoRows.
func GetPartByNumber(db Querier, partNumber string) (*Part, error) {
	return scanPart(db.QueryRow(`SELECT `+partSelectCols+` FROM parts WHERE part_number=$1`, partNumber))
}

// ListParts returns every part, ordered by number.
func ListParts(db *sql.DB) ([]*Part, error) {
	rows, err := db.Query(`SELECT ` + partSelectCols + ` FROM parts ORDER BY part_number`)
	if err != nil {
		return nil, fmt.Errorf("list parts: %w", err)
	}
	defer rows.Close()
	var out []*Part
	for rows.Next() {
		p, err := scanPart(rows)
		if err != nil {
			return nil, fmt.Errorf("scan part: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Querier is the read surface UpsertPart and its callers need, satisfied by
// both *sql.DB and *sql.Tx so an upsert can join a caller's transaction.
type Querier interface {
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
}

// UpsertPart creates the part or returns the existing one, and is where a
// typed cat id meets one that is already recorded.
//
// THE THREE CASES, AND ONLY ONE OF THEM IS A JUDGEMENT CALL:
//
//   - the part is new              → insert it with whatever was typed
//   - the part exists, no cat id   → fill it in; nobody is being contradicted
//   - the part exists WITH a cat id, and a different one arrives → refuse with
//     ErrCATIDConflict and change nothing
//
// A blank incoming cat id never clears a recorded one — that is the entry form
// leaving a box alone, not a statement that the part has no controls identity.
func UpsertPart(db Querier, partNumber, catid, description string) (*Part, error) {
	existing, err := GetPartByNumber(db, partNumber)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("look up part %s: %w", partNumber, err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := db.Exec(
			`INSERT INTO parts (part_number, catid, description) VALUES ($1, $2, $3)`,
			partNumber, catid, description); err != nil {
			return nil, fmt.Errorf("create part %s: %w", partNumber, err)
		}
		return GetPartByNumber(db, partNumber)
	}
	if catid != "" && existing.CATID != "" && existing.CATID != catid {
		return nil, &ErrCATIDConflict{PartNumber: partNumber, Existing: existing.CATID, Incoming: catid}
	}
	if (catid != "" && existing.CATID == "") || (description != "" && existing.Description != description) {
		if _, err := db.Exec(`UPDATE parts SET
				catid = CASE WHEN $2 = '' THEN catid ELSE $2 END,
				description = CASE WHEN $3 = '' THEN description ELSE $3 END,
				updated_at = NOW()
			WHERE id = $1`, existing.ID, catid, description); err != nil {
			return nil, fmt.Errorf("update part %s: %w", partNumber, err)
		}
		return GetPartByNumber(db, partNumber)
	}
	return existing, nil
}

// SetPartCATID records a part's controls identity, overwriting whatever was
// there. The conflict prompt in UpsertPart is what a person answers "yes, same
// part, the new one is right" to; this is that answer being applied.
func SetPartCATID(db *sql.DB, partNumber, catid string) error {
	res, err := db.Exec(`UPDATE parts SET catid=$2, updated_at=NOW() WHERE part_number=$1`, partNumber, catid)
	if err != nil {
		return fmt.Errorf("set cat id on part %s: %w", partNumber, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no part %s", partNumber)
	}
	return nil
}
