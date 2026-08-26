//go:build docker

package main

import (
	"strings"
	"testing"

	"shingocore/dispatch"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

func hasLine(lines []string, substr string) bool {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// TestFloorHistogramAndUndeclaredWaits proves the SOAK'S INSTRUMENTS before the
// soak leans on them.
//
// The multi-hour run is where the tripwire count is supposed to trend to zero
// and where an undeclared (status, cause) pair would surface. That reading is
// only worth having if the reader is right — and this repo has been burned
// exactly there twice: soakstat counted burial-shadow OCCURRENCES and reported
// one bypass as "322" on a line labelled a tripwire, and its stall populations
// were hand-listed so `pending` and `sourcing` were watched by nothing. A
// measurement bug in a soak tool is worse than no tool, because the number is
// believed.
//
// So both new checks are driven against real rows: one floor release under a
// known cause, one under the absence-class cause, and one order parked under a
// cause the inventory does not describe.
func TestFloorHistogramAndUndeclaredWaits(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	// Two floor releases the tripwire must separate: an ordinary missing-emitter
	// case and the absence-class one that is expected by design.
	mustRecord(t, db, 101, string(dispatch.CauseLaneDigActive))
	mustRecord(t, db, 102, string(dispatch.CauseLaneDigActive))
	mustRecord(t, db, 103, string(dispatch.CauseFleetRefusedCreate))

	// An order parked under a cause no causeReleasers row covers. On the rig this
	// is the shape that matters: a wait the plant produces and the inventory does
	// not describe is a hold class nobody designed a way out of.
	undeclared := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = "queued"
	})
	if _, err := db.Exec(`UPDATE orders SET queue_cause = $1 WHERE id = $2`,
		"invented-wait", undeclared.ID); err != nil {
		t.Fatalf("seed undeclared cause: %v", err)
	}

	// A DECLARED cause on another order, so the check is proven to be selective
	// rather than flagging everything it sees.
	declared := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = "queued"
	})
	if _, err := db.Exec(`UPDATE orders SET queue_cause = $1 WHERE id = $2`,
		string(dispatch.CauseNoShuffleSlot), declared.ID); err != nil {
		t.Fatalf("seed declared cause: %v", err)
	}

	v := checkInvariants(db)

	// ── the histogram ──────────────────────────────────────────────────────
	if !hasLine(v, "FLOOR-RELEASE: 2 order(s)") || !hasLine(v, string(dispatch.CauseLaneDigActive)) {
		t.Errorf("the floor histogram did not report 2 releases under %s.\n%s",
			dispatch.CauseLaneDigActive, strings.Join(v, "\n"))
	}
	// The absence-class cause is reported APART. Folding it in would make the
	// tripwire look dirty on a plant whose only problem is a busy fleet, and
	// suppressing it would hide the gap exactly when it starts to matter.
	if !hasLine(v, "FLOOR-RELEASE (expected, absence-class)") {
		t.Errorf("the fleet-refusal release was not separated from the emitter gaps — it has no "+
			"event by design, so counting it as a missing emitter makes the worklist wrong.\n%s",
			strings.Join(v, "\n"))
	}

	// ── observed vs declared ───────────────────────────────────────────────
	if !hasLine(v, "UNDECLARED WAIT") || !hasLine(v, "invented-wait") {
		t.Errorf("an order parked under a cause with no causeReleasers row was NOT reported. That "+
			"is a wait the plant produces and the inventory does not describe — the F-22 shape "+
			"found from the other end.\n%s", strings.Join(v, "\n"))
	}
	for _, l := range v {
		if strings.Contains(l, "UNDECLARED WAIT") && strings.Contains(l, string(dispatch.CauseNoShuffleSlot)) {
			t.Errorf("a DECLARED cause was reported as undeclared — the check flags everything and "+
				"therefore says nothing: %s", l)
		}
	}
}

func mustRecord(t *testing.T, db *store.DB, orderID int64, cause string) {
	t.Helper()
	detail := `freed by the lane liveness floor, not by an event (gate-staged, lane 1, cause "` +
		cause + `"). the event that should have ended it: something`
	if err := db.RecordRecoveryAction(dispatch.FloorReleaseAction, "order", orderID, detail, "system"); err != nil {
		t.Fatalf("seed floor-release record: %v", err)
	}
}
