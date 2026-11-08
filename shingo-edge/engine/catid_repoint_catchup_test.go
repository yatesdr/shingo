// catid_repoint_catchup_test.go — the one-click re-point counts the strokes
// the PLC made on the sibling slot since it moved the count there: the point
// is seeded at the sibling's count when the PLC moved, as the slot watch read
// it, never at its count at the click.
package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/plc"
	"shingoedge/store/counters"
)

// watchedRig is primedRig with Prod_Counter_02 declared a DINT (the click's
// type check) and the slot watch run over passes a minute apart from t0: each
// pass the counted slot's and the sibling's counts, both slots naming 1001.
func watchedRig(t *testing.T, passes [][2]int64) (eng *Engine, pid int64, cm *catidMonitor, t0 time.Time) {
	t.Helper()
	eng, _, pid, cm, wl := primedRig(t)
	wl.tags = append(wl.tags, plc.WarlinkTagInfo{Name: "MES_Cell.Prod_Counter_02", Type: "DINT"})
	eng.tickMonitor.Store(cm)
	t0 = time.Now().Add(-time.Duration(len(passes)) * time.Minute)
	for i, p := range passes {
		setSlots(eng, [mesSlots]int64{1001, 1001, -1, -1, -1}, [mesSlots]int64{p[0], p[1], 0, 0, 0})
		cm.watchSlots(t0.Add(time.Duration(i) * time.Minute))
	}
	return eng, pid, cm, t0
}

// click is the item's click: the counter edit to Prod_Counter_02, everything
// else as the process has it, with _02 reading now.
func click(t *testing.T, eng *Engine, pid, now int64) error {
	t.Helper()
	eng.plcMgr.SetTagValueForTest("plc", "MES_Cell.Prod_Counter_02", float64(now))
	return eng.EditProcessCounter(pid, domain.ProcessCounterEdit{PLCName: "plc", TagName: "MES_Cell.Prod_Counter_02", Enabled: true})
}

// pointOn02 is the reporting point on Prod_Counter_02.
func pointOn02(t *testing.T, eng *Engine) *counters.ReportingPoint {
	t.Helper()
	rp, err := eng.db.GetReportingPointByTag("plc", "MES_Cell.Prod_Counter_02")
	testutil.MustNoErr(t, err, "the point on _02")
	return rp
}

// TestRepoint_SeedsFromTheFirstRiseNotNow: the process counts Prod_Counter_01
// and the PLC moves its count to Prod_Counter_02. The click re-points the
// counter, and the point is seeded at _02's count when the PLC moved — the
// sibling's reading before it rose, as the watch read it — so its next read
// counts every stroke since, once; never at _02's count at the click, which
// lost them. Each row a shape of the move against the watch's minute:
//
//   - after a pass: _02 stands at 10 while _01 rises, then rises alone;
//   - the straddling pass: the move falls inside a pass, so both rise once
//     (_01 to its last count, _02 from 10), then _02 alone: still 10;
//   - a second counter of the same stroke (_02 rising with _01 every pass)
//     when _01 stops: _02's count at _01's last rise, 48 — which counts no
//     stroke twice, and may miss the ones inside that one pass.
func TestRepoint_SeedsFromTheFirstRiseNotNow(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		passes [][2]int64
		now    int64
		base   int64
	}{
		{"after a pass", [][2]int64{{50, 10}, {55, 10}, {55, 14}}, 17, 10},
		{"the straddling pass", [][2]int64{{50, 10}, {52, 10}, {55, 12}, {55, 15}}, 17, 10},
		{"a second counter of the same stroke", [][2]int64{{50, 40}, {55, 45}, {58, 48}, {58, 51}}, 53, 48},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			eng, pid, _, _ := watchedRig(t, row.passes)
			if items := slotItemsOf(t, eng, pid); len(items) != 1 || items[0].Kind != catidKindSlotIdle {
				t.Fatalf("slot items = %+v, want the red %s", items, catidKindSlotIdle)
			}
			testutil.MustNoErr(t, click(t, eng, pid, row.now), "the click")
			if rp := pointOn02(t, eng); rp.LastPollAt == nil || rp.LastCount != row.base {
				t.Errorf("the point on _02 after the click: count %d, read %v; want it seeded at %d, _02's count when the PLC "+
					"moved, so its next read counts the %d strokes since (unread, it seeds at %d and counts none of them)",
					rp.LastCount, rp.LastPollAt, row.base, row.now-row.base, row.now)
			}
		})
	}
}

// TestRepoint_WithNoReadingFromBeforeTheMoveSeedsAsToday: the watch's first
// pass since a start finds _01 standing and _02 rising: it never saw _01 count,
// so it cannot say when the PLC moved (a restart inside the window). The item
// says which strokes the record lacks — those since the record's last count on
// _01 — and the click seeds the point at its first read, as before.
func TestRepoint_WithNoReadingFromBeforeTheMoveSeedsAsToday(t *testing.T) {
	t.Parallel()
	eng, pid, _, _ := watchedRig(t, [][2]int64{{55, 10}, {55, 14}})
	rp, err := eng.db.GetReportingPointByTag("plc", "MES_Cell.Prod_Counter_01")
	testutil.MustNoErr(t, err, "the point on _01")
	last := time.Now().Add(-10 * time.Minute).Truncate(time.Minute)
	_, err = eng.db.InsertCounterSnapshot(rp.ID, 55, 1, "", counters.TickStamp{RecordedAt: last.UTC(), ProcessID: pid, CATID: "1001"})
	testutil.MustNoErr(t, err, "_01's last count in the record")
	items := slotItemsOf(t, eng, pid)
	if len(items) != 1 || items[0].Kind != catidKindSlotIdle {
		t.Fatalf("slot items = %+v, want the red %s", items, catidKindSlotIdle)
	}
	want := fmt.Sprintf("strokes since %s are not in the record", last.Format("2006-01-02 15:04"))
	if !strings.Contains(items[0].Detail, want) {
		t.Errorf("the item's detail %q, want it to say %q", items[0].Detail, want)
	}
	if got, ok := items[0].Data["not_recorded_since"].(time.Time); !ok || !got.Equal(last) {
		t.Errorf("the item's not_recorded_since = %v, want %v", items[0].Data["not_recorded_since"], last)
	}
	slots, err := eng.CountingSlots(pid)
	testutil.MustNoErr(t, err, "the Counting block's read")
	if s := slots[1]; s.NotRecordedSince == nil || !s.NotRecordedSince.Equal(last) {
		t.Errorf("the Counting block's _02 line: not recorded since %v, want %v", s.NotRecordedSince, last)
	}
	testutil.MustNoErr(t, click(t, eng, pid, 17), "the click")
	if rp := pointOn02(t, eng); rp.LastPollAt != nil {
		t.Errorf("the point on _02 after the click: count %d read %v; want it unread, seeding at its first read", rp.LastCount, rp.LastPollAt)
	}
}

// TestRepoint_RefusedMidChangeoverSeedsNothing: with a changeover open the
// click is refused as before, and the point stays where it reads.
func TestRepoint_RefusedMidChangeoverSeedsNothing(t *testing.T) {
	t.Parallel()
	eng, pid, _, _ := watchedRig(t, [][2]int64{{50, 10}, {55, 10}, {55, 14}})
	alt, err := eng.db.CreateStyle("ALT", "", pid)
	testutil.MustNoErr(t, err, "style ALT")
	p := mustProcess(t, eng.db, pid)
	_, err = eng.changeoverService.Create(pid, p.ActiveStyleID, alt, "test", "", nil, nil, nil, nil)
	testutil.MustNoErr(t, err, "a changeover")
	if err := click(t, eng, pid, 17); !errors.Is(err, ErrChangeoverActive) {
		t.Fatalf("the click mid-changeover: %v, want %v", err, ErrChangeoverActive)
	}
	if rp, err := eng.db.GetReportingPointByTag("plc", "MES_Cell.Prod_Counter_02"); err == nil && rp != nil {
		t.Errorf("a point on _02 after a refused click: %+v", rp)
	}
	if p := mustProcess(t, eng.db, pid); p.CounterTagName != "MES_Cell.Prod_Counter_01" {
		t.Errorf("the process counts %s after a refused click", p.CounterTagName)
	}
}

// TestRepoint_CatchUpWaitsForTheNewSlotsIdentity: the seeded point's strokes
// wait for the monitor's first read of CATID_02 (the poll asks catchUpHeld
// each pass and writes nothing while it holds), so the read that counts them
// is attributed by the part the PLC reports on the new slot; and they wait no
// longer than catchUpWait for a read that never comes.
func TestRepoint_CatchUpWaitsForTheNewSlotsIdentity(t *testing.T) {
	t.Parallel()
	eng, pid, cm, _ := watchedRig(t, [][2]int64{{50, 10}, {55, 10}, {55, 14}})
	testutil.MustNoErr(t, click(t, eng, pid, 17), "the click")
	const plcName, tag = "plc", "MES_Cell.Prod_Counter_02"
	if !eng.catchUpHeld(plcName, tag) {
		t.Fatal("the point counts before the monitor has read CATID_02: its strokes would carry no part")
	}
	p := mustProcess(t, eng.db, pid)
	if id := eng.attributeTick(pid, *p.ActiveStyleID); id.CATID != "" {
		t.Fatalf("attribution before the read = %+v, want no part yet", id)
	}
	cm.tick(time.Now())
	if eng.catchUpHeld(plcName, tag) {
		t.Fatal("the point still held after the monitor read CATID_02")
	}
	if id := eng.attributeTick(pid, *p.ActiveStyleID); id.CATID != "1001" {
		t.Errorf("attribution of the catch-up's read = %+v, want CATID_02's 1001", id)
	}

	eng2, pid2, _, _ := watchedRig(t, [][2]int64{{50, 10}, {55, 10}, {55, 14}})
	testutil.MustNoErr(t, click(t, eng2, pid2, 17), "the click")
	v, ok := eng2.catchUps.Load(catchUpKey(plcName, tag))
	if !ok {
		t.Fatal("no catch-up held after the click")
	}
	c := *v.(*catchUp)
	c.until = time.Now().Add(-time.Millisecond)
	eng2.catchUps.Store(catchUpKey(plcName, tag), &c)
	if eng2.catchUpHeld(plcName, tag) {
		t.Error("the point held past catchUpWait with no read of CATID_02")
	}
}
