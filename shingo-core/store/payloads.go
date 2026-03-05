package store

import (
	"database/sql"
	"fmt"
	"time"
)

type PayloadInstance struct {
	ID            int64      `json:"id"`
	StyleID       int64      `json:"style_id"`
	NodeID        *int64     `json:"node_id,omitempty"`
	TagID         string     `json:"tag_id"`
	Status        string     `json:"status"`
	UOPRemaining  int        `json:"uop_remaining"`
	ClaimedBy     *int64     `json:"claimed_by,omitempty"`
	LoadedAt      *time.Time `json:"loaded_at,omitempty"`
	DeliveredAt   time.Time  `json:"delivered_at"`
	Notes         string     `json:"notes"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	// Joined fields
	StyleName  string `json:"style_name"`
	FormFactor string `json:"form_factor"`
	NodeName   string `json:"node_name"`
}

const instanceJoinQuery = `SELECT p.id, p.style_id, p.node_id, p.tag_id, p.status, p.uop_remaining, p.claimed_by, p.loaded_at, p.delivered_at, p.notes, p.created_at, p.updated_at,
	ps.name, ps.form_factor, COALESCE(n.name, '')
	FROM payload_instances p
	JOIN payload_styles ps ON ps.id = p.style_id
	LEFT JOIN nodes n ON n.id = p.node_id`

func scanInstance(row interface{ Scan(...any) error }, withJoins bool) (*PayloadInstance, error) {
	var p PayloadInstance
	var nodeID, claimedBy sql.NullInt64
	var loadedAt, deliveredAt, createdAt, updatedAt any

	if withJoins {
		err := row.Scan(&p.ID, &p.StyleID, &nodeID, &p.TagID, &p.Status, &p.UOPRemaining, &claimedBy,
			&loadedAt, &deliveredAt, &p.Notes, &createdAt, &updatedAt,
			&p.StyleName, &p.FormFactor, &p.NodeName)
		if err != nil {
			return nil, err
		}
	} else {
		err := row.Scan(&p.ID, &p.StyleID, &nodeID, &p.TagID, &p.Status, &p.UOPRemaining, &claimedBy,
			&loadedAt, &deliveredAt, &p.Notes, &createdAt, &updatedAt)
		if err != nil {
			return nil, err
		}
	}

	if nodeID.Valid {
		p.NodeID = &nodeID.Int64
	}
	if claimedBy.Valid {
		p.ClaimedBy = &claimedBy.Int64
	}
	p.LoadedAt = parseTimePtr(loadedAt)
	p.DeliveredAt = parseTime(deliveredAt)
	p.CreatedAt = parseTime(createdAt)
	p.UpdatedAt = parseTime(updatedAt)
	return &p, nil
}

func scanInstances(rows *sql.Rows, withJoins bool) ([]*PayloadInstance, error) {
	var instances []*PayloadInstance
	for rows.Next() {
		p, err := scanInstance(rows, withJoins)
		if err != nil {
			return nil, err
		}
		instances = append(instances, p)
	}
	return instances, rows.Err()
}

func (db *DB) CreateInstance(p *PayloadInstance) error {
	result, err := db.Exec(db.Q(`INSERT INTO payload_instances (style_id, node_id, tag_id, status, uop_remaining, notes) VALUES (?, ?, ?, ?, ?, ?)`),
		p.StyleID, nullableInt64(p.NodeID), p.TagID, p.Status, p.UOPRemaining, p.Notes)
	if err != nil {
		return fmt.Errorf("create instance: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("create instance last id: %w", err)
	}
	p.ID = id
	db.logInstanceEvent(id, InstanceEventCreated, fmt.Sprintf("style_id=%d status=%s", p.StyleID, p.Status))
	return nil
}

func (db *DB) UpdateInstance(p *PayloadInstance) error {
	_, err := db.Exec(db.Q(`UPDATE payload_instances SET style_id=?, node_id=?, tag_id=?, status=?, uop_remaining=?, notes=?, updated_at=datetime('now','localtime') WHERE id=?`),
		p.StyleID, nullableInt64(p.NodeID), p.TagID, p.Status, p.UOPRemaining, p.Notes, p.ID)
	return err
}

func (db *DB) DeleteInstance(id int64) error {
	_, err := db.Exec(db.Q(`DELETE FROM payload_instances WHERE id=?`), id)
	return err
}

func (db *DB) GetInstance(id int64) (*PayloadInstance, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`%s WHERE p.id=?`, instanceJoinQuery)), id)
	return scanInstance(row, true)
}

func (db *DB) GetInstanceByTag(tagID string) (*PayloadInstance, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`%s WHERE p.tag_id=?`, instanceJoinQuery)), tagID)
	return scanInstance(row, true)
}

func (db *DB) ListInstances() ([]*PayloadInstance, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`%s ORDER BY p.id DESC`, instanceJoinQuery)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanInstances(rows, true)
}

func (db *DB) ListInstancesByStatus(status string) ([]*PayloadInstance, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`%s WHERE p.status=? ORDER BY p.id DESC`, instanceJoinQuery)), status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanInstances(rows, true)
}

func (db *DB) ListInstancesByNode(nodeID int64) ([]*PayloadInstance, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`%s WHERE p.node_id=? ORDER BY p.id DESC`, instanceJoinQuery)), nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanInstances(rows, true)
}

func (db *DB) CountInstancesByNode(nodeID int64) (int, error) {
	var count int
	err := db.QueryRow(db.Q(`SELECT COUNT(*) FROM payload_instances WHERE node_id=?`), nodeID).Scan(&count)
	return count, err
}

// ClaimInstance marks an instance as claimed by an order to prevent double-dispatch.
func (db *DB) ClaimInstance(instanceID, orderID int64) error {
	_, err := db.Exec(db.Q(`UPDATE payload_instances SET claimed_by=?, updated_at=datetime('now','localtime') WHERE id=?`), orderID, instanceID)
	if err == nil {
		db.logInstanceEvent(instanceID, InstanceEventClaimed, fmt.Sprintf("order_id=%d", orderID))
	}
	return err
}

// UnclaimInstance releases an instance from an order claim.
func (db *DB) UnclaimInstance(instanceID int64) error {
	_, err := db.Exec(db.Q(`UPDATE payload_instances SET claimed_by=NULL, updated_at=datetime('now','localtime') WHERE id=?`), instanceID)
	if err == nil {
		db.logInstanceEvent(instanceID, InstanceEventUnclaimed, "")
	}
	return err
}

// MoveInstance moves an instance to a new node, updating delivered_at.
func (db *DB) MoveInstance(instanceID, toNodeID int64) error {
	_, err := db.Exec(db.Q(`UPDATE payload_instances SET node_id=?, delivered_at=datetime('now','localtime'), updated_at=datetime('now','localtime') WHERE id=?`), toNodeID, instanceID)
	if err == nil {
		db.logInstanceEvent(instanceID, InstanceEventMoved, fmt.Sprintf("to_node_id=%d", toNodeID))
	}
	return err
}

// UnclaimOrderInstances releases all instances claimed by a specific order.
func (db *DB) UnclaimOrderInstances(orderID int64) {
	instances, err := db.ListInstancesByClaimedOrder(orderID)
	if err != nil {
		return
	}
	for _, p := range instances {
		db.UnclaimInstance(p.ID)
	}
}

// FindSourceInstanceFIFO finds the best unclaimed instance at an enabled storage node using FIFO.
func (db *DB) FindSourceInstanceFIFO(styleCode string) (*PayloadInstance, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`%s
		LEFT JOIN node_types ntype ON ntype.id = n.node_type_id
		WHERE ps.name = ?
		  AND n.node_type = 'storage'
		  AND n.enabled = 1
		  AND COALESCE(ntype.is_synthetic, 0) = 0
		  AND p.claimed_by IS NULL
		  AND p.status = 'available'
		ORDER BY p.delivered_at ASC
		LIMIT 1`, instanceJoinQuery)), styleCode)
	return scanInstance(row, true)
}

// FindStorageDestinationForInstance finds the best storage node for an instance style.
func (db *DB) FindStorageDestinationForInstance(styleID int64) (*Node, error) {
	// Try consolidation: storage nodes that already have this style with capacity remaining.
	row := db.QueryRow(db.Q(fmt.Sprintf(`
		SELECT %s %s WHERE n.id = (
			SELECT sn.id
			FROM nodes sn
			LEFT JOIN node_types snt ON snt.id = sn.node_type_id
			JOIN payload_instances match ON match.node_id = sn.id AND match.style_id = ?
			LEFT JOIN payload_instances total ON total.node_id = sn.id
			WHERE sn.node_type = 'storage' AND sn.enabled = 1 AND sn.capacity > 0
			  AND COALESCE(snt.is_synthetic, 0) = 0
			GROUP BY sn.id, sn.capacity
			HAVING COUNT(DISTINCT total.id) < sn.capacity
			ORDER BY COUNT(DISTINCT match.id) DESC
			LIMIT 1
		)`, nodeSelectCols, nodeFromClause)), styleID)
	n, err := scanNode(row)
	if err == nil {
		return n, nil
	}

	// Fall back to emptiest storage node with capacity
	row = db.QueryRow(db.Q(fmt.Sprintf(`
		SELECT %s %s WHERE n.id = (
			SELECT sn.id
			FROM nodes sn
			LEFT JOIN node_types snt ON snt.id = sn.node_type_id
			LEFT JOIN payload_instances sp ON sp.node_id = sn.id
			WHERE sn.node_type = 'storage' AND sn.enabled = 1 AND sn.capacity > 0
			  AND COALESCE(snt.is_synthetic, 0) = 0
			GROUP BY sn.id, sn.capacity
			HAVING COUNT(sp.id) < sn.capacity
			ORDER BY COUNT(sp.id) ASC
			LIMIT 1
		)`, nodeSelectCols, nodeFromClause)))
	return scanNode(row)
}

// ListInstancesByClaimedOrder returns all instances claimed by a specific order.
func (db *DB) ListInstancesByClaimedOrder(orderID int64) ([]*PayloadInstance, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`%s WHERE p.claimed_by=?`, instanceJoinQuery)), orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanInstances(rows, true)
}
