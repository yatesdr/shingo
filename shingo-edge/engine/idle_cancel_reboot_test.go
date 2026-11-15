package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
)

// idle_cancel_reboot_test.go — the rule's off across an Edge reboot, and a pair
// that parks at a line already idle past its hour.

// reboot is this Edge starting again on the same database: a new engine, so
// nothing the rule kept in memory survives, its counter read once since boot.
func (l *idleLine) reboot(t *testing.T) {
	t.Helper()
	l.eng = testEngine(t, l.db)
	l.eng.stationHealth.onRead("PLC1", "COUNT", "")
	bridgeCore(t, l.eng, headOccupancyStub(t, false).URL)
	holdPlaces(t, l.eng, everyAs(false))
}

// noPartStamp moves the line's clock with no part, ago before now: the
// reading a counter reset to 0 writes (the sim's fake PLC does it at every
// Edge boot), or a point's first read.
func (l *idleLine) noPartStamp(t *testing.T, ago time.Duration) {
	t.Helper()
	_, err := l.db.DB.Exec(`UPDATE reporting_points SET last_poll_at=? WHERE plc_name='PLC1' AND tag_name='COUNT'`,
		time.Now().Add(-ago).UTC().Format("2006-01-02 15:04:05"))
	testutil.MustNoErr(t, err, "stamp the clock with no part")
}

// TestIdleCancel_TheRulesOffEndsOnARestartAcrossAReboot: the rule's off was
// written at a stopped line, so any part the line counts after it is a
// restart, whether the Edge was up to see the stop or not.
func TestIdleCancel_TheRulesOffEndsOnARestartAcrossAReboot(t *testing.T) {
	t.Parallel()
	sentBack := func(t *testing.T) *idleLine {
		t.Helper()
		l := newIdleLine(t, 3*time.Hour)
		l.addLegs(t, parked)
		l.eng.idleCancelLines()
		if got := l.hold(t); got != domain.ReorderHoldOff {
			t.Fatalf("setup: the position holds %q after the hour, want off", got)
		}
		return l
	}
	t.Run("the Edge restarts at a stopped line, which counts a part after the boot", func(t *testing.T) {
		t.Parallel()
		l := sentBack(t)
		l.reboot(t)
		l.eng.idleCancelLines()
		if got := l.hold(t); got != domain.ReorderHoldOff {
			t.Fatalf("the boot alone ended the off (%q); the line has not counted", got)
		}
		l.lastPart(t, -2*time.Second)
		l.eng.idleCancelLines()
		if got := l.hold(t); got != domain.ReorderHoldNone {
			t.Errorf("the line counted after the boot and the position holds %q, want nothing", got)
		}
	})
	t.Run("the same, with the counter's reset at boot stamping the clock with no part", func(t *testing.T) {
		t.Parallel()
		l := sentBack(t)
		l.reboot(t)
		l.noPartStamp(t, -time.Second)
		l.eng.idleCancelLines()
		if got := l.hold(t); got != domain.ReorderHoldOff {
			t.Fatalf("a reading with no part ended the off (%q)", got)
		}
		l.lastPart(t, -2*time.Second)
		l.eng.idleCancelLines()
		if got := l.hold(t); got != domain.ReorderHoldNone {
			t.Errorf("the line counted after the boot and the position holds %q, want nothing", got)
		}
	})
	t.Run("the line restarts while the Edge is down", func(t *testing.T) {
		t.Parallel()
		l := sentBack(t)
		l.lastPart(t, -2*time.Second) // what it made while the Edge was down, counted at the first poll after boot
		l.reboot(t)
		l.eng.idleCancelLines()
		if got := l.hold(t); got != domain.ReorderHoldNone {
			t.Errorf("the line had restarted when the Edge booted and the position holds %q, want nothing", got)
		}
	})
}

// TestIdleCancel_AReleaseCoreRejectedRestartsTheHour: mid-release is not a
// person's wait. A release Core rejects rolls the leg back from in_transit to
// staged (orders.RollbackReleaseRejection) with its intent cleared, so the
// leg reads as a robot waiting on a RELEASE again; the rollback is the staged
// mirror's write, which stamps staged_at, so the hour starts again from it
// (sim, Edge 1, 2026-11-14: PLN_523 and ALN_021 sent back while the sim
// operator pressed RELEASE every second and Core refused each).
func TestIdleCancel_AReleaseCoreRejectedRestartsTheHour(t *testing.T) {
	t.Parallel()
	l := newIdleLine(t, 3*time.Hour)
	ids := l.addLegs(t, parked, parked)
	l.parkedAgo(t, 2*time.Hour, ids...)
	_, err := l.db.DB.Exec(`UPDATE orders SET status=? WHERE id=?`, string(protocol.StatusInTransit), ids[0])
	testutil.MustNoErr(t, err, "a release sent")
	o, err := l.db.GetOrder(ids[0])
	testutil.MustNoErr(t, err, "read the leg")
	testutil.MustNoErr(t, l.eng.orderMgr.RollbackReleaseRejection(o.UUID, "the fleet did not take the release"), "Core rejects it")
	if got := l.status(t, ids[0]); got != protocol.StatusStaged {
		t.Fatalf("setup: the rejected leg is %s, want staged", got)
	}

	l.eng.idleCancelLines()
	for _, id := range ids {
		if got := l.status(t, id); got == protocol.StatusCancelled {
			t.Errorf("order %d was sent back a moment after a release Core rejected", id)
		}
	}
	if got := l.hold(t); got != domain.ReorderHoldNone {
		t.Errorf("the position holds %q, want nothing", got)
	}
}

// TestIdleCancel_APairThatParksAtALineIdleForHours: the hour does not start
// before the robots arrive. A pair arriving at a line silent for three hours
// is left for a full hour after it parks (the latest arrival in the group),
// then sent back. (Before the fourth anchor it was sent back as it parked.)
func TestIdleCancel_APairThatParksAtALineIdleForHours(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		first, second time.Duration // how long ago each leg's robot parked
		wantSentBack  bool
	}{
		{name: "both parked just now", wantSentBack: false},
		{name: "parked 59 minutes ago", first: 59 * time.Minute, second: 59 * time.Minute, wantSentBack: false},
		{name: "one parked 61 minutes ago, its partner 30", first: 61 * time.Minute, second: 30 * time.Minute, wantSentBack: false},
		{name: "both parked 61 minutes ago", first: 61 * time.Minute, second: 61 * time.Minute, wantSentBack: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := newIdleLine(t, 3*time.Hour)
			ids := l.addLegs(t, parked, parked)
			l.parkedAgo(t, tc.first, ids[0])
			l.parkedAgo(t, tc.second, ids[1])
			l.eng.reconcileDemand()
			for _, id := range ids {
				if got := l.status(t, id) == protocol.StatusCancelled; got != tc.wantSentBack {
					t.Errorf("order %d sent back = %v, want %v", id, got, tc.wantSentBack)
				}
			}
			want := domain.ReorderHoldNone
			if tc.wantSentBack {
				want = domain.ReorderHoldOff
			}
			if got := l.hold(t); got != want {
				t.Errorf("the position holds %q, want %q", got, want)
			}
		})
	}
}
