//go:build docker

package engine

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// clear_trade_docker_test.go — a home clear's two-robot trade (no pull, no
// open spot) against the fleet simulator: the lift order either way, both legs
// in one pass or neither, the second leg refused by the fleet, a fault on
// either leg, a leg whose drop is already appended, a death before the send,
// the parent's cancel, the refusals before any write, and the re-run reaching
// the trade.

// bornTrade births a trade without the confirm's advance: the parent and the
// partial's move written as the confirm writes them, then choose under the
// Door (no pull, no spot: the trade). Nothing is sent.
func (r *clearRig) bornTrade(t *testing.T) (parent, l1, l2 *orders.Order) {
	t.Helper()
	job, refusal := r.eng.resolveClearJob(r.home, clearStation, protocol.ClearHomeRequest{HomeNode: r.home.Name})
	if refusal != nil {
		t.Fatalf("resolve: %+v", refusal.Body)
	}
	birth, _, err := r.eng.birthClear(job)
	testutil.MustNoErr(t, err, "birth")
	e, err := r.db.GetBin(r.e.ID)
	testutil.MustNoErr(t, err, "E after the zero")
	var arm string
	r.eng.door(func() {
		arm, err = r.d.ChooseClearLocked(dispatch.ClearFacts{Parent: birth.Parent, Home: r.home, LoaderID: r.loaderID,
			EmptyBin: e, PartialAt: r.s.Name, Partial: birth.Partial})
	})
	testutil.MustNoErr(t, err, "choose")
	if arm != protocol.ClearArmTrade {
		t.Fatalf("choose took arm %q, want the trade", arm)
	}
	l1, l2 = r.tradeLegs(t, birth.Parent.ID)
	if l1.VendorOrderID != "" || l2.VendorOrderID != "" {
		t.Fatalf("choose sent a leg: partial's move vendor=%q, empty's move vendor=%q", l1.VendorOrderID, l2.VendorOrderID)
	}
	return birth.Parent, l1, l2
}

// cancelCode is the code on an order's cancelled history row, "" when none.
func (r *clearRig) cancelCode(t *testing.T, id int64) string {
	t.Helper()
	h, err := r.db.LatestOrderHistoryForStatus(id, protocol.StatusCancelled)
	testutil.MustNoErr(t, err, fmt.Sprintf("cancel history of %d", id))
	if h == nil {
		return ""
	}
	return h.Code
}

// TestClearTrade_LiftOrderEitherWay: the partial's move lifts first, while the
// empty's move is still only dispatched (no robot moving yet). Its lift
// appends the empty's move's drop at once (the dispatched row is asked), and
// the empty's lift appends the partial's — no floor needed either way. The
// other order is TestClearHome_FullBufferNoPull_BirthsTheTrade.
func TestClearTrade_LiftOrderEitherWay(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CTO", false)
	if ans := r.tap(t); ans.Body.Arm != protocol.ClearArmTrade {
		t.Fatalf("tap: %d %+v, want the trade", ans.Status, ans.Body)
	}
	parent := r.parent(t)
	l1, l2 := r.tradeLegs(t, parent.ID)
	if l2.Status != protocol.StatusDispatched {
		t.Fatalf("fixture: the empty's move must still be only dispatched, is %s", l2.Status)
	}
	r.lift(t, l1.ID)
	if got := r.reload(t, l2.ID); got.WaitIndex != 1 {
		t.Fatalf("P has left %s and the dispatched empty's move's drop is not appended (wait %d, status %s)",
			r.s.Name, got.WaitIndex, got.Status)
	}
	if got := r.reload(t, l1.ID); got.WaitIndex != 0 {
		t.Fatalf("the partial's move's drop on %s was appended before E left it", r.home.Name)
	}
	r.lift(t, l2.ID)
	if got := r.reload(t, l1.ID); got.WaitIndex != 1 {
		t.Fatalf("E has left %s and the partial's move's drop is not appended (wait %d)", r.home.Name, got.WaitIndex)
	}
	r.finish(t, l1.ID)
	r.finish(t, l2.ID)
	r.eng.after.settle()
	if got := r.reload(t, parent.ID); got.Status != protocol.StatusConfirmed {
		t.Errorf("parent after both landed: %s, want confirmed", got.Status)
	}
	r.binOn(t, r.e.ID, r.s)
	r.binOn(t, r.p.ID, r.home)
}

// TestClearTrade_BothLegsGoInOnePassOrNeither: a trade leg that cannot be sent
// holds its partner too, unlifted. With the partial claimed by a stranger
// (its move's bin claim would refuse), the advance sends neither leg and the
// empty's move carries the partner's cause; with the empty claimed instead,
// its own hand refuses and the partial's move is never reached. Once the claim
// is gone, the vacate floor's re-drive sends both in the one call.
func TestClearTrade_BothLegsGoInOnePassOrNeither(t *testing.T) {
	t.Parallel()
	for _, held := range []string{"partial", "empty"} {
		t.Run(held, func(t *testing.T) {
			t.Parallel()
			r := newClearRig(t, "CT1"+strings.ToUpper(held[:1]), false)
			parent, l1, l2 := r.bornTrade(t)
			stranger := &orders.Order{EdgeUUID: "ct1-stranger-" + held, StationID: "edge.other", OrderType: dispatch.OrderTypeMove,
				Status: protocol.StatusDispatched, Quantity: 1}
			testutil.MustNoErr(t, r.db.CreateOrder(stranger), "stranger")
			bin := r.p.ID
			if held == "empty" {
				bin = r.e.ID
			}
			_, err := r.db.DB.Exec(`UPDATE bins SET claimed_by = $1 WHERE id = $2`, stranger.ID, bin)
			testutil.MustNoErr(t, err, "a stranger claims the bin")

			testutil.MustNoErr(t, r.d.AdvanceCompoundOrder(parent.ID), "advance")
			l1, l2 = r.reload(t, l1.ID), r.reload(t, l2.ID)
			if l1.VendorOrderID != "" || l2.VendorOrderID != "" {
				t.Fatalf("one leg went without the other: partial's move vendor=%q, empty's move vendor=%q",
					l1.VendorOrderID, l2.VendorOrderID)
			}
			if dispatch.QueueCause(l2.QueueCause) != dispatch.CauseDigBlockerClaimed {
				t.Errorf("the held empty's move says %q, want %q", l2.QueueCause, dispatch.CauseDigBlockerClaimed)
			}
			for _, b := range []int64{r.e.ID, r.p.ID} {
				got, err := r.db.GetBin(b)
				testutil.MustNoErr(t, err, "bin")
				if got.ClaimedBy != nil && *got.ClaimedBy != stranger.ID {
					t.Errorf("bin %s is claimed by %d with neither leg sent", got.Label, *got.ClaimedBy)
				}
			}

			_, err = r.db.DB.Exec(`UPDATE bins SET claimed_by = NULL WHERE id = $1`, bin)
			testutil.MustNoErr(t, err, "the stranger lets go of the bin")
			r.d.RedriveVacateWaits(0)
			l1, l2 = r.reload(t, l1.ID), r.reload(t, l2.ID)
			if l1.VendorOrderID == "" || l2.VendorOrderID == "" {
				t.Fatalf("the floor did not send the pair: partial's move vendor=%q cause=%q, empty's move vendor=%q cause=%q",
					l1.VendorOrderID, l1.QueueCause, l2.VendorOrderID, l2.QueueCause)
			}
		})
	}
}

// refuseCreateFrom is the simulator refusing, once, the create whose first
// block is at one node: the fleet's blip on one leg.
type refuseCreateFrom struct {
	*simulator.SimulatorBackend
	at      string
	refused bool
}

func (f *refuseCreateFrom) CreateOrder(req fleet.CreateOrderRequest) (fleet.TransportOrderResult, error) {
	if !f.refused && len(req.Blocks) > 0 && req.Blocks[0].Location == f.at {
		f.refused = true
		return fleet.TransportOrderResult{}, fmt.Errorf("sim: create refused once at %s", f.at)
	}
	return f.SimulatorBackend.CreateOrder(req)
}

// TestPair_SecondLegFleetRefused_FirstLegUnwinds: the fleet refuses the trade's
// second create (the partial's move, lifting at S) after the empty's move is
// with the fleet. That is a death, not a demote: the refused leg is cancelled
// and the empty's move with it, both coded TermPeerTerminal, the vendor order
// the fleet took is cancelled, the job ends, and nothing is held. E keeps its
// zero on H; P stays on S. Every other pair keeps its demote
// (TestC2bPin_APairWhoseReturnIsRefusedKeepsItsLifter).
func TestPair_SecondLegFleetRefused_FirstLegUnwinds(t *testing.T) {
	t.Parallel()
	var wrapped *refuseCreateFrom
	r := newClearRigWith(t, "CTR", false, clearRigOpts{fleet: func(s *simulator.SimulatorBackend) fleet.Backend {
		wrapped = &refuseCreateFrom{SimulatorBackend: s}
		return wrapped
	}})
	wrapped.at = r.s.Name
	ans := r.tap(t)
	if ans.Status != http.StatusOK || ans.Body.Arm != protocol.ClearArmTrade {
		t.Fatalf("tap: %d %+v, want the trade born", ans.Status, ans.Body)
	}
	r.eng.after.settle()
	if !wrapped.refused {
		t.Fatal("fixture: the partial's move's create was never refused")
	}
	parent := r.parent(t)
	l1, l2 := r.tradeLegs(t, parent.ID)
	for _, o := range []*orders.Order{l1, l2} {
		if o.Status != protocol.StatusCancelled || r.cancelCode(t, o.ID) != string(protocol.TermPeerTerminal) {
			t.Errorf("order %d is %s (code %q), want cancelled %s", o.ID, o.Status, r.cancelCode(t, o.ID), protocol.TermPeerTerminal)
		}
	}
	if l2.VendorOrderID == "" {
		t.Fatal("the empty's move was never sent: the refusal was not of the second leg")
	}
	if sent := r.sim.GetOrder(l2.VendorOrderID); sent == nil || sent.State != "STOPPED" {
		t.Errorf("the empty's move's vendor order is %+v, want it cancelled at the fleet", sent)
	}
	if got := r.reload(t, parent.ID); got.Status != protocol.StatusCancelled {
		t.Errorf("parent %s, want the job ended", got.Status)
	}
	r.binOn(t, r.e.ID, r.home)
	r.binOn(t, r.p.ID, r.s)
	e, err := r.db.GetBin(r.e.ID)
	testutil.MustNoErr(t, err, "E")
	if e.PayloadCode != "" || e.UOPRemaining != 0 {
		t.Errorf("E lost its zero: payload=%q uop=%d", e.PayloadCode, e.UOPRemaining)
	}
	testdb.AssertNoHoldsAtRest(t, r.db, "after the trade's second leg was refused")
}

// TestClearTrade_EachLegsFailureCancelsTheOther: either leg failing before its
// lift ends the trade (clearLegFailed's trade arm, the swap's death rule): the
// other is cancelled TermPeerTerminal and its vendor order cancelled, the job
// ends, nothing is held. No re-run chooses again.
func TestClearTrade_EachLegsFailureCancelsTheOther(t *testing.T) {
	t.Parallel()
	for _, failing := range []string{"partial", "empty"} {
		t.Run(failing, func(t *testing.T) {
			t.Parallel()
			r := newClearRig(t, "CTF"+strings.ToUpper(failing[:1]), false)
			if ans := r.tap(t); ans.Body.Arm != protocol.ClearArmTrade {
				t.Fatalf("tap: %+v, want the trade", ans.Body)
			}
			parent := r.parent(t)
			l1, l2 := r.tradeLegs(t, parent.ID)
			dead, other := l1, l2
			if failing == "empty" {
				dead, other = l2, l1
			}
			testutil.MustNoErr(t, r.d.Lifecycle().Fail(dead, dead.StationID, "robot_fault", "the robot faulted"), "fail a leg")
			r.eng.after.settle()
			other = r.reload(t, other.ID)
			if other.Status != protocol.StatusCancelled || r.cancelCode(t, other.ID) != string(protocol.TermPeerTerminal) {
				t.Errorf("the other leg is %s (code %q), want cancelled %s", other.Status, r.cancelCode(t, other.ID), protocol.TermPeerTerminal)
			}
			if sent := r.sim.GetOrder(other.VendorOrderID); sent == nil || sent.State != "STOPPED" {
				t.Errorf("the other leg's vendor order is %+v, want it cancelled at the fleet", sent)
			}
			if got := r.reload(t, parent.ID); got.Status != protocol.StatusCancelled {
				t.Errorf("parent %s, want the job ended", got.Status)
			}
			if kids, err := r.db.ListChildOrders(parent.ID); err != nil || len(kids) != 2 {
				t.Errorf("%d children (%v), want the two legs and no re-run's", len(kids), err)
			}
			testdb.AssertNoHoldsAtRest(t, r.db, "after a trade leg failed")
		})
	}
}

// TestClearTrade_AnAppendedLegFinishesWhenItsPartnerDies: the empty's move
// lifts E (which appends the partial's move's drop on H: E has left it), then
// faults carrying E. The partial's move is not cancelled — the place it drops
// on is empty — and finishes: P comes home and the job is done. The kept leg
// is audited.
func TestClearTrade_AnAppendedLegFinishesWhenItsPartnerDies(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CTK", false)
	if ans := r.tap(t); ans.Body.Arm != protocol.ClearArmTrade {
		t.Fatalf("tap: %+v, want the trade", ans.Body)
	}
	parent := r.parent(t)
	l1, l2 := r.tradeLegs(t, parent.ID)
	r.lift(t, l2.ID)
	if got := r.reload(t, l1.ID); got.WaitIndex != 1 {
		t.Fatalf("fixture: the partial's move's drop must be appended once E left, wait %d", got.WaitIndex)
	}
	l2 = r.reload(t, l2.ID)
	testutil.MustNoErr(t, r.d.Lifecycle().Fail(l2, l2.StationID, "robot_fault", "the robot faulted"), "fail the empty's move")
	r.eng.after.settle()
	if got := r.reload(t, l1.ID); protocol.IsTerminal(got.Status) {
		t.Fatalf("the partial's move went %s with its drop already appended", got.Status)
	}
	audits, err := r.db.ListEntityAudit("order", l1.ID)
	testutil.MustNoErr(t, err, "audit")
	kept := false
	for _, a := range audits {
		kept = kept || a.Action == "trade_leg_kept"
	}
	if !kept {
		t.Error("the kept leg carries no trade_leg_kept audit")
	}
	r.lift(t, l1.ID)
	r.finish(t, l1.ID)
	r.eng.after.settle()
	r.binOn(t, r.p.ID, r.home)
	if got := r.reload(t, parent.ID); got.Status != protocol.StatusConfirmed {
		t.Errorf("parent %s, want confirmed: the partial came home", got.Status)
	}
}

// TestClearTrade_PartnerDiedBeforeSend_NoRerun: the trade is born and not yet
// sent; the empty's move is cancelled. The death rule cancels the partial's
// move, and the re-run never chooses again for it (the empty stands on the
// home held by nobody — the re-run's own state — but its partial's move is a
// trade leg). The chapter's end closes the job.
func TestClearTrade_PartnerDiedBeforeSend_NoRerun(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CTD", false)
	parent, l1, l2 := r.bornTrade(t)
	r.d.Lifecycle().CancelOrder(l2, l2.StationID, "operator cancelled", dispatch.CancelCause{})
	r.eng.after.settle()
	if got := r.reload(t, l1.ID); got.Status != protocol.StatusCancelled || r.cancelCode(t, l1.ID) != string(protocol.TermPeerTerminal) {
		t.Fatalf("the partial's move is %s (code %q), want cancelled by the death rule", got.Status, r.cancelCode(t, l1.ID))
	}
	if n := r.d.RerunClears(); n != 0 {
		t.Errorf("the floor re-ran %d clears after the trade died, want 0", n)
	}
	if kids, err := r.db.ListChildOrders(parent.ID); err != nil || len(kids) != 2 {
		t.Errorf("%d children (%v), want the two legs and no re-run's", len(kids), err)
	}
	testutil.MustNoErr(t, r.d.AdvanceCompoundOrder(parent.ID), "the chapter's end")
	if got := r.reload(t, parent.ID); got.Status != protocol.StatusCancelled {
		t.Errorf("parent %s, want the job ended", got.Status)
	}
}

// TestClearTrade_ParentCancelCancelsHeldCarriers: the parent is cancelled with
// the empty's move lifted and still held at its vacate wait (the partial's
// move appended but not lifted). The cascade cancels the partial's move (not
// carrying) and keeps the carrying one, as every cascade does; the death rule
// then cancels the held carrier, whose drop waits on a lift its cancelled
// partner will never make. Its bin is left on the robot for the cancel-return,
// which the carried-bin watch makes from robot telemetry (not in this rig: the
// sims record where it goes). With both drops appended, both legs carrying,
// the cancel keeps both and they land.
func TestClearTrade_ParentCancelCancelsHeldCarriers(t *testing.T) {
	t.Parallel()
	t.Run("held", func(t *testing.T) {
		t.Parallel()
		r := newClearRig(t, "CTCH", false)
		if ans := r.tap(t); ans.Body.Arm != protocol.ClearArmTrade {
			t.Fatalf("tap: %+v, want the trade", ans.Body)
		}
		parent := r.parent(t)
		l1, l2 := r.tradeLegs(t, parent.ID)
		r.lift(t, l2.ID)
		if got := r.reload(t, l2.ID); got.WaitIndex != 0 {
			t.Fatalf("fixture: the empty's move must be held after its lift, wait %d", got.WaitIndex)
		}
		r.d.CancelOrderWithCascade(r.reload(t, parent.ID), clearStation, "operator cancelled", dispatch.CancelCause{})
		r.eng.after.settle()
		for _, id := range []int64{l1.ID, l2.ID} {
			if got := r.reload(t, id); got.Status != protocol.StatusCancelled {
				t.Errorf("leg %d is %s after the parent's cancel, want cancelled", id, got.Status)
			}
		}
		if sent := r.sim.GetOrder(r.reload(t, l2.ID).VendorOrderID); sent == nil || sent.State != "STOPPED" {
			t.Errorf("the held carrier's vendor order is %+v, want it cancelled at the fleet", sent)
		}
	})
	t.Run("appended", func(t *testing.T) {
		t.Parallel()
		r := newClearRig(t, "CTCA", false)
		if ans := r.tap(t); ans.Body.Arm != protocol.ClearArmTrade {
			t.Fatalf("tap: %+v, want the trade", ans.Body)
		}
		parent := r.parent(t)
		l1, l2 := r.tradeLegs(t, parent.ID)
		r.lift(t, l2.ID)
		r.lift(t, l1.ID)
		r.d.CancelOrderWithCascade(r.reload(t, parent.ID), clearStation, "operator cancelled", dispatch.CancelCause{})
		r.eng.after.settle()
		for _, id := range []int64{l1.ID, l2.ID} {
			if got := r.reload(t, id); protocol.IsTerminal(got.Status) {
				t.Errorf("carrying leg %d with its drop appended went %s on the parent's cancel, want kept", id, got.Status)
			}
		}
		r.finish(t, l1.ID)
		r.finish(t, l2.ID)
		r.eng.after.settle()
		r.binOn(t, r.e.ID, r.s)
		r.binOn(t, r.p.ID, r.home)
	})
}

// TestClearHome_TradeThatCannotBeBorn_RefusedBeforeAnyWrite: no open spot and
// the partial's spot does not take the empty's carrier type, so the trade
// cannot be born: 400 with the one sentence, before anything is written — no
// order, no hold, the carrier untouched.
func TestClearHome_TradeThatCannotBeBorn_RefusedBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CTB", false)
	other := &bins.BinType{Code: "CTB-OTHER"}
	testutil.MustNoErr(t, r.db.CreateBinType(other), "another carrier type")
	testutil.MustNoErr(t, r.db.SetLoaderHomeBinTypes(r.loaderID, r.s.ID, []int64{other.ID}), "S takes the other type only")
	ans := r.tap(t)
	if ans.Status != http.StatusBadRequest || !strings.Contains(ans.Body.Error, r.s.Name+" does not take "+r.e.Label) ||
		ans.Body.DeltaEpoch != 0 {
		t.Fatalf("tap: %d %+v, want 400 naming %s and %s, nothing zeroed", ans.Status, ans.Body, r.s.Name, r.e.Label)
	}
	var orderRows int
	testutil.MustNoErr(t, r.db.DB.QueryRow(`SELECT COUNT(*) FROM orders`).Scan(&orderRows), "orders")
	e, err := r.db.GetBin(r.e.ID)
	testutil.MustNoErr(t, err, "E")
	if orderRows != 0 || e.PayloadCode != r.part || e.DeltaEpoch != r.e.DeltaEpoch {
		t.Errorf("written before the refusal: orders=%d E payload=%q epoch=%d", orderRows, e.PayloadCode, e.DeltaEpoch)
	}
	testdb.AssertNoHoldsAtRest(t, r.db, "after the refusal")
}

// TestClearHome_PromisedPartial_RefusedBeforeAnyWrite: the only partial of the
// home's part is promised to another order (its soft hold). The partial pick
// skips it, so the confirm is the pick's own 400 before anything is written
// (was: the zero recorded and the partial left where it stands).
func TestClearHome_PromisedPartial_RefusedBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CTP", true)
	holder := &orders.Order{EdgeUUID: "ctp-holder", StationID: "edge.other", OrderType: dispatch.OrderTypeRetrieve,
		Status: protocol.StatusQueued, Quantity: 1, PayloadCode: r.part}
	testutil.MustNoErr(t, r.db.CreateOrder(holder), "holder")
	testutil.MustNoErr(t, reservations.Acquire(r.db.DB, holder.ID, 0, r.p.ID, "test"), "the partial is promised")
	ans := r.tap(t)
	if ans.Status != http.StatusBadRequest || ans.Body.Error != "no buffer spot holds a partial of "+r.part || ans.Body.DeltaEpoch != 0 {
		t.Fatalf("tap: %d %+v, want 400 'no buffer spot holds a partial of %s', nothing zeroed", ans.Status, ans.Body, r.part)
	}
	if p := r.parent(t); p != nil {
		t.Errorf("a parent was born: %+v", p)
	}
	e, err := r.db.GetBin(r.e.ID)
	testutil.MustNoErr(t, err, "E")
	if e.PayloadCode != r.part || e.DeltaEpoch != r.e.DeltaEpoch {
		t.Errorf("the carrier was zeroed: payload=%q epoch=%d", e.PayloadCode, e.DeltaEpoch)
	}
}

// TestClearHome_RerunReachesTheTrade: the pull that took the carrier is
// cancelled before it lifts and no spot has opened. The re-run (on the order's
// end) reuses the live partial's move and births the trade from it: the empty's
// move is born, both legs are sent, and the carrier is not zeroed again.
func TestClearHome_RerunReachesTheTrade(t *testing.T) {
	t.Parallel()
	r := newClearRig(t, "CTE", false)
	far := &nodes.Node{Name: "CTE-FAR", Enabled: true}
	testutil.MustNoErr(t, r.db.CreateNode(far), "far node")
	r.d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: "cte-pull",
		OrderType: dispatch.OrderTypeRetrieveEmpty, DeliveryNode: far.Name, Quantity: 1})
	ans := r.tap(t)
	if ans.Body.Arm != protocol.ClearArmDemand {
		t.Fatalf("tap: %+v, want demand", ans.Body)
	}
	parent := r.parent(t)
	partial, _ := r.children(t, parent.ID)
	pull := testdb.RequireOrder(t, r.db, "cte-pull")
	r.d.Lifecycle().CancelOrder(pull, pull.StationID, "operator cancelled", dispatch.CancelCause{})
	r.eng.after.settle()

	l1, l2 := r.tradeLegs(t, parent.ID)
	if l1.ID != partial.ID {
		t.Errorf("a second partial's move %d (first %d)", l1.ID, partial.ID)
	}
	if l1.VendorOrderID == "" || l2.VendorOrderID == "" || l2.DeliveryNode != r.s.Name {
		t.Fatalf("after the re-run: partial's move vendor=%q, empty's move vendor=%q to %s, want the trade sent",
			l1.VendorOrderID, l2.VendorOrderID, l2.DeliveryNode)
	}
	e, err := r.db.GetBin(r.e.ID)
	testutil.MustNoErr(t, err, "E")
	if e.DeltaEpoch != ans.Body.DeltaEpoch {
		t.Errorf("the carrier was zeroed again: epoch %d, confirm's %d", e.DeltaEpoch, ans.Body.DeltaEpoch)
	}
}

// TestClearTrade_FirstLegFleetRefused_TheFloorSendsThePair: the fleet refuses
// the trade's first create (the empty's move, lifting at H). Nothing of the
// trade is with the fleet, so it is the ordinary demote: the empty's move rests
// queued under the fleet's cause, the partial's move is not sent, nothing dies.
// The vacate floor's re-drive sends the pair in one call.
func TestClearTrade_FirstLegFleetRefused_TheFloorSendsThePair(t *testing.T) {
	t.Parallel()
	var wrapped *refuseCreateFrom
	r := newClearRigWith(t, "CTQ", false, clearRigOpts{fleet: func(s *simulator.SimulatorBackend) fleet.Backend {
		wrapped = &refuseCreateFrom{SimulatorBackend: s}
		return wrapped
	}})
	wrapped.at = r.home.Name
	if ans := r.tap(t); ans.Status != http.StatusOK || ans.Body.Arm != protocol.ClearArmTrade {
		t.Fatalf("tap: %d %+v, want the trade born", ans.Status, ans.Body)
	}
	parent := r.parent(t)
	l1, l2 := r.tradeLegs(t, parent.ID)
	if !wrapped.refused || l2.VendorOrderID != "" || l1.VendorOrderID != "" || protocol.IsTerminal(l1.Status) || protocol.IsTerminal(l2.Status) {
		t.Fatalf("after the refusal: refused=%v, empty's move %s vendor=%q, partial's move %s vendor=%q; want both unsent and live",
			wrapped.refused, l2.Status, l2.VendorOrderID, l1.Status, l1.VendorOrderID)
	}
	if dispatch.QueueCause(l2.QueueCause) != dispatch.CauseFleetRefusedCreate {
		t.Errorf("the refused empty's move says %q, want %q", l2.QueueCause, dispatch.CauseFleetRefusedCreate)
	}
	r.d.RedriveVacateWaits(0)
	l1, l2 = r.tradeLegs(t, parent.ID)
	if l1.VendorOrderID == "" || l2.VendorOrderID == "" {
		t.Fatalf("the floor did not send the pair: partial's move vendor=%q, empty's move vendor=%q", l1.VendorOrderID, l2.VendorOrderID)
	}
}
