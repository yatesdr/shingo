package seerrds

import (
	"encoding/json"
	"fmt"
	"time"

	"shingocore/fleet"
	"shingocore/rds"

	"github.com/google/uuid"
)

// Config holds the configuration for creating a Seer RDS adapter.
type Config struct {
	BaseURL      string
	Timeout      time.Duration
	PollInterval time.Duration
	DebugLog     func(string, ...any)
}

// Adapter wraps an rds.Client to implement fleet.TrackingBackend,
// fleet.RobotLister, fleet.NodeOccupancyProvider, and fleet.VendorProxy.
type Adapter struct {
	client       *rds.Client
	pollInterval time.Duration
	poller       *rds.Poller
	debugLog     func(string, ...any)
}

// New creates a new Seer RDS adapter.
func New(cfg Config) *Adapter {
	client := rds.NewClient(cfg.BaseURL, cfg.Timeout)
	client.DebugLog = cfg.DebugLog
	return &Adapter{
		client:       client,
		pollInterval: cfg.PollInterval,
		debugLog:     cfg.DebugLog,
	}
}

// --- fleet.Backend ---

func (a *Adapter) CreateTransportOrder(req fleet.TransportOrderRequest) (fleet.TransportOrderResult, error) {
	rdsReq := &rds.SetJoinOrderRequest{
		ID:         req.OrderID,
		ExternalID: req.ExternalID,
		FromLoc:    req.FromLoc,
		ToLoc:      req.ToLoc,
		Priority:   req.Priority,
	}
	if err := a.client.CreateJoinOrder(rdsReq); err != nil {
		return fleet.TransportOrderResult{}, err
	}
	return fleet.TransportOrderResult{VendorOrderID: req.OrderID}, nil
}

func (a *Adapter) CancelOrder(vendorOrderID string) error {
	return a.client.TerminateOrder(&rds.TerminateRequest{
		ID:             vendorOrderID,
		DisableVehicle: false,
	})
}

func (a *Adapter) SetOrderPriority(vendorOrderID string, priority int) error {
	return a.client.SetPriority(vendorOrderID, priority)
}

func (a *Adapter) Ping() error {
	_, err := a.client.Ping()
	return err
}

func (a *Adapter) Name() string {
	return "SEER RDS"
}

func (a *Adapter) MapState(vendorState string) string {
	return MapState(vendorState)
}

func (a *Adapter) IsTerminalState(vendorState string) bool {
	return IsTerminalState(vendorState)
}

func (a *Adapter) Reconfigure(cfg fleet.ReconfigureParams) {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	a.client.Reconfigure(cfg.BaseURL, timeout)
}

// --- fleet.TrackingBackend ---

func (a *Adapter) InitTracker(emitter fleet.TrackerEmitter, resolver fleet.OrderIDResolver) {
	bridge := &emitterBridge{emitter: emitter}
	resolverBridge := &resolverBridge{resolver: resolver}
	a.poller = rds.NewPoller(a.client, bridge, resolverBridge, a.pollInterval)
	a.poller.DebugLog = a.debugLog
}

func (a *Adapter) Tracker() fleet.OrderTracker {
	return a.poller
}

// --- fleet.RobotLister ---

func (a *Adapter) GetRobotsStatus() ([]fleet.RobotStatus, error) {
	robots, err := a.client.GetRobotsStatus()
	if err != nil {
		return nil, err
	}
	result := make([]fleet.RobotStatus, len(robots))
	for i, r := range robots {
		result[i] = mapRobotStatus(r)
	}
	return result, nil
}

func (a *Adapter) SetAvailability(vehicleID string, available bool) error {
	dispatchType := "undispatchable_unignore"
	if available {
		dispatchType = "dispatchable"
	}
	return a.client.SetDispatchable(&rds.DispatchableRequest{
		Vehicles: []string{vehicleID},
		Type:     dispatchType,
	})
}

func (a *Adapter) RetryFailed(vehicleID string) error {
	return a.client.RedoFailed(&rds.RedoFailedRequest{
		Vehicles: []string{vehicleID},
	})
}

func (a *Adapter) ForceComplete(vehicleID string) error {
	return a.client.ManualFinish(&rds.ManualFinishRequest{
		Vehicles: []string{vehicleID},
	})
}

// --- fleet.NodeOccupancyProvider ---

func (a *Adapter) GetNodeOccupancy(groups ...string) ([]fleet.OccupancyDetail, error) {
	bins, err := a.client.GetBinDetails(groups...)
	if err != nil {
		return nil, err
	}
	result := make([]fleet.OccupancyDetail, len(bins))
	for i, b := range bins {
		result[i] = fleet.OccupancyDetail{
			ID:       b.ID,
			Occupied: b.Filled,
			Holder:   b.Holder,
			Status:   b.Status,
		}
	}
	return result, nil
}

// --- fleet.VendorProxy ---

func (a *Adapter) BaseURL() string {
	return a.client.BaseURL()
}

// --- fleet.SceneSyncer ---

func (a *Adapter) GetSceneAreas() ([]fleet.SceneArea, error) {
	scene, err := a.client.GetScene()
	if err != nil {
		return nil, err
	}
	areas := make([]fleet.SceneArea, len(scene.Areas))
	for i, rdsArea := range scene.Areas {
		fa := fleet.SceneArea{Name: rdsArea.Name}
		for _, ap := range rdsArea.LogicalMap.AdvancedPoints {
			label := ""
			if p, ok := rds.FindProperty(ap.Property, "label"); ok {
				label = p.StringValue
			}
			propsJSON, _ := json.Marshal(ap.Property)
			fa.AdvancedPoints = append(fa.AdvancedPoints, fleet.ScenePoint{
				ClassName:      ap.ClassName,
				InstanceName:   ap.InstanceName,
				Label:          label,
				Dir:            ap.Dir,
				PosX:           ap.Pos.X,
				PosY:           ap.Pos.Y,
				PosZ:           ap.Pos.Z,
				PropertiesJSON: string(propsJSON),
			})
		}
		for _, blg := range rdsArea.LogicalMap.BinLocationsList {
			for _, bin := range blg.BinLocationList {
				propsJSON, _ := json.Marshal(bin.Property)
				fa.BinLocations = append(fa.BinLocations, fleet.ScenePoint{
					ClassName:      bin.ClassName,
					InstanceName:   bin.InstanceName,
					PointName:      bin.PointName,
					GroupName:      bin.GroupName,
					PosX:           bin.Pos.X,
					PosY:           bin.Pos.Y,
					PosZ:           bin.Pos.Z,
					PropertiesJSON: string(propsJSON),
				})
			}
		}
		areas[i] = fa
	}
	return areas, nil
}

// RDSClient returns the underlying rds.Client for vendor-specific operations
// (simulation, etc.) that don't belong in the fleet interface.
func (a *Adapter) RDSClient() *rds.Client {
	return a.client
}

// --- fleet.VendorCommander ---

func (a *Adapter) ExecuteVendorCommand(cmd fleet.VendorCommand) (*fleet.VendorCommandResult, error) {
	switch cmd.Type {
	// Fire-and-forget commands
	case "pause":
		return a.fireAndForget(a.client.PauseNavigation([]string{cmd.RobotID}))
	case "resume":
		return a.fireAndForget(a.client.ResumeNavigation([]string{cmd.RobotID}))
	case "redo_failed":
		return a.fireAndForget(a.client.RedoFailed(&rds.RedoFailedRequest{Vehicles: []string{cmd.RobotID}}))
	case "manual_finish":
		return a.fireAndForget(a.client.ManualFinish(&rds.ManualFinishRequest{Vehicles: []string{cmd.RobotID}}))
	case "preempt":
		return a.fireAndForget(a.client.PreemptControl([]string{cmd.RobotID}))
	case "release":
		return a.fireAndForget(a.client.ReleaseControl([]string{cmd.RobotID}))
	case "confirm_reloc":
		return a.fireAndForget(a.client.ConfirmRelocalization([]string{cmd.RobotID}))
	case "clear_goods":
		return a.fireAndForget(a.client.ClearAllContainerGoods(cmd.RobotID))
	case "dispatchable":
		dt := cmd.DispatchType
		if dt == "" {
			dt = "dispatchable"
		}
		return a.fireAndForget(a.client.SetDispatchable(&rds.DispatchableRequest{Vehicles: []string{cmd.RobotID}, Type: dt}))
	case "switch_map":
		return a.fireAndForget(a.client.SwitchMap(cmd.RobotID, cmd.MapName))
	case "terminate":
		return a.fireAndForget(a.client.TerminateOrder(&rds.TerminateRequest{ID: cmd.OrderID}))
	case "bind_goods":
		return a.fireAndForget(a.client.BindContainerGoods(&rds.BindGoodsRequest{
			Vehicle: cmd.RobotID, ContainerName: cmd.ContainerName, GoodsID: cmd.GoodsID,
		}))
	case "unbind_goods":
		return a.fireAndForget(a.client.UnbindGoods(cmd.RobotID, cmd.GoodsID))
	case "unbind_container":
		return a.fireAndForget(a.client.UnbindContainerGoods(cmd.RobotID, cmd.ContainerName))

	// Order-creating commands
	case "move", "jack", "unjack", "charge":
		return a.executeOrderCommand(cmd)
	default:
		return nil, fmt.Errorf("unknown command type: %s", cmd.Type)
	}
}

func (a *Adapter) fireAndForget(err error) (*fleet.VendorCommandResult, error) {
	if err != nil {
		return &fleet.VendorCommandResult{State: "FAILED", Detail: err.Error()}, err
	}
	return &fleet.VendorCommandResult{State: "COMPLETED"}, nil
}

func (a *Adapter) executeOrderCommand(cmd fleet.VendorCommand) (*fleet.VendorCommandResult, error) {
	orderID := "tc-" + uuid.New().String()[:8]
	blockID := orderID + "-b1"

	block := rds.Block{BlockID: blockID, Location: cmd.Location}
	if cmd.Type == "jack" || cmd.Type == "unjack" {
		block.PostAction = &rds.PostAction{ConfigID: cmd.ConfigID}
	}

	rdsReq := &rds.SetOrderRequest{
		ID:       orderID,
		Vehicle:  cmd.RobotID,
		Blocks:   []rds.Block{block},
		Complete: true,
	}
	if err := a.client.CreateOrder(rdsReq); err != nil {
		return &fleet.VendorCommandResult{State: "FAILED", Detail: err.Error()}, err
	}
	return &fleet.VendorCommandResult{VendorOrderID: orderID, State: "CREATED"}, nil
}

func (a *Adapter) GetVendorOrderDetail(vendorOrderID string) (*fleet.VendorOrderDetail, error) {
	detail, err := a.client.GetOrderDetails(vendorOrderID)
	if err != nil {
		return nil, err
	}
	return &fleet.VendorOrderDetail{
		State:      string(detail.State),
		IsTerminal: detail.State.IsTerminal(),
		Raw:        detail,
	}, nil
}
