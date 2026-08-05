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
	CtrlTemp     float64  `json:"controller_temp"`
	CtrlHumi     float64  `json:"controller_humi"`
	CtrlVoltage  float64  `json:"controller_voltage"`
}

type RbkReport struct {
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Angle float64 `json:"angle"`
	// Vx/Vy/W are the chassis velocity triple (m/s, m/s, rad/s), verified on
	// the wire at Hopkinsville 2026-08-05. A parked robot reports small
	// non-zero noise (~1e-4), so these are a corroborating motion signal, not
	// a threshold: position delta between polls is the primary test.
	Vx float64 `json:"vx"`
	Vy float64 `json:"vy"`
	W  float64 `json:"w"`
	// Confidence is the robot's own localization confidence, 0.0–1.0. The
	// vendor's operator UI bands it >0.8 green, >0.3 yellow, else red
	// (rds-user-manual.pdf); the HMI tile reuses those exact cuts.
	Confidence     float64 `json:"confidence"`
	BatteryLevel   float64 `json:"battery_level"`
	Charging       bool    `json:"charging"`
	CurrentStation string  `json:"current_station"`
	LastStation    string  `json:"last_station"`
	TaskStatus     int     `json:"task_status"`
	Blocked        bool    `json:"blocked"`
	Emergency      bool    `json:"emergency"`
	// RelocStatus is the localization state machine, documented twice in
	// "Robokit API Protocol" (API 1021 and the rbk_report field table):
	//
	//	0 = FAILED    localization failed — the robot knows it is lost
	//	1 = SUCCESS   localization correct, operator-confirmed
	//	2 = RELOCING  actively relocating; the pose estimate is in flight
	//	3 = COMPLETED relocation finished but not yet operator-confirmed
	//
	// Only 2 makes Confidence meaningless. 0 and 3 are settled states that
	// mean different things and are both worth recording — see
	// store/robotconfidence for how they are kept but held out of statistics.
	RelocStatus         int         `json:"reloc_status"`
	Containers          []Container `json:"containers"`
	AvailableContainers int         `json:"available_containers"`
	TotalContainers     int         `json:"total_containers"`
	Odo                 float64     `json:"odo"`
	TodayOdo            float64     `json:"today_odo"`
	Time                int64       `json:"time"`
	TotalTime           int64       `json:"total_time"`
	Jack                JackReport  `json:"jack"`
	Voltage             float64     `json:"voltage"`
	Current             float64     `json:"current"`
	Alarms              RbkAlarms   `json:"alarms"`
}

// RbkAlarms is the robot's active-alarm snapshot (SEER rbk_report.alarms,
// verified live 2026-06-07). Severity is the array an alarm sits in:
// fatals > errors > warnings > notices. The flat top-level rbk_report.errors/
// warnings/… arrays carry a duplicate dynamic "<code>":<ts> key and are
// ignored in favor of this clean nested shape.
type RbkAlarms struct {
	Fatals   []RbkAlarm `json:"fatals"`
	Errors   []RbkAlarm `json:"errors"`
	Warnings []RbkAlarm `json:"warnings"`
	Notices  []RbkAlarm `json:"notices"`
}

// RbkAlarm is one SEER alarm: a 5xxxx robot code with a human desc. times is
// the repeat count; timestamp is unix seconds.
type RbkAlarm struct {
	Code      int    `json:"code"`
	Desc      string `json:"desc"`
	Times     int    `json:"times"`
	Timestamp int64  `json:"timestamp"`
	DateTime  string `json:"dateTime"`
}
type JackReport struct {
	JackLoadTimes int     `json:"jack_load_times"`
	JackHeight    float64 `json:"jack_height"`
	JackErrorCode int     `json:"jack_error_code"`
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
