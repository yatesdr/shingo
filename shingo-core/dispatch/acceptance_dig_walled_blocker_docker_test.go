//go:build docker

package dispatch

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// acceptance_dig_walled_blocker_docker_test.go — a dig does not accuse the order
// its OWN lock is holding.
//
// ── THE FALSE ALARM THIS REMOVES ──────────────────────────────────────────
//
// claimantStopped is a pure age test on updated_at. An order refused at a lane
// gate is never touched, so its updated_at freezes and it ages into "stopped" no
// matter how healthy it is — and if the thing refusing it is THIS dig's own
// exclusive hold, the dig then files a stopped-blocker alarm against an order
// that is behaving correctly. dig-blocker-order-stopped sends an engineer to
// resolve the holder. There is nothing to resolve; the thing that has to move is
// the dig.
//
// ── WHY THE STALL IS PART OF THE FIXTURE AND NOT AN EXTRA ─────────────────
//
// The suppression is only worth anything on a claimant the stall window WOULD
// have accused. So every case here walks the claimant's updated_at back past the
// window first: without the walled check these orders get CauseDigBlockerStopped
// and an alarm row, which is exactly what the mutation below produces.
//
// The fixture is synthetic — it builds its own group, lane, bins and orders and
// never loads a plant spec.

// walledClaimant puts a claimant in the state the predicate reads: refused at a
// lane gate by an exclusive mouth hold, and stalled long enough that the age test
// would call it stopped. Both facts are written directly, because nothing in the
// ordinary lifecycle produces the pair on demand.
func walledClaimant(t *testing.T, db *store.DB, claimantID int64, cause QueueCause) {
	t.Helper()
	_, err := db.DB.Exec(`UPDATE orders SET queue_cause=$2 WHERE id=$1`, claimantID, string(cause))
	testutil.MustNoErr(t, err, "give the claimant its lane-gate refusal")
	stallClaimant(t, db, claimantID)
}

// TestSummonOwnDigs_AWalledBlockerIsNotAccused is the split §R.115a named in
// advance.
//
// MUTATION (run 2026-08-31, WALL): delete the blockerIsWalledByThisDig fork from
// parkOnClaimedBlocker. Every sub-test below flips to CauseDigBlockerStopped and
// files one stopped-blocker alarm against a healthy order — the false alarm,
// reproduced.
func TestSummonOwnDigs_AWalledBlockerIsNotAccused(t *testing.T) {
	t.Parallel()

	// BOTH REFUSAL SPELLINGS, because the predicate accepts either and a reader
	// should not have to check which one the fixture happened to pick.
	for _, cause := range []QueueCause{CauseLaneDigActive, CauseLaneHeldSource} {
		t.Run(string(cause), func(t *testing.T) {
			t.Parallel()
			db := testDBShared(t)
			d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

			tag := "WALLED-" + strings.ToUpper(strings.ReplaceAll(string(cause), "-", ""))
			lane, entry, wallBin, claimant := claimedWallFixture(t, db, tag)
			dweller := testdb.CreateOrder(t, db, func(o *orders.Order) {
				o.EdgeUUID = tag + "-dweller"
				o.Status = protocol.StatusStaged
			})
			walledClaimant(t, db, claimant.ID, cause)

			// THE DIG'S OWN LOCK IS THE WALL. This is the fact that separates this
			// population from a genuinely stopped holder: the excavation refusing
			// the claimant is the one being planned here.
			testutil.MustNoErr(t, reservations.AcquireLanes(db.DB, dweller.ID, reservations.ModeDig,
				reservations.ByExcavation, lane.ID), "take the excavation for the dweller")

			d.summonOwnDigs(lane, acceptanceRequest{order: dweller, entry: entry})

			after := reloadOrder(t, db, dweller.ID)

			// (a) THE CAUSE NAMES THE DEADLOCK, not a stalled holder. The two
			// causes carry different ACTIONS — one sends an engineer to the
			// holder, this one says the lane has to be released — so writing the
			// wrong one is not a wording problem.
			if after.QueueCause != string(CauseDigBlockerWaitsOnThisDig) {
				t.Fatalf("the wait is %q, want %q. Order %d holds bin %d and is refused with %q on "+
					"the very lane this dig holds: it is not stopped, it is walled, and an "+
					"engineer sent to resolve it finds nothing to fix.",
					after.QueueCause, CauseDigBlockerWaitsOnThisDig, claimant.ID, wallBin, cause)
			}

			// (b) NO ALARM AGAINST A HEALTHY ORDER. This is the whole point of the
			// split: the stall window would have fired here, and the row above is
			// what stops it.
			if filed := stoppedBlockerAlarms(t, db, claimant.ID); len(filed) != 0 {
				t.Errorf("%d stopped-blocker alarm(s) filed against order %d, which is WALLED BY "+
					"THIS DIG and behaving correctly. One confirmed false alarm is what re-opened "+
					"this split; filing it again is the defect returning. Details: %v",
					len(filed), claimant.ID, filed)
			}
		})
	}
}

// TestSummonOwnDigs_AnUnwalledStoppedBlockerIsStillAccused is the control, and it
// is the half that keeps the suppression honest.
//
// Returning true on a guess would hide a REAL stopped blocker and leave a genuine
// fault unreported — strictly worse than the false alarm being removed. So the
// predicate has to be narrow, and this pins the narrowness: same stalled claimant,
// same dig, but the claimant is not refused at a lane gate, so the alarm still
// fires.
func TestSummonOwnDigs_AnUnwalledStoppedBlockerIsStillAccused(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	lane, entry, _, claimant := claimedWallFixture(t, db, "UNWALLED")
	dweller := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "unwalled-dweller"
		o.Status = protocol.StatusStaged
	})
	// Stalled, and the dig holds the lane — but the claimant carries NO lane-gate
	// refusal, so nothing says this dig is why it stopped.
	stallClaimant(t, db, claimant.ID)
	testutil.MustNoErr(t, reservations.AcquireLanes(db.DB, dweller.ID, reservations.ModeDig,
		reservations.ByExcavation, lane.ID), "take the excavation for the dweller")

	d.summonOwnDigs(lane, acceptanceRequest{order: dweller, entry: entry})

	after := reloadOrder(t, db, dweller.ID)
	if after.QueueCause != string(CauseDigBlockerStopped) {
		t.Fatalf("the wait is %q, want %q — a stalled holder with no lane-gate refusal is the "+
			"population the stopped-blocker alarm exists for, and suppressing it here would "+
			"hide a real fault", after.QueueCause, CauseDigBlockerStopped)
	}
	if filed := stoppedBlockerAlarms(t, db, claimant.ID); len(filed) != 1 {
		t.Fatalf("%d stopped-blocker alarm(s) filed against order %d, want exactly 1. The walled "+
			"check must be NARROW: it removes a named false alarm, not the alarm.",
			len(filed), claimant.ID)
	}
}

// TestSummonOwnDigs_ADigDoesNotAccuseItsOwnFamily pins the exclusion the predicate
// carries for compounds: a child working inside its parent's dig is not walled by
// it, and reporting a deadlock there would hide a real stall inside a compound.
func TestSummonOwnDigs_ADigDoesNotAccuseItsOwnFamily(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	lane, entry, _, claimant := claimedWallFixture(t, db, "OWNFAMILY")
	dweller := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "ownfamily-dweller"
		o.Status = protocol.StatusStaged
	})
	walledClaimant(t, db, claimant.ID, CauseLaneDigActive)

	// THE EXCAVATION IS THE CLAIMANT'S OWN. excavator == claimantID, so the
	// deadlock reading is false: nothing external is walling it.
	testutil.MustNoErr(t, reservations.AcquireLanes(db.DB, claimant.ID, reservations.ModeDig,
		reservations.ByExcavation, lane.ID), "take the excavation for the claimant itself")

	d.summonOwnDigs(lane, acceptanceRequest{order: dweller, entry: entry})

	after := reloadOrder(t, db, dweller.ID)
	if after.QueueCause == string(CauseDigBlockerWaitsOnThisDig) {
		t.Fatalf("the wait is %q, but the excavation on %s belongs to order %d itself. A holder "+
			"inside its own dig is not walled by it, and naming a deadlock here would hide a "+
			"real stall inside a compound.", after.QueueCause, lane.Name, claimant.ID)
	}
}
