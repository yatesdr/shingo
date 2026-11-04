package plc

import "testing"

// attribution_test.go — the poll stamps a counted tick with the identity the
// engine decides (EventEmitter.AttributeTick) before the snapshot INSERT, so
// the row, the event and the shipped tick all carry it.

// attributingEmitter answers every AttributeTick with id.
type attributingEmitter struct {
	deltaRecorder
	id TickIdentity
}

func (a *attributingEmitter) AttributeTick(processID, pointStyleID int64) TickIdentity { return a.id }

// TestSnapshot_RowCarriesTheCATID: the stroke's row carries the PLC's part
// and the attributed style.
func TestSnapshot_RowCarriesTheCATID(t *testing.T) {
	t.Parallel()
	em := &attributingEmitter{}
	r := newTickRigEmitting(t, em)
	em.id = TickIdentity{StyleID: r.sty, CATID: "1002", Kind: TickTarget}
	r.pass(4)
	var catid string
	var style int64
	if err := r.db.QueryRow(`SELECT catid, style_id FROM counter_snapshots WHERE delta > 0 ORDER BY id DESC LIMIT 1`).Scan(&catid, &style); err != nil {
		t.Fatalf("read the row: %v", err)
	}
	if catid != "1002" || style != r.sty {
		t.Errorf("row = catid %q style %d, want 1002 on %d", catid, style, r.sty)
	}
}

// TestShipper_RowShipsUnderTheAttributedStyle: Core counts the stroke under
// the attributed style, not the reporting point's.
func TestShipper_RowShipsUnderTheAttributedStyle(t *testing.T) {
	t.Parallel()
	em := &attributingEmitter{}
	r := newTickRigEmitting(t, em)
	other, err := r.db.CreateStyle("STYLE-B", "", r.proc)
	if err != nil {
		t.Fatalf("style: %v", err)
	}
	em.id = TickIdentity{StyleID: other, CATID: "1002", Kind: TickTarget}
	r.pass(4)
	ticks := r.shippedTicks()
	if len(ticks) == 0 || ticks[len(ticks)-1].StyleID != other {
		t.Errorf("shipped = %+v, want the last tick under style %d", ticks, other)
	}
}
