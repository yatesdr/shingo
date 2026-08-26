package seerrds

import (
	"log"
	"time"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/rds"
)

// MapState translates an RDS order state to a ShinGo dispatch status.
func MapState(vendorState string) string {
	switch rds.OrderState(vendorState) {
	case rds.StateCreated, rds.StateToBeDispatched:
		return string(protocol.StatusDispatched)
	case rds.StateRunning:
		return string(protocol.StatusInTransit)
	case rds.StateWaiting:
		return string(protocol.StatusStaged)
	case rds.StateFinished:
		return string(protocol.StatusDelivered)
	case rds.StateFailed:
		return string(protocol.StatusFaulted)
	case rds.StateStopped:
		return string(protocol.StatusCancelled)
	default:
		log.Printf("mapstate: unrecognized RDS state %q, defaulting to dispatched", vendorState)
		return string(protocol.StatusDispatched)
	}
}

// IsTerminalState returns true if the RDS state is a terminal state.
func IsTerminalState(vendorState string) bool {
	return rds.OrderState(vendorState).IsTerminal()
}

// BinTaskForAction maps an abstract dispatch action to the vendor-specific
// BinTask value for SEER RDS. Dispatch passes action strings ("pickup",
// "dropoff", "wait"); the adapter translates them so dispatch doesn't depend
// on vendor-specific vocabulary.
func BinTaskForAction(action string) string {
	switch action {
	case protocol.ActionPickup:
		return "JackLoad"
	case protocol.ActionDropoff:
		return "JackUnload"
	case protocol.ActionWait:
		return "Wait"
	default:
		return ""
	}
}

// mapOrderSnapshot converts an rds.OrderDetail to a fleet.OrderSnapshot.
func mapOrderSnapshot(d *rds.OrderDetail) *fleet.OrderSnapshot {
	s := &fleet.OrderSnapshot{
		VendorOrderID: d.ID,
		State:         string(d.State),
		Vehicle:       d.Vehicle,
		CreateTime:    d.CreateTime,
		TerminalTime:  d.TerminalTime,
	}
	for _, b := range d.Blocks {
		s.Blocks = append(s.Blocks, fleet.BlockSnapshot{
			BlockID:  b.BlockID,
			Location: b.Location,
			State:    string(b.State),
		})
	}
	for _, e := range d.Errors {
		s.Errors = append(s.Errors, fleet.OrderMessage{Code: e.Code, Desc: e.Desc, Times: e.Times, Timestamp: e.Timestamp})
	}
	for _, w := range d.Warnings {
		s.Warnings = append(s.Warnings, fleet.OrderMessage{Code: w.Code, Desc: w.Desc, Times: w.Times, Timestamp: w.Timestamp})
	}
	for _, n := range d.Notices {
		s.Notices = append(s.Notices, fleet.OrderMessage{Code: n.Code, Desc: n.Desc, Times: n.Times, Timestamp: n.Timestamp})
	}
	return s
}

// mapRobotStatus converts an rds.RobotStatus to a fleet.RobotStatus.
func mapRobotStatus(r rds.RobotStatus) fleet.RobotStatus {
	return fleet.RobotStatus{
		VehicleID:         r.VehicleID,
		Connected:         r.ConnectionStatus != 0,
		Available:         r.Dispatchable,
		Busy:              r.ProcBusiness,
		Emergency:         r.RbkReport.Emergency,
		Blocked:           r.RbkReport.Blocked,
		IsError:           r.IsError,
		BatteryLevel:      r.RbkReport.BatteryLevel * 100, // SEER returns 0.0–1.0; we expose 0–100
		Charging:          r.RbkReport.Charging,
		CurrentMap:        r.BasicInfo.CurrentMap,
		Model:             r.BasicInfo.Model,
		IP:                r.BasicInfo.IP,
		X:                 r.RbkReport.X,
		Y:                 r.RbkReport.Y,
		Angle:             r.RbkReport.Angle,
		Confidence:        r.RbkReport.Confidence,
		RelocStatus:       r.RbkReport.RelocStatus,
		AreaIDs:           r.RbkReport.AreaIDs,
		NetworkDelay:      r.NetworkDelay,
		CurrentStation:    r.RbkReport.CurrentStation,
		LastStation:       r.RbkReport.LastStation,
		OdoTotal:          r.RbkReport.Odo,
		OdoToday:          r.RbkReport.TodayOdo,
		SessionMs:         r.RbkReport.Time,
		TotalMs:           r.RbkReport.TotalTime,
		LiftCount:         r.RbkReport.Jack.JackLoadTimes,
		LiftHeight:        r.RbkReport.Jack.JackHeight,
		LiftError:         r.RbkReport.Jack.JackErrorCode,
		JackState:         r.RbkReport.Jack.JackState,
		JackIsFull:        r.RbkReport.Jack.JackIsFull,
		IsLoaded:          r.IsLoaded,
		BatteryV:          r.RbkReport.Voltage,
		BatteryA:          r.RbkReport.Current,
		BatteryTemp:       r.RbkReport.BatteryTemperature,
		BatteryCycle:      r.RbkReport.BatteryCycle,
		BatteryChargeA:    r.RbkReport.BatteryChargeCurr,
		BatteryChargeV:    r.RbkReport.BatteryChargeVolt,
		BatteryMaxChargeA: r.RbkReport.BatteryMaxChargeCurr,
		BatteryMaxChargeV: r.RbkReport.BatteryMaxChargeVolt,
		BatteryManualConn: r.RbkReport.BatteryManualConn,
		MapMD5:            r.RbkReport.CurrentMapMD5,
		CtrlTemp:          r.BasicInfo.CtrlTemp,
		CtrlHumi:          r.BasicInfo.CtrlHumi,
		CtrlVoltage:       r.BasicInfo.CtrlVoltage,
		Version:           r.BasicInfo.Version,
		TaskStatus:        r.RbkReport.TaskStatus,
		Suspended:         r.UndispatchableReason.Suspended,
		Undispatchable: fleet.Undispatchable{
			MapInvalid:       r.UndispatchableReason.CurrentMapInvalid,
			UnconfirmedReloc: r.UndispatchableReason.UnconfirmedReloc,
			LowBattery:       r.UndispatchableReason.LowBattery,
			Disconnect:       r.UndispatchableReason.Disconnect,
			Suspended:        r.UndispatchableReason.Suspended,
			Status:           r.UndispatchableReason.DispatchableStatus,
			Unlock:           r.UndispatchableReason.Unlock,
		},
		Alarms: mapRobotAlarms(r.RbkReport.Alarms),
	}
}

// mapSceneState lifts the fleet-wide fields off the /robotsStatus envelope.
//
// These belong to no robot, which is why they survived unread for as long as
// they did: every consumer of this endpoint reaches past the envelope for
// .Report and nothing ever looked at the object holding it.
func mapSceneState(resp *rds.RobotsStatusResponse, observedAt time.Time) fleet.SceneState {
	s := fleet.SceneState{SceneMD5: resp.SceneMD5, ObservedAt: observedAt}
	for _, p := range resp.DisablePaths {
		if p.ID != "" {
			s.DisabledPaths = append(s.DisabledPaths, p.ID)
		}
	}
	for _, p := range resp.DisablePoints {
		if p.ID != "" {
			s.DisabledPoints = append(s.DisabledPoints, p.ID)
		}
	}
	return s
}

// mapRobotAlarms flattens the SEER rbk_report.alarms severity buckets into one
// vendor-neutral list, tagging each with its severity (Q-026). Order is
// fatal→error→warning→notice so the most severe lead the slice.
func mapRobotAlarms(a rds.RbkAlarms) []fleet.RobotAlarm {
	n := len(a.Fatals) + len(a.Errors) + len(a.Warnings) + len(a.Notices)
	if n == 0 {
		return nil
	}
	out := make([]fleet.RobotAlarm, 0, n)
	add := func(severity string, alarms []rds.RbkAlarm) {
		for _, al := range alarms {
			out = append(out, fleet.RobotAlarm{Code: al.Code, Severity: severity, Desc: al.Desc})
		}
	}
	add("fatal", a.Fatals)
	add("error", a.Errors)
	add("warning", a.Warnings)
	add("notice", a.Notices)
	return out
}
