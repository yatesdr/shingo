package store

import (
	"database/sql"
	"fmt"
	"time"
)

type Blueprint struct {
	ID                  int64     `json:"id"`
	Code                string    `json:"code"`
	Description         string    `json:"description"`
	UOPCapacity         int       `json:"uop_capacity"`
	DefaultManifestJSON string    `json:"default_manifest_json"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

const blueprintSelectCols = `id, code, description, uop_capacity, default_manifest_json, created_at, updated_at`

func scanBlueprint(row interface{ Scan(...any) error }) (*Blueprint, error) {
	var bp Blueprint
	var createdAt, updatedAt any
	err := row.Scan(&bp.ID, &bp.Code, &bp.Description,
		&bp.UOPCapacity, &bp.DefaultManifestJSON, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	bp.CreatedAt = parseTime(createdAt)
	bp.UpdatedAt = parseTime(updatedAt)
	return &bp, nil
}

func scanBlueprints(rows *sql.Rows) ([]*Blueprint, error) {
	var blueprints []*Blueprint
	for rows.Next() {
		bp, err := scanBlueprint(rows)
		if err != nil {
			return nil, err
		}
		blueprints = append(blueprints, bp)
	}
	return blueprints, rows.Err()
}

func (db *DB) CreateBlueprint(bp *Blueprint) error {
	result, err := db.Exec(db.Q(`INSERT INTO blueprints (code, description, uop_capacity, default_manifest_json) VALUES (?, ?, ?, ?)`),
		bp.Code, bp.Description, bp.UOPCapacity, bp.DefaultManifestJSON)
	if err != nil {
		return fmt.Errorf("create blueprint: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("create blueprint last id: %w", err)
	}
	bp.ID = id
	return nil
}

func (db *DB) UpdateBlueprint(bp *Blueprint) error {
	_, err := db.Exec(db.Q(`UPDATE blueprints SET code=?, description=?, uop_capacity=?, default_manifest_json=?, updated_at=datetime('now') WHERE id=?`),
		bp.Code, bp.Description, bp.UOPCapacity, bp.DefaultManifestJSON, bp.ID)
	return err
}

func (db *DB) DeleteBlueprint(id int64) error {
	_, err := db.Exec(db.Q(`DELETE FROM blueprints WHERE id=?`), id)
	return err
}

func (db *DB) GetBlueprint(id int64) (*Blueprint, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM blueprints WHERE id=?`, blueprintSelectCols)), id)
	return scanBlueprint(row)
}

func (db *DB) GetBlueprintByCode(code string) (*Blueprint, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM blueprints WHERE code=?`, blueprintSelectCols)), code)
	return scanBlueprint(row)
}

func (db *DB) ListBlueprints() ([]*Blueprint, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM blueprints ORDER BY code`, blueprintSelectCols)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBlueprints(rows)
}

// ListBinTypesForBlueprint returns all bin types associated with a blueprint.
func (db *DB) ListBinTypesForBlueprint(blueprintID int64) ([]*BinType, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM bin_types WHERE id IN (SELECT bin_type_id FROM blueprint_bin_types WHERE blueprint_id=?) ORDER BY code`, binTypeSelectCols)), blueprintID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBinTypes(rows)
}

// SetBlueprintBinTypes replaces all bin type associations for a blueprint.
func (db *DB) SetBlueprintBinTypes(blueprintID int64, binTypeIDs []int64) error {
	if _, err := db.Exec(db.Q(`DELETE FROM blueprint_bin_types WHERE blueprint_id=?`), blueprintID); err != nil {
		return err
	}
	for _, btID := range binTypeIDs {
		if _, err := db.Exec(db.Q(`INSERT INTO blueprint_bin_types (blueprint_id, bin_type_id) VALUES (?, ?)`), blueprintID, btID); err != nil {
			return err
		}
	}
	return nil
}
