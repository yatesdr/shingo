//go:build docker

package dispatch

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// promiseHolder is an order holding a bin the way a demand holds one BEFORE a
// robot is committed: a pending reservation and the pointer, no claimed_by. That
// is the population §7's ranked take contests — the record calls it a promise,
// and it is exactly the holder the claim CAS cannot see (it refuses claimed
// bins, and a promise-holder has none).
func promiseHolder(t *testing.T, db *store.DB, uuid string, priority int, binID int64) *orders.Order {
	t.Helper()
	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = uuid
		o.OrderType = OrderTypeRetrieve
		o.Status = StatusSourcing
		o.Priority = priority
	})
	testutil.MustNoErr(t, reservations.Acquire(db.DB, o.ID, binID, "test-promise"), "the holder's promise")
	testutil.MustNoErr(t, db.UpdateOrderBinID(o.ID, binID), "the holder's pointer")
	got, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "re-read the holder")
	return got
}

// holdShapeOf is what an order holds right now: pointer, reservations, claims.
type holdShapeOf struct {
	binID    int64
	res      int
	binClaim int
}

func holdsOf(t *testing.T, db *store.DB, orderID int64) holdShapeOf {
	t.Helper()
	var h holdShapeOf
	o, err := db.GetOrder(orderID)
	testutil.MustNoErr(t, err, "read the order")
	if o.BinID != nil {
		h.binID = *o.BinID
	}
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT COUNT(*) FROM reservations WHERE order_id=$1 AND resource_kind='bin'`, orderID).Scan(&h.res),
		"count the order's bin reservations")
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT COUNT(*) FROM bins WHERE claimed_by=$1`, orderID).Scan(&h.binClaim), "count the order's claims")
	return h
}

// digAgainstBlocker builds the one shape every test below needs: a bin standing
// in a lane, a compound parent whose dig must move it, and one child leg that
// names it. Returns the parent and the children slice ready for
// CreateCompoundChildren.
func digAgainstBlocker(t *testing.T, db *store.DB, prefix string, parentPriority int, blocker *bins.Bin) (*orders.Order, []store.CompoundChild) {
	t.Helper()
	parent := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = prefix + "-parent"
		o.OrderType = OrderTypeRetrieve
		o.Status = StatusReshuffling
		o.Priority = parentPriority
	})
	leg := &orders.Order{
		EdgeUUID:      prefix + "-leg",
		StationID:     "line-1",
		OrderType:     OrderTypeMove,
		Status:        StatusPending,
		Quantity:      1,
		ParentOrderID: &parent.ID,
		Sequence:      1,
		BinID:         &blocker.ID,
	}
	return parent, []store.CompoundChild{{Order: leg, BinID: blocker.ID}}
}

// blockerBin makes a bin at a fresh node for a dig to be blocked by.
func blockerBin(t *testing.T, db *store.DB, prefix string) *bins.Bin {
	t.Helper()
	sd := testdb.SetupStandardData(t, db)
	node := &nodes.Node{Name: prefix + "-SLOT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(node), "create the blocker's node")
	return testdb.CreateBinAtNode(t, db, sd.Payload.Code, node.ID, prefix+"-BLOCKER")
}

// TestRankedTake_AnOutrankedDigBacksOutWhole is §7: a dig is not a privileged
// move, and one that has to wait for a complex or a move waits.
//
// The steal was unconditional, on the reasoning that a blocker is positional.
// That is an argument about WHICH bin, not about whose turn — so the take now
// goes by the demand ranking and a dig that loses backs out whole.
func TestRankedTake_AnOutrankedDigBacksOutWhole(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	blocker := blockerBin(t, db, "RANKOUT")
	holder := promiseHolder(t, db, "rankout-holder", 5, blocker.ID)
	parent, children := digAgainstBlocker(t, db, "rankout", 0, blocker)

	before := holdsOf(t, db, holder.ID)

	_, err := db.CreateCompoundChildren(children)
	if err == nil {
		t.Fatal("the dig's parent is outranked by the bin's holder; the steal must refuse")
	}
	var refused *store.BlockerClaimedError
	if !errors.As(err, &refused) {
		t.Fatalf("refusal error = %T (%v), want a *store.BlockerClaimedError — the caller's park "+
			"disposition keys on the type, so an untyped refusal fails the dig instead of parking it", err, err)
	}
	if refused.HolderID != holder.ID {
		t.Errorf("the refusal names holder %d, want %d — the park's operator sentence is built from "+
			"this, and a wait that cannot say whose bin it is waiting on is a wait nobody can chase",
			refused.HolderID, holder.ID)
	}

	// ── THE T3 ZOMBIE PIN ──────────────────────────────────────────────────
	//
	// A rank check that merely DECLINED to un-point one holder would fall
	// through: the claim CAS only refuses CLAIMED bins and a promise-holder has
	// none, so the CAS passes and supersedeBinLedger then wipes the ledger for
	// the whole bin — shredding the book of the order that WON the contest, and
	// leaving it pointing at a bin it no longer holds. The pointer wedge,
	// manufactured for the winner. So the refusal must abort the transaction.
	after := holdsOf(t, db, holder.ID)
	if after != before {
		t.Errorf("the holder's holds changed across a REFUSED steal: before %+v, after %+v.\n"+
			"It won the contest. A refusal that lets the transaction continue wipes its ledger row "+
			"and leaves its pointer — the pointer wedge, built for the order that won.", before, after)
	}
	if after.binID != blocker.ID || after.res != 1 {
		t.Errorf("the winner holds %+v, want its pointer at bin %d and one reservation", after, blocker.ID)
	}

	// And nothing of the dig's survives: no children, no claims, no ledger rows.
	var kids int
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT COUNT(*) FROM orders WHERE parent_order_id=$1`, parent.ID).Scan(&kids), "count children")
	if kids != 0 {
		t.Errorf("the aborted dig left %d child order(s) behind — the whole hand unwinds or the "+
			"plan is half-written", kids)
	}
	dig := holdsOf(t, db, parent.ID)
	if dig.res != 0 || dig.binClaim != 0 {
		t.Errorf("the aborted dig's parent still holds %+v, want nothing. A WAITING DIG HOLDS "+
			"NOTHING: it leaves so the holder can actually get in, or the wait is a deadlock with "+
			"extra steps.", dig)
	}
}

// TestRankedTake_TheWinningPathIsUnchanged is the other half: an outranking dig
// steals exactly as before — set-valued, pointer cleared, ledger superseded.
// Asserted from the gate's side so a future tightening of the comparator cannot
// quietly stop digs plant-wide.
func TestRankedTake_TheWinningPathIsUnchanged(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	blocker := blockerBin(t, db, "RANKWIN")
	holder := promiseHolder(t, db, "rankwin-holder", 0, blocker.ID)
	parent, children := digAgainstBlocker(t, db, "rankwin", 5, blocker)
	_ = parent

	_, err := db.CreateCompoundChildren(children)
	testutil.MustNoErr(t, err, "the dig outranks the holder; the steal must proceed")

	after := holdsOf(t, db, holder.ID)
	if after.binID != 0 {
		t.Errorf("the beaten holder still points at bin %d. The un-point is what sends it back "+
			"through the finder to re-resolve; left pointed it re-enters through dispatchHeldBin, "+
			"which never re-acquires.", after.binID)
	}
	var claimedBy *int64
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT claimed_by FROM bins WHERE id=$1`, blocker.ID).Scan(&claimedBy), "read the blocker's claim")
	if claimedBy == nil {
		t.Error("the winning dig did not claim the blocker")
	}
}

// TestRankedTake_ALegRanksOnItsParent is T2 proved at the gate: the same leg
// wins or loses depending on whose demand it presents. On its own row a leg is
// priority 0 and the youngest timestamp in the plant, so it loses to any
// promise-holder with a priority.
func TestRankedTake_ALegRanksOnItsParent(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	blocker := blockerBin(t, db, "RANKLEG")
	holder := promiseHolder(t, db, "rankleg-holder", 3, blocker.ID)
	// The PARENT carries the priority; the leg's own row has none, and its
	// created_at is later than the holder's. On its own row it loses both ways.
	_, children := digAgainstBlocker(t, db, "rankleg", 5, blocker)

	_, err := db.CreateCompoundChildren(children)
	testutil.MustNoErr(t, err, "a leg of a priority-5 parent must beat a priority-3 promise-holder")

	if after := holdsOf(t, db, holder.ID); after.binID != 0 {
		t.Errorf("the priority-3 holder kept its pointer, so the leg was ranked on its OWN row " +
			"(priority 0, youngest in the plant) rather than on its parent's. Every dig in the " +
			"plant loses forever that way.")
	}
}

// TestRankedTake_EqualRankGoesToTheOlder pins the tie-break, which is what makes
// progress a guarantee: created_at is stamped once by the INSERT and no writer
// restamps it, so a demand that keeps losing ages toward the front of its class
// instead of two demands trading one bin forever.
func TestRankedTake_EqualRankGoesToTheOlder(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	blocker := blockerBin(t, db, "RANKTIE")
	holder := promiseHolder(t, db, "ranktie-holder", 0, blocker.ID)
	parent, children := digAgainstBlocker(t, db, "ranktie", 0, blocker)
	// The dig's demand is OLDER than the holder's, at equal priority.
	_, err := db.DB.Exec(`UPDATE orders SET created_at = NOW() - INTERVAL '2 hours' WHERE id=$1`, parent.ID)
	testutil.MustNoErr(t, err, "age the dig's parent")

	if _, err := db.CreateCompoundChildren(children); err != nil {
		t.Fatalf("an older demand at equal priority must win the bin: %v", err)
	}
	if after := holdsOf(t, db, holder.ID); after.binID != 0 {
		t.Error("the younger holder kept the bin against an older demand at the same priority")
	}
}

// TestRankedTake_AnEqualAndYoungerDigYields is the tie's other side: at equal
// priority the newer dig yields. A tie going to the challenger would let two
// demands take the bin from each other on alternate passes.
func TestRankedTake_AnEqualAndYoungerDigYields(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	blocker := blockerBin(t, db, "RANKYOUNG")
	holder := promiseHolder(t, db, "rankyoung-holder", 0, blocker.ID)
	_, err := db.DB.Exec(`UPDATE orders SET created_at = NOW() - INTERVAL '2 hours' WHERE id=$1`, holder.ID)
	testutil.MustNoErr(t, err, "age the holder's demand")
	_, children := digAgainstBlocker(t, db, "rankyoung", 0, blocker)

	if _, err := db.CreateCompoundChildren(children); err == nil {
		t.Fatal("a younger dig at equal priority must yield to the incumbent")
	}
	if after := holdsOf(t, db, holder.ID); after.binID != blocker.ID || after.res != 1 {
		t.Errorf("the older incumbent lost something to a refused steal: %+v", after)
	}
}

// TestRankedTake_TheParkNamesItsOwnCause pins the outranked disposition end to
// end, through the real planner.
//
// The cause must not be dig-blocker-claimed: that releaser is a robot finishing
// its drive, and a promise-holder has none. A wait naming the wrong releaser is
// worse than one naming none.
func TestRankedTake_TheParkNamesItsOwnCause(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	blocker := createTestBinAtNode(t, db, bp.Code, slots[0].ID, "RANKPARK-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "RANKPARK-TGT")
	holder := promiseHolder(t, db, "rankpark-holder", 9, blocker.ID)

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "rankpark-demand"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.Status = StatusQueued
	})
	d.planner.planBuriedReshuffle(demand, &BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID})

	got, err := db.GetOrder(demand.ID)
	testutil.MustNoErr(t, err, "re-read the demand")
	if got.QueueCause != string(CauseDigBlockerPromised) {
		t.Errorf("the outranked dig parked under %q, want %q.\n"+
			"dig-blocker-claimed's releaser is a robot's drive time; a promise-holder has no robot, "+
			"so reusing it tells an operator to wait for something that is not coming.",
			got.QueueCause, CauseDigBlockerPromised)
	}
	if got.QueueCode != string(protocol.QueueStorageRearranging) {
		t.Errorf("queue_code = %q, want %q", got.QueueCode, protocol.QueueStorageRearranging)
	}

	// A WAITING DIG HOLDS NOTHING — it leaves so the holder can actually get in.
	// The lane lock IS a mouth row in dig mode (binresolver.LaneLock →
	// reservations.AcquireLanesFor), so one count answers both halves.
	var mouths int
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT COUNT(*) FROM reservations WHERE resource_kind='mouth' AND node_id=$1`,
		lane.ID).Scan(&mouths), "count mouth rows on the contested lane")
	if mouths != 0 {
		t.Errorf("the contested lane still carries %d mouth row(s) after the dig was outranked, "+
			"want none. A dig that waits while squatting on the corridor is a deadlock with extra "+
			"steps: the holder cannot get in to remove the very bin the dig is waiting on.", mouths)
	}
	_ = holder
}
