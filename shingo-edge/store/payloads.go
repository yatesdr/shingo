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
	PayloadCode        string    `json:"payload_code"`
	AutoReorder        bool      `json:"auto_reorder"`
	Role               string    `json:"role"`
	AutoRemoveEmpties  bool      `json:"auto_remove_empties"`
	AutoOrderEmpties   bool      `json:"auto_order_empties"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`

	// Hot-swap configuration
	HotSwap             string `json:"hot_swap"`               // "", "two_robot", "single_robot"
	StagingNodeGroup    string `json:"staging_node_group"`
	StagingNode2        string `json:"staging_node_2"`
	StagingNode2Group   string `json:"staging_node_2_group"`
	FullPickupNode      string `json:"full_pickup_node"`
	FullPickupNodeGroup string `json:"full_pickup_node_group"`
	EmptyDropNode       string `json:"empty_drop_node"`
	EmptyDropNodeGroup  string `json:"empty_drop_node_group"`

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
			&p.Status, &p.PayloadCode, &p.AutoReorder,
			&p.Role, &p.AutoRemoveEmpties, &p.AutoOrderEmpties,
			&createdAt, &updatedAt,
			&p.HotSwap, &p.StagingNodeGroup,
			&p.StagingNode2, &p.StagingNode2Group,
			&p.FullPickupNode, &p.FullPickupNodeGroup,
			&p.EmptyDropNode, &p.EmptyDropNodeGroup,
			&p.JobStyleName); err != nil {
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
	p.status, p.payload_code, p.auto_reorder,
	p.role, p.auto_remove_empties, p.auto_order_empties,
	p.created_at, p.updated_at,
	p.hot_swap, p.staging_node_group,
	p.staging_node_2, p.staging_node_2_group,
	p.full_pickup_node, p.full_pickup_node_group,
	p.empty_drop_node, p.empty_drop_node_group,
	COALESCE(js.name, '')`

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

func (db *DB) ListProducePayloads() ([]Payload, error) {
	rows, err := db.Query(`SELECT ` + payloadSelectCols + ` ` + payloadJoin + ` WHERE p.role = 'produce' AND p.auto_order_empties = 1 ORDER BY p.location`)
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
			&p.Status, &p.PayloadCode, &p.AutoReorder,
			&p.Role, &p.AutoRemoveEmpties, &p.AutoOrderEmpties,
			&createdAt, &updatedAt,
			&p.HotSwap, &p.StagingNodeGroup,
			&p.StagingNode2, &p.StagingNode2Group,
			&p.FullPickupNode, &p.FullPickupNodeGroup,
			&p.EmptyDropNode, &p.EmptyDropNodeGroup,
			&p.JobStyleName)
	if err != nil {
		return nil, err
	}
	p.CreatedAt = scanTime(createdAt)
	p.UpdatedAt = scanTime(updatedAt)
	return p, nil
}

// PayloadInput holds all writable fields for creating or updating a payload.
// Using a struct avoids long parameter lists and prevents argument ordering bugs.
type PayloadInput struct {
	JobStyleID        int64
	Location          string
	StagingNode       string
	Description       string
	Manifest          string
	Multiplier        float64
	ProductionUnits   int
	Remaining         int
	ReorderPoint      int
	ReorderQty        int
	RetrieveEmpty     bool
	PayloadCode       string
	Role              string
	AutoRemoveEmpties bool
	AutoOrderEmpties  bool
	// Hot-swap configuration
	HotSwap             string
	StagingNodeGroup    string
	StagingNode2        string
	StagingNode2Group   string
	FullPickupNode      string
	FullPickupNodeGroup string
	EmptyDropNode       string
	EmptyDropNodeGroup  string
}

func (db *DB) CreatePayload(p PayloadInput) (int64, error) {
	if p.Role == "" {
		p.Role = "consume"
	}
	res, err := db.Exec(`
		INSERT INTO payloads (job_style_id, location, staging_node, description, manifest, multiplier,
			production_units, remaining, reorder_point, reorder_qty, retrieve_empty, payload_code, role,
			auto_remove_empties, auto_order_empties,
			hot_swap, staging_node_group, staging_node_2, staging_node_2_group,
			full_pickup_node, full_pickup_node_group, empty_drop_node, empty_drop_node_group)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.JobStyleID, p.Location, p.StagingNode, p.Description, p.Manifest, p.Multiplier,
		p.ProductionUnits, p.Remaining, p.ReorderPoint, p.ReorderQty, p.RetrieveEmpty,
		p.PayloadCode, p.Role, p.AutoRemoveEmpties, p.AutoOrderEmpties,
		p.HotSwap, p.StagingNodeGroup, p.StagingNode2, p.StagingNode2Group,
		p.FullPickupNode, p.FullPickupNodeGroup, p.EmptyDropNode, p.EmptyDropNodeGroup)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdatePayload(id int64, p PayloadInput) error {
	if p.Role == "" {
		p.Role = "consume"
	}
	_, err := db.Exec(`
		UPDATE payloads SET location=?, staging_node=?, description=?, manifest=?, multiplier=?,
			production_units=?, remaining=?, reorder_point=?, reorder_qty=?, retrieve_empty=?,
			payload_code=?, role=?, auto_remove_empties=?, auto_order_empties=?,
			hot_swap=?, staging_node_group=?, staging_node_2=?, staging_node_2_group=?,
			full_pickup_node=?, full_pickup_node_group=?, empty_drop_node=?, empty_drop_node_group=?,
			updated_at=datetime('now')
		WHERE id=?`,
		p.Location, p.StagingNode, p.Description, p.Manifest, p.Multiplier,
		p.ProductionUnits, p.Remaining, p.ReorderPoint, p.ReorderQty, p.RetrieveEmpty,
		p.PayloadCode, p.Role, p.AutoRemoveEmpties, p.AutoOrderEmpties,
		p.HotSwap, p.StagingNodeGroup, p.StagingNode2, p.StagingNode2Group,
		p.FullPickupNode, p.FullPickupNodeGroup, p.EmptyDropNode, p.EmptyDropNodeGroup,
		id)
	return err
}

func (db *DB) UpdatePayloadRemaining(id int64, remaining int, status string) error {
	_, err := db.Exec(`UPDATE payloads SET remaining=?, status=?, updated_at=datetime('now') WHERE id=?`,
		remaining, status, id)
	return err
}

func (db *DB) ResetPayload(id int64, productionUnits int) error {
	_, err := db.Exec(`UPDATE payloads SET remaining=?, status='active', updated_at=datetime('now') WHERE id=?`,
		productionUnits, id)
	return err
}

func (db *DB) UpdatePayloadReorderPoint(id int64, reorderPoint int) error {
	_, err := db.Exec(`UPDATE payloads SET reorder_point=?, updated_at=datetime('now') WHERE id=?`,
		reorderPoint, id)
	return err
}

func (db *DB) UpdatePayloadAutoReorder(id int64, autoReorder bool) error {
	_, err := db.Exec(`UPDATE payloads SET auto_reorder=?, updated_at=datetime('now') WHERE id=?`,
		autoReorder, id)
	return err
}


func (db *DB) DeletePayload(id int64) error {
	_, err := db.Exec(`DELETE FROM payloads WHERE id=?`, id)
	return err
}
