//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// A ROBOT AT THE MARK IS NOT IN THE CORRIDOR — the order-22 deadlock, closed.
//
// ── WHAT WENT WRONG ───────────────────────────────────────────────────────
//
// A robot waiting at a lane's mark holds that lane's inbound mouth row, and that
// row is doing honest work: the release pass reads it as "this one is still
// coming", so a shallower store cannot wall the dweller in. But admitMouth
// refused an EXCAVATION on any other owner's row, exempting only the dig's own
// beneficiary — so the robot standing OUTSIDE the corridor turned away the dig
// that would have cleared the lane it was waiting for.
//
// Gated sim, 2026-08-30. Order 22 held Lane_08's mouth, gate-staged at its mark:
//
//	lane gate: release order 22 into lane Lane_08: no empty slot in lane 39:
//	           lane closed to stores: a claimed bin sits deeper
//
// once a minute, forever, while orders 23, 62 and 76 all sat under "Rearranging
// lane Lane_08" — three digs waiting on a mouth held by an order waiting on
// those digs. The plant went 7 machines to 5 to 4 to 3.
//
// The three tests below are the three claims the exemption makes, and the second
// is the one that keeps it honest: it must NOT widen past a robot at a mark.

// TestDigMarkExemption_StagedDwellerDoesNotRefuseAForeignDig is the deadlock.
//
// RED at fc68bda4: the acquire returns ErrReservationConflict and the pre-check
// returns false, because admitMouth's `mode == ModeDig` arm fires on the
// dweller's inbound row and nothing exempts it.
func TestDigMarkExemption_StagedDwellerDoesNotRefuseAForeignDig(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	wall, _, w, _, _ := clearLaneFixture(t, db, "DME1")
	line := lineNode(t, db, "DME1-LINE")

	dweller := stagedDwellerHoldingItsRow(t, db, d, "DME1", wall, line, w)

	// It really is holding the row — otherwise this test would pass for the
	// wrong reason, which is exactly how round 12b's deferral looked green.
	rows, err := reservations.ActiveMouthRows(db.DB, wall.ID)
	testutil.MustNoErr(t, err, "read mouth rows")
	held := false
	for _, r := range rows {
		if r.OrderID == dweller.ID && r.Mode == reservations.ModeInbound {
			held = true
		}
	}
	if !held {
		t.Fatalf("fixture: the dweller must HOLD an inbound mouth row on %s — the whole point is that "+
			"the row stays and stops excluding digs, not that it goes away. rows=%+v", wall.Name, rows)
	}

	// And it is seen as standing at the mark.
	atMark := stagedAtMarkOnLane(db, wall.ID)
	if !atMark.Has(dweller.ID) {
		t.Fatalf("stagedAtMarkOnLane(%s) does not contain the dweller %d — got %v",
			wall.Name, dweller.ID, atMark)
	}

	// A dig for SOMEBODY ELSE. Not the dweller's own rescue, so the beneficiary
	// exemption cannot be what admits it.
	stranger := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = line.Name
		o.Status = protocol.StatusPending
	})
	digFor := digAskerFor(stranger)
	if digFor.Owns(dweller.ID) {
		t.Fatal("fixture: the dig must NOT be raised for the dweller, or the beneficiary arm admits it")
	}

	if !d.laneLock.CanTakeFor(wall.ID, digFor, atMark) {
		t.Error("the PRE-CHECK refused a dig on a lane whose only holder is standing at the mark. " +
			"That robot is outside the corridor; it cannot obstruct an excavation. This is order 22.")
	}
	if !d.laneLock.TryLockFor(wall.ID, stranger.ID, digFor, stagedAtMarkOnLane(db, wall.ID)) {
		t.Fatal("the ACQUIRE refused a dig on a lane whose only holder is standing at the mark — " +
			"the order-22 deadlock: three digs waiting on a mouth held by an order waiting on them")
	}
}

// TestDigMarkExemption_InTransitHolderStillRefusesTheDig is the arm that keeps
// the exemption from widening, and it is asserted directly rather than left to
// the F-11 suite so that a change loosening the predicate goes red HERE, next to
// the thing it loosened.
//
// An `in_transit` holder is a robot working the corridor. It refuses, exactly as
// it did before — this is byte-for-byte the expectation
// TestWindow3_OrdinaryMouthHoldRefusesTheHealBeforeMintingAParent pins.
func TestDigMarkExemption_InTransitHolderStillRefusesTheDig(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	wall, _, w, _, bp := clearLaneFixture(t, db, "DME2")
	line := lineNode(t, db, "DME2-LINE")

	// THE HOLDER IS A DWELLER THAT WAS ALREADY LET IN, and that is the only shape
	// worth testing here.
	//
	// A holder with no vendor order and no plan is not an ActiveGateCandidate at
	// all, so it can never be in the exempt set however wrong the predicate gets —
	// asserting against one proves nothing. This order has BOTH: it was gate-staged
	// at this mark, its tail was appended, and it drove in. Vendor id and plan are
	// still on the row; what changed is that its wait index has moved past its wait,
	// so IsGateStaged is false. It is in the corridor and it must still refuse.
	inLane := stagedDwellerHoldingItsRow(t, db, d, "DME2", wall, line, w)
	testutil.MustNoErr(t, execOne(db, `UPDATE orders SET status='in_transit', wait_index=99 WHERE id=$1`,
		inLane.ID), "release it into the lane")
	reloaded, err := db.GetOrder(inLane.ID)
	testutil.MustNoErr(t, err, "reload")
	if IsGateStaged(reloaded) {
		t.Fatalf("fixture: order %d must read as RELEASED (in the corridor), not staged", reloaded.ID)
	}
	if reloaded.VendorOrderID == "" || reloaded.StepsJSON == "" {
		t.Fatalf("fixture: it must still be an ActiveGateCandidate (vendor=%q steps=%d bytes) — "+
			"otherwise the exempt set could never contain it and this test has no teeth",
			reloaded.VendorOrderID, len(reloaded.StepsJSON))
	}

	atMark := stagedAtMarkOnLane(db, wall.ID)
	if atMark.Has(inLane.ID) {
		t.Fatalf("order %d was let into %s and is driving in it, but stagedAtMarkOnLane still reports "+
			"it as standing at the mark. The exemption has widened past the fact it keys on, and a "+
			"dig would now be sent into a corridor with a robot in it. atMark=%v",
			inLane.ID, wall.Name, atMark)
	}

	stranger := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = line.Name
		o.Status = protocol.StatusPending
	})
	digFor := digAskerFor(stranger)

	if d.laneLock.CanTakeFor(wall.ID, digFor, atMark) {
		t.Error("the pre-check admitted a dig on a lane held by a robot that is INSIDE it. The " +
			"exemption is for robots parked at the mark and nobody else.")
	}
	if d.laneLock.TryLockFor(wall.ID, stranger.ID, digFor, atMark) {
		t.Fatal("the acquire admitted a dig on a lane held by a robot that is INSIDE it — two " +
			"robots in one single-file corridor is what the mouth exists to prevent")
	}

	// ── AND THE OTHER REQUESTER KIND, because the exemption now covers both ──
	//
	// 12d widened the exemption from excavations to every ModeDig acquire, so the
	// anti-widening arm has to be asked in both voices. A §R.101 source lock is
	// refused by an in-corridor holder exactly as an excavation is: what changed
	// is which requesters the exemption protects, never which holders it exempts.
	srcBin := createTestBinAtNode(t, db, bp.Code, w[2].ID, "BIN-DME2-DEEP")
	sourceLock := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = protocol.OrderTypeRetrieve
		o.SourceNode = w[2].Name
		o.DeliveryNode = line.Name
		o.BinID = &srcBin.ID
		o.Status = protocol.StatusPending
	})
	admitted, cause, _, err := d.AcquireLanesForOrder(sourceLock, w[2], line, EntryHeldBin)
	if err != nil {
		t.Fatalf("source-lock acquire errored: %v", err)
	}
	if admitted {
		t.Fatalf("a §R.101 source lock was admitted into %s while order %d is INSIDE the corridor. "+
			"The widening is about which requesters the exemption serves, not about letting one "+
			"past a robot that is actually in the lane (cause would have been %s).",
			wall.Name, inLane.ID, cause)
	}
}

// TestDigMarkExemption_DwellerIsNotReleasedWhileTheDigHoldsTheLane is the other
// direction, and it is why the exemption is safe.
//
// Letting the dig PAST the dweller's row must not let the dweller INTO the lane
// while that dig works. It does not, and nothing here had to be built: a foreign
// dig-mode row refuses lane entry at admission.go:834 (`digOwner != 0 &&
// !d.ownsDig(...)` → CauseLaneDigActive), which is asked on every evaluator pass
// through gateEntryVerdict. The exemption changed admitMouth and DigAdmissible;
// it did not touch DigOwner, which is what that arm reads.
//
// When the dig finishes, the dweller is re-evaluated by existing wiring:
// unlockLaneForCompound (compound.go:2144-2156) releases each held lane and then
// calls EvaluateLaneReleases and EvaluateDwellersSharingGroupWith on it.
func TestDigMarkExemption_DwellerIsNotReleasedWhileTheDigHoldsTheLane(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	wall, _, w, _, _ := clearLaneFixture(t, db, "DME3")
	line := lineNode(t, db, "DME3-LINE")

	dweller := stagedDwellerHoldingItsRow(t, db, d, "DME3", wall, line, w)

	// A foreign dig takes the lane, admitted past the dweller's row.
	stranger := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = line.Name
		o.Status = protocol.StatusPending
	})
	if !d.laneLock.TryLockFor(wall.ID, stranger.ID, digAskerFor(stranger), stagedAtMarkOnLane(db, wall.ID)) {
		t.Fatal("fixture: the dig must be admitted — that is the other test's subject")
	}

	// Now the dweller must NOT be let in.
	d.EvaluateLaneReleases(wall.ID)

	after, err := db.GetOrder(dweller.ID)
	testutil.MustNoErr(t, err, "reload the dweller")
	if !IsGateStaged(after) {
		t.Fatalf("the dweller was released into %s while a dig held it — status=%s. The dig takes "+
			"the lane EXCLUSIVELY; exempting its row from the dig's admission must not exempt the "+
			"dig from the dweller's.", wall.Name, after.Status)
	}
	if after.Status != "staged" {
		t.Errorf("the dweller's status is %q, want it still staged at the mark", after.Status)
	}
}

// stagedDwellerHoldingItsRow builds the production state this exemption is about:
// an order gate-staged at lane `wall`'s mark, holding its own inbound mouth row,
// and the ONLY holder on that lane.
//
// Three steps, and each one is here because the state cannot be reached without
// it:
//
//  1. A deeper store takes the lane first. A gated order released with nothing in
//     its way has its tail appended immediately and stops being gate-staged, so
//     something has to hold the lane while it dispatches. This is how the F-11
//     fixtures build a dweller too (Tier 2).
//  2. The dweller takes its OWN mouth row before the fleet create. stageGatedStore
//     calls DispatchDirect directly and skips admitLanes, so it does not model
//     this — but production does: the scanner's admitLanes runs before the fleet
//     commit (AcquireLanesForOrder → resolveOrderLaneHolds), and that row is
//     precisely what refused the digs in the order-22 deadlock. A fixture without
//     it would test the exemption against a lane nobody holds.
//  3. The deeper store PLACES — its row is released, which is what happens when a
//     bin lands. Leaving it would mean the dig below is refused by IT rather than
//     by the dweller, and the test would pass while proving nothing.
//
// What is left is one robot standing at one mark holding one inbound row: order
// 22 on Lane_08.
func stagedDwellerHoldingItsRow(t *testing.T, db *store.DB, d *Dispatcher, name string,
	wall, line *nodes.Node, w []*nodes.Node) *orders.Order {
	t.Helper()

	deep := stageDeeperBlocker(t, db, d, line, w[2], name+"-deep")

	o := testdb.CreateOrder(t, db, func(ord *orders.Order) {
		ord.DeliveryNode = w[1].Name
		ord.Status = "sourcing"
	})
	// STATED, NOT PRODUCED, and the reason is the whole subject of this file.
	//
	// The dweller's inbound row used to come from AcquireLanesForOrder. It cannot
	// any more: a gated lane defers its destination hold to the mark
	// (resolveOrderLaneHolds), precisely because a robot standing at a group's
	// waiting spot has not chosen a slot yet. So the deadlock this exemption was
	// built for — a dweller's own row excluding the digs that would free it — is
	// no longer CONSTRUCTIBLE through any plain door.
	//
	// The exemption is kept and kept pinned anyway. It is recent code, its
	// property is correct, and "I believe this population is now empty" is a
	// claim for an owner to retire rather than for a test to assume: an
	// unpinned exemption is one nobody notices going wrong if a door ever
	// produces the shape again. So the fixture asserts the row as a fact and
	// asks admitMouth what it does with it, which is what the test was always
	// really about.
	if adm := d.acquireOrderLanes(o.ID,
		[]laneHold{{laneID: wall.ID, mode: reservations.ModeInbound}}); adm.err != nil || !adm.admitted {
		t.Fatalf("fixture: the dweller must take its own inbound mouth row: adm=%v err=%v",
			adm.admitted, adm.err)
	}
	if _, err := d.DispatchDirect(o, line, w[1]); err != nil {
		t.Fatalf("fixture: DispatchDirect for %s: %v", w[1].Name, err)
	}
	dweller, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload the dweller")
	if !IsGateStaged(dweller) {
		t.Fatalf("fixture: the dweller must be gate-staged behind the deeper store "+
			"(wait_index=%d vendor=%q)", dweller.WaitIndex, dweller.VendorOrderID)
	}
	markStaged(t, db, dweller.ID)

	testutil.MustNoErr(t, reservations.ReleaseLane(db.DB, deep.ID, wall.ID), "the deeper store places")
	return dweller
}

// execOne runs a single statement against the test DB.
func execOne(db *store.DB, q string, args ...any) error {
	_, err := db.Exec(q, args...)
	return err
}

// TestDigMarkExemption_AMarkStagesForTheWholeGroup pins the owner's ruling of
// 2026-08-31, and it is the one that says what a wait point IS.
//
// A wait point is staging for the node GROUP. It is not a doorway onto the lane
// whose name it carries: "DME4-WALL-WAIT" is a map point painted near that lane,
// and a robot standing on it has not entered a corridor and has not been
// committed to one — the oracle can still send it into any lane in the group. So
// it obstructs an excavation in NONE of them.
//
// The predicate must therefore answer for the whole group. Matching on
// w.WaitLane == laneID would be reading the paint rather than the geometry, and
// would go on refusing a dig on a sibling lane for a robot standing still,
// several lanes away, in the shared staging area.
//
// RED under a lane-match: the dweller is staged at WALL's mark, and this asserts
// it is exempt on PARK — its sibling — which a lane-match never reports.
//
// NOTE ON REACH. No production path currently puts a gate-staged order's INBOUND
// mouth row on a lane other than the one its own wait names, so on today's tree
// this changes no dispatch outcome; it is asserted at the predicate because that
// is where the model lives. It becomes load-bearing the moment wait points are
// genuinely shared across a group's lanes, which is the group-mouth work.
func TestDigMarkExemption_AMarkStagesForTheWholeGroup(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	wall, park, w, _, _ := clearLaneFixture(t, db, "DME4")
	line := lineNode(t, db, "DME4-LINE")
	dweller := stagedDwellerHoldingItsRow(t, db, d, "DME4", wall, line, w)

	// Its own lane, which a lane-match also gets right.
	if !stagedAtMarkOnLane(db, wall.ID).Has(dweller.ID) {
		t.Fatalf("the dweller %d is not reported at %s's own mark", dweller.ID, wall.Name)
	}

	// THE SIBLING. Same group, different lane, and the robot is standing in the
	// staging area in front of both.
	if !stagedAtMarkOnLane(db, park.ID).Has(dweller.ID) {
		t.Errorf("order %d is standing at %s's wait point, which is staging for the whole group, "+
			"and it is NOT reported as outside sibling lane %s. A robot in the staging area "+
			"obstructs no corridor in the group; scoping this to the lane the mark is named after "+
			"reads the paint on the floor instead of the geometry.", dweller.ID, wall.Name, park.Name)
	}

	// AND NOT ACROSS GROUPS. A different group's staging area is a different
	// place, and an order standing in it is not outside these corridors.
	otherWall, _, _, _, _ := clearLaneFixture(t, db, "DME5")
	if stagedAtMarkOnLane(db, otherWall.ID).Has(dweller.ID) {
		t.Errorf("order %d is staged in %s's group but was reported as outside %s, which is in a "+
			"different group entirely — the exemption has leaked across a group boundary",
			dweller.ID, wall.Name, otherWall.Name)
	}
}

// TestDigMarkExemption_SourceLockRetrieveIsAdmittedPastAMarkHolder is the 12c
// wedge, reproduced and closed.
//
// ── WHAT WENT WRONG, AND IT IS THE ORDER-22 DEADLOCK IN A NEW COSTUME ─────
//
// The exemption first shipped scoped to EXCAVATIONS: a dig raised by the heal
// path or by planBuriedReshuffle was admitted past a robot at a mark, and a
// §R.101 source lock was not, because a source lock is not an excavation. The
// vocabulary was right and the physics were wrong.
//
// Gated sim, 2026-08-31, ~4,158 orders in. Order 22 gate-staged at Lane_08's
// mark, holding that lane's inbound row. Order 23, a RETRIEVE, wanted the bin
// one slot deeper; its source hold takes Lane_08 in ModeDig and was refused
// `lane-held-traffic`. Order 22's own re-bind was then refused BECAUSE order 23
// was coming for that bin — storing at the mouth would seal it in. Each was the
// other's only releaser, and the plant stopped.
//
// RED on b0dca8a5: the acquire below returns admitted=false with
// CauseLaneHeldTraffic, because acquireOrderLanes passed nil.
func TestDigMarkExemption_SourceLockRetrieveIsAdmittedPastAMarkHolder(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	wall, _, w, _, bp := clearLaneFixture(t, db, "DME6")
	line := lineNode(t, db, "DME6-LINE")

	// Order 22: a dweller at the mark, holding the lane's inbound row.
	dweller := stagedDwellerHoldingItsRow(t, db, d, "DME6", wall, line, w)

	// Order 23: a retrieve coming for the bin one slot DEEPER than the dweller's
	// destination. Its source is in the lane, so §R.101 takes the lane in ModeDig.
	deeper := createTestBinAtNode(t, db, bp.Code, w[2].ID, "BIN-DME6-DEEP")
	retrieve := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = protocol.OrderTypeRetrieve
		o.SourceNode = w[2].Name
		o.DeliveryNode = line.Name
		o.BinID = &deeper.ID
		o.Status = protocol.StatusPending
	})

	admitted, cause, laneName, err := d.AcquireLanesForOrder(retrieve, w[2], line, EntryHeldBin)
	if err != nil {
		t.Fatalf("acquire errored: %v", err)
	}
	if !admitted {
		t.Fatalf("the retrieve was refused (%s on %s) by order %d, which is standing at %s's mark "+
			"and is not in the corridor. This is the 12c wedge: the retrieve is the only thing that "+
			"would free the dweller, and the dweller is the only thing refusing the retrieve.",
			cause, laneName, dweller.ID, wall.Name)
	}
}

// TestDigMarkExemption_TheCycleBreaksEndToEnd is the one that proves the wedge
// is gone rather than that its first refusal was removed.
//
// Three links, in order: while the retrieve holds the lane in ModeDig the
// dweller must NOT be let in (the dig-excludes-everyone direction, unchanged);
// the retrieve then lifts the deeper bin and releases; and the dweller's re-bind,
// which was refused because somebody was coming for that bin, now succeeds.
func TestDigMarkExemption_TheCycleBreaksEndToEnd(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	wall, _, w, _, bp := clearLaneFixture(t, db, "DME7")
	line := lineNode(t, db, "DME7-LINE")
	dweller := stagedDwellerHoldingItsRow(t, db, d, "DME7", wall, line, w)

	deeper := createTestBinAtNode(t, db, bp.Code, w[2].ID, "BIN-DME7-DEEP")
	retrieve := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = protocol.OrderTypeRetrieve
		o.SourceNode = w[2].Name
		o.DeliveryNode = line.Name
		o.BinID = &deeper.ID
		o.Status = protocol.StatusPending
	})
	if adm, cause, ln, err := d.AcquireLanesForOrder(retrieve, w[2], line, EntryHeldBin); err != nil || !adm {
		t.Fatalf("link 1: the retrieve must be admitted (%v %s %s)", err, cause, ln)
	}

	// LINK 2: the dweller stays out while the retrieve owns the corridor.
	d.EvaluateLaneReleases(wall.ID)
	mid, err := db.GetOrder(dweller.ID)
	testutil.MustNoErr(t, err, "reload the dweller")
	if !IsGateStaged(mid) {
		t.Fatalf("the dweller was released into %s while a ModeDig holder owned it (status=%s) — "+
			"exempting the retrieve from the dweller's row must not exempt the dweller from the "+
			"retrieve's", wall.Name, mid.Status)
	}

	// LINK 3: the retrieve takes its bin out and lets the lane go.
	testutil.MustNoErr(t, execOne(db, `UPDATE bins SET node_id=NULL WHERE id=$1`, deeper.ID),
		"the retrieve lifts the deeper bin")
	testutil.MustNoErr(t, execOne(db, `UPDATE orders SET status='confirmed' WHERE id=$1`, retrieve.ID),
		"the retrieve completes")
	_, rlErr := reservations.ReleaseLanesByOwner(db.DB, retrieve.ID)
	testutil.MustNoErr(t, rlErr, "release its lanes")

	// The sealing guard that refused the dweller — "somebody is coming for that
	// bin" — no longer holds, so the re-bind resolves.
	if _, rErr := db.FindStoreSlotInLaneExcluding(wall.ID, dweller.ID); rErr != nil {
		t.Fatalf("the dweller's re-bind is STILL refused after the retrieve took its bin out and "+
			"released the lane: %v. The cycle is not broken — only its first refusal was.", rErr)
	}
}
