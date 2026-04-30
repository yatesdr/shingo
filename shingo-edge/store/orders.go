package store

// Phase 5b delegate file: order CRUD now lives in store/orders/.
// This file preserves the *store.DB method surface so external callers
// do not need to change.

import (
	"time"

	"shingoedge/store/orders"
)

// ListOrders returns every order, newest first.
func (db *DB) ListOrders() ([]orders.Order, error) {
	return orders.List(db.DB)
}

// ListActiveOrders returns every non-terminal order, newest first.
func (db *DB) ListActiveOrders() ([]orders.Order, error) {
	return orders.ListActive(db.DB)
}

// CountActiveOrders returns the count of non-terminal orders.
func (db *DB) CountActiveOrders() int {
	return orders.CountActive(db.DB)
}

// ListActiveOrdersByProcess returns non-terminal orders for one process.
func (db *DB) ListActiveOrdersByProcess(processID int64) ([]orders.Order, error) {
	return orders.ListActiveByProcess(db.DB, processID)
}

// GetOrder returns one order by id.
func (db *DB) GetOrder(id int64) (*orders.Order, error) {
	return orders.Get(db.DB, id)
}

// GetOrderByUUID returns one order by uuid.
func (db *DB) GetOrderByUUID(uuid string) (*orders.Order, error) {
	return orders.GetByUUID(db.DB, uuid)
}

// CreateOrder inserts an order and returns the new row id.
func (db *DB) CreateOrder(uuid, orderType string, processNodeID *int64, retrieveEmpty bool, quantity int64, deliveryNode, stagingNode, sourceNode, loadType string, autoConfirm bool, payloadCode string) (int64, error) {
	return orders.Create(db.DB, uuid, orderType, processNodeID, retrieveEmpty, quantity, deliveryNode, stagingNode, sourceNode, loadType, autoConfirm, payloadCode)
}

// UpdateOrderProcessNode rebinds an order to a different process_node.
func (db *DB) UpdateOrderProcessNode(id int64, processNodeID *int64) error {
	return orders.UpdateProcessNode(db.DB, id, processNodeID)
}

// UpdateOrderStatus changes the order status and bumps updated_at.
func (db *DB) UpdateOrderStatus(id int64, newStatus string) error {
	return orders.UpdateStatus(db.DB, id, newStatus)
}

// UpdateOrderWaybill writes the carrier waybill and ETA fields.
func (db *DB) UpdateOrderWaybill(id int64, waybillID, eta string) error {
	return orders.UpdateWaybill(db.DB, id, waybillID, eta)
}

// UpdateOrderETA sets the ETA on an order.
func (db *DB) UpdateOrderETA(id int64, eta string) error {
	return orders.UpdateETA(db.DB, id, eta)
}

// UpdateOrderFinalCount writes the final count and operator-confirmation
// flag.
func (db *DB) UpdateOrderFinalCount(id int64, finalCount int64, confirmed bool) error {
	return orders.UpdateFinalCount(db.DB, id, finalCount, confirmed)
}

// UpdateOrderDeliveryNode rebinds the delivery node on an order.
func (db *DB) UpdateOrderDeliveryNode(id int64, deliveryNode string) error {
	return orders.UpdateDeliveryNode(db.DB, id, deliveryNode)
}

// UpdateOrderStepsJSON stores the per-order steps document used by the
// scenesync runtime.
func (db *DB) UpdateOrderStepsJSON(id int64, stepsJSON string) error {
	return orders.UpdateStepsJSON(db.DB, id, stepsJSON)
}

// UpdateOrderStagedExpireAt sets (or clears) the staged_expire_at
// timestamp on an order.
func (db *DB) UpdateOrderStagedExpireAt(id int64, stagedExpireAt *time.Time) error {
	return orders.UpdateStagedExpireAt(db.DB, id, stagedExpireAt)
}

// UpdateOrderBinUOPRemaining sets (or clears) the bin_uop_remaining
// snapshot on an order. Captures the bin's authoritative uop_remaining
// from Core at delivery so handleNormalReplenishment can reset lineside
// UOP without guessing claim.UOPCapacity.
func (db *DB) UpdateOrderBinUOPRemaining(id int64, binUOPRemaining *int) error {
	return orders.UpdateBinUOPRemaining(db.DB, id, binUOPRemaining)
}

// InsertOrderHistory writes one order_history row.
func (db *DB) InsertOrderHistory(orderID int64, oldStatus, newStatus, detail string) error {
	return orders.InsertHistory(db.DB, orderID, oldStatus, newStatus, detail)
}

// ListStagedOrdersByProcessNode returns staged orders linked to a
// specific process_node.
func (db *DB) ListStagedOrdersByProcessNode(processNodeID int64) ([]orders.Order, error) {
	return orders.ListStagedByProcessNode(db.DB, processNodeID)
}

// ListActiveOrdersByProcessNodeAndType returns non-terminal orders for
// a process node filtered by order type.
func (db *DB) ListActiveOrdersByProcessNodeAndType(processNodeID int64, orderType string) ([]orders.Order, error) {
	return orders.ListActiveByProcessNodeAndType(db.DB, processNodeID, orderType)
}

// ListActiveOrdersByProcessNode returns non-terminal orders for a
// process node.
func (db *DB) ListActiveOrdersByProcessNode(processNodeID int64) ([]orders.Order, error) {
	return orders.ListActiveByProcessNode(db.DB, processNodeID)
}

// ListActiveOrdersByProcessNodeOrSource returns non-terminal orders that
// are either tracked at the given process node OR source from the given
// source node name. The station service uses this so a manual_swap
// loader sees demand for orders sourcing from its bin even when those
// orders are tracked at a different (consumer) node.
func (db *DB) ListActiveOrdersByProcessNodeOrSource(processNodeID int64, sourceNodeName string) ([]orders.Order, error) {
	return orders.ListActiveByProcessNodeOrSource(db.DB, processNodeID, sourceNodeName)
}

// ListOrderHistory returns the status history for one order, oldest
// first.
func (db *DB) ListOrderHistory(orderID int64) ([]orders.History, error) {
	return orders.ListHistory(db.DB, orderID)
}
