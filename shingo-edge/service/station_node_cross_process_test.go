package service

import (
	"errors"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// One physical slot, two processes. SetNodes adopts by core_node_name within
// the station's OWN process, but nothing stopped a name that is already a LIVE
// position of a DIFFERENT process from being written here as a second row: two
// rows resolving one slot, two tiles counting one window, a UOP adjustment
// resolving by name landing on whichever row the lookup answers. The save now
// refuses, naming the other process — EXCEPT when the name sits inside a
// Core-owned loader's window/position set (a shared window IS legitimately
// named by several processes' loaders; the loader aggregate is the thing that
// makes the sharing safe), or when the row already belongs to this process (a
// re-save of the station's own list).

// crossProcessFixture builds two processes, one station on the first, and wires
// nothing — the guards below must fail OPEN without a Core resolver, exactly
// like unknownCoreNodes does.
func crossProcessFixture(t *testing.T) (*StationService, int64, int64) {
	t.Helper()
	db := testdb.Open(t)
	svc := NewStationService(db)
	pidA, err := db.CreateProcess("Press A", "", "", "", "", false)
	if err != nil {
		t.Fatalf("CreateProcess A: %v", err)
	}
	pidB, err := db.CreateProcess("Press B", "", "", "", "", false)
	if err != nil {
		t.Fatalf("CreateProcess B: %v", err)
	}
	if _, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: pidB, CoreNodeName: "FGN_001", Code: "B-POS", Name: "FGN_001",
		Sequence: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProcessNode on B: %v", err)
	}
	id, err := db.CreateOperatorStation(stations.Input{ProcessID: pidA, Name: "Loader 1"})
	if err != nil {
		t.Fatalf("CreateOperatorStation: %v", err)
	}
	return svc, id, pidB
}

func TestSetNodes_RefusesALivePositionOfAnotherProcess(t *testing.T) {
	t.Parallel()
	svc, id, pidB := crossProcessFixture(t)

	err := setNodesErr(svc, id, []string{"FGN_001"})
	if err == nil {
		t.Fatalf("the save went through: FGN_001 is already a live position of process %d", pidB)
	}
	if !strings.Contains(err.Error(), "Press B") {
		t.Errorf("refusal does not name the other process: %q", err)
	}

	// WHOLESALE REPLACE — the refusal must be total, no partial write.
	got := nodeNames(t, svc, id)
	if len(got) != 0 {
		t.Errorf("partial write on a rejected save: %v", got)
	}
}

func TestSetNodes_SameProcessResaveStillAccepted(t *testing.T) {
	t.Parallel()
	svc, id, _ := crossProcessFixture(t)

	// The node belongs to the STATION's own process — the ordinary shape, a
	// position created for this process and re-saved with its station.
	pid, err := svc.db.GetOperatorStation(id)
	if err != nil {
		t.Fatalf("GetOperatorStation: %v", err)
	}
	if _, err := svc.db.CreateProcessNode(processes.NodeInput{
		ProcessID: pid.ProcessID, CoreNodeName: "OWN_001", Code: "OWN", Name: "OWN_001",
		Sequence: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProcessNode own: %v", err)
	}

	testutil.MustNoErr(t, setNodesErr(svc, id, []string{"OWN_001"}), "first save")
	testutil.MustNoErr(t, setNodesErr(svc, id, []string{"OWN_001"}), "re-save")

	// Still ONE row: the re-save adopts in place, it does not mint a second.
	got := nodeNames(t, svc, id)
	if len(got) != 1 || got[0] != "OWN_001" {
		t.Errorf("re-save changed the rows: %v", got)
	}
}

// stubLoaderResolver answers LoaderForNode from one fixed loader, everything
// else a clean miss — the contract SetLoaderResolver's real implementation
// keeps.
type stubLoaderResolver struct {
	loader *domain.Loader
}

func (r stubLoaderResolver) LoaderAt(domain.NodeID, domain.LoaderRole) (*domain.Loader, error) {
	return nil, errors.New("not used by SetNodes")
}

func (r stubLoaderResolver) LoaderForNode(core domain.NodeID) (*domain.Loader, error) {
	if r.loader != nil && r.loader.Contains(core) {
		return r.loader, nil
	}
	return nil, nil
}

// A name inside a Core-owned loader's window set is the shared-window shape:
// the loader aggregate is what makes several processes naming one window safe,
// so the save accepts it where a bare position would be refused.
func TestSetNodes_AcceptsANameInsideALoaderWindow(t *testing.T) {
	t.Parallel()
	svc, id, _ := crossProcessFixture(t)

	loader, err := domain.NewSharedWindowLoader(
		domain.LoaderID("LD-SHARED"), "Shared windows", domain.RoleConsume,
		domain.ReplenishmentOperator,
		[]domain.Window{{Node: "FGN_001"}, {Node: "FGN_002"}},
		nil, // a consume loader may declare no payloads
	)
	if err != nil {
		t.Fatalf("NewSharedWindowLoader: %v", err)
	}
	svc.SetLoaderResolver(stubLoaderResolver{loader: loader})

	testutil.MustNoErr(t, setNodesErr(svc, id, []string{"FGN_001"}), "loader-window save")
	got := nodeNames(t, svc, id)
	if len(got) != 1 || got[0] != "FGN_001" {
		t.Errorf("loader-window name not saved: %v", got)
	}
}

// A name the loader does NOT know is still refused — the window exception does
// not open the door for everything.
func TestSetNodes_LoaderWiredStillRefusesABarePosition(t *testing.T) {
	t.Parallel()
	svc, id, _ := crossProcessFixture(t)

	loader, err := domain.NewSharedWindowLoader(
		domain.LoaderID("LD-SHARED"), "Shared windows", domain.RoleConsume,
		domain.ReplenishmentOperator,
		[]domain.Window{{Node: "FGN_002"}},
		nil,
	)
	if err != nil {
		t.Fatalf("NewSharedWindowLoader: %v", err)
	}
	svc.SetLoaderResolver(stubLoaderResolver{loader: loader})

	err = setNodesErr(svc, id, []string{"FGN_001"})
	if err == nil {
		t.Fatalf("the save went through: FGN_001 is not in the loader's window set")
	}
	if !strings.Contains(err.Error(), "Press B") {
		t.Errorf("refusal does not name the other process: %q", err)
	}
}
