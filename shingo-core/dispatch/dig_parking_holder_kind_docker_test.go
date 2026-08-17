//go:build docker

package dispatch

import (
	"errors"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// dig_parking_holder_kind_docker_test.go — §R.61's right of way, and the two
// kinds of holder §R.101 put behind it.
//
// ── WHY THIS SITE WAS NOT ON THE BRIEF, AND HOW IT WAS FOUND ──────────────
//
// The unit-5 brief named four readers that ask "is a dig here" and get §R.101's
// source lock. This is a fifth, and it turned up while measuring whether one of
// the four was worth changing: an experiment planting a source lock on a sibling
// lane produced, verbatim,
//
//	"lane PROBE2-SIB is held by dig 2"
//
// out of DigParkingHeldError — naming an excavation that had never been planned,
// on the message that goes into the wait and the recovery row. §R.61's rule is
// written about digs ("a dig must not plan into a lane another dig holds") and
// §R.101 widened the population it removes without widening the words.
//
// ── WHAT IS PINNED ────────────────────────────────────────────────────────
//
//  1. The refusal is UNCHANGED for both kinds. Right of way removes the lane
//     either way; §R.101 rules that a demand owns the lane it sources from, and
//     nothing here relaxes that. Both sub-tests assert the dig is still refused.
//  2. The two are named apart, and they split on the RELEASER: a reshuffle
//     finishes, a demand's already-dispatched robot carries one bin out.
//
// MUTATION (driven this session, fires): hardcode HolderIsExcavation to true in
// digParkingHeld. The source-lock sub-test fails on the rendered sentence.
func TestRightOfWay_NamesTheKindOfHolderItWasRefusedBy(t *testing.T) {
	t.Parallel()

	t.Run("a source lock refuses the dig and is not called an excavation", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		newTestDispatcher(t, db, testdb.NewSuccessBackend())
		grp, dug, park, _, dugSlots, _, bp := setupDwellGroup(t, db, "ROWSRC", 4, false)
		createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "ROWSRC-BLK")
		createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "ROWSRC-TGT")

		digger := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "rowsrc-dig" })
		holder := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "rowsrc-holder" })
		if err := reservations.AcquireLanesFor(db.DB, holder.ID, reservations.ModeDig,
			reservations.Anyone, reservations.BySourceLock, park.ID); err != nil {
			t.Fatalf("plant the source lock: %v", err)
		}

		_, err := findShuffleSlots(db, dug.ID, grp.ID, 1, digAskerFor(digger), nil)
		var held *DigParkingHeldError
		if !errors.As(err, &held) {
			t.Fatalf("findShuffleSlots err = %v, want a DigParkingHeldError. §R.101's source lock is "+
				"an exclusive hold and right of way must still remove the lane — a dig planning into "+
				"a lane a demand has resolved onto is the refusal this rule exists to make", err)
		}
		if held.HolderIsExcavation {
			t.Errorf("right of way reported lane %s as held by an EXCAVATION. Order %d holds it as a "+
				"§R.101 source lock: no reshuffle was planned, none will run, and the sentence this "+
				"renders — %q — sends the operator looking for a dig that does not exist. The two "+
				"also clear differently: a reshuffle finishes, this one ends when one already-"+
				"dispatched robot carries one bin out",
				held.Lane, holder.ID, held.Error())
		}
		if held.HolderID != holder.ID {
			t.Errorf("holder = %d, want %d — the wait names an order to go and look at, so a wrong "+
				"id is worse than none", held.HolderID, holder.ID)
		}
	})

	t.Run("a real excavation still refuses the dig AND is still called one", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
		grp, dug, park, _, dugSlots, _, bp := setupDwellGroup(t, db, "ROWDIG", 4, false)
		createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "ROWDIG-BLK")
		createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "ROWDIG-TGT")

		digger := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "rowdig-dig" })
		sibling := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "rowdig-sibling" })
		if !d.laneLock.TryLock(park.ID, sibling.ID) {
			t.Fatal("the sibling dig could not take the parking lane")
		}

		_, err := findShuffleSlots(db, dug.ID, grp.ID, 1, digAskerFor(digger), nil)
		var held *DigParkingHeldError
		if !errors.As(err, &held) {
			t.Fatalf("findShuffleSlots err = %v, want a DigParkingHeldError — this is §R.61's "+
				"original case and the split must not have cost it", err)
		}
		if !held.HolderIsExcavation {
			t.Errorf("right of way reported lane %s, held by dig %d, as an ordinary source lock. "+
				"Without this the test above is satisfied by a split that has collapsed the other "+
				"way round and every dig-versus-dig standoff loses its name", held.Lane, sibling.ID)
		}
	})
}
