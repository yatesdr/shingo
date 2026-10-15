// robot_lidar.go — lidar hours: one laser frame per parked robot per hour,
// through RDS's relay, reduced to a few numbers per lidar and kept 90 days.
//
// DATA COLLECTION, NOT A SIGNAL. The gauge is unproven on window film (the
// wipe tests in DATA-laser-frame-gauge.md were clean-on-clean and moved
// nothing); the record is kept so the daily wipe cycle can answer it over
// weeks. Nothing in the UI or the cleaning loop reads these rows as dirt, and
// nothing compares one robot with another: a single frame is dominated by what
// the robot is parked facing (DATA §3), so the only readable comparison is a
// robot against itself at the same station. Before/after a cleaning is a time
// join on robot_lidar_hours for the same (vehicle, station), not a probe.
//
// ITS OWN GOROUTINE, NEVER THE POLL'S. A relayed frame is 50-700 ms; a dozen
// in sequence on robotRefreshLoop would stall the 2-second cadence the
// confidence sampler and the carried-bin sweep depend on.

package engine

import (
	"math"
	"time"

	"shingocore/fleet"
	"shingocore/store/robothours"

	"shingo/protocol/clock"
)

// lidarRoundOffset places each hourly round this far past the top of the UTC
// hour. Aligning to the clock rather than to process start is what keeps a
// restart from costing a second round of relay calls: a process that starts
// after this hour's round already ran waits for the next hour, and Core
// restarts often enough (Springfield ~15 times a week) that a start-relative
// ticker would add a round per restart. Off the hour itself so the round does
// not land on the hourly record's rollover flush.
const lidarRoundOffset = 5 * time.Minute

// lidarRobotGap is the least time between two robots' probes. Robokit asks for
// 100-200 ms between requests on a connection; RDS holds the connection, and
// this keeps the relay at that pace.
const lidarRobotGap = 200 * time.Millisecond

// lidarParkedMetres is how far a robot may move across the probe and still
// count as parked: 5 cm, the limit DATA §6 used because the scene seen by the
// lidar moves under a larger shift. It is deliberately NOT the confidence write
// rule's 0.25 m dead band, which belongs to a different instrument.
const lidarParkedMetres = 0.05

// lidarPollWait bounds how long the round waits for a poll newer than a probe,
// so the bracket's second reading postdates the frame. Two poll intervals and
// a margin; a fleet that stops answering drops the robot from the round.
const lidarPollWait = 2*robotPollInterval + time.Second

// nextLidarRound is the next round time strictly after now: lidarRoundOffset
// past a UTC hour.
func nextLidarRound(now time.Time) time.Time {
	next := now.UTC().Truncate(time.Hour).Add(lidarRoundOffset)
	if !next.After(now) {
		next = next.Add(time.Hour)
	}
	return next
}

// lidarKeep decides whether a frame taken between two poll readings of one
// robot is a parked frame. ok is false when the robot was not in the later
// poll. The reason is "" when the frame is kept, else a word for the round's
// summary.
func lidarKeep(before, after fleet.RobotStatus, ok bool) string {
	switch {
	case !ok:
		return "gone"
	case !after.Connected:
		return "disconnected"
	case after.Busy:
		return "busy"
	case after.CurrentStation == "" || after.CurrentStation != before.CurrentStation:
		return "station changed"
	case math.Hypot(after.X-before.X, after.Y-before.Y) >= lidarParkedMetres:
		return "moved"
	}
	return ""
}

// lidarCandidate is the round's population test: a robot the poll says is
// connected, idle and at a station.
func lidarCandidate(r fleet.RobotStatus) bool {
	return r.VehicleID != "" && r.Connected && !r.Busy && r.CurrentStation != ""
}

// lidarRows maps one robot's reduced frame to its rows, one per lidar.
func lidarRows(hour time.Time, vehicleID, station string, stats []fleet.LidarStats) []robothours.LidarHour {
	out := make([]robothours.LidarHour, 0, len(stats))
	for _, s := range stats {
		out = append(out, robothours.LidarHour{
			Hour: hour, VehicleID: vehicleID, Lidar: s.Lidar, Station: station, UseForLoc: s.UseForLoc,
			Beams: s.Beams, Valid: s.Valid, RSSIMedValid: s.RSSIMedValid, RSSIP25Valid: s.RSSIP25Valid,
			RSSIP75Valid: s.RSSIP75Valid, NearValid: s.NearValid,
		})
	}
	return out
}

// lidarHoursLoop runs one round per UTC hour (nextLidarRound) for as long as
// the engine runs. A fleet that cannot relay a frame (the simulator) makes it
// a no-op, said once.
func (e *Engine) lidarHoursLoop() {
	reader, ok := e.fleet.(fleet.LidarFrameReader)
	if !ok {
		e.logFn("engine: lidar hours: fleet cannot relay a laser frame; nothing collected")
		return
	}
	if e.db == nil {
		return
	}
	clk := clock.Default()
	for {
		select {
		case <-e.stopChan:
			return
		case <-clk.After(nextLidarRound(clk.Now()).Sub(clk.Now())):
		}
		if !e.fleetConnected.Load() {
			e.dbg("engine: lidar hours: fleet not connected; round skipped")
			continue
		}
		e.lidarRound(reader, clk.Now())
	}
}

// lidarPolled finds one robot in the latest poll, and the poll's time.
func (e *Engine) lidarPolled(vehicleID string) (fleet.RobotStatus, time.Time, bool) {
	robots, at := e.latestPolledRobots()
	for _, r := range robots {
		if r.VehicleID == vehicleID {
			return r, at, true
		}
	}
	return fleet.RobotStatus{}, at, false
}

// lidarPollAfter waits for a poll that landed after t and returns the robot
// from it. false when none landed within lidarPollWait or the engine stopped.
func (e *Engine) lidarPollAfter(vehicleID string, t time.Time) (fleet.RobotStatus, bool) {
	deadline := time.Now().Add(lidarPollWait)
	for {
		r, at, ok := e.lidarPolled(vehicleID)
		if at.After(t) {
			return r, ok
		}
		if time.Now().After(deadline) {
			return fleet.RobotStatus{}, false
		}
		select {
		case <-e.stopChan:
			return fleet.RobotStatus{}, false
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// lidarRound probes every candidate robot once, sequentially, and writes the
// parked frames in one statement.
//
// THE BRACKET IS THE PARKED TEST. The robot's poll reading before the probe and
// a poll reading that landed after it must agree on the station, still show it
// connected and idle, and put it within 5 cm. The second reading waits for a
// poll newer than the frame, because the cache an instant later is usually the
// same poll as the first reading and would prove nothing.
func (e *Engine) lidarRound(reader fleet.LidarFrameReader, start time.Time) {
	hour := start.UTC().Truncate(time.Hour)
	robots, _ := e.latestPolledRobots()
	var (
		rows                           []robothours.LidarHour
		candidates, probed, kept, errs int
		skipped                        = map[string]int{}
	)
	for _, r := range robots {
		if !lidarCandidate(r) {
			continue
		}
		candidates++
		before, _, ok := e.lidarPolled(r.VehicleID)
		if !ok || !lidarCandidate(before) {
			skipped["not parked"]++
			continue
		}
		stats, err := reader.ReadLidarFrames(r.VehicleID)
		probedAt := time.Now()
		probed++
		if err != nil {
			errs++
			e.dbg("engine: lidar hours: %s: %v", r.VehicleID, err)
		} else {
			after, seen := e.lidarPollAfter(r.VehicleID, probedAt)
			if why := lidarKeep(before, after, seen); why != "" {
				skipped[why]++
			} else {
				kept++
				rows = append(rows, lidarRows(hour, r.VehicleID, before.CurrentStation, stats)...)
			}
		}
		select {
		case <-e.stopChan:
			return
		case <-time.After(lidarRobotGap):
		}
	}

	if err := robothours.InsertLidarHours(e.db.DB, rows); err != nil {
		e.logFn("engine: lidar hours: write %d rows: %v", len(rows), err)
		rows = nil
	}
	pruned, err := robothours.PruneLidarHours(e.db.DB, start)
	if err != nil {
		e.dbg("engine: lidar hours: prune: %v", err)
	}
	e.logFn("engine: lidar hours %s: %d parked candidates, %d probed, %d kept, skipped %v, %d errors, %d rows, %d pruned",
		hour.Format(time.RFC3339), candidates, probed, kept, skipped, errs, len(rows), pruned)
}
