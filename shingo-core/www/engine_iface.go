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
type EngineAccess interface {
	// Subsystem accessors
	DB() *store.DB
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
}

// Compile-time assertion that *engine.Engine satisfies EngineAccess.
// If this breaks, either add the missing method to *engine.Engine or
// remove it from the EngineAccess contract above.
var _ EngineAccess = (*engine.Engine)(nil)
