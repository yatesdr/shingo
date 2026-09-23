package bins

import (
	"database/sql"
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
const BinTypeSelectCols = `id, code, description, width_in, height_in, length_in, required_robot_group, bare, created_at, updated_at`

// ScanBinType reads a single bin_types row. Exported so cross-aggregate readers
// at the outer store/ level can use it.
func ScanBinType(row interface{ Scan(...any) error }) (*BinType, error) {
	var bt BinType
	err := row.Scan(&bt.ID, &bt.Code, &bt.Description, &bt.WidthIn, &bt.HeightIn, &bt.LengthIn,
		&bt.RequiredRobotGroup, &bt.Bare, &bt.CreatedAt, &bt.UpdatedAt)
	if err != nil {
		return nil, err
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

// CreateType inserts a new bin type and sets bt.ID on success.
func CreateType(db *sql.DB, bt *BinType) error {
	id, err := helpers.InsertID(db, `INSERT INTO bin_types (code, description, width_in, height_in, length_in, required_robot_group, bare)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		bt.Code, bt.Description, bt.WidthIn, bt.HeightIn, bt.LengthIn, bt.RequiredRobotGroup, bt.Bare)
	if err != nil {
		return fmt.Errorf("create bin type: %w", err)
	}
	bt.ID = id
	return nil
}

// UpdateType writes the mutable columns on a bin type.
func UpdateType(db *sql.DB, bt *BinType) error {
	_, err := db.Exec(`UPDATE bin_types SET code=$1, description=$2, width_in=$3, height_in=$4, length_in=$5,
		required_robot_group=$6, bare=$7, updated_at=NOW() WHERE id=$8`,
		bt.Code, bt.Description, bt.WidthIn, bt.HeightIn, bt.LengthIn, bt.RequiredRobotGroup, bt.Bare, bt.ID)
	return err
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

// TypeInPayloadRule reports whether any payload rule lists the type. Flagging
// a type bare is refused while one does.
func TypeInPayloadRule(db *sql.DB, id int64) (bool, error) {
	var in bool
	err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM payload_bin_types WHERE bin_type_id=$1)`, id).Scan(&in)
	return in, err
}

// TypeIsLiveLoadersBare reports whether a live loader names the type as the
// bare type its CLEAR stamps. Un-flagging it is refused while one does:
// otherwise the edit produces a loader whose bare type is not bare. An archived
// loader stamps nothing and does not count.
func TypeIsLiveLoadersBare(db *sql.DB, id int64) (bool, error) {
	var in bool
	err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM bin_loaders WHERE bare_bin_type_id=$1 AND archived_at IS NULL)`, id).Scan(&in)
	return in, err
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
