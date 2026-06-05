// engine_log.go — subsystem-tagged debug-log helpers on *Engine.
//
// Each helper routes to e.debugLogger.Log(subsystem, ...) so engine
// diagnostics (release, completion, changeover, side-cycle, warlink, ...)
// reach the ring buffer + browser debug log + stderr mirror in one call.
// Nil-safe: tests that build an Engine without a debug logger can call
// these helpers; they no-op silently.
//
// Why subsystem helpers instead of bare e.debugLogger.Log: keeps call
// sites short, makes the subsystem tag a compile-time constant (no
// string-typo drift across files), and gives the slog migration a
// single seam to swap when it lands.

package engine

func (e *Engine) logRelease(format string, args ...any) {
	if e.debugLogger != nil {
		e.debugLogger.Log("release", format, args...)
	}
}
