//go:build docker

package engine

import (
	"testing"
	"time"

	"shingo/protocol"
)

// threshold_sweep_no_orders_test.go — A RECONCILING SWEEP KEEPS THE RECORD
// STRAIGHT. IT NEVER CREATES AN ORDER AND IT NEVER EVALUATES A LEVEL.
//
// reconcileThresholdBindings' own doc says the level is not its business: "THE
// PRECONDITION IS THE BINDING, NOT THE LEVEL", because the rising edge in
// checkBindings owns the level, runs on every delta and says `recovered`. A
// sweep that also read the total would be a second opinion on a question that
// already has an answer; ordering off that total is the same mistake with
// material attached. It was briefly possible — the sweep once rebuilt the
// monitor's binding memory through the same function the notification doors
// use to evaluate — and the property is pinned so it cannot come back.

// One payload, two places. The withdrawn one has an open episode and no
// registry row; the live one is registered, below threshold and never
// evaluated. The sweep closes the first and does nothing at all about the
// second — no episode, no order — with the authoritative total both readable
// and not.
func TestThresholdSweep_ClosesTheWithdrawnAndOrdersForNothing(t *testing.T) {
	t.Parallel()
	for _, readable := range []bool{true, false} {
		name := "total_readable"
		if !readable {
			name = "total_unreadable"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRTRig(t, "PANEL-SWN-"+name, 40)

			// The withdrawn place: an episode whose binding exists nowhere.
			withdrawn := r.b
			withdrawn.coreNodeName = "SLN_GONE"
			r.m.checkBindings([]thresholdEntry{withdrawn}, 40, "below_threshold")
			if len(r.openRows()) != 1 {
				t.Fatal("setup: the withdrawn place's episode did not open")
			}
			firesBefore := len(r.fired())
			r.clock.advance(time.Hour) // every debounce long expired

			sweep := func() { r.eng.reconcileDemandEpisodes() }
			if readable {
				sweep()
			} else {
				withTableHidden(t, r.eng.db, "lineside_buckets", sweep)
			}

			rows := r.rows()
			if len(rows) != 1 {
				t.Fatalf("%d episodes after the sweep, want 1 — the sweep opened one for the live place", len(rows))
			}
			if rows[0].open || rows[0].closeReason != protocol.CloseReasonThresholdRemoved {
				t.Errorf("withdrawn episode open=%v reason=%q, want closed %q", rows[0].open, rows[0].closeReason,
					protocol.CloseReasonThresholdRemoved)
			}
			if got := len(r.fired()) - firesBefore; got != 0 {
				t.Errorf("the sweep made %d replenishment decision(s), want 0 — a sweep never orders", got)
			}
		})
	}
}
