package rds

import "encoding/json"

// Response is the common RDS API response envelope.
type Response struct {
	Code     int    `json:"code"`
	Msg      string `json:"msg"`
	CreateOn string `json:"create_on"`
}

// OrderState represents RDS order lifecycle states.
type OrderState string

const (
	StateCreated        OrderState = "CREATED"
	StateToBeDispatched OrderState = "TOBEDISPATCHED"
	StateRunning        OrderState = "RUNNING"
	StateFinished       OrderState = "FINISHED"
	StateFailed         OrderState = "FAILED"
	StateStopped        OrderState = "STOPPED"
	StateWaiting        OrderState = "WAITING"
)

func (s OrderState) IsTerminal() bool {
	return s == StateFinished || s == StateFailed || s == StateStopped
}

// --- Order requests ---

type SetJoinOrderRequest struct {
	ID               string      `json:"id"`
	ExternalID       string      `json:"externalId,omitempty"`
	FromLoc          string      `json:"fromLoc"`
	ToLoc            string      `json:"toLoc"`
	Vehicle          string      `json:"vehicle,omitempty"`
	Group            string      `json:"group,omitempty"`
	GoodsID          string      `json:"goodsId,omitempty"`
	Priority         int         `json:"priority,omitempty"`
	LoadPostAction   *PostAction `json:"loadPostAction,omitempty"`
	UnloadPostAction *PostAction `json:"unloadPostAction,omitempty"`
}

type SetOrderRequest struct {
	ID         string   `json:"id"`
	ExternalID string   `json:"externalId,omitempty"`
	Vehicle    string   `json:"vehicle,omitempty"`
	Group      string   `json:"group,omitempty"`
	Label      string   `json:"label,omitempty"`
	KeyRoute   []string `json:"keyRoute,omitempty"`
	KeyTask    string   `json:"keyTask,omitempty"`
	Priority   int      `json:"priority,omitempty"`
	Blocks     []Block  `json:"blocks"`
	Complete   bool     `json:"complete"`
}

type Block struct {
	BlockID       string            `json:"blockId"`
	Location      string            `json:"location"`
	Operation     string            `json:"operation,omitempty"`
	OperationArgs map[string]any    `json:"operation_args,omitempty"`
	BinTask       string            `json:"binTask,omitempty"`
	GoodsID       string            `json:"goodsId,omitempty"`
	ScriptName    string            `json:"scriptName,omitempty"`
	ScriptArgs    map[string]any    `json:"scriptArgs,omitempty"`
	PostAction    *PostAction       `json:"postAction,omitempty"`
}

type PostAction struct {
	ConfigID string `json:"configId,omitempty"`
}

type TerminateRequest struct {
	ID             string   `json:"id,omitempty"`
	IDList         []string `json:"idList,omitempty"`
	Vehicles       []string `json:"vehicles,omitempty"`
	DisableVehicle bool     `json:"disableVehicle"`
	ClearAll       bool     `json:"clearAll,omitempty"`
}

type SetPriorityRequest struct {
	ID       string `json:"id"`
	Priority int    `json:"priority"`
}

// --- Order responses ---

// OrderDetailsResponse handles the Seer RDS response for order detail endpoints.
// RDS returns order fields at the top level (not nested under "data"), so we
// unmarshal the entire JSON into both the Response base and the OrderDetail.
type OrderDetailsResponse struct {
	Response
	Detail OrderDetail
}

func (r *OrderDetailsResponse) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &r.Response); err != nil {
		return err
	}
	return json.Unmarshal(data, &r.Detail)
}

type OrderDetail struct {
	ID            string       `json:"id"`
	ExternalID    string       `json:"externalId"`
	Vehicle       string       `json:"vehicle"`
	Group         string       `json:"group"`
	State         OrderState   `json:"state"`
	Complete      bool         `json:"complete"`
	Priority      int          `json:"priority"`
	CreateTime    int64        `json:"createTime"`
	TerminalTime  int64        `json:"terminalTime"`
	Blocks        []BlockDetail `json:"blocks"`
	Errors        []string     `json:"errors"`
	Warnings      []string     `json:"warnings"`
	Notices       []string     `json:"notices"`
	// Join order fields
	FromLoc       string       `json:"fromLoc,omitempty"`
	ToLoc         string       `json:"toLoc,omitempty"`
	GoodsID       string       `json:"goodsId,omitempty"`
	ContainerName string       `json:"containerName,omitempty"`
	LoadOrderID   string       `json:"loadOrderId,omitempty"`
	LoadState     OrderState   `json:"loadState,omitempty"`
	UnloadOrderID string       `json:"unloadOrderId,omitempty"`
	UnloadState   OrderState   `json:"unloadState,omitempty"`
}

// BlockDetailsResponse handles the Seer RDS response for block detail endpoints.
type BlockDetailsResponse struct {
	Response
	Detail BlockDetail
}

func (r *BlockDetailsResponse) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &r.Response); err != nil {
		return err
	}
	return json.Unmarshal(data, &r.Detail)
}

type BlockDetail struct {
	BlockID       string     `json:"blockId"`
	Location      string     `json:"location"`
	State         OrderState `json:"state"`
	ContainerName string     `json:"containerName"`
	GoodsID       string     `json:"goodsId"`
	Operation     string     `json:"operation"`
	BinTask       string     `json:"binTask"`
	OperationArgs string     `json:"operation_args"`
	ScriptArgs    string     `json:"script_args"`
	ScriptName    string     `json:"script_name"`
}

type OrderListResponse struct {
	Response
	Data []OrderDetail `json:"data,omitempty"`
}

// --- Robot types ---

type RobotsStatusResponse struct {
	Response
	Report []RobotStatus `json:"report,omitempty"`
}

type RobotStatus struct {
	UUID             string           `json:"uuid"`
	VehicleID        string           `json:"vehicle_id"`
	ConnectionStatus int              `json:"connection_status"`
	Dispatchable     bool             `json:"dispatchable"`
	IsError          bool             `json:"is_error"`
	ProcBusiness     bool             `json:"procBusiness"`
	NetworkDelay     int              `json:"network_delay"`
	BasicInfo        RobotBasicInfo   `json:"basic_info"`
	RbkReport        RbkReport        `json:"rbk_report"`
	CurrentOrder     any              `json:"current_order"`
}

type RobotBasicInfo struct {
	IP           string   `json:"ip"`
	Model        string   `json:"model"`
	Version      string   `json:"version"`
	CurrentArea  []string `json:"current_area"`
	CurrentGroup string   `json:"current_group"`
	CurrentMap   string   `json:"current_map"`
}

type RbkReport struct {
	X                   float64     `json:"x"`
	Y                   float64     `json:"y"`
	Angle               float64     `json:"angle"`
	BatteryLevel        float64     `json:"battery_level"`
	Charging            bool        `json:"charging"`
	CurrentStation      string      `json:"current_station"`
	LastStation         string      `json:"last_station"`
	TaskStatus          int         `json:"task_status"`
	Blocked             bool        `json:"blocked"`
	Emergency           bool        `json:"emergency"`
	RelocStatus         int         `json:"reloc_status"`
	Containers          []Container `json:"containers"`
	AvailableContainers int         `json:"available_containers"`
	TotalContainers     int         `json:"total_containers"`
}

type Container struct {
	ContainerName string `json:"container_name"`
	GoodsID       string `json:"goods_id"`
	HasGoods      bool   `json:"has_goods"`
	Desc          string `json:"desc"`
}

type DispatchableRequest struct {
	Vehicles []string `json:"vehicles"`
	Type     string   `json:"type"` // "dispatchable", "undispatchable_unignore", "undispatchable_ignore"
}

type RedoFailedRequest struct {
	Vehicles []string `json:"vehicles"`
}

type ManualFinishRequest struct {
	Vehicles []string `json:"vehicles"`
}

// --- Bin/location types ---

type BinDetailsResponse struct {
	Response
	Data []BinDetail `json:"data,omitempty"`
}

type BinDetail struct {
	ID     string `json:"id"`
	Filled bool   `json:"filled"`
	Holder int    `json:"holder"`
	Status int    `json:"status"`
}

type BinCheckRequest struct {
	Bins []string `json:"bins"`
}

type BinCheckResponse struct {
	Response
	Bins []BinCheckResult `json:"bins,omitempty"`
}

type BinCheckResult struct {
	ID     string         `json:"id"`
	Exist  bool           `json:"exist"`
	Valid  bool           `json:"valid"`
	Status *BinPointStatus `json:"status,omitempty"`
}

type BinPointStatus struct {
	PointName string `json:"point_name"`
}

// --- Scene types (full-fidelity, matches real /scene API) ---

type SceneResponse struct {
	Response
	Scene *Scene `json:"scene,omitempty"`
}

type Pos3D struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

type SceneProperty struct {
	Key         string  `json:"key"`
	Type        string  `json:"type"`
	StringValue string  `json:"stringValue,omitempty"`
	BoolValue   *bool   `json:"boolValue,omitempty"`
	Int32Value  *int    `json:"int32Value,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	Tag         string  `json:"tag,omitempty"`
	Value       string  `json:"value,omitempty"`
}

// FindProperty searches a property slice for a key.
func FindProperty(props []SceneProperty, key string) (SceneProperty, bool) {
	for _, p := range props {
		if p.Key == key {
			return p, true
		}
	}
	return SceneProperty{}, false
}

type AdvancedPoint struct {
	ClassName    string          `json:"className"`
	InstanceName string          `json:"instanceName"`
	Desc         string          `json:"desc,omitempty"`
	Dir          float64         `json:"dir"`
	IgnoreDir    bool            `json:"ignoreDir,omitempty"`
	Pos          Pos3D           `json:"pos"`
	Property     []SceneProperty `json:"property,omitempty"`
}

type BinLocation struct {
	ClassName    string          `json:"className"`
	InstanceName string          `json:"instanceName"`
	Desc         string          `json:"desc,omitempty"`
	PointName    string          `json:"pointName"`
	GroupName    string          `json:"groupName,omitempty"`
	Pos          Pos3D           `json:"pos"`
	Property     []SceneProperty `json:"property,omitempty"`
}

type BinLocationGroup struct {
	BinLocationList []BinLocation `json:"binLocationList"`
}

type SceneMap struct {
	MapName string `json:"mapName"`
	MD5     string `json:"md5"`
	RobotID string `json:"robotId"`
}

type LogicalMap struct {
	AdvancedPoints   []AdvancedPoint    `json:"advancedPoints,omitempty"`
	BinLocationsList []BinLocationGroup `json:"binLocationsList,omitempty"`
	AdvancedCurves   []any              `json:"advancedCurves,omitempty"`
	AdvancedBlocks   []any              `json:"advancedBlocks,omitempty"`
	AdvancedAreaList []any              `json:"advancedAreaList,omitempty"`
}

type Area struct {
	Name       string     `json:"name"`
	LogicalMap LogicalMap `json:"logicalMap"`
	Maps       []SceneMap `json:"maps,omitempty"`
	Pos        *Pos3D     `json:"pos,omitempty"`
}

type SceneRobotEntry struct {
	ID       string          `json:"id"`
	Property []SceneProperty `json:"property,omitempty"`
}

type RobotGroup struct {
	Name     string            `json:"name"`
	Desc     string            `json:"desc,omitempty"`
	Robot    []SceneRobotEntry `json:"robot,omitempty"`
	Property []SceneProperty   `json:"property,omitempty"`
}

type Scene struct {
	Areas       []Area       `json:"areas,omitempty"`
	RobotGroups []RobotGroup `json:"robotGroup,omitempty"`
	BlockGroups []any        `json:"blockGroup,omitempty"`
	Doors       []any        `json:"doors,omitempty"`
	Labels      []any        `json:"labels,omitempty"`
	Lifts       []any        `json:"lifts,omitempty"`
	BinAreas    []any        `json:"binAreas,omitempty"`
	BinMonitors []any        `json:"binMonitors,omitempty"`
	Terminals   []any        `json:"terminals,omitempty"`
	Desc        string       `json:"desc,omitempty"`
}

type PingResponse struct {
	Product string `json:"product"`
	Version string `json:"version"`
}

// --- Additional order types ---

type MarkCompleteRequest struct {
	ID string `json:"id"`
}

type SetLabelRequest struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type AddBlocksRequest struct {
	ID       string  `json:"id"`
	Blocks   []Block `json:"blocks"`
	Complete bool    `json:"complete"`
}

// --- Additional robot types ---

type VehiclesRequest struct {
	Vehicles []string `json:"vehicles"`
}

type SwitchMapRequest struct {
	Vehicle string `json:"vehicle"`
	Map     string `json:"map"`
}

type ModifyParamsRequest struct {
	Vehicle string                       `json:"vehicle"`
	Body    map[string]map[string]any    `json:"body"`
}

type RestoreParamsEntry struct {
	Plugin string   `json:"plugin"`
	Params []string `json:"params"`
}

type RestoreParamsRequest struct {
	Vehicle string               `json:"vehicle"`
	Body    []RestoreParamsEntry `json:"body"`
}

// --- Simulation types ---

type SimStateTemplateResponse struct {
	Response
	Data json.RawMessage `json:"data,omitempty"`
}

type UpdateSimStateResponse struct {
	Response
	Data json.RawMessage `json:"data,omitempty"`
}

// --- Container types ---

type BindGoodsRequest struct {
	Vehicle       string `json:"vehicle"`
	ContainerName string `json:"containerName"`
	GoodsID       string `json:"goodsId"`
}

type UnbindGoodsRequest struct {
	Vehicle string `json:"vehicle"`
	GoodsID string `json:"goodsId"`
}

type UnbindContainerRequest struct {
	Vehicle       string `json:"vehicle"`
	ContainerName string `json:"containerName"`
}

type ClearAllGoodsRequest struct {
	Vehicle string `json:"vehicle"`
}

// --- Mutex types ---

type MutexGroupRequest struct {
	ID         string   `json:"id"`
	BlockGroup []string `json:"blockGroup"`
}

type MutexGroupResult struct {
	Name       string `json:"name"`
	IsOccupied bool   `json:"isOccupied"`
	Occupier   string `json:"occupier"`
}

type MutexGroupStatus struct {
	Name       string `json:"name"`
	IsOccupied bool   `json:"isOccupied"`
	Occupier   string `json:"occupier"`
}

// --- Device types ---

type CallTerminalRequest struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type CallTerminalResponse struct {
	Response
	Data json.RawMessage `json:"data,omitempty"`
}

type DevicesResponse struct {
	Response
	Doors     []DoorStatus     `json:"doors,omitempty"`
	Lifts     []LiftStatus     `json:"lifts,omitempty"`
	Terminals []TerminalStatus `json:"terminals,omitempty"`
}

type DoorStatus struct {
	Name     string              `json:"name"`
	State    int                 `json:"state"`
	Disabled bool                `json:"disabled"`
	Reasons  []UnavailableReason `json:"reasons,omitempty"`
}

type LiftStatus struct {
	Name     string              `json:"name"`
	State    int                 `json:"state"`
	Disabled bool                `json:"disabled"`
	Reasons  []UnavailableReason `json:"reasons,omitempty"`
}

type UnavailableReason struct {
	Reason string `json:"reason"`
}

type TerminalStatus struct {
	ID    string `json:"id"`
	State int    `json:"state"`
}

type CallDoorRequest struct {
	Name  string `json:"name"`
	State int    `json:"state"`
}

type DisableDeviceRequest struct {
	Names    []string `json:"names"`
	Disabled bool     `json:"disabled"`
}

type CallLiftRequest struct {
	Name       string `json:"name"`
	TargetArea string `json:"target_area"`
}

// --- System types ---

type GetProfilesRequest struct {
	File string `json:"file"`
}

type LicenseResponse struct {
	Response
	Data *LicenseInfo `json:"data,omitempty"`
}

type LicenseInfo struct {
	MaxRobots int              `json:"maxRobots"`
	Expiry    string           `json:"expiry"`
	Features  []LicenseFeature `json:"features,omitempty"`
}

type LicenseFeature struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}
