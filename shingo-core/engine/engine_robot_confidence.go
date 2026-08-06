// engine_robot_confidence.go — localization-confidence sampling off the
// existing robot poll.
//
// This adds NO new polling. robotRefreshLoop already asks RDS for every
// robot's status every 2 seconds; the confidence figure has been arriving in
// that response all along and being discarded at unmarshal. This file decides
// which of those readings are worth keeping and writes them.

package engine

import (
	"time"

	"shingocore/fleet"
	"shingocore/store/robotconfidence"
)

// confidenceOrderRefresh bounds how often the robot→order map is re-read.
//
// Attributing a sample to the mission it was taken during is worth having,
// but not worth a query on every tick: at one refresh per ten seconds the
// cost is six queries a minute regardless of how many samples are written,
// and the worst case is a sample tagged with an order the robot finished up
// to ten seconds ago. That is far finer than any question these rows answer.
const confidenceOrderRefresh = 10 * time.Second

// robotConfidenceSampler is the write rule's memory between polls.
//
// Single-writer by construction: every field is touched only from
// robotRefreshLoop's goroutine, which is also the only caller of
// sampleRobotConfidence. No lock, and none needed — but nothing else may
// reach in here without changing that.
type robotConfidenceSampler struct {
	last     map[string]robotconfidence.LastStored
	orders   map[string]int64
	ordersAt time.Time
}

// sampleRobotConfidence evaluates one poll's worth of robots against the
// write rule and stores whatever survives.
func (e *Engine) sampleRobotConfidence(robots []fleet.RobotStatus, now time.Time) {
	if e.cfg == nil || e.db == nil {
		return
	}
	c := e.cfg.RobotConfidence
	// Disabled means the write path does not run at all, rather than running
	// and discarding. This is the kill switch a plant reaches for if the
	// collector ever surprises it on load.
	if !c.Enabled {
		return
	}
	if e.confidence == nil {
		e.confidence = &robotConfidenceSampler{last: map[string]robotconfidence.LastStored{}}
	}
	s := e.confidence

	// Read the rule from config every pass so a reconfigure takes effect
	// without a restart.
	rule := robotconfidence.WriteRule{
		DeadBandMetres:     c.DeadBandMetres,
		DeadBandConfidence: c.DeadBandConfidence,
		LowThreshold:       c.LowConfidenceThreshold,
		LowInterval:        c.LowInterval,
		StuckInterval:      c.StuckInterval,
		FailedInterval:     c.FailedInterval,
	}

	var batch []robotconfidence.Sample
	for _, r := range robots {
		if r.VehicleID == "" {
			continue
		}
		var last *robotconfidence.LastStored
		if v, ok := s.last[r.VehicleID]; ok {
			last = &v
		}
		keep, _ := rule.Decide(robotconfidence.Observation{
			Connected:   r.Connected,
			RelocStatus: r.RelocStatus,
			Confidence:  r.Confidence,
			X:           r.X,
			Y:           r.Y,
			OnTask:      r.Busy,
		}, last, now)
		if !keep {
			continue
		}
		batch = append(batch, robotconfidence.Sample{
			VehicleID:   r.VehicleID,
			SampledAt:   now,
			Confidence:  r.Confidence,
			X:           r.X,
			Y:           r.Y,
			Angle:       r.Angle,
			Station:     r.CurrentStation,
			LastStation: r.LastStation,
			OnTask:      r.Busy,
			Blocked:     r.Blocked,
			RelocStatus: r.RelocStatus,
			AreaIDs:     r.AreaIDs,
		})
	}
	if len(batch) == 0 {
		return
	}

	// Resolve order attribution only when there is something to write.
	if now.Sub(s.ordersAt) >= confidenceOrderRefresh {
		if m, err := e.db.ActiveOrderIDsByRobot(); err != nil {
			// Not fatal. A sample with order_id = 0 is still a valid
			// localization reading; losing the whole sample because the
			// mission lookup failed would be the worse trade.
			e.dbg("engine: robot confidence: order lookup: %v", err)
		} else {
			s.orders, s.ordersAt = m, now
		}
	}
	for i := range batch {
		batch[i].OrderID = s.orders[batch[i].VehicleID]
	}

	if err := robotconfidence.InsertBatch(e.db.DB, batch, c.LowConfidenceThreshold); err != nil {
		e.dbg("engine: robot confidence: insert: %v", err)
		return
	}

	// The baseline advances ONLY after a successful write, and that ordering
	// matters. If a failed insert still moved the last-stored marker, the
	// dead-bands would be measured from a row that does not exist — a robot
	// could cross the plant during a database outage and, on recovery, look
	// like it had never moved far enough to sample.
	for _, w := range batch {
		s.last[w.VehicleID] = robotconfidence.LastStored{
			X: w.X, Y: w.Y, Confidence: w.Confidence, At: w.SampledAt,
		}
	}
}
