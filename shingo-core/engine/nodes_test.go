//go:build docker

package engine

import (
	"errors"
	"strings"
	"testing"

	"shingocore/fleet"
	"shingocore/fleet/simulator"
)

// nodes_test.go — coverage for nodes.go.
//
// Covers GetNodeOccupancy (both fleet-supported and unsupported paths)
// plus the IsFleetUnsupported / errFleetUnsupported helper pair.

// fakeOccupancyBackend wraps the simulator and adds GetNodeOccupancy so
// it satisfies fleet.NodeOccupancyProvider — the simulator on its own
// does not, which is exactly what the unsupported-path test needs.
type fakeOccupancyBackend struct {
	*simulator.SimulatorBackend
	locations []fleet.OccupancyDetail
	err       error
}

func (f *fakeOccupancyBackend) GetNodeOccupancy(groups ...string) ([]fleet.OccupancyDetail, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.locations, nil
}

// ── helpers ─────────────────────────────────────────────────────────

// TestErrFleetUnsupported_FormatAndIs covers the helper round-trip:
// the constructor returns an error whose Error() reads sensibly and
// IsFleetUnsupported correctly identifies it (and nothing else).
func TestErrFleetUnsupported_FormatAndIs(t *testing.T) {
	err := errFleetUnsupported("occupancy status")
	if err == nil {
		t.Fatal("errFleetUnsupported returned nil")
	}
	if !strings.Contains(err.Error(), "occupancy status") {
		t.Errorf("Error() = %q, want feature name embedded", err.Error())
	}
	if !strings.Contains(err.Error(), "fleet backend does not support") {
		t.Errorf("Error() = %q, want canonical prefix", err.Error())
	}

	if !IsFleetUnsupported(err) {
		t.Error("IsFleetUnsupported should return true for errFleetUnsupported value")
	}
	if IsFleetUnsupported(errors.New("totally unrelated")) {
		t.Error("IsFleetUnsupported must return false for a generic error")
	}
	if IsFleetUnsupported(nil) {
		t.Error("IsFleetUnsupported(nil) must return false")
	}
}

// ── GetNodeOccupancy: unsupported backend ───────────────────────────

// TestGetNodeOccupancy_Unsupported_SimulatorBackend confirms the type
// assertion bails out and the engine returns the IsFleetUnsupported
// sentinel when fleet is a plain SimulatorBackend (no NodeOccupancy).
func TestGetNodeOccupancy_Unsupported_SimulatorBackend(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	out, err := eng.GetNodeOccupancy()
	if err == nil {
		t.Fatal("expected error from unsupported backend, got nil")
	}
	if !IsFleetUnsupported(err) {
		t.Errorf("err = %v, want IsFleetUnsupported(true)", err)
	}
	if out != nil {
		t.Errorf("expected nil result on unsupported, got %+v", out)
	}
}

// ── GetNodeOccupancy: supported backend, mixed cases ────────────────

// TestGetNodeOccupancy_DiscrepancyClassification proves the comparison
// loop produces three categories:
//
//   - matched: fleet location AND a node with that name → no Discrepancy
//   - fleet_only: fleet has it, ShinGo doesn't
//   - shingo_only: ShinGo has it, fleet doesn't
//
// Matching nodes are also flagged InShinGo=true so the UI can shade them.
func TestGetNodeOccupancy_DiscrepancyClassification(t *testing.T) {
	db := testDB(t)
	storageNode, _, _ := setupTestData(t, db)
	// "STORAGE-A1" is the standard storage node; pretend the fleet sees it
	// plus an extra location ShinGo doesn't know about. ShinGo also has
	// LINE1-IN, which the fleet doesn't report → classified as shingo_only.
	fakeFleet := &fakeOccupancyBackend{
		SimulatorBackend: simulator.New(),
		locations: []fleet.OccupancyDetail{
			{ID: storageNode.Name, Occupied: true},
			{ID: "FLEET-ONLY-1", Occupied: false},
		},
	}
	eng := newTestEngine(t, db, fakeFleet.SimulatorBackend)
	// Replace the fleet with our wrapper after start so the engine
	// dispatches calls through it. (Wiring/dispatcher already use the
	// underlying simulator — that's fine, we only care about the Engine
	// type assertion in GetNodeOccupancy.)
	eng.fleet = fakeFleet

	results, err := eng.GetNodeOccupancy()
	if err != nil {
		t.Fatalf("GetNodeOccupancy: %v", err)
	}

	byID := make(map[string]OccupancyEntry, len(results))
	for _, r := range results {
		byID[r.LocationID] = r
	}

	matched, ok := byID[storageNode.Name]
	if !ok {
		t.Fatalf("matched location %s missing from results: %+v", storageNode.Name, results)
	}
	if !matched.InShinGo {
		t.Error("matched location should have InShinGo=true")
	}
	if matched.Discrepancy != "" {
		t.Errorf("matched location should have empty Discrepancy, got %q", matched.Discrepancy)
	}
	if matched.FleetOccupied == nil || !*matched.FleetOccupied {
		t.Errorf("matched location FleetOccupied = %v, want true", matched.FleetOccupied)
	}

	fleetOnly, ok := byID["FLEET-ONLY-1"]
	if !ok {
		t.Fatalf("FLEET-ONLY-1 missing from results: %+v", results)
	}
	if fleetOnly.InShinGo {
		t.Error("fleet-only location should have InShinGo=false")
	}
	if fleetOnly.Discrepancy != "fleet_only" {
		t.Errorf("Discrepancy = %q, want fleet_only", fleetOnly.Discrepancy)
	}

	// LINE1-IN comes from setupTestData — fleet doesn't report it.
	shinGoOnly, ok := byID["LINE1-IN"]
	if !ok {
		t.Fatalf("LINE1-IN missing from results: %+v", results)
	}
	if shinGoOnly.Discrepancy != "shingo_only" {
		t.Errorf("Discrepancy = %q, want shingo_only", shinGoOnly.Discrepancy)
	}
	if !shinGoOnly.InShinGo {
		t.Error("shingo-only location should have InShinGo=true")
	}
}

// TestGetNodeOccupancy_FleetError surfaces a fleet-side failure intact:
// the engine wrapper returns it without conversion to IsFleetUnsupported.
func TestGetNodeOccupancy_FleetError(t *testing.T) {
	db := testDB(t)
	fakeFleet := &fakeOccupancyBackend{
		SimulatorBackend: simulator.New(),
		err:              errors.New("rds offline"),
	}
	eng := newTestEngine(t, db, fakeFleet.SimulatorBackend)
	eng.fleet = fakeFleet

	out, err := eng.GetNodeOccupancy()
	if err == nil {
		t.Fatal("expected error from fleet, got nil")
	}
	if IsFleetUnsupported(err) {
		t.Errorf("err = %v should not be IsFleetUnsupported (real fleet error)", err)
	}
	if out != nil {
		t.Errorf("expected nil result on error, got %+v", out)
	}
}

// TestGetNodeOccupancy_ListNodesError_PathExists exercises the second
// error-return: when the fleet succeeds but db.ListNodes is unreachable.
// We can't easily trigger ListNodes failure in-process, so this test
// instead asserts the empty-empty case (no fleet locations + no nodes
// in DB) returns an empty slice without error — characterizes the
// happy-but-empty branch.
func TestGetNodeOccupancy_EmptyFleetEmptyDB(t *testing.T) {
	db := testDB(t)
	fakeFleet := &fakeOccupancyBackend{
		SimulatorBackend: simulator.New(),
		locations:        nil,
	}
	eng := newTestEngine(t, db, fakeFleet.SimulatorBackend)
	eng.fleet = fakeFleet

	out, err := eng.GetNodeOccupancy()
	if err != nil {
		t.Fatalf("GetNodeOccupancy: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty result, got %+v", out)
	}
}

// Compile-time check that fakeOccupancyBackend really does satisfy the
// interface — guards against future fleet refactors silently breaking
// the test fixture.
var _ fleet.NodeOccupancyProvider = (*fakeOccupancyBackend)(nil)
