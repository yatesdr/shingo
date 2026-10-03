//go:build docker

package dispatch

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// A DIG IS NEVER WRITTEN UNDER A PARENT THAT IS GONE.
//
// The scanner plans a buried order's dig from the order row it read; a wire
// cancel can land between that read and the compound's write. The cascade
// (CancelOrderWithCascade) cancels the parent and then lists its children once,
// so children written after that list are nobody's: they dispatch, claim their
// bins and deliver for an order that no longer exists. In the sim, a cancelled
// keep-staged refill's dig child landed the incoming part on the spot after the
// changeover cancel had decided it, and the cell wedged.

type parentGoneFixture struct {
	db      *store.DB
	d       *Dispatcher
	backend *testdb.MockTrackingBackend
	sc      *testdb.CompoundScenario
	order   *orders.Order // as the scanner read it: queued, before the cancel
	buried  *BuriedError
}

func seedBuriedRetrieve(t *testing.T, prefix string) *parentGoneFixture {
	t.Helper()
	db := testDB(t)
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{Prefix: prefix})
	backend := testdb.NewTrackingBackend()
	d := NewDispatcher(db, backend, &mockEmitter{}, "core", "shingo.dispatch", &DefaultResolver{DB: db})
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: prefix + "-order", OrderType: OrderTypeMove, PayloadCode: sc.Payload.Code,
		SourceNode: sc.Grp.Name, DeliveryNode: sc.LineNode.Name, Quantity: 1.0,
	})
	order := testdb.RequireOrder(t, db, prefix+"-order")
	res := d.finder.FindSource(order, IntentFull)
	if res.Outcome != OutcomeReshuffle {
		t.Fatalf("fixture: finder outcome %v, want OutcomeReshuffle", res.Outcome)
	}
	return &parentGoneFixture{db: db, d: d, backend: backend, sc: sc, order: order, buried: res.Buried}
}

func (f *parentGoneFixture) cancel(t *testing.T) {
	t.Helper()
	f.d.HandleOrderCancel(testEnvelope(), &protocol.OrderCancel{OrderUUID: f.order.EdgeUUID, Reason: "operator"})
	if o := testdb.RequireOrder(t, f.db, f.order.EdgeUUID); o.Status != protocol.StatusCancelled {
		t.Fatalf("fixture: the cancel left the parent %q", o.Status)
	}
}

// endWithoutCascade ends the parent through the lifecycle alone, the way the
// operations page's terminate did before it cascaded: the legs are not touched.
func (f *parentGoneFixture) endWithoutCascade(t *testing.T, parentID int64) {
	t.Helper()
	o, err := f.db.GetOrder(parentID)
	testutil.MustNoErr(t, err, "parent")
	f.d.lifecycle.CancelOrder(o, "core", "test: ended without cascade", CancelCause{})
}

// nothingLeftBehind is the whole contract: no live child, nothing with the
// fleet, no bin claimed by a child, and the lane not held in the dead parent's
// name.
func (f *parentGoneFixture) nothingLeftBehind(t *testing.T) {
	t.Helper()
	children, err := f.db.ListChildOrders(f.order.ID)
	testutil.MustNoErr(t, err, "children")
	childIDs := map[int64]bool{}
	for _, c := range children {
		childIDs[c.ID] = true
		if !protocol.IsTerminal(c.Status) {
			t.Errorf("child %d of the cancelled parent is %q, want none live", c.ID, c.Status)
		}
	}
	cancelled := map[string]bool{}
	for _, id := range f.backend.CancelRequests() {
		cancelled[id] = true
	}
	live := 0
	for id := range f.backend.Orders() {
		if !cancelled[id] {
			live++
		}
	}
	if live != 0 {
		t.Errorf("%d fleet order(s) live for a cancelled parent's dig", live)
	}
	all, err := f.db.ListBins()
	testutil.MustNoErr(t, err, "bins")
	for _, b := range all {
		if b.ClaimedBy != nil && (childIDs[*b.ClaimedBy] || *b.ClaimedBy == f.order.ID) {
			t.Errorf("bin %s is still claimed by order %d of the cancelled dig", b.Label, *b.ClaimedBy)
		}
	}
	if held := f.d.laneLock.LanesHeldBy(f.order.ID); len(held) != 0 {
		t.Errorf("the cancelled parent still holds lane lock(s) %v: every dig behind that lane starves", held)
	}
}

// The cancel lands between the scanner's read and the compound's write.
func TestCompoundDig_ParentCancelledBeforeTheWrite(t *testing.T) {
	t.Parallel()
	f := seedBuriedRetrieve(t, "PGONE1")
	f.cancel(t)
	ended := testdb.RequireOrder(t, f.db, f.order.EdgeUUID)

	// The first thing the pass takes for the dead parent is its lane lock, and a
	// lane row is a reservation: its insert refuses an ended owner
	// (reservations.OwnerLiveSQL), so the pass stops there, a layer before the
	// compound write would refuse with ErrParentGone (the store pin below holds
	// that one on its own). Either way it stops: nothing is written or held, and
	// the order is neither failed nor parked.
	if err := f.d.PlanBuriedReshuffle(f.order, f.buried); err == nil { // the scanner's stale row
		t.Fatal("PlanBuriedReshuffle planned a dig for an ended order")
	}
	f.nothingLeftBehind(t)
	if o := testdb.RequireOrder(t, f.db, f.order.EdgeUUID); o.QueueCause != ended.QueueCause ||
		o.QueueReason != ended.QueueReason || o.Status != ended.Status {
		t.Errorf("the refusal rewrote the ended order: %q/%q/%q -> %q/%q/%q", ended.Status, ended.QueueCause,
			ended.QueueReason, o.Status, o.QueueCause, o.QueueReason)
	}
}

// The cancel lands after the compound's write commits and before the parent
// moves into reshuffling. Its cascade finds the legs and cancels them; the
// creation stops instead of advancing them.
func TestCompoundDig_ParentCancelledAfterTheWrite(t *testing.T) {
	t.Parallel()
	f := seedBuriedRetrieve(t, "PGONE2")
	f.d.SetCompoundWrittenHook(func(int64) { f.cancel(t) })

	err := f.d.PlanBuriedReshuffle(f.order, f.buried)
	if !errors.Is(err, ErrParentGone) {
		t.Fatalf("PlanBuriedReshuffle = %v, want ErrParentGone", err)
	}
	children, lerr := f.db.ListChildOrders(f.order.ID)
	testutil.MustNoErr(t, lerr, "children")
	if len(children) == 0 {
		t.Fatal("fixture: no legs were written before the cancel — this test is about the window after the write")
	}
	f.nothingLeftBehind(t)
}

// An end that does not cascade (the parent's row turned terminal by itself) in
// the same window: the creation withdraws the legs itself.
func TestCompoundDig_ParentEndedWithoutCascadeAfterTheWrite(t *testing.T) {
	t.Parallel()
	f := seedBuriedRetrieve(t, "PGONE3")
	f.d.SetCompoundWrittenHook(func(parentID int64) {
		f.endWithoutCascade(t, parentID)
	})

	if err := f.d.PlanBuriedReshuffle(f.order, f.buried); !errors.Is(err, ErrParentGone) {
		t.Fatalf("PlanBuriedReshuffle = %v, want ErrParentGone", err)
	}
	f.nothingLeftBehind(t)
}

// A leg still pending under a parent that has ended is not dispatched by the
// advance; it is cancelled, which releases its claim.
func TestCompoundDig_AdvanceSendsNoLegOfAnEndedParent(t *testing.T) {
	t.Parallel()
	f := seedBuriedRetrieve(t, "PGONE4")
	testutil.MustNoErr(t, f.d.PlanBuriedReshuffle(f.order, f.buried), "plan the dig")
	sent := len(f.backend.Orders())
	if sent == 0 {
		t.Fatal("fixture: the dig sent nothing")
	}
	f.endWithoutCascade(t, f.order.ID)

	testutil.MustNoErr(t, f.d.AdvanceCompoundOrder(f.order.ID), "advance")

	if got := len(f.backend.Orders()); got != sent {
		t.Fatalf("fleet orders %d -> %d: the advance sent a leg of an ended parent", sent, got)
	}
	children, err := f.db.ListChildOrders(f.order.ID)
	testutil.MustNoErr(t, err, "children")
	for _, c := range children {
		if c.Status == protocol.StatusPending {
			t.Errorf("leg %d is still pending under an ended parent", c.ID)
		}
		if c.VendorOrderID == "" && c.Status == protocol.StatusCancelled {
			continue
		}
	}
	all, err := f.db.ListBins()
	testutil.MustNoErr(t, err, "bins")
	for _, b := range all {
		for _, c := range children {
			if b.ClaimedBy != nil && *b.ClaimedBy == c.ID && c.VendorOrderID == "" {
				t.Errorf("bin %s is still claimed by unsent leg %d of an ended parent", b.Label, c.ID)
			}
		}
	}
}

// The requester's own dig (proposeLaneClearDig) refuses an ended requester the
// same way and releases the lane it took in that requester's name.
func TestCompoundDig_RequesterDigForAnEndedRequester(t *testing.T) {
	t.Parallel()
	f := seedBuriedRetrieve(t, "PGONE5")
	f.cancel(t)
	target := f.sc.Slots[len(f.sc.Slots)-1]

	res := f.d.proposeLaneClearDig(f.sc.Lane, target, f.order)

	// The lane lock refuses the ended requester before the compound write can
	// (both are reservation-guarded); either refusal is a stop with nothing held.
	if res.outcome != laneClearParentGone && res.outcome != laneClearLaneBusy {
		t.Fatalf("outcome %v (%v), want a refusal: laneClearParentGone or the lane lock's laneClearLaneBusy", res.outcome, res.err)
	}
	f.nothingLeftBehind(t)
}

// The store refuses on its own: a leg written under a parent that has ended is
// refused inside the write's transaction, typed, and nothing is left behind.
// The dispatch layers above it catch the sequential cases too; this is the one
// that holds when the cancel's cascade lists the children before the write.
func TestCompoundDig_StoreRefusesALegUnderAnEndedParent(t *testing.T) {
	t.Parallel()
	f := seedBuriedRetrieve(t, "PGONE6")
	f.cancel(t)
	leg := &orders.Order{
		EdgeUUID: "PGONE6-leg", StationID: f.order.StationID, OrderType: OrderTypeMove, Status: StatusPending,
		ParentOrderID: &f.order.ID, Sequence: 1, BinID: &f.sc.Blockers[0].ID, SourceNode: f.sc.Slots[0].Name,
	}

	_, err := f.db.CreateCompoundChildren([]store.CompoundChild{{Order: leg, BinID: f.sc.Blockers[0].ID}})

	var gone *store.ParentGoneError
	if !errors.As(err, &gone) || gone.ParentID != f.order.ID {
		t.Fatalf("CreateCompoundChildren = %v, want ParentGoneError for parent %d", err, f.order.ID)
	}
	f.nothingLeftBehind(t)
	children, err := f.db.ListChildOrders(f.order.ID)
	testutil.MustNoErr(t, err, "children")
	if len(children) != 0 {
		t.Fatalf("%d leg(s) written under the ended parent", len(children))
	}
}
