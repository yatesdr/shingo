package service

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

func iptr(v int) *int { return &v }

// shortTile builds one station with one produce tile, its active style's claim
// strict or not with a pack tolerance of pct, the part's catalog pack 132 and
// the seat counted at count, and returns the tile as the station's JSON
// carries it.
func shortTile(t *testing.T, strict bool, pct, count int) map[string]any {
	t.Helper()
	db := testdb.Open(t)
	processID, err := db.CreateProcess("SYN-FG-PROC", "", "", "", false)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	stationID, err := db.CreateOperatorStation(stations.Input{ProcessID: processID, Code: "SYN-FG-ST", Name: "FG", Sequence: 1, Enabled: true})
	if err != nil {
		t.Fatalf("station: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{ProcessID: processID, OperatorStationID: &stationID,
		CoreNodeName: "SYN-FG-1", Code: "FG1", Name: "FG 1", Sequence: 1, Enabled: true})
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	styleID, err := db.CreateStyle("SYN-STYLE", "", processID)
	if err != nil {
		t.Fatalf("style: %v", err)
	}
	if err := db.SetActiveStyle(processID, &styleID); err != nil {
		t.Fatalf("active style: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO payload_catalog (id, name, code, uop_capacity) VALUES (1, 'SYN-PART-132', 'SYN-PART-132', 132)`); err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if _, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "SYN-FG-1", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobot,
		PayloadCode: "SYN-PART-132", InboundStaging: "SYN-STG", InboundSource: "SYN-SRC", OutboundDestination: "SYN-DOCK",
		StrictPack: strict, PackTolerancePct: pct,
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("runtime: %v", err)
	}
	if err := db.UpdateProcessNodeUOP(nodeID, count); err != nil {
		t.Fatalf("count: %v", err)
	}
	view, err := NewStationService(db).BuildView(context.Background(), stationID)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	for i := range view.Nodes {
		if view.Nodes[i].Node.ID != nodeID {
			continue
		}
		raw, err := json.Marshal(view.Nodes[i])
		if err != nil {
			t.Fatalf("marshal tile: %v", err)
		}
		out := map[string]any{}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("read tile: %v", err)
		}
		return out
	}
	t.Fatal("the tile is missing from its station")
	return nil
}

// TestBuildView_ReleaseShort pins what a produce tile tells the board about a
// short release: the RELEASE button's label reads the tile, and the stamp at
// the release reads the claim, through the one helper
// (NodeClaim.ReleaseShort). Pinned before the tolerance existed, when no tile
// said anything; now a strict tile short of its pack and outside its
// tolerance carries release_short, and every other tile carries nothing.
func TestBuildView_ReleaseShort(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		strict bool
		pct    int
		count  int
		want   any
	}{
		{"strict, no tolerance, 113 of 132", true, 0, 113, true},
		{"strict, 5% (T 6), 113 of 132", true, 5, 113, true},
		{"strict, 5% (T 6), 128 of 132", true, 5, 128, nil},
		{"strict, no tolerance, 128 of 132", true, 0, 128, true},
		{"strict, at the pack", true, 5, 132, nil},
		{"strict, empty", true, 5, 0, nil},
		{"not strict, 113 of 132", false, 5, 113, nil},
	} {
		tile := shortTile(t, c.strict, c.pct, c.count)
		if got := tile["release_short"]; got != c.want {
			t.Errorf("%s: release_short = %v, want %v", c.name, got, c.want)
		}
		// The claim rides the tile: its tolerance costs bytes only when set.
		claim, _ := tile["active_claim"].(map[string]any)
		got, has := claim["pack_tolerance_pct"]
		if c.pct == 0 && has {
			t.Errorf("%s: active_claim carries pack_tolerance_pct = %v with none set", c.name, got)
		}
		if c.pct != 0 && got != float64(c.pct) {
			t.Errorf("%s: active_claim pack_tolerance_pct = %v, want %d", c.name, got, c.pct)
		}
	}
}

// The board's buttons come from the rows the view holds (SHAPE 3.8). Each case
// is a node's live legs; the want is what the tile renders.
func TestReleasePurposes(t *testing.T) {
	t.Parallel()
	co := `{"purposes":["ready","tooling_done"]}`
	swap := `{"purposes":["swap"]}`
	leg := func(status protocol.Status, wait *int, kind, facts, intent string) orders.Order {
		return orders.Order{Status: status, StationWait: wait, WaitKind: kind, ReleaseFacts: facts, ReleaseIntent: intent}
	}
	st := protocol.WaitKindStation
	for _, c := range []struct {
		name string
		legs []orders.Order
		want []domain.ReleasePurpose
	}{
		{"a changeover evac parked at ready", []orders.Order{leg(protocol.StatusStaged, iptr(0), st, co, "")},
			[]domain.ReleasePurpose{{Purpose: "ready", Ready: true}}},
		{"parked at tooling done: the button says so", []orders.Order{leg(protocol.StatusStaged, iptr(1), st, co, "")},
			[]domain.ReleasePurpose{{Purpose: "tooling_done", Ready: true}}},
		{"released at ready and driving on to tooling done: present, not ready",
			[]orders.Order{leg(protocol.StatusInTransit, iptr(0), st, co, `{"station_wait":0,"sent_at":"x"}`)},
			[]domain.ReleasePurpose{{Purpose: "tooling_done"}}},
		{"at a lane wait: present, not ready", []orders.Order{leg(protocol.StatusStaged, nil, protocol.WaitKindLane, swap, "")},
			[]domain.ReleasePurpose{{Purpose: "swap"}}},
		{"not yet moving: present, not ready", []orders.Order{leg(protocol.StatusDispatched, nil, "", swap, "")},
			[]domain.ReleasePurpose{{Purpose: "swap"}}},
		{"two purposes at one node, in the order owed", []orders.Order{
			leg(protocol.StatusStaged, iptr(0), st, swap, ""), leg(protocol.StatusStaged, iptr(1), st, co, "")},
			[]domain.ReleasePurpose{{Purpose: "tooling_done", Ready: true}, {Purpose: "swap", Ready: true}}},
		{"past its last wait, terminal, or no stored facts: nothing", []orders.Order{
			leg(protocol.StatusInTransit, iptr(0), st, swap, `{"station_wait":0,"sent_at":"x"}`),
			leg(protocol.StatusConfirmed, iptr(0), st, swap, ""),
			leg(protocol.StatusStaged, iptr(0), st, "", "")}, nil},
	} {
		if got := releasePurposes(c.legs); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

// L9: the chip reads every live leg's sentence at the node, not only the
// runtime's active order's.
func TestReleaseHeld(t *testing.T) {
	t.Parallel()
	got := releaseHeld([]orders.Order{
		{Status: protocol.StatusStaged, ReleaseHeld: "waiting for the bin on SYN-PRESS to be lifted"},
		{Status: protocol.StatusStaged, ReleaseHeld: "Core rejected the release: invalid_state"},
		{Status: protocol.StatusStaged, ReleaseHeld: "waiting for the bin on SYN-PRESS to be lifted"},
		{Status: protocol.StatusCancelled, ReleaseHeld: "an ended leg says nothing"},
		{Status: protocol.StatusInTransit},
	})
	want := "waiting for the bin on SYN-PRESS to be lifted; Core rejected the release: invalid_state"
	if got != want {
		t.Errorf("chip = %q, want %q", got, want)
	}
}
