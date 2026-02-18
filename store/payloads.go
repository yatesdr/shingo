package store

import (
	"database/sql"
	"fmt"
	"time"
)

type Payload struct {
	ID            int64
	PayloadTypeID int64
	NodeID        *int64
	Status        string
	ClaimedBy     *int64
	DeliveredAt   time.Time
	Notes         string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	// Joined fields
	PayloadTypeName string
	FormFactor      string
	NodeName        string
}

const payloadSelectCols = `p.id, p.payload_type_id, p.node_id, p.status, p.claimed_by, p.delivered_at, p.notes, p.created_at, p.updated_at`

const payloadJoinQuery = `SELECT p.id, p.payload_type_id, p.node_id, p.status, p.claimed_by, p.delivered_at, p.notes, p.created_at, p.updated_at,
	pt.name, pt.form_factor, COALESCE(n.name, '')
	FROM payloads p
	JOIN payload_types pt ON pt.id = p.payload_type_id
	LEFT JOIN nodes n ON n.id = p.node_id`

func scanPayload(row interface{ Scan(...any) error }, withJoins bool) (*Payload, error) {
	var p Payload
	var nodeID, claimedBy sql.NullInt64
	var deliveredAt, createdAt, updatedAt string

	if withJoins {
		err := row.Scan(&p.ID, &p.PayloadTypeID, &nodeID, &p.Status, &claimedBy, &deliveredAt, &p.Notes, &createdAt, &updatedAt,
			&p.PayloadTypeName, &p.FormFactor, &p.NodeName)
		if err != nil {
			return nil, err
		}
	} else {
		err := row.Scan(&p.ID, &p.PayloadTypeID, &nodeID, &p.Status, &claimedBy, &deliveredAt, &p.Notes, &createdAt, &updatedAt)
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
	p.DeliveredAt, _ = time.Parse("2006-01-02 15:04:05", deliveredAt)
	p.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
	p.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt)
	return &p, nil
}

func scanPayloads(rows *sql.Rows, withJoins bool) ([]*Payload, error) {
	var payloads []*Payload
	for rows.Next() {
		p, err := scanPayload(rows, withJoins)
		if err != nil {
			return nil, err
		}
		payloads = append(payloads, p)
	}
	return payloads, rows.Err()
}

func (db *DB) CreatePayload(p *Payload) error {
	var nodeID any
	if p.NodeID != nil {
		nodeID = *p.NodeID
	}
	result, err := db.Exec(db.Q(`INSERT INTO payloads (payload_type_id, node_id, status, notes) VALUES (?, ?, ?, ?)`),
		p.PayloadTypeID, nodeID, p.Status, p.Notes)
	if err != nil {
		return fmt.Errorf("create payload: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("create payload last id: %w", err)
	}
	p.ID = id
	return nil
}

func (db *DB) UpdatePayload(p *Payload) error {
	var nodeID any
	if p.NodeID != nil {
		nodeID = *p.NodeID
	}
	_, err := db.Exec(db.Q(`UPDATE payloads SET payload_type_id=?, node_id=?, status=?, notes=?, updated_at=datetime('now','localtime') WHERE id=?`),
		p.PayloadTypeID, nodeID, p.Status, p.Notes, p.ID)
	return err
}

func (db *DB) DeletePayload(id int64) error {
	_, err := db.Exec(db.Q(`DELETE FROM payloads WHERE id=?`), id)
	return err
}

func (db *DB) GetPayload(id int64) (*Payload, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`%s WHERE p.id=?`, payloadJoinQuery)), id)
	return scanPayload(row, true)
}

func (db *DB) ListPayloads() ([]*Payload, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`%s ORDER BY p.id DESC`, payloadJoinQuery)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPayloads(rows, true)
}

func (db *DB) ListPayloadsByStatus(status string) ([]*Payload, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`%s WHERE p.status=? ORDER BY p.id DESC`, payloadJoinQuery)), status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPayloads(rows, true)
}

func (db *DB) ListPayloadsByNode(nodeID int64) ([]*Payload, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`%s WHERE p.node_id=? ORDER BY p.id DESC`, payloadJoinQuery)), nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPayloads(rows, true)
}

func (db *DB) CountPayloadsByNode(nodeID int64) (int, error) {
	var count int
	err := db.QueryRow(db.Q(`SELECT COUNT(*) FROM payloads WHERE node_id=?`), nodeID).Scan(&count)
	return count, err
}

// ClaimPayload marks a payload as claimed by an order to prevent double-dispatch.
func (db *DB) ClaimPayload(payloadID, orderID int64) error {
	_, err := db.Exec(db.Q(`UPDATE payloads SET claimed_by=?, updated_at=datetime('now','localtime') WHERE id=?`), orderID, payloadID)
	return err
}

// UnclaimPayload releases a payload from an order claim.
func (db *DB) UnclaimPayload(payloadID int64) error {
	_, err := db.Exec(db.Q(`UPDATE payloads SET claimed_by=NULL, updated_at=datetime('now','localtime') WHERE id=?`), payloadID)
	return err
}

// MovePayload moves a payload to a new node, updating delivered_at.
func (db *DB) MovePayload(payloadID, toNodeID int64) error {
	_, err := db.Exec(db.Q(`UPDATE payloads SET node_id=?, delivered_at=datetime('now','localtime'), updated_at=datetime('now','localtime') WHERE id=?`), toNodeID, payloadID)
	return err
}

// FindSourcePayloadFIFO finds the best unclaimed payload at an enabled storage node using FIFO.
func (db *DB) FindSourcePayloadFIFO(payloadTypeCode string) (*Payload, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`%s
		WHERE pt.name = ?
		  AND n.node_type = 'storage'
		  AND n.enabled = 1
		  AND p.claimed_by IS NULL
		  AND p.status = 'available'
		ORDER BY p.delivered_at ASC
		LIMIT 1`, payloadJoinQuery)), payloadTypeCode)
	return scanPayload(row, true)
}

// FindStorageDestinationForPayload finds the best storage node for a payload type.
// Prefers nodes that already have this payload type (consolidation), then emptiest.
func (db *DB) FindStorageDestinationForPayload(payloadTypeID int64) (*Node, error) {
	// Try consolidation: storage nodes that already have this payload type with capacity remaining.
	row := db.QueryRow(db.Q(fmt.Sprintf(`
		SELECT %s FROM nodes WHERE id = (
			SELECT n.id
			FROM nodes n
			JOIN payloads match ON match.node_id = n.id AND match.payload_type_id = ?
			LEFT JOIN payloads total ON total.node_id = n.id
			WHERE n.node_type = 'storage' AND n.enabled = 1 AND n.capacity > 0
			GROUP BY n.id, n.capacity
			HAVING COUNT(DISTINCT total.id) < n.capacity
			ORDER BY COUNT(DISTINCT match.id) DESC
			LIMIT 1
		)`, nodeSelectCols)), payloadTypeID)
	n, err := scanNode(row)
	if err == nil {
		return n, nil
	}

	// Fall back to emptiest storage node with capacity
	row = db.QueryRow(db.Q(fmt.Sprintf(`
		SELECT %s FROM nodes WHERE id = (
			SELECT n.id
			FROM nodes n
			LEFT JOIN payloads p ON p.node_id = n.id
			WHERE n.node_type = 'storage' AND n.enabled = 1 AND n.capacity > 0
			GROUP BY n.id, n.capacity
			HAVING COUNT(p.id) < n.capacity
			ORDER BY COUNT(p.id) ASC
			LIMIT 1
		)`, nodeSelectCols)))
	return scanNode(row)
}

// ListPayloadsByClaimedOrder returns all payloads claimed by a specific order.
func (db *DB) ListPayloadsByClaimedOrder(orderID int64) ([]*Payload, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`%s WHERE p.claimed_by=?`, payloadJoinQuery)), orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPayloads(rows, true)
}
