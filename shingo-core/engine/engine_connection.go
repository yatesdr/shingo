package engine

import "time"

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
			e.Events.Emit(Event{Type: EventFleetConnected, Payload: ConnectionEvent{Detail: e.fleet.Name() + " connected"}})
			go func() {
				total, created, deleted, err := e.SceneSync()
				if err != nil {
					e.logFn("engine: auto scene sync: %v", err)
					return
				}
				e.logFn("engine: auto scene sync: %d points, created %d, deleted %d nodes", total, created, deleted)
			}()
		}
	} else {
		if e.fleetConnected.CompareAndSwap(true, false) {
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
