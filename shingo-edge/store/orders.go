package store

import (
	"database/sql"
	"time"
)

// Order represents an E-Kanban order.
type Order struct {
	ID             int64     `json:"id"`
	UUID           string    `json:"uuid"`
	OrderType      string    `json:"order_type"`
	Status         string    `json:"status"`
	PayloadID      *int64    `json:"payload_id"`
	RetrieveEmpty  bool      `json:"retrieve_empty"`
	Quantity       int64     `json:"quantity"`
	DeliveryNode   string    `json:"delivery_node"`
	StagingNode    string    `json:"staging_node"`
	PickupNode     string    `json:"pickup_node"`
	LoadType       string    `json:"load_type"`
	WaybillID      *string   `json:"waybill_id"`
	ExternalRef    *string   `json:"external_ref"`
	FinalCount     *int64    `json:"final_count"`
	CountConfirmed bool      `json:"count_confirmed"`
	ETA            *string   `json:"eta"`
	AutoConfirm    bool      `json:"auto_confirm"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`

	// Joined fields
	PayloadDesc     string `json:"payload_desc"`
	PayloadLocation string `json:"payload_location"`
	BlueprintCode   string `json:"blueprint_code"`
	LineName        string `json:"line_name"`
}

// OrderHistory records a status transition.
type OrderHistory struct {
	ID        int64     `json:"id"`
	OrderID   int64     `json:"order_id"`
	OldStatus string    `json:"old_status"`
	NewStatus string    `json:"new_status"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}

const orderSelectCols = `o.id, o.uuid, o.order_type, o.status, o.payload_id, o.retrieve_empty, o.quantity,
	o.delivery_node, o.staging_node, o.pickup_node, o.load_type,
	o.waybill_id, o.external_ref, o.final_count,
	o.count_confirmed, o.eta, o.auto_confirm, o.created_at, o.updated_at,
	COALESCE(p.description, ''), COALESCE(p.location, ''), COALESCE(p.blueprint_code, ''), COALESCE(pl.name, '')`

const orderJoin = `FROM orders o
	LEFT JOIN payloads p ON p.id = o.payload_id
	LEFT JOIN job_styles js ON js.id = p.job_style_id
	LEFT JOIN production_lines pl ON pl.id = js.line_id`

func (db *DB) ListOrders() ([]Order, error) {
	rows, err := db.Query(`SELECT ` + orderSelectCols + ` ` + orderJoin + ` ORDER BY o.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

func (db *DB) ListActiveOrders() ([]Order, error) {
	rows, err := db.Query(`SELECT ` + orderSelectCols + ` ` + orderJoin + `
		WHERE o.status NOT IN ('confirmed', 'cancelled')
		ORDER BY o.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

func (db *DB) CountActiveOrders() int {
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM orders WHERE status NOT IN ('confirmed', 'cancelled', 'failed')`).Scan(&count)
	return count
}

func (db *DB) ListActiveOrdersByLine(lineID int64) ([]Order, error) {
	rows, err := db.Query(`SELECT `+orderSelectCols+` `+orderJoin+`
		WHERE o.status NOT IN ('confirmed', 'cancelled')
		AND pl.id = ?
		ORDER BY o.created_at DESC`, lineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

func scanOrders(rows *sql.Rows) ([]Order, error) {
	var orders []Order
	for rows.Next() {
		var o Order
		var createdAt, updatedAt string
		if err := rows.Scan(&o.ID, &o.UUID, &o.OrderType, &o.Status, &o.PayloadID, &o.RetrieveEmpty, &o.Quantity,
			&o.DeliveryNode, &o.StagingNode, &o.PickupNode, &o.LoadType,
			&o.WaybillID, &o.ExternalRef, &o.FinalCount,
			&o.CountConfirmed, &o.ETA, &o.AutoConfirm, &createdAt, &updatedAt,
			&o.PayloadDesc, &o.PayloadLocation, &o.BlueprintCode, &o.LineName); err != nil {
			return nil, err
		}
		o.CreatedAt = scanTime(createdAt)
		o.UpdatedAt = scanTime(updatedAt)
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

func scanOrder(o *Order, scanner interface{ Scan(...interface{}) error }) error {
	var createdAt, updatedAt string
	if err := scanner.Scan(&o.ID, &o.UUID, &o.OrderType, &o.Status, &o.PayloadID, &o.RetrieveEmpty, &o.Quantity,
		&o.DeliveryNode, &o.StagingNode, &o.PickupNode, &o.LoadType,
		&o.WaybillID, &o.ExternalRef, &o.FinalCount,
		&o.CountConfirmed, &o.ETA, &o.AutoConfirm, &createdAt, &updatedAt,
		&o.PayloadDesc, &o.PayloadLocation, &o.BlueprintCode, &o.LineName); err != nil {
		return err
	}
	o.CreatedAt = scanTime(createdAt)
	o.UpdatedAt = scanTime(updatedAt)
	return nil
}

func (db *DB) GetOrder(id int64) (*Order, error) {
	o := &Order{}
	if err := scanOrder(o, db.QueryRow(`SELECT `+orderSelectCols+` `+orderJoin+` WHERE o.id = ?`, id)); err != nil {
		return nil, err
	}
	return o, nil
}

func (db *DB) GetOrderByUUID(uuid string) (*Order, error) {
	o := &Order{}
	if err := scanOrder(o, db.QueryRow(`SELECT `+orderSelectCols+` `+orderJoin+` WHERE o.uuid = ?`, uuid)); err != nil {
		return nil, err
	}
	return o, nil
}

func (db *DB) CreateOrder(uuid, orderType string, payloadID *int64, retrieveEmpty bool, quantity int64, deliveryNode, stagingNode, pickupNode, loadType string, autoConfirm bool) (int64, error) {
	res, err := db.Exec(`
		INSERT INTO orders (uuid, order_type, payload_id, retrieve_empty, quantity, delivery_node, staging_node, pickup_node, load_type, auto_confirm)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid, orderType, payloadID, retrieveEmpty, quantity, deliveryNode, stagingNode, pickupNode, loadType, autoConfirm)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdateOrderStatus(id int64, newStatus string) error {
	_, err := db.Exec(`UPDATE orders SET status=?, updated_at=datetime('now','localtime') WHERE id=?`, newStatus, id)
	return err
}

func (db *DB) UpdateOrderWaybill(id int64, waybillID, eta string) error {
	_, err := db.Exec(`UPDATE orders SET waybill_id=?, eta=?, updated_at=datetime('now','localtime') WHERE id=?`, waybillID, eta, id)
	return err
}

func (db *DB) UpdateOrderETA(id int64, eta string) error {
	_, err := db.Exec(`UPDATE orders SET eta=?, updated_at=datetime('now','localtime') WHERE id=?`, eta, id)
	return err
}

func (db *DB) UpdateOrderFinalCount(id int64, finalCount int64, confirmed bool) error {
	_, err := db.Exec(`UPDATE orders SET final_count=?, count_confirmed=?, updated_at=datetime('now','localtime') WHERE id=?`, finalCount, confirmed, id)
	return err
}

func (db *DB) UpdateOrderDeliveryNode(id int64, deliveryNode string) error {
	_, err := db.Exec(`UPDATE orders SET delivery_node=?, updated_at=datetime('now','localtime') WHERE id=?`, deliveryNode, id)
	return err
}

func (db *DB) UpdateOrderStepsJSON(id int64, stepsJSON string) error {
	_, err := db.Exec(`UPDATE orders SET steps_json=?, updated_at=datetime('now','localtime') WHERE id=?`, stepsJSON, id)
	return err
}

func (db *DB) InsertOrderHistory(orderID int64, oldStatus, newStatus, detail string) error {
	_, err := db.Exec(`INSERT INTO order_history (order_id, old_status, new_status, detail) VALUES (?, ?, ?, ?)`,
		orderID, oldStatus, newStatus, detail)
	return err
}

func (db *DB) ListOrderHistory(orderID int64) ([]OrderHistory, error) {
	rows, err := db.Query(`SELECT id, order_id, old_status, new_status, detail, created_at FROM order_history WHERE order_id = ? ORDER BY created_at`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var history []OrderHistory
	for rows.Next() {
		var h OrderHistory
		var createdAt string
		if err := rows.Scan(&h.ID, &h.OrderID, &h.OldStatus, &h.NewStatus, &h.Detail, &createdAt); err != nil {
			return nil, err
		}
		h.CreatedAt = scanTime(createdAt)
		history = append(history, h)
	}
	return history, rows.Err()
}
