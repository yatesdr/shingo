//go:build docker

package main

import (
	"fmt"
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

// TestArmoredWithNoVendorOrder proves the crash sliver's instrument, and proves
// it is selective — the whole value of the check is that it is the one question
// nothing else in this file, or in Core, asks.
//
// Every other dispatched-family check filters for a NON-EMPTY vendor_order_id, correctly
// for what it measures. So the empty case had no reader at all: the poller's
// re-registration selects non-empty ids only, which means an order in this state
// is watched by nothing. Core writes `dispatched` and then creates the vendor
// order — the racing-dispatchers lock, kept deliberately — so a crash inside
// that window leaves an order armored, claimed, holding its lane, and silent.
//
// Three rows: the wedge, a fresh one inside the window that must NOT be reported
// (or the check fires on every ordinary dispatch), and a normal armored order
// with a vendor id.
func TestArmoredWithNoVendorOrder(t *testing.T) {
	t.Parallel()
	testdb.DisableWedgeSweep(t, "this fixture BACKDATES a `dispatched` order with no vendor id on purpose — that state is what the sweep under test is for, so the crash-sliver clause is correctly reporting the thing being arranged")
	db := testdb.Open(t)

	wedged := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "dispatched" })
	if _, err := db.Exec(
		`UPDATE orders SET vendor_order_id = '', updated_at = now() - interval '30 minutes' WHERE id = $1`,
		wedged.ID); err != nil {
		t.Fatalf("seed the wedge: %v", err)
	}

	fresh := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "dispatched" })
	if _, err := db.Exec(
		`UPDATE orders SET vendor_order_id = '', updated_at = now() WHERE id = $1`, fresh.ID); err != nil {
		t.Fatalf("seed the in-window order: %v", err)
	}

	healthy := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "dispatched" })
	if _, err := db.Exec(
		`UPDATE orders SET vendor_order_id = 'RDS-1', updated_at = now() - interval '30 minutes' WHERE id = $1`,
		healthy.ID); err != nil {
		t.Fatalf("seed the healthy order: %v", err)
	}

	v := checkInvariants(db)

	if !hasLine(v, "ARMORED, NO FLEET ORDER") {
		t.Fatalf("an order dispatched half an hour ago with an empty vendor_order_id was NOT reported. "+
			"It holds its claims and its lane and nothing polls it — the poller's re-registration "+
			"selects non-empty ids only.\n%s", strings.Join(v, "\n"))
	}
	for _, l := range v {
		if !strings.Contains(l, "ARMORED, NO FLEET ORDER") {
			continue
		}
		if strings.Contains(l, fmt.Sprintf("order %d ", fresh.ID)) {
			t.Errorf("an order dispatched a moment ago was reported. Inside the create window that "+
				"state is correct and expected, and a check that fires on every ordinary dispatch "+
				"says nothing: %s", l)
		}
		if strings.Contains(l, fmt.Sprintf("order %d ", healthy.ID)) {
			t.Errorf("an order the fleet actually took was reported: %s", l)
		}
	}
}
