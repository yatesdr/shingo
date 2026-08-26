//go:build docker

package dispatch

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// acceptance_dig_liveness_docker_test.go — the LIVE pin for 2e, both halves.
//
// ── WHAT WAS WRONG ────────────────────────────────────────────────────────
//
// The acceptance arm's fact 3 was a bare `ClaimedBy == nil` (binIsUnclaimed),
// which reads IDENTICALLY for a robot driving the bin out and for an order that
// stopped moving an hour ago. §R.98 stage C had already ruled that question and
// built the answer on the dissolve path (54c60fee); the gate was left asking the
// pre-ruling version one file over. So a stale claim suppressed the dig forever:
// re-refused every pass, "somebody is coming" every pass, with a releaser that was
// not coming — and a gate-staged dweller is exempt from the abandon sweep by name,
// so nothing else in the plant ever ends that wait.
//
// ── AND WHAT THE FIX IS NOT ───────────────────────────────────────────────
//
// The repoint alone does NOT produce a dig, and a pin asserting one would be
// asserting a premise §R.114 falsified at the tree. Three layers sit under fact 3
// and only one of them moved: proposeLaneClearDig's plan-time binIsUnclaimed
// still refuses ANY claim (deliberately — R.30 measured JackUnload waits to 959s,
// so taking a hard claim off a live order is how you take a bin off a working
// robot), the claim CAS still refuses foreign claims, and ReleaseOrphanedClaims
// still sweeps TERMINAL claimants only.
//
// §R.115 ruled the disposition the missing layer needed, and it is not a machine
// one: A STOPPED ORDER IS A PERSON'S JOB. The wait is honest, it NAMES the stopped
// order, and it is LOUD — §R.45's shape, because a wait whose releaser is a human
// is worth nothing if the human cannot see it. So what these tests pin is: the
// gate can now TELL THE TWO APART (test 1), and each one gets the wait that is
// true about it (test 2).

// stallClaimant walks an order's updated_at back past the stall window without
// touching its status or its claim. That is the honest fixture for "stopped but
// not terminated": nothing in the ordinary lifecycle produces it, which is exactly
// why the bare read survived contact with every well-behaved order for so long.
func stallClaimant(t *testing.T, db *store.DB, claimantID int64) {
	t.Helper()
	stale := clock.Now().UTC().Add(-2 * claimantStallWindow)
	_, err := db.DB.Exec(`UPDATE orders SET updated_at=$2 WHERE id=$1`, claimantID, stale)
	testutil.MustNoErr(t, err, "stall the claimant")
}

// terminalizeClaimantLeavingItsClaim writes a terminal status directly and leaves
// the claim standing. TerminalizeOrder releases the claim in the same transaction,
// so the ordinary path cannot produce this row — but the state is real, which is
// why ReleaseOrphanedClaims exists at all, and between the two the gate is looking
// at a claim whose owner has ended.
func terminalizeClaimantLeavingItsClaim(t *testing.T, db *store.DB, claimantID int64) {
	t.Helper()
	_, err := db.DB.Exec(`UPDATE orders SET status='failed' WHERE id=$1`, claimantID)
	testutil.MustNoErr(t, err, "terminalize the claimant")
}

// claimedWallFixture builds the shape both tests need: a diggable lane in a group
// with somewhere to park blockers, one bin walling the entry, and an order holding
// that bin. Returns the lane, the entry slot behind the wall, the wall bin and the
// claimant.
func claimedWallFixture(t *testing.T, db *store.DB, tag string) (
	lane, entry *nodes.Node, wallBin int64, claimant *orders.Order) {
	t.Helper()
	laneNode, _, w, _, bp := clearLaneFixture(t, db, tag)
	bin := testdb.CreateBinAtNode(t, db, bp.Code, w[0].ID, tag+"-WALL")
	claimant = testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = tag + "-claimant"
		o.SourceNode = w[0].Name
		o.Status = protocol.StatusDispatched
	})
	testdb.ClaimBinForTest(t, db, bin.ID, claimant.ID)
	return laneNode, w[1], bin.ID, claimant
}

// stoppedBlockerAlarms counts the recovery-action rows filed against one accused
// order. It is a count and not a boolean on purpose: "the alarm fired" and "the
// alarm fires on every pass" are different findings, and the second one is the
// 38,203-row shape this house has already paid for once.
func stoppedBlockerAlarms(t *testing.T, db *store.DB, accused int64) []string {
	t.Helper()
	actions, err := db.ListRecoveryActions(200)
	testutil.MustNoErr(t, err, "read recovery actions")
	var filed []string
	for _, a := range actions {
		if a.Action == StoppedBlockerAction && a.TargetID == accused {
			filed = append(filed, a.Detail)
		}
	}
	return filed
}

// TestAcceptanceDig_TellsASlowClaimantFromAStoppedOne is the repoint itself: fact
// 3 asked through blockersSpokenFor rather than through a bare ClaimedBy read.
//
// It asserts the POSITIVE direction, which is the only direction that can tell the
// two spellings apart. Under the old read both a live claim and a dead one produce
// the same answer — "somebody is coming, do not dig" — so no assertion that only
// says "no dig" discriminates. What discriminates is that the arm now PROCEEDS
// when the claimant has stopped.
//
// MUTATION (re-driven this session, fires): restore the
// `for _, b := range blockers { if !binIsUnclaimed(b.bin) { … return false } }`
// loop in acceptanceDigNeeded. The "claimant has stopped" case flips to false and
// this test names it — the dweller waiting on a releaser that is not coming, which
// is the whole finding.
func TestAcceptanceDig_TellsASlowClaimantFromAStoppedOne(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		tag     string
		stop    func(t *testing.T, db *store.DB, id int64)
		wantDig bool
		because string
	}{
		{
			name:    "the claimant is moving, so somebody IS coming",
			tag:     "AL-LIVE",
			stop:    func(*testing.T, *store.DB, int64) {},
			wantDig: false,
			because: "a hard claim on a moving order means a robot is carrying that bin out. The " +
				"lane clears itself and an excavation would be racing a robot for a bin it is " +
				"already holding.",
		},
		{
			name:    "the claimant has stopped without terminating",
			tag:     "AL-STOP",
			stop:    stallClaimant,
			wantDig: true,
			because: "the claim's holder has not advanced inside the stall window, so it is not a " +
				"releaser. Reading it as one is what suppressed the dig forever (§R.98 stage C, " +
				"ruled on the dissolve path and left unapplied here until 2e).",
		},
		{
			name:    "the claimant has ended and left its claim behind",
			tag:     "AL-TERM",
			stop:    terminalizeClaimantLeavingItsClaim,
			wantDig: true,
			because: "a terminal order carries nothing out. Its claim is a leftover the orphan sweep " +
				"will clear, and until it does nobody is coming for that bin.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testDBShared(t)
			d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

			lane, entry, _, claimant := claimedWallFixture(t, db, tc.tag)
			dweller := testdb.CreateOrder(t, db, func(o *orders.Order) {
				o.EdgeUUID = tc.tag + "-dweller"
				o.Status = protocol.StatusStaged
			})
			tc.stop(t, db, claimant.ID)

			_, got := d.acceptanceDigNeeded(lane, gateCandidate{
				order: dweller, node: entry, retrieve: true,
			})
			if got != tc.wantDig {
				t.Fatalf("acceptanceDigNeeded = %v for a wall whose claimant %d is %q, want %v.\n%s",
					got, claimant.ID, tc.name, tc.wantDig, tc.because)
			}
		})
	}
}

// TestSummonOwnDigs_StoppedBlockerNamesTheOrderAndCallsAHuman is §R.115's ruling,
// end to end through the arm that writes the wait.
//
// The dig is refused either way — the plan-time pre-check will not take a hard
// claim from anybody, and that is deliberate. What the ruling changes is WHAT THE
// ORDER IS TOLD IT IS WAITING FOR. A live holder is congestion and stays quiet. A
// stopped holder has no machine releaser at all, so the wait names the order and a
// person is called, loudly, once.
//
// MUTATION (re-driven this session, fires): make parkOnClaimedBlocker write
// CauseDigBlockerClaimed unconditionally (delete the claimantStopped fork). The
// stopped sub-test fires on the cause, on the sentence naming the order, and on
// the missing alarm row — three assertions, because the finding has three halves:
// the operator is told the wrong releaser, the order id is not there to act on,
// and nobody is called.
func TestSummonOwnDigs_StoppedBlockerNamesTheOrderAndCallsAHuman(t *testing.T) {
	t.Parallel()

	t.Run("a moving claimant is ordinary congestion and stays quiet", func(t *testing.T) {
		t.Parallel()
		db := testDBShared(t)
		d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

		lane, entry, _, claimant := claimedWallFixture(t, db, "SB-LIVE")
		dweller := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = "sb-live-dweller"
			o.Status = protocol.StatusStaged
		})

		d.summonOwnDigs(lane, acceptanceRequest{order: dweller, entry: entry})

		after := reloadOrder(t, db, dweller.ID)
		if after.QueueCause != string(CauseDigBlockerClaimed) {
			t.Errorf("the wait is %q, want %q — a robot is carrying that bin out and the wait ends "+
				"when it does", after.QueueCause, CauseDigBlockerClaimed)
		}
		if n := stoppedBlockerAlarms(t, db, claimant.ID); len(n) != 0 {
			t.Errorf("%d stopped-blocker alarm(s) filed against order %d, which is MOVING. Calling "+
				"an engineer out for ordinary congestion is how an alarm stops being read at all "+
				"— and §R.115a's watch item is exactly this rate.", len(n), claimant.ID)
		}
	})

	t.Run("an ended claimant is the sweep's job, not a person's", func(t *testing.T) {
		t.Parallel()
		db := testDBShared(t)
		d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

		lane, entry, _, claimant := claimedWallFixture(t, db, "SB-TERM")
		dweller := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = "sb-term-dweller"
			o.Status = protocol.StatusStaged
		})
		terminalizeClaimantLeavingItsClaim(t, db, claimant.ID)

		d.summonOwnDigs(lane, acceptanceRequest{order: dweller, entry: entry})

		after := reloadOrder(t, db, dweller.ID)
		if after.QueueCause != string(CauseDigBlockerClaimed) {
			t.Errorf("the wait is %q, want %q — a claim left behind by a TERMINAL order has a "+
				"machine releaser (ReleaseOrphanedClaims), so it is not a person's job",
				after.QueueCause, CauseDigBlockerClaimed)
		}
		if n := stoppedBlockerAlarms(t, db, claimant.ID); len(n) != 0 {
			t.Errorf("%d stopped-blocker alarm(s) filed against order %d, which has ENDED. The "+
				"orphan sweep clears its claim on the next pass; calling a human out to watch a "+
				"sweep run is the false-alarm class §R.115a says to keep countable.",
				len(n), claimant.ID)
		}
	})

	t.Run("a stopped claimant names the order and calls a human, once", func(t *testing.T) {
		t.Parallel()
		db := testDBShared(t)
		d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

		lane, entry, wallBin, claimant := claimedWallFixture(t, db, "SB-STOP")
		dweller := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = "sb-stop-dweller"
			o.Status = protocol.StatusStaged
		})
		stallClaimant(t, db, claimant.ID)

		d.summonOwnDigs(lane, acceptanceRequest{order: dweller, entry: entry})

		after := reloadOrder(t, db, dweller.ID)
		// (a) THE WAIT IS THE TRUE ONE. The congestion cause promises "the order
		// holding the blocker finishes carrying it out", which for a stopped holder
		// is a releaser that cannot fire — and a wait naming the wrong releaser is
		// worse than one naming none.
		if after.QueueCause != string(CauseDigBlockerStopped) {
			t.Fatalf("the wait is %q, want %q. Order %d holds bin %d and has not moved for longer "+
				"than the stall window: no robot is coming, the claim CAS will not take it, and "+
				"the orphan sweep only reaches TERMINAL holders.",
				after.QueueCause, CauseDigBlockerStopped, claimant.ID, wallBin)
		}
		// (b) AND IT NAMES THE ORDER, which is the whole of what makes it
		// actionable — §R.115: with the order id in the sentence so nobody has to
		// go hunting.
		if want := fmt.Sprintf("order %d", claimant.ID); !strings.Contains(after.QueueReason, want) {
			t.Errorf("the operator sentence is %q and does not contain %q. A wait whose releaser "+
				"is a person has to say WHICH row that person opens.", after.QueueReason, want)
		}
		// (c) AND A HUMAN IS CALLED, LOUDLY — §R.45's shape, with the accused order
		// and its last-movement age in the row so a false alarm is one row to check
		// (§R.115a's named watch item).
		filed := stoppedBlockerAlarms(t, db, claimant.ID)
		if len(filed) != 1 {
			t.Fatalf("%d stopped-blocker alarm(s) filed against order %d, want exactly 1. A wait "+
				"whose releaser is a person is worth nothing if the person cannot see it.",
				len(filed), claimant.ID)
		}
		if !strings.Contains(filed[0], fmt.Sprintf("%d", claimant.ID)) {
			t.Errorf("the alarm does not name the accused order %d:\n  %s", claimant.ID, filed[0])
		}
		if !strings.Contains(filed[0], "has not moved for") {
			t.Errorf("the alarm does not carry how long the accused order has been standing "+
				"still:\n  %s\nThat number is what makes a false alarm cheap to dismiss, and "+
				"§R.115a accepted the false-alarm risk on exactly that basis.", filed[0])
		}

		// (d) AND IT DOES NOT FIRE AGAIN ON THE NEXT PASS. This arm re-runs on
		// every lane event; an alarm per pass would rebuild the 38,203-row
		// livelock's shape in the recovery table while the operator's wait — which
		// IS continuously visible — says the same thing all along.
		d.summonOwnDigs(lane, acceptanceRequest{order: reloadOrder(t, db, dweller.ID), entry: entry})
		if again := stoppedBlockerAlarms(t, db, claimant.ID); len(again) != 1 {
			t.Errorf("%d alarm(s) after a second identical pass, want still 1 — the alarm rides the "+
				"EDGE of the wait (setQueueReason's unchanged short-circuit), not the pass",
				len(again))
		}
	})
}

// reloadOrder re-reads an order, because every assertion here is about what was
// PERSISTED rather than about the in-memory copy the arm mutated.
func reloadOrder(t *testing.T, db *store.DB, id int64) *orders.Order {
	t.Helper()
	o, err := db.GetOrder(id)
	testutil.MustNoErr(t, err, "reload order")
	if o == nil {
		t.Fatalf("order %d vanished", id)
	}
	return o
}
