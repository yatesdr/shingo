// Package orders holds order-aggregate persistence for shingo-core.
//
// Stage 2D of the architecture plan moved order CRUD, history, filters,
// and the order_bins junction out of the flat store/ package and into
// this sub-package. The outer store/ keeps type aliases
// (`store.Order = orders.Order`, etc.) and one-line delegate methods on
// *store.DB so callers see no public API change. Cross-aggregate methods
// that mutate bins in the same transaction (CreateCompoundChildren,
// FailOrderAtomic, CancelOrderAtomic, ApplyBinArrival, ApplyMultiBinArrival)
// stay at the outer store/ level as composition methods.
package orders

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"shingocore/domain"
	"shingocore/store/internal/helpers"
)

// Order is the order domain entity. The struct lives in shingocore/domain
// (Stage 2A); this alias keeps the orders.Order name used by ScanOrder,
// Create/Update, the filter + list helpers, and the outer store/ orders.go
// re-export (store.Order). History also lifted to domain in Stage 2A.2 so
// www handlers can return order-with-history shapes without importing this
// sub-package; Filter stays local because it's a query DSL.
type Order = domain.Order

// History is the order-history audit row. The struct lives in
// shingocore/domain (Stage 2A.2); this alias keeps the orders.History
// name used by store readers and downstream callers.
type History = domain.OrderHistory

// SelectCols is exported so cross-aggregate readers at the outer store/
// level (e.g. ListOrdersByBin, which joins orders from the bin side) can
// reuse the column list.
const SelectCols = `id, edge_uuid, station_id, order_type, status, quantity, source_node, delivery_node, vendor_order_id, vendor_state, robot_id, priority, payload_desc, error_detail, created_at, updated_at, completed_at, parent_order_id, sequence, steps_json, bin_id, payload_code, wait_index`

// ScanOrder reads a single orders row.
// Exported for cross-aggregate readers at the outer store/ level.
func ScanOrder(row interface{ Scan(...any) error }) (*Order, error) {
	var o Order
	var parentOrderID, binID sql.NullInt64

	err := row.Scan(&o.ID, &o.EdgeUUID, &o.StationID, &o.OrderType, &o.Status,
		&o.Quantity,
		&o.SourceNode, &o.DeliveryNode, &o.VendorOrderID, &o.VendorState, &o.RobotID,
		&o.Priority, &o.PayloadDesc, &o.ErrorDetail, &o.CreatedAt, &o.UpdatedAt, &o.CompletedAt,
		&parentOrderID, &o.Sequence, &o.StepsJSON, &binID, &o.PayloadCode, &o.WaitIndex)
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

// ScanOrders reads all orders rows from a *sql.Rows.
func ScanOrders(rows *sql.Rows) ([]*Order, error) {
	var orders []*Order
	for rows.Next() {
		o, err := ScanOrder(rows)
		if err != nil {
			return nil, err
		}
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

// Create inserts a new order row and sets o.ID on success.
func Create(db *sql.DB, o *Order) error {
	id, err := helpers.InsertID(db, `INSERT INTO orders (edge_uuid, station_id, order_type, status, quantity, source_node, delivery_node, priority, payload_desc, parent_order_id, sequence, steps_json, bin_id, payload_code) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING id`,
		o.EdgeUUID, o.StationID, o.OrderType, o.Status,
		o.Quantity,
		o.SourceNode, o.DeliveryNode, o.Priority, o.PayloadDesc,
		helpers.NullableInt64(o.ParentOrderID), o.Sequence, o.StepsJSON,
		helpers.NullableInt64(o.BinID), o.PayloadCode)
	if err != nil {
		return fmt.Errorf("create order: %w", err)
	}
	o.ID = id
	return nil
}

// ListChildren returns all child orders for a parent order.
func ListChildren(db *sql.DB, parentOrderID int64) ([]*Order, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM orders WHERE parent_order_id=$1 ORDER BY sequence`, SelectCols), parentOrderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanOrders(rows)
}

// GetNextChild returns the next pending child order for a parent.
func GetNextChild(db *sql.DB, parentOrderID int64) (*Order, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM orders WHERE parent_order_id=$1 AND status='pending' ORDER BY sequence LIMIT 1`, SelectCols), parentOrderID)
	return ScanOrder(row)
}

// UpdateStatus transitions an order to a new status and records it in history.
// detail is persisted to error_detail only for terminal error statuses; it is
// cleared on normal transitions so the UI doesn't show stale error text.
func UpdateStatus(db *sql.DB, id int64, status, detail string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
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

// UpdateWaitIndex increments the wait_index for a complex order after
// releasing one wait segment.
func UpdateWaitIndex(db *sql.DB, id int64, waitIndex int) error {
	_, err := db.Exec(`UPDATE orders SET wait_index=$1, updated_at=NOW() WHERE id=$2`,
		waitIndex, id)
	return err
}

// UpdateVendor stores vendor-side identifiers on an order.
func UpdateVendor(db *sql.DB, id int64, vendorOrderID, vendorState, robotID string) error {
	_, err := db.Exec(`UPDATE orders SET vendor_order_id=$1, vendor_state=$2, robot_id=$3, updated_at=NOW() WHERE id=$4`,
		vendorOrderID, vendorState, robotID, id)
	return err
}

// UpdateSourceNode rewrites the source_node field.
func UpdateSourceNode(db *sql.DB, id int64, sourceNode string) error {
	_, err := db.Exec(`UPDATE orders SET source_node=$1, updated_at=NOW() WHERE id=$2`,
		sourceNode, id)
	return err
}

// UpdateDeliveryNode rewrites the delivery_node field.
func UpdateDeliveryNode(db *sql.DB, id int64, deliveryNode string) error {
	_, err := db.Exec(`UPDATE orders SET delivery_node=$1, updated_at=NOW() WHERE id=$2`,
		deliveryNode, id)
	return err
}

// Complete marks an order as completed (timestamp only; status transitions
// happen via UpdateStatus).
func Complete(db *sql.DB, id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE orders SET completed_at=NOW(), updated_at=NOW() WHERE id=$1`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// Get fetches an order by ID.
func Get(db *sql.DB, id int64) (*Order, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM orders WHERE id=$1`, SelectCols), id)
	return ScanOrder(row)
}

// GetByUUID fetches the most recent order for the given edge UUID.
func GetByUUID(db *sql.DB, uuid string) (*Order, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM orders WHERE edge_uuid=$1 ORDER BY id DESC LIMIT 1`, SelectCols), uuid)
	return ScanOrder(row)
}

// GetByVendorID fetches an order by its vendor-side order ID.
func GetByVendorID(db *sql.DB, vendorOrderID string) (*Order, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM orders WHERE vendor_order_id=$1 LIMIT 1`, SelectCols), vendorOrderID)
	return ScanOrder(row)
}

// List returns up to `limit` orders, optionally filtered by status.
func List(db *sql.DB, status string, limit int) ([]*Order, error) {
	var rows *sql.Rows
	var err error
	if status != "" {
		rows, err = db.Query(fmt.Sprintf(`SELECT %s FROM orders WHERE status=$1 ORDER BY id DESC LIMIT $2`, SelectCols), status, limit)
	} else {
		rows, err = db.Query(fmt.Sprintf(`SELECT %s FROM orders ORDER BY id DESC LIMIT $1`, SelectCols), limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanOrders(rows)
}

// Filter supports filtered, paginated order queries.
type Filter struct {
	Statuses  []string   // filter by status IN (...); empty = all
	StationID string     // filter by station_id; empty = all
	Since     *time.Time // filter by created_at >= since
	Limit     int        // max rows; 0 = default 100
	Offset    int        // pagination offset
}

// ListFiltered returns orders matching the given filter with pagination.
func ListFiltered(db *sql.DB, f Filter) ([]*Order, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	query := fmt.Sprintf(`SELECT %s FROM orders WHERE 1=1`, SelectCols)
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
	return ScanOrders(rows)
}

// ListActive returns all orders in non-terminal statuses.
func ListActive(db *sql.DB) ([]*Order, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM orders WHERE status NOT IN ('confirmed', 'failed', 'cancelled') ORDER BY id DESC`, SelectCols))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanOrders(rows)
}

// ListHistory returns the audit log entries for an order, oldest first.
func ListHistory(db *sql.DB, orderID int64) ([]*History, error) {
	rows, err := db.Query(`SELECT id, order_id, status, detail, created_at FROM order_history WHERE order_id=$1 ORDER BY id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var history []*History
	for rows.Next() {
		var h History
		if err := rows.Scan(&h.ID, &h.OrderID, &h.Status, &h.Detail, &h.CreatedAt); err != nil {
			return nil, err
		}
		history = append(history, &h)
	}
	return history, rows.Err()
}

// UpdatePriority rewrites the priority field.
func UpdatePriority(db *sql.DB, id int64, priority int) error {
	_, err := db.Exec(`UPDATE orders SET priority=$1, updated_at=NOW() WHERE id=$2`,
		priority, id)
	return err
}

// ListByStation returns up to `limit` orders targeting the given station.
func ListByStation(db *sql.DB, stationID string, limit int) ([]*Order, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM orders WHERE station_id=$1 ORDER BY id DESC LIMIT $2`, SelectCols), stationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanOrders(rows)
}

// CountActiveByDeliveryNode counts non-terminal orders targeting a delivery node.
func CountActiveByDeliveryNode(db *sql.DB, nodeName string) (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM orders WHERE delivery_node=$1 AND status NOT IN ('confirmed','failed','cancelled')`, nodeName).Scan(&count)
	return count, err
}

// ListDispatchedVendorOrderIDs returns vendor order IDs for orders currently
// dispatched, in transit, or staged.
func ListDispatchedVendorOrderIDs(db *sql.DB) ([]string, error) {
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

// ListActiveBySourceRef returns orders in pre-dispatch states (pending,
// sourcing, queued) whose source_node matches any of the provided names.
// Used by reparent/delete guards to detect orders that would break.
func ListActiveBySourceRef(db *sql.DB, names []string) ([]*Order, error) {
	if len(names) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(names))
	args := make([]any, len(names))
	for i, n := range names {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = n
	}
	q := fmt.Sprintf(`SELECT %s FROM orders WHERE source_node IN (%s) AND status IN ('pending', 'sourcing', 'queued') ORDER BY created_at ASC`,
		SelectCols, strings.Join(placeholders, ","))
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanOrders(rows)
}

// ListQueued returns all orders in "queued" status, oldest first (FIFO).
func ListQueued(db *sql.DB) ([]*Order, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM orders WHERE status = 'queued' ORDER BY created_at ASC`, SelectCols))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanOrders(rows)
}

// UpdatePayloadCode sets the payload_code on an order.
func UpdatePayloadCode(db *sql.DB, orderID int64, payloadCode string) error {
	_, err := db.Exec(`UPDATE orders SET payload_code = $1, updated_at = NOW() WHERE id = $2`, payloadCode, orderID)
	return err
}

// CountInFlightByDeliveryNode counts non-queued, non-terminal active orders
// targeting a delivery node.
func CountInFlightByDeliveryNode(db *sql.DB, deliveryNode string) (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM orders WHERE delivery_node = $1 AND status NOT IN ('queued', 'confirmed', 'cancelled', 'failed')`, deliveryNode).Scan(&count)
	return count, err
}

// UpdateRobotID rewrites just the robot_id field.
func UpdateRobotID(db *sql.DB, id int64, robotID string) error {
	_, err := db.Exec(`UPDATE orders SET robot_id=$1, updated_at=NOW() WHERE id=$2`, robotID, id)
	return err
}

// UpdateBinID sets the bin_id on an order.
// (Junction-style write against the orders table; bins-aggregate readers
// live at outer store/ as composition.)
func UpdateBinID(db *sql.DB, orderID, binID int64) error {
	_, err := db.Exec(`UPDATE orders SET bin_id=$1, updated_at=NOW() WHERE id=$2`, binID, orderID)
	return err
}

// ListByBinID returns recent orders involving a specific bin.
// Owned by orders/ because the return type is *Order.
func ListByBinID(db *sql.DB, binID int64, limit int) ([]*Order, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM orders WHERE bin_id=$1 ORDER BY id DESC LIMIT $2`, SelectCols), binID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanOrders(rows)
}
