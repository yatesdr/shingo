package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type Order struct {
	ID            int64      `json:"id"`
	EdgeUUID      string     `json:"edge_uuid"`
	StationID     string     `json:"station_id"`
	OrderType     string     `json:"order_type"`
	Status        string     `json:"status"`
	Quantity      int64      `json:"quantity"`
	SourceNode    string     `json:"source_node"`
	DeliveryNode  string     `json:"delivery_node"`
	VendorOrderID string     `json:"vendor_order_id"`
	VendorState   string     `json:"vendor_state"`
	RobotID       string     `json:"robot_id"`
	Priority      int        `json:"priority"`
	PayloadDesc   string     `json:"payload_desc"`
	ErrorDetail   string     `json:"error_detail"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	ParentOrderID *int64     `json:"parent_order_id,omitempty"`
	Sequence      int        `json:"sequence"`
	StepsJSON     string     `json:"steps_json,omitempty"`
	BinID         *int64     `json:"bin_id,omitempty"`
	PayloadCode   string     `json:"payload_code"`
}

type OrderHistory struct {
	ID        int64     `json:"id"`
	OrderID   int64     `json:"order_id"`
	Status    string    `json:"status"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}

const orderSelectCols = `id, edge_uuid, station_id, order_type, status, quantity, source_node, delivery_node, vendor_order_id, vendor_state, robot_id, priority, payload_desc, error_detail, created_at, updated_at, completed_at, parent_order_id, sequence, steps_json, bin_id, payload_code`

func scanOrder(row interface{ Scan(...any) error }) (*Order, error) {
	var o Order
	var parentOrderID, binID sql.NullInt64

	err := row.Scan(&o.ID, &o.EdgeUUID, &o.StationID, &o.OrderType, &o.Status,
		&o.Quantity,
		&o.SourceNode, &o.DeliveryNode, &o.VendorOrderID, &o.VendorState, &o.RobotID,
		&o.Priority, &o.PayloadDesc, &o.ErrorDetail, &o.CreatedAt, &o.UpdatedAt, &o.CompletedAt,
		&parentOrderID, &o.Sequence, &o.StepsJSON, &binID, &o.PayloadCode)
	if err != nil {
		return nil, err
	}
	if parentOrderID.Valid {
		o.ParentOrderID = &parentOrderID.Int64
	}
	if binID.Valid {
		o.BinID = &binID.Int64
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
	id, err := db.insertID(`INSERT INTO orders (edge_uuid, station_id, order_type, status, quantity, source_node, delivery_node, priority, payload_desc, parent_order_id, sequence, steps_json, bin_id, payload_code) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING id`,
		o.EdgeUUID, o.StationID, o.OrderType, o.Status,
		o.Quantity,
		o.SourceNode, o.DeliveryNode, o.Priority, o.PayloadDesc,
		nullableInt64(o.ParentOrderID), o.Sequence, o.StepsJSON,
		nullableInt64(o.BinID), o.PayloadCode)
	if err != nil {
		return fmt.Errorf("create order: %w", err)
	}
	o.ID = id
	return nil
}

// CompoundChild describes a child order to create in a compound order transaction.
type CompoundChild struct {
	Order *Order
	BinID int64 // bin to claim for this child
}

// CreateCompoundChildren creates all child orders and claims their payloads in a single transaction.
func (db *DB) CreateCompoundChildren(children []CompoundChild) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	for _, c := range children {
		o := c.Order
		var id int64
		err := tx.QueryRow(`INSERT INTO orders (edge_uuid, station_id, order_type, status, quantity, source_node, delivery_node, priority, payload_desc, parent_order_id, sequence, steps_json, bin_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING id`,
			o.EdgeUUID, o.StationID, o.OrderType, o.Status,
			o.Quantity,
			o.SourceNode, o.DeliveryNode, o.Priority, o.PayloadDesc,
			nullableInt64(o.ParentOrderID), o.Sequence, o.StepsJSON,
			nullableInt64(o.BinID)).Scan(&id)
		if err != nil {
			return fmt.Errorf("create child order (seq %d): %w", o.Sequence, err)
		}
		o.ID = id

		// Bin-centric claiming: if the child order has a bin, claim it
		if o.BinID != nil {
			_, err = tx.Exec(`UPDATE bins SET claimed_by=$1 WHERE id=$2`, o.ID, *o.BinID)
			if err != nil {
				return fmt.Errorf("claim bin %d for child %d: %w", *o.BinID, o.ID, err)
			}
		}
	}

	return tx.Commit()
}

// ListChildOrders returns all child orders for a parent order.
func (db *DB) ListChildOrders(parentOrderID int64) ([]*Order, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM orders WHERE parent_order_id=$1 ORDER BY sequence`, orderSelectCols), parentOrderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

// GetNextChildOrder returns the next pending child order for a parent.
func (db *DB) GetNextChildOrder(parentOrderID int64) (*Order, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM orders WHERE parent_order_id=$1 AND status='pending' ORDER BY sequence LIMIT 1`, orderSelectCols), parentOrderID)
	return scanOrder(row)
}

func (db *DB) UpdateOrderStatus(id int64, status, detail string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Only persist detail into error_detail for terminal error statuses;
	// clear it on normal transitions so the UI doesn't show stale error text.
	errDetail := ""
	if status == "failed" || status == "cancelled" {
		errDetail = detail
	}
	if _, err := tx.Exec(`UPDATE orders SET status=$1, error_detail=$2, updated_at=NOW() WHERE id=$3`, status, errDetail, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO order_history (order_id, status, detail) VALUES ($1, $2, $3)`, id, status, detail); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) UpdateOrderVendor(id int64, vendorOrderID, vendorState, robotID string) error {
	_, err := db.Exec(`UPDATE orders SET vendor_order_id=$1, vendor_state=$2, robot_id=$3, updated_at=NOW() WHERE id=$4`,
		vendorOrderID, vendorState, robotID, id)
	return err
}

func (db *DB) UpdateOrderSourceNode(id int64, sourceNode string) error {
	_, err := db.Exec(`UPDATE orders SET source_node=$1, updated_at=NOW() WHERE id=$2`,
		sourceNode, id)
	return err
}

func (db *DB) UpdateOrderDeliveryNode(id int64, deliveryNode string) error {
	_, err := db.Exec(`UPDATE orders SET delivery_node=$1, updated_at=NOW() WHERE id=$2`,
		deliveryNode, id)
	return err
}

func (db *DB) CompleteOrder(id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE orders SET status='confirmed', completed_at=NOW(), updated_at=NOW() WHERE id=$1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO order_history (order_id, status, detail) VALUES ($1, 'confirmed', 'order confirmed')`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) GetOrder(id int64) (*Order, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM orders WHERE id=$1`, orderSelectCols), id)
	return scanOrder(row)
}

func (db *DB) GetOrderByUUID(uuid string) (*Order, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM orders WHERE edge_uuid=$1 ORDER BY id DESC LIMIT 1`, orderSelectCols), uuid)
	return scanOrder(row)
}

func (db *DB) GetOrderByVendorID(vendorOrderID string) (*Order, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM orders WHERE vendor_order_id=$1 LIMIT 1`, orderSelectCols), vendorOrderID)
	return scanOrder(row)
}

func (db *DB) ListOrders(status string, limit int) ([]*Order, error) {
	var rows *sql.Rows
	var err error
	if status != "" {
		rows, err = db.Query(fmt.Sprintf(`SELECT %s FROM orders WHERE status=$1 ORDER BY id DESC LIMIT $2`, orderSelectCols), status, limit)
	} else {
		rows, err = db.Query(fmt.Sprintf(`SELECT %s FROM orders ORDER BY id DESC LIMIT $1`, orderSelectCols), limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

// OrderFilter supports filtered, paginated order queries.
type OrderFilter struct {
	Statuses  []string   // filter by status IN (...); empty = all
	StationID string     // filter by station_id; empty = all
	Since     *time.Time // filter by created_at >= since
	Limit     int        // max rows; 0 = default 100
	Offset    int        // pagination offset
}

// ListOrdersFiltered returns orders matching the given filter with pagination.
func (db *DB) ListOrdersFiltered(f OrderFilter) ([]*Order, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	query := fmt.Sprintf(`SELECT %s FROM orders WHERE 1=1`, orderSelectCols)
	args := []any{}
	n := 0

	if len(f.Statuses) > 0 {
		placeholders := make([]string, len(f.Statuses))
		for i, s := range f.Statuses {
			n++
			placeholders[i] = fmt.Sprintf("$%d", n)
			args = append(args, s)
		}
		query += fmt.Sprintf(` AND status IN (%s)`, strings.Join(placeholders, ", "))
	}
	if f.StationID != "" {
		n++
		query += fmt.Sprintf(` AND station_id = $%d`, n)
		args = append(args, f.StationID)
	}
	if f.Since != nil {
		n++
		query += fmt.Sprintf(` AND created_at >= $%d`, n)
		args = append(args, *f.Since)
	}

	n++
	query += fmt.Sprintf(` ORDER BY id DESC LIMIT $%d`, n)
	args = append(args, f.Limit)
	n++
	query += fmt.Sprintf(` OFFSET $%d`, n)
	args = append(args, f.Offset)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

func (db *DB) ListActiveOrders() ([]*Order, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM orders WHERE status NOT IN ('confirmed', 'failed', 'cancelled') ORDER BY id DESC`, orderSelectCols))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

func (db *DB) ListOrderHistory(orderID int64) ([]*OrderHistory, error) {
	rows, err := db.Query(`SELECT id, order_id, status, detail, created_at FROM order_history WHERE order_id=$1 ORDER BY id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var history []*OrderHistory
	for rows.Next() {
		var h OrderHistory
		if err := rows.Scan(&h.ID, &h.OrderID, &h.Status, &h.Detail, &h.CreatedAt); err != nil {
			return nil, err
		}
		history = append(history, &h)
	}
	return history, rows.Err()
}

func (db *DB) UpdateOrderPriority(id int64, priority int) error {
	_, err := db.Exec(`UPDATE orders SET priority=$1, updated_at=NOW() WHERE id=$2`,
		priority, id)
	return err
}

func (db *DB) ListOrdersByStation(stationID string, limit int) ([]*Order, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM orders WHERE station_id=$1 ORDER BY id DESC LIMIT $2`, orderSelectCols), stationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

// CountActiveOrdersByDeliveryNode counts non-terminal orders targeting a specific delivery node.
func (db *DB) CountActiveOrdersByDeliveryNode(nodeName string) (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM orders WHERE delivery_node=$1 AND status NOT IN ('confirmed','failed','cancelled')`, nodeName).Scan(&count)
	return count, err
}

// ListDispatchedVendorOrderIDs returns vendor order IDs for all non-terminal orders.
func (db *DB) ListDispatchedVendorOrderIDs() ([]string, error) {
	rows, err := db.Query(`SELECT vendor_order_id FROM orders WHERE vendor_order_id != '' AND status IN ('dispatched', 'in_transit', 'staged')`)
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

// ListQueuedOrders returns all orders in "queued" status, oldest first (FIFO).
func (db *DB) ListQueuedOrders() ([]*Order, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM orders WHERE status = 'queued' ORDER BY created_at ASC`, orderSelectCols))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

// UpdateOrderPayloadCode sets the payload_code on an order.
func (db *DB) UpdateOrderPayloadCode(orderID int64, payloadCode string) error {
	_, err := db.Exec(`UPDATE orders SET payload_code = $1, updated_at = NOW() WHERE id = $2`, payloadCode, orderID)
	return err
}

// CountInFlightOrdersByDeliveryNode counts non-queued, non-terminal active orders targeting a delivery node.
func (db *DB) CountInFlightOrdersByDeliveryNode(deliveryNode string) (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM orders WHERE delivery_node = $1 AND status NOT IN ('queued', 'confirmed', 'cancelled', 'failed')`, deliveryNode).Scan(&count)
	return count, err
}
