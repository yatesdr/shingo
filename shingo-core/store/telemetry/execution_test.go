package telemetry

import (
	"strings"
	"testing"
)

// TestExecutionMSExpr_Structure pins the execution-time SQL builder: it must
// reference the given alias's core_completed/order_id and key the assignment
// off the acknowledged/in_transit events. Guards against an fmt.Sprintf arg
// slip and documents the Q-031 assignment-state choice.
func TestExecutionMSExpr_Structure(t *testing.T) {
	got := executionMSExpr("mt")
	for _, want := range []string{
		"mt.order_id",
		"order_history",
		"MIN(oh.created_at)",
		"'acknowledged'", // assignment endpoint
		"'in_transit'",
		"'delivered'", // completion endpoint
		"EXTRACT(EPOCH FROM",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("executionMSExpr(\"mt\") missing %q in:\n%s", want, got)
		}
	}
	// A different alias must propagate to the correlated subqueries.
	if a := executionMSExpr("q"); strings.Count(a, "q.order_id") < 2 {
		t.Errorf("alias not propagated to both subqueries: %s", a)
	}
}
