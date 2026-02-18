package store

import (
	"database/sql"
	"fmt"
	"time"
)

type InventoryItem struct {
	ID            int64
	NodeID        int64
	MaterialID    int64
	Quantity      float64
	IsPartial     bool
	DeliveredAt   time.Time
	SourceOrderID *int64
	Metadata      string
	Notes         string
	ClaimedBy     *int64
	CreatedAt     time.Time
	// Joined fields
	MaterialCode string
	NodeName     string
}

const inventorySelectCols = `i.id, i.node_id, i.material_id, i.quantity, i.is_partial, i.delivered_at, i.source_order_id, i.metadata, i.notes, i.claimed_by, i.created_at`
const inventoryJoinCols = `i.id, i.node_id, i.material_id, i.quantity, i.is_partial, i.delivered_at, i.source_order_id, i.metadata, i.notes, i.claimed_by, i.created_at, m.code, n.name`

func scanInventoryItem(row interface{ Scan(...any) error }, withJoins bool) (*InventoryItem, error) {
	var item InventoryItem
	var isPartial int
	var deliveredAt, createdAt string
	var sourceOrderID, claimedBy sql.NullInt64

	var dest []any
	if withJoins {
		dest = []any{&item.ID, &item.NodeID, &item.MaterialID, &item.Quantity, &isPartial, &deliveredAt, &sourceOrderID, &item.Metadata, &item.Notes, &claimedBy, &createdAt, &item.MaterialCode, &item.NodeName}
	} else {
		dest = []any{&item.ID, &item.NodeID, &item.MaterialID, &item.Quantity, &isPartial, &deliveredAt, &sourceOrderID, &item.Metadata, &item.Notes, &claimedBy, &createdAt}
	}

	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	item.IsPartial = isPartial != 0
	item.DeliveredAt, _ = time.Parse("2006-01-02 15:04:05", deliveredAt)
	item.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
	if sourceOrderID.Valid {
		item.SourceOrderID = &sourceOrderID.Int64
	}
	if claimedBy.Valid {
		item.ClaimedBy = &claimedBy.Int64
	}
	return &item, nil
}

func scanInventoryItems(rows *sql.Rows, withJoins bool) ([]*InventoryItem, error) {
	var items []*InventoryItem
	for rows.Next() {
		item, err := scanInventoryItem(rows, withJoins)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (db *DB) AddInventory(nodeID, materialID int64, quantity float64, isPartial bool, sourceOrderID *int64, notes string) (int64, error) {
	var srcID any
	if sourceOrderID != nil {
		srcID = *sourceOrderID
	}
	result, err := db.Exec(db.Q(`INSERT INTO node_inventory (node_id, material_id, quantity, is_partial, source_order_id, notes) VALUES (?, ?, ?, ?, ?, ?)`),
		nodeID, materialID, quantity, boolToInt(isPartial), srcID, notes)
	if err != nil {
		return 0, fmt.Errorf("add inventory: %w", err)
	}
	return result.LastInsertId()
}

func (db *DB) RemoveInventory(id int64) error {
	_, err := db.Exec(db.Q(`DELETE FROM node_inventory WHERE id=?`), id)
	return err
}

func (db *DB) UpdateInventoryQuantity(id int64, quantity float64) error {
	_, err := db.Exec(db.Q(`UPDATE node_inventory SET quantity=? WHERE id=?`), quantity, id)
	return err
}

func (db *DB) ClaimInventory(inventoryID, orderID int64) error {
	_, err := db.Exec(db.Q(`UPDATE node_inventory SET claimed_by=? WHERE id=?`), orderID, inventoryID)
	return err
}

func (db *DB) UnclaimInventory(inventoryID int64) error {
	_, err := db.Exec(db.Q(`UPDATE node_inventory SET claimed_by=NULL WHERE id=?`), inventoryID)
	return err
}

func (db *DB) MoveInventory(id, toNodeID int64) error {
	_, err := db.Exec(db.Q(`UPDATE node_inventory SET node_id=?, delivered_at=datetime('now','localtime') WHERE id=?`), toNodeID, id)
	return err
}

func (db *DB) GetInventoryItem(id int64) (*InventoryItem, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM node_inventory i JOIN materials m ON i.material_id=m.id JOIN nodes n ON i.node_id=n.id WHERE i.id=?`, inventoryJoinCols)), id)
	return scanInventoryItem(row, true)
}

func (db *DB) ListNodeInventory(nodeID int64) ([]*InventoryItem, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM node_inventory i JOIN materials m ON i.material_id=m.id JOIN nodes n ON i.node_id=n.id WHERE i.node_id=? ORDER BY i.delivered_at`, inventoryJoinCols)), nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanInventoryItems(rows, true)
}

// FindSourceFIFO finds the best source inventory for a material using FIFO with partial priority.
// Excludes items already claimed by other orders.
func (db *DB) FindSourceFIFO(materialCode string) (*InventoryItem, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`
		SELECT %s
		FROM node_inventory i
		JOIN materials m ON i.material_id = m.id
		JOIN nodes n ON i.node_id = n.id
		WHERE m.code = ?
		  AND n.enabled = 1
		  AND n.node_type = 'storage'
		  AND i.claimed_by IS NULL
		ORDER BY i.is_partial DESC, i.delivered_at ASC
		LIMIT 1`, inventoryJoinCols)), materialCode)
	return scanInventoryItem(row, true)
}

// CountNodeInventory returns the current item count at a node.
func (db *DB) CountNodeInventory(nodeID int64) (int, error) {
	var count int
	err := db.QueryRow(db.Q(`SELECT COUNT(*) FROM node_inventory WHERE node_id=?`), nodeID).Scan(&count)
	return count, err
}

// FindStorageDestination finds the best storage node for a material.
// Prefers nodes that already have this material (consolidation), then emptiest.
func (db *DB) FindStorageDestination(materialID int64) (*Node, error) {
	// Try consolidation first: storage nodes that already have this material and still have capacity.
	// Uses two JOINs: one to check for matching material, one to count total items for capacity check.
	row := db.QueryRow(db.Q(fmt.Sprintf(`
		SELECT %s FROM nodes WHERE id = (
			SELECT n.id
			FROM nodes n
			JOIN node_inventory mat ON mat.node_id = n.id AND mat.material_id = ?
			LEFT JOIN node_inventory total ON total.node_id = n.id
			WHERE n.node_type = 'storage' AND n.enabled = 1 AND n.capacity > 0
			GROUP BY n.id, n.capacity
			HAVING COUNT(DISTINCT total.id) < n.capacity
			ORDER BY COUNT(DISTINCT mat.id) DESC
			LIMIT 1
		)`, nodeSelectCols)), materialID)
	n, err := scanNode(row)
	if err == nil {
		return n, nil
	}

	// Fall back to emptiest storage node with capacity
	row = db.QueryRow(db.Q(fmt.Sprintf(`
		SELECT %s FROM nodes WHERE id = (
			SELECT n.id
			FROM nodes n
			LEFT JOIN node_inventory ni ON ni.node_id = n.id
			WHERE n.node_type = 'storage' AND n.enabled = 1 AND n.capacity > 0
			GROUP BY n.id, n.capacity
			HAVING COUNT(ni.id) < n.capacity
			ORDER BY COUNT(ni.id) ASC
			LIMIT 1
		)`, nodeSelectCols)))
	return scanNode(row)
}
