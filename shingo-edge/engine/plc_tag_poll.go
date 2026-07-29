package engine

import "time"

// plc_tag_poll.go — polling cadence and value coercion shared by the PLC tag
// monitors.
//
// These three symbols used to live in plc_cutover_monitor.go. That file
// implemented PLC-driven changeover completion by subscribing to a
// Changeover_Active tag; it was REMOVED because no plant ever wired that tag,
// so the feature could not fire at either Springfield or Hopkinsville
// (measured: 6 of 6 processes across both plants had auto_cutover_enabled=0).
// CATID auto-arm replaced it.
//
// The constants are named for what they DO rather than for the removed
// feature: they were `cutoverPollInterval` and `cutoverDebounce`, which would
// have left every remaining caller referring to a mechanism that no longer
// exists. Stale naming of exactly that kind is what made the two changeover
// controls read as one duplicated feature in the first place.

const (
	// plcPollInterval is how often a tag monitor re-reads its tracked
	// value from the WarLink cache. Cache freshness is driven by
	// WarLink's own SSE/poll cadence; this interval just controls how
	// quickly we react to a value change shingo already knows about.
	plcPollInterval = 500 * time.Millisecond

	// plcEdgeDebounce is how long a tag must hold a new value before a
	// monitor acts on it. PLC restarts and fault-recovery sequences can
	// toggle values briefly; this guards against single-tick flicker
	// driving a spurious state change.
	plcEdgeDebounce = 2 * time.Second
)

// plcTagInt64 coerces a WarLink tag value (any) to int64.
// WarLink delivers numeric tag values as float64 over JSON; PLC
// integer types come through that path. Falls back to int / int32 /
// int64 / float32 for completeness.
func plcTagInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float32:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}
