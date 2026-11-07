// lineside_flip_stamp_test.go — a flip records the carrier it leaves: every
// consume seat of the departing style's whose bound carrier could not say its
// part is recorded as that style's part there, at each door that flips.
package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// flipStampSeat is one seat of the stamp rig: what stands there before the
// flip, and what the seat says after it.
type flipStampSeat struct {
	name string
	// fromRole and fromMode are the departing style's claim ("" role: none);
	// toPart the incoming style's (a consume claim; "" for none).
	fromRole protocol.ClaimRole
	fromMode protocol.SwapMode
	toPart   string
	// bin is the bound carrier (0: none); part/known/source its record.
	bin    int64
	part   string
	known  bool
	source string
	// want is what the seat says after the flip.
	wantPart  string
	wantKnown bool
	wantSrc   string
}

// flipStampSeats are the rows: one seat each, all on one process.
func flipStampSeats() []flipStampSeat {
	consume, simple := protocol.ClaimRoleConsume, protocol.SwapModeSimple
	return []flipStampSeat{
		{name: "FS-UNSAID", fromRole: consume, fromMode: simple, toPart: "PART-R", bin: 9801,
			wantPart: "PART-L", wantKnown: true, wantSrc: string(domain.CarrierFromFlip)},
		{name: "FS-LANDED", fromRole: consume, fromMode: simple, toPart: "PART-R", bin: 9802,
			part: "PART-L", known: true, source: "landing", wantPart: "PART-L", wantKnown: true, wantSrc: "landing"},
		{name: "FS-KNOWN-EMPTY", fromRole: consume, fromMode: simple, toPart: "PART-R", bin: 9803,
			known: true, source: "operator", wantKnown: true, wantSrc: "operator"},
		{name: "FS-NO-CART", fromRole: consume, fromMode: simple, toPart: "PART-R"},
		{name: "FS-PRODUCE", fromRole: protocol.ClaimRoleProduce, fromMode: simple, bin: 9805},
		{name: "FS-LOADER", fromRole: consume, fromMode: protocol.SwapModeManualSwap, bin: 9806},
		{name: "FS-TO-ONLY", toPart: "PART-R", bin: 9807},
	}
}

// flipStampRig is a process on FROM (pinned 1001) with TO (pinned 1002), and
// the seats.
func flipStampRig(t *testing.T) (eng *Engine, pid, from, to int64, nodes map[string]int64) {
	t.Helper()
	db := testEngineDB(t)
	eng = testEngine(t, db)
	eng.wireEventHandlers()
	pid, err := db.CreateProcess("FS-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "process")
	from, err = db.CreateStyle("FS-FROM", "", pid)
	testutil.MustNoErr(t, err, "from style")
	to, err = db.CreateStyle("FS-TO", "", pid)
	testutil.MustNoErr(t, err, "to style")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(from, "1001"), "pin from")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(to, "1002"), "pin to")
	testutil.MustNoErr(t, db.SetActiveStyle(pid, &from), "active")
	nodes = map[string]int64{}
	for _, s := range flipStampSeats() {
		id, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: s.name, Code: s.name, Name: s.name, Enabled: true})
		testutil.MustNoErr(t, err, "node "+s.name)
		nodes[s.name] = id
		var claim *int64
		if s.fromRole != "" {
			part := "PART-L"
			if s.fromRole == protocol.ClaimRoleProduce {
				part = "PART-P"
			}
			c, err := upsertClaimRetiredMode(db, processes.NodeClaimInput{StyleID: from, CoreNodeName: s.name, Role: s.fromRole,
				SwapMode: s.fromMode, PayloadCode: part, UOPCapacity: 40, InboundSource: "SRC"})
			testutil.MustNoErr(t, err, "from claim "+s.name)
			claim = &c
		}
		if s.toPart != "" {
			c, err := upsertClaimRetiredMode(db, processes.NodeClaimInput{StyleID: to, CoreNodeName: s.name, Role: protocol.ClaimRoleConsume,
				SwapMode: protocol.SwapModeSimple, PayloadCode: s.toPart, UOPCapacity: 40, InboundSource: "SRC"})
			testutil.MustNoErr(t, err, "to claim "+s.name)
			if claim == nil {
				claim = &c
			}
		}
		_, err = db.EnsureProcessNodeRuntime(id)
		testutil.MustNoErr(t, err, "runtime "+s.name)
		var bin *int64
		if s.bin != 0 {
			bin = binPtr(s.bin)
		}
		testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(id, claim, bin, 30), "seat "+s.name)
		if s.known {
			testutil.MustNoErr(t, db.SetProcessNodeRuntimeLinesidePayload(id, s.part, true, s.source), "record "+s.name)
		}
	}
	return eng, pid, from, to, nodes
}

// TestFlip_RecordsTheCarrierItLeaves: at every door that flips the style, a
// consume seat of the departing style's whose carrier could not say its part
// reads that style's part afterwards, source flip; a seat already known, an
// empty seat, a produce seat, a loader and a node the departing style does not
// claim are left as they were. Re-setting the running style stamps nothing.
// Before, the flip left the seat unknown, and every reader that falls back to
// the active style's part named the incoming part for a cart bound under the
// departing style.
func TestFlip_RecordsTheCarrierItLeaves(t *testing.T) {
	t.Parallel()
	doors := []struct {
		name  string
		flips bool
		flip  func(t *testing.T, eng *Engine, pid, from, to int64, nodes map[string]int64)
	}{
		{"completeCutover gated", true, func(t *testing.T, eng *Engine, pid, from, to int64, _ map[string]int64) {
			_, err := eng.changeoverService.Create(pid, &from, to, "test", "", nil, nil, nil, nil)
			testutil.MustNoErr(t, err, "a changeover with nothing to do: the gate is green")
			testutil.MustNoErr(t, eng.CompleteProcessProductionCutover(pid), "CUTOVER")
		}},
		{"completeCutover forced", true, func(t *testing.T, eng *Engine, pid, from, to int64, nodes map[string]int64) {
			ns, err := eng.db.ListProcessNodesByProcess(pid)
			testutil.MustNoErr(t, err, "nodes")
			coID, err := eng.changeoverService.Create(pid, &from, to, "test", "", nil, []processes.NodeTaskInput{{ProcessID: pid,
				CoreNodeName: "FS-UNSAID", Situation: "swap", State: "swap_required"}}, nil, ns)
			testutil.MustNoErr(t, err, "a changeover with a swap still to do: the gate is red")
			if ok, _, err := eng.canCompleteChangeover(coID); err != nil || ok {
				t.Fatalf("gate = %v (%v), want red", ok, err)
			}
			blockers, err := eng.completeCutover(pid, coID, triggeredAutoCATID, CutoverForced)
			testutil.MustNoErr(t, err, "the forced door")
			if len(blockers) == 0 {
				t.Fatal("the forced door overrode no blocker: the gate was not red")
			}
		}},
		{"SetProcessActiveStyle", true, func(t *testing.T, eng *Engine, pid, _, to int64, _ map[string]int64) {
			testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &to), "Mark as running")
		}},
		{"autoSwitchStyle", true, func(t *testing.T, eng *Engine, pid, from, to int64, _ map[string]int64) {
			testutil.MustNoErr(t, eng.db.SetChangeoverAutoArm(pid, "auto"), "auto")
			testutil.MustNoErr(t, eng.autoSwitchStyle(pid, from, to, "1002"), "the automatic switch")
		}},
		{"the same style re-set", false, func(t *testing.T, eng *Engine, pid, from, _ int64, _ map[string]int64) {
			testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &from), "re-set the running style")
		}},
	}
	for _, d := range doors {
		t.Run(d.name, func(t *testing.T) {
			t.Parallel()
			eng, pid, from, to, nodes := flipStampRig(t)
			d.flip(t, eng, pid, from, to, nodes)
			wantStyle := to
			if !d.flips {
				wantStyle = from
			}
			if p := mustProcess(t, eng.db, pid); p.ActiveStyleID == nil || *p.ActiveStyleID != wantStyle {
				t.Fatalf("active style = %v, want %d", p.ActiveStyleID, wantStyle)
			}
			for _, s := range flipStampSeats() {
				t.Run(s.name, func(t *testing.T) {
					want := s
					if !d.flips {
						want.wantPart, want.wantKnown, want.wantSrc = s.part, s.known, s.source
					}
					rt := runtimeOf(t, eng, nodes[s.name])
					if string(rt.LinesidePayloadCode) != want.wantPart || rt.LinesidePayloadKnown != want.wantKnown ||
						rt.LinesideSource != want.wantSrc {
						t.Errorf("%s after %s: part %q known %t source %q; want %q known %t source %q", s.name, d.name,
							rt.LinesidePayloadCode, rt.LinesidePayloadKnown, rt.LinesideSource, want.wantPart, want.wantKnown,
							want.wantSrc)
					}
				})
			}
		})
	}
}
