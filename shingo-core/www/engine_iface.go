package www

import (
	"shingocore/config"
	"shingocore/dispatch"
	"shingocore/engine"
	"shingocore/fleet"
	"shingocore/messaging"
	"shingocore/service"
	"shingocore/store"
)

// EngineAccess defines the interface that www handlers require from the engine.
// Consumer-side interface per Go convention. *engine.Engine satisfies it.
//
// Stage 1 refactor removed DB() *store.DB; handlers now call the 101 named
// store methods directly on the Engine (see engine/engine_db_methods.go).
// If a new handler needs another store method, add a delegation there and a
// matching entry here — both sides stay in lockstep via the compile-time
// assertion at the bottom.
type EngineAccess interface {
	// Subsystem accessors
	AppConfig() *config.Config
	ConfigPath() string
	Dispatcher() *dispatch.Dispatcher
	Fleet() fleet.Backend
	Tracker() fleet.OrderTracker
	MsgClient() *messaging.Client
	EventBus() *engine.EventBus
	Reconciliation() *engine.ReconciliationService
	Recovery() *engine.RecoveryService
	BinManifest() *service.BinManifestService
	BinService() *service.BinService
	OrderService() *service.OrderService
	NodeService() *service.NodeService

	// Cached robot status
	GetCachedRobotStatus(vehicleID string) (fleet.RobotStatus, bool)
	GetAllCachedRobots() []fleet.RobotStatus

	// Corrections
	ApplyCorrection(req engine.ApplyCorrectionRequest) (int64, error)
	ApplyBatchCorrection(req engine.BatchCorrectionRequest) error

	// Orders
	CreateDirectOrder(req engine.DirectOrderRequest) (*engine.DirectOrderResult, error)
	TerminateOrder(orderID int64, actor string) error

	// Node queries
	GetNodeOccupancy() ([]engine.OccupancyEntry, error)

	// Scene sync
	SceneSync() (int, int, int, error)
	SyncScenePoints(areas []fleet.SceneArea) (int, map[string]string)
	UpdateNodeZones(locMap map[string]string, overwrite bool)

	// Messaging
	SendDataToEdge(subject string, stationID string, payload any) error

	// Live reconfiguration
	ReconfigureDatabase()
	ReconfigureFleet()
	ReconfigureMessaging()
	ReconfigureCountGroups()

	// ── Named store methods (delegates to *store.DB) ────────────────
	// Kept alphabetical to make audits / diff reviews easier.
	AddBinNote(binID int64, noteType, message, actor string) error
	AddLane(groupID int64, name string) (int64, error)
	AppendAudit(entityType string, entityID int64, action, oldValue, newValue, actor string) error
	BinHasNotes(binIDs []int64) (map[int64]bool, error)
	ClaimBin(binID, orderID int64) error
	ClearAllProduced() error
	CountBinsByAllNodes() (map[int64]int, error)
	ClearProduced(id int64) error
	CompleteTestCommand(id int64) error
	CountBinsByNode(nodeID int64) (int, error)
	CreateBin(b *store.Bin) error
	CreateBinType(bt *store.BinType) error
	CreateDemand(catID, description string, demandQty int64) (int64, error)
	CreateNode(n *store.Node) error
	CreateNodeGroup(name string) (int64, error)
	CreateOrder(o *store.Order) error
	CreatePayload(p *store.Payload) error
	CreatePayloadManifestItem(item *store.PayloadManifestItem) error
	CreateTestCommand(tc *store.TestCommand) error
	DeleteBin(id int64) error
	DeleteBinType(id int64) error
	DeleteDemand(id int64) error
	DeleteNode(id int64) error
	DeleteNodeGroup(grpID int64) error
	DeleteNodeProperty(nodeID int64, key string) error
	DeletePayload(id int64) error
	DeletePayloadManifestItem(id int64) error
	FailOrderAtomic(orderID int64, detail string) error
	GetAdminUser(username string) (*store.AdminUser, error)
	GetBin(id int64) (*store.Bin, error)
	GetBinByLabel(label string) (*store.Bin, error)
	GetBinManifest(binID int64) (*store.BinManifest, error)
	GetBinType(id int64) (*store.BinType, error)
	GetDemand(id int64) (*store.Demand, error)
	GetEffectiveBinTypes(nodeID int64) ([]*store.BinType, error)
	GetEffectiveStations(nodeID int64) ([]string, error)
	GetGroupLayout(groupID int64) (*store.GroupLayout, error)
	GetMissionStats(f store.MissionFilter) (*store.MissionStats, error)
	GetMissionTelemetry(orderID int64) (*store.MissionTelemetry, error)
	GetNode(id int64) (*store.Node, error)
	GetNodeByDotName(name string) (*store.Node, error)
	GetNodeByName(name string) (*store.Node, error)
	GetNodeProperty(nodeID int64, key string) string
	GetOrder(id int64) (*store.Order, error)
	GetOrderByUUID(uuid string) (*store.Order, error)
	GetPayload(id int64) (*store.Payload, error)
	GetPayloadByCode(code string) (*store.Payload, error)
	GetReconciliationSummary() (*store.ReconciliationSummary, error)
	GetSlotDepth(nodeID int64) (int, error)
	GetTestCommand(id int64) (*store.TestCommand, error)
	ListActiveOrders() ([]*store.Order, error)
	ListActiveOrdersBySourceRef(names []string) ([]*store.Order, error)
	ListAllCMSTransactions(limit, offset int) ([]*store.CMSTransaction, error)
	ListBinTypes() ([]*store.BinType, error)
	ListBinTypesForNode(nodeID int64) ([]*store.BinType, error)
	ListBinTypesForPayload(payloadID int64) ([]*store.BinType, error)
	ListBins() ([]*store.Bin, error)
	ListBinsByNode(nodeID int64) ([]*store.Bin, error)
	ListCMSTransactions(nodeID int64, limit, offset int) ([]*store.CMSTransaction, error)
	ListChildNodes(parentID int64) ([]*store.Node, error)
	ListChildOrders(parentOrderID int64) ([]*store.Order, error)
	ListCorrectionsByNode(nodeID int64, limit int) ([]*store.Correction, error)
	ListDemands() ([]*store.Demand, error)
	ListEdges() ([]store.EdgeRegistration, error)
	ListEntityAudit(entityType string, entityID int64) ([]*store.AuditEntry, error)
	ListInventory() ([]store.InventoryRow, error)
	ListLaneSlots(laneID int64) ([]*store.Node, error)
	ListMissionEvents(orderID int64) ([]*store.MissionEvent, error)
	ListMissions(f store.MissionFilter) ([]*store.MissionTelemetry, int, error)
	ListNodeProperties(nodeID int64) ([]*store.NodeProperty, error)
	ListNodeStates() (map[int64]*store.NodeState, error)
	ListNodes() ([]*store.Node, error)
	NodeTileStates() (map[int64]store.NodeTileState, error)
	ListNodesForPayload(payloadID int64) ([]*store.Node, error)
	ListOrderHistory(orderID int64) ([]*store.OrderHistory, error)
	ListOrders(status string, limit int) ([]*store.Order, error)
	ListOrdersByBin(binID int64, limit int) ([]*store.Order, error)
	ListOrdersByStation(stationID string, limit int) ([]*store.Order, error)
	ListPayloadManifest(payloadID int64) ([]*store.PayloadManifestItem, error)
	ListPayloads() ([]*store.Payload, error)
	ListProductionLog(catID string, limit int) ([]*store.ProductionLogEntry, error)
	ListScenePoints() ([]*store.ScenePoint, error)
	ListScenePointsByArea(areaName string) ([]*store.ScenePoint, error)
	ListScenePointsByClass(className string) ([]*store.ScenePoint, error)
	ListStationsForNode(nodeID int64) ([]string, error)
	ListTestCommands(limit int) ([]*store.TestCommand, error)
	LockBin(binID int64, actor string) error
	MoveBin(binID, toNodeID int64) error
	Ping() error
	RecordBinCount(binID int64, actualUOP int, actor string) error
	ReleaseStagedBin(binID int64) error
	ReorderLaneSlots(laneID int64, orderedNodeIDs []int64) error
	ReparentNode(nodeID int64, parentID *int64, position int) error
	ReplacePayloadManifest(payloadID int64, items []*store.PayloadManifestItem) error
	SetBinManifestFromTemplate(binID int64, payloadCode string, uopCapacity int) error
	SetNodeBinTypes(nodeID int64, binTypeIDs []int64) error
	SetNodePayloads(nodeID int64, payloadIDs []int64) error
	SetNodeProperty(nodeID int64, key, value string) error
	SetNodeStations(nodeID int64, stationIDs []string) error
	SetPayloadBinTypes(payloadID int64, binTypeIDs []int64) error
	SetProduced(id int64, qty int64) error
	UnclaimBin(binID int64) error
	UnconfirmBinManifest(binID int64) error
	UnlockBin(binID int64) error
	UpdateBin(b *store.Bin) error
	UpdateBinStatus(binID int64, status string) error
	UpdateBinType(bt *store.BinType) error
	UpdateDemand(id int64, catID, description string, demandQty, producedQty int64) error
	UpdateDemandAndResetProduced(id int64, description string, demandQty int64) error
	UpdateNode(n *store.Node) error
	UpdateOrderPriority(id int64, priority int) error
	UpdateOrderStatus(id int64, status, detail string) error
	UpdateOrderVendor(id int64, vendorOrderID, vendorState, robotID string) error
	UpdatePayload(p *store.Payload) error
	UpdatePayloadManifestItem(id int64, partNumber string, quantity int64) error
	UpdateTestCommandStatus(id int64, vendorState, detail string) error
}

// Compile-time assertion that *engine.Engine satisfies EngineAccess.
// If this breaks, either add the missing method to *engine.Engine or
// remove it from the EngineAccess contract above.
var _ EngineAccess = (*engine.Engine)(nil)
