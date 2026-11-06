// catid_slot_watch_test.go — the MES sibling slots: the watch reads the
// struct's five (CATID_0N, Prod_Counter_0N) pairs off the tick path, and the
// strip says when the PLC counts on a slot the process does not.
package engine

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/config"
	"shingoedge/domain"
	"shingoedge/plc"
)

// setSlots puts the five slots' CATIDs and counters of MES_Cell in the
// manager's cache, as WarLink publishes them.
func setSlots(eng *Engine, catids, counts [mesSlots]int64) {
	for i := 0; i < mesSlots; i++ {
		n := slotSuffix(i + 1)
		eng.plcMgr.SetTagValueForTest("plc", "MES_Cell.CATID_"+n, float64(catids[i]))
		eng.plcMgr.SetTagValueForTest("plc", "MES_Cell.Prod_Counter_"+n, float64(counts[i]))
	}
}

// slotItemsOf is the process's strip items of the slot kinds.
func slotItemsOf(t *testing.T, eng *Engine, pid int64) []CatidHealthItem {
	t.Helper()
	items, err := eng.CatidHealth(pid)
	testutil.MustNoErr(t, err, "health")
	var out []CatidHealthItem
	for _, it := range items {
		if strings.HasPrefix(it.Kind, "counter_slot_") {
			out = append(out, it)
		}
	}
	return out
}

// TestSlotItem_CountedSlotFlatWhileASiblingRises: the process counts _01; the
// PLC counts the part on _02, whose CATID names it. The strip says so, red,
// and names the slot its click re-points to. Once the counted slot moves, the
// item clears.
func TestSlotItem_CountedSlotFlatWhileASiblingRises(t *testing.T) {
	t.Parallel()
	eng, _, pid, cm, _ := primedRig(t)
	t0 := time.Now()
	setSlots(eng, [mesSlots]int64{1001, 1002, -1, -1, -1}, [mesSlots]int64{50, 10, 0, 0, 0})
	cm.watchSlots(t0)
	setSlots(eng, [mesSlots]int64{1001, 1002, -1, -1, -1}, [mesSlots]int64{50, 12, 0, 0, 0})
	cm.watchSlots(t0.Add(time.Minute))

	items := slotItemsOf(t, eng, pid)
	if len(items) != 1 || items[0].Kind != catidKindSlotIdle || items[0].Severity != CatidHealthRed {
		t.Fatalf("slot items = %+v, want one red %s", items, catidKindSlotIdle)
	}
	it := items[0]
	if it.Data["tag"] != "MES_Cell.Prod_Counter_02" || it.Data["catid"] != "1002" {
		t.Errorf("item data = %v, want the sibling's tag MES_Cell.Prod_Counter_02 and CATID 1002", it.Data)
	}
	if !strings.Contains(it.Headline, "Prod_Counter_02") || !strings.Contains(it.Headline, "Prod_Counter_01") {
		t.Errorf("headline %q names neither the slot the PLC counts on nor the one the process counts", it.Headline)
	}

	setSlots(eng, [mesSlots]int64{1001, 1002, -1, -1, -1}, [mesSlots]int64{51, 12, 0, 0, 0})
	cm.watchSlots(t0.Add(2 * time.Minute))
	for _, it := range slotItemsOf(t, eng, pid) {
		if it.Severity == CatidHealthRed {
			t.Errorf("the counted slot moved and the item stayed: %+v", it)
		}
	}
}

// TestSlotItem_UnusedSlotsAreNotParts: a slot reading -1 (unused) or 0 (no
// program) names no part; its counter moving while the counted slot is flat
// raises nothing.
func TestSlotItem_UnusedSlotsAreNotParts(t *testing.T) {
	t.Parallel()
	eng, _, pid, cm, _ := primedRig(t)
	t0 := time.Now()
	setSlots(eng, [mesSlots]int64{1001, -1, 0, -1, 0}, [mesSlots]int64{50, 10, 10, 0, 0})
	cm.watchSlots(t0)
	setSlots(eng, [mesSlots]int64{1001, -1, 0, -1, 0}, [mesSlots]int64{50, 14, 13, 0, 0})
	cm.watchSlots(t0.Add(time.Minute))
	if items := slotItemsOf(t, eng, pid); len(items) != 0 {
		t.Errorf("unused slots raised %+v", items)
	}
}

// TestSlotItem_LockstepSiblingIsInfo: a die pair counts each stroke on two
// slots, both naming a part, both moving together. That is a second counter
// of the same stroke: an info line, never the red item, suggesting nothing.
func TestSlotItem_LockstepSiblingIsInfo(t *testing.T) {
	t.Parallel()
	eng, _, pid, cm, _ := primedRig(t)
	t0 := time.Now()
	setSlots(eng, [mesSlots]int64{1001, 1002, -1, -1, -1}, [mesSlots]int64{400, 400, 0, 0, 0})
	cm.watchSlots(t0)
	setSlots(eng, [mesSlots]int64{1001, 1002, -1, -1, -1}, [mesSlots]int64{403, 403, 0, 0, 0})
	cm.watchSlots(t0.Add(time.Minute))
	items := slotItemsOf(t, eng, pid)
	if len(items) != 1 || items[0].Kind != catidKindSlotLockstep || items[0].Severity != CatidHealthInfo {
		t.Fatalf("slot items = %+v, want one info %s", items, catidKindSlotLockstep)
	}
	if !strings.Contains(items[0].Headline, "a second counter of the same stroke") {
		t.Errorf("headline = %q", items[0].Headline)
	}
}

// TestRepointToSlot_DerivesTheIdentityAndReprimes: the item's click is the
// counter edit to the sibling's tag. The identity follows by derivation
// (CATID_02), the monitor re-primes on it, and the point re-seeds from the new
// slot's first read. Characterization: green before the watch existed.
func TestRepointToSlot_DerivesTheIdentityAndReprimes(t *testing.T) {
	t.Parallel()
	eng, db, pid, cm, wl := primedRig(t)
	wl.tags = append(wl.tags, plc.WarlinkTagInfo{Name: "MES_Cell.Prod_Counter_02", Type: "DINT"})
	before, err := db.GetReportingPointByTag("plc", "MES_Cell.Prod_Counter_01")
	testutil.MustNoErr(t, err, "point")
	_, err = db.UpdateReportingPointCounter(before.ID, "plc", "MES_Cell.Prod_Counter_01", 50)
	testutil.MustNoErr(t, err, "seed the point")

	testutil.MustNoErr(t, eng.EditProcessCounter(pid, domain.ProcessCounterEdit{
		PLCName: "plc", TagName: "MES_Cell.Prod_Counter_02", Enabled: true}), "re-point to _02")
	p := mustProcess(t, db, pid)
	if got := identityTagOf(p); got != "MES_Cell.CATID_02" {
		t.Errorf("identity = %q, want MES_Cell.CATID_02 by derivation", got)
	}
	cm.mu.Lock()
	st := cm.states[pid]
	cm.mu.Unlock()
	if st == nil || st.tagName != "MES_Cell.CATID_02" || st.seenValue {
		t.Errorf("monitor state = %+v, want a fresh tracker on MES_Cell.CATID_02", st)
	}
	rp, err := db.GetReportingPoint(before.ID)
	testutil.MustNoErr(t, err, "point after")
	if rp.TagName != "MES_Cell.Prod_Counter_02" || rp.LastPollAt != nil {
		t.Errorf("point = %s, last poll %v; want the same point on _02, unread (it seeds at its first read)", rp.TagName, rp.LastPollAt)
	}
}

// blockingStructWarlink answers the struct read only when released, and
// counts the struct reads.
type blockingStructWarlink struct {
	recordingWarlink
	entered chan struct{}
	release chan struct{}
	reads   atomic.Int32
	once    sync.Once
}

func (w *blockingStructWarlink) ReadTagValue(ctx context.Context, _, tagName string) (any, error) {
	w.reads.Add(1)
	w.once.Do(func() { close(w.entered) })
	select {
	case <-w.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return map[string]any{"CATID_01": float64(1001), "Prod_Counter_01": float64(5),
		"CATID_02": float64(1002), "Prod_Counter_02": float64(7)}, nil
}

// TestSlotWatch_NeverTakesTheTickPath: the watch's struct read (an HTTP round
// trip) runs on its own goroutine and holds none of the monitor's locks: a
// tick and the guard's read finish while it waits, and the tick never reads a
// struct. Run under -race with the tick and the watch concurrent.
func TestSlotWatch_NeverTakesTheTickPath(t *testing.T) {
	t.Parallel()
	eng, _, pid, cm, _ := primedRig(t)
	wl := &blockingStructWarlink{entered: make(chan struct{}), release: make(chan struct{})}
	eng.plcMgr.ReplaceClient(wl)
	// Only the counted slot is published: the siblings come from the struct.
	eng.plcMgr.SetTagValueForTest("plc", "MES_Cell.CATID_01", float64(1001))
	eng.plcMgr.SetTagValueForTest("plc", "MES_Cell.Prod_Counter_01", float64(5))

	watched := make(chan struct{})
	go func() {
		defer close(watched)
		cm.watchSlots(time.Now())
	}()
	select {
	case <-wl.entered:
	case <-watched:
		t.Fatal("the watch finished without reading the struct for the unpublished siblings")
	case <-time.After(5 * time.Second):
		t.Fatal("the watch never read the struct")
	}

	ticked := make(chan struct{})
	go func() {
		defer close(ticked)
		for i := 0; i < 20; i++ {
			cm.tick(time.Now())
			_, _ = cm.liveCATID(pid)
		}
	}()
	select {
	case <-ticked:
	case <-time.After(5 * time.Second):
		t.Fatal("the tick waited on the watch's struct read")
	}
	if n := wl.reads.Load(); n != 1 {
		t.Errorf("struct reads = %d while the tick ran, want the watch's 1", n)
	}
	close(wl.release)
	<-watched
	f := cm.slotFactsOf(pid)
	if f == nil || f.slots[1].catid != "1002" || f.slots[1].count != 7 {
		t.Fatalf("published slots = %+v, want _02 read from the struct (1002, 7)", f)
	}
}

// TestSlotWatch_OneStatementAPass: a pass reads the process list and nothing
// else from the database, however many processes it watches: one statement a
// minute, none per tick.
func TestSlotWatch_OneStatementAPass(t *testing.T) {
	t.Parallel()
	db, counter := testEngineDBCounting(t)
	eng := testEngine(t, db)
	eng.plcMgr = plc.NewManager(nil, config.WarLinkConfig{}, 0, 0, nil, &recordingWarlink{})
	for _, name := range []string{"CELL-A", "CELL-B", "CELL-C"} {
		_, err := db.CreateProcess(name, "", "plc", "MES_"+name+".Prod_Counter_01", true)
		testutil.MustNoErr(t, err, "create process")
	}
	cm := &catidMonitor{eng: eng, states: map[int64]*catidState{}}
	counter.Reset()
	cm.watchSlots(time.Now())
	if got := counter.Count(); got != 1 {
		t.Errorf("a pass ran %d statements, want 1 (the process list)", got)
	}
	if m := cm.slots.Load(); m == nil || len(*m) != 3 {
		t.Errorf("published %v, want the three processes' slots", m)
	}
}
