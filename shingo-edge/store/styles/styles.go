// Package styles holds style/recipe persistence for shingo-edge.
//
// Phase 5b of the architecture plan moved the styles CRUD out of the
// flat store/ package and into this sub-package. The outer store/
// keeps a type alias (`store.Style = styles.Style`) and one-line
// delegate methods on *store.DB so external callers see no API change.
package styles

import (
	"database/sql"
	"time"

	"shingoedge/store/internal/helpers"
)

// Style represents a product/recipe style that maps to a BOM.
type Style struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	ProcessID   int64     `json:"process_id"`
	CreatedAt   time.Time `json:"created_at"`
}

func scanStyle(scanner interface{ Scan(...interface{}) error }) (Style, error) {
	var s Style
	var createdAt string
	if err := scanner.Scan(&s.ID, &s.Name, &s.Description, &s.ProcessID, &createdAt); err != nil {
		return s, err
	}
	s.CreatedAt = helpers.ScanTime(createdAt)
	return s, nil
}

func scanStyles(rows *sql.Rows) ([]Style, error) {
	var styles []Style
	for rows.Next() {
		s, err := scanStyle(rows)
		if err != nil {
			return nil, err
		}
		styles = append(styles, s)
	}
	return styles, rows.Err()
}

// List returns all styles ordered by name.
func List(db *sql.DB) ([]Style, error) {
	rows, err := db.Query(`SELECT id, name, description, COALESCE(process_id, 0) as process_id, created_at FROM styles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStyles(rows)
}

// ListByProcess returns styles for a single process_id.
func ListByProcess(db *sql.DB, processID int64) ([]Style, error) {
	rows, err := db.Query(`SELECT id, name, description, COALESCE(process_id, 0) as process_id, created_at FROM styles WHERE process_id = ? ORDER BY name`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStyles(rows)
}

// GetByName looks up a single style by name.
func GetByName(db *sql.DB, name string) (*Style, error) {
	s, err := scanStyle(db.QueryRow(`SELECT id, name, description, COALESCE(process_id, 0) as process_id, created_at FROM styles WHERE name = ?`, name))
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// Get looks up a single style by id.
func Get(db *sql.DB, id int64) (*Style, error) {
	s, err := scanStyle(db.QueryRow(`SELECT id, name, description, COALESCE(process_id, 0) as process_id, created_at FROM styles WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// Create inserts a new style and returns the new row id.
func Create(db *sql.DB, name, description string, processID int64) (int64, error) {
	res, err := db.Exec(`INSERT INTO styles (name, description, process_id) VALUES (?, ?, ?)`, name, description, processID)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Update modifies an existing style.
func Update(db *sql.DB, id int64, name, description string, processID int64) error {
	_, err := db.Exec(`UPDATE styles SET name=?, description=?, process_id=? WHERE id=?`, name, description, processID, id)
	return err
}

// Delete removes a style row by id.
func Delete(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM styles WHERE id=?`, id)
	return err
}
