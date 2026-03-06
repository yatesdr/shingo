package store

import (
	"database/sql"
	"time"
)

// Payload represents a payload slot at an LSL node for a job style.
type Payload struct {
	ID              int64     `json:"id"`
	JobStyleID      int64     `json:"job_style_id"`
	Location        string    `json:"location"`
	StagingNode     string    `json:"staging_node"`
	Description     string    `json:"description"`
	Manifest        string    `json:"manifest"`
	Multiplier      float64   `json:"multiplier"`
	ProductionUnits int       `json:"production_units"`
	Remaining       int       `json:"remaining"`
	ReorderPoint    int       `json:"reorder_point"`
	ReorderQty      int       `json:"reorder_qty"`
	RetrieveEmpty   bool      `json:"retrieve_empty"`
	Status          string    `json:"status"`
	BlueprintCode   string    `json:"blueprint_code"`
	AutoReorder     bool      `json:"auto_reorder"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	// Joined
	JobStyleName string `json:"job_style_name"`
}

func scanPayloads(rows *sql.Rows) ([]Payload, error) {
	var payloads []Payload
	for rows.Next() {
		var p Payload
		var createdAt, updatedAt string
		if err := rows.Scan(&p.ID, &p.JobStyleID, &p.Location, &p.StagingNode,
			&p.Description, &p.Manifest, &p.Multiplier, &p.ProductionUnits,
			&p.Remaining, &p.ReorderPoint, &p.ReorderQty, &p.RetrieveEmpty,
			&p.Status, &p.BlueprintCode, &p.AutoReorder,
			&createdAt, &updatedAt, &p.JobStyleName); err != nil {
			return nil, err
		}
		p.CreatedAt = scanTime(createdAt)
		p.UpdatedAt = scanTime(updatedAt)
		payloads = append(payloads, p)
	}
	return payloads, rows.Err()
}

const payloadSelectCols = `p.id, p.job_style_id, p.location, p.staging_node,
	p.description, p.manifest, p.multiplier, p.production_units,
	p.remaining, p.reorder_point, p.reorder_qty, p.retrieve_empty,
	p.status, p.blueprint_code, p.auto_reorder,
	p.created_at, p.updated_at, COALESCE(js.name, '')`

const payloadJoin = `FROM payloads p LEFT JOIN job_styles js ON js.id = p.job_style_id`

func (db *DB) ListPayloads() ([]Payload, error) {
	rows, err := db.Query(`SELECT ` + payloadSelectCols + ` ` + payloadJoin + ` ORDER BY js.name, p.location`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPayloads(rows)
}

func (db *DB) ListPayloadsByJobStyle(jobStyleID int64) ([]Payload, error) {
	rows, err := db.Query(`SELECT `+payloadSelectCols+` `+payloadJoin+` WHERE p.job_style_id = ? ORDER BY p.location`, jobStyleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPayloads(rows)
}

func (db *DB) ListActivePayloadsByJobStyle(jobStyleID int64) ([]Payload, error) {
	rows, err := db.Query(`SELECT `+payloadSelectCols+` `+payloadJoin+` WHERE p.job_style_id = ? AND p.status IN ('active', 'replenishing') ORDER BY p.location`, jobStyleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPayloads(rows)
}

func (db *DB) GetPayload(id int64) (*Payload, error) {
	p := &Payload{}
	var createdAt, updatedAt string
	err := db.QueryRow(`SELECT `+payloadSelectCols+` `+payloadJoin+` WHERE p.id = ?`, id).
		Scan(&p.ID, &p.JobStyleID, &p.Location, &p.StagingNode,
			&p.Description, &p.Manifest, &p.Multiplier, &p.ProductionUnits,
			&p.Remaining, &p.ReorderPoint, &p.ReorderQty, &p.RetrieveEmpty,
			&p.Status, &p.BlueprintCode, &p.AutoReorder,
			&createdAt, &updatedAt, &p.JobStyleName)
	if err != nil {
		return nil, err
	}
	p.CreatedAt = scanTime(createdAt)
	p.UpdatedAt = scanTime(updatedAt)
	return p, nil
}

func (db *DB) CreatePayload(jobStyleID int64, location, stagingNode, description, manifest string, multiplier float64, productionUnits, remaining, reorderPoint, reorderQty int, retrieveEmpty bool, blueprintCode string) (int64, error) {
	res, err := db.Exec(`
		INSERT INTO payloads (job_style_id, location, staging_node, description, manifest, multiplier, production_units, remaining, reorder_point, reorder_qty, retrieve_empty, blueprint_code)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		jobStyleID, location, stagingNode, description, manifest, multiplier, productionUnits, remaining, reorderPoint, reorderQty, retrieveEmpty, blueprintCode)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdatePayload(id int64, location, stagingNode, description, manifest string, multiplier float64, productionUnits, remaining, reorderPoint, reorderQty int, retrieveEmpty bool, blueprintCode string) error {
	_, err := db.Exec(`
		UPDATE payloads SET location=?, staging_node=?, description=?, manifest=?, multiplier=?,
			production_units=?, remaining=?, reorder_point=?, reorder_qty=?, retrieve_empty=?,
			blueprint_code=?, updated_at=datetime('now','localtime')
		WHERE id=?`,
		location, stagingNode, description, manifest, multiplier,
		productionUnits, remaining, reorderPoint, reorderQty, retrieveEmpty, blueprintCode, id)
	return err
}

func (db *DB) UpdatePayloadRemaining(id int64, remaining int, status string) error {
	_, err := db.Exec(`UPDATE payloads SET remaining=?, status=?, updated_at=datetime('now','localtime') WHERE id=?`,
		remaining, status, id)
	return err
}

func (db *DB) ResetPayload(id int64, productionUnits int) error {
	_, err := db.Exec(`UPDATE payloads SET remaining=?, status='active', updated_at=datetime('now','localtime') WHERE id=?`,
		productionUnits, id)
	return err
}

func (db *DB) UpdatePayloadReorderPoint(id int64, reorderPoint int) error {
	_, err := db.Exec(`UPDATE payloads SET reorder_point=?, updated_at=datetime('now','localtime') WHERE id=?`,
		reorderPoint, id)
	return err
}

func (db *DB) UpdatePayloadAutoReorder(id int64, autoReorder bool) error {
	_, err := db.Exec(`UPDATE payloads SET auto_reorder=?, updated_at=datetime('now','localtime') WHERE id=?`,
		autoReorder, id)
	return err
}


func (db *DB) DeletePayload(id int64) error {
	_, err := db.Exec(`DELETE FROM payloads WHERE id=?`, id)
	return err
}
