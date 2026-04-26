// styles.go — recipe style persistence inside the processes aggregate.
//
// Phase 6.0c of the architecture refactor folded shingo-edge/store/styles/
// into store/processes/ because styles are part of the process domain
// cluster (process runs a style, style has claims on core nodes, changeover
// transitions between styles). Function names carry the Style suffix so
// they don't collide with the equivalent Process / Node / Claim /
// Changeover functions in their sibling files within this package.

package processes

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

// ListStyles returns all styles ordered by name.
func ListStyles(db *sql.DB) ([]Style, error) {
	rows, err := db.Query(`SELECT id, name, description, COALESCE(process_id, 0) as process_id, created_at FROM styles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStyles(rows)
}

// ListStylesByProcess returns styles for a single process_id.
func ListStylesByProcess(db *sql.DB, processID int64) ([]Style, error) {
	rows, err := db.Query(`SELECT id, name, description, COALESCE(process_id, 0) as process_id, created_at FROM styles WHERE process_id = ? ORDER BY name`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStyles(rows)
}

// GetStyleByName looks up a single style by name.
func GetStyleByName(db *sql.DB, name string) (*Style, error) {
	s, err := scanStyle(db.QueryRow(`SELECT id, name, description, COALESCE(process_id, 0) as process_id, created_at FROM styles WHERE name = ?`, name))
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// GetStyle looks up a single style by id.
func GetStyle(db *sql.DB, id int64) (*Style, error) {
	s, err := scanStyle(db.QueryRow(`SELECT id, name, description, COALESCE(process_id, 0) as process_id, created_at FROM styles WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// CreateStyle inserts a new style and returns the new row id.
func CreateStyle(db *sql.DB, name, description string, processID int64) (int64, error) {
	res, err := db.Exec(`INSERT INTO styles (name, description, process_id) VALUES (?, ?, ?)`, name, description, processID)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateStyle modifies an existing style.
func UpdateStyle(db *sql.DB, id int64, name, description string, processID int64) error {
	_, err := db.Exec(`UPDATE styles SET name=?, description=?, process_id=? WHERE id=?`, name, description, processID, id)
	return err
}

// DeleteStyle removes a style row by id.
func DeleteStyle(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM styles WHERE id=?`, id)
	return err
}
