//go:build docker

package messaging

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/store/nodes"
)

// kanban_rings_docker_test.go — what moves a process rings the line of the kanbans
// supporting it, and nothing else does. A ring is a push armed on the Edge watching
// the group; the rings are latency, and the heartbeat's compare stays the floor.

// armed reports whether a push is armed for the Edge, and disarms it.
func armed(f *groupsFixture, station string) bool {
	f.svc.bins.mu.Lock()
	defer f.svc.bins.mu.Unlock()
	timer := f.svc.bins.pending[station]
	if timer == nil {
		return false
	}
	timer.Stop()
	delete(f.svc.bins.pending, station)
	return true
}

func TestKanbanRings_WhatMovesAProcess(t *testing.T) {
	t.Parallel()
	f := newGroupsFixture(t, true)
	f.svc.bins.delay = time.Hour // armed, never fired: the test reads the arming
	f.svc.HandleBinWatch(feedsPinEnv("edge.a"), binsWatchList(t, binsFixtureNames...))

	press := &nodes.Node{Name: "RING-PRESS", Enabled: true}
	testutil.MustNoErr(t, f.db.CreateNode(press), "a supported position")
	testutil.MustNoErr(t, nodes.SetMaintainSupports(f.db.DB, f.group.ID, []int64{press.ID}), "supports")
	report := func(process, node string) *protocol.PlantClaimsReport {
		return &protocol.PlantClaimsReport{ProcessID: process, Styles: []protocol.PlantClaimsStyle{{StyleID: "S", Active: true,
			Claims: []protocol.PlantClaim{{CoreNodeName: node, Role: protocol.ClaimRoleProduce,
				SwapMode: protocol.SwapMode("press_index"), PayloadCode: "RING-PART"}}}}}
	}

	f.svc.HandlePlantClaims(feedsPinEnv("edge.a"), report("RING-ELSEWHERE", "RING-OTHER-NODE"))
	if armed(f, "edge.a") {
		t.Error("the plant claims of a process no kanban supports rang")
	}
	f.svc.HandlePlantClaims(feedsPinEnv("edge.a"), report("RING-P", press.Name))
	if !armed(f, "edge.a") {
		t.Error("the plant claims of the supported process did not ring")
	}

	origin := func(id, kind string, closed bool) *protocol.DemandOriginState {
		st := &protocol.DemandOriginState{OriginID: id, Revision: 1, EpisodeKey: kind + "|" + id, Kind: kind,
			ProcessID: "RING-P", CoreNodeName: press.Name, PayloadCode: "RING-PART", OpenedAt: time.Now().UTC()}
		if closed {
			now := time.Now().UTC()
			st.Revision, st.ClosedAt, st.CloseReason = 2, &now, "cancelled"
		}
		return st
	}
	const co = "abababab-0000-4000-8000-000000000001"
	f.svc.HandleDemandOrigin(feedsPinEnv("edge.a"), origin(co, protocol.EpisodeKindChangeover, false))
	if !armed(f, "edge.a") {
		t.Error("a changeover opening did not ring")
	}
	f.svc.HandleDemandOrigin(feedsPinEnv("edge.a"), origin(co, protocol.EpisodeKindChangeover, true))
	if !armed(f, "edge.a") {
		t.Error("a changeover closing did not ring")
	}
	const cell = "abababab-0000-4000-8000-000000000002"
	f.svc.HandleDemandOrigin(feedsPinEnv("edge.a"), origin(cell, protocol.EpisodeKindCell, false))
	if armed(f, "edge.a") {
		t.Error("a cell episode of the same process rang: only a changeover moves the cover")
	}
}

// A plant with no Edge watching a group's line reads nothing for a ring.
func TestKanbanRings_NoWatcherNoStatement(t *testing.T) {
	t.Parallel()
	f := newGroupsFixture(t, true)
	f.counter.Reset()
	f.svc.ringProcessKanbans("RING-NOBODY")
	if n := f.counter.Count(); n != 0 {
		t.Errorf("a ring with no Edge watching read %d statements: %v", n, f.counter.Statements())
	}
}
