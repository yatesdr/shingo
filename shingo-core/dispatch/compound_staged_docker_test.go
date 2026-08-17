//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// compound_staged_docker_test.go — the §R.104 shape at the moment its chapter
// stops short.
//
// A staged parent OWNS ITS OWN DIGS, so its dig legs failing is a fact about the
// demand's own excavation, not about the demand. The fork in
// AdvanceCompoundOrder had two dispositions — Queue and Cancel — and both are
// wrong here: `{staged → queued}` is refused by the state machine (the old arm
// ran, errored, and left the row stranded at the mark), and Cancel ends a demand
// whose own work is not what failed (§R.91).
//
// These tests pin the third disposition — NO TRANSITION, a cause, the lane kept
// while it is still owed one (§R.105 item 7) — and the recovery that follows:
// the evaluator re-asks, and either raises a replacement chapter or appends the
// tail where the robot stands.

// failADigLeg ends a leg the way the fleet would — terminal FAILED with a
// non-config detail — and closes the chapter through the real failure entry, so
// the fork is reached by the machinery that reaches it in production.
func failADigLeg(t *testing.T, db *store.DB, d *Dispatcher, parentID int64) {
	t.Helper()
	legID := openLeg(t, db, parentID)
	ok, err := db.TerminalizeOrder(legID, protocol.StatusFailed, "robot fault mid-dig (test)")
	testutil.MustNoErr(t, err, "terminalize the failed leg")
	if !ok {
		t.Fatalf("leg %d was already terminal — the fixture did not produce an open chapter", legID)
	}
	d.HandleChildOrderFailure(parentID, legID)
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parentID), "the disposition fork")
}

// TestStagedParent_LosesADigLeg_ParksAtTheMark is the whole disposition in one
// sequence: the dweller digs its own lane, a leg dies, and the parent must take
// NO transition, wear the failed-dig cause, keep the lane it still owes a drop
// to, and append nothing — then, when the lane is re-asked with the wall still
// standing, raise a replacement chapter rather than a tail.
//
// MUTATION (verified): delete the staged arm in the fork, restoring the old
// Queue-then-unlock. The cause assertion fires first (the row keeps the summon's
// stale staged-own-dig — "my dig is running", which just stopped being true) —
// and the lane assertion fires with it: unlockLaneForCompound drops the corridor
// the robot is standing in.
//
// MUTATION (verified): keep the arm but drop its per-lane decider split (fall
// through to unlockLaneForCompound). Only the lane assertion fires — the
// disposition was right and the release was wrong, which is §R.105 item 7 as a
// test.
func TestStagedParent_LosesADigLeg_ParksAtTheMark(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	wall, dweller, _ := stageDwellerBehindAWall(t, db, d, "STG-LEG")

	// The dweller summons its own dig and stands at the mark behind it.
	d.EvaluateLaneReleases(wall.ID)
	if !d.hasOpenDigChapter(dweller.ID) {
		t.Fatal("the dweller did not summon a dig — this test is about that chapter failing")
	}
	if n := appendsTo(backend, dweller.VendorOrderID); n != 0 {
		t.Fatalf("the dweller received %d tail append(s) with its chapter open", n)
	}

	failADigLeg(t, db, d, dweller.ID)

	after, err := db.GetOrder(dweller.ID)
	testutil.MustNoErr(t, err, "reload the dweller")
	if after.Status != protocol.StatusStaged {
		t.Fatalf("parent is %q, want `staged` — a staged parent's successors are all transitions, "+
			"and its dig failing is congestion, not the demand's own failure (§R.91)", after.Status)
	}
	if !IsGateStaged(after) {
		t.Fatalf("parent left the gate-staged shape as %q — the robot is at the mark and the plan "+
			"is still the plan", after.Status)
	}
	if got := QueueCause(after.QueueCause); got != CauseStagedDigFailed {
		t.Fatalf("parent's cause is %q, want %q — the row must say the dig STOPPED, not (as the "+
			"summon's stale cause would) that it is running", got, CauseStagedDigFailed)
	}
	if after.QueueReason == "" {
		t.Fatal("the parent carries a cause and no sentence — the board renders the sentence")
	}
	if !d.laneLock.IsLocked(wall.ID) {
		t.Fatal("the lane lock dropped when the chapter failed — the parent still owes this lane a " +
			"drop (§R.105 item 7: the lock survives chapter close into the append), and releasing it " +
			"readmits traffic into the corridor the robot is standing in")
	}
	if n := appendsTo(backend, dweller.VendorOrderID); n != 0 {
		t.Fatalf("the dweller received %d tail append(s) after its dig failed — the robot is parked "+
			"at the mark pending the re-ask, not released into a lane nothing is digging", n)
	}

	// ── THE RE-ASK. The wall never moved, so the honest answer is another dig,
	// not a tail. The failed chapter's legs are terminal, so the open-chapter
	// skip does not hold the dweller back and the episode gate is clear (its
	// non-terminal-child test finds the dead chapter terminal).
	d.EvaluateLaneReleases(wall.ID)
	if !d.hasOpenDigChapter(dweller.ID) {
		t.Fatal("the lane was re-asked with the wall still standing and raised no replacement " +
			"chapter — a parked staged parent with no re-ask is the wedge this cause exists to name")
	}
	fresh, err := db.GetOrder(dweller.ID)
	testutil.MustNoErr(t, err, "reload after the re-ask")
	if got := QueueCause(fresh.QueueCause); got != CauseStagedOwnDig {
		t.Fatalf("parent's cause is %q after the re-summon, want %q — the wait flipped back to "+
			"\"my dig is running\" the moment a new chapter opened", got, CauseStagedOwnDig)
	}
	if n := appendsTo(backend, dweller.VendorOrderID); n != 0 {
		t.Fatalf("the dweller received %d tail append(s) while its replacement chapter runs", n)
	}
}

// TestStagedParent_StaleChapter_WatchdogSeesIt pins the widening: a staged
// parent with an open leg is inside the stalled-chapter population, and a live
// mission on the leg makes it WAIT VISIBLY — not dissolve.
//
// Before the widening this row was invisible to the watchdog twice over: the SQL
// selected `reshuffling` only, and the dissolver refused the parent as "being
// torn down". A staged chapter gone quiet had no reader at all.
//
// MUTATION (verified): revert the SQL to `p.status = 'reshuffling'`. The sweep
// returns all zeroes and the "one waiting" assertion fires.
func TestStagedParent_StaleChapter_WatchdogSeesIt(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	wall, dweller, _ := stageDwellerBehindAWall(t, db, d, "STG-WATCH")
	d.EvaluateLaneReleases(wall.ID)
	if !d.hasOpenDigChapter(dweller.ID) {
		t.Fatal("the dweller did not summon a dig")
	}

	// A live mission on the open leg: the fleet holds it, RUNNING.
	legID := openLeg(t, db, dweller.ID)
	testutil.MustNoErr(t, db.UpdateOrderVendor(legID, "sg-stgw-1", "RUNNING", "AMR-07"), "hand the leg to the fleet")
	_, err := db.DB.Exec(`UPDATE orders SET status='in_transit' WHERE id=$1`, legID)
	testutil.MustNoErr(t, err, "the fleet took it")

	// A staged parent whose legs are all terminal is NOT in the population —
	// the widened SQL must not swallow the whole status, only the gate-staged
	// half. Asserted by construction: this fixture has an open leg.
	quiesce(t, db, dweller.ID)
	r := d.SweepStalledChapters()
	if r.Waiting != 1 || r.Dissolved != 0 || r.Residue != 0 {
		t.Fatalf("watchdog returned %+v, want one waiting — a staged parent's chapter is a chapter; "+
			"a live mission on its leg outranks every Core-side clock (R.30)", r)
	}
	after, err := db.GetOrder(dweller.ID)
	testutil.MustNoErr(t, err, "reload the dweller")
	if got := QueueCause(after.QueueCause); got != CauseChapterLegInFlight {
		t.Fatalf("parent's cause is %q, want %q — the visible-wait arm must stamp the staged row "+
			"too, or the board renders a parked robot with no reason", got, CauseChapterLegInFlight)
	}
	if after.Status != protocol.StatusStaged {
		t.Fatalf("the watchdog moved the parent to %q — its arms move legs and stamp causes, never "+
			"the parent's status", after.Status)
	}
}

// TestStagedParent_DissolverNotRefused pins the carve-out at the dissolver
// itself. The old guard read "not `reshuffling`" as "being torn down", which was
// true while every parent passed through that status — a staged parent never
// did, so the ONE dissolver refused the one shape whose only way out of a stale
// plan IS a dissolve: the evaluator will not touch a parent whose chapter is
// open, and nothing else closes chapters.
//
// MUTATION (verified): restore `parent.Status != StatusReshuffling` without the
// IsGateStaged carve-out. The dissolve becomes a no-op, no leg carries the
// marker, and the assertion fires.
func TestStagedParent_DissolverNotRefused(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	wall, dweller, _ := stageDwellerBehindAWall(t, db, d, "STG-DISS")
	d.EvaluateLaneReleases(wall.ID)
	if !d.hasOpenDigChapter(dweller.ID) {
		t.Fatal("the dweller did not summon a dig")
	}

	testutil.MustNoErr(t, d.dissolveCompound(dweller.ID, "test: the plan went stale"), "the dissolve")

	if n := dissolvedLegCount(t, db, dweller.ID); n == 0 {
		t.Fatal("no leg carries the dissolve marker — the dissolver refused a staged parent, and a " +
			"stale staged chapter has no other way out: the evaluator skips it while its chapter is " +
			"open, so the wedge is permanent")
	}
	after, err := db.GetOrder(dweller.ID)
	testutil.MustNoErr(t, err, "reload the dweller")
	if after.Status != protocol.StatusStaged {
		t.Fatalf("the dissolve moved the parent to %q — a dissolve never transitions the parent; "+
			"that is what makes it safe for the staged shape", after.Status)
	}

	// And the dissolved chapter's terminal legs reach the fork, which parks the
	// parent with the failed-dig cause rather than a transition. This is the
	// dissolve-triggered twin of the failure path in the first test.
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(dweller.ID), "the disposition fork")
	parked, err := db.GetOrder(dweller.ID)
	testutil.MustNoErr(t, err, "reload after the fork")
	if got := QueueCause(parked.QueueCause); got != CauseStagedDigFailed {
		t.Fatalf("parent's cause is %q, want %q — a dissolved staged chapter parks exactly like a "+
			"failed one: no transition, a cause, the re-ask", got, CauseStagedDigFailed)
	}
}

// TestStagedParent_OperatorStagedRow_StaysWithTheAbandonSweep pins the widening's
// other edge: `staged` is also the OPERATOR's word for a robot staged at a
// station wait, and those rows belong to AbandonStuckOrders — which selects
// `staged` and exempts exactly IsGateStaged, drawing the ownership line in one
// spelling. The watchdog's Go-side filter reads the same predicate so the two
// passes never both own a row.
//
// MUTATION (verified): delete the filter's `continue`. The sweep counts this row
// as a stalled chapter and dissolves a robot-holding wait the abandon sweep was
// pacing — two policies on one row.
func TestStagedParent_OperatorStagedRow_StaysWithTheAbandonSweep(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// A staged row with NO lane wait — no steps_json, so not gate-staged — and a
	// quiet open child. This is the abandon sweep's population, not the
	// watchdog's.
	parent := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = protocol.StatusStaged
	})
	child := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.ParentOrderID = &parent.ID
		o.Status = protocol.StatusQueued
	})
	if IsGateStaged(parent) {
		t.Fatal("fixture bug: an order with no steps_json cannot be gate-staged")
	}
	quiesce(t, db, parent.ID)

	if r := d.SweepStalledChapters(); r.Dissolved+r.Waiting+r.Residue != 0 {
		t.Fatalf("watchdog returned %+v on an OPERATOR-staged row — `staged` is two populations and "+
			"this one is AbandonStuckOrders' (it selects staged and exempts IsGateStaged); two "+
			"passes both acting on one row is two policies", r)
	}
	_ = child
}
