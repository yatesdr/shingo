// Package process_groups holds the persistence for the process_groups
// table — the UI-only organizational grouping on the Processes admin
// page. A process belongs to at most one group (or none). Deleting a
// group reverts its members to Ungrouped via ON DELETE SET NULL.
package process_groups

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"shingoedge/domain"
	"shingoedge/store/internal/helpers"
)

// Group is one row of process_groups. Alias to domain.ProcessGroup so
// every scan helper and the outer store/ re-exports share one struct.
type Group = domain.ProcessGroup

const groupCols = `id, name, description, sort_order, created_at`

func scanGroup(scanner interface{ Scan(...any) error }) (Group, error) {
	var g Group
	var createdAt string
	if err := scanner.Scan(&g.ID, &g.Name, &g.Description, &g.SortOrder, &createdAt); err != nil {
		return g, err
	}
	g.CreatedAt = helpers.ScanTime(createdAt)
	return g, nil
}

// ListGroups returns every process_groups row, ordered by name (alphabetical).
func ListGroups(db *sql.DB) ([]Group, error) {
	rows, err := db.Query(`SELECT ` + groupCols + ` FROM process_groups ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetGroup returns one process_group by id.
func GetGroup(db *sql.DB, id int64) (*Group, error) {
	g, err := scanGroup(db.QueryRow(`SELECT `+groupCols+` FROM process_groups WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// ErrDuplicateGroupName is returned when a create or rename collides with
// an existing group's name (UNIQUE constraint on process_groups.name). The
// handler maps it to 409 so the operator sees "name already in use" rather
// than a raw SQLite error in a 500 toast.
var ErrDuplicateGroupName = errors.New("a group with that name already exists")

// isUniqueViolation reports whether err is SQLite's UNIQUE-constraint
// failure. modernc.org/sqlite surfaces it as a plain *sqlite.Error with
// that message; there is no exported sentinel to errors.Is against.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// CreateGroup inserts a process_group row and returns the new id. name must
// be non-empty and unique — the UNIQUE constraint on the column enforces
// the latter; duplicates come back as ErrDuplicateGroupName.
func CreateGroup(db *sql.DB, name, description string) (int64, error) {
	if name == "" {
		return 0, fmt.Errorf("name is required")
	}
	res, err := db.Exec(`INSERT INTO process_groups (name, description) VALUES (?, ?)`,
		name, description)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrDuplicateGroupName
		}
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateGroup modifies a process_group's name and description. Renaming to
// a name another group already holds returns ErrDuplicateGroupName.
func UpdateGroup(db *sql.DB, id int64, name, description string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	_, err := db.Exec(`UPDATE process_groups SET name=?, description=? WHERE id=?`,
		name, description, id)
	if isUniqueViolation(err) {
		return ErrDuplicateGroupName
	}
	return err
}

// DeleteGroup removes a process_group row. Member processes are
// explicitly reverted to Ungrouped FIRST, because foreign_keys is OFF
// (see store/store.go) so the ON DELETE SET NULL FK does not fire.
func DeleteGroup(db *sql.DB, id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE processes SET group_id=NULL WHERE group_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM process_groups WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// CountGroupMembers returns the number of processes currently in a group.
// Used by the delete-confirm dialog to tell the operator how many
// processes will be moved back to Ungrouped.
func CountGroupMembers(db *sql.DB, id int64) (int, error) {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM processes WHERE group_id = ?`, id).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}
