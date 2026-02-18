package rds

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

type OrderDetailsResponse struct {
	Response
	Data *OrderDetail `json:"data,omitempty"`
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

type SceneResponse struct {
	Response
	Data *Scene `json:"data,omitempty"`
}

type Area struct {
	Name   string   `json:"name"`
	Points []string `json:"points,omitempty"`
}

type Scene struct {
	Areas       []Area        `json:"areas"`
	BlockGroups []any         `json:"blockGroup"`
	Doors       []any         `json:"doors"`
	Labels      []any         `json:"labels"`
	Lifts       []any         `json:"lifts"`
	RobotGroups []RobotGroup  `json:"robotGroup"`
}

type RobotGroup struct {
	Name   string       `json:"name"`
	Robots []RobotEntry `json:"robot"`
}

type RobotEntry struct {
	IP         string `json:"ip"`
	CurrentMap string `json:"current_map"`
	Color      string `json:"color"`
}

type PingResponse struct {
	Product string `json:"product"`
	Version string `json:"version"`
}
