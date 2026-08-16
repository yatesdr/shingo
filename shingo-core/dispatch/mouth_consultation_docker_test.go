//go:build docker

package dispatch

import (
	"errors"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// mouth_consultation_docker_test.go — the resolver's consultation as a REAL
// filter (§R.95/§R.96 stage 2, the last of them).
//
// The parking pool already dropped lanes held by a foreign DIG, in SQL, inside
// ListChildNodesUnlocked — whose exclusion clause reads `mode = 'dig'` and
// nothing else. That is half of admitMouth's rule: a dig excludes everyone AND is
// excluded by everyone, either side. A lane carrying an ordinary inbound hold
// refused the dig at admission and was still offered here as parking.
//
// The cost of that is not a refusal, it is A WORSE SLOT: the resolver takes the
// refusal, drops the slot, and walks to the next candidate — which is the next
// slot SHALLOWER in the same lane. That is the LS_C4 shape (a four-slot empty
// lane left X X . X with an unreachable hole at depth 3), reached through the
// mouth instead of through occupancy.

// A lane whose mouth another order owns leaves the pool at the source, and the
// refusal NAMES it rather than reporting a full group.
//
// MUTATION (verified): pass skipTheMouth from findShuffleSlots. The held lane is
// offered as parking and the typed refusal never appears.
func TestMouthConsultation_AHeldLaneIsNotParking(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// One group, two lanes: the one being dug, and the only parking.
	_, dugID, dugSlot := gatedLane(t, db, "MC-DUG", "")
	dug, err := db.GetNode(dugID)
	testutil.MustNoErr(t, err, "reload the dug lane")
	grpID := *dug.ParentID

	sib := laneInGroup(t, db, grpID, "MC-SIB")
	line := lineNode(t, db, "MC-LINE")

	// The digger, and the pool it can reach while nothing holds the sibling.
	digger := testdb.CreateOrder(t, db)
	asker := reservations.AskerFor(digger.ID, digger.ID)
	slots, err := findShuffleSlots(db, dugID, grpID, 1, asker, nil)
	testutil.MustNoErr(t, err, "the sibling must be parking while it is free")
	if len(slots) != 1 {
		t.Fatalf("free pool returned %d slot(s), want 1 — the fixture has no parking to lose", len(slots))
	}

	// ── AND NOW SOMEBODY ELSE OWNS THE SIBLING'S MOUTH. An ordinary store on
	// its way in: an INBOUND hold, which the SQL exclusion cannot see.
	store := testdb.CreateOrder(t, db)
	sibSlot, err := db.GetNodeByDotName("MC-SIB-S0")
	testutil.MustNoErr(t, err, "resolve the sibling's slot")
	admitted, _, _, err := d.AcquireLanesForOrder(store, line, sibSlot, EntryFreshBin)
	testutil.MustNoErr(t, err, "the store's acquire")
	if !admitted {
		t.Fatal("the store must take the sibling's mouth for this fixture to mean anything")
	}
	if held := digHoldOwnerOrZero(t, db, sib); held != 0 {
		t.Fatalf("the sibling is held as a DIG by %d — then the SQL exclusion already sees it and "+
			"this test proves nothing about the mouth", held)
	}

	_, err = findShuffleSlots(db, dugID, grpID, 1, asker, nil)
	if err == nil {
		t.Fatal("the pool offered parking in a lane whose mouth another order owns. The dig would be " +
			"refused at admission, drop that slot, and walk to the next one shallower in the same " +
			"lane — which is how a four-slot empty lane ends up with a hole nothing can reach")
	}
	var mouthHeld *LaneMouthHeldParkingError
	if !errors.As(err, &mouthHeld) {
		t.Fatalf("the refusal is %v, want the typed mouth-held error — a wait that cannot name the "+
			"lane it is waiting on has no releaser a reader can check (law 8)", err)
	}
	if mouthHeld.Lane != "MC-SIB-LANE" {
		t.Errorf("the refusal names lane %q, want MC-SIB-LANE", mouthHeld.Lane)
	}
	_ = dugSlot
}

// AND "CANNOT SEE" IS NOT "FULL" — asserted on the DISPOSITION, which is where
// the distinction actually has to hold.
//
// ── WHAT THIS DOES NOT DO, AND WHY, STATED RATHER THAN IMPLIED ────────────
//
// The first version of this test closed the database and asserted the walk did
// not report a full group. It PASSED, and it passed for the wrong reason: with
// the database gone, ListChildNodesUnlocked fails before the walk starts, so the
// error came back from a read three steps upstream of the consultation and the
// assertion never touched the code it was named after. The mutation that removes
// the fail-closed arm did not break it — which is how it was caught.
//
// There is no seam to fail ONLY the mouth read, and adding one to a hot path for
// a test is a cost this does not earn. So the runtime reachability of the
// `unseen` bucket is established by reading, and what is asserted here is the
// half that can be: the two findings are different errors with different
// dispositions, and nothing collapses one into the other.
func TestMouthConsultation_CannotSeeIsNotFull(t *testing.T) {
	t.Parallel()
	unseen := &MouthUnreadableError{Lanes: []string{"LS_C4", "LS_C5"}, Short: 2}

	// NOT A FULL GROUP. Every caller that reports congestion tests ErrNoShuffleSlot,
	// and collapsing into it is what would send an operator to make room that is
	// already there behind a read that failed.
	if errors.Is(unseen, ErrNoShuffleSlot) {
		t.Fatal("an unreadable mouth unwraps to ErrNoShuffleSlot — 'I could not count the pool' and " +
			"'the pool is full' are different findings with different fixes")
	}
	if !errors.Is(unseen, ErrMouthUnreadable) {
		t.Fatal("the typed error does not unwrap to its own sentinel, so no caller can branch on it")
	}
	// It NAMES what it could not see. A shortfall that cannot say which lanes went
	// uncounted is a wait with nothing to go and look at.
	if !strings.Contains(unseen.Error(), "LS_C4") || !strings.Contains(unseen.Error(), "LS_C5") {
		t.Errorf("the message does not name the unread lanes: %q", unseen.Error())
	}
	if !strings.Contains(unseen.Error(), "NOT a full group") {
		t.Errorf("the message does not say what it is not: %q", unseen.Error())
	}

	// AND THE MOUTH-HELD SIBLING IS ALSO NOT A FULL GROUP, for the same reason and
	// with a different releaser: that order finishes with the lane.
	held := &LaneMouthHeldParkingError{Lane: "LS_C4", Short: 1}
	if errors.Is(held, ErrNoShuffleSlot) {
		t.Fatal("a mouth-held lane unwraps to ErrNoShuffleSlot, losing the lane it named")
	}
	if !errors.Is(held, ErrLaneMouthHeld) {
		t.Fatal("the typed error does not unwrap to its own sentinel")
	}
	if errors.Is(held, ErrMouthUnreadable) || errors.Is(unseen, ErrLaneMouthHeld) {
		t.Fatal("the two mouth refusals unwrap to each other — 'somebody owns it' and 'nobody could " +
			"read it' are the distinction this whole item is about")
	}
}

// The disposition half: neither mouth refusal may be classified as a fault. Both
// are congestion — they wait and retry — and the one thing that must never happen
// is a database read turning into a statement about the plant's geometry, which
// is the only thing in this file that kills an order.
func TestMouthConsultation_BothRefusalsAreTransient(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want serviceDigOutcome
	}{
		{"mouth held", &LaneMouthHeldParkingError{Lane: "L", Short: 1}, serviceDigLaneBusy},
		{"cannot see", &MouthUnreadableError{Lanes: []string{"L"}, Short: 1}, serviceDigReadFailed},
	} {
		if got := classifyPlanError(tc.err); got != tc.want {
			t.Errorf("%s classified as %v, want %v — an unplannable verdict is the terminal one, and "+
				"neither of these is a fact about geometry", tc.name, got, tc.want)
		}
	}
}

// laneInGroup adds a LANE with one slot under an existing group, so a fixture can
// have parking that is not the lane being dug.
func laneInGroup(t *testing.T, db *store.DB, groupID int64, name string) int64 {
	t.Helper()
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	testutil.MustNoErr(t, err, "get LANE type")
	lane := &nodes.Node{Name: name + "-LANE", IsSynthetic: true, Enabled: true,
		NodeTypeID: &laneType.ID, ParentID: &groupID}
	testutil.MustNoErr(t, db.CreateNode(lane), "create lane")
	d0 := 0
	slot := &nodes.Node{Name: name + "-S0", Enabled: true, ParentID: &lane.ID, Depth: &d0}
	testutil.MustNoErr(t, db.CreateNode(slot), "create slot")
	return lane.ID
}

// digHoldOwnerOrZero reports the dig-mode holder of a lane, 0 when there is none.
func digHoldOwnerOrZero(t *testing.T, db *store.DB, laneID int64) int64 {
	t.Helper()
	owner, err := reservations.DigHoldOwner(db.DB, laneID)
	testutil.MustNoErr(t, err, "read the dig hold owner")
	return owner
}
