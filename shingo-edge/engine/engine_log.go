// engine_log.go — subsystem-tagged debug-log helpers on *Engine.
//
// Each helper routes to e.debugLogger.Log(subsystem, ...) so engine
// diagnostics (completion, release, changeover, side-cycle, warlink, ...)
// reach the ring buffer + browser debug log + stderr mirror in one call.
// Nil-safe: tests that build an Engine without a debug logger can call
// these helpers; they no-op silently.
//
// Why subsystem helpers instead of bare e.debugLogger.Log: keeps call
// sites short, makes the subsystem tag a compile-time constant (no
// string-typo drift across files), and gives the slog migration a
// single seam to swap when it lands.

package engine

func (e *Engine) logEngine(format string, args ...any) {
	if e.debugLogger != nil {
		e.debugLogger.Log("engine", format, args...)
	}
}

func (e *Engine) logCompletion(format string, args ...any) {
	if e.debugLogger != nil {
		e.debugLogger.Log("completion", format, args...)
	}
}

func (e *Engine) logRelease(format string, args ...any) {
	if e.debugLogger != nil {
		e.debugLogger.Log("release", format, args...)
	}
}

func (e *Engine) logChangeover(format string, args ...any) {
	if e.debugLogger != nil {
		e.debugLogger.Log("changeover", format, args...)
	}
}

func (e *Engine) logSideCycle(format string, args ...any) {
	if e.debugLogger != nil {
		e.debugLogger.Log("side_cycle", format, args...)
	}
}

func (e *Engine) logWarlink(format string, args ...any) {
	if e.debugLogger != nil {
		e.debugLogger.Log("warlink", format, args...)
	}
}

func (e *Engine) logBinPickup(format string, args ...any) {
	if e.debugLogger != nil {
		e.debugLogger.Log("bin_pickup", format, args...)
	}
}

func (e *Engine) logBackfill(format string, args ...any) {
	if e.debugLogger != nil {
		e.debugLogger.Log("backfill", format, args...)
	}
}

func (e *Engine) logReconcile(format string, args ...any) {
	if e.debugLogger != nil {
		e.debugLogger.Log("reconcile", format, args...)
	}
}

func (e *Engine) logDemand(format string, args ...any) {
	if e.debugLogger != nil {
		e.debugLogger.Log("demand", format, args...)
	}
}

func (e *Engine) logStation(format string, args ...any) {
	if e.debugLogger != nil {
		e.debugLogger.Log("station", format, args...)
	}
}
