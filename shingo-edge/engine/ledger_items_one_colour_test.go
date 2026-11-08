// ledger_items_one_colour_test.go — decision 55, the strip: every row of the
// ledger is a count a person clears, so its items are one colour, info. A
// share ("N held on CATID V", strokes counted on a part no cart of which
// stands for them) reads like a no-cart row; red stays the wrong-part item's,
// the PLC on a part the active style does not run with parts counted on it.
// RED at the overpack commit: the share item was red.
package engine

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/store/counters"
)

func TestPinStrip_TheLedgersItemsAreInfoTheWrongPartStaysRed(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, _ := holdRig(t, "1007")
	strokeAt(eng, pid, a, 3)
	if hs := openHolds(t, eng, pid); len(hs) != 1 || hs[0].Reason != holdReasonHeld {
		t.Fatalf("holds = %+v, want one share of the strokes on 1007", hs)
	}
	// The three strokes in the record, as the poll writes them.
	rp, err := eng.db.CreateReportingPoint("plc", "MES_Cell.Prod_Counter_01", a)
	testutil.MustNoErr(t, err, "the cell's counting point")
	_, err = eng.db.InsertCounterSnapshot(rp, 3, 3, "", counters.TickStamp{RecordedAt: time.Now().UTC(), ProcessID: pid, StyleID: a, CATID: "1007"})
	testutil.MustNoErr(t, err, "the strokes on 1007 in the record")
	kinds := healthKinds(t, eng, pid)
	if it, ok := kinds[catidKindHeld]; !ok || it.Severity != CatidHealthInfo {
		t.Errorf("the share's item %+v (present %t), want info: a ledger row is a count a person clears", it, ok)
	}
	if it, ok := kinds[catidKindWrongPart]; !ok || it.Severity != CatidHealthRed {
		t.Errorf("the wrong-part item %+v (present %t), want red: the PLC is on a part the active style does not run, with parts counted", it, ok)
	}
}
