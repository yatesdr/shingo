// station_counters_test.go — the station counters' save rules, their WarLink
// publishing, and the rules for counting a position at one.
package engine

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/plc"
	"shingoedge/store/processes"
)

// stationRig is counterRig (a wired process, a recording WarLink) whose
// WarLink also lists two station tags as DINT.
func stationRig(t *testing.T) (*Engine, int64, int64, *recordingWarlink) {
	t.Helper()
	eng, _, pid, style, wl := counterRig(t)
	wl.tags = append(wl.tags,
		plc.WarlinkTagInfo{Name: "MES_Cell.Station_01", Type: "DINT"},
		plc.WarlinkTagInfo{Name: "MES_Cell.Station_02", Type: "DINT"})
	return eng, pid, style, wl
}

func refusedSentence(t *testing.T, err error) string {
	t.Helper()
	var r *StationCounterRefused
	if !errors.As(err, &r) {
		t.Fatalf("err = %v, want a StationCounterRefused", err)
	}
	return r.Sentence
}

// TestStationCounterSave_RefusesWithoutAWiredCounter: a station counter reads
// beside the cell's production counter; a process with none is refused.
func TestStationCounterSave_RefusesWithoutAWiredCounter(t *testing.T) {
	t.Parallel()
	eng, _, _, _ := stationRig(t)
	other, err := eng.db.CreateProcess("UNWIRED", "", "", "", false)
	testutil.MustNoErr(t, err, "process")
	_, err = eng.CreateStationCounter(other, "load station", "MES_Cell.Station_01")
	if s := refusedSentence(t, err); !strings.Contains(s, "no production counter wired") {
		t.Errorf("refusal = %q", s)
	}
}

// TestStationCounterSave_RefusesATagAnotherCounterReads: the process's own
// counter tag, or another station counter's, is refused.
func TestStationCounterSave_RefusesATagAnotherCounterReads(t *testing.T) {
	t.Parallel()
	eng, pid, _, _ := stationRig(t)
	_, err := eng.CreateStationCounter(pid, "", "MES_Cell.Prod_Counter_01")
	if s := refusedSentence(t, err); !strings.Contains(s, "read by a production counter") {
		t.Errorf("the production tag: %q", s)
	}
	_, err = eng.CreateStationCounter(pid, "load station", "MES_Cell.Station_01")
	testutil.MustNoErr(t, err, "first station counter")
	_, err = eng.CreateStationCounter(pid, "op 20", "MES_Cell.Station_01")
	if s := refusedSentence(t, err); !strings.Contains(s, "already station counter") {
		t.Errorf("another station counter's tag: %q", s)
	}
}

// TestStationCounterSave_RealTagRefused: a tag WarLink declares REAL would
// charge truncated differences; refused, and its publishing is not left on.
func TestStationCounterSave_RealTagRefused(t *testing.T) {
	t.Parallel()
	eng, pid, _, wl := stationRig(t)
	_, err := eng.CreateStationCounter(pid, "", "MES_Cell.Cycle_Time_Average")
	if s := refusedSentence(t, err); !strings.Contains(s, "REAL") {
		t.Errorf("refusal = %q, want the declared type named", s)
	}
	if got := strings.Join(wl.toggles(), ","); strings.Contains(got, "on plc.MES_Cell.Cycle_Time_Average") &&
		!strings.Contains(got, "off plc.MES_Cell.Cycle_Time_Average") {
		t.Errorf("toggles = %s: the refused tag was left published", got)
	}
}

// TestStationCounter_ManagedLifecycle: the Edge publishes a tag WarLink was not
// publishing and records it as its own; a re-point publishes the new tag,
// unpublishes the old and re-seeds; a delete unpublishes.
func TestStationCounter_ManagedLifecycle(t *testing.T) {
	t.Parallel()
	eng, pid, _, wl := stationRig(t)
	lc, err := eng.CreateStationCounter(pid, "load station", "MES_Cell.Station_01")
	testutil.MustNoErr(t, err, "create")
	if !lc.WarlinkManaged || lc.LastPollAt != nil || lc.PLCName != "plc" {
		t.Fatalf("created = %+v, want managed, unread, on the process's PLC", lc)
	}
	testutil.MustNoErr(t, eng.UpdateStationCounter(lc.ID, "load station", "MES_Cell.Station_02"), "re-point")
	got, err := eng.db.GetLineCounter(lc.ID)
	testutil.MustNoErr(t, err, "read")
	if got.TagName != "MES_Cell.Station_02" || got.LastPollAt != nil || !got.WarlinkManaged {
		t.Errorf("re-pointed = %+v, want the new tag, unread, managed", got)
	}
	testutil.MustNoErr(t, eng.DeleteStationCounter(lc.ID), "delete")
	toggles := strings.Join(wl.toggles(), ",")
	for _, want := range []string{"on plc.MES_Cell.Station_01", "on plc.MES_Cell.Station_02",
		"off plc.MES_Cell.Station_01", "off plc.MES_Cell.Station_02"} {
		if !strings.Contains(toggles, want) {
			t.Errorf("toggles = %s, want %q", toggles, want)
		}
	}
}

// TestProcessCounter_RefusesAStationCountersTag: the process-counter save
// refuses a tag a station counter reads.
func TestProcessCounter_RefusesAStationCountersTag(t *testing.T) {
	t.Parallel()
	eng, pid, _, _ := stationRig(t)
	_, err := eng.CreateStationCounter(pid, "load station", "MES_Cell.Station_01")
	testutil.MustNoErr(t, err, "station counter")
	err = eng.EditProcessCounter(pid, domain.ProcessCounterEdit{PLCName: "plc", TagName: "MES_Cell.Station_01", Enabled: true})
	if !errors.Is(err, errStationTagRead) {
		t.Errorf("err = %v, want the station counter's tag refused", err)
	}
}

// bindRig adds a position at core claimed by style in role/mode (naming
// partner), returning the node id.
func bindNode(t *testing.T, eng *Engine, pid, style int64, core string, role protocol.ClaimRole, mode protocol.SwapMode, partner string) int64 {
	t.Helper()
	nodeID, err := eng.db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: core, Code: core, Name: core, Enabled: true})
	testutil.MustNoErr(t, err, "node "+core)
	if role == "" {
		return nodeID
	}
	in := processes.NodeClaimInput{StyleID: style, CoreNodeName: core, Role: role, SwapMode: mode, PayloadCode: "PART-B",
		UOPCapacity: 50, InboundSource: "SRC", PairedCoreNode: partner}
	if mode == protocol.SwapModeManualSwap {
		in.AutoConfirm = true
		in.OutboundDestination = "DEST"
	}
	_, err = upsertClaimRetiredMode(eng.db, in)
	testutil.MustNoErr(t, err, "claim "+core)
	return nodeID
}

// TestBind_Refusals: a position counted at a station must be a consume
// position some live style claims, under no live style a loader, a produce
// claim or an A/B pair; and the counter must be its own process's.
func TestBind_Refusals(t *testing.T) {
	t.Parallel()
	eng, pid, style, _ := stationRig(t)
	lc, err := eng.CreateStationCounter(pid, "load station", "MES_Cell.Station_01")
	testutil.MustNoErr(t, err, "station counter")
	other, err := eng.db.CreateStyle("ALT", "", pid)
	testutil.MustNoErr(t, err, "second style")
	ok := bindNode(t, eng, pid, style, "B-OK", "consume", "simple", "")
	produceElsewhere := bindNode(t, eng, pid, style, "B-PROD", "consume", "simple", "")
	_, err = upsertClaimRetiredMode(eng.db, processes.NodeClaimInput{StyleID: other, CoreNodeName: "B-PROD", Role: "produce",
		SwapMode: "simple", PayloadCode: "PART-B", UOPCapacity: 50, InboundSource: "SRC"})
	testutil.MustNoErr(t, err, "produce under another live style")
	loader := bindNode(t, eng, pid, style, "B-LOAD", "consume", protocol.SwapModeManualSwap, "")
	pair := bindNode(t, eng, pid, style, "B-PAIR", "consume", "simple", "B-PARTNER")
	unclaimed := bindNode(t, eng, pid, style, "B-NONE", "", "", "")

	testutil.MustNoErr(t, eng.SetCountedAt(ok, &lc.ID), "a consume position")
	for name, c := range map[string]struct {
		node int64
		want string
	}{
		"produce under another live style": {produceElsewhere, "produces under ALT"},
		"a loader":                         {loader, "is a loader"},
		"an A/B pair":                      {pair, "half of an A/B pair"},
		"claimed by no live style":         {unclaimed, "no live style consumes"},
	} {
		err := eng.SetCountedAt(c.node, &lc.ID)
		if s := refusedSentence(t, err); !strings.Contains(s, c.want) {
			t.Errorf("%s: %q, want %q", name, s, c.want)
		}
	}
	otherProc, err := eng.db.CreateProcess("OTHER", "", "plc", "MES_Other.Prod_Counter_01", true)
	testutil.MustNoErr(t, err, "another process")
	foreign := bindNode(t, eng, otherProc, style, "F-1", "", "", "")
	if s := refusedSentence(t, eng.SetCountedAt(foreign, &lc.ID)); !strings.Contains(s, "not a counter of") {
		t.Errorf("another process's position: %q", s)
	}
	testutil.MustNoErr(t, eng.SetCountedAt(ok, nil), "back to the production counter")
	node, err := eng.db.GetProcessNode(ok)
	testutil.MustNoErr(t, err, "node")
	if node.LineCounterID != nil {
		t.Errorf("unbound node still counted at %d", *node.LineCounterID)
	}
}

// TestBind_LogsTheTransition: a binding moves the parts already in the line
// between the cart and the counter to the other counter; it says so.
func TestBind_LogsTheTransition(t *testing.T) {
	t.Parallel()
	eng, pid, style, _ := stationRig(t)
	var mu sync.Mutex
	var lines []string
	eng.logFn = func(format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	lc, err := eng.CreateStationCounter(pid, "load station", "MES_Cell.Station_01")
	testutil.MustNoErr(t, err, "station counter")
	n := bindNode(t, eng, pid, style, "T-1", "consume", "simple", "")
	testutil.MustNoErr(t, eng.SetCountedAt(n, &lc.ID), "bind")
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, l := range lines {
		if strings.Contains(l, "T-1 moves from the production counter to station counter") && strings.Contains(l, "parts already in the line") {
			found = true
		}
	}
	if !found {
		t.Errorf("log = %q, want the transition named", lines)
	}
}
