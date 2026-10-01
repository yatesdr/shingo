package engine

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// release_ledger_pins_test.go — the ledger commits with its effect (ruling 7).
//
// A crash between a release's effect and the ledger row that records it is
// stood in for by a trigger that aborts the ledger insert. If the two commit
// separately, the effect survives with no record, and the retry the operator
// makes applies it again. If they commit together, the crash leaves neither,
// and the retry applies it once.

// crashLedger arms a trigger that fails the ledger insert of one kind, as a
// process dying between the effect and its record would.
func crashLedger(kind string) func(h *relHarness) {
	return func(h *relHarness) {
		_, err := h.db.Exec(fmt.Sprintf(`CREATE TRIGGER crash_ledger BEFORE INSERT ON release_paperwork
			WHEN NEW.kind = '%s' BEGIN SELECT RAISE(ABORT, 'crash before the ledger row'); END`, kind))
		testutil.MustNoErr(h.t, err, "arm the crash")
	}
}

// crashThenRetry is the operator's release, crashed, then retried once the
// Edge is back: if the first attempt's envelope went out, Core refuses it (a manifest sync it could not make) and
// the leg is staged again; the trigger is gone (the restart); the operator
// presses RELEASE again with the same quantities.
func crashThenRetry(leg string, disp ReleaseDisposition) func(h *relHarness) error {
	return func(h *relHarness) error {
		_ = h.eng.ReleaseOrderWithLineside(h.leg(leg), disp)
		if h.order(leg).Status == protocol.StatusInTransit {
			h.coreRefuses(leg, "manifest_sync_failed")
		}
		_, err := h.db.Exec(`DROP TRIGGER crash_ledger`)
		testutil.MustNoErr(h.t, err, "restart")
		return h.eng.ReleaseOrderWithLineside(h.leg(leg), disp)
	}
}

func TestReleaseLedgerPins(t *testing.T) {
	t.Parallel()
	twoRobotConsume := pairSpec{mode: protocol.SwapModeTwoRobot, role: protocol.ClaimRoleConsume}
	twoRobot := pairSpec{mode: protocol.SwapModeTwoRobot}
	S, D := protocol.StatusStaged, protocol.StatusDispatched
	runRelCells(t, []relCell{
		// PULL PARTS 5: the pile gains 5 once, however the first attempt died.
		{name: "ledger/capture crashes before its record, then the release is retried",
			bug:   "ledger-capture",
			today: "ok | evac=in_transit supply=dispatched | rel=evac,evac | ingest=0 capred=0 | pile=10",
			want:  "ok | evac=in_transit supply=dispatched | rel=evac | ingest=0 capred=0 | pile=5",
			build: func(h *relHarness) {
				pairAt(twoRobotConsume, "evac", S, "supply", D)(h)
				crashLedger("capture")(h)
			},
			act:   crashThenRetry("evac", dispPull(5)),
			probe: func(h *relHarness) []string { return []string{fmt.Sprintf("pile=%d", h.pile(fxPart))} }},
		// The produce bin's ingest: shipped once, by the attempt that recorded
		// it; the crashed attempt ships nothing and releases nothing.
		{name: "ledger/ingest crashes before its record, then the release is retried",
			bug:   "ledger-ingest",
			today: "ok | evac=in_transit supply=dispatched | rel=evac,evac | ingest=1 capred=0 | uop=0",
			want:  "ok | evac=in_transit supply=dispatched | rel=evac | ingest=1 capred=0 | uop=0",
			build: func(h *relHarness) {
				pairAt(twoRobot, "evac", S, "supply", D)(h)
				crashLedger("ingest")(h)
			},
			act: crashThenRetry("evac", dispEmpty), probe: probes(pUOP)},
	})
}
