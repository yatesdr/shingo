package www

import (
	"log"
	_ "sync" // the plain lock inlines only where sync is imported (protocol/lockcheck)
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/lockcheck"
	"shingocore/config"
	"shingocore/fleet"
	"shingocore/internal/lockorder"
	"shingocore/service"
)

// RobotOrderLine is what a robot tile says about the order it is carrying.
//
// The tile shows a robot's own state and has never named the order it is on,
// which is the join an operator makes by hand when a robot stops moving. A
// faulted order is the case where that matters most: the robot looks busy, the
// order is stuck, and nothing on the page connects the two.
//
// ONE ALARM, AND ONLY ONE THAT BEGAN WITH THE FAULT. Reflector and battery
// warnings fire constantly, so an alarm printed beside a fault reads as its
// cause when it is usually background. FaultAlarm is therefore the robot's
// most severe alarm that Core FIRST SAW no earlier than faultAlarmLead before
// the fault began (faultAlarm), never one of the standing codes. Core's own
// clock decides "first seen": a robot's alarm timestamps are its own clock,
// which drifts by hours (SPR AMR-10, 2026-11-12, read ~20 h ahead).
type RobotOrderLine struct {
	OrderID     int64       `json:"order_id,omitempty"`
	OrderStatus string      `json:"order_status,omitempty"`
	FaultSince  *time.Time  `json:"fault_since,omitempty"`
	FaultNotice bool        `json:"fault_notice,omitempty"`
	FaultAlarm  *FaultAlarm `json:"fault_alarm,omitempty"`
}

// FaultAlarm is the alarm that began with a robot's fault, as the fleet words
// it; the robot map shortens it for a person (dashboard-map.js alarmWords).
type FaultAlarm struct {
	Code     int    `json:"code"`
	Severity string `json:"severity"`
	Desc     string `json:"desc"`
}

// faultAlarmLead is how far before the fault an alarm may begin and still be
// its cause: the obstacle, the e-stop or the lost cart comes first and the
// fleet fails the order a few seconds to a minute after.
const faultAlarmLead = 2 * time.Minute

// standingAlarms never explain a fault: on at every plant most of the time
// (HOP census, 30 days to 2026-11-12: 54018 reflector area 6,830 times on all
// 14 robots) or routine (54211 battery below the warning level, 54014
// vibration while stationary, 57101 motor calibration interrupted by an
// e-stop, a notice).
var standingAlarms = map[int]bool{54018: true, 54211: true, 54014: true, 57101: true}

var alarmRank = map[string]int{"fatal": 0, "error": 1, "warning": 2, "notice": 3}

// faultAlarm picks the alarm that began with a fault from a robot's current
// alarms, given when Core first saw each code (zero: present when Core first
// saw the robot, so its onset is unknown and it is not offered) and when the
// fault began. Most severe first, then the latest to begin. nil: none.
func faultAlarm(alarms []fleet.RobotAlarm, firstSeen map[int]time.Time, since time.Time) *FaultAlarm {
	var best *fleet.RobotAlarm
	var bestAt time.Time
	for i := range alarms {
		a := &alarms[i]
		at := firstSeen[a.Code]
		if standingAlarms[a.Code] || at.IsZero() || at.Before(since.Add(-faultAlarmLead)) {
			continue
		}
		if best == nil || rankOf(a.Severity) < rankOf(best.Severity) ||
			(rankOf(a.Severity) == rankOf(best.Severity) && at.After(bestAt)) {
			best, bestAt = a, at
		}
	}
	if best == nil {
		return nil
	}
	return &FaultAlarm{Code: best.Code, Severity: best.Severity, Desc: best.Desc}
}

func rankOf(severity string) int {
	if r, ok := alarmRank[severity]; ok {
		return r
	}
	return len(alarmRank)
}

// alarmOnsets is when Core first saw each alarm code on each robot, kept
// across fleet polls. FIRST SIGHT IS UNKNOWN: the alarms a robot carries when
// Core first sees it (boot, a reconnect, a robot back in the list) began at an
// unknown time and are recorded as zero, as robot_hours.go's onset count
// seeds rather than counts them. A code that clears is forgotten, so its next
// appearance is a new onset.
type alarmOnsets struct {
	mu   lockcheck.Mutex[lockorder.AlarmOnsets]
	seen map[string]map[int]time.Time
}

func (a *alarmOnsets) observe(robots []fleet.RobotStatus, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seen == nil {
		a.seen = map[string]map[int]time.Time{}
	}
	for _, r := range robots {
		prev, known := a.seen[r.VehicleID]
		if !r.Connected {
			delete(a.seen, r.VehicleID)
			continue
		}
		cur := make(map[int]time.Time, len(r.Alarms))
		for _, al := range r.Alarms {
			switch t, ok := prev[al.Code]; {
			case ok:
				cur[al.Code] = t
			case known:
				cur[al.Code] = now
			default:
				cur[al.Code] = time.Time{}
			}
		}
		a.seen[r.VehicleID] = cur
	}
}

// firstSeen is a copy of one robot's onsets.
func (a *alarmOnsets) firstSeen(vehicleID string) map[int]time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[int]time.Time, len(a.seen[vehicleID]))
	for k, v := range a.seen[vehicleID] {
		out[k] = v
	}
	return out
}

// buildRobotOrderLines is what the robot feed calls. A variable only so a test
// can count the builds per fleet tick (LC8); production never reassigns it.
var buildRobotOrderLines = robotOrderLines

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
