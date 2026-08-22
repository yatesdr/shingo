package www

import (
	"log"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/config"
	"shingocore/service"
)

// RobotOrderLine is what a robot tile says about the order it is carrying.
//
// The tile shows a robot's own state and has never named the order it is on,
// which is the join an operator makes by hand when a robot stops moving. A
// faulted order is the case where that matters most: the robot looks busy, the
// order is stuck, and nothing on the page connects the two.
//
// NO ALARMS. Reflector and localization alarms fire constantly, so an alarm
// printed beside a fault reads as its cause when it is usually background. The
// rule for ever adding one is "first seen after the fault began"; until a
// follow-up shows that a robot's alarms at fault time differ from its alarms the
// rest of the time, the tile stays accurate instead of suggestive.
type RobotOrderLine struct {
	OrderID     int64      `json:"order_id,omitempty"`
	OrderStatus string     `json:"order_status,omitempty"`
	FaultSince  *time.Time `json:"fault_since,omitempty"`
	FaultNotice bool       `json:"fault_notice,omitempty"`
}

// robotOrderLines maps robot id to the active order it is on.
//
// One pass over the active orders, plus one history read per FAULTED order —
// not per robot. A robot with more than one active order keeps the first seen;
// that is not a state the dispatcher creates, and a tile can only name one.
func robotOrderLines(svc *service.OrderService, cfg *config.Config) map[string]RobotOrderLine {
	lines := map[string]RobotOrderLine{}
	if svc == nil {
		return lines
	}
	orders, err := svc.ListActiveOrders()
	if err != nil {
		log.Printf("robot order lines: list active orders: %v", err)
		return lines
	}
	noticeAfter := time.Duration(0)
	if cfg != nil {
		noticeAfter = cfg.RDS.FaultNoticeAfter
	}
	now := clock.Now().UTC()

	// Two passes so the fault clocks are ONE query for the whole page rather
	// than one per faulted order — the same batching the health gauge does, and
	// for the same reason: this runs on every fleet change, and the per-order
	// form cost the most exactly when the most orders were faulted.
	// Keyed by order id rather than holding the rows: `orders` is the local
	// slice's name here, so the package's type is not nameable in this scope,
	// and the second pass only needs the id → robot mapping anyway.
	faultedRobot := map[int64]string{}
	faultedIDs := make([]int64, 0, 4)
	for _, o := range orders {
		if o.RobotID == "" {
			continue
		}
		if _, seen := lines[o.RobotID]; seen {
			continue
		}
		lines[o.RobotID] = RobotOrderLine{OrderID: o.ID, OrderStatus: string(o.Status)}
		if o.Status == protocol.StatusFaulted {
			faultedIDs = append(faultedIDs, o.ID)
			faultedRobot[o.ID] = o.RobotID
		}
	}
	if len(faultedIDs) == 0 {
		return lines
	}
	since, err := svc.LatestOrderHistoryTimesForStatus(faultedIDs, protocol.StatusFaulted)
	if err != nil {
		// The tiles still name their orders; they just carry no clock. A fault
		// row we could not read must not cost the page the rest of the line.
		log.Printf("robot order lines: fault clocks for %d orders: %v", len(faultedIDs), err)
		return lines
	}
	for id, robotID := range faultedRobot {
		t, ok := since[id]
		if !ok {
			continue
		}
		line := lines[robotID]
		at := t
		line.FaultSince = &at
		line.FaultNotice = noticeAfter > 0 && now.Sub(at) >= noticeAfter
		lines[robotID] = line
	}
	return lines
}
