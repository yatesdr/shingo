package engine

import (
	"fmt"
	"time"

	"shingo/protocol"
	"shingocore/dispatch/eta"
)

type BoardOrder struct {
	OrderID        int64  `json:"order_id"`
	RobotID        string `json:"robot_id"`
	SourceNode     string `json:"source_node"`
	PayloadCode    string `json:"payload_code"`
	DeliveryNode   string `json:"delivery_node"`
	Status         string `json:"status"`
	ETA            string `json:"eta,omitempty"`
	StationID      string `json:"station_id"`
	CreatedAt      string `json:"created_at"`
	CurrentStation string `json:"current_station"`
}

func (e *Engine) GetActiveOrdersWithRobotLocation() ([]BoardOrder, error) {
	dbOrders, err := e.db.ListActiveBoardOrders()
	if err != nil {
		return nil, fmt.Errorf("board: list active orders: %w", err)
	}

	robots := e.GetAllCachedRobots()
	robotMap := make(map[string]string, len(robots))
	for _, r := range robots {
		robotMap[r.VehicleID] = r.CurrentStation
	}

	result := make([]BoardOrder, 0, len(dbOrders))
	for _, o := range dbOrders {
		bo := BoardOrder{
			OrderID:        o.ID,
			RobotID:        o.RobotID,
			SourceNode:     o.SourceNode,
			PayloadCode:    o.PayloadCode,
			DeliveryNode:   o.DeliveryNode,
			Status:         string(o.Status),
			StationID:      o.StationID,
			CreatedAt:      o.CreatedAt.Format(time.RFC3339),
			CurrentStation: robotMap[o.RobotID],
		}
		if string(o.Status) == "in_transit" {
			bo.ETA = eta.Stamp(e.etaCache, o.SourceNode, o.DeliveryNode)
		}
		result = append(result, bo)
	}
	return result, nil
}

func (e *Engine) GetActiveOrderWithRobotLocation(orderID int64) (*BoardOrder, error) {
	o, err := e.db.GetOrder(orderID)
	if err != nil {
		return nil, fmt.Errorf("board: get order %d: %w", orderID, err)
	}
	if o == nil || protocol.IsTerminal(o.Status) {
		return nil, nil
	}

	bo := &BoardOrder{
		OrderID:        o.ID,
		RobotID:        o.RobotID,
		SourceNode:     o.SourceNode,
		PayloadCode:    o.PayloadCode,
		DeliveryNode:   o.DeliveryNode,
		Status:         string(o.Status),
		StationID:      o.StationID,
		CreatedAt:      o.CreatedAt.Format(time.RFC3339),
	}
	if rs, ok := e.GetCachedRobotStatus(o.RobotID); ok {
		bo.CurrentStation = rs.CurrentStation
	}
	if string(o.Status) == "in_transit" {
		bo.ETA = eta.Stamp(e.etaCache, o.SourceNode, o.DeliveryNode)
	}
	return bo, nil
}
