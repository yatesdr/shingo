package engine

import "shingocore/fleet"

// ── Live reconfiguration ────────────────────────────────────────────
//
// Reconfigure* methods are called from the www/ config-reload handler
// when the operator saves a settings change. Each one applies the
// current *config.Config to its subsystem and re-runs the connection
// health probe so the UI sees the transition immediately.

// ReconfigureDatabase reconnects the database with current config.
func (e *Engine) ReconfigureDatabase() {
	if err := e.db.Reconnect(&e.cfg.Database); err != nil {
		e.logFn("engine: database reconfigure error: %v", err)
	} else {
		e.logFn("engine: database reconfigured")
	}
	e.checkConnectionStatus()
}

// ReconfigureFleet applies fleet config changes live.
func (e *Engine) ReconfigureFleet() {
	e.fleet.Reconfigure(fleet.ReconfigureParams{
		BaseURL: e.cfg.RDS.BaseURL,
		Timeout: e.cfg.RDS.Timeout,
	})
	e.logFn("engine: fleet reconfigured (%s)", e.fleet.Name())
	e.checkConnectionStatus()
}

// ReconfigureCountGroups stops the current count-group runner and starts
// a new one with the latest config. Safe to call when the builder is nil
// (feature was never enabled) — it logs and returns.
func (e *Engine) ReconfigureCountGroups() {
	if e.countGroupBuild == nil {
		e.logFn("engine: count-group reconfigure skipped (no builder registered)")
		return
	}

	// Stop the old runner gracefully.
	if e.countGroup != nil {
		e.countGroup.Stop()
		e.logFn("engine: count-group runner stopped for reconfiguration")
	}

	// Build and start a fresh runner — the builder's closure reads
	// cfg.CountGroups at call time, so it picks up the new group list.
	e.countGroup = e.countGroupBuild(&countGroupEventEmitter{bus: e.Events})
	e.countGroup.Start()
	e.logFn("engine: count-group runner reconfigured (%d groups)", len(e.cfg.CountGroups.Groups))
}

// ReconfigureMessaging reconnects messaging with current config.
func (e *Engine) ReconfigureMessaging() {
	if err := e.msgClient.Reconfigure(&e.cfg.Messaging); err != nil {
		e.logFn("engine: messaging reconfigure error: %v", err)
	} else {
		e.logFn("engine: messaging reconfigured")
	}
	e.checkConnectionStatus()
}
