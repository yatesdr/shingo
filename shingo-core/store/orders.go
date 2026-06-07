package store

// Stage 2D delegate file: order CRUD and history live in store/orders/.
// Cross-aggregate methods (CreateCompoundChildren, FailOrderAtomic,
// CancelOrderAtomic) stay here because they mutate both the orders and
// bins tables in a single transaction.

import (
	"fmt"

	"shingocore/store/internal/helpers"
	"shingocore/store/orders"
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
		var id int64
		err := tx.QueryRow(`INSERT INTO orders (edge_uuid, station_id, order_type, status, quantity, source_node, delivery_node, process_node, priority, payload_desc, parent_order_id, sequence, steps_json, bin_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING id`,
			o.EdgeUUID, o.StationID, o.OrderType, o.Status,
			o.Quantity,
			o.SourceNode, o.DeliveryNode, o.ProcessNode, o.Priority, o.PayloadDesc,
			helpers.NullableInt64(o.ParentOrderID), o.Sequence, o.StepsJSON,
			helpers.NullableInt64(o.BinID)).Scan(&id)
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

// UpdateOrderWaitIndex increments the wait_index for a complex order after
// releasing one wait segment.
func (db *DB) UpdateOrderWaitIndex(id int64, waitIndex int) error {
	return orders.UpdateWaitIndex(db.DB, id, waitIndex)
}

// SetOrderQueueReason stores or clears the blocking reason on a queued
// order. Phase 4 of bin-transit-state.
func (db *DB) SetOrderQueueReason(id int64, reason string) error {
	return orders.SetQueueReason(db.DB, id, reason)
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

func (db *DB) UpdateOrderPriority(id int64, priority int) error {
	return orders.UpdatePriority(db.DB, id, priority)
}

func (db *DB) ListOrdersByStation(stationID string, limit int) ([]*orders.Order, error) {
	return orders.ListByStation(db.DB, stationID, limit)
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

// ListDispatchedVendorOrderIDs returns vendor order IDs for all non-terminal
// orders.
func (db *DB) ListDispatchedVendorOrderIDs() ([]string, error) {
	return orders.ListDispatchedVendorOrderIDs(db.DB)
}

// ListActiveOrdersBySourceRef returns orders in pre-dispatch states (pending,
// sourcing, queued) whose source_node matches any of the provided names.
func (db *DB) ListActiveOrdersBySourceRef(names []string) ([]*orders.Order, error) {
	return orders.ListActiveBySourceRef(db.DB, names)
}

// ListQueuedOrders returns all orders in "queued" status, oldest first (FIFO).
func (db *DB) ListQueuedOrders() ([]*orders.Order, error) { return orders.ListQueued(db.DB) }

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

// FailOrderAtomic transitions an order to "failed" and releases all bin
// claims in a single transaction. This prevents the leak where
// UpdateOrderStatus succeeds but UnclaimOrderBins fails silently, leaving
// bins permanently claimed by a terminal order. Cross-aggregate.
func (db *DB) FailOrderAtomic(orderID int64, detail string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE orders SET status='failed', error_detail=$1, updated_at=NOW() WHERE id=$2`, detail, orderID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO order_history (order_id, status, detail) VALUES ($1, 'failed', $2)`, orderID, detail); err != nil {
		return err
	}
	// Anomaly mark MUST run before claim release: the WHERE filters on
	// claimed_by=$orderID, which the next statement clears. Bins at
	// _TRANSIT with no live claim are the binary anomaly signal under
	// bin-transit-state — operator recovery picks them up via
	// ListAnomalousTransitBins. COALESCE preserves an earlier stamp if
	// a bin was already flagged from a prior failure.
	if _, err := tx.Exec(`
		UPDATE bins SET anomaly_at=COALESCE(anomaly_at, NOW()), updated_at=NOW()
		WHERE claimed_by=$1
		  AND node_id IN (SELECT id FROM nodes WHERE name='_TRANSIT')`, orderID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE bins SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID); err != nil {
		return err
	}
	// Release this order's destination-slot claims too (store dual of the bin
	// release above); ReleaseOrphanedClaims is the defense-in-depth backstop.
	if _, err := tx.Exec(`UPDATE nodes SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM order_bins WHERE order_id=$1`, orderID); err != nil {
		return err
	}
	return tx.Commit()
}

// SkipOrderAtomic transitions an order to "skipped" and releases all bin
// claims in a single transaction. Same atomic-write rationale as
// FailOrderAtomic. Distinct from Fail by intent: skipped means "the work
// was never needed" (the world already advanced past the order's purpose,
// e.g. complex evac with no bin at any pickup node). Bins are NOT marked
// anomalous — the missing inventory is the expected condition, not a leak
// to investigate. Cross-aggregate.
func (db *DB) SkipOrderAtomic(orderID int64, detail string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE orders SET status='skipped', error_detail=$1, updated_at=NOW() WHERE id=$2`, detail, orderID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO order_history (order_id, status, detail) VALUES ($1, 'skipped', $2)`, orderID, detail); err != nil {
		return err
	}
	// Release any bin claims this order took during dispatch. In the
	// no_source_bin path that produces today's sole Skip, zero bins were
	// claimed so this is a no-op — included for symmetry with Fail/Cancel
	// and to keep future Skip producers safe by default.
	if _, err := tx.Exec(`UPDATE bins SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID); err != nil {
		return err
	}
	// Release this order's destination-slot claims too (store dual of the bin
	// release above); ReleaseOrphanedClaims is the defense-in-depth backstop.
	if _, err := tx.Exec(`UPDATE nodes SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM order_bins WHERE order_id=$1`, orderID); err != nil {
		return err
	}
	return tx.Commit()
}

// CancelOrderAtomic transitions an order to "cancelled" and releases all bin
// claims in a single transaction. Same rationale as FailOrderAtomic.
// Cross-aggregate.
func (db *DB) CancelOrderAtomic(orderID int64, detail string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE orders SET status='cancelled', error_detail=$1, updated_at=NOW() WHERE id=$2`, detail, orderID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO order_history (order_id, status, detail) VALUES ($1, 'cancelled', $2)`, orderID, detail); err != nil {
		return err
	}
	// Same anomaly-mark contract as FailOrderAtomic — see comments there.
	if _, err := tx.Exec(`
		UPDATE bins SET anomaly_at=COALESCE(anomaly_at, NOW()), updated_at=NOW()
		WHERE claimed_by=$1
		  AND node_id IN (SELECT id FROM nodes WHERE name='_TRANSIT')`, orderID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE bins SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID); err != nil {
		return err
	}
	// Release this order's destination-slot claims too (store dual of the bin
	// release above); ReleaseOrphanedClaims is the defense-in-depth backstop.
	if _, err := tx.Exec(`UPDATE nodes SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM order_bins WHERE order_id=$1`, orderID); err != nil {
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

func (db *DB) UpdateOrderRobotID(id int64, robotID string) error {
	return orders.UpdateRobotID(db.DB, id, robotID)
}
