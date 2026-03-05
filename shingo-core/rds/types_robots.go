package rds

// --- Robot types ---

type RobotsStatusResponse struct {
	Response
	Report []RobotStatus `json:"report,omitempty"`
}

type RobotStatus struct {
	UUID             string         `json:"uuid"`
	VehicleID        string         `json:"vehicle_id"`
	ConnectionStatus int            `json:"connection_status"`
	Dispatchable     bool           `json:"dispatchable"`
	IsError          bool           `json:"is_error"`
	ProcBusiness     bool           `json:"procBusiness"`
	NetworkDelay     int            `json:"network_delay"`
	BasicInfo        RobotBasicInfo `json:"basic_info"`
	RbkReport        RbkReport      `json:"rbk_report"`
	CurrentOrder     any            `json:"current_order"`
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

type VehiclesRequest struct {
	Vehicles []string `json:"vehicles"`
}

type SwitchMapRequest struct {
	Vehicle string `json:"vehicle"`
	Map     string `json:"map"`
}

type ModifyParamsRequest struct {
	Vehicle string                    `json:"vehicle"`
	Body    map[string]map[string]any `json:"body"`
}

type RestoreParamsEntry struct {
	Plugin string   `json:"plugin"`
	Params []string `json:"params"`
}

type RestoreParamsRequest struct {
	Vehicle string               `json:"vehicle"`
	Body    []RestoreParamsEntry `json:"body"`
}
