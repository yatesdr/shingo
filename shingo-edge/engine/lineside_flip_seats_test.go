// lineside_flip_seats_test.go — a flip changes no seat: a cart's part was
// recorded at its bind, known or assumed (decision 57), and every door that
// flips the style leaves every seat's record as it found it.
package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// flipSeat is one seat of the flip rig: the claims there and what stands on it.
type flipSeat struct {
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
	// landed: the cart landed naming no part just before the flip, after the
	// changeover the flip completes opened, and its bind recorded what the
	// position's claim names, assumed.
	landed bool
}

// assumedSource is the bind's record of a part no source named.
var assumedSource = domain.CarrierSource("assumed")

// flipSeats are the rows: one seat each, all on one process.
func flipSeats() []flipSeat {
	consume, simple := protocol.ClaimRoleConsume, protocol.SwapModeSimple
	return []flipSeat{
		{name: "FS-ASSUMED", fromRole: consume, fromMode: simple, toPart: "PART-R", bin: 9801,
			part: "PART-L", source: string(assumedSource)},
		{name: "FS-LANDED", fromRole: consume, fromMode: simple, toPart: "PART-R", bin: 9802,
			part: "PART-L", known: true, source: "landing"},
		{name: "FS-KNOWN-EMPTY", fromRole: consume, fromMode: simple, toPart: "PART-R", bin: 9803,
			known: true, source: "operator"},
		{name: "FS-UNSAID", fromRole: consume, fromMode: simple, toPart: "PART-R", bin: 9804},
		{name: "FS-NO-CART", fromRole: consume, fromMode: simple, toPart: "PART-R"},
		{name: "FS-PRODUCE", fromRole: protocol.ClaimRoleProduce, fromMode: simple, bin: 9805},
		{name: "FS-LOADER", fromRole: consume, fromMode: protocol.SwapModeManualSwap, bin: 9806},
		{name: "FS-TO-ONLY", toPart: "PART-R", bin: 9807},
		{name: "FS-LANDED-UNNAMED", fromRole: consume, fromMode: simple, toPart: "PART-R", bin: 9808, landed: true},
	}
}

// landUnnamed records each landed seat's cart as its bind records a cart that
// names no part: the position's claim's part, assumed.
func landUnnamed(t *testing.T, eng *Engine, nodes map[string]int64) {
	t.Helper()
	for _, s := range flipSeats() {
		if s.landed {
			testutil.MustNoErr(t, eng.db.SetProcessNodeRuntimeLinesidePayload(nodes[s.name], "PART-L", false,
				string(assumedSource)), "the bind's record of "+s.name)
		}
	}
}

// openedBefore dates the changeover's start two seconds back, so the landings
// after it are later to the second the rows keep.
func openedBefore(t *testing.T, eng *Engine, coID int64) {
	t.Helper()
	_, err := eng.db.DB.Exec(`UPDATE process_changeovers SET started_at = datetime(started_at, '-2 seconds') WHERE id = ?`, coID)
	testutil.MustNoErr(t, err, "the changeover started two seconds ago")
}

// flipRig is a process on FROM (pinned 1001) with TO (pinned 1002), and the
// seats.
func flipRig(t *testing.T) (eng *Engine, pid, from, to int64, nodes map[string]int64) {
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
	for _, s := range flipSeats() {
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
		if s.source != "" {
			testutil.MustNoErr(t, db.SetProcessNodeRuntimeLinesidePayload(id, s.part, s.known, s.source), "record "+s.name)
		}
	}
	return eng, pid, from, to, nodes
}

// seatRecord is what a seat says of its cart.
type seatRecord struct {
	part   string
	known  bool
	source string
}

func seatRecordOf(t *testing.T, eng *Engine, nodeID int64) seatRecord {
	t.Helper()
	rt := runtimeOf(t, eng, nodeID)
	return seatRecord{part: string(rt.LinesidePayloadCode), known: rt.LinesidePayloadKnown, source: rt.LinesideSource}
}

// TestFlip_ChangesNoSeat: at every door that flips the style, and on a re-set
// of the running style, every seat reads after the flip exactly what it read
// before: a cart assumed the departing part at its bind keeps it, a cart that
// landed naming no part during the changeover keeps what its bind assumed, a
// known cart, an empty seat, a produce seat, a loader, a node only the
// incoming style claims and a seat that records nothing are left as they were.
// Before (decisions 38 rule 1 and 40, superseded by 57), the flip stamped
// every unknown consume seat of the departing style with its part, source
// flip, except the seats bound since the changeover started.
func TestFlip_ChangesNoSeat(t *testing.T) {
	t.Parallel()
	doors := []struct {
		name string
		// flips: the style changes.
		flips bool
		flip  func(t *testing.T, eng *Engine, pid, from, to int64, nodes map[string]int64)
	}{
		{"completeCutover gated", true, func(t *testing.T, eng *Engine, pid, from, to int64, nodes map[string]int64) {
			coID, err := eng.changeoverService.Create(pid, &from, to, "test", "", nil, nil, nil, nil)
			testutil.MustNoErr(t, err, "a changeover with nothing to do: the gate is green")
			openedBefore(t, eng, coID)
			landUnnamed(t, eng, nodes)
			testutil.MustNoErr(t, eng.CompleteProcessProductionCutover(pid), "CUTOVER")
		}},
		{"completeCutover forced", true, func(t *testing.T, eng *Engine, pid, from, to int64, nodes map[string]int64) {
			ns, err := eng.db.ListProcessNodesByProcess(pid)
			testutil.MustNoErr(t, err, "nodes")
			coID, err := eng.changeoverService.Create(pid, &from, to, "test", "", nil, []processes.NodeTaskInput{{ProcessID: pid,
				CoreNodeName: "FS-ASSUMED", Situation: "swap", State: "swap_required"}}, nil, ns)
			testutil.MustNoErr(t, err, "a changeover with a swap still to do: the gate is red")
			if ok, _, err := eng.canCompleteChangeover(coID); err != nil || ok {
				t.Fatalf("gate = %v (%v), want red", ok, err)
			}
			openedBefore(t, eng, coID)
			landUnnamed(t, eng, nodes)
			blockers, err := eng.completeCutover(pid, coID, triggeredAutoCATID, CutoverForced)
			testutil.MustNoErr(t, err, "the forced door")
			if len(blockers) == 0 {
				t.Fatal("the forced door overrode no blocker: the gate was not red")
			}
		}},
		{"SetProcessActiveStyle completing a changeover", true, func(t *testing.T, eng *Engine, pid, from, to int64, nodes map[string]int64) {
			coID, err := eng.changeoverService.Create(pid, &from, to, "test", "", nil, nil, nil, nil)
			testutil.MustNoErr(t, err, "a changeover open")
			openedBefore(t, eng, coID)
			landUnnamed(t, eng, nodes)
			testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &to), "Mark as running")
		}},
		{"SetProcessActiveStyle", true, func(t *testing.T, eng *Engine, pid, _, to int64, nodes map[string]int64) {
			landUnnamed(t, eng, nodes)
			testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &to), "Mark as running")
		}},
		{"the same style re-set", false, func(t *testing.T, eng *Engine, pid, from, _ int64, nodes map[string]int64) {
			landUnnamed(t, eng, nodes)
			testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &from), "re-set the running style")
		}},
	}
	for _, d := range doors {
		t.Run(d.name, func(t *testing.T) {
			t.Parallel()
			eng, pid, from, to, nodes := flipRig(t)
			d.flip(t, eng, pid, from, to, nodes)
			wantStyle := to
			if !d.flips {
				wantStyle = from
			}
			if p := mustProcess(t, eng.db, pid); p.ActiveStyleID == nil || *p.ActiveStyleID != wantStyle {
				t.Fatalf("active style = %v, want %d", p.ActiveStyleID, wantStyle)
			}
			for _, s := range flipSeats() {
				want := seatRecord{part: s.part, known: s.known, source: s.source}
				if s.landed {
					want = seatRecord{part: "PART-L", source: string(assumedSource)}
				}
				if got := seatRecordOf(t, eng, nodes[s.name]); got != want {
					t.Errorf("%s after %s reads %+v, want %+v as before the flip: the flip changes no seat",
						s.name, d.name, got, want)
				}
			}
		})
	}
}
