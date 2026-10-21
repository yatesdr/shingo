//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/store"
)

// rowReadingEmitter reads an order's row at the moment it is projected.
// AdmitCoreAsk projects after the birth's INSERT and before ReplenishLoader
// gets control back, so this is a read at the same seam a scanner pass outside
// the door can take: after the row exists, before anything ReplenishLoader
// writes to it afterwards.
type rowReadingEmitter struct {
	mockEmitter
	t       *testing.T
	db      *store.DB
	payload map[string]string // edge uuid -> payload_code on the row when projected
}

func (e *rowReadingEmitter) ProjectOrder(stationID string, p protocol.OrderProjection) {
	e.mockEmitter.ProjectOrder(stationID, p)
	o, err := e.db.GetOrderByUUID(p.OrderUUID)
	testutil.MustNoErr(e.t, err, "read the projected order's row")
	e.payload[p.OrderUUID] = o.PayloadCode
}

// TestPinCoreAsk_TheLoaderAskIsBornWithItsPart — D34's second window. The
// loader's empty pull names the part it is for (the lead-time readers select
// on it, and the finder's carrier set comes from it). Its row must carry the
// part from the INSERT: a scanner pass that reads the order between its birth
// and a later payload write sees a payload-less ask.
func TestPinCoreAsk_TheLoaderAskIsBornWithItsPart(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	em := &rowReadingEmitter{t: t, db: db, payload: map[string]string{}}
	d := NewDispatcher(db, nil, em, "core", "shingo.dispatch", nil)
	d.DebugLog = testDebugLog(t)
	req, cfg, _ := replenishFixture(t, db, "RPB", 2)

	res, err := d.ReplenishLoader(req, cfg)
	testutil.MustNoErr(t, err, "ReplenishLoader")
	if len(res.Created) != 2 {
		t.Fatalf("created %d asks, want 2", len(res.Created))
	}
	for _, o := range res.Created {
		got, seen := em.payload[o.EdgeUUID]
		if !seen {
			t.Fatalf("ask %s was never projected", o.EdgeUUID)
		}
		if got != req.PayloadCode {
			t.Errorf("ask %s: payload_code on its row right after the birth = %q, want %q (a scanner pass "+
				"reading it there sees a payload-less ask)", o.EdgeUUID, got, req.PayloadCode)
		}
		row, err := db.GetOrder(o.ID)
		testutil.MustNoErr(t, err, "read the ask back")
		if row.PayloadCode != req.PayloadCode {
			t.Errorf("ask %s: payload_code = %q, want %q", o.EdgeUUID, row.PayloadCode, req.PayloadCode)
		}
	}
	// The station's projection is as it always was: a Core ask is projected
	// generic, so the Edge's row for it does not change.
	for _, p := range em.projected {
		if p.projection.PayloadCode != "" {
			t.Errorf("projection of %s carries payload %q; the station's row for a Core ask is generic", p.projection.OrderUUID, p.projection.PayloadCode)
		}
	}
}

// TestPinCoreAsk_TheAskIsReceivedAtBirth — a Core ask reports its receipt the
// moment it is born, as an Edge request does, so everything that hears an order
// arrive (the bins feed's push of the window it delivers to, the audit row,
// Core's own pages) hears this one at creation rather than at dispatch. The
// event carries the ask's part and its window.
func TestPinCoreAsk_TheAskIsReceivedAtBirth(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	em := &mockEmitter{}
	d := NewDispatcher(db, nil, em, "core", "shingo.dispatch", nil)
	d.DebugLog = testDebugLog(t)
	req, cfg, _ := replenishFixture(t, db, "RPR", 2)

	res, err := d.ReplenishLoader(req, cfg)
	testutil.MustNoErr(t, err, "ReplenishLoader")
	if len(res.Created) != 2 {
		t.Fatalf("created %d asks, want 2", len(res.Created))
	}
	got := map[int64]string{}
	for _, r := range em.received {
		got[r.orderID] = r.payloadCode
	}
	for _, o := range res.Created {
		payload, seen := got[o.ID]
		if !seen {
			t.Errorf("ask %d (%s) was born without a received event", o.ID, o.EdgeUUID)
			continue
		}
		if payload != req.PayloadCode {
			t.Errorf("ask %d received with payload %q, want %q", o.ID, payload, req.PayloadCode)
		}
	}
}
