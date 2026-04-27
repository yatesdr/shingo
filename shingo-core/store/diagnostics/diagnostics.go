// Package diagnostics holds test_commands persistence for shingo-core.
//
// Phase 5 of the architecture plan moved the test_commands CRUD out of
// the flat store/ package and into this sub-package. The outer store/
// keeps a type alias (`store.TestCommand = diagnostics.TestCommand`)
// and one-line delegate methods on *store.DB so external callers see no
// API change.
//
// "test_commands" is the developer-tooling table that drives the
// diagnostics console — it has nothing to do with Go test files.
package diagnostics

import (
	"database/sql"
	"fmt"

	"shingocore/domain"
	"shingocore/store/internal/helpers"
)

// TestCommand is one diagnostics test_commands row. The struct lives
// in shingocore/domain (Stage 2A.2); this alias keeps the
// diagnostics.TestCommand name used by scan helpers and the outer
// store/ re-export, and lets www handlers reference TestCommand via
// shingocore/domain instead of importing this persistence
// sub-package.
type TestCommand = domain.TestCommand

const testCommandCols = `id, command_type, robot_id, vendor_order_id, vendor_state, location, config_id, detail, created_at, updated_at, completed_at`

func scanTestCommand(row interface{ Scan(...any) error }) (*TestCommand, error) {
	var tc TestCommand

	err := row.Scan(&tc.ID, &tc.CommandType, &tc.RobotID, &tc.VendorOrderID,
		&tc.VendorState, &tc.Location, &tc.ConfigID, &tc.Detail,
		&tc.CreatedAt, &tc.UpdatedAt, &tc.CompletedAt)
	if err != nil {
		return nil, err
	}
	return &tc, nil
}

func scanTestCommands(rows *sql.Rows) ([]*TestCommand, error) {
	var cmds []*TestCommand
	for rows.Next() {
		tc, err := scanTestCommand(rows)
		if err != nil {
			return nil, err
		}
		cmds = append(cmds, tc)
	}
	return cmds, rows.Err()
}

// Create inserts a test_commands row and sets tc.ID on success.
func Create(db *sql.DB, tc *TestCommand) error {
	id, err := helpers.InsertID(db, `INSERT INTO test_commands (command_type, robot_id, vendor_order_id, vendor_state, location, config_id, detail) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		tc.CommandType, tc.RobotID, tc.VendorOrderID, tc.VendorState, tc.Location, tc.ConfigID, tc.Detail)
	if err != nil {
		return fmt.Errorf("create test command: %w", err)
	}
	tc.ID = id
	return nil
}

// UpdateStatus updates vendor_state + detail for one row.
func UpdateStatus(db *sql.DB, id int64, vendorState, detail string) error {
	_, err := db.Exec(`UPDATE test_commands SET vendor_state=$1, detail=$2, updated_at=NOW() WHERE id=$3`,
		vendorState, detail, id)
	return err
}

// Complete marks one test_commands row as completed.
func Complete(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE test_commands SET completed_at=NOW(), updated_at=NOW() WHERE id=$1`, id)
	return err
}

// Get returns one test_commands row by id.
func Get(db *sql.DB, id int64) (*TestCommand, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM test_commands WHERE id=$1`, testCommandCols), id)
	return scanTestCommand(row)
}

// List returns the most recent test_commands rows.
func List(db *sql.DB, limit int) ([]*TestCommand, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM test_commands ORDER BY id DESC LIMIT $1`, testCommandCols), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTestCommands(rows)
}

// ListActive returns test_commands rows that have not completed.
func ListActive(db *sql.DB) ([]*TestCommand, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM test_commands WHERE completed_at IS NULL ORDER BY id DESC`, testCommandCols))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTestCommands(rows)
}
