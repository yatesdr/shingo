// Package orders holds E-Kanban order persistence for shingo-edge.
//
// Phase 5b of the architecture plan moved the orders + order_history
// CRUD out of the flat store/ package and into this sub-package. The
// outer store/ keeps type aliases (`store.Order = orders.Order`,
// `store.OrderHistory = orders.History`) and one-line delegate methods
// on *store.DB so external callers see no API change.
package orders

import (
	"database/sql"
	"log"
	"time"

	"shingoedge/domain"
	"shingoedge/store/internal/helpers"
)

// Order and History are the edge-order data types. The structs live
// in shingoedge/domain (Stage 2A.2); these aliases keep the
// orders.Order / orders.History names used by every scan helper,
// Insert/Update call site, and the outer store/ re-exports.
//
// The domain rename `History` → `OrderHistory` exists so the type is
// self-describing once outside this package; the alias here keeps
// the local orders.History name for backward compatibility.
type (
	Order   = domain.Order
	History = domain.OrderHistory
)

const selectCols = `o.id, o.uuid, o.order_type, o.status, o.process_node_id, o.retrieve_empty, o.quantity,
	o.delivery_node, o.staging_node, o.source_node, o.load_type,
	o.waybill_id, o.external_ref, o.final_count,
	o.count_confirmed, o.eta, o.auto_confirm, o.staged_expire_at, o.bin_uop_remaining, o.payload_code, o.created_at, o.updated_at,
	COALESCE(pl.name, ''), COALESCE(n.name, ''), COALESCE(os.name, '')`

const joinClause = `FROM orders o
	LEFT JOIN process_nodes n ON n.id = o.process_node_id
	LEFT JOIN operator_stations os ON os.id = n.operator_station_id
	LEFT JOIN processes pl ON pl.id = n.process_id`

// List returns every order, newest first.
func List(db *sql.DB) ([]Order, error) {
	rows, err := db.Query(`SELECT ` + selectCols + ` ` + joinClause + ` ORDER BY o.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

// ListActive returns every non-terminal order, newest first.
func ListActive(db *sql.DB) ([]Order, error) {
	rows, err := db.Query(`SELECT ` + selectCols + ` ` + joinClause + `
		WHERE o.status NOT IN ('confirmed', 'cancelled')
		ORDER BY o.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

// CountActive returns the count of non-terminal orders. Logs and
// returns 0 on error to keep dashboards alive.
func CountActive(db *sql.DB) int {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM orders WHERE status NOT IN ('confirmed', 'cancelled', 'failed')`).Scan(&count); err != nil {
		log.Printf("count active orders: %v", err)
		return 0
	}
	return count
}

// ListActiveByProcess returns non-terminal orders for one process.
func ListActiveByProcess(db *sql.DB, processID int64) ([]Order, error) {
	rows, err := db.Query(`SELECT `+selectCols+` `+joinClause+`
		WHERE o.status NOT IN ('confirmed', 'cancelled')
		AND pl.id = ?
		ORDER BY o.created_at DESC`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

func scanOrders(rows *sql.Rows) ([]Order, error) {
	var out []Order
	for rows.Next() {
		var o Order
		var stagedExpireAt sql.NullString
		var binUOP sql.NullInt64
		var createdAt, updatedAt string
		if err := rows.Scan(&o.ID, &o.UUID, &o.OrderType, &o.Status, &o.ProcessNodeID, &o.RetrieveEmpty, &o.Quantity,
			&o.DeliveryNode, &o.StagingNode, &o.SourceNode, &o.LoadType,
			&o.WaybillID, &o.ExternalRef, &o.FinalCount,
			&o.CountConfirmed, &o.ETA, &o.AutoConfirm, &stagedExpireAt, &binUOP, &o.PayloadCode, &createdAt, &updatedAt,
			&o.ProcessName, &o.ProcessNodeName, &o.StationName); err != nil {
			return nil, err
		}
		if stagedExpireAt.Valid {
			t := helpers.ScanTime(stagedExpireAt.String)
			o.StagedExpireAt = &t
		}
		if binUOP.Valid {
			v := int(binUOP.Int64)
			o.BinUOPRemaining = &v
		}
		o.CreatedAt = helpers.ScanTime(createdAt)
		o.UpdatedAt = helpers.ScanTime(updatedAt)
		out = append(out, o)
	}
	return out, rows.Err()
}

func scanOrder(o *Order, scanner interface{ Scan(...interface{}) error }) error {
	var stagedExpireAt sql.NullString
	var binUOP sql.NullInt64
	var createdAt, updatedAt string
	if err := scanner.Scan(&o.ID, &o.UUID, &o.OrderType, &o.Status, &o.ProcessNodeID, &o.RetrieveEmpty, &o.Quantity,
		&o.DeliveryNode, &o.StagingNode, &o.SourceNode, &o.LoadType,
		&o.WaybillID, &o.ExternalRef, &o.FinalCount,
		&o.CountConfirmed, &o.ETA, &o.AutoConfirm, &stagedExpireAt, &binUOP, &o.PayloadCode, &createdAt, &updatedAt,
		&o.ProcessName, &o.ProcessNodeName, &o.StationName); err != nil {
		return err
	}
	if stagedExpireAt.Valid {
		t := helpers.ScanTime(stagedExpireAt.String)
		o.StagedExpireAt = &t
	}
	if binUOP.Valid {
		v := int(binUOP.Int64)
		o.BinUOPRemaining = &v
	}
	o.CreatedAt = helpers.ScanTime(createdAt)
	o.UpdatedAt = helpers.ScanTime(updatedAt)
	return nil
}

// Get returns one order by id.
func Get(db *sql.DB, id int64) (*Order, error) {
	o := &Order{}
	if err := scanOrder(o, db.QueryRow(`SELECT `+selectCols+` `+joinClause+` WHERE o.id = ?`, id)); err != nil {
		return nil, err
	}
	return o, nil
}

// GetByUUID returns one order by uuid.
func GetByUUID(db *sql.DB, uuid string) (*Order, error) {
	o := &Order{}
	if err := scanOrder(o, db.QueryRow(`SELECT `+selectCols+` `+joinClause+` WHERE o.uuid = ?`, uuid)); err != nil {
		return nil, err
	}
	return o, nil
}

// Create inserts an order and returns the new row id.
func Create(db *sql.DB, uuid, orderType string, processNodeID *int64, retrieveEmpty bool, quantity int64, deliveryNode, stagingNode, sourceNode, loadType string, autoConfirm bool, payloadCode string) (int64, error) {
	res, err := db.Exec(`
		INSERT INTO orders (uuid, order_type, process_node_id, retrieve_empty, quantity, delivery_node, staging_node, source_node, load_type, auto_confirm, payload_code)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid, orderType, processNodeID, retrieveEmpty, quantity, deliveryNode, stagingNode, sourceNode, loadType, autoConfirm, payloadCode)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateProcessNode rebinds an order to a different process_node.
func UpdateProcessNode(db *sql.DB, id int64, processNodeID *int64) error {
	_, err := db.Exec(`UPDATE orders SET process_node_id=?, updated_at=datetime('now') WHERE id=?`, processNodeID, id)
	return err
}

// UpdateStatus changes the order status and bumps updated_at.
func UpdateStatus(db *sql.DB, id int64, newStatus string) error {
	_, err := db.Exec(`UPDATE orders SET status=?, updated_at=datetime('now') WHERE id=?`, newStatus, id)
	return err
}

// UpdateWaybill writes the carrier waybill and ETA fields.
func UpdateWaybill(db *sql.DB, id int64, waybillID, eta string) error {
	_, err := db.Exec(`UPDATE orders SET waybill_id=?, eta=?, updated_at=datetime('now') WHERE id=?`, waybillID, eta, id)
	return err
}

// UpdateETA sets the ETA on an order.
func UpdateETA(db *sql.DB, id int64, eta string) error {
	_, err := db.Exec(`UPDATE orders SET eta=?, updated_at=datetime('now') WHERE id=?`, eta, id)
	return err
}

// UpdateFinalCount writes the final count and operator-confirmation
// flag.
func UpdateFinalCount(db *sql.DB, id int64, finalCount int64, confirmed bool) error {
	_, err := db.Exec(`UPDATE orders SET final_count=?, count_confirmed=?, updated_at=datetime('now') WHERE id=?`, finalCount, confirmed, id)
	return err
}

// UpdateDeliveryNode rebinds the delivery node on an order.
func UpdateDeliveryNode(db *sql.DB, id int64, deliveryNode string) error {
	_, err := db.Exec(`UPDATE orders SET delivery_node=?, updated_at=datetime('now') WHERE id=?`, deliveryNode, id)
	return err
}

// UpdateStepsJSON stores the per-order steps document used by the
// scenesync runtime.
func UpdateStepsJSON(db *sql.DB, id int64, stepsJSON string) error {
	_, err := db.Exec(`UPDATE orders SET steps_json=?, updated_at=datetime('now') WHERE id=?`, stepsJSON, id)
	return err
}

// UpdateStagedExpireAt sets (or clears, when stagedExpireAt is nil) the
// staged_expire_at timestamp on an order. Times are stored in
// SQLite-canonical UTC formatting.
func UpdateStagedExpireAt(db *sql.DB, id int64, stagedExpireAt *time.Time) error {
	if stagedExpireAt == nil {
		_, err := db.Exec(`UPDATE orders SET staged_expire_at=NULL, updated_at=datetime('now') WHERE id=?`, id)
		return err
	}
	_, err := db.Exec(`UPDATE orders SET staged_expire_at=?, updated_at=datetime('now') WHERE id=?`, stagedExpireAt.UTC().Format(helpers.TimeLayout), id)
	return err
}

// UpdateBinUOPRemaining writes the bin's uop_remaining snapshot from the
// OrderDelivered envelope. nil clears the column (multi-bin orders, older
// Core builds). handleNormalReplenishment reads this value to reset
// lineside UOP without a second telemetry round-trip.
func UpdateBinUOPRemaining(db *sql.DB, id int64, binUOPRemaining *int) error {
	if binUOPRemaining == nil {
		_, err := db.Exec(`UPDATE orders SET bin_uop_remaining=NULL, updated_at=datetime('now') WHERE id=?`, id)
		return err
	}
	_, err := db.Exec(`UPDATE orders SET bin_uop_remaining=?, updated_at=datetime('now') WHERE id=?`, *binUOPRemaining, id)
	return err
}

// InsertHistory writes one order_history row.
func InsertHistory(db *sql.DB, orderID int64, oldStatus, newStatus, detail string) error {
	_, err := db.Exec(`INSERT INTO order_history (order_id, old_status, new_status, detail) VALUES (?, ?, ?, ?)`,
		orderID, oldStatus, newStatus, detail)
	return err
}

// ListStagedByProcessNode returns staged orders linked to a specific
// process_node.
func ListStagedByProcessNode(db *sql.DB, processNodeID int64) ([]Order, error) {
	rows, err := db.Query(`SELECT `+selectCols+` `+joinClause+`
		WHERE o.process_node_id = ? AND o.status = 'staged'
		ORDER BY o.created_at`, processNodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

// ListActiveByProcessNodeAndType returns non-terminal orders for a
// process node filtered by order type.
func ListActiveByProcessNodeAndType(db *sql.DB, processNodeID int64, orderType string) ([]Order, error) {
	rows, err := db.Query(`SELECT `+selectCols+` `+joinClause+`
		WHERE o.process_node_id = ? AND o.order_type = ? AND o.status NOT IN ('confirmed', 'cancelled', 'failed')
		ORDER BY o.created_at`, processNodeID, orderType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

// ListActiveByProcessNode returns non-terminal orders for a process
// node.
func ListActiveByProcessNode(db *sql.DB, processNodeID int64) ([]Order, error) {
	rows, err := db.Query(`SELECT `+selectCols+` `+joinClause+`
		WHERE o.process_node_id = ? AND o.status NOT IN ('confirmed', 'cancelled', 'failed')
		ORDER BY o.created_at`, processNodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

// ListActiveByProcessNodeOrSource returns non-terminal orders that are
// either tracked at the given process node (process_node_id match) OR
// source FROM the given source node name (source_node match). Used by
// the station service so a manual_swap supermarket loader sees demand
// for orders sourcing FROM it — not just orders directly created at
// it. Plant test 2026-04-27: line-initiated swap orders source from
// SMN_001's bin but are tracked at the line node, so the loader UI's
// "DEMAND" indicator went silent. The OR-query restores that signal.
//
// sourceNodeName matches against orders.source_node (the dot-name
// string Edge stores, not the integer node ID). Pass empty string to
// skip the source match (degenerates to ListActiveByProcessNode).
func ListActiveByProcessNodeOrSource(db *sql.DB, processNodeID int64, sourceNodeName string) ([]Order, error) {
	if sourceNodeName == "" {
		return ListActiveByProcessNode(db, processNodeID)
	}
	rows, err := db.Query(`SELECT `+selectCols+` `+joinClause+`
		WHERE (o.process_node_id = ? OR o.source_node = ?)
		  AND o.status NOT IN ('confirmed', 'cancelled', 'failed')
		ORDER BY o.created_at`, processNodeID, sourceNodeName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

// ListHistory returns the status history for one order, oldest first.
func ListHistory(db *sql.DB, orderID int64) ([]History, error) {
	rows, err := db.Query(`SELECT id, order_id, old_status, new_status, detail, created_at FROM order_history WHERE order_id = ? ORDER BY created_at`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []History
	for rows.Next() {
		var h History
		var createdAt string
		if err := rows.Scan(&h.ID, &h.OrderID, &h.OldStatus, &h.NewStatus, &h.Detail, &createdAt); err != nil {
			return nil, err
		}
		h.CreatedAt = helpers.ScanTime(createdAt)
		out = append(out, h)
	}
	return out, rows.Err()
}
