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

// SceneArea represents a named area in the fleet scene containing points and locations.
type SceneArea struct {
	Name           string
	AdvancedPoints []ScenePoint
	BinLocations   []ScenePoint
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
