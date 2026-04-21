package bins

import (
	"database/sql"
	"fmt"

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
const BinTypeSelectCols = `id, code, description, width_in, height_in, created_at, updated_at`

// ScanBinType reads a single bin_types row. Exported so cross-aggregate readers
// at the outer store/ level can use it.
func ScanBinType(row interface{ Scan(...any) error }) (*BinType, error) {
	var bt BinType
	err := row.Scan(&bt.ID, &bt.Code, &bt.Description, &bt.WidthIn, &bt.HeightIn, &bt.CreatedAt, &bt.UpdatedAt)
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
	id, err := helpers.InsertID(db, `INSERT INTO bin_types (code, description, width_in, height_in) VALUES ($1, $2, $3, $4) RETURNING id`,
		bt.Code, bt.Description, bt.WidthIn, bt.HeightIn)
	if err != nil {
		return fmt.Errorf("create bin type: %w", err)
	}
	bt.ID = id
	return nil
}

// UpdateType writes the mutable columns on a bin type.
func UpdateType(db *sql.DB, bt *BinType) error {
	_, err := db.Exec(`UPDATE bin_types SET code=$1, description=$2, width_in=$3, height_in=$4, updated_at=NOW() WHERE id=$5`,
		bt.Code, bt.Description, bt.WidthIn, bt.HeightIn, bt.ID)
	return err
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
