//go:build docker

package dispatch

// THE ASK (orders.asked_vendor_order_id), PINNED AT THE WRITES THAT OPEN AND
// CLOSE IT. The hand opens it with the vendor order id it is about to ask the
// fleet for; the fleet's answer closes it. While it is open no status move
// lands on the row except an ending, and an ending inside the create cancels
// the job the fleet then gives, keeping the job named until the fleet has let
// it go.

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingocore/fleet"
	"shingocore/internal/testdb"
)

// TestAsk_TheRowNamesTheJobUntilTheFleetAnswers: inside each create, refused or
// accepted, the row's ask is the vendor order id the fleet is being asked for;
// after a refusal it is closed and the order waits with no job named; after the
// yes the job is in vendor_order_id and the ask is closed. A fresh id each pass
// (the unanswered create, as it stands).
func TestAsk_TheRowNamesTheJobUntilTheFleetAnswers(t *testing.T) {
	t.Parallel()
	c := newCWCell(t, "ask-names")
	c.fleet.Inside = func(n int, req fleet.CreateOrderRequest) error {
		if got := c.row().AskedVendorOrderID; got != req.OrderID {
			t.Errorf("inside create %d: the row's ask is %q, want the id being asked for (%q)", n, got, req.OrderID)
		}
		if n <= 2 {
			return errCWRefused
		}
		return nil
	}
	for pass := 1; pass <= 3; pass++ {
		_, err := c.pass()
		row := c.row()
		if row.AskedVendorOrderID != "" {
			t.Errorf("after pass %d: the ask %q is still open", pass, row.AskedVendorOrderID)
		}
		if pass <= 2 {
			if err == nil || row.Status != StatusQueued || row.VendorOrderID != "" {
				t.Errorf("after refused pass %d: err %v, status %s, vendor %q; want a refusal, queued, no job",
					pass, err, row.Status, row.VendorOrderID)
			}
			continue
		}
		asked := c.fleet.Asked()
		if err != nil || row.Status != StatusDispatched || row.VendorOrderID != asked[len(asked)-1] {
			t.Errorf("after the accepted pass: err %v, status %s, vendor %q; want dispatched naming %q",
				err, row.Status, row.VendorOrderID, asked[len(asked)-1])
		}
	}
	if got := len(c.fleet.Asked()); got != 3 {
		t.Errorf("the fleet was asked %d times, want 3", got)
	}
}

// TestAsk_NoStatusMoveLandsOnARowBeingAskedFor: inside a create, a non-terminal
// status move from a FRESH read of the row (one that agrees with the status in
// the database) is refused with ConcurrentTransition, and the row is as it was:
// only the fleet's answer, or an ending, moves an order Core is asking for.
// The stale-copy moves are TestCreateWindow_StaleMovesAndARivalHandLose's.
func TestAsk_NoStatusMoveLandsOnARowBeingAskedFor(t *testing.T) {
	t.Parallel()
	c := newCWCell(t, "ask-no-move")
	tried := 0
	c.refuseThenTake(1, func(n int) {
		moves := []struct {
			name string
			move func() error
		}{
			{"Park", func() error {
				o := c.row()
				o.QueueCode = string(protocol.QueueWaitingForMaterial)
				return c.d.lifecycle.Park(o, "ask pin")
			}},
			{"MarkInTransit", func() error {
				return c.d.lifecycle.MarkInTransit(c.row(), "AMR-PIN", "ask pin", protocol.OrderUpdate{})
			}},
			{"MarkStaged", func() error { return c.d.lifecycle.MarkStaged(c.row(), "ask pin", protocol.OrderUpdate{}) }},
		}
		for _, m := range moves {
			before := c.row()
			if !protocol.IsValidTransition(before.Status, map[string]protocol.Status{
				"Park": StatusQueued, "MarkInTransit": StatusInTransit, "MarkStaged": StatusStaged,
			}[m.name]) {
				t.Fatalf("inside create %d: %s is illegal from %s, so the pin proves nothing", n, m.name, before.Status)
			}
			tried++
			if err := m.move(); !IsConcurrentTransition(err) {
				t.Errorf("inside create %d: %s from a fresh read = %v, want ConcurrentTransition", n, m.name, err)
			}
			after := c.row()
			if after.Status != before.Status || after.Version != before.Version {
				t.Errorf("inside create %d: %s moved the row %s v%d -> %s v%d", n, m.name,
					before.Status, before.Version, after.Status, after.Version)
			}
		}
	})
	if tried != 6 {
		t.Fatalf("tried %d moves, want 6 (3 in each of 2 creates)", tried)
	}
}

// cancelAnswerFleet is the create-window fleet whose answer to a cancel the
// test chooses.
type cancelAnswerFleet struct {
	*testdb.CreateWindowFleet
	refuseCancel bool
	cancels      []string
}

func (f *cancelAnswerFleet) CancelOrder(vendorOrderID string) error {
	f.cancels = append(f.cancels, vendorOrderID)
	if f.refuseCancel {
		return errors.New("ask pin: the fleet did not take the cancel")
	}
	return f.CreateWindowFleet.CancelOrder(vendorOrderID)
}

// TestAsk_ACancelInsideTheCreateKeepsTheJobNamed: an operator cancel lands
// inside the create (the ending is free while the ask is open), the fleet takes
// the job, and Core cancels it. The ask closes only once the fleet has taken
// the cancel. When the fleet does not take it, the job moves from the ask to
// the ended row's vendor_order_id as CREATED, in the terminate chase's
// population (ListEndedWithLiveVendorState), which asks the fleet about it and
// re-sends the terminate.
func TestAsk_ACancelInsideTheCreateKeepsTheJobNamed(t *testing.T) {
	t.Parallel()
	for _, refuse := range []bool{false, true} {
		t.Run(fmt.Sprintf("fleet refuses the cancel=%v", refuse), func(t *testing.T) {
			t.Parallel()
			c := newCWCell(t, fmt.Sprintf("ask-cancel-%v", refuse))
			f := &cancelAnswerFleet{CreateWindowFleet: c.fleet, refuseCancel: refuse}
			c.d, _ = newTestDispatcher(t, c.db, f)
			c.fleet.Inside = func(int, fleet.CreateOrderRequest) error {
				c.d.lifecycle.CancelOrder(c.row(), "edge.line1", "ask pin: operator cancel", CancelCause{})
				return nil
			}
			if _, err := c.pass(); err == nil {
				t.Fatal("the pass succeeded on an order cancelled inside its create")
			}
			job := c.fleet.Asked()[0]
			if len(f.cancels) != 1 || f.cancels[0] != job {
				t.Fatalf("cancels sent %v, want one for the job the fleet took (%s)", f.cancels, job)
			}
			row := c.row()
			if row.Status != StatusCancelled || row.AskedVendorOrderID != "" {
				t.Errorf("row %s with ask %q, want cancelled with the ask closed", row.Status, row.AskedVendorOrderID)
			}
			now := clock.Now().UTC()
			chased, err := c.db.ListEndedWithLiveVendorState(now.Add(-time.Hour), now.Add(time.Minute), 20)
			testutil.MustNoErr(t, err, "the terminate chase's population")
			inChase := false
			for _, o := range chased {
				inChase = inChase || o.ID == c.order.ID
			}
			if !refuse {
				if row.VendorOrderID != "" || inChase {
					t.Errorf("the fleet took the cancel, yet the row names job %q (chased: %v)", row.VendorOrderID, inChase)
				}
				return
			}
			if row.VendorOrderID != job || row.VendorState != "CREATED" || !inChase {
				t.Errorf("the fleet kept job %s: the row names %q as %q, chased %v; want it named, CREATED, chased",
					job, row.VendorOrderID, row.VendorState, inChase)
			}
		})
	}
}
