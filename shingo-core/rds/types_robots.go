package rds

// --- Robot types ---

// RobotsStatusResponse is the /robotsStatus envelope. Report is per robot;
// the fields beside it are FLEET-WIDE and belong to no robot at all, which is
// why they were invisible for as long as they were — every consumer of this
// endpoint reached straight past the envelope for .Report.
type RobotsStatusResponse struct {
	Response
	Report []RobotStatus `json:"report,omitempty"`
	// SceneMD5 hashes the RDS scene — the same scene scenesync mirrors into
	// scene_points/scene_edges. It moves when the scene does, so it is the
	// change gate for that sync, which today has no schedule at all and
	// re-runs only when Core reconnects to the fleet.
	//
	// NOT to be confused with rbk_report.current_map_md5, which hashes the
	// robot's own onboard .smap. They are different artifacts on different
	// transports and either can move without the other.
	SceneMD5 string `json:"scene_md5"`
	// DisablePaths / DisablePoints are the lanes and points an operator has
	// switched off in RoboShop. A disabled lane can never accumulate a
	// sample, so anything drawing "traversed vs not" from telemetry alone
	// renders it as never-driven — true of the data and false of the plant,
	// and false in the reassuring direction. Four lanes are disabled at
	// Springfield as of 2026-08-06 (LM108-LM40, LM40-LM108, LM11-LM40,
	// LM40-LM11); Hopkinsville has none.
	DisablePaths  []DisabledID `json:"disable_paths"`
	DisablePoints []DisabledID `json:"disable_points"`
}

// DisabledID is the vendor's one-key wrapper around a disabled lane or point
// id. It is an object rather than a bare string on the wire; unwrapping it
// here keeps that shape out of every consumer.
type DisabledID struct {
	ID string `json:"id"`
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
	// UndispatchableReason is RDS's own account of why a robot is not taking
	// work. mapRobotStatus used to hardcode Suspended: false behind a
	// "Phase 2" comment while this struct sat on the wire unread — a
	// constant standing in for a measurement, which is worse than a dropped
	// field: a drop looks like never-having-had-it, a constant looks like an
	// answer.
	UndispatchableReason UndispatchableReason `json:"undispatchable_reason"`
}

// UndispatchableReason is the seven-field struct RDS publishes per robot.
//
// CurrentMapInvalid is the one with no other source. Measured at
// Hopkinsville 2026-08-06: eleven robots on map Hop_20 and AMR-11 on Hop_21
// with CurrentMapInvalid true and DispatchableStatus 2 — a connected robot
// held out of service because of its map, on a fleet split across two maps,
// and nothing in Core could see any part of it.
type UndispatchableReason struct {
	CurrentMapInvalid  bool `json:"current_map_invalid"`
	UnconfirmedReloc   bool `json:"unconfirmed_reloc"`
	LowBattery         bool `json:"low_battery"`
	Disconnect         bool `json:"disconnect"`
	Suspended          bool `json:"suspended"`
	DispatchableStatus int  `json:"dispatchable_status"`
	Unlock             int  `json:"unlock"`
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
	//
	// SEER publishes literal -0.0 here as a SENTINEL, not a measurement:
	// measured at Springfield 2026-08-06, every tick a robot spends inside
	// map area 8 reads -0.0 and the first tick outside it returns to ~0.83,
	// at the same speed and with reloc_status unchanged at 1. It means "no
	// reflector-based estimate available here", and the whole fleet stands
	// on alarm 54018 ("reflectors in map not enough") naming that area. It
	// is NOT a confidence of zero and must never be averaged as one — see
	// AreaIDs below, which is what makes the distinction recoverable.
	Confidence float64 `json:"confidence"`
	// AreaIDs is the robot's CURRENT advanced-area membership from its own
	// onboard map — the ids it is standing inside right now, usually empty.
	//
	// This is NOT BasicInfo.CurrentArea. They are different namespaces and
	// they do not agree: CurrentArea is the RDS *scene* area ("Area-01",
	// matching scene_edges.area_name, one value across all of Springfield),
	// while this is the robot map's advanced-area id ("8"). Only this one
	// explains the confidence sentinel, and only this one is unrecoverable
	// — RDS's /scene returns advancedAreaList: [] and keeps no history, so
	// a tick not stored here is gone.
	AreaIDs        []string `json:"area_ids"`
	BatteryLevel   float64  `json:"battery_level"`
	Charging       bool     `json:"charging"`
	CurrentStation string   `json:"current_station"`
	LastStation    string   `json:"last_station"`
	TaskStatus     int      `json:"task_status"`
	Blocked        bool     `json:"blocked"`
	Emergency      bool     `json:"emergency"`
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
	// CurrentMapMD5 hashes the robot's OWN onboard .smap — the artifact that
	// holds the reflector positions and the ReflectorArea polygons, none of
	// which RDS exposes (its /scene returns advancedAreaList: []).
	//
	// It arrives on a poll Core already makes 43,200 times a day and was
	// discarded at unmarshal, which cost this project two things. It is the
	// change gate for pulling the map at all, so the expensive 7.3 MB fetch
	// can fire when the map moves instead of on a calendar. And because it
	// is PER ROBOT it is the only way to see a fleet that is not all on one
	// map: measured at Hopkinsville 2026-08-06, eleven robots on Hop_20 and
	// one on Hop_21. A sample from a robot on a different map is being
	// snapped to a scene it is not localizing against, so this is a
	// correctness input to the roll-up and not only an observability one.
	//
	// Do NOT use the top-level model_md5 for this. That hashes the robot
	// MODEL definition and is byte-identical at both plants, which is the
	// proof it is not about the map.
	CurrentMapMD5 string `json:"current_map_md5"`
	// CurrentMap is the loaded map's name. Measured identical to
	// basic_info.current_map on all 24 robots across both plants
	// 2026-08-06; it lives here too so a name and its hash can be read from
	// one object rather than paired across two.
	CurrentMap string `json:"current_map"`
	// --- battery and thermal ---
	//
	// The whole block is published every 2 s and was discarded at unmarshal,
	// which is the confidence bug repeating: shingo keeps NO battery
	// time-series, RDS's t_robotstatusrecord has no battery column and its
	// t_batterylevelrecord is empty, so this poll is the only source of
	// battery history the system could ever have. Two AMR incidents across
	// both plants have already been diagnosed without it.
	//
	// basic_info.controller_temp is already carried, so today the controller
	// is observable and the battery is not, which is the wrong way round.
	BatteryTemperature float64 `json:"battery_temperature"`
	BatteryCycle       int     `json:"battery_cycle"`
	BatteryChargeCurr  float64 `json:"battery_charge_current"`
	BatteryChargeVolt  float64 `json:"battery_charge_voltage"`
	// MaxCharge* are the pack's rated ceilings, and they are NOT always a
	// measurement: Springfield reports 19.68 A / 54.8 V while Hopkinsville
	// reports -1.0 for both, which is the vendor's "unknown" sentinel and
	// not a negative current. Anything deriving a percentage-of-rated figure
	// has to test for it — a ratio against -1 is silently backwards.
	BatteryMaxChargeCurr float64 `json:"battery_max_charge_current"`
	BatteryMaxChargeVolt float64 `json:"battery_max_charge_voltage"`
	BatteryManualConn    bool    `json:"battery_is_manually_connected"`
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
