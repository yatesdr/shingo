package fleet

import "time"

// RobotLister provides robot status and control capabilities.
// Web handlers type-assert Backend to this interface.
type RobotLister interface {
	GetRobotsStatus() ([]RobotStatus, error)
	SetAvailability(vehicleID string, available bool) error
	RetryFailed(vehicleID string) error
	ForceComplete(vehicleID string) error
}

// NodeOccupancyProvider provides node location occupancy details.
type NodeOccupancyProvider interface {
	GetNodeOccupancy(groups ...string) ([]OccupancyDetail, error)
}

// SceneSyncer provides access to the fleet's physical scene layout.
type SceneSyncer interface {
	GetSceneAreas() ([]SceneArea, error)
}

// RobotGroup is a vendor-neutral named robot-dispatch group from the scene
// (e.g. a "1500kg" group). A payload's robot_group is picked from this list.
type RobotGroup struct {
	Name string `json:"name"`
	Desc string `json:"desc,omitempty"`
}

// RobotGroupLister exposes the fleet's configured robot-dispatch groups so the
// payload editor can offer them as a picker. Web handlers type-assert Backend
// to this interface; backends without it (e.g. the simulator) degrade to
// free-text entry.
type RobotGroupLister interface {
	GetRobotGroups() ([]RobotGroup, error)
}

// LocationTasks reports the binTask actions configured at one storage location,
// as returned by a fleet backend's location/bin check (SEER /binCheck). TaskNames
// is the set of binTask action names valid at Location — empty when the location
// exists but has none, or when it doesn't exist. Exists/Valid mirror the vendor's
// per-location existence + validity flags so callers can tell "no such location"
// (couldn't verify) apart from "location present but missing a key" (a hard fail).
type LocationTasks struct {
	Location  string
	Exists    bool
	Valid     bool
	TaskNames []string
}

// BinTaskChecker is the OPTIONAL backend capability for querying which binTask
// actions are configured at given storage locations (SEER /binCheck). It backs
// config-time validation of a payload's advanced load sequence: the configured
// task names must exist at every location the payload loads at. Web/engine
// callers type-assert Backend to this interface; backends without it (the
// simulator) degrade to unverified — exactly like RobotGroupLister.
type BinTaskChecker interface {
	CheckLocationTasks(locations []string) ([]LocationTasks, error)
}

// VendorProxy exposes the vendor API base URL for raw proxy requests.
type VendorProxy interface {
	BaseURL() string
}

// VendorCommand represents a raw vendor command for debugging/testing.
type VendorCommand struct {
	Type          string
	RobotID       string
	Location      string
	ConfigID      string
	DispatchType  string
	MapName       string
	OrderID       string
	ContainerName string
	GoodsID       string
}

// VendorCommandResult holds the result of a vendor command.
type VendorCommandResult struct {
	VendorOrderID string // non-empty for order-creating commands
	State         string // COMPLETED, FAILED, CREATED
	Detail        string // error details if failed
}

// VendorOrderDetail holds the detail of a vendor order for status polling.
type VendorOrderDetail struct {
	State      string
	IsTerminal bool
	Raw        any // vendor-specific detail for API consumers
}

// VendorCommander executes raw vendor-specific commands for debugging/testing.
type VendorCommander interface {
	ExecuteVendorCommand(cmd VendorCommand) (*VendorCommandResult, error)
	GetVendorOrderDetail(vendorOrderID string) (*VendorOrderDetail, error)
}

// FireAlarmStatus is a vendor-neutral representation of fire alarm state.
type FireAlarmStatus struct {
	IsFire    bool   `json:"is_fire"`
	ChangedAt string `json:"changed_at"` // ISO timestamp from vendor, empty if never triggered
}

// FireAlarmController provides fire alarm control for supported fleet backends.
// Web handlers type-assert Backend to this interface.
type FireAlarmController interface {
	GetFireAlarmStatus() (*FireAlarmStatus, error)
	SetFireAlarm(on bool, autoResume bool) error
}

// RobotStatus is a vendor-neutral representation of a robot's state.
type RobotStatus struct {
	VehicleID    string
	Connected    bool
	Available    bool
	Busy         bool
	Emergency    bool
	Blocked      bool
	IsError      bool
	BatteryLevel float64
	Charging     bool
	CurrentMap   string
	Model        string
	IP           string
	X            float64
	Y            float64
	Angle        float64
	// Confidence is the vendor's localization confidence, 0.0–1.0.
	// RelocStatus is the localization state machine (0=FAILED, 1=SUCCESS,
	// 2=RELOCING, 3=COMPLETED); Confidence is only meaningful outside
	// RELOCING. See rds.RbkReport for the full enum.
	//
	// A Confidence of exactly -0.0 is the vendor's "no estimate here"
	// sentinel and is explained by AreaIDs, not by the robot being lost.
	Confidence  float64
	RelocStatus int
	// AreaIDs is the robot's current advanced-area membership on its own
	// map (rbk_report.area_ids), nil or empty for most of the plant. It is
	// the only thing that distinguishes "confidence unavailable in this
	// zone" from "this robot is losing localization", so it travels with
	// every sample rather than being resolved later — RDS keeps no history
	// of it and its /scene advancedAreaList is empty.
	AreaIDs []string
	// MapMD5 hashes the robot's own loaded map. It is per robot on purpose:
	// a fleet is not guaranteed to be on one map, and when it is not, every
	// place-keyed statistic computed from these robots is mixing two frames.
	// See rds.RbkReport.CurrentMapMD5.
	MapMD5         string
	NetworkDelay   int
	CurrentStation string
	LastStation    string
	OdoTotal       float64
	OdoToday       float64
	SessionMs      int64
	TotalMs        int64
	LiftCount      int
	LiftHeight     float64
	LiftError      int
	BatteryV       float64
	BatteryA       float64
	// Battery pack telemetry. Nothing in this system has ever retained a
	// battery time-series — RDS has no column for it and its
	// t_batterylevelrecord is empty — so this poll is the only place it can
	// come from. BatteryMaxChargeA/V carry the vendor's -1.0 "unknown"
	// sentinel at some plants and are not always a measurement.
	BatteryTemp       float64
	BatteryCycle      int
	BatteryChargeA    float64
	BatteryChargeV    float64
	BatteryMaxChargeA float64
	BatteryMaxChargeV float64
	BatteryManualConn bool
	CtrlTemp          float64
	CtrlHumi          float64
	CtrlVoltage       float64
	Version           string
	TaskStatus        int
	// Suspended is now read from the fleet rather than asserted. It was
	// hardcoded false while Undispatchable.Suspended sat unread on the wire.
	Suspended      bool
	Undispatchable Undispatchable
	Alarms         []RobotAlarm
}

// Undispatchable is the fleet's own account of why a robot is not taking
// work — vendor-neutral mirror of RDS's undispatchable_reason.
//
// MapInvalid is the field with no other source in the system, and it is not
// hypothetical: Hopkinsville 2026-08-06 had a connected robot held out of
// service with MapInvalid true while the rest of the fleet ran a different
// map. Status is the vendor's own enum rather than a bool because it
// distinguishes "dispatchable" from the several ways of not being so.
type Undispatchable struct {
	MapInvalid       bool
	UnconfirmedReloc bool
	LowBattery       bool
	Disconnect       bool
	Suspended        bool
	Status           int
	Unlock           int
}

// SceneState carries the fleet-wide facts that arrive on the /robotsStatus
// ENVELOPE rather than on any robot in it.
//
// It is read back from a cache rather than fetched, and that is deliberate.
// Core already polls /robotsStatus every 2 seconds; adding a second call for
// data that is already arriving is the exact shape of bug this whole line of
// work exists because of. The adapter captures the envelope on the way past.
type SceneState struct {
	// SceneMD5 hashes the RDS scene. Empty means the fleet backend has not
	// been polled yet — never treat it as "the scene has no hash".
	SceneMD5 string
	// DisabledPaths are lane ids an operator switched off. A disabled lane
	// accumulates no samples, so a map drawn from telemetry alone shows it
	// as never-driven; this is what tells the difference.
	DisabledPaths  []string
	DisabledPoints []string
	// ObservedAt is when the envelope was captured. Zero means never.
	ObservedAt time.Time
}

// SceneStateProvider is implemented by fleet backends that can report the
// scene-level envelope. Optional: a backend without it simply has no scene
// hash and no disabled-lane list, which every consumer must tolerate.
type SceneStateProvider interface {
	GetSceneState() SceneState
}

// RobotAlarm is a vendor-neutral active robot alarm (Q-026). JSON tags match
// domain.PrimaryFailureReason's seerRobotAlarm so a marshaled []RobotAlarm
// feeds the failure classifier directly when snapshotted onto a mission.
type RobotAlarm struct {
	Code     int    `json:"code"`
	Severity string `json:"severity"` // fatal | error | warning | notice
	Desc     string `json:"desc"`
}

// State returns a computed state string for the robot: offline, error, busy, paused, or ready.
func (r RobotStatus) State() string {
	if !r.Connected {
		return "offline"
	}
	if r.Emergency || r.Blocked {
		return "error"
	}
	if r.Busy {
		return "busy"
	}
	if !r.Available {
		return "paused"
	}
	return "ready"
}

// OccupancyDetail is a vendor-neutral representation of a location's occupancy status.
type OccupancyDetail struct {
	ID       string
	Occupied bool
	Holder   int
	Status   int
}

// SceneArea represents a named area in the fleet scene containing points,
// locations, and the drivable path segments connecting them.
type SceneArea struct {
	Name           string
	AdvancedPoints []ScenePoint
	BinLocations   []ScenePoint
	Edges          []SceneEdge
}

// SceneEdge is a vendor-neutral drivable path segment between two scene
// points (SEER "advanced curves"). Endpoints carry both the point instance
// name and raw coordinates so consumers can use the segment even when an
// endpoint wasn't synced as a point.
//
// Ctrl1/Ctrl2 are the segment's cubic-Bezier control handles in the From→To
// direction, nil on a segment the fleet drives straight. A curved segment
// whose handles are dropped is drawn as its chord, which at Springfield puts
// the painted lane up to 1.30 m from the one the robot drives.
type SceneEdge struct {
	ClassName    string
	InstanceName string
	FromName     string
	ToName       string
	FromX        float64
	FromY        float64
	ToX          float64
	ToY          float64
	Ctrl1        *ScenePos
	Ctrl2        *ScenePos
}

// ScenePos is a plane coordinate in scene/world units. Only x and y are
// carried: every control handle in every plant scene has z = 0, and the map
// is a plan view that has nowhere to put a third axis.
type ScenePos struct {
	X float64
	Y float64
}

// ScenePoint is a vendor-neutral point in the fleet scene.
type ScenePoint struct {
	ClassName      string
	InstanceName   string
	PointName      string  // bin locations only
	GroupName      string  // bin locations only
	Label          string  // extracted from vendor properties
	Dir            float64 // advanced points only
	PosX           float64
	PosY           float64
	PosZ           float64
	PropertiesJSON string // raw JSON of vendor properties
}
