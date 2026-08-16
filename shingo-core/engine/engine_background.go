package engine

import (
	"crypto/sha256"
	"encoding/json"
	"time"

	"shingocore/dispatch"
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
				// The fleet (SEER RDS) occasionally lists an unprovisioned slot
				// with no vehicle id; don't cache a keyless ghost — it renders as
				// a nameless fleet row and inflates the count for every consumer.
				if r.VehicleID == "" {
					continue
				}
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

// laneLivenessFloorInterval is the MAXIMUM WAIT the floor imposes, not a poll
// rate: the longest an order that could be released can sit after the event that
// should have freed it went missing. Events do the work continuously; on a
// healthy plant this pass finds nothing.
//
// 60s, matching the fulfillment scanner's sweep, because they are the same kind
// of thing over different populations and two different numbers would invite the
// question of why.
const laneLivenessFloorInterval = 60 * time.Second

// laneLivenessFloorLoop is F-22's floor: the periodic pass over the two wait
// populations that had only event releasers — gate-staged dwellers and compound
// legs Core has not yet handed to the fleet.
//
// It is the third and fourth instances of a shape this system already had twice
// (the fulfillment sweep, AdvanceStuckReshuffleParents), which is why it is
// eleven lines: everything it needs is level-triggered and idempotent already,
// so the loop is a trigger and nothing else. See dispatch.SweepLaneWaiters.
func (e *Engine) laneLivenessFloorLoop() {
	if e.dispatcher == nil {
		return
	}
	ticker := time.NewTicker(laneLivenessFloorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopChan:
			return
		case <-ticker.C:
			// The count is deliberately not logged when zero. Every release it
			// makes writes its own recovery_actions record naming the order and
			// the cause; a periodic "floor released 0" line would be the cry-wolf
			// AdvanceStuckReshuffleParents warns about, one level up.
			if n := e.dispatcher.SweepLaneWaiters(); n > 0 {
				e.logFn("engine: lane liveness floor released %d order(s) an event should have — "+
					"see recovery_actions (%s) for the causes", n, "lane_floor_release")
			}
			// THE STANDOFF TRIPWIRE RIDES THE SAME TICK, and after the floor
			// rather than before it: the floor's re-drive is what clears a wait
			// that only looked circular, so asking first would report standoffs
			// that the very next line dissolves. What survives a floor pass is
			// the real thing.
			//
			// Alarm only — it records and a human rules the incident. Silent at
			// zero, which is its normal state, so this line means a set of loaded
			// robots is holding itself still.
			if n := e.dispatcher.SweepMutualDigHolds(); n > 0 {
				e.logFn("engine: %d MUTUAL DIG HOLD(S) detected — digs waiting on each other in a "+
					"closed loop that cannot self-clear. See recovery_actions (%s). Dig admission "+
					"is supposed to make this unreachable, so each one is a defect in the "+
					"usable-capacity claim", n, "dig_standoff_detected")
			}

			// AND THE OTHER WAY A DIG HOLDS FOREVER: not waiting on another dig,
			// but holding its own lane for a bin whose episode has ended. It rides
			// the same pass because it asks the same population one more question,
			// and because both are alarms rather than actions — nothing here
			// releases anything, by ruling.
			if n := e.dispatcher.SweepReshufflesHoldingTargets(); n > 0 {
				e.logFn("engine: %d RESHUFFLE(S) are holding a lane for a bin nobody is coming for — "+
					"the demand that caused the dig is gone and the hold is keyed on the bin "+
					"leaving, so it will not end on its own. See recovery_actions (%s). A human "+
					"rules each one; the escape hatch is the Core-side hard release",
					n, dispatch.UnfetchedTargetAction)
			}
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
