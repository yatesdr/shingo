package store

// Stage 2D delegate file: order CRUD and history live in store/orders/.
// Cross-aggregate methods (CreateCompoundChildren, FailOrderAtomic,
// CancelOrderAtomic) stay here because they mutate both the orders and
// bins tables in a single transaction.

import (
	"encoding/json"
	"fmt"

	"shingo/protocol"
	"shingo/shared/clock"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

func (db *DB) CreateOrder(o *orders.Order) error { return orders.Create(db.DB, o) }

// CompoundChild describes a child order to create in a compound order
// transaction. Declared here (not in the orders sub-package) because
// CreateCompoundChildren is cross-aggregate.
type CompoundChild struct {
	Order *orders.Order
	BinID int64 // bin to claim for this child
}

// CreateCompoundChildren creates all child orders and claims their payloads
// in a single transaction. Cross-aggregate (orders ↔ bins).
func (db *DB) CreateCompoundChildren(children []CompoundChild) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	for _, c := range children {
		o := c.Order
		// One writer for the orders table. orders.Create runs inside this
		// transaction because it takes a QueryRower rather than a *sql.DB, so
		// children carry the same columns as every other order and cannot drift
		// from them. It also owns the clock.Now() timestamps and the o.ID
		// write-back that the code below depends on.
		if err := orders.Create(tx, o); err != nil {
			return fmt.Errorf("create child order (seq %d): %w", o.Sequence, err)
		}

		// Bin-centric claiming: if the child order has a bin, claim it
		if o.BinID != nil {
			if _, err := tx.Exec(`UPDATE bins SET claimed_by=$1 WHERE id=$2`, o.ID, *o.BinID); err != nil {
				return fmt.Errorf("claim bin %d for child %d: %w", *o.BinID, o.ID, err)
			}
		}
	}

	return tx.Commit()
}

// ListChildOrders returns all child orders for a parent order.
func (db *DB) ListChildOrders(parentOrderID int64) ([]*orders.Order, error) {
	return orders.ListChildren(db.DB, parentOrderID)
}

// GetNextChildOrder returns the next pending child order for a parent.
func (db *DB) GetNextChildOrder(parentOrderID int64) (*orders.Order, error) {
	return orders.GetNextChild(db.DB, parentOrderID)
}

func (db *DB) UpdateOrderStatus(id int64, status, detail string) error {
	return orders.UpdateStatus(db.DB, id, status, detail)
}

// UpdateOrderStatusFrom is the compare-and-swap status write — see
// orders.UpdateStatusFrom. Returns false when the order already moved on.
func (db *DB) UpdateOrderStatusFrom(id int64, from, to, detail string) (bool, error) {
	return orders.UpdateStatusFrom(db.DB, id, from, to, detail)
}

// UpdateOrderStatusFromWithReason is the CAS status write plus the typed
// reason for the history row (migration 55). The →queued row is the important
// caller: orders.queue_code is overwritten in place, so the history row is the
// only place a queue reason becomes a time series.
func (db *DB) UpdateOrderStatusFromWithReason(id int64, from, to, detail string, reason HistoryReason) (bool, error) {
	return orders.UpdateStatusFromWithReason(db.DB, id, from, to, detail, reason.Code, reason.Actor, reason.refJSON())
}

// UpdateOrderWaitIndex increments the wait_index for a complex order after
// releasing one wait segment.
func (db *DB) UpdateOrderWaitIndex(id int64, waitIndex int) error {
	return orders.UpdateWaitIndex(db.DB, id, waitIndex)
}

// SetOrderQueueDetail stores the blocking reason on a queued order — the
// generated sentence, its structured queue code, and the engineer-only cause —
// in one write. code is typed (protocol.QueueCode) so a caller cannot pass free
// text: the dispatch formatter generates the sentence and is the sole caller.
// Pass "" / empty code to clear (on successful dispatch).
func (db *DB) SetOrderQueueDetail(id int64, reason string, code protocol.QueueCode, cause string) error {
	return orders.SetQueueDetail(db.DB, id, reason, string(code), cause)
}

// LinkOrderSiblingsByEdgeUUID records a two-robot swap pairing keyed on
// edge UUID — see orders.LinkSiblingsByEdgeUUID. Returns rows updated.
func (db *DB) LinkOrderSiblingsByEdgeUUID(uuidA, uuidB string) (int64, error) {
	return orders.LinkSiblingsByEdgeUUID(db.DB, uuidA, uuidB)
}

// OrderSiblingUUID returns the order's two-robot swap sibling edge UUID, or "".
func (db *DB) OrderSiblingUUID(id int64) (string, error) {
	return orders.SiblingUUID(db.DB, id)
}

func (db *DB) UpdateOrderVendor(id int64, vendorOrderID, vendorState, robotID string) error {
	return orders.UpdateVendor(db.DB, id, vendorOrderID, vendorState, robotID)
}

func (db *DB) UpdateOrderSourceNode(id int64, sourceNode string) error {
	return orders.UpdateSourceNode(db.DB, id, sourceNode)
}

func (db *DB) UpdateOrderDeliveryNode(id int64, deliveryNode string) error {
	return orders.UpdateDeliveryNode(db.DB, id, deliveryNode)
}

func (db *DB) UpdateOrderRemainingUOP(id int64, remainingUOP *int) error {
	return orders.UpdateRemainingUOP(db.DB, id, remainingUOP)
}

func (db *DB) UpdateOrderStepsJSON(id int64, stepsJSON string) error {
	return orders.UpdateStepsJSON(db.DB, id, stepsJSON)
}

func (db *DB) CompleteOrder(id int64) error             { return orders.Complete(db.DB, id) }
func (db *DB) GetOrder(id int64) (*orders.Order, error) { return orders.Get(db.DB, id) }
func (db *DB) GetOrderByUUID(uuid string) (*orders.Order, error) {
	return orders.GetByUUID(db.DB, uuid)
}
func (db *DB) GetOrderByVendorID(vendorOrderID string) (*orders.Order, error) {
	return orders.GetByVendorID(db.DB, vendorOrderID)
}

func (db *DB) ListOrders(status string, limit int) ([]*orders.Order, error) {
	return orders.List(db.DB, status, limit)
}

// ListOrdersFiltered returns orders matching the given filter with pagination.
func (db *DB) ListOrdersFiltered(f orders.Filter) ([]*orders.Order, error) {
	return orders.ListFiltered(db.DB, f)
}

func (db *DB) ListActiveOrders() ([]*orders.Order, error) { return orders.ListActive(db.DB) }

func (db *DB) ListActiveBoardOrders() ([]*orders.Order, error) { return orders.ListActiveBoard(db.DB) }

// ListActiveBoardOrdersFiltered scopes the board to a set of station IDs.
// Empty stations = plant-wide (same as ListActiveBoardOrders).
func (db *DB) ListActiveBoardOrdersFiltered(stations []string) ([]*orders.Order, error) {
	return orders.ListActiveBoardFiltered(db.DB, stations)
}

// ListOrderStations returns the distinct station IDs seen on orders.
func (db *DB) ListOrderStations() ([]string, error) {
	return orders.ListDistinctStations(db.DB)
}

func (db *DB) ListOrderHistory(orderID int64) ([]*orders.History, error) {
	return orders.ListHistory(db.DB, orderID)
}

// OrderEverReachedStatus reports whether the order ever recorded the status —
// see orders.EverReachedStatus.
func (db *DB) OrderEverReachedStatus(orderID int64, status string) (bool, error) {
	return orders.EverReachedStatus(db.DB, orderID, status)
}

func (db *DB) UpdateOrderPriority(id int64, priority int) error {
	return orders.UpdatePriority(db.DB, id, priority)
}

func (db *DB) ListOrdersByStation(stationID string, limit int) ([]*orders.Order, error) {
	return orders.ListByStation(db.DB, stationID, limit)
}

// ListActiveOrdersByStation returns the non-terminal orders for one station —
// the set the order reconcile compares an Edge's own list against.
func (db *DB) ListActiveOrdersByStation(stationID string) ([]*orders.Order, error) {
	return orders.ListActiveByStation(db.DB, stationID)
}

// CountActiveOrdersByDeliveryNode counts non-terminal orders targeting a
// specific delivery node.
func (db *DB) CountActiveOrdersByDeliveryNode(nodeName string) (int, error) {
	return orders.CountActiveByDeliveryNode(db.DB, nodeName)
}

// CountActiveOrders returns the number of non-terminal orders (dashboard
// "in flight" KPI).
func (db *DB) CountActiveOrders() (int, error) {
	return orders.CountActive(db.DB)
}

// ListTrackedVendorOrderIDs returns the vendor order IDs Core must keep watching.
// (The old comment here said "all non-terminal orders", which it never was.)
func (db *DB) ListTrackedVendorOrderIDs() ([]string, error) {
	return orders.ListTrackedVendorOrderIDs(db.DB)
}

// ListActiveOrdersBySourceRef returns orders in pre-dispatch states (pending,
// sourcing, queued) whose source_node matches any of the provided names.
func (db *DB) ListActiveOrdersBySourceRef(names []string) ([]*orders.Order, error) {
	return orders.ListActiveBySourceRef(db.DB, names)
}

// ListAcquiringOrders returns all orders in an acquiring status (queued or
// sourcing) — the fulfillment scanner's retry set — priority then FIFO.
func (db *DB) ListAcquiringOrders() ([]*orders.Order, error) { return orders.ListAcquiring(db.DB) }

// UpdateOrderPayloadCode sets the payload_code on an order.
func (db *DB) UpdateOrderPayloadCode(orderID int64, payloadCode string) error {
	return orders.UpdatePayloadCode(db.DB, orderID, payloadCode)
}

// UpdateOrderBinID sets the bin_id on an order. Kept as a delegate even
// though the function lives in orders/ because every outer caller expects
// this name.
func (db *DB) UpdateOrderBinID(orderID, binID int64) error {
	return orders.UpdateBinID(db.DB, orderID, binID)
}

// ListOrdersByBin returns recent orders involving a specific bin.
// Cross-aggregate entry point: the query lives in orders/ (returns *orders.Order)
// but callers reach it via the bins-side delegate name.
func (db *DB) ListOrdersByBin(binID int64, limit int) ([]*orders.Order, error) {
	return orders.ListByBinID(db.DB, binID, limit)
}

// ActiveOrdersByBin returns non-terminal orders referencing a bin via bin_id.
// Cross-aggregate delegate for BinService's orphan-order guard on retire/move.
func (db *DB) ActiveOrdersByBin(binID int64) ([]*orders.Order, error) {
	return orders.ActiveByBinID(db.DB, binID)
}

// TerminalizeOrder transitions an order to a terminal status and releases ALL of
// its holds — bin claims, destination-slot claims, order_bins junction rows, and
// reservations (pending and confirmed) — in a single transaction. It is the one
// chokepoint that makes "reaching a terminal status releases everything" a
// structural invariant rather than several divergent write paths; transition()
// routes every IsTerminal target here (including the success terminal
// 'confirmed', whose reservation previously leaked through UpdateOrderStatus and
// bricked the bin via the uq_reservations_bin_active partial unique index).
//
// Any bin still claimed by this order and parked at _TRANSIT when the order
// terminalizes never arrived anywhere, so it is stamped anomalous (anomaly_at,
// the signal operator recovery picks up via ListAnomalousTransitBins) regardless
// of which terminal we reached. In the happy path this matches ZERO rows: a
// delivered bin was moved out of _TRANSIT and unclaimed at delivery time. It
// fires only when an arrival failed or was skipped — including the confirmed
// case where the operator confirmed receipt but the engine's delivery-arrival
// write never landed (the completion safety-net can't recover it because this
// chokepoint has, correctly, already cleared claimed_by). error_detail is
// persisted for every terminal except the clean success 'confirmed' (which would
// otherwise surface receipt text as an "error"); order_history keeps the full
// detail regardless. Cross-aggregate.
// HistoryReason is the typed reason recorded on an order_history row —
// order_history.code / actor / ref (migration 55).
//
// Zero value means "uncoded", which is correct for a transition with no
// category: most of them. Never invent a code to fill it.
type HistoryReason struct {
	Code  string           // protocol.TermCode on a terminal, protocol.QueueCode on a →queued row
	Actor string           // who caused it
	Ref   protocol.TermRef // what it concerns: node / payload / peer
}

// refJSON renders Ref for the JSONB column, or nil when it carries nothing —
// so an empty reference is SQL NULL rather than the string "{}", and the
// partial indexes stay small.
func (r HistoryReason) refJSON() any {
	if r.Ref.Empty() {
		return nil
	}
	b, err := json.Marshal(r.Ref)
	if err != nil {
		return nil
	}
	return string(b)
}

func (db *DB) TerminalizeOrder(orderID int64, status protocol.Status, detail string) (bool, error) {
	return db.TerminalizeOrderWithReason(orderID, status, detail, HistoryReason{})
}

// TerminalizeOrderWithReason is TerminalizeOrder plus the typed reason for the
// history row. Event.ErrorCode and Event.Actor were being set at every Fail
// and Skip call site and dropped in transition(), which passed only the prose
// detail down here — so the categories existed in memory and never reached
// disk.
func (db *DB) TerminalizeOrderWithReason(orderID int64, status protocol.Status, detail string, reason HistoryReason) (bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// error_detail is suppressed only for the clean success 'confirmed' (which
	// would otherwise surface receipt text as an "error").
	errDetail := detail
	if status == protocol.StatusConfirmed {
		errDetail = ""
	}
	// COMPARE-AND-SWAP on "still live". Two callers can each hold a snapshot
	// showing a non-terminal order and both pass lifecycle's guard — an operator
	// cancel racing a scanner fail is the everyday pair. Unguarded, the second
	// write flipped an already-terminal order to a different terminal and fired a
	// second actionMap entry (fireCancelled AND fireFailed for one order).
	//
	// The predicate is "current status is not terminal" rather than
	// "current status = the caller's from". A cancel is operator intent that must
	// land from ANY live state, and an order legitimately moves non-terminally
	// (queued→sourcing) between snapshot and cancel; keying on `from` would
	// silently refuse that cancel, and CancelOrder returns no error to notice it
	// with. Terminal-absorbs-terminal is the property we actually want.
	//
	// The set is derived from the transition table (protocol.TerminalStatuses),
	// not hard-coded here, so adding a terminal status can't quietly bypass this.
	terminals := protocol.TerminalStatuses()
	terminalNames := make([]string, len(terminals))
	for i, t := range terminals {
		terminalNames[i] = string(t)
	}
	// updated_at from clock.Now(), not the database's NOW(): the daily
	// terminal-volume histogram buckets on COALESCE(completed_at, updated_at),
	// so a DB-clock stamp here puts a sim order's terminal in a different
	// (real-time) day from its creation.
	res, err := tx.Exec(`UPDATE orders SET status=$1, error_detail=$2, updated_at=$5
		WHERE id=$3 AND status <> ALL($4)`, string(status), errDetail, orderID, terminalNames, clock.Now().UTC())
	if err != nil {
		return false, err
	}
	moved, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	won := moved > 0
	// History only for the winner — a losing terminalize must not add a second
	// terminal row to the order's audit trail.
	if won {
		if _, err := tx.Exec(
			`INSERT INTO order_history (order_id, status, detail, code, actor, ref, created_at)
			 VALUES ($1, $2, $3, NULLIF($4,''), NULLIF($5,''), $6, $7)`,
			orderID, string(status), detail,
			reason.Code, reason.Actor, reason.refJSON(), clock.Now().UTC()); err != nil {
			return false, err
		}
	}
	// EVERYTHING BELOW RUNS FOR THE LOSER TOO, and commits.
	//
	// The loser must not assume the winner released this order's holds. Every
	// release here is keyed on the ORDER id and is idempotent by construction
	// (WHERE claimed_by=$1 matches zero rows the second time; the DELETE and the
	// reservation release are set-based), so running them twice is a no-op — but
	// running them ZERO times because we returned early would strand a claim on a
	// bin forever, which is the exact leak this chokepoint exists to prevent.
	// Idempotent release is an invariant of this function, not an accident.
	// Anomaly mark MUST run before claim release: the WHERE filters on
	// claimed_by=$orderID, which the next statement clears. COALESCE preserves an
	// earlier stamp. Unconditional across terminals — a bin still claimed by this
	// order and parked at _TRANSIT never arrived, whether the order failed, was
	// skipped, or was confirmed with a lost arrival write. Zero rows on the happy
	// path (a delivered bin already left _TRANSIT and dropped its claim).
	if _, err := tx.Exec(`
		UPDATE bins SET anomaly_at=COALESCE(anomaly_at, NOW()), updated_at=NOW()
		WHERE claimed_by=$1
		  AND node_id IN (SELECT id FROM nodes WHERE name='_TRANSIT')`, orderID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE bins SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID); err != nil {
		return false, err
	}
	// Release this order's destination-slot claims too (store dual of the bin
	// release above); ReleaseOrphanedClaims is the defense-in-depth backstop.
	if _, err := tx.Exec(`UPDATE nodes SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM order_bins WHERE order_id=$1`, orderID); err != nil {
		return false, err
	}
	// Release any reservations this order holds (pending or confirmed). Must run
	// in the same tx so no window exists where the order is terminal but its
	// reservation still blocks the bin. The owner-liveness reaper is the
	// defense-in-depth backstop for any row that leaks past this path.
	if err := reservations.ReleaseByOrder(tx, orderID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return won, nil
}

// FailOrderAtomic transitions an order to "failed" and releases all its holds.
// A failure marks _TRANSIT bins anomalous (a claim released mid-flight is a leak
// to investigate). Thin wrapper over TerminalizeOrder.
func (db *DB) FailOrderAtomic(orderID int64, detail string) error {
	_, err := db.TerminalizeOrder(orderID, protocol.StatusFailed, detail)
	return err
}

// ReleaseClaimForBin is the inverse of a single ClaimForDispatch: it clears the
// bin's claim AND releases its reservation in one transaction. Dispatch-failure
// rollbacks route through this instead of a bare UnclaimBin, which would clear
// claimed_by only and orphan the CONFIRMED reservation ClaimForDispatch leaves
// on success — bricking the bin via uq_reservations_bin_active. Owner-scoped
// (only clears claimed_by held by orderID) and bin-keyed on the reservation (the
// unique index guarantees at most one active row per bin, and this order owns
// it). Idempotent: a not-claimed / not-reserved bin is a harmless no-op.
func (db *DB) ReleaseClaimForBin(binID, orderID int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE bins SET claimed_by=NULL, updated_at=NOW() WHERE id=$1 AND claimed_by=$2`, binID, orderID); err != nil {
		return err
	}
	if err := reservations.ReleaseByBin(tx, binID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReleaseClaimByOrder is the multi-resource inverse: it clears claimed_by for
// every bin AND every slot this order holds, deletes its order_bins junction rows,
// and releases all its reservations (both kinds) in one transaction. The coupled
// replacement for UnclaimOrderBins on rollback / re-queue paths that abandon an
// order's claims without going terminal (which would otherwise leak the confirmed
// reservations). Idempotent.
//
// Release-set unification: this now releases the SAME set as
// TerminalizeOrder (minus the terminal-only _TRANSIT anomaly stamp). Before the
// slot substrate it cleared only bins + reservations; once slots are
// reservation-backed and hard-claimed via ConfirmSlotClaim, dropping the bin
// claims without the slot claims + order_bins would strand them — the gap this
// commit makes real, so it closes it.
func (db *DB) ReleaseClaimByOrder(orderID int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE bins SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID); err != nil {
		return err
	}
	// Slot claims too (store dual of the bin release above).
	if _, err := tx.Exec(`UPDATE nodes SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM order_bins WHERE order_id=$1`, orderID); err != nil {
		return err
	}
	// Both reservation kinds — ReleaseByOrder is order-keyed and kind-agnostic.
	if err := reservations.ReleaseByOrder(tx, orderID); err != nil {
		return err
	}
	return tx.Commit()
}

// CountInFlightOrdersByDeliveryNode counts non-queued, non-terminal active
// orders targeting a delivery node.
func (db *DB) CountInFlightOrdersByDeliveryNode(deliveryNode string) (int, error) {
	return orders.CountInFlightByDeliveryNode(db.DB, deliveryNode)
}

// CountInFlightOrdersByDeliveryNodeExcluding counts in-flight orders for a
// delivery node, excluding a specific order ID (the caller's own row).
// Phase 4c of bin-transit-state: planning-time capacity gates need to
// avoid self-collision when checking from inside the order's own
// dispatch path.
func (db *DB) CountInFlightOrdersByDeliveryNodeExcluding(deliveryNode string, excludeID int64) (int, error) {
	return orders.CountInFlightByDeliveryNodeExcluding(db.DB, deliveryNode, excludeID)
}

// CountLiveOrdersByOrigin counts a demand episode's own non-terminal orders —
// unlike the two counts above, `queued` is included. The sizing half of the
// replenishment bound. See orders.CountLiveByOrigin.
func (db *DB) CountLiveOrdersByOrigin(originID string) (int, error) {
	return orders.CountLiveByOrigin(db.DB, originID)
}

// CountLiveOrdersByDeliveryNode counts every non-terminal order pointed at a
// node, `queued` included and regardless of origin — "is this window spoken
// for". See orders.CountLiveByDeliveryNode.
func (db *DB) CountLiveOrdersByDeliveryNode(deliveryNode string) (int, error) {
	return orders.CountLiveByDeliveryNode(db.DB, deliveryNode)
}

// CountLiveCarrierRequestsByDeliveryNode counts every non-terminal order that
// asked for a CARRIER at a node — queued included, origin-blind, returns
// excluded. "Has a carrier already been asked for here". See
// orders.CountLiveCarrierRequestsByDeliveryNode.
func (db *DB) CountLiveCarrierRequestsByDeliveryNode(deliveryNode string) (int, error) {
	return orders.CountLiveCarrierRequestsByDeliveryNode(db.DB, deliveryNode)
}

func (db *DB) UpdateOrderRobotID(id int64, robotID string) error {
	return orders.UpdateRobotID(db.DB, id, robotID)
}

func (db *DB) ActiveOrderIDsByRobot() (map[string]int64, error) {
	return orders.ActiveIDsByRobot(db.DB)
}
