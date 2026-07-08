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

	"shingo/protocol"
	"shingo/shared/clock"
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
const SelectCols = `id, edge_uuid, station_id, order_type, status, quantity, source_node, delivery_node, process_node, vendor_order_id, vendor_state, robot_id, priority, payload_desc, error_detail, created_at, updated_at, completed_at, parent_order_id, sequence, steps_json, bin_id, payload_code, wait_index, queue_reason, skip_auto_confirm, sibling_order_uuid, source_intent`

// adminListExcludeTypeFilter excludes Core-internal housekeeping
// order types from admin-facing list queries. Today the only excluded
// type is reshuffle_restore (the synthetic parent for the post-
// pickup restock compound — never operator-actionable). Embedded into
// List, ListFiltered, and ListActive so the operator UI doesn't have
// to render a new order type. If a "show housekeeping orders" toggle
// is added later, callers can opt into the full query.
const adminListExcludeTypeFilter = `order_type != 'reshuffle_restore'`

// ScanOrder reads a single orders row.
// Exported for cross-aggregate readers at the outer store/ level.
func ScanOrder(row interface{ Scan(...any) error }) (*Order, error) {
	var o Order
	var parentOrderID, binID sql.NullInt64

	err := row.Scan(&o.ID, &o.EdgeUUID, &o.StationID, &o.OrderType, &o.Status,
		&o.Quantity,
		&o.SourceNode, &o.DeliveryNode, &o.ProcessNode, &o.VendorOrderID, &o.VendorState, &o.RobotID,
		&o.Priority, &o.PayloadDesc, &o.ErrorDetail, &o.CreatedAt, &o.UpdatedAt, &o.CompletedAt,
		&parentOrderID, &o.Sequence, &o.StepsJSON, &binID, &o.PayloadCode, &o.WaitIndex, &o.QueueReason,
		&o.SkipAutoConfirm, &o.SiblingOrderUUID, &o.SourceIntent)
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
	id, err := helpers.InsertID(db, `INSERT INTO orders (edge_uuid, station_id, order_type, status, quantity, source_node, delivery_node, process_node, priority, payload_desc, parent_order_id, sequence, steps_json, bin_id, payload_code, skip_auto_confirm, sibling_order_uuid, source_intent) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18) RETURNING id`,
		o.EdgeUUID, o.StationID, o.OrderType, o.Status,
		o.Quantity,
		o.SourceNode, o.DeliveryNode, o.ProcessNode, o.Priority, o.PayloadDesc,
		helpers.NullableInt64(o.ParentOrderID), o.Sequence, o.StepsJSON,
		helpers.NullableInt64(o.BinID), o.PayloadCode, o.SkipAutoConfirm, o.SiblingOrderUUID, o.SourceIntent)
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

// UpdateStatus transitions an order to a NON-terminal status and records it in
// history. Terminal statuses are refused: they MUST go through TerminalizeOrder
// (via lifecycle.transition), which sets the status AND releases the order's
// claims + reservations atomically. A raw terminal write here would leave those
// holds behind and brick the bin via uq_reservations_bin_active — the leak this
// guard closes. Test fixtures that need to seed a terminal state use
// testdb.SeedOrderStatus (a raw write); production has no terminal caller.
func UpdateStatus(db *sql.DB, id int64, status, detail string) error {
	if protocol.IsTerminal(protocol.Status(status)) {
		return fmt.Errorf("UpdateStatus: refusing raw terminal write to %q (id=%d) — route terminal transitions through TerminalizeOrder", status, id)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	errDetail := ""
	if status == "failed" || status == "cancelled" {
		errDetail = detail
	}
	if _, err := tx.Exec(`UPDATE orders SET status=$1, error_detail=$2, updated_at=$4 WHERE id=$3`, status, errDetail, id, clock.Now().UTC()); err != nil {
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
	_, err := db.Exec(`UPDATE orders SET wait_index=$1, updated_at=$3 WHERE id=$2`,
		waitIndex, id, clock.Now().UTC())
	return err
}

// SetQueueReason stores the blocking reason on a queued order. Pass ""
// to clear (e.g., when a previously-queued order successfully dispatches).
// Phase 4 of bin-transit-state — exposed via order-status responses so
// ops can see *why* an order is queued instead of guessing.
func SetQueueReason(db *sql.DB, id int64, reason string) error {
	_, err := db.Exec(`UPDATE orders SET queue_reason=$1, updated_at=$3 WHERE id=$2`,
		reason, id, clock.Now().UTC())
	return err
}

// LinkSiblingsByEdgeUUID records a two-robot swap pairing (supply ↔ evac)
// on Core, setting each order's sibling_order_uuid to the other. Keyed on
// edge_uuid because the link arrives (TypeOrderSiblingLink) carrying the
// two edge UUIDs — independent of Core's own ids and of which leg's
// ComplexOrderRequest landed first. One statement sets both directions;
// idempotent. Returns the number of order rows updated (0, 1, or 2).
func LinkSiblingsByEdgeUUID(db *sql.DB, uuidA, uuidB string) (int64, error) {
	res, err := db.Exec(`UPDATE orders SET
		sibling_order_uuid = CASE edge_uuid WHEN $1 THEN $2 WHEN $2 THEN $1 END,
		updated_at = $3
		WHERE edge_uuid IN ($1, $2)`, uuidA, uuidB, clock.Now().UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SiblingUUID returns the order's two-robot swap sibling edge UUID, or "".
func SiblingUUID(db *sql.DB, id int64) (string, error) {
	var s string
	err := db.QueryRow(`SELECT sibling_order_uuid FROM orders WHERE id=$1`, id).Scan(&s)
	return s, err
}

// UpdateVendor stores vendor-side identifiers on an order.
func UpdateVendor(db *sql.DB, id int64, vendorOrderID, vendorState, robotID string) error {
	_, err := db.Exec(`UPDATE orders SET vendor_order_id=$1, vendor_state=$2, robot_id=$3, updated_at=$5 WHERE id=$4`,
		vendorOrderID, vendorState, robotID, id, clock.Now().UTC())
	return err
}

// UpdateSourceNode rewrites the source_node field.
func UpdateSourceNode(db *sql.DB, id int64, sourceNode string) error {
	_, err := db.Exec(`UPDATE orders SET source_node=$1, updated_at=$3 WHERE id=$2`,
		sourceNode, id, clock.Now().UTC())
	return err
}

// UpdateDeliveryNode rewrites the delivery_node field.
func UpdateDeliveryNode(db *sql.DB, id int64, deliveryNode string) error {
	_, err := db.Exec(`UPDATE orders SET delivery_node=$1, updated_at=$3 WHERE id=$2`,
		deliveryNode, id, clock.Now().UTC())
	return err
}

// UpdateStepsJSON rewrites the steps_json field — used by complex-
// order replay when deferred NGRP resolution succeeds on a later tick
// and the scanner needs to lock the new concrete-child names ahead of
// claim. Round-3 follow-up.
func UpdateStepsJSON(db *sql.DB, id int64, stepsJSON string) error {
	_, err := db.Exec(`UPDATE orders SET steps_json=$1, updated_at=$3 WHERE id=$2`,
		stepsJSON, id, clock.Now().UTC())
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
	if _, err := tx.Exec(`UPDATE orders SET completed_at=$2, updated_at=$2 WHERE id=$1`, id, clock.Now().UTC()); err != nil {
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
		rows, err = db.Query(fmt.Sprintf(`SELECT %s FROM orders WHERE status=$1 AND %s ORDER BY id DESC LIMIT $2`, SelectCols, adminListExcludeTypeFilter), status, limit)
	} else {
		rows, err = db.Query(fmt.Sprintf(`SELECT %s FROM orders WHERE %s ORDER BY id DESC LIMIT $1`, SelectCols, adminListExcludeTypeFilter), limit)
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
	query := fmt.Sprintf(`SELECT %s FROM orders WHERE %s`, SelectCols, adminListExcludeTypeFilter)
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
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM orders WHERE status NOT IN (%s) AND %s ORDER BY id DESC`, SelectCols, protocol.TerminalStatusSQLList(), adminListExcludeTypeFilter))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanOrders(rows)
}

// CountActive returns the number of orders in non-terminal statuses, using
// the same WHERE clause as ListActive so the count matches the list exactly.
// Backs the dashboard "in flight" KPI (plan §3.A / §15.A).
func CountActive(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM orders WHERE status NOT IN (%s) AND %s`,
		protocol.TerminalStatusSQLList(), adminListExcludeTypeFilter)).Scan(&n)
	return n, err
}

// ListActiveBoard returns active non-terminal orders with an assigned robot,
// ordered oldest-first for the task board display.
func ListActiveBoard(db *sql.DB) ([]*Order, error) {
	rows, err := db.Query(fmt.Sprintf(
		`SELECT %s FROM orders WHERE status NOT IN (%s) AND robot_id != '' AND %s ORDER BY created_at ASC`,
		SelectCols, protocol.TerminalStatusSQLList(), adminListExcludeTypeFilter))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanOrders(rows)
}

// ListDistinctStations returns the distinct station IDs seen on orders,
// sorted. These are the values a dashboard's station scope can actually
// match — the board filter below is an exact station_id comparison, so
// offering anything else in a picker would silently empty the board.
func ListDistinctStations(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT station_id FROM orders WHERE station_id != '' ORDER BY station_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListActiveBoardFiltered is ListActiveBoard scoped to a set of station IDs —
// the server-side "area" filter for a dashboard. An empty/nil stations slice
// means no scoping (plant-wide), identical to ListActiveBoard. The IN list is
// built with positional placeholders rather than a SQL array type to stay
// portable across the database/sql + pgx stdlib path the rest of this package
// uses.
func ListActiveBoardFiltered(db *sql.DB, stations []string) ([]*Order, error) {
	if len(stations) == 0 {
		return ListActiveBoard(db)
	}
	placeholders := make([]string, len(stations))
	args := make([]any, len(stations))
	for i, s := range stations {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = s
	}
	query := fmt.Sprintf(
		`SELECT %s FROM orders WHERE status NOT IN (%s) AND robot_id != '' AND %s AND station_id IN (%s) ORDER BY created_at ASC`,
		SelectCols, protocol.TerminalStatusSQLList(), adminListExcludeTypeFilter, strings.Join(placeholders, ","))
	rows, err := db.Query(query, args...)
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
	_, err := db.Exec(`UPDATE orders SET priority=$1, updated_at=$3 WHERE id=$2`,
		priority, id, clock.Now().UTC())
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
	err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM orders WHERE delivery_node=$1 AND status NOT IN (%s)`, protocol.TerminalStatusSQLList()), nodeName).Scan(&count)
	return count, err
}

// ListDispatchedVendorOrderIDs returns vendor order IDs for orders currently
// dispatched, in transit, or staged.
func ListDispatchedVendorOrderIDs(db *sql.DB) ([]string, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT vendor_order_id FROM orders WHERE vendor_order_id != '' AND status IN (%s)`, protocol.VendorActiveStatusSQLList()))
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
	q := fmt.Sprintf(`SELECT %s FROM orders WHERE source_node IN (%s) AND status IN (%s) ORDER BY created_at ASC`,
		SelectCols, strings.Join(placeholders, ","), protocol.PreDispatchStatusSQLList())
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanOrders(rows)
}

// ListAcquiring returns all orders in an "acquiring" status (queued or
// sourcing) — the fulfillment scanner's retry set — ordered by priority DESC
// (highest first) then created_at ASC (FIFO within a priority class).
// orders.priority is INTEGER NOT NULL DEFAULT 0, so unset orders fall to FIFO
// naturally.
//
// Widened from queued-only (commit 3b): the scanner also retries orders sitting
// in `sourcing`. Before commit 4 moves MoveToSourcing to the start of the reserve
// attempt few orders rest there, but the scan set must see them when they do.
func ListAcquiring(db *sql.DB) ([]*Order, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM orders WHERE status IN (%s) ORDER BY priority DESC, created_at ASC`, SelectCols, protocol.AcquiringStatusSQLList()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanOrders(rows)
}

// UpdatePayloadCode sets the payload_code on an order.
func UpdatePayloadCode(db *sql.DB, orderID int64, payloadCode string) error {
	_, err := db.Exec(`UPDATE orders SET payload_code = $1, updated_at = $3 WHERE id = $2`, payloadCode, orderID, clock.Now().UTC())
	return err
}

// CountInFlightByDeliveryNode counts non-queued, non-terminal active orders
// targeting a delivery node.
func CountInFlightByDeliveryNode(db *sql.DB, deliveryNode string) (int, error) {
	var count int
	// "In-flight" = not terminal AND not queued. The queued exclusion is
	// composed inline rather than baked into a predicate because no other
	// site needs this combo.
	err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM orders WHERE delivery_node = $1 AND status NOT IN (%s) AND status != 'queued'`, protocol.TerminalStatusSQLList()), deliveryNode).Scan(&count)
	return count, err
}

// CountInFlightByDeliveryNodeExcluding is the same count but excludes
// a specific order ID. Used by planning-time capacity gates that check
// from inside the order's own dispatch path — without exclusion the
// caller's own pending/sourcing row would self-block. Pass excludeID=0
// to count all orders (no exclusion). Phase 4c of bin-transit-state.
func CountInFlightByDeliveryNodeExcluding(db *sql.DB, deliveryNode string, excludeID int64) (int, error) {
	var count int
	err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM orders WHERE delivery_node = $1 AND status NOT IN (%s) AND status != 'queued' AND id != $2`, protocol.TerminalStatusSQLList()),
		deliveryNode, excludeID).Scan(&count)
	return count, err
}

// UpdateRobotID rewrites just the robot_id field.
func UpdateRobotID(db *sql.DB, id int64, robotID string) error {
	_, err := db.Exec(`UPDATE orders SET robot_id=$1, updated_at=$3 WHERE id=$2`, robotID, id, clock.Now().UTC())
	return err
}

// UpdateBinID sets the bin_id on an order.
// (Junction-style write against the orders table; bins-aggregate readers
// live at outer store/ as composition.)
func UpdateBinID(db *sql.DB, orderID, binID int64) error {
	_, err := db.Exec(`UPDATE orders SET bin_id=$1, updated_at=$3 WHERE id=$2`, binID, orderID, clock.Now().UTC())
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
