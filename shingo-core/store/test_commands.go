package store

import (
	"database/sql"
	"fmt"
	"time"
)

type TestCommand struct {
	ID            int64
	CommandType   string
	RobotID       string
	VendorOrderID string
	VendorState   string
	Location      string
	ConfigID      string
	Detail        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	CompletedAt   *time.Time
}

func scanTestCommand(row interface{ Scan(...any) error }) (*TestCommand, error) {
	var tc TestCommand
	var createdAt, updatedAt any
	var completedAt any

	err := row.Scan(&tc.ID, &tc.CommandType, &tc.RobotID, &tc.VendorOrderID,
		&tc.VendorState, &tc.Location, &tc.ConfigID, &tc.Detail,
		&createdAt, &updatedAt, &completedAt)
	if err != nil {
		return nil, err
	}
	tc.CreatedAt = parseTime(createdAt)
	tc.UpdatedAt = parseTime(updatedAt)
	tc.CompletedAt = parseTimePtr(completedAt)
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

const testCommandCols = `id, command_type, robot_id, vendor_order_id, vendor_state, location, config_id, detail, created_at, updated_at, completed_at`

func (db *DB) CreateTestCommand(tc *TestCommand) error {
	result, err := db.Exec(db.Q(`INSERT INTO test_commands (command_type, robot_id, vendor_order_id, vendor_state, location, config_id, detail) VALUES (?, ?, ?, ?, ?, ?, ?)`),
		tc.CommandType, tc.RobotID, tc.VendorOrderID, tc.VendorState, tc.Location, tc.ConfigID, tc.Detail)
	if err != nil {
		return fmt.Errorf("create test command: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("create test command last id: %w", err)
	}
	tc.ID = id
	return nil
}

func (db *DB) UpdateTestCommandStatus(id int64, vendorState, detail string) error {
	_, err := db.Exec(db.Q(`UPDATE test_commands SET vendor_state=?, detail=?, updated_at=datetime('now') WHERE id=?`),
		vendorState, detail, id)
	return err
}

func (db *DB) CompleteTestCommand(id int64) error {
	_, err := db.Exec(db.Q(`UPDATE test_commands SET completed_at=datetime('now'), updated_at=datetime('now') WHERE id=?`), id)
	return err
}

func (db *DB) GetTestCommand(id int64) (*TestCommand, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM test_commands WHERE id=?`, testCommandCols)), id)
	return scanTestCommand(row)
}

func (db *DB) ListTestCommands(limit int) ([]*TestCommand, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM test_commands ORDER BY id DESC LIMIT ?`, testCommandCols)), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTestCommands(rows)
}

func (db *DB) ListActiveTestCommands() ([]*TestCommand, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM test_commands WHERE completed_at IS NULL ORDER BY id DESC`, testCommandCols)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTestCommands(rows)
}
