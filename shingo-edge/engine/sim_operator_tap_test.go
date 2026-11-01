//go:build sim

package engine

import (
	"context"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingoedge/config"
	"shingoedge/domain"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// TestChooseUnloaderTap is the operator's whole decision.
func TestChooseUnloaderTap(t *testing.T) {
	t.Parallel()
	three := 3
	big := &domain.TileAssignment{BinType: "BIG-45"}
	high := &domain.TileAssignment{BinType: "TOTES-45", Layers: &three}
	for _, tc := range []struct {
		name    string
		mode    string
		stocked bool
		lit     *domain.TileAssignment
		want    unloaderTap
	}{
		{"lit: a full takes the lit CLEAR", config.SimTapLit, true, big, unloaderTap{binType: "BIG-45"}},
		{"lit: an empty takes the lit PUSH AS", config.SimTapLit, false, big, unloaderTap{binType: "BIG-45"}},
		{"lit: a height travels with the type", config.SimTapLit, false, high, unloaderTap{binType: "TOTES-45", layers: &three}},
		{"lit: a full with nothing lit is a plain CLEAR", config.SimTapLit, true, nil, unloaderTap{}},
		{"lit: an empty with nothing lit is PUSH EMPTY", config.SimTapLit, false, nil, unloaderTap{pushEmpty: true}},
		{"empty: an empty is PUSH EMPTY whatever is lit", config.SimTapEmpty, false, big, unloaderTap{pushEmpty: true}},
		{"empty: a full is a plain CLEAR whatever is lit", config.SimTapEmpty, true, big, unloaderTap{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := chooseUnloaderTap(tc.mode, tc.stocked, tc.lit)
			if got.pushEmpty != tc.want.pushEmpty || got.binType != tc.want.binType || got.layers != tc.want.layers {
				t.Errorf("chooseUnloaderTap = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// tapRig is one Core-owned unloader window on its own operator station, with a
// stub Core capturing the CLEARs.
type tapRig struct {
	eng    *Engine
	op     *simOperator
	core   *clearCaptureCore
	window string
	node   int64
}

func newTapRig(t *testing.T, station, mode string) *tapRig {
	t.Helper()
	eng := testEngine(t, testEngineDB(t))
	core := newClearCaptureCore(t)
	bridgeCore(t, eng, core.srv.URL)
	const window = "ULN_T1"
	info := sharedLoaderInfo(window, "consume", "operator", "PART-HL", 0, 0)
	info.InboundSource = ""
	info.OutboundDest = "K-GROUP"
	nodeID := coreUnloaderWindow(t, eng, window, info)

	n, err := eng.db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "read node")
	stationID, err := eng.db.CreateOperatorStation(stations.Input{ProcessID: n.ProcessID, Code: station, Name: station, Enabled: true})
	testutil.MustNoErr(t, err, "create station")
	testutil.MustNoErr(t, eng.db.UpdateProcessNode(nodeID, processes.NodeInput{
		ProcessID: n.ProcessID, OperatorStationID: &stationID, CoreNodeName: window, Code: n.Code, Name: n.Name, Sequence: n.Sequence, Enabled: true,
	}), "put the node on the station")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	op := &simOperator{
		e:   eng,
		ops: config.SimOperatorsConfig{UnloaderTaps: []config.SimUnloaderTap{{Station: "KP-UNLOAD-OPS", Tap: mode}}, UnloaderAutoClear: time.Millisecond},
		clk: clock.Real(), ctx: ctx, pending: map[int64]bool{}, releasing: map[int64]bool{}, confirming: map[int64]bool{},
	}
	return &tapRig{eng: eng, op: op, core: core, window: window, node: nodeID}
}

// hold is the Edge's copy of the bins feed: one cart at the window and what the
// group's line asks of it.
func (r *tapRig) hold(t *testing.T, stocked bool, assigned ...protocol.NodeBinsAssignment) {
	t.Helper()
	payload := ""
	if stocked {
		payload = "PART-HL"
	}
	_, own, ok := r.eng.binWatchList()
	if !ok {
		t.Fatal("the watch list has no digest")
	}
	if assigned == nil {
		assigned = []protocol.NodeBinsAssignment{}
	}
	snap := protocol.NodeBinsSnapshot{ListDigest: own, Rows: []protocol.NodeBinsRow{{
		Node: r.window, State: protocol.BinsStateOccupied, Occupied: true, BinID: 91, PayloadCode: payload,
		BinTypeCode: "SMALL-45", Stocked: stocked,
	}}, Groups: []protocol.NodeBinsGroup{{Group: "K-GROUP", Wants: []protocol.NodeBinsWant{}, Assignments: assigned}}}
	var err error
	snap.Digest, err = protocol.NodeBinsDigest(snap)
	testutil.MustNoErr(t, err, "digest")
	testutil.MustNoErr(t, r.eng.ApplyNodeBinsSnapshot(snap), "apply")
	ackHeld(r.eng)
}

func (r *tapRig) moveOrders(t *testing.T) int {
	t.Helper()
	moves, err := r.eng.db.ListActiveOrdersByBinAndType(91, protocol.OrderTypeMove)
	testutil.MustNoErr(t, err, "list moves")
	return len(moves)
}

// TestSimOperator_TapsWhatThePanelLights: the tap at a window goes through the
// doors the panel posts to, with the assignment the board's card lights: the
// CLEAR with the lit type, and with nothing lit PUSH EMPTY for an empty and a
// plain CLEAR for a full.
func TestSimOperator_TapsWhatThePanelLights(t *testing.T) {
	t.Parallel()
	lit := protocol.NodeBinsAssignment{Node: "ULN_T1", BinType: "BIG-45"}
	other := protocol.NodeBinsAssignment{Node: "ULN_OTHER", BinType: "TOTES-45"}
	for _, tc := range []struct {
		name      string
		mode      string
		stocked   bool
		assigned  []protocol.NodeBinsAssignment
		wantClear bool
		wantType  string
	}{
		{"full, lit", config.SimTapLit, true, []protocol.NodeBinsAssignment{other, lit}, true, "BIG-45"},
		{"empty, lit: PUSH AS", config.SimTapLit, false, []protocol.NodeBinsAssignment{lit}, true, "BIG-45"},
		{"full, nothing lit here", config.SimTapLit, true, []protocol.NodeBinsAssignment{other}, true, ""},
		{"empty, nothing lit: PUSH EMPTY", config.SimTapLit, false, nil, false, ""},
		{"empty, set to PUSH EMPTY, lit", config.SimTapEmpty, false, []protocol.NodeBinsAssignment{lit}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newTapRig(t, "KP-UNLOAD-OPS", tc.mode)
			r.hold(t, tc.stocked, tc.assigned...)

			testutil.MustNoErr(t, r.op.tapUnloader(r.node, r.window, tc.mode), "tap")

			clears, _ := r.core.snapshot()
			if !tc.wantClear {
				if len(clears) != 0 {
					t.Errorf("bin-clear POSTs = %v, want none: PUSH EMPTY posts no clear", clears)
				}
			} else {
				if len(clears) != 1 {
					t.Fatalf("bin-clear POSTs = %d, want 1", len(clears))
				}
				if got := clears[0]["bin_type_code"]; got != tc.wantType {
					t.Errorf("CLEAR's bin_type_code = %q, want %q", got, tc.wantType)
				}
			}
			if got := r.moveOrders(t); got != 1 {
				t.Errorf("moves carrying the cart = %d, want the one empty-out", got)
			}
		})
	}
}

// TestSimOperator_LeavesACartWhoseEmptyOutIsFiled: an empty still standing at the
// window with its move filed is on its way, in the sweep and in the tap. Tapped
// again, PUSH EMPTY would refuse it and the operator would retry eight times, on
// every cart, between its CLEAR and its pickup.
func TestSimOperator_LeavesACartWhoseEmptyOutIsFiled(t *testing.T) {
	t.Parallel()
	r := newTapRig(t, "KP-UNLOAD-OPS", config.SimTapLit)
	r.hold(t, false)
	row := r.eng.LocalNodeBins().Rows[r.window]
	if !r.op.operatorTapsAt(row) {
		t.Fatal("an empty standing at a tap window is work before its move is filed")
	}
	testutil.MustNoErr(t, r.op.tapUnloader(r.node, r.window, config.SimTapLit), "first tap")
	if r.op.operatorTapsAt(row) {
		t.Error("the sweep would schedule a cart whose empty-out is already filed")
	}
	testutil.MustNoErr(t, r.op.tapUnloader(r.node, r.window, config.SimTapLit), "second tap")
	if got := r.moveOrders(t); got != 1 {
		t.Errorf("moves carrying the cart = %d, want the one", got)
	}
	if clears, _ := r.core.snapshot(); len(clears) != 0 {
		t.Errorf("bin-clear POSTs = %v, want none", clears)
	}
}

// TestSimOperator_TapSettingIsTheStations: only a station the config names is
// worked by its panel. Any other keeps the plain CLEAR, which is every station of
// every other plant on the same dev config, and an empty there stays no work.
func TestSimOperator_TapSettingIsTheStations(t *testing.T) {
	t.Parallel()
	named := newTapRig(t, "KP-UNLOAD-OPS", config.SimTapLit)
	if _, label, _, ok := named.op.classifyFromClaim(named.node); !ok || label != "tap" {
		t.Errorf("named station: classify = %q, %v; want the tap", label, ok)
	}
	unnamed := newTapRig(t, "SOME-OTHER-OPS", config.SimTapLit)
	if _, label, _, ok := unnamed.op.classifyFromClaim(unnamed.node); !ok || label != "clear" {
		t.Errorf("unnamed station: classify = %q, %v; want the plain clear", label, ok)
	}
	mistyped := newTapRig(t, "KP-UNLOAD-OPS", "lit-up")
	n, err := mistyped.eng.db.GetProcessNode(mistyped.node)
	testutil.MustNoErr(t, err, "read node")
	if got := mistyped.op.unloaderTapAt(n); got != "" {
		t.Errorf("an unknown setting reads %q, want the plain CLEAR", got)
	}
}
