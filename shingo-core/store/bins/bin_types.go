package bins

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"shingocore/domain"
	"shingocore/store/internal/helpers"
)

// BinType is the bin-type domain entity. The struct lives in
// shingocore/domain (Stage 2A); this alias keeps the bins.BinType name
// used by ScanBinType, CreateType, UpdateType, and the cross-aggregate
// GetEffectiveBinTypes reader at the outer store/ level.
type BinType = domain.BinType

// BinTypeSelectCols is exported so cross-aggregate readers (e.g. GetEffectiveBinTypes
// at the outer store/ level, which JOINs node ancestors) can reuse the column list.
const BinTypeSelectCols = `id, code, description, width_in, height_in, length_in, required_robot_group, bare, bare_of, created_at, updated_at`

// ScanBinType reads a single bin_types row. Exported so cross-aggregate readers
// at the outer store/ level can use it.
func ScanBinType(row interface{ Scan(...any) error }) (*BinType, error) {
	var bt BinType
	var bareOf sql.NullInt64
	err := row.Scan(&bt.ID, &bt.Code, &bt.Description, &bt.WidthIn, &bt.HeightIn, &bt.LengthIn,
		&bt.RequiredRobotGroup, &bt.Bare, &bareOf, &bt.CreatedAt, &bt.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if bareOf.Valid {
		bt.BareOf = &bareOf.Int64
	}
	return &bt, nil
}

// ScanBinTypes reads all bin_types rows from a *sql.Rows.
func ScanBinTypes(rows *sql.Rows) ([]*BinType, error) {
	var types []*BinType
	for rows.Next() {
		bt, err := ScanBinType(rows)
		if err != nil {
			return nil, err
		}
		types = append(types, bt)
	}
	return types, rows.Err()
}

// CreateType inserts a new bin type and sets bt.ID on success. Bare and BareOf
// are not written: bare is generated from bare_of, and bare_of has one writer,
// EnsureBareMarkerTx.
func CreateType(db *sql.DB, bt *BinType) error {
	id, err := helpers.InsertID(db, `INSERT INTO bin_types (code, description, width_in, height_in, length_in, required_robot_group)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		bt.Code, bt.Description, bt.WidthIn, bt.HeightIn, bt.LengthIn, bt.RequiredRobotGroup)
	if err != nil {
		return fmt.Errorf("create bin type: %w", err)
	}
	bt.ID = id
	return nil
}

// UpdateType writes the mutable columns on a bin type. Bare and BareOf are not
// among them (see CreateType).
func UpdateType(db *sql.DB, bt *BinType) error {
	_, err := db.Exec(`UPDATE bin_types SET code=$1, description=$2, width_in=$3, height_in=$4, length_in=$5,
		required_robot_group=$6, updated_at=NOW() WHERE id=$7`,
		bt.Code, bt.Description, bt.WidthIn, bt.HeightIn, bt.LengthIn, bt.RequiredRobotGroup, bt.ID)
	return err
}

// BareMarkerSuffix names the bare marker derived from a carrier type.
const BareMarkerSuffix = "-BARE"

// ErrBareMarkerTaken refuses a derived marker code that already names a type
// that is not the carrier's marker, which would make stage 1 stamp an ordinary
// type.
var ErrBareMarkerTaken = errors.New("the bare marker's code is already a bin type that is not bare: rename that type first")

// EnsureBareMarkerTx returns the bare marker for a cart type, creating it the
// first time that type comes through a stage 1. A bare type is its own marker:
// a stage-1 CLEAR tapped twice leaves the cart as it is rather than flipping it
// back to its carrier.
//
// The marker takes the carrier's code with BareMarkerSuffix, its robot group,
// and bare_of = the carrier. ON CONFLICT DO NOTHING absorbs two clears racing to
// create the same marker; a code already held by some other type is refused
// with ErrBareMarkerTaken rather than stamped.
func EnsureBareMarkerTx(tx *sql.Tx, typeID int64) (int64, error) {
	var bareOf sql.NullInt64
	if err := tx.QueryRow(`SELECT bare_of FROM bin_types WHERE id=$1`, typeID).Scan(&bareOf); err != nil {
		return 0, fmt.Errorf("cart type %d: %w", typeID, err)
	}
	if bareOf.Valid {
		return typeID, nil
	}
	if _, err := tx.Exec(`INSERT INTO bin_types (code, description, required_robot_group, bare_of)
		SELECT code || $2, code || ' with no bin on it, between the stages of a two-stage unloader', required_robot_group, id
		  FROM bin_types WHERE id = $1
		ON CONFLICT DO NOTHING`, typeID, BareMarkerSuffix); err != nil {
		return 0, fmt.Errorf("create bare marker of type %d: %w", typeID, err)
	}
	var id int64
	err := tx.QueryRow(`SELECT id FROM bin_types WHERE bare_of=$1`, typeID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		var code string
		if cerr := tx.QueryRow(`SELECT code || $2 FROM bin_types WHERE id=$1`, typeID, BareMarkerSuffix).Scan(&code); cerr == nil {
			return 0, fmt.Errorf("%w (%s)", ErrBareMarkerTaken, code)
		}
		return 0, ErrBareMarkerTaken
	}
	if err != nil {
		return 0, fmt.Errorf("read bare marker of type %d: %w", typeID, err)
	}
	return id, nil
}

// EnsureBareMarker is EnsureBareMarkerTx in its own transaction.
func EnsureBareMarker(db *sql.DB, typeID int64) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin bare marker tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	id, err := EnsureBareMarkerTx(tx, typeID)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit bare marker: %w", err)
	}
	return id, nil
}

// TypeBareOf returns the carrier a bare marker stands for, or nil for a real
// type. The two dispatch fences read it to admit a marker where its carrier is
// admitted, and only after the direct match failed.
func TypeBareOf(db *sql.DB, typeID int64) (*int64, error) {
	var bareOf sql.NullInt64
	if err := db.QueryRow(`SELECT bare_of FROM bin_types WHERE id=$1`, typeID).Scan(&bareOf); err != nil {
		return nil, err
	}
	if !bareOf.Valid {
		return nil, nil
	}
	return &bareOf.Int64, nil
}

// TypeAdmits is the one spelling of the Allowed Bin Types rule for a single
// node's list: an empty list takes anything; otherwise the type must be listed,
// or be the marker of a listed carrier. bareOf is the type's BareOf (nil for a
// real type).
func TypeAdmits(list []*BinType, typeID int64, bareOf *int64) bool {
	if len(list) == 0 {
		return true
	}
	for _, bt := range list {
		if bt.ID == typeID || (bareOf != nil && bt.ID == *bareOf) {
			return true
		}
	}
	return false
}

// BareTypeCodes returns the codes of those ids that are flagged bare, in code
// order. The payload-rule save asks it: a bare type holds no container, so it
// can never be a carrier a part is declared to travel in.
func BareTypeCodes(db *sql.DB, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// pgx stdlib has no native []int64 array param: a positional IN list.
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	rows, err := db.Query(`SELECT code FROM bin_types WHERE id IN (`+strings.Join(ph, ",")+`) AND bare ORDER BY code`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteType removes a bin type.
func DeleteType(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM bin_types WHERE id=$1`, id)
	return err
}

// GetType fetches a bin type by ID.
func GetType(db *sql.DB, id int64) (*BinType, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM bin_types WHERE id=$1`, BinTypeSelectCols), id)
	return ScanBinType(row)
}

// GetTypeByCode fetches a bin type by its unique code.
func GetTypeByCode(db *sql.DB, code string) (*BinType, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM bin_types WHERE code=$1`, BinTypeSelectCols), code)
	return ScanBinType(row)
}

// ListTypes returns every bin type ordered by code.
func ListTypes(db *sql.DB) ([]*BinType, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM bin_types ORDER BY code`, BinTypeSelectCols))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanBinTypes(rows)
}

// ListTypesForPayload returns all bin types associated with a payload template
// via payload_bin_types. Owned by bins/ because the return type is *BinType;
// caller at outer store/ exposes this as (*store.DB).ListBinTypesForPayload.
func ListTypesForPayload(db *sql.DB, payloadID int64) ([]*BinType, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM bin_types WHERE id IN (SELECT bin_type_id FROM payload_bin_types WHERE payload_id=$1) ORDER BY code`, BinTypeSelectCols), payloadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanBinTypes(rows)
}
