package store

// Phase 5b delegate file: order CRUD now lives in store/orders/.
// This file preserves the *store.DB method surface so external callers
// do not need to change.

import (
	"time"

	"shingo/protocol"
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
func (db *DB) CreateOrder(uuid string, orderType protocol.OrderType, processNodeID *int64, retrieveEmpty bool, quantity int64, deliveryNode, stagingNode, sourceNode, loadType string, autoConfirm bool, payloadCode string) (int64, error) {
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

// GetOrderStepsJSON returns the raw steps_json document for an order.
func (db *DB) GetOrderStepsJSON(id int64) (string, error) {
	return orders.GetStepsJSON(db.DB, id)
}

// UpdateOrderStagedExpireAt sets (or clears) the staged_expire_at
// timestamp on an order.
func (db *DB) UpdateOrderStagedExpireAt(id int64, stagedExpireAt *time.Time) error {
	return orders.UpdateStagedExpireAt(db.DB, id, stagedExpireAt)
}

// UpdateOrderBinID sets (or clears) the bin_id snapshot on an order.
// PLC tick attribution at consume/produce time reads the active
// order's BinID for delta envelope routing. Captured from the
// OrderDelivered envelope.
func (db *DB) UpdateOrderBinID(id int64, binID *int64) error {
	return orders.UpdateBinID(db.DB, id, binID)
}

// SetOrderQueueReason writes (or clears) the blocking reason on a queued order.
// Called from the edge handler when Core pushes an OrderUpdate with a QueueReason.
func (db *DB) SetOrderQueueReason(uuid, reason string) error {
	return orders.SetQueueReason(db.DB, uuid, reason)
}

// LinkOrderSiblings writes a bidirectional sibling_order_id pointer
// between two orders in a two-robot swap pair. Used so the supply
// guard and release gate can identify the pair without depending on
// volatile runtime slot pointers.
func (db *DB) LinkOrderSiblings(orderA, orderB int64) error {
	return orders.LinkSiblings(db.DB, orderA, orderB)
}

// ClearOrderSibling unidirectionally nulls the sibling pointer on one
// order. Test-only helper for simulating single-leg flows or silent
// linkage failures.
func (db *DB) ClearOrderSibling(orderID int64) error {
	return orders.ClearSibling(db.DB, orderID)
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
func (db *DB) ListActiveOrdersByProcessNodeAndType(processNodeID int64, orderType protocol.OrderType) ([]orders.Order, error) {
	return orders.ListActiveByProcessNodeAndType(db.DB, processNodeID, orderType)
}

// ListActiveOrdersByProcessNode returns non-terminal orders for a
// process node.
func (db *DB) ListActiveOrdersByProcessNode(processNodeID int64) ([]orders.Order, error) {
	return orders.ListActiveByProcessNode(db.DB, processNodeID)
}

// ListDeliveredRetrieveByDeliveryNode returns delivered retrieve orders matching
// retrieveEmpty whose delivery_node is the given core node name, regardless of
// which process_node row tracks them. The loader/unloader confirm-on-action
// paths use it so a shared loader/unloader's side-cycle order is found even when
// it's staged against a sibling process_node. See the orders-package docstring.
func (db *DB) ListDeliveredRetrieveByDeliveryNode(deliveryNode string, retrieveEmpty bool) ([]orders.Order, error) {
	return orders.ListDeliveredRetrieveByDeliveryNode(db.DB, deliveryNode, retrieveEmpty)
}

// ListActiveOrdersByDeliveryNode returns non-terminal orders whose delivery_node
// is the given core node name, across every process_node row. The manual_swap
// side-cycle in-flight / dedup checks use it so a shared loader/unloader counts
// orders at its physical slot regardless of which process_node staged them.
func (db *DB) ListActiveOrdersByDeliveryNode(deliveryNode string) ([]orders.Order, error) {
	return orders.ListActiveByDeliveryNode(db.DB, deliveryNode)
}

// ListActiveOrdersByDeliveryNodeSet returns non-terminal orders whose
// delivery_node is in the given set, in one query. The multi-window reservation
// seam counts a loader's in-flight empties across its whole delivery cluster with
// this — one snapshot, not N per-node snapshots that would reopen the count→fire
// race between reads.
func (db *DB) ListActiveOrdersByDeliveryNodeSet(deliveryNodes []string) ([]orders.Order, error) {
	return orders.ListActiveByDeliveryNodeSet(db.DB, deliveryNodes)
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
