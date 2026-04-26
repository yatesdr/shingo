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

	"shingocore/store/telemetry"
)

// recordMissionEvent captures a state transition with robot position snapshot for telemetry.
func (e *Engine) recordMissionEvent(ev OrderStatusChangedEvent) {
	me := &telemetry.Event{
		OrderID:       ev.OrderID,
		VendorOrderID: ev.VendorOrderID,
		OldState:      ev.OldStatus,
		NewState:      ev.NewStatus,
		RobotID:       ev.RobotID,
		Detail:        ev.Detail,
		BlocksJSON:    "[]",
		ErrorsJSON:    "[]",
	}

	// Snapshot robot position from cache
	if ev.RobotID != "" {
		if rs, ok := e.GetCachedRobotStatus(ev.RobotID); ok {
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

	now := time.Now().UTC()
	mt := &telemetry.Mission{
		OrderID:       ev.OrderID,
		VendorOrderID: ev.VendorOrderID,
		RobotID:       ev.RobotID,
		StationID:     order.StationID,
		OrderType:     order.OrderType,
		SourceNode:    order.SourceNode,
		DeliveryNode:  order.DeliveryNode,
		TerminalState: ev.NewStatus,
		CoreCreated:   &order.CreatedAt,
		CoreCompleted: &now,
		DurationMS:    now.Sub(order.CreatedAt).Milliseconds(),
		BlocksJSON:    "[]",
		ErrorsJSON:    "[]",
		WarningsJSON:  "[]",
		NoticesJSON:   "[]",
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
