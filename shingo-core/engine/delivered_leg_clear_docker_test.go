//go:build docker

package engine

import (
	"net/http"
	"slices"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
)

// delivered_leg_clear_docker_test.go: brief C item 5 (docket R87), ruled in the
// final brief's 1c (docket section 25, B21).
//
// The home clear's end (dispatch.EndClear) run while the clear's empty's move
// is delivered and not confirmed. EndClear cancels every live child whose bin
// has not left its pickup (splitLiftedLegs), and the parent with them when
// none is kept. It was a characterisation of the door cancelling the delivered
// move; the ruling (a leg that has put its bin down is finished, and it
// confirms) turned it into a pin of the door keeping it. The dig's doors are
// beside their fixtures in dispatch/delivered_leg_docker_test.go; this door
// needs a clear, and a clear is born here (clearRig).

// stopRecorder is the simulator with a record of the jobs Core asked it to
// stop.
type stopRecorder struct {
	*simulator.SimulatorBackend
	mu      sync.Mutex
	stopped []string
}

func (s *stopRecorder) CancelOrder(vendorOrderID string) error {
	s.mu.Lock()
	s.stopped = append(s.stopped, vendorOrderID)
	s.mu.Unlock()
	return s.SimulatorBackend.CancelOrder(vendorOrderID)
}

func (s *stopRecorder) stops() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.stopped)
}

// TestDeliveredLeg_EndClear: a home clear on the spot arm. The empty's move
// lifts the empty off the home, which sends the partial's move, and sets it
// down at the spot. Core records the delivery as the engine's delivered arm
// does up to its delivered write (handleOrderDelivered, then MarkDelivered,
// which runs the engine's own subscribers), and stops before the arm's
// confirm: a delivered order is not re-tracked on restart, so the empty's move
// stays delivered until the reconciliation sweep. Meanwhile the partial's move
// fails at the fleet (handleFleetOrderFailed's Fail), and the engine's failure
// subscriber ends the clear (clearLegFailed -> EndClear).
//
// Final brief 1c: the clear's end keeps the empty's move, which has set the
// emptied carrier down at the spot; its confirm lands, the fleet is not asked
// to stop its finished job, and the station is told the move confirmed. The
// carrier stays at the spot, unclaimed; the clear's parent is then cancelled by
// the clear's own chapter end, "the partial's move ended failed"; no order is
// made. (The move was cancelled from delivered with "clear ended: the
// partial's move failed", its finished job stopped, the station told it
// cancelled, a confirm after it refused, and the parent cancelled by EndClear
// with "the partial's move failed".)
func TestDeliveredLeg_EndClear(t *testing.T) {
	t.Parallel()
	var flt *stopRecorder
	r := newClearRigWith(t, "DLC", true, clearRigOpts{fleet: func(s *simulator.SimulatorBackend) fleet.Backend {
		flt = &stopRecorder{SimulatorBackend: s}
		return flt
	}})
	if ans := r.tap(t); ans.Status != http.StatusOK || ans.Body.Arm != protocol.ClearArmSpot {
		t.Fatalf("tap: %d %+v, want 200 spot", ans.Status, ans.Body)
	}
	parent := r.parent(t)
	partial, empties := r.children(t, parent.ID)
	if partial == nil || len(empties) != 1 {
		t.Fatalf("fixture: children partial=%v empties=%d, want the partial's move and one empty's move", partial, len(empties))
	}
	r.lift(t, empties[0].ID)
	if got := r.reload(t, partial.ID); got.VendorOrderID == "" {
		t.Fatalf("fixture: the empty's lift did not send the partial's move (%s, %q)", got.Status, got.QueueCause)
	}
	em := r.reload(t, empties[0].ID)
	r.eng.handleOrderDelivered(em)
	testutil.MustNoErr(t, r.d.Lifecycle().MarkDelivered(em, "fleet", protocol.OrderUpdate{Detail: "fleet state: FINISHED"}),
		"the delivered write")
	r.eng.after.settle()
	if got := r.reload(t, em.ID); got.Status != protocol.StatusDelivered {
		t.Fatalf("fixture: the empty's move is %q after its delivered write, want delivered", got.Status)
	}

	mark, stops := outboxMark(t, r.db), len(flt.stops())
	var lastOrder int64
	testutil.MustNoErr(t, r.db.DB.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM orders`).Scan(&lastOrder), "last order id")
	p := r.reload(t, partial.ID)
	testutil.MustNoErr(t, r.d.Lifecycle().Fail(p, p.Requester(), "fleet_failed", "fleet order failed"),
		"the fleet fails the partial's move")
	r.eng.after.settle()
	_, confirmErr := r.d.Lifecycle().ConfirmReceipt(em, em.Requester(), "auto_confirm_internal", 0)
	r.eng.after.settle() // the confirm's completion: the clear's chapter end

	got := r.reload(t, em.ID)
	hist, err := r.db.ListOrderHistory(em.ID)
	testutil.MustNoErr(t, err, "the empty's move's history")
	var steps []protocol.Status
	for _, h := range hist {
		steps = append(steps, h.Status)
	}
	t.Logf("record: empty's move %d %s (%q) completed=%v history=%v", got.ID, got.Status, got.ErrorDetail,
		got.CompletedAt != nil, steps)
	if got.Status != protocol.StatusConfirmed || got.ErrorDetail != "" || got.CompletedAt == nil {
		t.Errorf("the empty's move is %q (%q) completed=%v, want confirmed: it had set its carrier down",
			got.Status, got.ErrorDetail, got.CompletedAt != nil)
	}
	if want := []protocol.Status{protocol.StatusDispatched, protocol.StatusInTransit, protocol.StatusDelivered,
		protocol.StatusConfirmed}; len(steps) < len(want) || !slices.Equal(steps[len(steps)-len(want):], want) {
		t.Errorf("the empty's move passed through %v, want it to end %v", steps, want)
	}
	if confirmErr != nil {
		t.Errorf("the confirm after the clear's end returned %v, want it to land", confirmErr)
	}
	r.binOn(t, r.e.ID, r.x)
	if b, err := r.db.GetBin(r.e.ID); err != nil || b.ClaimedBy != nil {
		t.Errorf("the emptied carrier is claimed by %v (%v), want nobody", b.ClaimedBy, err)
	}
	r.binOn(t, r.p.ID, r.s)
	if got := flt.stops()[stops:]; len(got) != 0 {
		t.Errorf("fleet stops since the delivery: %v, want none: the empty's move's job had finished", got)
	}
	t.Logf("told about the empty's move: %v", tellsAfter(t, r.db, em.EdgeUUID, mark))
	requireTells(t, r.db, em.EdgeUUID, mark, "order.update confirmed "+clearStation)
	if pp := r.reload(t, parent.ID); pp.Status != protocol.StatusCancelled || pp.ErrorDetail != "the partial's move ended failed" {
		t.Errorf("the clear's parent is %q (%q), want cancelled by the clear's chapter end", pp.Status, pp.ErrorDetail)
	}
	t.Logf("told about the parent: %v", tellsAfter(t, r.db, parent.EdgeUUID, mark))
	var made int
	testutil.MustNoErr(t, r.db.DB.QueryRow(`SELECT count(*) FROM orders WHERE id > $1`, lastOrder).Scan(&made), "count new orders")
	if made != 0 {
		t.Errorf("%d order(s) made since the delivery, want none", made)
	}
}
