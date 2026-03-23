package store

import (
	"time"
)

// Process represents a production process (physical production area).
type Process struct {
	ID              int64   `json:"id"`
	Name            string  `json:"name"`
	Description     string  `json:"description"`
	ActiveStyleID   *int64  `json:"active_style_id"`
	TargetStyleID   *int64  `json:"target_style_id,omitempty"`
	ProductionState string  `json:"production_state"`
	CounterPLCName  string  `json:"counter_plc_name"`
	CounterTagName  string  `json:"counter_tag_name"`
	CounterEnabled  bool    `json:"counter_enabled"`
	CreatedAt       time.Time `json:"created_at"`
}

func scanProcess(scanner interface{ Scan(...interface{}) error }) (Process, error) {
	var p Process
	var createdAt string
	if err := scanner.Scan(&p.ID, &p.Name, &p.Description, &p.ActiveStyleID, &p.TargetStyleID, &p.ProductionState, &p.CounterPLCName, &p.CounterTagName, &p.CounterEnabled, &createdAt); err != nil {
		return p, err
	}
	p.CreatedAt = scanTime(createdAt)
	return p, nil
}

func (db *DB) ListProcesses() ([]Process, error) {
	rows, err := db.Query(`SELECT id, name, description, active_style_id, target_style_id, production_state, counter_plc_name, counter_tag_name, counter_enabled, created_at FROM processes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var processes []Process
	for rows.Next() {
		l, err := scanProcess(rows)
		if err != nil {
			return nil, err
		}
		processes = append(processes, l)
	}
	return processes, rows.Err()
}

func (db *DB) GetProcess(id int64) (*Process, error) {
	l, err := scanProcess(db.QueryRow(`SELECT id, name, description, active_style_id, target_style_id, production_state, counter_plc_name, counter_tag_name, counter_enabled, created_at FROM processes WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &l, nil
}

func (db *DB) CreateProcess(name, description, productionState string, counterPLC, counterTag string, counterEnabled bool) (int64, error) {
	if productionState == "" {
		productionState = "active_production"
	}
	res, err := db.Exec(`INSERT INTO processes (name, description, production_state, counter_plc_name, counter_tag_name, counter_enabled) VALUES (?, ?, ?, ?, ?, ?)`,
		name, description, productionState, counterPLC, counterTag, counterEnabled)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdateProcess(id int64, name, description, productionState string, counterPLC, counterTag string, counterEnabled bool) error {
	if productionState == "" {
		productionState = "active_production"
	}
	_, err := db.Exec(`UPDATE processes SET name=?, description=?, production_state=?, counter_plc_name=?, counter_tag_name=?, counter_enabled=? WHERE id=?`,
		name, description, productionState, counterPLC, counterTag, counterEnabled, id)
	return err
}

func (db *DB) DeleteProcess(id int64) error {
	_, err := db.Exec(`DELETE FROM processes WHERE id=?`, id)
	return err
}

func (db *DB) SetActiveStyle(processID int64, styleID *int64) error {
	_, err := db.Exec(`UPDATE processes SET active_style_id=? WHERE id=?`, styleID, processID)
	return err
}

func (db *DB) SetTargetStyle(processID int64, styleID *int64) error {
	_, err := db.Exec(`UPDATE processes SET target_style_id=? WHERE id=?`, styleID, processID)
	return err
}

func (db *DB) GetActiveStyleID(processID int64) (*int64, error) {
	var id *int64
	err := db.QueryRow(`SELECT active_style_id FROM processes WHERE id = ?`, processID).Scan(&id)
	if err != nil {
		return nil, err
	}
	return id, nil
}

func (db *DB) SetProcessProductionState(processID int64, state string) error {
	_, err := db.Exec(`UPDATE processes SET production_state=? WHERE id=?`, state, processID)
	return err
}
