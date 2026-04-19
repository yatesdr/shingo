package seerrds

import (
	"log"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/rds"
)

// MapState translates an RDS order state to a ShinGo dispatch status.
func MapState(vendorState string) string {
	switch rds.OrderState(vendorState) {
	case rds.StateCreated, rds.StateToBeDispatched:
		return protocol.StatusDispatched
	case rds.StateRunning:
		return protocol.StatusInTransit
	case rds.StateWaiting:
		return protocol.StatusStaged
	case rds.StateFinished:
		return protocol.StatusDelivered
	case rds.StateFailed:
		return protocol.StatusFailed
	case rds.StateStopped:
		return protocol.StatusCancelled
	default:
		log.Printf("mapstate: unrecognized RDS state %q, defaulting to dispatched", vendorState)
		return protocol.StatusDispatched
	}
}

// IsTerminalState returns true if the RDS state is a terminal state.
func IsTerminalState(vendorState string) bool {
	return rds.OrderState(vendorState).IsTerminal()
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
		VehicleID:      r.VehicleID,
		Connected:      r.ConnectionStatus != 0,
		Available:      r.Dispatchable,
		Busy:           r.ProcBusiness,
		Emergency:      r.RbkReport.Emergency,
		Blocked:        r.RbkReport.Blocked,
		IsError:        r.IsError,
		BatteryLevel:   r.RbkReport.BatteryLevel,
		Charging:       r.RbkReport.Charging,
		CurrentMap:     r.BasicInfo.CurrentMap,
		Model:          r.BasicInfo.Model,
		IP:             r.BasicInfo.IP,
		X:              r.RbkReport.X,
		Y:              r.RbkReport.Y,
		Angle:          r.RbkReport.Angle,
		NetworkDelay:   r.NetworkDelay,
		CurrentStation: r.RbkReport.CurrentStation,
		LastStation:    r.RbkReport.LastStation,
 		OdoTotal:       r.RbkReport.Odo,
 		OdoToday:       r.RbkReport.TodayOdo,
 		SessionMs:      r.RbkReport.Time,
 		TotalMs:        r.RbkReport.TotalTime,
 		LiftCount:      r.RbkReport.Jack.JackLoadTimes,
 		LiftHeight:     r.RbkReport.Jack.JackHeight,
 		LiftError:      r.RbkReport.Jack.JackErrorCode,
 		BatteryV:       r.RbkReport.Voltage,
 		BatteryA:       r.RbkReport.Current,
 		CtrlTemp:       r.BasicInfo.CtrlTemp,
 		CtrlHumi:       r.BasicInfo.CtrlHumi,
 		CtrlVoltage:    r.BasicInfo.CtrlVoltage,
 		Version:        r.BasicInfo.Version,
	}
}
