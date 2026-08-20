// wiring_telemetry.go — Mission telemetry recording.
//
// recordMissionEvent captures each vendor status transition with a
// robot position snapshot for per-state timing and diagnostic history.
// finalizeMissionTelemetry writes the mission summary row on terminal
// states, computing durations and persisting the final block/error/
// warning/notice JSON payloads.

package engine

import (
	"encoding/json"
	"time"

	"shingo/protocol/clock"
	"shingocore/store/telemetry"
)

// recordMissionEvent captures a state transition with robot position snapshot for telemetry.
func (e *Engine) recordMissionEvent(ev OrderStatusChangedEvent) {
	// Same fallback as finalizeMissionTelemetry below, for the same reason:
	// terminal and sim transitions routinely carry no robot on the EVENT, so
	// taking it from the event alone left mission_events.robot_id blank — and
	// robot_x/y/angle/battery/station NULL, because the position snapshot is
	// gated on the same id. The fix landed in one of the two places; this is
	// the other one. No reviewer across three rounds caught it.
	robotID := ev.RobotID
	if robotID == "" {
		if order, err := e.db.GetOrder(ev.OrderID); err == nil && order != nil {
			robotID = order.RobotID
		}
	}

	me := &telemetry.Event{
		OrderID:       ev.OrderID,
		VendorOrderID: ev.VendorOrderID,
		OldState:      ev.OldStatus,
		NewState:      ev.NewStatus,
		RobotID:       robotID,
		Detail:        ev.Detail,
		BlocksJSON:    "[]",
		ErrorsJSON:    "[]",
	}

	// Snapshot robot position from cache
	if robotID != "" {
		if rs, ok := e.GetCachedRobotStatus(robotID); ok {
			me.RobotX = &rs.X
			me.RobotY = &rs.Y
			me.RobotAngle = &rs.Angle
			me.RobotBattery = &rs.BatteryLevel
			me.RobotStation = rs.CurrentStation
		}
	}

	// Capture block states and errors from vendor snapshot
	if ev.Snapshot != nil {
		if len(ev.Snapshot.Blocks) > 0 {
			if data, err := json.Marshal(ev.Snapshot.Blocks); err == nil {
				me.BlocksJSON = string(data)
			}
		}
		if len(ev.Snapshot.Errors) > 0 {
			if data, err := json.Marshal(ev.Snapshot.Errors); err == nil {
				me.ErrorsJSON = string(data)
			}
		}
	}

	if err := e.db.InsertMissionEvent(me); err != nil {
		e.logFn("engine: record mission event: %v", err)
	}

	// On terminal state, write the mission summary
	if e.fleet.IsTerminalState(ev.NewStatus) {
		e.finalizeMissionTelemetry(ev)
	}
}

// finalizeMissionTelemetry writes the summary row when a mission reaches a terminal state.
func (e *Engine) finalizeMissionTelemetry(ev OrderStatusChangedEvent) {
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil {
		e.logFn("engine: finalize telemetry: get order %d: %v", ev.OrderID, err)
		return
	}

	// The robot comes from the ORDER, not the event — every other field here
	// already does. Terminal transitions routinely carry no robot on the
	// event: the simulator's completion path passes "" outright
	// (fleet/simulator/transitions.go:45) and the RDS poller can only pass
	// detail.Vehicle if the vendor happened to include it on the terminal
	// poll. The result was a blank robot_id on every summary row, which
	// silently disabled three things — the per-robot breakdown returned zero
	// rows, the robot filter had nothing to filter, and the robot-alarm
	// snapshot below is gated on this same id, so the failure Pareto never
	// had an alarm to classify a hardware fault from.
	robotID := order.RobotID
	if robotID == "" {
		robotID = ev.RobotID
	}

	now := clock.Now().UTC()
	mt := &telemetry.Mission{
		OrderID:         ev.OrderID,
		VendorOrderID:   ev.VendorOrderID,
		RobotID:         robotID,
		StationID:       order.StationID,
		OrderType:       order.OrderType,
		SourceNode:      order.SourceNode,
		DeliveryNode:    order.DeliveryNode,
		TerminalState:   ev.NewStatus,
		CoreCreated:     &order.CreatedAt,
		CoreCompleted:   &now,
		DurationMS:      now.Sub(order.CreatedAt).Milliseconds(),
		BlocksJSON:      "[]",
		ErrorsJSON:      "[]",
		WarningsJSON:    "[]",
		NoticesJSON:     "[]",
		RobotAlarmsJSON: "[]",
	}

	// Snapshot the robot's active alarms at terminal time (Q-026). For a FAILED
	// mission the causal fault (blocked / battery / hardware) is still active on
	// the robot, so this is the signal the failure Pareto classifies first. The
	// cache is ≤2s stale (the robot poll cadence), current enough at failure.
	if robotID != "" {
		if rs, ok := e.GetCachedRobotStatus(robotID); ok && len(rs.Alarms) > 0 {
			if data, err := json.Marshal(rs.Alarms); err == nil {
				mt.RobotAlarmsJSON = string(data)
			}
		}
	}

	if ev.Snapshot != nil {
		if ev.Snapshot.CreateTime > 0 {
			t := time.UnixMilli(ev.Snapshot.CreateTime)
			mt.VendorCreated = &t
		}
		if ev.Snapshot.TerminalTime > 0 {
			t := time.UnixMilli(ev.Snapshot.TerminalTime)
			mt.VendorCompleted = &t
		}
		if mt.VendorCreated != nil && mt.VendorCompleted != nil {
			mt.VendorDurationMS = mt.VendorCompleted.Sub(*mt.VendorCreated).Milliseconds()
		}
		if data, err := json.Marshal(ev.Snapshot.Blocks); err == nil {
			mt.BlocksJSON = string(data)
		}
		if data, err := json.Marshal(ev.Snapshot.Errors); err == nil {
			mt.ErrorsJSON = string(data)
		}
		if data, err := json.Marshal(ev.Snapshot.Warnings); err == nil {
			mt.WarningsJSON = string(data)
		}
		if data, err := json.Marshal(ev.Snapshot.Notices); err == nil {
			mt.NoticesJSON = string(data)
		}
	}

	if err := e.db.UpsertMissionTelemetry(mt); err != nil {
		e.logFn("engine: finalize telemetry: %v", err)
	}
}
