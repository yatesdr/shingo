package engine

import (
	"fmt"
	"time"

	"shingo/protocol"
	"shingocore/dispatch/eta"
	"shingocore/fleet"
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
	// Carrying is true when the assigned robot is physically loaded (lift/jack up).
	// The map uses it as the authoritative fetch-vs-carry signal instead of inferring
	// the leg from robot↔source geometry — which broke on multi-leg / complex orders.
	Carrying bool `json:"carrying"`
}

// liftLoadedThreshold is the lift/jack height (metres) above which a robot counts as
// physically loaded (carrying). A jacked-up AMR reports ~0.15 m; the small floor keeps
// sensor noise from reading as "loaded". A robot with no lift reports 0 → carrying=false,
// and the map falls back to its geometric leg inference for those.
const liftLoadedThreshold = 0.02

func (e *Engine) GetActiveOrdersWithRobotLocation() ([]BoardOrder, error) {
	return e.GetActiveOrdersWithRobotLocationFiltered(nil)
}

// GetActiveOrdersWithRobotLocationFiltered is the board query scoped to a set
// of station IDs — the server-side "area" filter a dashboard applies. An
// empty/nil stations slice returns the plant-wide board. Robot-cache and ETA
// enrichment are identical to the unscoped path.
func (e *Engine) GetActiveOrdersWithRobotLocationFiltered(stations []string) ([]BoardOrder, error) {
	dbOrders, err := e.db.ListActiveBoardOrdersFiltered(stations)
	if err != nil {
		return nil, fmt.Errorf("board: list active orders: %w", err)
	}

	robots := e.GetAllCachedRobots()
	robotMap := make(map[string]fleet.RobotStatus, len(robots))
	for _, r := range robots {
		robotMap[r.VehicleID] = r
	}

	result := make([]BoardOrder, 0, len(dbOrders))
	for _, o := range dbOrders {
		rob := robotMap[o.RobotID]
		bo := BoardOrder{
			OrderID:        o.ID,
			RobotID:        o.RobotID,
			SourceNode:     o.SourceNode,
			PayloadCode:    o.PayloadCode,
			DeliveryNode:   o.DeliveryNode,
			Status:         string(o.Status),
			StationID:      o.StationID,
			CreatedAt:      o.CreatedAt.Format(time.RFC3339),
			CurrentStation: rob.CurrentStation,
			Carrying:       rob.LiftHeight > liftLoadedThreshold,
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
		OrderID:      o.ID,
		RobotID:      o.RobotID,
		SourceNode:   o.SourceNode,
		PayloadCode:  o.PayloadCode,
		DeliveryNode: o.DeliveryNode,
		Status:       string(o.Status),
		StationID:    o.StationID,
		CreatedAt:    o.CreatedAt.Format(time.RFC3339),
	}
	if rs, ok := e.GetCachedRobotStatus(o.RobotID); ok {
		bo.CurrentStation = rs.CurrentStation
		bo.Carrying = rs.LiftHeight > liftLoadedThreshold
	}
	if string(o.Status) == "in_transit" {
		bo.ETA = eta.Stamp(e.etaCache, o.SourceNode, o.DeliveryNode)
	}
	return bo, nil
}
