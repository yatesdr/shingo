package service

import (
	"shingo/protocol"
	"shingoedge/store/orders"
)

// RobotWatchOrders is what the live robot feed watches for one operator
// station while its robot panel is open (www robot_stream.go, LEAD-NOTES D12):
// the orders its board draws that have a Core uuid and a status Core sends a
// position for (protocol.IsVendorTracked: dispatched, in_transit, staged,
// faulted; U8 ruling 1), keyed uuid → this Edge's order id, the join every
// robot-pos frame carries.
//
// THE BOARD'S ORDERS, BY THE BOARD'S OWN READS, so the panel is never told of
// an order the station's cards do not show, nor left without one they do:
// newStationView's active changeover, adoptChangeoverParticipants' tiles, and
// one orders.ListActiveByNodeKeys over them, matched as BuildView matches a
// tile (its process node, or its core node as the order's source). A SEND BACK
// order at a fed press's outside position is on no tile's card (the board
// reads it for the SEND BACK line only), so it is not watched. Four statements
// with no changeover; a changeover adds its tasks, its participants and one
// read per adopted position. Pinned by
// TestRobotWatchOrders_TheBoardsTrackedOrdersWithAUUID.
//
// Empty, never nil, when the board draws none.
func (s *StationService) RobotWatchOrders(stationID int64) (map[string]int64, error) {
	station, err := s.db.GetOperatorStation(stationID)
	if err != nil {
		return nil, err
	}
	co, err := s.db.GetActiveProcessChangeover(station.ProcessID)
	if err != nil {
		co = nil // as newStationView: no active changeover is the error case
	}
	nodes, _, _, err := s.adoptChangeoverParticipants(stationID, co)
	if err != nil {
		return nil, err
	}
	keys := make([]orders.NodeKey, 0, len(nodes))
	for _, n := range nodes {
		keys = append(keys, orders.NodeKey{ProcessNodeID: n.ID, CoreNodeName: n.CoreNodeName})
	}
	byTile, err := orders.ListActiveByNodeKeys(s.db.DB, keys)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, list := range byTile {
		for _, o := range list {
			if o.UUID != "" && protocol.IsVendorTracked(o.Status) {
				out[o.UUID] = o.ID
			}
		}
	}
	return out, nil
}
