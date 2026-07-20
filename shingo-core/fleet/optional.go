package fleet

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
	VehicleID      string
	Connected      bool
	Available      bool
	Busy           bool
	Emergency      bool
	Blocked        bool
	IsError        bool
	BatteryLevel   float64
	Charging       bool
	CurrentMap     string
	Model          string
	IP             string
	X              float64
	Y              float64
	Angle          float64
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
	CtrlTemp       float64
	CtrlHumi       float64
	CtrlVoltage    float64
	Version        string
	TaskStatus     int
	Suspended      bool
	Alarms         []RobotAlarm
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
type SceneEdge struct {
	ClassName    string
	InstanceName string
	FromName     string
	ToName       string
	FromX        float64
	FromY        float64
	ToX          float64
	ToY          float64
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
