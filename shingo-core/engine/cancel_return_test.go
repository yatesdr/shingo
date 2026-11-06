package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/store/orders"
)

// The six producers of `cancelled`, each as the terminal row it actually
// writes (dispatch/lifecycle.go CancelOrder and its callers), through the one
// predicate that decides them. The owner's ruling is that every one returns:
// cancelled is cancelled, and the trigger's physical gates are the safety. A
// new producer is a new row here.
func TestReturnEligible_TheProducers(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	window := 2 * time.Hour
	cancelled := &orders.Order{ID: 7, Status: protocol.StatusCancelled}
	row := func(code protocol.TermCode, detail string, age time.Duration) *orders.History {
		return &orders.History{Status: protocol.StatusCancelled, Code: string(code), Detail: detail, CreatedAt: now.Add(-age)}
	}
	cases := []struct {
		producer string
		hist     *orders.History
		want     bool
	}{
		{"person / station terminate", row(protocol.TermOperatorCancelled, "cancelled by operator", time.Minute), true},
		{"compound child of a person's cancel", row(protocol.TermOperatorCancelled, "parent order cancelled: x", time.Minute), true},
		{"fleet stop", row("", "fleet order stopped", time.Minute), true},
		{"reconciliation abandon", row("", "abandoned: stuck in in_transit past 30m0s", time.Minute), true},
		{"abandon of a child leg", row("", "reshuffle dissolved: a dig leg failed; the demand re-plans", time.Minute), true},
		{"reshuffle dissolve", row("", "reshuffle dissolved: the dig's plan went stale; re-planning", time.Minute), true},
		{"reshuffle leg failed", row("", "reshuffle dissolved: a dig leg failed; the demand re-plans", time.Minute), true},
		{"swap-peer cascade", row(protocol.TermPeerTerminal, "coordinated swap peer failed; cancelling", time.Minute), true},
		{"a cancel whose code the telemetry calls a failure", row(protocol.TermGraceTimeout, "grace timeout", time.Minute), true},
		{"a cancel older than the window", row(protocol.TermOperatorCancelled, "cancelled by operator", 3*time.Hour), false},
		{"no terminal row", nil, false},
	}
	for _, c := range cases {
		if got, why := returnEligible(cancelled, c.hist, window, now); got != c.want {
			t.Errorf("%s: eligible=%v (%s), want %v", c.producer, got, why, c.want)
		}
	}
	for _, st := range []protocol.Status{protocol.StatusFailed, protocol.StatusSkipped, protocol.StatusConfirmed} {
		o := &orders.Order{ID: 8, Status: st}
		if ok, _ := returnEligible(o, &orders.History{Status: st, CreatedAt: now}, window, now); ok {
			t.Errorf("a %s order's bin was eligible — only a cancelled order's bin is returned", st)
		}
	}
}

// TestStuckContext pins what a stuck-sweep abandon adds after "abandoned: …":
// the fleet's last state, the robot, the wait it stood under — or nothing.
func TestStuckContext(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		o    orders.Order
		want string
	}{
		{orders.Order{VendorState: "WAITING", RobotID: "AMR-06", QueueReason: "Waiting for a slot"},
			" (RDS WAITING, AMR-06; last wait: Waiting for a slot)"},
		{orders.Order{VendorState: "CREATED"}, " (RDS CREATED)"},
		{orders.Order{QueueReason: "Waiting for a slot"}, " (last wait: Waiting for a slot)"},
		{orders.Order{}, ""},
	} {
		if got := stuckContext(&c.o); got != c.want {
			t.Errorf("stuckContext(%+v) = %q, want %q", c.o, got, c.want)
		}
	}
}
