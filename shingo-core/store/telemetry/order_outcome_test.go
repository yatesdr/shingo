package telemetry

import (
	"strings"
	"testing"
	"time"
)

// TestOrderOutcomeWhere_Structure pins the orders-outcome WHERE builder (no DB):
// the terminal-status guard is always present, the window is on
// COALESCE(completed_at, updated_at) — NOT mission_telemetry's core_completed —
// and the alias prefixes every column so the LATERAL-join cancel-origin query
// disambiguates against order_history. Placeholders count up with the args.
func TestOrderOutcomeWhere_Structure(t *testing.T) {
	// Unaliased, no filters: just the terminal-status guard, no args.
	where, args := orderOutcomeWhere("", Filter{})
	if !strings.Contains(where, "status IN ('confirmed','failed','cancelled','canceled','skipped')") {
		t.Errorf("missing terminal-status guard:\n%s", where)
	}
	if len(args) != 0 {
		t.Errorf("args = %d, want 0 for an unfiltered query", len(args))
	}

	// Aliased, fully filtered: alias prefixes every column, window on the
	// COALESCE terminal timestamp, $1..$3 in order (station, since, until).
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	where, args = orderOutcomeWhere("o", Filter{StationID: "S", Since: &since, Until: &until})
	for _, want := range []string{
		"o.status IN (",
		"o.station_id=$1",
		"COALESCE(o.completed_at, o.updated_at) >= $2",
		"COALESCE(o.completed_at, o.updated_at) <= $3",
	} {
		if !strings.Contains(where, want) {
			t.Errorf("missing %q in:\n%s", want, where)
		}
	}
	if len(args) != 3 {
		t.Errorf("args = %d, want 3 (station, since, until)", len(args))
	}
	if strings.Contains(where, "core_completed") {
		t.Errorf("must window on orders timestamp, not mission_telemetry's core_completed:\n%s", where)
	}

	// RobotID adds its own condition (scopes to orders that reached a robot).
	_, args = orderOutcomeWhere("", Filter{RobotID: "AMR-01"})
	if len(args) != 1 {
		t.Errorf("RobotID arg not added: args = %d, want 1", len(args))
	}
}
