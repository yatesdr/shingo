// station_health_test.go — the station counters' health lines.
package engine

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/plc"
	"shingoedge/store/processes"
)

// healthStation is a pair rig with one consume position counted at a station.
func healthStation(t *testing.T) (*pairRig, int64, int64) {
	t.Helper()
	r := newPairRig(t)
	n := r.consume("HS-1", "", "simple", true)
	lc := stationCounter(t, r.db, r.pid, n)
	return r, n, lc
}

func (r *pairRig) prodStroke(n int) {
	for i := 0; i < n; i++ {
		r.eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{ProcessID: r.pid, StyleID: r.style, Delta: 1}})
	}
}

func (r *pairRig) stationClick(lc int64, n int) {
	stationTick(r.eng, lc, r.pid, plc.TickIdentity{StyleID: r.style, Kind: plc.TickToday}, int64(n))
}

// TestHealth_QuietThroughARunOut: a cart's run-out (the line produces from the
// parts already in it while the station waits for the next cart) is not a
// stop; a stop deeper than the deepest run-out seen is, and never while a
// changeover is open.
func TestHealth_QuietThroughARunOut(t *testing.T) {
	t.Parallel()
	r, _, lc := healthStation(t)
	for i := 0; i < 5; i++ {
		r.stationClick(lc, 1)
		r.prodStroke(1)
	}
	r.prodStroke(16) // the run-out
	if _, ok := healthKinds(t, r.eng, r.pid)[stationKindStopped]; ok {
		t.Fatal("stopped during a 16-stroke run-out")
	}
	r.stationClick(lc, 1) // the next cart: the deepest run-out is 17 (the last stroke after a click, and 16)
	r.prodStroke(25)
	it, ok := healthKinds(t, r.eng, r.pid)[stationKindStopped]
	if !ok || !strings.Contains(it.Detail, "25 strokes") || !strings.Contains(it.Detail, "deepest run-out seen is 17") {
		t.Fatalf("after 25 strokes with no click: %+v, want stopped", it)
	}
	other, err := r.db.CreateStyle("PAIR-ALT", "", r.pid)
	testutil.MustNoErr(t, err, "second style")
	_, err = r.eng.changeoverService.Create(r.pid, &r.style, other, "test", "", nil, nil, nil, nil)
	testutil.MustNoErr(t, err, "open a changeover")
	if _, ok := healthKinds(t, r.eng, r.pid)[stationKindStopped]; ok {
		t.Error("stopped shown while a changeover is open")
	}
}

// TestHealth_ObservedRatioIgnoresTheStartUpLead: clicks before the line's
// first stroke land in one interval; the median of the last 20 reads one per
// part.
func TestHealth_ObservedRatioIgnoresTheStartUpLead(t *testing.T) {
	t.Parallel()
	r, _, lc := healthStation(t)
	r.stationClick(lc, 5) // the lead
	for i := 0; i < stationRatioWindow; i++ {
		r.stationClick(lc, 1)
		r.prodStroke(1)
	}
	if it, ok := healthKinds(t, r.eng, r.pid)[stationKindRatio]; ok {
		t.Errorf("ratio item after a start-up lead: %+v", it)
	}
}

// TestHealth_WeldGunRatioFires: a tag that clicks four times per part (a weld
// gun saved by mistake) reads "not one click per part".
func TestHealth_WeldGunRatioFires(t *testing.T) {
	t.Parallel()
	r, _, lc := healthStation(t)
	for i := 0; i < stationRatioWindow; i++ {
		r.stationClick(lc, 4)
		r.prodStroke(1)
	}
	it, ok := healthKinds(t, r.eng, r.pid)[stationKindRatio]
	if !ok || !strings.Contains(it.Headline, "not one click per part") || !strings.Contains(it.Detail, "4.0 clicks per part") {
		t.Errorf("ratio item = %+v, want 4 clicks per part", it)
	}
}

// TestHealth_UnreadableTagNamed: a failed read (a PLC not found included)
// names the counter and the failure; a good read clears it.
func TestHealth_UnreadableTagNamed(t *testing.T) {
	t.Parallel()
	r, _, lc := healthStation(t)
	c, err := r.db.GetLineCounter(lc)
	testutil.MustNoErr(t, err, "station counter")
	r.eng.Events.Emit(Event{Type: EventCounterReadError, Payload: CounterReadErrorEvent{PLCName: c.PLCName, TagName: c.TagName,
		Error: "PLC plc is not connected"}})
	it, ok := healthKinds(t, r.eng, r.pid)[counterKindUnreadable]
	if !ok || it.Severity != CatidHealthRed || !strings.Contains(it.Headline, "load station") || !strings.Contains(it.Detail, "not connected") {
		t.Fatalf("unreadable item = %+v", it)
	}
	r.eng.Events.Emit(Event{Type: EventCounterRead, Payload: CounterReadEvent{PLCName: c.PLCName, TagName: c.TagName, Value: 3}})
	if _, ok := healthKinds(t, r.eng, r.pid)[counterKindUnreadable]; ok {
		t.Error("unreadable outlived a good read")
	}
}

// TestHealth_BoundPositionTurnedPairNamed: a bound position whose claim later
// named an A/B partner is named — it is counted at the cell's counter now.
func TestHealth_BoundPositionTurnedPairNamed(t *testing.T) {
	t.Parallel()
	r, _, _ := healthStation(t)
	_, err := upsertClaimRetiredMode(r.db, processes.NodeClaimInput{StyleID: r.style, CoreNodeName: "HS-1", Role: "consume",
		SwapMode: "simple", PayloadCode: "PART-P", UOPCapacity: 100, InboundSource: "SRC", PairedCoreNode: "HS-2"})
	testutil.MustNoErr(t, err, "the claim names a partner")
	it, ok := healthKinds(t, r.eng, r.pid)[stationKindPaired]
	if !ok || !strings.Contains(it.Headline, "HS-1 became an A/B pair") {
		t.Errorf("paired item = %+v", it)
	}
}

// TestHealth_ACounterNeverClickedSinceBootShowsStopped: a station counter that
// has not clicked once since the Edge started (a mistyped member, or a station
// stopped across a restart) is stopped like one that clicked and then quit.
// Before, a counter with no click had no account, and the strip stayed silent.
func TestHealth_ACounterNeverClickedSinceBootShowsStopped(t *testing.T) {
	t.Parallel()
	r, _, _ := healthStation(t)
	r.prodStroke(200)
	it, ok := healthKinds(t, r.eng, r.pid)[stationKindStopped]
	if !ok || !strings.Contains(it.Detail, "200 strokes") {
		t.Fatalf("after 200 strokes and no click since boot: %+v, want stopped", it)
	}
}

// TestHealth_ANewCounterCountsFromItsCreation: a counter made while the line
// has been running counts its strokes from its creation, not from the boot.
func TestHealth_ANewCounterCountsFromItsCreation(t *testing.T) {
	t.Parallel()
	eng, pid, style, _ := stationRig(t)
	eng.wireEventHandlers()
	prod := func(n int) {
		for i := 0; i < n; i++ {
			eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{ProcessID: pid, StyleID: style, Delta: 1}})
		}
	}
	prod(100)
	node := bindNode(t, eng, pid, style, "NEW-1", "consume", "simple", "")
	lc, err := eng.CreateStationCounter(pid, "load station", "MES_Cell.Station_01")
	testutil.MustNoErr(t, err, "station counter")
	testutil.MustNoErr(t, eng.SetCountedAt(node, &lc.ID), "bind")
	if it, ok := healthKinds(t, eng, pid)[stationKindStopped]; ok {
		t.Fatalf("a counter made after 100 strokes is stopped at once: %+v", it)
	}
	prod(25)
	if _, ok := healthKinds(t, eng, pid)[stationKindStopped]; !ok {
		t.Error("25 strokes after its creation with no click: want stopped")
	}
}

// TestHealth_ARunOutNeverReadsAsNotOneClickPerPart: a run-out deeper than half
// the window (the line producing from the parts already in it while the
// station waits for a cart) is not a counter that counts something else.
// Before, its zero-click strokes filled the window and the median read 0.
func TestHealth_ARunOutNeverReadsAsNotOneClickPerPart(t *testing.T) {
	t.Parallel()
	r, _, lc := healthStation(t)
	for i := 0; i < stationRatioWindow; i++ {
		r.stationClick(lc, 1)
		r.prodStroke(1)
	}
	r.prodStroke(12) // the run-out
	if it, ok := healthKinds(t, r.eng, r.pid)[stationKindRatio]; ok {
		t.Errorf("ratio item through a 12-stroke run-out: %+v", it)
	}
}

// TestHealth_RatioQuietWhileAChangeoverIsOpen: across a changeover the station
// and the cell count different parts' strokes; the ratio says nothing then.
func TestHealth_RatioQuietWhileAChangeoverIsOpen(t *testing.T) {
	t.Parallel()
	r, _, lc := healthStation(t)
	for i := 0; i < stationRatioWindow; i++ {
		r.stationClick(lc, 4)
		r.prodStroke(1)
	}
	other, err := r.db.CreateStyle("PAIR-ALT", "", r.pid)
	testutil.MustNoErr(t, err, "second style")
	_, err = r.eng.changeoverService.Create(r.pid, &r.style, other, "test", "", nil, nil, nil, nil)
	testutil.MustNoErr(t, err, "open a changeover")
	if it, ok := healthKinds(t, r.eng, r.pid)[stationKindRatio]; ok {
		t.Errorf("ratio item while a changeover is open: %+v", it)
	}
}

// TestHealth_StoppedShowsThoughAnotherStylePairsThePosition: the running
// style claims the bound position unpaired (the station counts it); another
// style's claim there naming a partner does not hide "stopped". Before, the
// first style that paired it ended the position's check.
func TestHealth_StoppedShowsThoughAnotherStylePairsThePosition(t *testing.T) {
	t.Parallel()
	r, _, _ := healthStation(t)
	early, err := r.db.CreateStyle("A-EARLY", "", r.pid) // listed before the running style
	testutil.MustNoErr(t, err, "another style")
	_, err = upsertClaimRetiredMode(r.db, processes.NodeClaimInput{StyleID: early, CoreNodeName: "HS-1", Role: "consume",
		SwapMode: "simple", PayloadCode: "PART-E", UOPCapacity: 100, InboundSource: "SRC", PairedCoreNode: "HS-2"})
	testutil.MustNoErr(t, err, "the other style pairs it")
	r.prodStroke(stationStopFloor + 5)
	if _, ok := healthKinds(t, r.eng, r.pid)[stationKindStopped]; !ok {
		t.Error("stopped hidden by another style's pair at the position the running style counts there")
	}
}

// TestHealth_PairWarningNamesTheStyleThatPairsIt: a pair under another style
// counts the position at the cell's counter only while that style runs; the
// warning names it.
func TestHealth_PairWarningNamesTheStyleThatPairsIt(t *testing.T) {
	t.Parallel()
	r, _, _ := healthStation(t)
	other, err := r.db.CreateStyle("STYLE-PAIRS", "", r.pid)
	testutil.MustNoErr(t, err, "another style")
	_, err = upsertClaimRetiredMode(r.db, processes.NodeClaimInput{StyleID: other, CoreNodeName: "HS-1", Role: "consume",
		SwapMode: "simple", PayloadCode: "PART-E", UOPCapacity: 100, InboundSource: "SRC", PairedCoreNode: "HS-2"})
	testutil.MustNoErr(t, err, "the other style pairs it")
	it, ok := healthKinds(t, r.eng, r.pid)[stationKindPaired]
	if !ok || !strings.Contains(it.Headline, "STYLE-PAIRS") {
		t.Errorf("paired item = %+v, want the pairing style named", it)
	}
}
