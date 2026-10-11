//go:build sim

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"shingoedge/store"
	"shingoedge/store/processes"
)

// store frames as the counting driver sees them: what sits between a caller
// and SQLite on every statement.
var storeFrames = []callerFrame{
	{fn: "main.(*stmtObserver).observe", file: "cmd/shingoedge/sim_statements.go"},
	{fn: "shingoedge/store.(*QueryCounter).record", file: "store/query_count.go"},
	{fn: "database/sql.(*DB).QueryContext", file: "database/sql/sql.go"},
	{fn: "shingoedge/store/processes.ListClaims", file: "store/processes/claims.go"},
}

func stack(frames ...callerFrame) []callerFrame {
	return append(append([]callerFrame{}, storeFrames...), frames...)
}

// Every row of the table, on a frame it should take, gives its own tag: the
// table is the classifier, so each row is pinned.
func TestSimCallerTag_EveryRowTakesItsFrame(t *testing.T) {
	for i, p := range callerTable {
		f := callerFrame{fn: p.fn, file: "x/y.go"}
		if p.file != "" {
			f = callerFrame{fn: "shingoedge/engine.(*Engine).someEntry", file: p.file + "x.go"}
		} else if strings.HasSuffix(p.fn, ".") {
			f.fn = p.fn + "Method"
		}
		want := p.tag
		if want == "" {
			want = tagOther // a pass-through alone decides nothing
		}
		got, _ := classifyFrames(stack(f))
		if got != want {
			t.Errorf("row %d %+v: frame %+v tagged %q, want %q", i, p, f, got, want)
		}
	}
}

func TestSimCallerTag_Table(t *testing.T) {
	cases := []struct {
		name     string
		frames   []callerFrame
		tag      string
		pathWant string
	}{
		{
			name: "counter delta, the reader named after the entry",
			frames: []callerFrame{
				{fn: "shingoedge/engine.(*Engine).counterDeltaRows", file: "engine/wiring_counter_delta.go"},
				{fn: "shingoedge/engine.(*Engine).handleCounterDelta", file: "engine/wiring_counter_delta.go"},
				{fn: "shingoedge/engine.(*Engine).wireEventHandlers.func1", file: "engine/wiring.go"},
				{fn: "shingo/protocol/eventbus.(*Bus[...]).Emit", file: "protocol/eventbus/eventbus.go"},
				{fn: "shingoedge/plc.(*Manager).pollReportingPoint", file: "plc/manager.go"},
				{fn: "shingoedge/plc.(*Manager).pollAllReportingPoints", file: "plc/manager.go"},
			},
			tag:      tagTick,
			pathWant: "engine/wiring_counter_delta.go:(*Engine).counterDeltaRows",
		},
		{
			name: "the poll pass's own reporting-point list is poll, not tick",
			frames: []callerFrame{
				{fn: "shingoedge/plc.(*Manager).pollAllReportingPoints", file: "plc/manager.go"},
				{fn: "shingoedge/plc.(*Manager).pollLoop", file: "plc/manager.go"},
			},
			tag:      tagPoll,
			pathWant: "plc/manager.go:(*Manager).pollAllReportingPoints",
		},
		{
			name: "pollReportingPointSafe is not pollReportingPoint (whole-name match)",
			frames: []callerFrame{
				{fn: "shingoedge/plc.(*Manager).pollReportingPointSafe", file: "plc/manager.go"},
			},
			tag: tagPoll,
		},
		{
			name: "the snapshot write inside the poll is tick",
			frames: []callerFrame{
				{fn: "shingoedge/plc.(*Manager).pollReportingPoint", file: "plc/manager.go"},
				{fn: "shingoedge/plc.(*Manager).pollReportingPointSafe", file: "plc/manager.go"},
				{fn: "shingoedge/plc.(*Manager).pollAllReportingPoints", file: "plc/manager.go"},
			},
			tag:      tagTick,
			pathWant: "plc/manager.go:(*Manager).pollReportingPoint",
		},
		{
			name: "an operator action the sim operator presses is order; innermost entry wins",
			frames: []callerFrame{
				{fn: "shingoedge/engine.(*Engine).RequestFull", file: "engine/operator_demand.go"},
				{fn: "shingoedge/engine.(*simOperator).run", file: "engine/sim_operator.go"},
			},
			tag:      tagOrder,
			pathWant: "engine/operator_demand.go:(*Engine).RequestFull",
		},
		{
			name: "the sim operator's sweep through the shared loadActiveNode helper is sim, not order",
			frames: []callerFrame{
				{fn: "shingoedge/engine.requestedClaimAtNode", file: "engine/operator_helpers.go"},
				{fn: "shingoedge/engine.(*Engine).claimAtNode", file: "engine/operator_helpers.go"},
				{fn: "shingoedge/engine.(*Engine).loadActiveNode", file: "engine/operator_helpers.go"},
				{fn: "shingoedge/engine.(*simOperator).sweepManualSwapNodes", file: "engine/sim_operator.go"},
				{fn: "shingoedge/engine.(*simOperator).reconcile", file: "engine/sim_operator.go"},
			},
			tag:      tagSim,
			pathWant: "engine/sim_operator.go:(*simOperator).sweepManualSwapNodes > engine/operator_helpers.go:requestedClaimAtNode",
		},
		{
			name: "the same helper under an operator action is order",
			frames: []callerFrame{
				{fn: "shingoedge/engine.(*Engine).loadActiveNode", file: "engine/operator_helpers.go"},
				{fn: "shingoedge/engine.(*Engine).LoadBin", file: "engine/operator_bin_ops.go"},
				{fn: "shingoedge/engine.(*simOperator).loadBin", file: "engine/sim_operator.go"},
			},
			tag: tagOrder,
		},
		{
			name: "the fake PLC's machine-ready gate, a closure in the sim wiring, is sim",
			frames: []callerFrame{
				{fn: "main.simWarlinkClient.func1", file: "cmd/shingoedge/sim_enabled.go"},
				{fn: "shingoedge/plc/simwarlink.(*FakeClient).tick", file: "plc/simwarlink/fake_client.go"},
			},
			tag: tagSim,
		},
		{
			name: "the sim operator's own read is sim",
			frames: []callerFrame{
				{fn: "shingoedge/engine.(*simOperator).capacityOf", file: "engine/sim_operator.go"},
				{fn: "shingoedge/engine.(*simOperator).run", file: "engine/sim_operator.go"},
			},
			tag: tagSim,
		},
		{
			name: "the heartbeat's tick-lag read is loop though TickShipper is tick",
			frames: []callerFrame{
				{fn: "shingoedge/messaging.(*TickShipper).Lag", file: "messaging/tick_shipper.go"},
				{fn: "shingoedge/engine.(*Engine).ProductionTickLag", file: "engine/engine.go"},
				{fn: "main.setupKafkaSubscribers.func4", file: "cmd/shingoedge/main.go"},
				{fn: "shingoedge/messaging.(*Heartbeater).sendHeartbeat", file: "messaging/heartbeat.go"},
			},
			tag: tagLoop,
		},
		{
			name: "the tick shipper's send is tick",
			frames: []callerFrame{
				{fn: "shingoedge/messaging.(*TickShipper).ShipPending", file: "messaging/tick_shipper.go"},
				{fn: "shingoedge/messaging.(*TickShipper).loop", file: "messaging/tick_shipper.go"},
			},
			tag: tagTick,
		},
		{
			name: "a Core order message",
			frames: []callerFrame{
				{fn: "shingoedge/orders.(*Manager).HandleOrderUpdate", file: "orders/manager.go"},
				{fn: "shingoedge/messaging.(*EdgeHandler).HandleOrderUpdate", file: "messaging/edge_handler.go"},
				{fn: "shingoedge/messaging.(*Client).readLoop", file: "messaging/client.go"},
			},
			tag: tagOrder,
		},
		{
			name: "a closure inside the lineside reporter is the reporter's",
			frames: []callerFrame{
				{fn: "shingoedge/engine.(*Engine).reportLinesideLevels.func1", file: "engine/lineside_reporter.go"},
				{fn: "shingoedge/uop.(*accumulator).WithPending", file: "uop/accumulator.go"},
				{fn: "shingoedge/engine.(*Engine).reportLinesideLevels", file: "engine/lineside_reporter.go"},
			},
			tag: tagLoop,
		},
		{
			name: "a sim route is sim though www is order",
			frames: []callerFrame{
				{fn: "shingoedge/www.(*Handlers).apiSimSeed", file: "www/sim_routes.go"},
			},
			tag: tagSim,
		},
		{
			name: "the fake PLC is sim, not poll",
			frames: []callerFrame{
				{fn: "shingoedge/plc/simwarlink.(*FakeClient).tick", file: "plc/simwarlink/fake_client.go"},
			},
			tag: tagSim,
		},
		{
			name: "the WAL checkpoint loop is matched on the store function",
			frames: []callerFrame{
				{fn: "shingoedge/store.(*DB).CheckpointWAL", file: "store/store.go"},
				{fn: "main.main.func9", file: "cmd/shingoedge/main.go"},
			},
			tag:      tagLoop,
			pathWant: "store/store.go:(*DB).CheckpointWAL > cmd/shingoedge/main.go:main.func9",
		},
		{
			name: "unmatched is other, with its root and its reader",
			frames: []callerFrame{
				{fn: "shingoedge/service.(*StationService).Build", file: "service/station_service.go"},
				{fn: "main.main", file: "cmd/shingoedge/main.go"},
				{fn: "runtime.main", file: "runtime/proc.go"},
				{fn: "runtime.goexit", file: "runtime/asm_amd64.s"},
			},
			tag:      tagOther,
			pathWant: "cmd/shingoedge/main.go:main > service/station_service.go:(*StationService).Build",
		},
	}
	for _, c := range cases {
		tag, path := classifyFrames(stack(c.frames...))
		if tag != c.tag {
			t.Errorf("%s: tag %q (path %s), want %q", c.name, tag, path, c.tag)
		}
		if c.pathWant != "" && path != c.pathWant {
			t.Errorf("%s: path %q, want %q", c.name, path, c.pathWant)
		}
	}
}

// On a real stack: a statement sent from this test through the counting store
// resolves to frames whose reader is the test function and, no row matching a
// test, tags other. Proves the observer sees the sender's goroutine, the store
// frames are skipped, and frames read module-relative.
func TestSimCallerTag_RealStackThroughTheStore(t *testing.T) {
	db, _, err := store.OpenCounting(filepath.Join(t.TempDir(), "edge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	obs := newStmtObserver()
	store.SetStatementObserver(obs.observe)
	t.Cleanup(func() { store.SetStatementObserver(nil) })

	if _, err := processes.ListClaims(db.DB, 1); err != nil {
		t.Fatal(err)
	}
	store.SetStatementObserver(nil)

	got := string(obs.render())
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 2 || lines[0]+"\n" != stmtHeader {
		t.Fatalf("render:\n%s\nwant the header and one row", got)
	}
	f := strings.Split(lines[1], "\t")
	if len(f) != 5 || f[0] != tagOther || f[1] != "SELECT" || f[2] != "style_node_claims" || f[3] != "1" {
		t.Fatalf("row %q, want other/SELECT/style_node_claims/1/<path>", lines[1])
	}
	if !strings.Contains(f[4], "cmd/shingoedge/sim_callers_test.go:TestSimCallerTag_RealStackThroughTheStore") {
		t.Errorf("path %q, want the test function as the reader", f[4])
	}
}
