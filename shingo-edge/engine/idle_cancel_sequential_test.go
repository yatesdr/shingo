package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// TestIdleCancel_ASequentialRemovalAtAnIdleLineGoesBackAlone: a sequential
// position (the release harness's, with a counter on its line) at a line silent
// two hours. Its Order A is driven through Core's waybill and staged notice to
// park at the line; it was asked for and parked two hours ago. Order B comes
// only with A's release at the line, so there is none: A is a lone parked
// order, sent back on its hour, the position switched off, and still no B
// after, at the next start too.
func TestIdleCancel_ASequentialRemovalAtAnIdleLineGoesBackAlone(t *testing.T) {
	t.Parallel()
	h := newRelHarness(t)
	h.sequentialAB(protocol.ClaimRoleConsume, true)
	var styleID int64
	testutil.MustNoErr(t, h.db.DB.QueryRow(`SELECT active_style_id FROM processes WHERE id=?`, h.processID).Scan(&styleID), "the line's style")
	_, err := h.db.DB.Exec(`UPDATE processes SET counter_plc_name='PLC1', counter_tag_name='COUNT' WHERE id=?`, h.processID)
	testutil.MustNoErr(t, err, "the line's counter")
	_, err = h.db.CreateReportingPoint("PLC1", "COUNT", styleID)
	testutil.MustNoErr(t, err, "its reporting point")
	twoHours := time.Now().Add(-2 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	_, err = h.db.DB.Exec(`UPDATE reporting_points SET last_poll_at=?, last_part_at=? WHERE plc_name='PLC1' AND tag_name='COUNT'`, twoHours, twoHours)
	testutil.MustNoErr(t, err, "the line's last part, two hours ago")
	h.eng.stationHealth.onRead("PLC1", "COUNT", "")

	coreWaybill(h, "removal", "AMR-1")
	coreStagedAt(h, "removal", 0)
	a := h.leg("removal")
	_, err = h.db.DB.Exec(`UPDATE orders SET staged_at=? WHERE id=?`, twoHours, a)
	testutil.MustNoErr(t, err, "A parked two hours ago")
	// The REQUEST that made A is a go; it too was two hours ago.
	testutil.MustNoErr(t, processes.GoReorder(h.db.DB, h.nodeID, time.Now().Add(-2*time.Hour)), "the REQUEST's go, two hours ago")
	if b := backfills(h); len(b) != 0 {
		t.Fatalf("A parked, not released: backfills %v, want none", b)
	}

	h.eng.idleCancelLines()

	if got := h.order("removal").Status; got != protocol.StatusCancelled {
		t.Errorf("Order A is %s after the idle hour, want cancelled (sent back)", got)
	}
	rt, err := h.db.GetProcessNodeRuntime(h.nodeID)
	testutil.MustNoErr(t, err, "runtime")
	if rt.ReorderHold != domain.ReorderHoldOff {
		t.Errorf("the position holds %q, want off", rt.ReorderHold)
	}
	h.eng.recoverSequentialBackfills()
	if b := backfills(h); len(b) != 0 {
		t.Errorf("after the send-back: backfills %v, want none", b)
	}
}
