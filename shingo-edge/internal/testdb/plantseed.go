package testdb

import (
	"testing"

	"shingo/protocol"
	"shingo/shared/scenefixtures"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/catalog"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// SeededPlant is what SeedPlant wrote: the Edge row ids the fixture's ids
// became, keyed by the fixture's names.
type SeededPlant struct {
	ProcessID int64
	// Stations by fixture station code; Styles by style name; Nodes by core
	// node name.
	Stations map[string]int64
	Styles   map[string]int64
	Nodes    map[string]int64
}

// SeedPlant writes one process of a real plant pull into an Edge database
// through the store's own write paths — the process, its stations, its
// process nodes (station binding preserved, stationless rows stay
// stationless), its styles and their claims, and the active style — so a
// test can build a station view over the plant's real configuration.
//
// Through UpsertClaim, not raw INSERTs, on purpose: the pull's claims are
// live rows and every one of them passes the validator today. A fixture row
// that stopped passing would be a finding about the validator, and this
// helper fails loudly on it rather than smuggling the row past.
//
// Orphaned claims (style rows gone at the plant — A has four) are skipped:
// they belong to no style the seeded process can run.
func SeedPlant(t *testing.T, db *store.DB, fx scenefixtures.Plant, processName string) SeededPlant {
	t.Helper()
	var proc *scenefixtures.Process
	for i := range fx.Processes {
		if fx.Processes[i].Name == processName {
			proc = &fx.Processes[i]
		}
	}
	if proc == nil {
		t.Fatalf("SeedPlant: %s has no process %q", fx.Plant, processName)
	}
	out := SeededPlant{Stations: map[string]int64{}, Styles: map[string]int64{}, Nodes: map[string]int64{}}
	var err error
	out.ProcessID, err = db.CreateProcess(proc.Name, "synthetic plant "+fx.Plant, "active_production", "", "", false)
	if err != nil {
		t.Fatalf("SeedPlant: create process: %v", err)
	}
	// The payload catalog first: a claim's UOP capacity is resolved from it on
	// every read, so a plant seeded without it reads every capacity as 0 and a
	// test about capacity would be about nothing.
	for _, e := range fx.PayloadCatalog {
		if err := db.UpsertPayloadCatalog(&catalog.CatalogEntry{
			ID: e.ID, Name: e.Name, Code: e.Code, UOPCapacity: e.UOPCapacity,
		}); err != nil {
			t.Fatalf("SeedPlant: catalog %s: %v", e.Code, err)
		}
	}

	stationByFixtureID := map[int64]int64{}
	for _, st := range fx.OperatorStations {
		if st.ProcessID != proc.ID {
			continue
		}
		id, err := db.CreateOperatorStation(stations.Input{ProcessID: out.ProcessID, Code: st.Code, Name: st.Name, Sequence: 1, Enabled: true})
		if err != nil {
			t.Fatalf("SeedPlant: create station %s: %v", st.Code, err)
		}
		out.Stations[st.Code] = id
		stationByFixtureID[st.ID] = id
	}
	for _, n := range fx.ProcessNodes {
		if n.ProcessID != proc.ID || n.DeletedAt != nil {
			continue
		}
		in := processes.NodeInput{ProcessID: out.ProcessID, CoreNodeName: n.CoreNodeName, Code: n.Code, Name: n.Name, Sequence: n.Sequence, Enabled: n.Enabled == 1}
		if n.OperatorStationID != nil {
			if sid, ok := stationByFixtureID[*n.OperatorStationID]; ok {
				in.OperatorStationID = &sid
			}
		}
		id, err := db.CreateProcessNode(in)
		if err != nil {
			t.Fatalf("SeedPlant: create node %s: %v", n.CoreNodeName, err)
		}
		out.Nodes[n.CoreNodeName] = id
	}
	styleByFixtureID := map[int64]int64{}
	for _, s := range fx.Styles {
		if s.ProcessID != proc.ID || s.DeletedAt != nil {
			continue
		}
		id, err := db.CreateStyle(s.Name, "", out.ProcessID)
		if err != nil {
			t.Fatalf("SeedPlant: create style %s: %v", s.Name, err)
		}
		out.Styles[s.Name] = id
		styleByFixtureID[s.ID] = id
	}
	for _, c := range fx.StyleNodeClaims {
		sid, ok := styleByFixtureID[c.StyleID]
		if !ok {
			continue // orphan: its style row is gone at the plant
		}
		if _, err := processes.UpsertClaim(db.DB, domain.NodeClaimInput{
			StyleID: sid, CoreNodeName: c.CoreNodeName, Role: protocol.ClaimRole(c.Role), SwapMode: protocol.SwapMode(c.SwapMode),
			PayloadCode:    c.PayloadCode,
			InboundStaging: c.InboundStaging, OutboundStaging: c.OutboundStaging,
			InboundSource: c.InboundSource, OutboundDestination: c.OutboundDestination,
			PairedCoreNode: c.PairedCoreNode, SecondPairedCoreNode: c.SecondPairedCoreNode,
		}); err != nil {
			t.Fatalf("SeedPlant: claim %s on style %d: %v", c.CoreNodeName, c.StyleID, err)
		}
	}
	if proc.ActiveStyleID != nil {
		if sid, ok := styleByFixtureID[*proc.ActiveStyleID]; ok {
			if err := db.SetActiveStyle(out.ProcessID, &sid); err != nil {
				t.Fatalf("SeedPlant: set active style: %v", err)
			}
		}
	}
	return out
}

// SceneGeometryOf builds the Edge cache shape from a pull, the way a full
// node-list response would have.
func SceneGeometryOf(fx scenefixtures.Plant) *domain.SceneGeometry {
	g := &domain.SceneGeometry{Revision: "fixture-" + fx.Plant, Points: map[string]domain.ScenePointGeom{}}
	for _, p := range fx.ScenePoints {
		g.Points[p.InstanceName] = domain.ScenePointGeom{InstanceName: p.InstanceName, ClassName: p.ClassName, X: p.PosX, Y: p.PosY, Dir: p.Dir}
	}
	for _, e := range fx.SceneEdges {
		ge := domain.SceneEdgeGeom{From: e.FromName, To: e.ToName, FromX: e.FromX, FromY: e.FromY, ToX: e.ToX, ToY: e.ToY}
		if e.Ctrl1X != nil && e.Ctrl1Y != nil && e.Ctrl2X != nil && e.Ctrl2Y != nil {
			ge.Handles = &[4]float64{*e.Ctrl1X, *e.Ctrl1Y, *e.Ctrl2X, *e.Ctrl2Y}
		}
		g.Edges = append(g.Edges, ge)
	}
	return g
}

// GroupsOf is NGRP name → member node names from a pull's nodes table, the
// shape engine.CoreNodeGroups retains from the node list.
func GroupsOf(fx scenefixtures.Plant) map[string][]string {
	out := map[string][]string{}
	for _, n := range fx.Nodes {
		if n.ParentName != nil && *n.ParentName != "" {
			out[*n.ParentName] = append(out[*n.ParentName], n.Name)
		}
	}
	return out
}

// BinTypeOf resolves a payload code to its first bin type in the pull.
func BinTypeOf(fx scenefixtures.Plant) func(string) string {
	m := fx.PayloadBinTypeCodes()
	return func(code string) string {
		if codes := m[code]; len(codes) > 0 {
			return codes[0]
		}
		return ""
	}
}

// SeedRoutingSet derives a process's routing set and adopts every row, which
// is the state a plant is in once an engineer has been through the backfill's
// offer.
//
// SIX TESTS WROTE THIS PREAMBLE BY HAND, nine lines apiece, because the
// derived rows arrive DISABLED — the backfill proposes, the engineer adopts —
// and an un-adopted routing set leaves every option list in the position panel
// empty. A test that forgets it does not fail loudly; it quietly asks its
// question of a flow with no destinations to name, which is the one shape none
// of them meant to test. `by` is the adopter's name, as the row records it.
func SeedRoutingSet(t *testing.T, db *store.DB, processID int64, by string) {
	t.Helper()
	if _, err := db.DeriveRoutingNodesForProcess(processID, func(string) bool { return false }); err != nil {
		t.Fatalf("SeedRoutingSet: derive: %v", err)
	}
	rows, err := db.ListRoutingNodes(processID)
	if err != nil {
		t.Fatalf("SeedRoutingSet: list: %v", err)
	}
	for _, r := range rows {
		if err := db.SetRoutingNodeEnabled(processID, r.ID, true, by); err != nil {
			t.Fatalf("SeedRoutingSet: adopt %s: %v", r.CoreNodeName, err)
		}
	}
}
