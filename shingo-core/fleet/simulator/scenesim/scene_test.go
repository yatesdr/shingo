package scenesim

import (
	"testing"

	"shingocore/domain"
	"shingocore/fleet"
)

// depthPtr / idPtr keep the fixture readable.
func idPtr(v int64) *int64 { return &v }
func depthPtr(v int) *int  { return &v }

// plantScene is a small but faithful slice of real plant geometry: a group with a
// depth-3 single-file lane, a lineside source, and an aisle the robot starts on.
// Built from the same domain.Node model laneForNode walks — not a toy.
func plantScene(t *testing.T) *Scene {
	t.Helper()
	nodes := []domain.Node{
		{ID: 1, Name: "GRP-A", NodeTypeCode: "NGRP"},
		{ID: 2, Name: "LANE-A1", NodeTypeCode: "LANE", ParentID: idPtr(1)},
		{ID: 3, Name: "A1-S0", ParentID: idPtr(2), Depth: depthPtr(0)},
		{ID: 4, Name: "A1-S1", ParentID: idPtr(2), Depth: depthPtr(1)},
		{ID: 5, Name: "A1-S2", ParentID: idPtr(2), Depth: depthPtr(2)},
		{ID: 6, Name: "LINE-IN", NodeTypeCode: "STOR"},
		{ID: 7, Name: "AISLE"},
	}
	sc, err := LoadScene(nodes)
	if err != nil {
		t.Fatalf("LoadScene: %v", err)
	}
	return sc
}

func TestLoadScene_LaneGeometry(t *testing.T) {
	sc := plantScene(t)

	lane := sc.Lane("LANE-A1")
	if lane == nil {
		t.Fatal("LANE-A1 not loaded as a lane")
	}
	want := []string{"A1-S0", "A1-S1", "A1-S2"} // shallow → deep
	if len(lane.Slots) != 3 || lane.Slots[0] != want[0] || lane.Slots[2] != want[2] {
		t.Fatalf("lane slots = %v, want %v (mouth first)", lane.Slots, want)
	}
	if got := sc.LaneForNode("A1-S2"); got != "LANE-A1" {
		t.Errorf("LaneForNode(A1-S2) = %q, want LANE-A1", got)
	}
	if got := sc.LaneForNode("LINE-IN"); got != "" {
		t.Errorf("LaneForNode(LINE-IN) = %q, want empty (not a lane node)", got)
	}
	if d, ok := sc.SlotDepth("A1-S1"); !ok || d != 1 {
		t.Errorf("SlotDepth(A1-S1) = %d,%v, want 1,true", d, ok)
	}
}

// storeReq is a plain store: pick at the line, drop into the lane's deepest slot.
func storeReq(id, source, slot string) fleet.CreateOrderRequest {
	return fleet.CreateOrderRequest{
		OrderID: id,
		Blocks: []fleet.OrderBlock{
			{BlockID: id + "-b1", Location: source, BinTask: "JackLoad"},
			{BlockID: id + "-b2", Location: slot, BinTask: "JackUnload"},
		},
		Complete: true,
	}
}

// TestS0Gate_PlainStoreEndToEnd is the S0 gate: the tick loop runs a plain store
// to completion on a real plant scene — robot travels to the source, picks up,
// enters the lane single-file, drops at the deepest slot, exits — with the mode-
// purity and no-deadlock checkers live and clean throughout.
func TestS0Gate_PlainStoreEndToEnd(t *testing.T) {
	sc := plantScene(t)
	sim := New(sc, Options{})
	if err := sim.AddRobot("R1", "AISLE"); err != nil {
		t.Fatalf("AddRobot: %v", err)
	}
	if err := sim.Submit("R1", storeReq("O1", "LINE-IN", "A1-S2"), false); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	ticks, violations, settled := sim.RunUntilIdle(500)
	if !settled {
		t.Fatalf("store did not complete within budget (ran %d ticks)", ticks)
	}
	if len(violations) != 0 {
		t.Fatalf("checkers fired on a healthy store: %+v", violations)
	}
	if !sim.AllIdle() {
		t.Error("robot not idle after completing the store")
	}
}

// TestS0_TwoSameModeStoresShareLane: two stores into the same lane are same-mode
// (both inbound); entered deepest-first (correct §5 discipline), both complete
// single-file with no violation — the concurrency the plants rely on. (Entered
// shallow-first they would air-bubble; that wound is the S1 reproduction.)
func TestS0_TwoSameModeStoresShareLane(t *testing.T) {
	sc := plantScene(t)
	sim := New(sc, Options{})
	_ = sim.AddRobot("R1", "AISLE")
	_ = sim.AddRobot("R2", "AISLE")

	// Deep store first, to completion.
	_ = sim.Submit("R1", storeReq("O1", "LINE-IN", "A1-S2"), false)
	_, v1, done1 := sim.RunUntilIdle(500)
	// Then the shallow store.
	_ = sim.Submit("R2", storeReq("O2", "LINE-IN", "A1-S1"), false)
	_, v2, done2 := sim.RunUntilIdle(500)

	if !done1 || !done2 {
		t.Fatalf("same-mode stores did not settle (deep=%v shallow=%v)", done1, done2)
	}
	for _, v := range append(v1, v2...) {
		t.Errorf("checker fired on a legal deepest-first same-mode pair: %s: %s", v.Checker, v.Detail)
	}
}
