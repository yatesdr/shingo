// engine_db_methods.go — thin delegating methods that forward to *store.DB.
//
// Stage 1 of the shingo-core/www refactor replaces h.engine.DB().X(...) with
// named methods on *engine.Engine so www handlers no longer need to reach
// through a *store.DB accessor. Each method here is a one-line pass-through
// intentionally; any non-trivial query logic belongs in a dedicated file
// (orders.go, nodes.go, etc.), not here.
//
// The list is fixed at the 101 store methods currently called from
// shingo-core/www. Add new entries only when a new handler needs one.

package engine

import (
	"shingocore/store"
)

func (e *Engine) AddLane(groupID int64, name string) (int64, error) {
	return e.db.AddLane(groupID, name)
}
func (e *Engine) AddBinNote(binID int64, noteType, message, actor string) error {
	return e.db.AddBinNote(binID, noteType, message, actor)
}
func (e *Engine) AppendAudit(entityType string, entityID int64, action, oldValue, newValue, actor string) error {
	return e.db.AppendAudit(entityType, entityID, action, oldValue, newValue, actor)
}
func (e *Engine) BinHasNotes(binIDs []int64) (map[int64]bool, error) {
	return e.db.BinHasNotes(binIDs)
}
func (e *Engine) ClaimBin(binID, orderID int64) error { return e.db.ClaimBin(binID, orderID) }
func (e *Engine) ClearAllProduced() error             { return e.db.ClearAllProduced() }
func (e *Engine) CountBinsByAllNodes() (map[int64]int, error) {
	return e.db.CountBinsByAllNodes()
}
func (e *Engine) ClearProduced(id int64) error        { return e.db.ClearProduced(id) }
func (e *Engine) CompleteTestCommand(id int64) error  { return e.db.CompleteTestCommand(id) }
func (e *Engine) CountBinsByNode(nodeID int64) (int, error) {
	return e.db.CountBinsByNode(nodeID)
}
func (e *Engine) CreateBin(b *store.Bin) error         { return e.db.CreateBin(b) }
func (e *Engine) CreateBinType(bt *store.BinType) error { return e.db.CreateBinType(bt) }
func (e *Engine) CreateDemand(catID, description string, demandQty int64) (int64, error) {
	return e.db.CreateDemand(catID, description, demandQty)
}
func (e *Engine) CreateNode(n *store.Node) error            { return e.db.CreateNode(n) }
func (e *Engine) CreateNodeGroup(name string) (int64, error) { return e.db.CreateNodeGroup(name) }
func (e *Engine) CreateOrder(o *store.Order) error          { return e.db.CreateOrder(o) }
func (e *Engine) CreatePayload(p *store.Payload) error      { return e.db.CreatePayload(p) }
func (e *Engine) CreatePayloadManifestItem(item *store.PayloadManifestItem) error {
	return e.db.CreatePayloadManifestItem(item)
}
func (e *Engine) CreateTestCommand(tc *store.TestCommand) error {
	return e.db.CreateTestCommand(tc)
}
func (e *Engine) DeleteBin(id int64) error        { return e.db.DeleteBin(id) }
func (e *Engine) DeleteBinType(id int64) error    { return e.db.DeleteBinType(id) }
func (e *Engine) DeleteDemand(id int64) error     { return e.db.DeleteDemand(id) }
func (e *Engine) DeleteNode(id int64) error       { return e.db.DeleteNode(id) }
func (e *Engine) DeleteNodeGroup(grpID int64) error { return e.db.DeleteNodeGroup(grpID) }
func (e *Engine) DeleteNodeProperty(nodeID int64, key string) error {
	return e.db.DeleteNodeProperty(nodeID, key)
}
func (e *Engine) DeletePayload(id int64) error             { return e.db.DeletePayload(id) }
func (e *Engine) DeletePayloadManifestItem(id int64) error { return e.db.DeletePayloadManifestItem(id) }
func (e *Engine) FailOrderAtomic(orderID int64, detail string) error {
	return e.db.FailOrderAtomic(orderID, detail)
}
func (e *Engine) GetAdminUser(username string) (*store.AdminUser, error) {
	return e.db.GetAdminUser(username)
}
func (e *Engine) GetBin(id int64) (*store.Bin, error)               { return e.db.GetBin(id) }
func (e *Engine) GetBinByLabel(label string) (*store.Bin, error)    { return e.db.GetBinByLabel(label) }
func (e *Engine) GetBinManifest(binID int64) (*store.BinManifest, error) {
	return e.db.GetBinManifest(binID)
}
func (e *Engine) GetBinType(id int64) (*store.BinType, error) { return e.db.GetBinType(id) }
func (e *Engine) GetDemand(id int64) (*store.Demand, error)   { return e.db.GetDemand(id) }
func (e *Engine) GetEffectiveBinTypes(nodeID int64) ([]*store.BinType, error) {
	return e.db.GetEffectiveBinTypes(nodeID)
}
func (e *Engine) GetEffectiveStations(nodeID int64) ([]string, error) {
	return e.db.GetEffectiveStations(nodeID)
}
func (e *Engine) GetGroupLayout(groupID int64) (*store.GroupLayout, error) {
	return e.db.GetGroupLayout(groupID)
}
func (e *Engine) GetMissionStats(f store.MissionFilter) (*store.MissionStats, error) {
	return e.db.GetMissionStats(f)
}
func (e *Engine) GetMissionTelemetry(orderID int64) (*store.MissionTelemetry, error) {
	return e.db.GetMissionTelemetry(orderID)
}
func (e *Engine) GetNode(id int64) (*store.Node, error)            { return e.db.GetNode(id) }
func (e *Engine) GetNodeByDotName(name string) (*store.Node, error) {
	return e.db.GetNodeByDotName(name)
}
func (e *Engine) GetNodeByName(name string) (*store.Node, error) { return e.db.GetNodeByName(name) }
func (e *Engine) GetNodeProperty(nodeID int64, key string) string {
	return e.db.GetNodeProperty(nodeID, key)
}
func (e *Engine) GetOrder(id int64) (*store.Order, error)           { return e.db.GetOrder(id) }
func (e *Engine) GetOrderByUUID(uuid string) (*store.Order, error)  { return e.db.GetOrderByUUID(uuid) }
func (e *Engine) GetPayload(id int64) (*store.Payload, error)       { return e.db.GetPayload(id) }
func (e *Engine) GetPayloadByCode(code string) (*store.Payload, error) {
	return e.db.GetPayloadByCode(code)
}
func (e *Engine) GetReconciliationSummary() (*store.ReconciliationSummary, error) {
	return e.db.GetReconciliationSummary()
}
func (e *Engine) GetSlotDepth(nodeID int64) (int, error) { return e.db.GetSlotDepth(nodeID) }
func (e *Engine) GetTestCommand(id int64) (*store.TestCommand, error) {
	return e.db.GetTestCommand(id)
}
func (e *Engine) ListActiveOrders() ([]*store.Order, error) { return e.db.ListActiveOrders() }
func (e *Engine) ListActiveOrdersBySourceRef(names []string) ([]*store.Order, error) {
	return e.db.ListActiveOrdersBySourceRef(names)
}
func (e *Engine) ListAllCMSTransactions(limit, offset int) ([]*store.CMSTransaction, error) {
	return e.db.ListAllCMSTransactions(limit, offset)
}
func (e *Engine) ListBinTypes() ([]*store.BinType, error) { return e.db.ListBinTypes() }
func (e *Engine) ListBinTypesForNode(nodeID int64) ([]*store.BinType, error) {
	return e.db.ListBinTypesForNode(nodeID)
}
func (e *Engine) ListBinTypesForPayload(payloadID int64) ([]*store.BinType, error) {
	return e.db.ListBinTypesForPayload(payloadID)
}
func (e *Engine) ListBins() ([]*store.Bin, error) { return e.db.ListBins() }
func (e *Engine) ListBinsByNode(nodeID int64) ([]*store.Bin, error) {
	return e.db.ListBinsByNode(nodeID)
}
func (e *Engine) ListCMSTransactions(nodeID int64, limit, offset int) ([]*store.CMSTransaction, error) {
	return e.db.ListCMSTransactions(nodeID, limit, offset)
}
func (e *Engine) ListChildNodes(parentID int64) ([]*store.Node, error) {
	return e.db.ListChildNodes(parentID)
}
func (e *Engine) ListChildOrders(parentOrderID int64) ([]*store.Order, error) {
	return e.db.ListChildOrders(parentOrderID)
}
func (e *Engine) ListCorrectionsByNode(nodeID int64, limit int) ([]*store.Correction, error) {
	return e.db.ListCorrectionsByNode(nodeID, limit)
}
func (e *Engine) ListDemands() ([]*store.Demand, error) { return e.db.ListDemands() }
func (e *Engine) ListEdges() ([]store.EdgeRegistration, error) { return e.db.ListEdges() }
func (e *Engine) ListEntityAudit(entityType string, entityID int64) ([]*store.AuditEntry, error) {
	return e.db.ListEntityAudit(entityType, entityID)
}
func (e *Engine) ListInventory() ([]store.InventoryRow, error) { return e.db.ListInventory() }
func (e *Engine) ListLaneSlots(laneID int64) ([]*store.Node, error) {
	return e.db.ListLaneSlots(laneID)
}
func (e *Engine) ListMissionEvents(orderID int64) ([]*store.MissionEvent, error) {
	return e.db.ListMissionEvents(orderID)
}
func (e *Engine) ListMissions(f store.MissionFilter) ([]*store.MissionTelemetry, int, error) {
	return e.db.ListMissions(f)
}
func (e *Engine) ListNodeProperties(nodeID int64) ([]*store.NodeProperty, error) {
	return e.db.ListNodeProperties(nodeID)
}
func (e *Engine) ListNodeStates() (map[int64]*store.NodeState, error) {
	return e.db.ListNodeStates()
}
func (e *Engine) ListNodes() ([]*store.Node, error) { return e.db.ListNodes() }
func (e *Engine) NodeTileStates() (map[int64]store.NodeTileState, error) {
	return e.db.NodeTileStates()
}
func (e *Engine) ListNodesForPayload(payloadID int64) ([]*store.Node, error) {
	return e.db.ListNodesForPayload(payloadID)
}
func (e *Engine) ListOrderHistory(orderID int64) ([]*store.OrderHistory, error) {
	return e.db.ListOrderHistory(orderID)
}
func (e *Engine) ListOrders(status string, limit int) ([]*store.Order, error) {
	return e.db.ListOrders(status, limit)
}
func (e *Engine) ListOrdersByBin(binID int64, limit int) ([]*store.Order, error) {
	return e.db.ListOrdersByBin(binID, limit)
}
func (e *Engine) ListOrdersByStation(stationID string, limit int) ([]*store.Order, error) {
	return e.db.ListOrdersByStation(stationID, limit)
}
func (e *Engine) ListPayloadManifest(payloadID int64) ([]*store.PayloadManifestItem, error) {
	return e.db.ListPayloadManifest(payloadID)
}
func (e *Engine) ListPayloads() ([]*store.Payload, error) { return e.db.ListPayloads() }
func (e *Engine) ListProductionLog(catID string, limit int) ([]*store.ProductionLogEntry, error) {
	return e.db.ListProductionLog(catID, limit)
}
func (e *Engine) ListScenePoints() ([]*store.ScenePoint, error) { return e.db.ListScenePoints() }
func (e *Engine) ListScenePointsByArea(areaName string) ([]*store.ScenePoint, error) {
	return e.db.ListScenePointsByArea(areaName)
}
func (e *Engine) ListScenePointsByClass(className string) ([]*store.ScenePoint, error) {
	return e.db.ListScenePointsByClass(className)
}
func (e *Engine) ListStationsForNode(nodeID int64) ([]string, error) {
	return e.db.ListStationsForNode(nodeID)
}
func (e *Engine) ListTestCommands(limit int) ([]*store.TestCommand, error) {
	return e.db.ListTestCommands(limit)
}
func (e *Engine) LockBin(binID int64, actor string) error { return e.db.LockBin(binID, actor) }
func (e *Engine) MoveBin(binID, toNodeID int64) error     { return e.db.MoveBin(binID, toNodeID) }
func (e *Engine) Ping() error                             { return e.db.Ping() }
func (e *Engine) RecordBinCount(binID int64, actualUOP int, actor string) error {
	return e.db.RecordBinCount(binID, actualUOP, actor)
}
func (e *Engine) ReleaseStagedBin(binID int64) error { return e.db.ReleaseStagedBin(binID) }
func (e *Engine) ReorderLaneSlots(laneID int64, orderedNodeIDs []int64) error {
	return e.db.ReorderLaneSlots(laneID, orderedNodeIDs)
}
func (e *Engine) ReparentNode(nodeID int64, parentID *int64, position int) error {
	return e.db.ReparentNode(nodeID, parentID, position)
}
func (e *Engine) ReplacePayloadManifest(payloadID int64, items []*store.PayloadManifestItem) error {
	return e.db.ReplacePayloadManifest(payloadID, items)
}
func (e *Engine) SetBinManifestFromTemplate(binID int64, payloadCode string, uopCapacity int) error {
	return e.db.SetBinManifestFromTemplate(binID, payloadCode, uopCapacity)
}
func (e *Engine) SetNodeBinTypes(nodeID int64, binTypeIDs []int64) error {
	return e.db.SetNodeBinTypes(nodeID, binTypeIDs)
}
func (e *Engine) SetNodePayloads(nodeID int64, payloadIDs []int64) error {
	return e.db.SetNodePayloads(nodeID, payloadIDs)
}
func (e *Engine) SetNodeProperty(nodeID int64, key, value string) error {
	return e.db.SetNodeProperty(nodeID, key, value)
}
func (e *Engine) SetNodeStations(nodeID int64, stationIDs []string) error {
	return e.db.SetNodeStations(nodeID, stationIDs)
}
func (e *Engine) SetPayloadBinTypes(payloadID int64, binTypeIDs []int64) error {
	return e.db.SetPayloadBinTypes(payloadID, binTypeIDs)
}
func (e *Engine) SetProduced(id int64, qty int64) error      { return e.db.SetProduced(id, qty) }
func (e *Engine) UnclaimBin(binID int64) error               { return e.db.UnclaimBin(binID) }
func (e *Engine) UnconfirmBinManifest(binID int64) error     { return e.db.UnconfirmBinManifest(binID) }
func (e *Engine) UnlockBin(binID int64) error                { return e.db.UnlockBin(binID) }
func (e *Engine) UpdateBin(b *store.Bin) error               { return e.db.UpdateBin(b) }
func (e *Engine) UpdateBinStatus(binID int64, status string) error {
	return e.db.UpdateBinStatus(binID, status)
}
func (e *Engine) UpdateBinType(bt *store.BinType) error { return e.db.UpdateBinType(bt) }
func (e *Engine) UpdateDemand(id int64, catID, description string, demandQty, producedQty int64) error {
	return e.db.UpdateDemand(id, catID, description, demandQty, producedQty)
}
func (e *Engine) UpdateDemandAndResetProduced(id int64, description string, demandQty int64) error {
	return e.db.UpdateDemandAndResetProduced(id, description, demandQty)
}
func (e *Engine) UpdateNode(n *store.Node) error { return e.db.UpdateNode(n) }
func (e *Engine) UpdateOrderPriority(id int64, priority int) error {
	return e.db.UpdateOrderPriority(id, priority)
}
func (e *Engine) UpdateOrderStatus(id int64, status, detail string) error {
	return e.db.UpdateOrderStatus(id, status, detail)
}
func (e *Engine) UpdateOrderVendor(id int64, vendorOrderID, vendorState, robotID string) error {
	return e.db.UpdateOrderVendor(id, vendorOrderID, vendorState, robotID)
}
func (e *Engine) UpdatePayload(p *store.Payload) error { return e.db.UpdatePayload(p) }
func (e *Engine) UpdatePayloadManifestItem(id int64, partNumber string, quantity int64) error {
	return e.db.UpdatePayloadManifestItem(id, partNumber, quantity)
}
func (e *Engine) UpdateTestCommandStatus(id int64, vendorState, detail string) error {
	return e.db.UpdateTestCommandStatus(id, vendorState, detail)
}
