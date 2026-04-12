package types

// DebugLogFunc is a nil-safe formatted debug log callback.
// A nil DebugLogFunc is safe to call via the Log method.
type DebugLogFunc func(format string, args ...any)

// Log calls the debug log function if non-nil. Safe to call on a nil receiver.
func (fn DebugLogFunc) Log(format string, args ...any) {
	if fn != nil {
		fn(format, args...)
	}
}
