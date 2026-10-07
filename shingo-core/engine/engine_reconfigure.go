package engine

import (
	"shingocore/fleet"
)

// ── Live reconfiguration ────────────────────────────────────────────
//
// The config page's save door (www/handlers_config.go) calls these after
// the file is written and the saved draft is swapped into the live
// *config.Config. Each Reconfigure* applies the live config to its subsystem;
// an error means the file was saved but the subsystem did not take it, and the
// door reports it in its `failed` list. The database has no Reconfigure: its
// settings apply after a restart (R27). The lane lock and the ETA cache hold
// the *sql.DB they were built with, so swapping the pool under a running Core
// left them on a closed one. The door pings the draft before it writes and
// nothing swaps a live pool.

// ReconfigureFleet applies fleet config changes live.
func (e *Engine) ReconfigureFleet() error {
	e.fleet.Reconfigure(fleet.ReconfigureParams{
		BaseURL:    e.cfg.RDS.BaseURL,
		Timeout:    e.cfg.RDS.Timeout,
		FaultGrace: e.cfg.RDS.FaultGrace,
	})
	e.logFn("engine: fleet reconfigured (%s)", e.fleet.Name())
	e.checkConnectionStatus()
	return nil
}

// ReconfigureMessaging reconnects messaging with current config.
func (e *Engine) ReconfigureMessaging() error {
	err := e.msgClient.Reconfigure(&e.cfg.Messaging)
	if err != nil {
		e.logFn("engine: messaging reconfigure error: %v", err)
	} else {
		e.logFn("engine: messaging reconfigured")
	}
	e.checkConnectionStatus()
	return err
}

// ReconfigureNotifications reloads the notifier with current config.
func (e *Engine) ReconfigureNotifications() error {
	if e.notifier == nil {
		e.logFn("engine: notifications reconfigure skipped (no notifier)")
		return nil
	}
	e.notifier.Reconfigure(&e.cfg.Notifications)
	e.logFn("engine: notifications reconfigured")
	return nil
}
