package seerrds

import (
	"encoding/json"
	"fmt"
	"log"
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
	FaultGrace   time.Duration
	DebugLog     func(string, ...any)
}

// Adapter wraps an rds.Client to implement fleet.TrackingBackend,
// fleet.RobotLister, fleet.NodeOccupancyProvider, and fleet.VendorProxy.
type Adapter struct {
	client       *rds.Client
	pollInterval time.Duration
	faultGrace   time.Duration
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
		faultGrace:   cfg.FaultGrace,
		debugLog:     cfg.DebugLog,
	}
}

// --- fleet.Backend ---

// CreateOrder is the single fleet create primitive (fleet.Backend). It posts a
// block-based /setOrder: each block carries its Location + BinTask (the only two
// per-block fields SEER acts on), plus a synthetic GoodsID (orderID+"_goods") per
// block — a placeholder that only matters if SEER goods-binding is active (a
// separate manual path, unused in the live order flow; SEER auto-generates one
// if blank). req.Complete selects the lifecycle: true = the fleet completes the
// order once its blocks finish (no-wait, single-shot); false = staged, with
// further blocks appended later via ReleaseOrder.
func (a *Adapter) CreateOrder(req fleet.CreateOrderRequest) (fleet.TransportOrderResult, error) {
	goodsID := req.OrderID + "_goods"
	blocks := make([]rds.Block, len(req.Blocks))
	for i, b := range req.Blocks {
		blocks[i] = rds.Block{
			BlockID:   b.BlockID,
			Location:  b.Location,
			BinTask:   b.BinTask,
			Operation: "",
			GoodsID:   goodsID,
		}
	}

	rdsReq := &rds.SetOrderRequest{
		ID:         req.OrderID,
		ExternalID: req.ExternalID,
		Group:      req.RobotGroup, // SEER robot-dispatch group; "" omitted → vendor default assignment
		KeyRoute:   req.KeyRoute,   // robot-selection hint; nil omitted → SEER auto-picks
		KeyTask:    req.KeyTask,    // robot-selection hint ("load"/"unload"); "" omitted → SEER auto-picks
		Blocks:     blocks,
		Complete:   req.Complete,
		Priority:   req.Priority,
	}
	if err := a.client.CreateOrder(rdsReq); err != nil {
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

// ReleaseOrder appends blocks to a staged RDS order, pinned to the robot that
// is already on it.
//
// ── THE VEHICLE READ IS A REFUSAL POINT, NOT A HINT ───────────────────────
//
// This used to read `GetOrderDetails` and, when the read errored OR reported no
// robot, append anyway with an empty pin. That is the production twin of the
// simulator's never-issued shrug (§R.98 stage A1): a read that did not answer
// was degraded into an answer, and the append went out blind. GetOrderDetails
// is also how RDS says "I have no such order" — it returns an error for an
// empty response body — so the mission-does-not-exist case arrived here too and
// was swallowed with everything else.
//
// Appending BLOCKS is handing work to a specific robot. Two facts have to hold
// and each gets its own refusal: the read has to answer, and the answer has to
// name a robot. Neither one is inferred from the other's silence.
//
// A pure MARK-COMPLETE (no blocks, complete=true — the no-wait complex path
// signalling "nothing further is coming") is a different call wearing the same
// method. It hands nobody any work, so there is nothing to pin and no robot to
// insist on; it fires microseconds after /setOrder, before RDS can plausibly
// have assigned one. It does not read, and it does not refuse.
func (a *Adapter) ReleaseOrder(vendorOrderID string, blocks []fleet.OrderBlock, complete bool) error {
	rdsBlocks := make([]rds.Block, len(blocks))
	for i, b := range blocks {
		rdsBlocks[i] = rds.Block{BlockID: b.BlockID, Location: b.Location, BinTask: b.BinTask}
	}

	if len(rdsBlocks) == 0 {
		return a.client.AddBlocks(vendorOrderID, rdsBlocks, complete, "")
	}

	detail, err := a.client.GetOrderDetails(vendorOrderID)
	if err != nil {
		return fmt.Errorf("release %s: the vehicle read did not answer, refusing to append %d blocks blind: %w",
			vendorOrderID, len(rdsBlocks), err)
	}
	if detail.Vehicle == "" {
		return fmt.Errorf("release %s: RDS holds the order but names no assigned vehicle; refusing to append %d blocks with no robot to pin them to",
			vendorOrderID, len(rdsBlocks))
	}

	// Pin the vehicle that's already assigned to the staged order so RDS
	// doesn't re-dispatch to a different robot when adding post-wait blocks.
	log.Printf("adapter: release pinning vehicle %s for order %s", detail.Vehicle, vendorOrderID)
	return a.client.AddBlocks(vendorOrderID, rdsBlocks, complete, detail.Vehicle)
}

func (a *Adapter) Reconfigure(cfg fleet.ReconfigureParams) {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	a.client.Reconfigure(cfg.BaseURL, timeout)
	// Grace applies to FAILED entries recorded from here on, so pushing it
	// into a live poller is safe and lets the config page take effect
	// without a core restart. Orders already counting down keep the
	// deadline they were stamped with.
	if cfg.FaultGrace > 0 {
		a.faultGrace = cfg.FaultGrace
		if a.poller != nil {
			a.poller.SetGraceDuration(cfg.FaultGrace)
		}
	}
}

// --- fleet.TrackingBackend ---

func (a *Adapter) InitTracker(emitter fleet.TrackerEmitter, resolver fleet.OrderIDResolver) {
	bridge := &emitterBridge{emitter: emitter}
	resolverBridge := &resolverBridge{resolver: resolver}
	a.poller = rds.NewPoller(a.client, bridge, resolverBridge, a.pollInterval, a.faultGrace)
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
				label := ""
				if p, ok := rds.FindProperty(bin.Property, "label"); ok {
					label = p.StringValue
				}
				propsJSON, _ := json.Marshal(bin.Property)
				fa.BinLocations = append(fa.BinLocations, fleet.ScenePoint{
					ClassName:      bin.ClassName,
					InstanceName:   bin.InstanceName,
					Label:          label,
					PointName:      bin.PointName,
					GroupName:      bin.GroupName,
					PosX:           bin.Pos.X,
					PosY:           bin.Pos.Y,
					PosZ:           bin.Pos.Z,
					PropertiesJSON: string(propsJSON),
				})
			}
		}
		// Advanced curves are the drivable path segments between advanced
		// points — the scene's real connectivity. Skip degenerate entries
		// with no endpoint names (nothing to join them to).
		for _, c := range rdsArea.LogicalMap.AdvancedCurves {
			if c.StartPos.InstanceName == "" && c.EndPos.InstanceName == "" {
				continue
			}
			e := fleet.SceneEdge{
				ClassName:    c.ClassName,
				InstanceName: c.InstanceName,
				FromName:     c.StartPos.InstanceName,
				ToName:       c.EndPos.InstanceName,
				FromX:        c.StartPos.Pos.X,
				FromY:        c.StartPos.Pos.Y,
				ToX:          c.EndPos.Pos.X,
				ToY:          c.EndPos.Pos.Y,
			}
			// The shape between the endpoints, when the scene states one.
			// ControlPoints already rejects the all-zero pair SEER writes on
			// straight segments, so a nil here means "the fleet drives this
			// straight", never "we lost it".
			if c1, c2, ok := c.ControlPoints(); ok {
				e.Ctrl1 = &fleet.ScenePos{X: c1.X, Y: c1.Y}
				e.Ctrl2 = &fleet.ScenePos{X: c2.X, Y: c2.Y}
			}
			fa.Edges = append(fa.Edges, e)
		}
		areas[i] = fa
	}
	return areas, nil
}

// --- fleet.RobotGroupLister ---

// GetRobotGroups returns the scene's named robot-dispatch groups (the picker
// source for a payload's robot_group). An RDS error propagates so the caller
// can degrade gracefully rather than show a stale list.
func (a *Adapter) GetRobotGroups() ([]fleet.RobotGroup, error) {
	scene, err := a.client.GetScene()
	if err != nil {
		return nil, err
	}
	groups := make([]fleet.RobotGroup, 0, len(scene.RobotGroups))
	for _, g := range scene.RobotGroups {
		groups = append(groups, fleet.RobotGroup{Name: g.Name, Desc: g.Desc})
	}
	return groups, nil
}

// --- fleet.BinTaskChecker ---

// CheckLocationTasks queries RDS (/binCheck) for the binTask actions configured
// at each given storage location and maps them to vendor-neutral LocationTasks.
// An RDS error propagates so config-time validation can degrade to "unverified"
// (save allowed, warn) rather than falsely reject. Each result's TaskNames is the
// set of binTask keys the location advertises; validation checks the configured
// sequence names against it.
func (a *Adapter) CheckLocationTasks(locations []string) ([]fleet.LocationTasks, error) {
	results, err := a.client.CheckBins(locations)
	if err != nil {
		return nil, err
	}
	out := make([]fleet.LocationTasks, 0, len(results))
	for _, r := range results {
		out = append(out, fleet.LocationTasks{
			Location:  r.ID,
			Exists:    r.Exist,
			Valid:     r.Valid,
			TaskNames: r.Status.TaskNames(), // nil-safe: Status is nil when the bin doesn't exist
		})
	}
	return out, nil
}

// --- fleet.FireAlarmController ---

func (a *Adapter) GetFireAlarmStatus() (*fleet.FireAlarmStatus, error) {
	return a.client.GetFireAlarmStatus()
}

func (a *Adapter) SetFireAlarm(on bool, autoResume bool) error {
	return a.client.SetFireAlarm(on, autoResume)
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
