package store

import (
	"database/sql"
	"fmt"
	"time"
)

// MaterialSlot statuses
const (
	SlotActive       = "active"
	SlotEmpty        = "empty"
	SlotReplenishing = "replenishing"
)

// MaterialSlot roles
const (
	RoleConsume = "consume"
	RoleProduce = "produce"
)

// Cycle modes
const (
	CycleModeSequential  = "sequential"
	CycleModeSingleRobot = "single_robot"
	CycleModeTwoRobot    = "two_robot"
)

// MaterialSlot represents a material slot at an LSL node for a style.
type MaterialSlot struct {
	ID           int64     `json:"id"`
	StyleID      int64     `json:"style_id"`
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
	OutgoingNode        string `json:"outgoing_node"`
	OutgoingNodeGroup   string `json:"outgoing_node_group"`

	// Joined
	StyleName string `json:"style_name"`
	Capacity  int    `json:"capacity"`
}

func scanSlots(rows *sql.Rows) ([]MaterialSlot, error) {
	var slots []MaterialSlot
	for rows.Next() {
		var s MaterialSlot
		var createdAt, updatedAt string
		if err := rows.Scan(
			&s.ID, &s.StyleID, &s.Location, &s.StagingNode,
			&s.Description, &s.PayloadCode, &s.Role, &s.AutoReorder,
			&s.Remaining, &s.ReorderPoint, &s.Status,
			&createdAt, &updatedAt,
			&s.CycleMode, &s.StagingNodeGroup,
			&s.StagingNode2, &s.StagingNode2Group,
			&s.FullPickupNode, &s.FullPickupNodeGroup,
			&s.OutgoingNode, &s.OutgoingNodeGroup,
			&s.StyleName,
			&s.Capacity,
		); err != nil {
			return nil, err
		}
		s.CreatedAt = scanTime(createdAt)
		s.UpdatedAt = scanTime(updatedAt)
		slots = append(slots, s)
	}
	return slots, rows.Err()
}

const slotSelectCols = `s.id, s.job_style_id, s.location, s.staging_node,
	s.description, s.payload_code, s.role, s.auto_reorder,
	s.remaining, s.reorder_point, s.status,
	s.created_at, s.updated_at,
	s.cycle_mode, s.staging_node_group,
	s.staging_node_2, s.staging_node_2_group,
	s.full_pickup_node, s.full_pickup_node_group,
	s.outgoing_node, s.outgoing_node_group,
	COALESCE(js.name, ''),
	COALESCE(pc.uop_capacity, 0)`

const slotJoin = `FROM material_slots s
	LEFT JOIN styles js ON js.id = s.job_style_id
	LEFT JOIN payload_catalog pc ON pc.code = s.payload_code`

func (db *DB) ListSlots() ([]MaterialSlot, error) {
	rows, err := db.Query(`SELECT ` + slotSelectCols + ` ` + slotJoin + ` ORDER BY js.name, s.location`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSlots(rows)
}

func (db *DB) ListSlotsByStyle(styleID int64) ([]MaterialSlot, error) {
	rows, err := db.Query(`SELECT `+slotSelectCols+` `+slotJoin+` WHERE s.job_style_id = ? ORDER BY s.location`, styleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSlots(rows)
}

func (db *DB) ListProduceSlots() ([]MaterialSlot, error) {
	rows, err := db.Query(`SELECT ` + slotSelectCols + ` ` + slotJoin + ` WHERE s.role = 'produce' ORDER BY s.location`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSlots(rows)
}

func (db *DB) ListActiveSlotsByStyle(styleID int64) ([]MaterialSlot, error) {
	rows, err := db.Query(`SELECT `+slotSelectCols+` `+slotJoin+` WHERE s.job_style_id = ? AND s.status IN ('active', 'replenishing') ORDER BY s.location`, styleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSlots(rows)
}

func (db *DB) GetSlot(id int64) (*MaterialSlot, error) {
	s := &MaterialSlot{}
	var createdAt, updatedAt string
	err := db.QueryRow(`SELECT `+slotSelectCols+` `+slotJoin+` WHERE s.id = ?`, id).
		Scan(
			&s.ID, &s.StyleID, &s.Location, &s.StagingNode,
			&s.Description, &s.PayloadCode, &s.Role, &s.AutoReorder,
			&s.Remaining, &s.ReorderPoint, &s.Status,
			&createdAt, &updatedAt,
			&s.CycleMode, &s.StagingNodeGroup,
			&s.StagingNode2, &s.StagingNode2Group,
			&s.FullPickupNode, &s.FullPickupNodeGroup,
			&s.OutgoingNode, &s.OutgoingNodeGroup,
			&s.StyleName,
			&s.Capacity,
		)
	if err != nil {
		return nil, err
	}
	s.CreatedAt = scanTime(createdAt)
	s.UpdatedAt = scanTime(updatedAt)
	return s, nil
}

// MaterialSlotInput holds all user-configurable fields for creating or updating a material slot.
type MaterialSlotInput struct {
	StyleID             int64  `json:"style_id"`
	Location            string `json:"location"`
	StagingNode         string `json:"staging_node"`
	Description         string `json:"description"`
	PayloadCode         string `json:"payload_code"` // mandatory — links to Core catalog for UOP capacity
	Role                string `json:"role"`
	AutoReorder         bool   `json:"auto_reorder"`
	ReorderPoint        int    `json:"reorder_point"`
	CycleMode           string `json:"cycle_mode"` // "sequential", "two_robot", "single_robot"
	StagingNodeGroup    string `json:"staging_node_group"`
	StagingNode2        string `json:"staging_node_2"`
	StagingNode2Group   string `json:"staging_node_2_group"`
	FullPickupNode      string `json:"full_pickup_node"`
	FullPickupNodeGroup string `json:"full_pickup_node_group"`
	OutgoingNode        string `json:"outgoing_node"`
	OutgoingNodeGroup   string `json:"outgoing_node_group"`
}

func (db *DB) CreateSlot(s MaterialSlotInput) (int64, error) {
	if s.Role == "" {
		s.Role = "consume"
	}
	if s.CycleMode == "" {
		s.CycleMode = "sequential"
	}
	if s.PayloadCode == "" {
		return 0, fmt.Errorf("payload_code is required")
	}
	res, err := db.Exec(`
		INSERT INTO material_slots (job_style_id, location, staging_node, description, payload_code,
			role, auto_reorder, reorder_point, cycle_mode,
			staging_node_group, staging_node_2, staging_node_2_group,
			full_pickup_node, full_pickup_node_group, outgoing_node, outgoing_node_group)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.StyleID, s.Location, s.StagingNode, s.Description, s.PayloadCode,
		s.Role, s.AutoReorder, s.ReorderPoint, s.CycleMode,
		s.StagingNodeGroup, s.StagingNode2, s.StagingNode2Group,
		s.FullPickupNode, s.FullPickupNodeGroup, s.OutgoingNode, s.OutgoingNodeGroup)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdateSlot(id int64, s MaterialSlotInput) error {
	if s.Role == "" {
		s.Role = "consume"
	}
	if s.CycleMode == "" {
		s.CycleMode = "sequential"
	}
	_, err := db.Exec(`
		UPDATE material_slots SET location=?, staging_node=?, description=?, payload_code=?,
			role=?, auto_reorder=?, reorder_point=?, cycle_mode=?,
			staging_node_group=?, staging_node_2=?, staging_node_2_group=?,
			full_pickup_node=?, full_pickup_node_group=?, outgoing_node=?, outgoing_node_group=?,
			updated_at=datetime('now')
		WHERE id=?`,
		s.Location, s.StagingNode, s.Description, s.PayloadCode,
		s.Role, s.AutoReorder, s.ReorderPoint, s.CycleMode,
		s.StagingNodeGroup, s.StagingNode2, s.StagingNode2Group,
		s.FullPickupNode, s.FullPickupNodeGroup, s.OutgoingNode, s.OutgoingNodeGroup,
		id)
	return err
}

func (db *DB) UpdateSlotRemaining(id int64, remaining int, status string) error {
	_, err := db.Exec(`UPDATE material_slots SET remaining=?, status=?, updated_at=datetime('now') WHERE id=?`,
		remaining, status, id)
	return err
}

func (db *DB) ResetSlot(id int64, uopCapacity int) error {
	_, err := db.Exec(`UPDATE material_slots SET remaining=?, status='active', updated_at=datetime('now') WHERE id=?`,
		uopCapacity, id)
	return err
}

func (db *DB) UpdateSlotReorderPoint(id int64, reorderPoint int) error {
	_, err := db.Exec(`UPDATE material_slots SET reorder_point=?, updated_at=datetime('now') WHERE id=?`,
		reorderPoint, id)
	return err
}

func (db *DB) UpdateSlotAutoReorder(id int64, autoReorder bool) error {
	_, err := db.Exec(`UPDATE material_slots SET auto_reorder=?, updated_at=datetime('now') WHERE id=?`,
		autoReorder, id)
	return err
}

func (db *DB) DeleteSlot(id int64) error {
	_, err := db.Exec(`DELETE FROM material_slots WHERE id=?`, id)
	return err
}
