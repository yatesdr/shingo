//go:build docker

package dispatch

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingo/shared/clock"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// chapter_floor_docker_test.go — §R.99's watchdog, one test per answer.
//
// The population is a demand in `reshuffling` with an open leg. It had no
// periodic pass at all: AdvanceStuckReshuffleParents covers the half where every
// child is terminal, and while the only occupants here were synthetic folders
// nobody noticed the other half. §R.91 put demands there.
//
// The watchdog asks one question — can this be safely re-planned now — and the
// three answers are pinned below. What is NOT pinned by a timer anywhere: the
// discriminator is whether the fleet holds a mission, because no elapsed time on
// Core's side is evidence about a robot (R.30: mid-order waits to 959s).

// quiesce ages a whole family past the stall window so the watchdog will look at
// it. It writes updated_at directly because there is no other way to say "this
// has been quiet" — every production writer's job is to make it untrue.
func quiesce(t *testing.T, db *store.DB, parentID int64) {
	t.Helper()
	old := clock.Now().UTC().Add(-2 * chapterStallWindow)
	_, err := db.DB.Exec(`UPDATE orders SET updated_at=$2 WHERE id=$1 OR parent_order_id=$1`, parentID, old)
	testutil.MustNoErr(t, err, "quiesce the family")
}

// openLeg returns the chapter's first non-terminal child.
func openLeg(t *testing.T, db *store.DB, parentID int64) int64 {
	t.Helper()
	legs, err := db.ListChildOrders(parentID)
	testutil.MustNoErr(t, err, "list legs")
	for _, l := range legs {
		if !protocol.IsTerminal(l.Status) {
			return l.ID
		}
	}
	t.Fatalf("compound %d has no open leg — the fixture is not the population under test", parentID)
	return 0
}

func dissolvedLegCount(t *testing.T, db *store.DB, parentID int64) int {
	t.Helper()
	legs, err := db.ListChildOrders(parentID)
	testutil.MustNoErr(t, err, "list legs")
	n := 0
	for _, l := range legs {
		if l.Status == protocol.StatusCancelled && l.ErrorDetail == reshuffleDissolveDetail {
			n++
		}
	}
	return n
}

// ANSWER 1 — no vehicle is committed. Nothing is moving, so what went stale is a
// PLAN, and the plan is the cheapest thing in the system to throw away (law 7:
// the reserve is a plan). The demand behind it survives and re-queues.
//
// MUTATION (verified): make classifyStalledChapter return chapterWaiting for
// every stalled chapter. The dissolve assertion fires and the demand sits in
// `reshuffling` forever wearing a cause — which is the corpse §R.99 refuses.
func TestChapterFloor_NoVehicleCommitted_DissolvesAndRequeues(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent, lane, _, _ := planStaleDigFixture(t, db, d, "CF-REPLAN")

	// PRECONDITION, or the assertion is about a fixture rather than a rule: the
	// open leg was never handed to the fleet.
	legID := openLeg(t, db, parent.ID)
	leg, err := db.GetOrder(legID)
	testutil.MustNoErr(t, err, "reload leg")
	if leg.VendorOrderID != "" {
		t.Fatalf("leg %d already carries vendor mission %q — this fixture is the WAITING case", legID, leg.VendorOrderID)
	}

	// A healthy, recently-written chapter is invisible to the watchdog. Asserted
	// before quiescing so the dissolve below is attributable to the quiet.
	if r := d.SweepStalledChapters(); r.Dissolved+r.Waiting+r.Residue != 0 {
		t.Fatalf("the watchdog acted on a chapter that has just been written: %+v", r)
	}

	quiesce(t, db, parent.ID)
	r := d.SweepStalledChapters()
	if r.Dissolved != 1 {
		t.Fatalf("watchdog returned %+v, want one dissolved — a stopped chapter with no robot on it "+
			"is a stale plan, and leaving it is the corpse the ruling refuses", r)
	}
	if n := dissolvedLegCount(t, db, parent.ID); n == 0 {
		t.Fatal("no leg carries the dissolve marker — the chapter was counted as dissolved and was not")
	}
	if d.laneLock.IsLocked(lane.ID) {
		t.Error("the lane is still locked after the dissolve — the re-plan refuses a locked lane")
	}

	// AND THE DEMAND SURVIVES, which is the half that makes this a resolution
	// rather than a cancellation. The parent transition is the cancel wiring's,
	// exactly as on the dispatch-triggered path.
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "the cancel wiring's re-drive")
	after, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload parent")
	if protocol.IsTerminal(after.Status) {
		t.Fatalf("parent is %q — the watchdog terminated a demand. Termination: never, any path", after.Status)
	}
	if !protocol.IsAcquiring(after.Status) {
		t.Fatalf("parent is %q, want an acquiring status — the oracle only adapts if the scanner "+
			"looks at this row again", after.Status)
	}
}

// ANSWER 2 — a vehicle is committed. A robot may be carrying a bin down a lane
// right now and Core's elapsed time is not evidence about that. So it waits, and
// the wait becomes visible: a cause and a releaser on the parent, which is the
// row an operator is actually looking at.
//
// MUTATION (verified): make the live-mission arm return chapterReplannable. This
// chapter dissolves with a robot out on it and the "one waiting, none dissolved"
// assertion fires.
//
// The obvious mutation — deleting the VendorOrderID guard — does NOT break this
// test, and the reason is worth recording rather than papering over: that guard
// is what keeps a PRE-FLEET leg out of the waiting arm, so removing it is caught
// by the sibling test above, not by this one. Two guards, two tests, and they do
// not cover for each other.
func TestChapterFloor_VehicleCommitted_WaitsVisibly(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent, _, _, _ := planStaleDigFixture(t, db, d, "CF-WAIT")

	legID := openLeg(t, db, parent.ID)
	testutil.MustNoErr(t, db.UpdateOrderVendor(legID, "sg-cfwait-1", "RUNNING", "AMR-04"), "hand the leg to the fleet")
	_, err := db.DB.Exec(`UPDATE orders SET status='in_transit' WHERE id=$1`, legID)
	testutil.MustNoErr(t, err, "the fleet took it")

	quiesce(t, db, parent.ID)
	r := d.SweepStalledChapters()
	if r.Waiting != 1 || r.Dissolved != 0 {
		t.Fatalf("watchdog returned %+v, want one waiting and none dissolved — the fleet holds a live "+
			"mission on leg %d and no Core-side clock is evidence about a robot", r, legID)
	}
	if n := dissolvedLegCount(t, db, parent.ID); n != 0 {
		t.Fatalf("%d leg(s) were cancelled while a mission was live on this chapter", n)
	}

	// THE WAIT IS ON THE BOARD. A parent in `reshuffling` with a blank cause reads
	// as an order nobody has looked at, which is what this population rendered as.
	after, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload parent")
	if QueueCause(after.QueueCause) != CauseChapterLegInFlight {
		t.Fatalf("parent's cause is %q, want %q", after.QueueCause, CauseChapterLegInFlight)
	}
	if after.QueueReason == "" {
		t.Fatal("the parent carries a cause and no sentence — the board renders the sentence")
	}
	if _, ok := releaserFor(CauseChapterLegInFlight); !ok {
		t.Fatal("the cause has no releaser row — a named wait with no declared releaser is what the " +
			"amended bar forbids")
	}
}

// ANSWER 3 — the fleet is done and Core never heard. Nobody is coming, so waiting
// cannot end it; but the robot has already acted, so dissolving could cancel a leg
// whose bin sits somewhere Core has not written down. This is the residue: alarm,
// and a human rules it.
//
// MUTATION (verified): fold the terminal-vendor-state arm into the waiting arm.
// The chapter waits forever on a mission that finished, which is the F-11 shape
// this whole round exists to stop reproducing, and the recovery-action assertion
// fires.
func TestChapterFloor_FleetFinishedCoreNeverHeard_Alarms(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent, _, _, _ := planStaleDigFixture(t, db, d, "CF-RESIDUE")

	legID := openLeg(t, db, parent.ID)
	testutil.MustNoErr(t, db.UpdateOrderVendor(legID, "sg-cfres-1", "FINISHED", "AMR-05"), "the fleet finished it")
	_, err := db.DB.Exec(`UPDATE orders SET status='in_transit' WHERE id=$1`, legID)
	testutil.MustNoErr(t, err, "and Core never heard")

	quiesce(t, db, parent.ID)
	r := d.SweepStalledChapters()
	if r.Residue != 1 || r.Dissolved != 0 || r.Waiting != 0 {
		t.Fatalf("watchdog returned %+v, want one residue and nothing else", r)
	}
	if n := dissolvedLegCount(t, db, parent.ID); n != 0 {
		t.Fatalf("%d leg(s) cancelled — the robot already acted and its bin may be somewhere Core "+
			"has not written down", n)
	}

	actions, err := db.ListRecoveryActions(50)
	testutil.MustNoErr(t, err, "read recovery actions")
	filed := 0
	for _, a := range actions {
		if a.Action == stalledChapterAction && a.TargetID == parent.ID {
			filed++
		}
	}
	if filed != 1 {
		t.Fatalf("%d %q action(s) recorded for compound %d, want 1 — the residue's whole disposition "+
			"is that a human is told", filed, stalledChapterAction, parent.ID)
	}
}

// And the property that keeps every one of the above honest: a chapter nobody has
// written to for a while but whose legs are RUNNING is not stalled. Quiet is
// asked across the family, not of the parent alone — a parent's own updated_at
// does not move while its legs work, so a parent-only test would call every
// healthy excavation stalled within a minute of starting.
func TestChapterFloor_ParentQuietButLegsMoving_IsNotStalled(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent, _, _, _ := planStaleDigFixture(t, db, d, "CF-BUSY")

	quiesce(t, db, parent.ID)
	// One leg writes, as a running leg does on every status change.
	legID := openLeg(t, db, parent.ID)
	_, err := db.DB.Exec(`UPDATE orders SET updated_at=$2 WHERE id=$1`, legID, clock.Now().UTC())
	testutil.MustNoErr(t, err, "the leg reports progress")

	if r := d.SweepStalledChapters(); r.Dissolved+r.Waiting+r.Residue != 0 {
		t.Fatalf("watchdog acted on a chapter whose leg is advancing: %+v", r)
	}

	// And once that leg goes quiet too, it is seen — otherwise this test would
	// pass against a watchdog that never fires at all.
	_, err = db.DB.Exec(`UPDATE orders SET updated_at=$2 WHERE id=$1`, legID,
		clock.Now().UTC().Add(-2*chapterStallWindow-time.Second))
	testutil.MustNoErr(t, err, "and then it stops")
	if r := d.SweepStalledChapters(); r.Dissolved+r.Waiting+r.Residue != 1 {
		t.Fatalf("watchdog missed a chapter that went quiet: %+v", r)
	}
}
