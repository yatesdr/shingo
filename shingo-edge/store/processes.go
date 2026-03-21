package store

import "time"

// Process represents a production process (physical production area).
type Process struct {
	ID             int64     `json:"id"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	ActiveStyleID  *int64    `json:"active_style_id"`
	CreatedAt      time.Time `json:"created_at"`
}

func (db *DB) ListProcesses() ([]Process, error) {
	rows, err := db.Query(`SELECT id, name, description, active_job_style_id, created_at FROM processes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lines []Process
	for rows.Next() {
		var l Process
		var createdAt string
		if err := rows.Scan(&l.ID, &l.Name, &l.Description, &l.ActiveStyleID, &createdAt); err != nil {
			return nil, err
		}
		l.CreatedAt = scanTime(createdAt)
		lines = append(lines, l)
	}
	return lines, rows.Err()
}

func (db *DB) GetProcess(id int64) (*Process, error) {
	l := &Process{}
	var createdAt string
	err := db.QueryRow(`SELECT id, name, description, active_job_style_id, created_at FROM processes WHERE id = ?`, id).
		Scan(&l.ID, &l.Name, &l.Description, &l.ActiveStyleID, &createdAt)
	if err != nil {
		return nil, err
	}
	l.CreatedAt = scanTime(createdAt)
	return l, nil
}

func (db *DB) CreateProcess(name, description string) (int64, error) {
	res, err := db.Exec(`INSERT INTO processes (name, description) VALUES (?, ?)`, name, description)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdateProcess(id int64, name, description string) error {
	_, err := db.Exec(`UPDATE processes SET name=?, description=? WHERE id=?`, name, description, id)
	return err
}

func (db *DB) DeleteProcess(id int64) error {
	_, err := db.Exec(`DELETE FROM processes WHERE id=?`, id)
	return err
}

func (db *DB) SetActiveStyle(lineID int64, styleID *int64) error {
	_, err := db.Exec(`UPDATE processes SET active_job_style_id=? WHERE id=?`, styleID, lineID)
	return err
}

func (db *DB) GetActiveStyleID(lineID int64) (*int64, error) {
	var id *int64
	err := db.QueryRow(`SELECT active_job_style_id FROM processes WHERE id = ?`, lineID).Scan(&id)
	if err != nil {
		return nil, err
	}
	return id, nil
}
