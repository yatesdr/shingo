package seerrds

import (
	"shingocore/dispatch"
	"shingocore/fleet"
	"shingocore/rds"
)

// MapState translates an RDS order state to a ShinGo dispatch status.
func MapState(vendorState string) string {
	switch rds.OrderState(vendorState) {
	case rds.StateCreated, rds.StateToBeDispatched:
		return dispatch.StatusDispatched
	case rds.StateRunning:
		return dispatch.StatusInTransit
	case rds.StateWaiting:
		return dispatch.StatusStaged
	case rds.StateFinished:
		return dispatch.StatusDelivered
	case rds.StateFailed:
		return dispatch.StatusFailed
	case rds.StateStopped:
		return dispatch.StatusCancelled
	default:
		return dispatch.StatusDispatched
	}
}

// IsTerminalState returns true if the RDS state is a terminal state.
func IsTerminalState(vendorState string) bool {
	return rds.OrderState(vendorState).IsTerminal()
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
	}
}
