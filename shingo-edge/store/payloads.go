package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Payload represents a payload slot at an LSL node for a job style.
type Payload struct {
	ID           int64     `json:"id"`
	JobStyleID   int64     `json:"job_style_id"`
	Location     string    `json:"location"`
	StagingNode  string    `json:"staging_node"`
	Description  string    `json:"description"`
	PayloadCode  string    `json:"payload_code"`
	Role         string    `json:"role"`
	AutoReorder  bool      `json:"auto_reorder"`
	Remaining    int       `json:"remaining"`
	ReorderPoint int       `json:"reorder_point"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`

	// Cycle mode: "sequential", "two_robot", "single_robot"
	CycleMode           string `json:"cycle_mode"`
	StagingNodeGroup    string `json:"staging_node_group"`
	StagingNode2        string `json:"staging_node_2"`
	StagingNode2Group   string `json:"staging_node_2_group"`
	FullPickupNode      string `json:"full_pickup_node"`
	FullPickupNodeGroup string `json:"full_pickup_node_group"`
	OutgoingNode       string `json:"outgoing_node"`
	OutgoingNodeGroup  string `json:"outgoing_node_group"`

	// Joined
	JobStyleName string `json:"job_style_name"`
}

// 22 scan targets: 20 fields + 2 time strings
func scanPayloads(rows *sql.Rows) ([]Payload, error) {
	var payloads []Payload
	for rows.Next() {
		var p Payload
		var createdAt, updatedAt string
		if err := rows.Scan(
			&p.ID, &p.JobStyleID, &p.Location, &p.StagingNode,
			&p.Description, &p.PayloadCode, &p.Role, &p.AutoReorder,
			&p.Remaining, &p.ReorderPoint, &p.Status,
			&createdAt, &updatedAt,
			&p.CycleMode, &p.StagingNodeGroup,
			&p.StagingNode2, &p.StagingNode2Group,
			&p.FullPickupNode, &p.FullPickupNodeGroup,
			&p.OutgoingNode, &p.OutgoingNodeGroup,
			&p.JobStyleName,
		); err != nil {
			return nil, err
		}
		p.CreatedAt = scanTime(createdAt)
		p.UpdatedAt = scanTime(updatedAt)
		payloads = append(payloads, p)
	}
	return payloads, rows.Err()
}

const payloadSelectCols = `p.id, p.job_style_id, p.location, p.staging_node,
	p.description, p.payload_code, p.role, p.auto_reorder,
	p.remaining, p.reorder_point, p.status,
	p.created_at, p.updated_at,
	p.cycle_mode, p.staging_node_group,
	p.staging_node_2, p.staging_node_2_group,
	p.full_pickup_node, p.full_pickup_node_group,
	p.outgoing_node, p.outgoing_node_group,
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
	rows, err := db.Query(`SELECT ` + payloadSelectCols + ` ` + payloadJoin + ` WHERE p.role = 'produce' ORDER BY p.location`)
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
		Scan(
			&p.ID, &p.JobStyleID, &p.Location, &p.StagingNode,
			&p.Description, &p.PayloadCode, &p.Role, &p.AutoReorder,
			&p.Remaining, &p.ReorderPoint, &p.Status,
			&createdAt, &updatedAt,
			&p.CycleMode, &p.StagingNodeGroup,
			&p.StagingNode2, &p.StagingNode2Group,
			&p.FullPickupNode, &p.FullPickupNodeGroup,
			&p.OutgoingNode, &p.OutgoingNodeGroup,
			&p.JobStyleName,
		)
	if err != nil {
		return nil, err
	}
	p.CreatedAt = scanTime(createdAt)
	p.UpdatedAt = scanTime(updatedAt)
	return p, nil
}

// PayloadInput holds all user-configurable fields for creating or updating a payload.
type PayloadInput struct {
	JobStyleID   int64
	Location     string
	StagingNode  string
	Description  string
	PayloadCode  string // mandatory — links to Core catalog for UOP capacity
	Role         string
	AutoReorder  bool
	ReorderPoint int
	// Cycle mode: "sequential", "two_robot", "single_robot"
	CycleMode           string
	StagingNodeGroup    string
	StagingNode2        string
	StagingNode2Group   string
	FullPickupNode      string
	FullPickupNodeGroup string
	OutgoingNode       string
	OutgoingNodeGroup  string
}

func (db *DB) CreatePayload(p PayloadInput) (int64, error) {
	if p.Role == "" {
		p.Role = "consume"
	}
	if p.CycleMode == "" {
		p.CycleMode = "sequential"
	}
	if p.PayloadCode == "" {
		return 0, fmt.Errorf("payload_code is required")
	}
	res, err := db.Exec(`
		INSERT INTO payloads (job_style_id, location, staging_node, description, payload_code,
			role, auto_reorder, reorder_point, cycle_mode,
			staging_node_group, staging_node_2, staging_node_2_group,
			full_pickup_node, full_pickup_node_group, outgoing_node, outgoing_node_group)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.JobStyleID, p.Location, p.StagingNode, p.Description, p.PayloadCode,
		p.Role, p.AutoReorder, p.ReorderPoint, p.CycleMode,
		p.StagingNodeGroup, p.StagingNode2, p.StagingNode2Group,
		p.FullPickupNode, p.FullPickupNodeGroup, p.OutgoingNode, p.OutgoingNodeGroup)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdatePayload(id int64, p PayloadInput) error {
	if p.Role == "" {
		p.Role = "consume"
	}
	if p.CycleMode == "" {
		p.CycleMode = "sequential"
	}
	_, err := db.Exec(`
		UPDATE payloads SET location=?, staging_node=?, description=?, payload_code=?,
			role=?, auto_reorder=?, reorder_point=?, cycle_mode=?,
			staging_node_group=?, staging_node_2=?, staging_node_2_group=?,
			full_pickup_node=?, full_pickup_node_group=?, outgoing_node=?, outgoing_node_group=?,
			updated_at=datetime('now')
		WHERE id=?`,
		p.Location, p.StagingNode, p.Description, p.PayloadCode,
		p.Role, p.AutoReorder, p.ReorderPoint, p.CycleMode,
		p.StagingNodeGroup, p.StagingNode2, p.StagingNode2Group,
		p.FullPickupNode, p.FullPickupNodeGroup, p.OutgoingNode, p.OutgoingNodeGroup,
		id)
	return err
}

func (db *DB) UpdatePayloadRemaining(id int64, remaining int, status string) error {
	_, err := db.Exec(`UPDATE payloads SET remaining=?, status=?, updated_at=datetime('now') WHERE id=?`,
		remaining, status, id)
	return err
}

func (db *DB) ResetPayload(id int64, uopCapacity int) error {
	_, err := db.Exec(`UPDATE payloads SET remaining=?, status='active', updated_at=datetime('now') WHERE id=?`,
		uopCapacity, id)
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
