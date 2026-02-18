package store

import (
	"database/sql"
	"fmt"
	"time"
)

type Order struct {
	ID            int64
	EdgeUUID   string
	ClientID      string
	FactoryID     string
	OrderType     string
	Status        string
	MaterialID    *int64
	MaterialCode  string
	Quantity      float64
	SourceNodeID  *int64
	DestNodeID    *int64
	PickupNode    string
	DeliveryNode  string
	RDSOrderID    string
	RDSState      string
	RobotID       string
	Priority      int
	PayloadDesc   string
	ErrorDetail   string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	CompletedAt   *time.Time
	PayloadTypeID *int64
	PayloadID     *int64
}

type OrderHistory struct {
	ID        int64
	OrderID   int64
	Status    string
	Detail    string
	CreatedAt time.Time
}

const orderSelectCols = `id, edge_uuid, client_id, factory_id, order_type, status, material_id, material_code, quantity, source_node_id, dest_node_id, pickup_node, delivery_node, rds_order_id, rds_state, robot_id, priority, payload_desc, error_detail, created_at, updated_at, completed_at, payload_type_id, payload_id`

func scanOrder(row interface{ Scan(...any) error }) (*Order, error) {
	var o Order
	var materialID, sourceNodeID, destNodeID, payloadTypeID, payloadID sql.NullInt64
	var createdAt, updatedAt string
	var completedAt sql.NullString

	err := row.Scan(&o.ID, &o.EdgeUUID, &o.ClientID, &o.FactoryID, &o.OrderType, &o.Status,
		&materialID, &o.MaterialCode, &o.Quantity, &sourceNodeID, &destNodeID,
		&o.PickupNode, &o.DeliveryNode, &o.RDSOrderID, &o.RDSState, &o.RobotID,
		&o.Priority, &o.PayloadDesc, &o.ErrorDetail, &createdAt, &updatedAt, &completedAt,
		&payloadTypeID, &payloadID)
	if err != nil {
		return nil, err
	}
	if materialID.Valid {
		o.MaterialID = &materialID.Int64
	}
	if sourceNodeID.Valid {
		o.SourceNodeID = &sourceNodeID.Int64
	}
	if destNodeID.Valid {
		o.DestNodeID = &destNodeID.Int64
	}
	if payloadTypeID.Valid {
		o.PayloadTypeID = &payloadTypeID.Int64
	}
	if payloadID.Valid {
		o.PayloadID = &payloadID.Int64
	}
	o.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
	o.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt)
	if completedAt.Valid {
		t, _ := time.Parse("2006-01-02 15:04:05", completedAt.String)
		o.CompletedAt = &t
	}
	return &o, nil
}

func scanOrders(rows *sql.Rows) ([]*Order, error) {
	var orders []*Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

func (db *DB) CreateOrder(o *Order) error {
	var matID, srcID, dstID, ptID, pID any
	if o.MaterialID != nil {
		matID = *o.MaterialID
	}
	if o.SourceNodeID != nil {
		srcID = *o.SourceNodeID
	}
	if o.DestNodeID != nil {
		dstID = *o.DestNodeID
	}
	if o.PayloadTypeID != nil {
		ptID = *o.PayloadTypeID
	}
	if o.PayloadID != nil {
		pID = *o.PayloadID
	}

	result, err := db.Exec(db.Q(`INSERT INTO orders (edge_uuid, client_id, factory_id, order_type, status, material_id, material_code, quantity, source_node_id, dest_node_id, pickup_node, delivery_node, priority, payload_desc, payload_type_id, payload_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		o.EdgeUUID, o.ClientID, o.FactoryID, o.OrderType, o.Status,
		matID, o.MaterialCode, o.Quantity, srcID, dstID,
		o.PickupNode, o.DeliveryNode, o.Priority, o.PayloadDesc, ptID, pID)
	if err != nil {
		return fmt.Errorf("create order: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("create order last id: %w", err)
	}
	o.ID = id
	return nil
}

func (db *DB) UpdateOrderStatus(id int64, status, detail string) error {
	_, err := db.Exec(db.Q(`UPDATE orders SET status=?, error_detail=?, updated_at=datetime('now','localtime') WHERE id=?`),
		status, detail, id)
	if err != nil {
		return err
	}
	_, err = db.Exec(db.Q(`INSERT INTO order_history (order_id, status, detail) VALUES (?, ?, ?)`),
		id, status, detail)
	return err
}

func (db *DB) UpdateOrderRDS(id int64, rdsOrderID, rdsState, robotID string) error {
	_, err := db.Exec(db.Q(`UPDATE orders SET rds_order_id=?, rds_state=?, robot_id=?, updated_at=datetime('now','localtime') WHERE id=?`),
		rdsOrderID, rdsState, robotID, id)
	return err
}

func (db *DB) UpdateOrderSourceNode(id, sourceNodeID int64) error {
	_, err := db.Exec(db.Q(`UPDATE orders SET source_node_id=?, updated_at=datetime('now','localtime') WHERE id=?`),
		sourceNodeID, id)
	return err
}

func (db *DB) CompleteOrder(id int64) error {
	_, err := db.Exec(db.Q(`UPDATE orders SET status='completed', completed_at=datetime('now','localtime'), updated_at=datetime('now','localtime') WHERE id=?`), id)
	if err != nil {
		return err
	}
	_, err = db.Exec(db.Q(`INSERT INTO order_history (order_id, status, detail) VALUES (?, 'completed', 'order completed')`), id)
	return err
}

func (db *DB) GetOrder(id int64) (*Order, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM orders WHERE id=?`, orderSelectCols)), id)
	return scanOrder(row)
}

func (db *DB) GetOrderByUUID(uuid string) (*Order, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM orders WHERE edge_uuid=? ORDER BY id DESC LIMIT 1`, orderSelectCols)), uuid)
	return scanOrder(row)
}

func (db *DB) GetOrderByRDSID(rdsOrderID string) (*Order, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM orders WHERE rds_order_id=? LIMIT 1`, orderSelectCols)), rdsOrderID)
	return scanOrder(row)
}

func (db *DB) ListOrders(status string, limit int) ([]*Order, error) {
	var rows *sql.Rows
	var err error
	if status != "" {
		rows, err = db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM orders WHERE status=? ORDER BY id DESC LIMIT ?`, orderSelectCols)), status, limit)
	} else {
		rows, err = db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM orders ORDER BY id DESC LIMIT ?`, orderSelectCols)), limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

func (db *DB) ListActiveOrders() ([]*Order, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM orders WHERE status NOT IN ('completed', 'failed', 'cancelled') ORDER BY id DESC`, orderSelectCols)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

func (db *DB) ListOrderHistory(orderID int64) ([]*OrderHistory, error) {
	rows, err := db.Query(db.Q(`SELECT id, order_id, status, detail, created_at FROM order_history WHERE order_id=? ORDER BY id`), orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var history []*OrderHistory
	for rows.Next() {
		var h OrderHistory
		var createdAt string
		if err := rows.Scan(&h.ID, &h.OrderID, &h.Status, &h.Detail, &createdAt); err != nil {
			return nil, err
		}
		h.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		history = append(history, &h)
	}
	return history, rows.Err()
}

func (db *DB) UpdateOrderPriority(id int64, priority int) error {
	_, err := db.Exec(db.Q(`UPDATE orders SET priority=?, updated_at=datetime('now','localtime') WHERE id=?`),
		priority, id)
	return err
}

// ListDispatchedRDSOrderIDs returns RDS order IDs for all non-terminal orders.
func (db *DB) ListDispatchedRDSOrderIDs() ([]string, error) {
	rows, err := db.Query(`SELECT rds_order_id FROM orders WHERE rds_order_id != '' AND status IN ('dispatched', 'in_transit')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
