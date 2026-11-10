// station_counters_test.go — the station counters' save rules, their WarLink
// publishing, and the rules for counting a position at one.
package engine

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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
		plc.WarlinkTagInfo{Name: "Sta010.Prod_Tool_1_CNT.ACC", Type: "DINT"},
		plc.WarlinkTagInfo{Name: "Sta020.Prod_Tool_1_CNT.ACC", Type: "DINT"})
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
	_, err = eng.CreateStationCounter(other, "load station", "Sta010.Prod_Tool_1_CNT.ACC", "")
	if s := refusedSentence(t, err); !strings.Contains(s, "no production counter wired") {
		t.Errorf("refusal = %q", s)
	}
}

// TestStationCounterSave_RefusesATagAnotherCounterReads: the process's own
// counter tag (a member of its MES struct), another process's counter tag, or
// another station counter's, is refused.
func TestStationCounterSave_RefusesATagAnotherCounterReads(t *testing.T) {
	t.Parallel()
	eng, pid, _, _ := stationRig(t)
	_, err := eng.CreateStationCounter(pid, "", "MES_Cell.Prod_Counter_01", "")
	if s := refusedSentence(t, err); !strings.Contains(s, "MES struct") {
		t.Errorf("the process's own production tag: %q", s)
	}
	other, err := eng.db.CreateProcess("OTHER", "", "plc", "MES_Other.Prod_Counter_01", true)
	testutil.MustNoErr(t, err, "another process")
	style, err := eng.db.CreateStyle("OTHER-RUN", "", other)
	testutil.MustNoErr(t, err, "its style")
	testutil.MustNoErr(t, eng.db.SetActiveStyle(other, &style), "active")
	testutil.MustNoErr(t, eng.syncPrimaryPoint(other, "", ""), "its point")
	_, err = eng.CreateStationCounter(pid, "", "MES_Other.Prod_Counter_01", "")
	if s := refusedSentence(t, err); !strings.Contains(s, "read by a production counter") {
		t.Errorf("another process's production tag: %q", s)
	}
	_, err = eng.CreateStationCounter(pid, "load station", "Sta010.Prod_Tool_1_CNT.ACC", "")
	testutil.MustNoErr(t, err, "first station counter")
	_, err = eng.CreateStationCounter(pid, "second station", "Sta010.Prod_Tool_1_CNT.ACC", "")
	if s := refusedSentence(t, err); !strings.Contains(s, "already station counter") {
		t.Errorf("another station counter's tag: %q", s)
	}
}

// TestStationCounterSave_RealTagRefused: a tag WarLink declares REAL would
// charge truncated differences; refused, and its publishing is not left on.
func TestStationCounterSave_RealTagRefused(t *testing.T) {
	t.Parallel()
	eng, pid, _, wl := stationRig(t)
	wl.tags = append(wl.tags, plc.WarlinkTagInfo{Name: "Sta010.Dwell_Time", Type: "REAL"})
	_, err := eng.CreateStationCounter(pid, "", "Sta010.Dwell_Time", "")
	if s := refusedSentence(t, err); !strings.Contains(s, "REAL") {
		t.Errorf("refusal = %q, want the declared type named", s)
	}
	if got := strings.Join(wl.toggles(), ","); strings.Contains(got, "on plc.Sta010.Dwell_Time") &&
		!strings.Contains(got, "off plc.Sta010.Dwell_Time") {
		t.Errorf("toggles = %s: the refused tag was left published", got)
	}
}

// TestStationCounter_ManagedLifecycle: the Edge publishes a tag WarLink was not
// publishing and records it as its own; a re-point publishes the new tag,
// unpublishes the old and re-seeds; a delete unpublishes.
func TestStationCounter_ManagedLifecycle(t *testing.T) {
	t.Parallel()
	eng, pid, _, wl := stationRig(t)
	lc, err := eng.CreateStationCounter(pid, "load station", "Sta010.Prod_Tool_1_CNT.ACC", "")
	testutil.MustNoErr(t, err, "create")
	if !lc.WarlinkManaged || lc.LastPollAt != nil || lc.PLCName != "plc" {
		t.Fatalf("created = %+v, want managed, unread, on the process's PLC", lc)
	}
	testutil.MustNoErr(t, eng.UpdateStationCounter(lc.ID, "load station", "Sta020.Prod_Tool_1_CNT.ACC", nil), "re-point")
	got, err := eng.db.GetLineCounter(lc.ID)
	testutil.MustNoErr(t, err, "read")
	if got.TagName != "Sta020.Prod_Tool_1_CNT.ACC" || got.LastPollAt != nil || !got.WarlinkManaged {
		t.Errorf("re-pointed = %+v, want the new tag, unread, managed", got)
	}
	testutil.MustNoErr(t, eng.DeleteStationCounter(lc.ID), "delete")
	toggles := strings.Join(wl.toggles(), ",")
	for _, want := range []string{"on plc.Sta010.Prod_Tool_1_CNT.ACC", "on plc.Sta020.Prod_Tool_1_CNT.ACC",
		"off plc.Sta010.Prod_Tool_1_CNT.ACC", "off plc.Sta020.Prod_Tool_1_CNT.ACC"} {
		if !strings.Contains(toggles, want) {
			t.Errorf("toggles = %s, want %q", toggles, want)
		}
	}
}

// TestStationCounter_SecondTag: a counter reads a second tag (one fixture
// per hand): the same tag twice is refused, as is a second tag another
// counter or the production counter would read; each tag is published and
// recorded as the Edge's; an update that removes the second tag unpublishes
// it and leaves the first counting (its reading kept); an update that leaves
// tag_name_2 out keeps it; a delete unpublishes both.
func TestStationCounter_SecondTag(t *testing.T) {
	t.Parallel()
	eng, pid, _, wl := stationRig(t)
	const lh, rh = "Sta020_Fix1_T1.Prod_Tool_1_CNT.ACC", "Sta020_Fix2_T1.Prod_Tool_1_CNT.ACC"
	wl.tags = append(wl.tags, plc.WarlinkTagInfo{Name: lh, Type: "DINT"}, plc.WarlinkTagInfo{Name: rh, Type: "DINT"})
	_, err := eng.CreateStationCounter(pid, "op 20", lh, strings.ToLower(lh))
	if s := refusedSentence(t, err); !strings.Contains(s, "already the counter's first tag") {
		t.Errorf("the same tag twice: %q", s)
	}
	lc, err := eng.CreateStationCounter(pid, "op 20", lh, rh)
	testutil.MustNoErr(t, err, "create with two tags")
	if lc.TagName != lh || lc.TagName2 != rh || !lc.WarlinkManaged || !lc.WarlinkManaged2 || lc.LastPollAt2 != nil {
		t.Fatalf("created = %+v, want both tags, each managed, the second unread", lc)
	}
	_, err = eng.CreateStationCounter(pid, "op 20 again", "Sta010.Prod_Tool_1_CNT.ACC", rh)
	if s := refusedSentence(t, err); !strings.Contains(s, "already station counter") {
		t.Errorf("another counter's second tag: %q", s)
	}
	err = eng.EditProcessCounter(pid, domain.ProcessCounterEdit{PLCName: "plc", TagName: rh, Enabled: true})
	if !errors.Is(err, errStationTagRead) {
		t.Errorf("the production counter on a second tag: %v, want refused", err)
	}

	_, err = eng.db.UpdateLineCounterRead(lc.ID, "plc", lh, false, 40, time.Now())
	testutil.MustNoErr(t, err, "the first tag's read")
	none := ""
	testutil.MustNoErr(t, eng.UpdateStationCounter(lc.ID, "op 20", lh, &none), "remove the second tag")
	got, err := eng.db.GetLineCounter(lc.ID)
	testutil.MustNoErr(t, err, "read")
	if got.TagName2 != "" || got.WarlinkManaged2 || got.LastCount != 40 || got.LastPollAt == nil || !got.WarlinkManaged {
		t.Errorf("after removing the second tag = %+v, want it gone and the first still read at 40, managed", got)
	}
	if toggles := strings.Join(wl.toggles(), ","); !strings.Contains(toggles, "off plc."+rh) ||
		strings.Contains(toggles, "off plc."+lh) {
		t.Errorf("toggles = %s, want only the second tag unpublished", toggles)
	}

	second := rh
	testutil.MustNoErr(t, eng.UpdateStationCounter(lc.ID, "op 20", "", &second), "add it back, the first kept")
	testutil.MustNoErr(t, eng.UpdateStationCounter(lc.ID, "op 20 load", "", nil), "rename, tag_name_2 left out")
	got, err = eng.db.GetLineCounter(lc.ID)
	testutil.MustNoErr(t, err, "read")
	if got.Label != "op 20 load" || got.TagName != lh || got.LastCount != 40 || got.TagName2 != rh ||
		!got.WarlinkManaged2 || got.LastPollAt2 != nil {
		t.Errorf("after adding it back and a rename = %+v, want both tags, the first's reading kept, the second unread", got)
	}

	testutil.MustNoErr(t, eng.DeleteStationCounter(lc.ID), "delete")
	toggles := strings.Join(wl.toggles(), ",")
	if strings.Count(toggles, "off plc."+rh) != 2 || !strings.Contains(toggles, "off plc."+lh) {
		t.Errorf("toggles = %s, want both tags unpublished at the delete", toggles)
	}
}

// TestStationCounter_RefusesAMemberOfTheProcessMESStruct: the process's own
// MES struct holds its counter slots and their identities; a sibling slot
// saved as a station counter would count the cell's strokes a second time.
// Any member of it is refused, on create and on a re-point (Logix names are
// case-insensitive); a station's own struct is not.
func TestStationCounter_RefusesAMemberOfTheProcessMESStruct(t *testing.T) {
	t.Parallel()
	eng, pid, _, wl := stationRig(t)
	wl.tags = append(wl.tags,
		plc.WarlinkTagInfo{Name: "MES_Cell.Prod_Counter_02", Type: "DINT"},
		plc.WarlinkTagInfo{Name: "MES_Cell.CATID_03", Type: "DINT"})
	_, err := eng.CreateStationCounter(pid, "sibling slot", "MES_Cell.Prod_Counter_02", "")
	if s := refusedSentence(t, err); !strings.Contains(s, "MES_Cell") {
		t.Errorf("refusal = %q, want the struct named", s)
	}
	lc, err := eng.CreateStationCounter(pid, "load station", "Sta010.Prod_Tool_1_CNT.ACC", "")
	testutil.MustNoErr(t, err, "a station's own struct")
	err = eng.UpdateStationCounter(lc.ID, "load station", "mes_cell.CATID_03", nil)
	if s := refusedSentence(t, err); !strings.Contains(s, "MES_Cell") {
		t.Errorf("re-point refusal = %q, want the struct named", s)
	}
}

// TestProcessCounter_RefusesAStationCountersTag: the process-counter save
// refuses a tag a station counter reads.
func TestProcessCounter_RefusesAStationCountersTag(t *testing.T) {
	t.Parallel()
	eng, pid, _, _ := stationRig(t)
	_, err := eng.CreateStationCounter(pid, "load station", "Sta010.Prod_Tool_1_CNT.ACC", "")
	testutil.MustNoErr(t, err, "station counter")
	err = eng.EditProcessCounter(pid, domain.ProcessCounterEdit{PLCName: "plc", TagName: "Sta010.Prod_Tool_1_CNT.ACC", Enabled: true})
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
	lc, err := eng.CreateStationCounter(pid, "load station", "Sta010.Prod_Tool_1_CNT.ACC", "")
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
	lc, err := eng.CreateStationCounter(pid, "load station", "Sta010.Prod_Tool_1_CNT.ACC", "")
	testutil.MustNoErr(t, err, "station counter")
	n := bindNode(t, eng, pid, style, "T-1", "consume", "simple", "")
	testutil.MustNoErr(t, eng.SetCountedAt(n, &lc.ID), "bind")
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, l := range lines {
		if strings.Contains(l, "T-1 moves from the production counter to station counter") {
			found = true
		}
	}
	if !found {
		t.Errorf("log = %q, want the transition named", lines)
	}
}

// TestProcessDelete_DeletesItsStationCountersAndFreesTheirTags: a deleted
// process takes its station counters with it, unpublishes the tags the Edge
// published, and leaves the tag free for another process. Before, the rows
// stayed and the tag was refused everywhere for good.
func TestProcessDelete_DeletesItsStationCountersAndFreesTheirTags(t *testing.T) {
	t.Parallel()
	eng, pid, _, wl := stationRig(t)
	lc, err := eng.CreateStationCounter(pid, "load station", "Sta010.Prod_Tool_1_CNT.ACC", "")
	testutil.MustNoErr(t, err, "station counter")
	testutil.MustNoErr(t, eng.DeleteProcess(pid), "delete the process")
	if got, err := eng.db.GetLineCounter(lc.ID); err != nil || got != nil {
		t.Fatalf("station counter after its process's delete = %+v (%v), want gone", got, err)
	}
	if toggles := strings.Join(wl.toggles(), ","); !strings.Contains(toggles, "off plc.Sta010.Prod_Tool_1_CNT.ACC") {
		t.Errorf("toggles = %s, want the station tag unpublished", toggles)
	}
	again, err := eng.db.CreateProcess("CELL-2", "", "plc", "MES_Other.Prod_Counter_01", true)
	testutil.MustNoErr(t, err, "another process")
	if _, err := eng.CreateStationCounter(again, "load station", "Sta010.Prod_Tool_1_CNT.ACC", ""); err != nil {
		t.Errorf("the freed tag on another process's station counter: %v", err)
	}
}

// TestProcessCounter_PLCEditRefusedWhileStationCountersExist: a station
// counter reads its process's counter PLC (it is set from the process and not
// edited after); moving the production counter to another PLC would leave it
// behind, so the edit is refused while station counters exist.
func TestProcessCounter_PLCEditRefusedWhileStationCountersExist(t *testing.T) {
	t.Parallel()
	eng, pid, _, _ := stationRig(t)
	_, err := eng.CreateStationCounter(pid, "load station", "Sta010.Prod_Tool_1_CNT.ACC", "")
	testutil.MustNoErr(t, err, "station counter")
	err = eng.EditProcessCounter(pid, domain.ProcessCounterEdit{PLCName: "plc-2", TagName: "MES_Cell.Prod_Counter_01", Enabled: true})
	if err == nil || !strings.Contains(err.Error(), "station counter") {
		t.Fatalf("PLC edit with a station counter = %v, want a refusal naming the station counters", err)
	}
	if p := mustProcess(t, eng.db, pid); p.CounterPLCName != "plc" {
		t.Errorf("a refused edit wrote the PLC: %q", p.CounterPLCName)
	}
	testutil.MustNoErr(t, eng.EditProcessCounter(pid, domain.ProcessCounterEdit{PLCName: "plc", TagName: "MES_Other.Prod_Counter_01", Enabled: true}),
		"a tag edit on the same PLC still saves")
}
