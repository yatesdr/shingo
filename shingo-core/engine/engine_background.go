package engine

import (
	"crypto/sha256"
	"encoding/json"
	"time"

	"shingocore/fleet"
)

// ── Background loops ────────────────────────────────────────────────
//
// robotRefreshLoop keeps the in-memory robot status cache warm and
// only emits EventRobotsUpdated when the serialized state actually
// changes (SHA-256 compare), so UI subscribers don't re-render on
// every poll. stagedBinSweepLoop runs the two bin-hygiene passes —
// expired staged bins and orphaned claims — on the configured staging
// sweep interval.

// robotRefreshLoop polls robot status every 2 seconds and emits EventRobotsUpdated
// only when the robot state has actually changed.
func (e *Engine) robotRefreshLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var prevHash [sha256.Size]byte
	for {
		select {
		case <-e.stopChan:
			return
		case <-ticker.C:
			if !e.fleetConnected.Load() {
				continue
			}
			rl, ok := e.fleet.(fleet.RobotLister)
			if !ok {
				continue
			}
			robots, err := rl.GetRobotsStatus()
			if err != nil {
				e.dbg("engine: robot refresh: %v", err)
				continue
			}
			// Update robot position cache (used for telemetry snapshots)
			e.robotsMu.Lock()
			for _, r := range robots {
				e.robotsCache[r.VehicleID] = r
			}
			e.robotsMu.Unlock()

			data, _ := json.Marshal(robots)
			hash := sha256.Sum256(data)
			if hash == prevHash {
				continue
			}
			prevHash = hash
			e.Events.Emit(Event{
				Type:    EventRobotsUpdated,
				Payload: RobotsUpdatedEvent{Robots: robots},
			})
		}
	}
}

// stagedBinSweepLoop periodically releases staged bins whose expiry has passed.
func (e *Engine) stagedBinSweepLoop() {
	interval := e.cfg.Staging.SweepInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopChan:
			return
		case <-ticker.C:
			count, err := e.db.ReleaseExpiredStagedBins()
			if err != nil {
				e.logFn("engine: staged bin sweep error: %v", err)
			} else if count > 0 {
				e.logFn("engine: released %d expired staged bins", count)
			}
			orphaned, err := e.db.ReleaseOrphanedClaims()
			if err != nil {
				e.logFn("engine: orphan claim sweep error: %v", err)
			} else if orphaned > 0 {
				e.logFn("engine: released %d orphaned bin claims from terminal orders", orphaned)
			}
		}
	}
}
