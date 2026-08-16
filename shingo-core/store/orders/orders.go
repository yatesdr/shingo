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
	"encoding/json"
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
const SelectCols = `id, edge_uuid, station_id, order_type, status, quantity, source_node, delivery_node, process_node, vendor_order_id, vendor_state, robot_id, priority, payload_desc, error_detail, created_at, updated_at, completed_at, parent_order_id, sequence, steps_json, bin_id, payload_code, wait_index, queue_reason, queue_code, queue_cause, skip_auto_confirm, sibling_order_uuid, source_intent, coordinated, remaining_uop, origin_id, origin_class, open_for_children, dig_target_node`

// Admin-facing list queries (List, ListFiltered, ListActive, ListActiveBoard,
// CountActive) return EVERY order type. They used to exclude reshuffle_restore —
// the synthetic parent of the post-pickup restock compound — on the grounds that
// it is never operator-actionable. It is, however, operator-DIAGNOSABLE: those
// synthetics can strand at `reshuffling`, and the reconciliation sweeps that
// resolve them are much easier to trust when their subjects are visible. If a
// "hide housekeeping orders" toggle is ever wanted, it belongs in the UI, not
// baked into the store.

// ScanOrder reads a single orders row.
// Exported for cross-aggregate readers at the outer store/ level.
func ScanOrder(row interface{ Scan(...any) error }) (*Order, error) {
	var o Order
	var parentOrderID, binID sql.NullInt64
	var remainingUOP sql.NullInt64
	var queueCode, queueCause sql.NullString
	// origin_id is a nullable UUID — NULL is the honest reading for an order
	// nothing asked for, and it is what the partial index on the column keys off.
	var originID sql.NullString

	err := row.Scan(&o.ID, &o.EdgeUUID, &o.StationID, &o.OrderType, &o.Status,
		&o.Quantity,
		&o.SourceNode, &o.DeliveryNode, &o.ProcessNode, &o.VendorOrderID, &o.VendorState, &o.RobotID,
		&o.Priority, &o.PayloadDesc, &o.ErrorDetail, &o.CreatedAt, &o.UpdatedAt, &o.CompletedAt,
		&parentOrderID, &o.Sequence, &o.StepsJSON, &binID, &o.PayloadCode, &o.WaitIndex, &o.QueueReason, &queueCode, &queueCause,
		&o.SkipAutoConfirm, &o.SiblingOrderUUID, &o.SourceIntent, &o.Coordinated, &remainingUOP,
		&originID, &o.OriginClass, &o.OpenForChildren, &o.DigTargetNode)
	if err != nil {
		return nil, err
	}
	if originID.Valid {
		o.OriginID = originID.String
	}
	if parentOrderID.Valid {
		o.ParentOrderID = &parentOrderID.Int64
	}
	if binID.Valid {
		o.BinID = &binID.Int64
	}
	if remainingUOP.Valid {
		v := int(remainingUOP.Int64)
		o.RemainingUOP = &v
	}
	if queueCode.Valid {
		o.QueueCode = queueCode.String
	}
	if queueCause.Valid {
		o.QueueCause = queueCause.String
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
//
// created_at/updated_at are written explicitly from clock.Now(). They carry
// DDL defaults of NOW(), and omitting them from the column list took the
// DATABASE's wall clock while every duration in the system is measured against
// clock.Now() — so under the sim's fast-forward clock an order was "created"
// hours after the transitions that follow it, and duration_ms came out
// non-positive on most rows. That is why four telemetry queries carry a
// FILTER (WHERE duration_ms > 0) guard and why execution.go sources both
// endpoints from order_history to sidestep it.
//
// At a plant the two clocks agree to ~40ms (0 of 1878 rows affected), so this
// is a sim-fidelity fix, not a plant-correctness one.
//
// dig_target_node IS BOUND HERE, and open_for_children deliberately is not, so
// the difference is worth one sentence. Openness CHANGES over a compound's life
// and has exactly one writer for that reason; a dig's target is a BIRTH fact —
// createServiceDigParent knows the slot it is excavating toward before it knows
// the order's id, and no later transition may revise it. Binding it at creation
// is what makes it immutable: there is nothing to keep in step with, because
// there is no second write.
func Create(db helpers.QueryRower, o *Order) error {
	now := clock.Now().UTC()
	id, err := helpers.InsertID(db, `INSERT INTO orders (edge_uuid, station_id, order_type, status, quantity, source_node, delivery_node, process_node, priority, payload_desc, parent_order_id, sequence, steps_json, bin_id, payload_code, skip_auto_confirm, sibling_order_uuid, source_intent, coordinated, origin_id, origin_class, dig_target_node, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $23) RETURNING id`,
		o.EdgeUUID, o.StationID, o.OrderType, o.Status,
		o.Quantity,
		o.SourceNode, o.DeliveryNode, o.ProcessNode, o.Priority, o.PayloadDesc,
		helpers.NullableInt64(o.ParentOrderID), o.Sequence, o.StepsJSON,
		helpers.NullableInt64(o.BinID), o.PayloadCode, o.SkipAutoConfirm, o.SiblingOrderUUID, o.SourceIntent, o.Coordinated,
		helpers.NullableText(o.OriginID), o.OriginClass, o.DigTargetNode,
		now)
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

// AwaitingFleetSQL renders "Core has NOT yet handed this leg to the fleet" —
// the one spelling of the question every compound re-drive asks.
//
// ── WHY IT IS A FUNCTION AND NOT A LITERAL AT EACH QUERY ──────────────────
//
// The question had three spellings and one of them was the authority:
//
//   - GetNextChild                 status='pending'
//   - ListHeldLegParentsInLane     status='pending'
//   - the exactly-once guard       vendor_order_id != ""  (compound.go)
//
// The third is the real one, and its own comment says so: vendor_order_id "is
// non-empty once and only once a child has been handed to the fleet, it
// survives a crash, and it does not depend on what any sibling is doing." The
// other two used a STATUS as a proxy for it, and the proxy was exact only
// because nothing ever left `pending` without reaching the fleet.
//
// A leg whose fleet CREATE is refused breaks that. It has been claimed
// (pending → sourcing) and rolled back out of `dispatched`, so it sits at
// `sourcing` holding no vendor order — not yet with the fleet, and invisible to
// both status-keyed queries. Terminating it was the old answer (the demand died
// on a robot-system blip); parking it is the new one, and parking only works if
// the re-drives can still see it.
//
// The status half stays, narrowed to its honest meaning: IsPreDispatch is
// "still in Core's planning space and has not yet been sent to the fleet
// vendor", which is this question exactly. A compound child is never `queued`,
// so on this population the set is {pending, sourcing} — but it is DERIVED from
// the predicate rather than hand-listed, so a status added to the pre-dispatch
// family cannot leave one of these queries behind.
//
// Rendered rather than written out, on the DigExclusionSQL precedent: there is
// no second place where the comparison is spelled.
//
// alias is the table alias the caller uses ("" for an unaliased `orders`).
func AwaitingFleetSQL(alias string) string {
	q := ""
	if alias != "" {
		q = alias + "."
	}
	return fmt.Sprintf("%sstatus IN (%s) AND COALESCE(%svendor_order_id, '') = ''",
		q, protocol.PreDispatchStatusSQLList(), q)
}

// GetNextChild returns the next child order a compound has not yet handed to
// the fleet — see AwaitingFleetSQL for why that is not the same as `pending`.
func GetNextChild(db *sql.DB, parentOrderID int64) (*Order, error) {
	row := db.QueryRow(fmt.Sprintf(
		`SELECT %s FROM orders WHERE parent_order_id=$1 AND %s ORDER BY sequence LIMIT 1`,
		SelectCols, AwaitingFleetSQL("")), parentOrderID)
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
	if _, err := tx.Exec(`INSERT INTO order_history (order_id, status, detail, created_at) VALUES ($1, $2, $3, $4)`, id, status, detail, clock.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateStatusFrom is the compare-and-swap form of UpdateStatus: the row moves
// only if its status in the DB is STILL `from`. Returns false (and no error)
// when the order has already moved on — a concurrent writer got there first and
// this write is refused rather than applied on top.
//
// Why the CAS exists. UpdateStatus writes by id alone, so a caller holding a
// stale *orders.Order overwrites whatever landed since it loaded. Terminal
// statuses have no outgoing edges in protocol.validTransitions, which makes
// them absorbing against SEQUENTIAL writers only — the guard in
// lifecycle.transition validates against the caller's own snapshot. CI #497:
// an in-flight fulfillment tick loaded order 1 as `queued`, the recovery op
// cancelled it, and the tick's queued→sourcing write (legal against its stale
// snapshot) resurrected the cancelled order. The CAS makes terminal states
// absorbing against concurrent writers too.
//
// Same terminal-write refusal as UpdateStatus — terminal transitions go
// through TerminalizeOrder, which also releases claims + reservations.
func UpdateStatusFrom(db *sql.DB, id int64, from, to, detail string) (bool, error) {
	return UpdateStatusFromWithReason(db, id, from, to, detail, "", "", nil)
}

// UpdateStatusFromWithReason is UpdateStatusFrom plus the typed reason for the
// history row (migration 55): code, actor, and the JSON-encoded reference.
//
// The →queued row is the one that matters most here. orders.queue_code is a
// LIVE column overwritten in place every time an order re-queues, so it only
// ever answers "why is this stuck right now" — the single reason
// starvation-by-cause has been unqueryable. Writing the code onto the HISTORY
// row makes it a time series.
//
// refJSON is nil for "no reference", which stores SQL NULL and keeps the
// partial index small.
func UpdateStatusFromWithReason(db *sql.DB, id int64, from, to, detail, code, actor string, refJSON any) (bool, error) {
	if protocol.IsTerminal(protocol.Status(to)) {
		return false, fmt.Errorf("UpdateStatusFrom: refusing raw terminal write to %q (id=%d) — route terminal transitions through TerminalizeOrder", to, id)
	}
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// error_detail is cleared exactly as UpdateStatus does on this path: its
	// failed/cancelled branch is unreachable here because both are terminal
	// and refused above.
	res, err := tx.Exec(`UPDATE orders SET status=$1, error_detail='', updated_at=$3 WHERE id=$2 AND status=$4`,
		to, id, clock.Now().UTC(), from)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if _, err := tx.Exec(
		`INSERT INTO order_history (order_id, status, detail, code, actor, ref, created_at)
		 VALUES ($1, $2, $3, NULLIF($4,''), NULLIF($5,''), $6, $7)`,
		id, to, detail, code, actor, refJSON, clock.Now().UTC()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// UpdateWaitIndex increments the wait_index for a complex order after
// releasing one wait segment.
func UpdateWaitIndex(db *sql.DB, id int64, waitIndex int) error {
	_, err := db.Exec(`UPDATE orders SET wait_index=$1, updated_at=$3 WHERE id=$2`,
		waitIndex, id, clock.Now().UTC())
	return err
}

// SetQueueDetail stores the blocking reason on a queued order — the generated
// sentence (queue_reason), its structured category (queue_code), and the
// engineer-only call-site tag (queue_cause) — together, in one write. Pass all
// empty to clear (e.g. when a previously-queued order successfully dispatches).
//
// This is the ONE writer for all three columns. The dispatch formatter is the
// only caller that should reach it: it generates the sentence from code+params
// and hands sentence+code+cause here, so a free-text queue reason can never be
// written directly. queue_code/queue_cause are nullable; NULL means a
// pre-schema row (no backfill) or a cleared reason.
func SetQueueDetail(db *sql.DB, id int64, reason, code, cause string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is the error close

	if _, err := tx.Exec(`UPDATE orders SET queue_reason=$1, queue_code=$4, queue_cause=$5, updated_at=$3 WHERE id=$2`,
		reason, id, clock.Now().UTC(), helpers.NullableText(code), helpers.NullableText(cause)); err != nil {
		return err
	}

	// Stamp the code onto this queue EPISODE's history row.
	//
	// orders.queue_code is a live column overwritten in place, so it only ever
	// answers "why is this order stuck right now" — which is the single reason
	// starvation-by-cause has been unqueryable. There is no record anywhere of
	// what an order waited for last Tuesday.
	//
	// The code is not known at the moment of the →queued transition (the
	// scanner discovers the reason on a later pass), so this updates the most
	// recent queued row rather than inserting a new one. Re-queueing creates a
	// fresh queued row, so each EPISODE keeps its own reason; successive
	// updates within one episode overwrite that episode's row, which is
	// correct — the last known reason is why it was still waiting.
	//
	// Clearing (empty code, on successful dispatch) deliberately does NOT wipe
	// the history row: the order waited for that reason, and it having stopped
	// waiting does not unmake the fact.
	if code != "" {
		if _, err := tx.Exec(
			`UPDATE order_history SET code = $2
			 WHERE id = (SELECT id FROM order_history
			             WHERE order_id = $1 AND status = 'queued'
			             ORDER BY id DESC LIMIT 1)`, id, code); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LinkSiblingsByEdgeUUID records a two-robot swap pairing (supply ↔ evac)
// on Core, setting each order's sibling_order_uuid to the other.
//
// Keyed on edge_uuid because the pairing is expressed in the two legs' own
// UUIDs — independent of Core's ids and of which leg's ComplexOrderRequest
// landed first. The link rides the SECOND leg's ComplexOrderRequest, in its
// SiblingOrderUUID field; there is no separate message for it. (This comment
// used to name a "TypeOrderSiblingLink" wire type, which has never existed in
// protocol/types.go — the feature is real, the message was not.)
//
// One statement sets both directions; idempotent. Returns the number of order
// rows updated (0, 1, or 2).
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

// SetCompoundOpen is THE writer of orders.open_for_children — the only thing
// in shingo-core that changes whether a compound parent may still gain
// children. Everything else reads it.
//
// ONE WRITER, ONE FACT, and the value is a parameter rather than the function
// being split in two. Seal-here / open-there would read better at the call
// sites and would be two places deciding one fact — the shape §17.1 records,
// which briefs 1 and 2 each spent a commit undoing.
//
// Creation is not a second writer. An order is born sealed by the column's
// DEFAULT and Create deliberately does not bind it (writer_totality_test names
// it), so there is exactly one statement in the codebase that can make a
// compound open and exactly one that can seal it, and they are this line.
//
// AND THERE IS NO AUTO-SEAL. The tempting rule — "seal it when the last leg
// completes" — is a second writer wearing a convenience, and it is wrong on its
// own terms: the whole point of openness is that a reshuffle between moves has
// all its children terminal and is NOT finished, so last-leg-completion is
// precisely the moment that cannot decide this. Whatever concludes there is no
// more digging to do calls this. Nothing infers it.
//
// A write that matches no row is an error rather than a silent no-op. A caller
// that sealed a parent that has since been deleted has had its instruction
// dropped, and finding that out later — from a reshuffle that completed half
// dug — is the expensive way.
func SetCompoundOpen(db *sql.DB, parentOrderID int64, open bool) error {
	res, err := db.Exec(`UPDATE orders SET open_for_children=$1, updated_at=$3 WHERE id=$2`,
		open, parentOrderID, clock.Now().UTC())
	if err != nil {
		return fmt.Errorf("set open_for_children=%t on order %d: %w", open, parentOrderID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set open_for_children on order %d: rows affected: %w", parentOrderID, err)
	}
	if n == 0 {
		return fmt.Errorf("set open_for_children=%t: order %d does not exist", open, parentOrderID)
	}
	return nil
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

// UpdateRemainingUOP rewrites the remaining_uop field — the operator's declared
// release-correction count carried to the (scanner-side) bin claim. nil clears it
// to NULL (plain claim, no manifest sync). Written at intake by planTransport so
// the scanner, which has no envelope, can seed the same atomic claim+sync.
func UpdateRemainingUOP(db *sql.DB, id int64, remainingUOP *int) error {
	_, err := db.Exec(`UPDATE orders SET remaining_uop=$1, updated_at=$3 WHERE id=$2`,
		helpers.NullableInt(remainingUOP), id, clock.Now().UTC())
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
	query := fmt.Sprintf(`SELECT %s FROM orders WHERE true`, SelectCols)
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
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM orders WHERE status NOT IN (%s) ORDER BY id DESC`, SelectCols, protocol.TerminalStatusSQLList()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanOrders(rows)
}

// ListActiveByStation returns the non-terminal orders for one station.
//
// It backs the healing half of the order reconcile: Core answers an Edge's
// status request with these, minus the ones the Edge already named, so an order
// Core authored and the Edge never heard about gets a row. Same non-terminal
// predicate as ListActive, so the two cannot disagree about what "active" means.
func ListActiveByStation(db *sql.DB, stationID string) ([]*Order, error) {
	rows, err := db.Query(fmt.Sprintf(
		`SELECT %s FROM orders WHERE station_id = $1 AND status NOT IN (%s) ORDER BY id DESC`,
		SelectCols, protocol.TerminalStatusSQLList()), stationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanOrders(rows)
}

// ActiveByDeliveryNodes returns non-terminal orders whose delivery_node is one of
// the given names — the active stores targeting a lane (the tiered-entry gate's
// input). Empty names → no orders.
func ActiveByDeliveryNodes(db *sql.DB, names []string) ([]*Order, error) {
	if len(names) == 0 {
		return nil, nil
	}
	ph := make([]string, len(names))
	args := make([]any, len(names))
	for i, n := range names {
		ph[i] = fmt.Sprintf("$%d", i+1)
		args[i] = n
	}
	rows, err := db.Query(fmt.Sprintf(
		`SELECT %s FROM orders WHERE delivery_node IN (%s) AND status NOT IN (%s) ORDER BY id`,
		SelectCols, strings.Join(ph, ", "), protocol.TerminalStatusSQLList()), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanOrders(rows)
}

// ActiveGateCandidates returns every non-terminal order that COULD be parked at
// a lane wait: it reached the fleet (a vendor order) and it carries a plan.
//
// ── WHY IT IS NOT KEYED ON THE LANE ───────────────────────────────────────
//
// The lane a gate wait belongs to lives on the WAIT STEP inside steps_json
// (resolvedStep.WaitLane), and steps_json is TEXT, not jsonb — there is no
// containment operator to key on, and the wait that matters is the one at
// wait_index, which means COUNTING waits. A LIKE on the rendered field would
// have to distinguish lane 4 from lane 42 by punctuation, which is the kind of
// clever that breaks silently.
//
// So the narrowing is done here on the two columns that ARE indexed facts, and
// the wait-step test happens in Go (dispatch.gateStagedForLane). The residual
// set is orders that are in flight AND carry a plan, which is small: it is
// bounded by what the fleet is currently doing, not by history.
//
// THIS REPLACED TWO ENDPOINT QUERIES. ActiveByDeliveryNodes/ActiveBySourceNodes
// found gate-staged orders by matching an endpoint column against a lane's slot
// names, which structurally missed any order whose lane entry is INTERIOR to its
// plan — neither its first actionable step nor its last. A spliced plan has
// exactly that shape whenever the lane is not an endpoint.
func ActiveGateCandidates(db *sql.DB) ([]*Order, error) {
	rows, err := db.Query(fmt.Sprintf(
		`SELECT %s FROM orders
		  WHERE vendor_order_id <> '' AND steps_json <> ''
		    AND status NOT IN (%s)
		  ORDER BY id`,
		SelectCols, protocol.TerminalStatusSQLList()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanOrders(rows)
}

// ActiveBySourceNodes WAS HERE AND IS DELETED, with its only caller.
//
// It was the source-node mirror of ActiveByDeliveryNodes, and the lane-gate
// evaluator used the pair to find gate-staged retrieves and stores by matching
// an ENDPOINT column against a lane's slot names. Candidate discovery now keys
// on the wait step (ActiveGateCandidates above), which is both the right
// question and the one that can see an order whose lane entry is interior to its
// plan. ActiveByDeliveryNodes survives because the tiered-entry classifier still
// legitimately asks "which active stores target this lane's slots".

// CountActive returns the number of orders in non-terminal statuses, using
// the same WHERE clause as ListActive so the count matches the list exactly.
// Backs the dashboard "in flight" KPI (plan §3.A / §15.A).
func CountActive(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM orders WHERE status NOT IN (%s)`,
		protocol.TerminalStatusSQLList())).Scan(&n)
	return n, err
}

// ListActiveBoard returns active non-terminal orders with an assigned robot,
// ordered oldest-first for the task board display.
func ListActiveBoard(db *sql.DB) ([]*Order, error) {
	rows, err := db.Query(fmt.Sprintf(
		`SELECT %s FROM orders WHERE status NOT IN (%s) AND robot_id != '' ORDER BY created_at ASC`,
		SelectCols, protocol.TerminalStatusSQLList()))
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
		`SELECT %s FROM orders WHERE status NOT IN (%s) AND robot_id != '' AND station_id IN (%s) ORDER BY created_at ASC`,
		SelectCols, protocol.TerminalStatusSQLList(), strings.Join(placeholders, ","))
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanOrders(rows)
}

// ListHistory returns the audit log entries for an order, oldest first.
func ListHistory(db *sql.DB, orderID int64) ([]*History, error) {
	rows, err := db.Query(`SELECT id, order_id, status, detail, code, actor, ref, created_at
		FROM order_history WHERE order_id=$1 ORDER BY id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var history []*History
	for rows.Next() {
		var h History
		var code, actor sql.NullString
		var ref []byte
		if err := rows.Scan(&h.ID, &h.OrderID, &h.Status, &h.Detail, &code, &actor, &ref, &h.CreatedAt); err != nil {
			return nil, err
		}
		h.Code, h.Actor = code.String, actor.String
		// A malformed ref is left nil rather than failing the whole read: the
		// history row's job is the timeline, and one bad JSON blob must not
		// hide an order's transitions.
		if len(ref) > 0 {
			var r protocol.TermRef
			if err := json.Unmarshal(ref, &r); err == nil {
				h.Ref = &r
			}
		}
		history = append(history, &h)
	}
	return history, rows.Err()
}

// EverReachedStatus reports whether the order ever recorded the given status.
//
// Asked of order_history rather than of the order's current status, because
// the question is about the PAST: an order that is now `cancelled` may or may
// not have had a robot moving for it, and only the history distinguishes those
// two. Uses idx_order_history_order.
func EverReachedStatus(db *sql.DB, orderID int64, status string) (bool, error) {
	var found bool
	err := db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM order_history WHERE order_id=$1 AND status=$2)`,
		orderID, status).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("order %d ever %s: %w", orderID, status, err)
	}
	return found, nil
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

// ListStalledChapters returns compound parents in `reshuffling` that still have a
// non-terminal child and whose whole family has gone quiet — nothing in the
// parent or any of its children written since `since`.
//
// ── THE POPULATION SR.91 CREATED AND LEFT UNFLOORED (law 8) ────────────────
//
// AdvanceStuckReshuffleParents covers the other half of this status: a parent
// whose children are ALL terminal, which is a chapter that finished and did not
// get returned. This is the half where a leg is still open. Before SR.91 that
// half held only synthetic folders and the demand behind them waited in `queued`,
// inside IsAcquiring, swept every 60s. SR.91 made the demand itself wear
// `reshuffling` -- which no sweep, no floor and no anomaly detector covers.
//
// QUIET IS ASKED ACROSS THE WHOLE FAMILY, not just the parent. A parent's own
// updated_at does not move while its legs run, so a parent-only test would call
// every healthy excavation stalled within a minute of starting.
func ListStalledChapters(db *sql.DB, since time.Time, limit int) ([]int64, error) {
	rows, err := db.Query(fmt.Sprintf(`
		SELECT p.id
		FROM orders p
		WHERE p.status = 'reshuffling'
		  AND p.updated_at < $1
		  AND EXISTS (
			SELECT 1 FROM orders c
			WHERE c.parent_order_id = p.id AND c.status NOT IN (%s)
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM orders c
			WHERE c.parent_order_id = p.id AND c.updated_at >= $1
		  )
		ORDER BY p.id
		LIMIT $2`, protocol.TerminalStatusSQLList()), since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListTrackedVendorOrderIDs returns the vendor order IDs Core must keep watching
// — the orders the fleet still holds.
//
// This is what re-registers orders with the poller after a restart, which makes
// the status list load-bearing rather than descriptive: an order missing from it
// is an order nothing polls, and for the states that end on a timer, nothing
// polling means the timer never fires.
//
// It selected the vendor-ACTIVE set, which excludes faulted. A faulted order is
// one the fleet failed while holding a bin; it ends when its grace period expires
// and the poller gives up on it. After a restart it was not tracked, so the grace
// period never ran, and since faulted is deliberately outside both stuck
// predicates nothing else looked at it either — while it went on blocking
// changeovers at its node.
func ListTrackedVendorOrderIDs(db *sql.DB) ([]string, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT vendor_order_id FROM orders WHERE vendor_order_id != '' AND status IN (%s)`, protocol.VendorTrackedStatusSQLList()))
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
// Widened from queued-only: the scanner also retries orders sitting in
// `sourcing`. Once MoveToSourcing moved to the start of the reserve
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

// CountLiveByOrigin returns a demand episode's OWN non-terminal order count — a
// bare total, not a per-window breakdown. It returns zero when the episode has
// nothing outstanding.
//
// IT DELIBERATELY COUNTS `queued`, which is the whole point and the one way it
// differs from the two counts above. Those answer "is something on its way
// here", and for that question a queued order is correctly invisible: it holds
// no destination, and the fulfillment scanner depends on it holding none
// (ListAcquiring and the `status != 'queued'` in the two in-flight counts are
// the same rule stated twice).
//
// This answers a different question — "what has this demand already asked for"
// — and there the answer must include an order that asked and has not yet been
// able to source. Springfield 2026-08-03: a loader window whose empty market was
// dry accumulated 241 identical queued retrieve_empty orders, about one a minute
// for three and a half hours, because the only guard against re-asking was an
// in-flight count that could not see the orders it had already created. Each one
// was, to that count, the first.
//
// A BARE TOTAL, not a per-window breakdown, because this answers the SIZING
// question only — how much of what this demand needs is already coming. Which
// windows are free is a different question with a different answer set, and it
// is not this episode's to answer: a window can be spoken for by an order this
// episode has never heard of (CountLiveByDeliveryNode below).
//
// The scan is over idx_orders_origin_id, a partial index on origin_id IS NOT
// NULL, so this reads only rows that carry an episode.
func CountLiveByOrigin(db *sql.DB, originID string) (int, error) {
	var count int
	err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM orders WHERE origin_id = $1 AND status NOT IN (%s)`, protocol.TerminalStatusSQLList()), originID).Scan(&count)
	return count, err
}

// CountLiveByDeliveryNode counts ALL non-terminal orders pointed at a delivery
// node, from any origin and in any status — `queued` included.
//
// It is the "is this window spoken for" question, and it is deliberately blind
// to who is asking. Two payloads at one shared-window loader are two separate
// demand episodes, and neither can see the other's orders; an episode-scoped
// check would let both put a carrier on the same window, which is the one thing
// "one order per window" is supposed to mean.
//
// Distinct from CountInFlightByDeliveryNode, which excludes `queued` because it
// answers "is something on its way" for the fulfillment scanner — a question
// where an unsourced order correctly counts for nothing. Here an unsourced order
// counts for everything: it is a claim on the window that has not been given up.
func CountLiveByDeliveryNode(db *sql.DB, deliveryNode string) (int, error) {
	var count int
	err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM orders WHERE delivery_node = $1 AND status NOT IN (%s)`, protocol.TerminalStatusSQLList()), deliveryNode).Scan(&count)
	return count, err
}

// CountLiveCarrierRequestsByDeliveryNode is CountLiveByDeliveryNode narrowed to
// orders that ASKED FOR A CARRIER — source_intent='empty'. Same queued-included
// rule, same origin-blindness, same "one order per window" contract; the only
// difference is WHICH orders count as having spoken for the window.
//
// WHY THE NARROWING EXISTS. The contract this guard enforces is the one its
// commit named: "a demand that already asked for a carrier must not ask again."
// A swap's EVAC leg never asked for a carrier — it is a return trip bringing a
// spent bin back — yet it names the home as its delivery node, so the unnarrowed
// count read it as an outstanding ask. Springfield 2026-08-05: evac 4186
// (ALN_006 -> SMN_030, queued on waiting_for_partner) held SMN_030's window for
// 8h57m. Its supply sibling 4185 could not source because SMN_030 was empty, and
// the replenishment that would have put a carrier there was refused because 4186
// was "already asking". Neither leg could move. The lineside bin ran to -575 UOP.
//
// WHAT STILL COUNTS, and why 2026-08-03 does not come back. The 241 duplicate
// retrieve_empty orders that motivated the original guard were carrier requests
// — source_intent='empty' — so they still count, and a demand that has asked
// still cannot ask again. Only returns (complex evac legs, moves) stop counting.
//
// WHY A RETURN CAN SAFELY STOP COUNTING. A swap is sequenced: the supply leg
// lifts the full bin OFF the home before the evac brings the spent one back, so
// the two never contend for the slot. If the supply leg dies first,
// HandleSwapPeerTerminal cancels the evac. And a return that is actually MOVING
// is still caught by CheckDropoffCapacity's in-flight arm, which runs before
// this check — that arm answers the physical question ("is there room"), this
// one answers the logical question ("have I already asked").
//
// source_intent is stamped once at intake by SourceIntentForType and is 'empty'
// for exactly OrderTypeRetrieveEmpty, which is the shape every replenishment
// carrier pull takes (loader_replenish.go admitReplenishOrder).
func CountLiveCarrierRequestsByDeliveryNode(db *sql.DB, deliveryNode string) (int, error) {
	var count int
	err := db.QueryRow(fmt.Sprintf(
		`SELECT COUNT(*) FROM orders WHERE delivery_node = $1 AND source_intent = 'empty' AND status NOT IN (%s)`,
		protocol.TerminalStatusSQLList()), deliveryNode).Scan(&count)
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

// ActiveByBinID returns non-terminal orders currently referencing a bin via
// bin_id. BinService consults it to refuse a manual retire/move that would
// orphan a live order — the SMN_029 zombie (2026-07-22): a delivered
// retrieve_empty still pointing at a carrier an operator retired + moved kept
// its delivery card on the HMI and its loader slot budget spent while the
// carrier was recycled into another part elsewhere. Core CLEARS bins.claimed_by
// on arrival (ApplyArrival), so orders.bin_id is the only durable bin↔order tie
// a post-delivery guard can key on. No LIMIT: correctness can't ride on a bin's
// active order being among its N most recent rows.
func ActiveByBinID(db *sql.DB, binID int64) ([]*Order, error) {
	rows, err := db.Query(fmt.Sprintf(
		`SELECT %s FROM orders WHERE bin_id=$1 AND status NOT IN (%s) ORDER BY id DESC`,
		SelectCols, protocol.TerminalStatusSQLList()), binID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanOrders(rows)
}
