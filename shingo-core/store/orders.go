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
	Order        *orders.Order
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
		err := tx.QueryRow(`INSERT INTO orders (edge_uuid, station_id, order_type, status, quantity, source_node, delivery_node, priority, payload_desc, parent_order_id, sequence, steps_json, bin_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING id`,
			o.EdgeUUID, o.StationID, o.OrderType, o.Status,
			o.Quantity,
			o.SourceNode, o.DeliveryNode, o.Priority, o.PayloadDesc,
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

func (db *DB) UpdateOrderVendor(id int64, vendorOrderID, vendorState, robotID string) error {
	return orders.UpdateVendor(db.DB, id, vendorOrderID, vendorState, robotID)
}

func (db *DB) UpdateOrderSourceNode(id int64, sourceNode string) error {
	return orders.UpdateSourceNode(db.DB, id, sourceNode)
}

func (db *DB) UpdateOrderDeliveryNode(id int64, deliveryNode string) error {
	return orders.UpdateDeliveryNode(db.DB, id, deliveryNode)
}

func (db *DB) CompleteOrder(id int64) error            { return orders.Complete(db.DB, id) }
func (db *DB) GetOrder(id int64) (*orders.Order, error)       { return orders.Get(db.DB, id) }
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
	if _, err := tx.Exec(`UPDATE bins SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID); err != nil {
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
	if _, err := tx.Exec(`UPDATE bins SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID); err != nil {
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

func (db *DB) UpdateOrderRobotID(id int64, robotID string) error {
	return orders.UpdateRobotID(db.DB, id, robotID)
}
