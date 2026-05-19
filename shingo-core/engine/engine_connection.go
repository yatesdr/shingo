package engine

import (
	"time"

	"shingocore/fleet"
)

// ── Connection health ───────────────────────────────────────────────
//
// checkConnectionStatus probes fleet, messaging, and database once
// and emits a Connected/Disconnected event only on edge transitions
// (guarded by atomic CompareAndSwap). connectionHealthLoop runs the
// probe on a 30s ticker until the engine's stop channel is closed.

func (e *Engine) checkConnectionStatus() {
	// Fleet
	if err := e.fleet.Ping(); err == nil {
		if e.fleetConnected.CompareAndSwap(false, true) {
			// Take the pre-disconnect availability baseline before any
			// downstream subscriber or goroutine has a chance to mutate
			// state. Passing it through as a function argument decouples
			// auto-resume from the 2-second refresh loop — which is about
			// to start polling again now that fleetConnected is true and
			// would otherwise race the resume handler for robotsCache.
			baseline := e.takeReconnectBaseline()
			e.Events.Emit(Event{Type: EventFleetConnected, Payload: ConnectionEvent{Detail: e.fleet.Name() + " connected"}})
			go func() {
				total, created, deleted, err := e.SceneSync()
				if err != nil {
					e.logFn("engine: auto scene sync: %v", err)
					return
				}
				e.logFn("engine: auto scene sync: %d points, created %d, deleted %d nodes", total, created, deleted)
			}()
			go e.autoResumeAfterFleetReconnect(baseline)
		}
	} else {
		if e.fleetConnected.CompareAndSwap(true, false) {
			// Snapshot the cache before emitting the event: synchronous
			// subscribers (sse.go and any future hooks) might react to
			// EventFleetDisconnected by reading or pruning robotsCache.
			// Capturing first guarantees the baseline reflects the last
			// good state observed before any disconnect-driven cleanup.
			e.captureDisconnectBaseline()
			e.Events.Emit(Event{Type: EventFleetDisconnected, Payload: ConnectionEvent{Detail: err.Error()}})
		}
	}

	// Messaging
	if e.msgClient != nil {
		if e.msgClient.IsConnected() {
			if e.msgConnected.CompareAndSwap(false, true) {
				e.Events.Emit(Event{Type: EventMessagingConnected, Payload: ConnectionEvent{Detail: "messaging connected"}})
			}
		} else {
			if e.msgConnected.CompareAndSwap(true, false) {
				e.Events.Emit(Event{Type: EventMessagingDisconnected, Payload: ConnectionEvent{Detail: "messaging disconnected"}})
			}
		}
	}

	// Database
	if err := e.db.Ping(); err == nil {
		if e.dbConnected.CompareAndSwap(false, true) {
			e.Events.Emit(Event{Type: EventDBConnected, Payload: ConnectionEvent{Detail: "database connected"}})
		}
	} else {
		if e.dbConnected.CompareAndSwap(true, false) {
			e.Events.Emit(Event{Type: EventDBDisconnected, Payload: ConnectionEvent{Detail: err.Error()}})
		}
	}
}

func (e *Engine) connectionHealthLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopChan:
			return
		case <-ticker.C:
			e.checkConnectionStatus()
		}
	}
}

// captureDisconnectBaseline snapshots per-robot Available state from
// robotsCache into preDisconnectAvailability. Called from the
// disconnect-detect branch of checkConnectionStatus, before the
// EventFleetDisconnected emit, so the baseline reflects the last
// state we observed before any disconnect-driven side effects fire.
//
// Single-writer contract: only checkConnectionStatus (running on the
// connectionHealthLoop goroutine) writes preDisconnectAvailability.
// robotsCache is read under its existing RLock convention.
func (e *Engine) captureDisconnectBaseline() {
	e.robotsMu.RLock()
	baseline := make(map[string]bool, len(e.robotsCache))
	for id, r := range e.robotsCache {
		baseline[id] = r.Available
	}
	e.robotsMu.RUnlock()
	e.preDisconnectAvailability = baseline
}

// takeReconnectBaseline returns and clears the disconnect baseline
// captured at the most recent disconnect-detect transition. Returns
// nil when no prior disconnect has occurred (first connect after
// engine start) or when the baseline was already consumed.
//
// Single-shot consumer: the returned map is the only reference the
// caller will get; subsequent calls without an intervening disconnect
// return nil. Same single-writer thread as captureDisconnectBaseline,
// so no lock is needed on preDisconnectAvailability itself.
func (e *Engine) takeReconnectBaseline() map[string]bool {
	baseline := e.preDisconnectAvailability
	e.preDisconnectAvailability = nil
	return baseline
}

// autoResumeAfterFleetReconnect un-parks robots that RDS forced into
// undispatchable state during a fleet-disconnect window. RDS auto-
// parks every connected robot when its link drops; once RDS comes
// back the robots stay parked until something explicitly resumes
// them. Before this, every RDS blip (network hiccup, RDS restart)
// required a manual click per robot in the operator UI to bring them
// back online — otherwise they sat idle until someone noticed.
//
// Scope is intentionally narrow: a robot is only resumed when the
// baseline shows it was Available=true before the disconnect AND it
// is currently Available=false. Operator-paused robots
// (Available=false before the blip) stay paused; robots in legitimate
// fleet-side holds (low battery → typically Connected=false, etc.)
// aren't touched because their pre-disconnect Available was already
// false.
//
// baseline is captured synchronously at disconnect-detect (see
// captureDisconnectBaseline) and handed in here, so this function is
// a pure consumer of its argument — no hidden coupling to robotsCache
// or robotRefreshLoop's 2-second polling cadence. A nil/empty
// baseline (first connect after startup, or no robots tracked
// before the disconnect) is the explicit no-op signal: refuse to
// blanket-wake on cold boot.
//
// Failure modes log and continue. The baseline has already been
// consumed by the caller, so this is a single attempt per reconnect
// — operator UI is the manual fallback for any robot that didn't
// auto-resume.
func (e *Engine) autoResumeAfterFleetReconnect(baseline map[string]bool) {
	if len(baseline) == 0 {
		return
	}
	rl, ok := e.fleet.(fleet.RobotLister)
	if !ok {
		// Backend doesn't expose availability control (simulator, MiR, etc.).
		return
	}

	current, err := rl.GetRobotsStatus()
	if err != nil {
		e.logFn("engine: auto-resume after reconnect: fetch failed: %v", err)
		return
	}

	var resumed int
	for _, r := range current {
		wasAvailable, knownBefore := baseline[r.VehicleID]
		if !knownBefore || !wasAvailable {
			continue // never seen, or operator-paused before the blip
		}
		if r.Available {
			continue // already cleared (RDS didn't park, or someone else resumed)
		}
		if err := rl.SetAvailability(r.VehicleID, true); err != nil {
			e.logFn("engine: auto-resume robot %s: %v", r.VehicleID, err)
			continue
		}
		resumed++
		e.logFn("engine: auto-resumed robot %s after RDS reconnect", r.VehicleID)
	}
	if resumed > 0 {
		e.logFn("engine: fleet reconnect — auto-resumed %d robot(s) parked during disconnect", resumed)
	}
}
