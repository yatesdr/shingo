package service

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/internal/testdb"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// Springfield 2026-08-26: the loader was correct on Core and no screen existed
// for it, and neither end said so. Core cannot see edge stations, and the Edge
// draws a board only where somebody put the loader's windows into a process by
// hand.
//
// The threshold — ZERO bound windows, not "some unbound" — is the design
// decision these pin. Partial binding is the normal state of a real loader.

func boardFixture(t *testing.T) (*store.DB, *StationService) {
	t.Helper()
	db := testdb.Open(t)
	svc := NewStationService(db)
	return db, svc
}

func cacheLoader(t *testing.T, db *store.DB, key, name, layout string, windows ...string) {
	t.Helper()
	pos := make([]protocol.LoaderPosition, 0, len(windows))
	for _, w := range windows {
		pos = append(pos, protocol.LoaderPosition{CoreNodeName: w})
	}
	err := db.ReplaceCoreLoaders([]protocol.LoaderInfo{{
		LoaderKey: key, Name: name, Role: "consume", Layout: layout,
		Replenishment: "operator", Positions: pos,
	}})
	if err != nil {
		t.Fatalf("ReplaceCoreLoaders: %v", err)
	}
}

// bindWindow models a window that is modelled as a process_node, optionally on a
// station. onStation=false is the orphan case: the node exists, no screen does.
func bindWindow(t *testing.T, db *store.DB, processID int64, stationID *int64, node string) {
	t.Helper()
	if _, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, OperatorStationID: stationID,
		CoreNodeName: node, Name: node, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProcessNode(%s): %v", node, err)
	}
}

func TestLoaderBoardGaps_ReportsALoaderWithNoScreen(t *testing.T) {
	t.Parallel()
	db, svc := boardFixture(t)
	cacheLoader(t, db, "loader:9", "Unloader", "shared_window", "ULN_002", "ULN_003")

	gaps, err := svc.LoaderBoardGaps()
	if err != nil {
		t.Fatalf("LoaderBoardGaps: %v", err)
	}
	if len(gaps) != 1 || gaps[0].LoaderKey != "loader:9" {
		t.Fatalf("expected loader:9 reported, got %+v", gaps)
	}
	if len(gaps[0].Windows) != 2 {
		t.Errorf("expected both windows listed, got %v", gaps[0].Windows)
	}
	// No process holds them, so there is nothing to infer and the caller must ask.
	if gaps[0].ProcessID != 0 {
		t.Errorf("inferred a process from nothing: %d", gaps[0].ProcessID)
	}
}

// THE ONE THAT KEEPS THIS PANEL HONEST. Springfield's dedicated supermarket
// loader has 33 positions: 22 on the Bin Loader screen and 11 buffer slots no
// operator ever works. Reporting "unbound windows" would put a permanent 11-row
// complaint on a correctly configured loader.
func TestLoaderBoardGaps_PartiallyBoundIsNotAGap(t *testing.T) {
	t.Parallel()
	db, svc := boardFixture(t)
	cacheLoader(t, db, "loader:7", "Supermarket", "dedicated_positions",
		"SMN_014", "SMN_015", "SMN_003", "SMN_004")

	pid, _ := db.CreateProcess("Bin Loader", "", "", "", false)
	sid, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "Loader"})
	bindWindow(t, db, pid, &sid, "SMN_014")
	bindWindow(t, db, pid, &sid, "SMN_015")
	// SMN_003 / SMN_004 are buffer slots: no process_node, no screen, correctly.

	gaps, err := svc.LoaderBoardGaps()
	if err != nil {
		t.Fatalf("LoaderBoardGaps: %v", err)
	}
	if len(gaps) != 0 {
		t.Errorf("a partially bound loader is not a gap, got %+v", gaps)
	}
}

// The nodes are modelled but nobody made the screen — so the process IS known,
// and the panel should not make the operator restate it.
func TestLoaderBoardGaps_InfersTheProcessHoldingTheWindows(t *testing.T) {
	t.Parallel()
	db, svc := boardFixture(t)
	cacheLoader(t, db, "loader:9", "Unloader", "shared_window", "ULN_002", "ULN_003")

	pid, _ := db.CreateProcess("Press 4", "", "", "", false)
	bindWindow(t, db, pid, nil, "ULN_002")
	bindWindow(t, db, pid, nil, "ULN_003")

	gaps, err := svc.LoaderBoardGaps()
	if err != nil {
		t.Fatalf("LoaderBoardGaps: %v", err)
	}
	if len(gaps) != 1 {
		t.Fatalf("expected the loader reported, got %+v", gaps)
	}
	if gaps[0].ProcessID != pid || gaps[0].ProcessName != "Press 4" {
		t.Errorf("process not inferred: %+v", gaps[0])
	}
}

// A loader with no members has nothing to bind, and Core's own box already says
// so at the place it gets fixed. Two complaints for one fault is one too many.
func TestLoaderBoardGaps_SkipsALoaderWithNoWindows(t *testing.T) {
	t.Parallel()
	db, svc := boardFixture(t)
	cacheLoader(t, db, "loader:9", "Unloader", "shared_window")

	gaps, err := svc.LoaderBoardGaps()
	if err != nil {
		t.Fatalf("LoaderBoardGaps: %v", err)
	}
	if len(gaps) != 0 {
		t.Errorf("a windowless loader is Core's complaint, not this one: %+v", gaps)
	}
}

func TestCreateLoaderBoard_MakesTheScreenAndBindsWindows(t *testing.T) {
	t.Parallel()
	db, svc := boardFixture(t)
	cacheLoader(t, db, "loader:9", "Unloader", "shared_window", "ULN_002", "ULN_003")
	pid, _ := db.CreateProcess("Press 4", "", "", "", false)

	id, err := svc.CreateLoaderBoard("loader:9", pid)
	if err != nil {
		t.Fatalf("CreateLoaderBoard: %v", err)
	}
	if got := nodeNames(t, svc, id); len(got) != 2 {
		t.Errorf("windows not bound to the new screen: %v", got)
	}
	// And the gap closes.
	gaps, err := svc.LoaderBoardGaps()
	if err != nil {
		t.Fatalf("LoaderBoardGaps: %v", err)
	}
	if len(gaps) != 0 {
		t.Errorf("gap still reported after making the screen: %+v", gaps)
	}
}

// A screen with no nodes is exactly the looks-configured-does-nothing state this
// work exists to remove, so a failed bind must not leave one behind.
func TestCreateLoaderBoard_RollsBackTheScreenWhenBindingFails(t *testing.T) {
	t.Parallel()
	db, svc := boardFixture(t)
	cacheLoader(t, db, "loader:9", "Unloader", "shared_window", "ULN_002")
	pid, _ := db.CreateProcess("Press 4", "", "", "", false)
	// Core knows a different node, so binding ULN_002 is refused.
	svc.SetCoreNodeResolver(func() map[string]bool { return map[string]bool{"PLN_001": true} })

	if _, err := svc.CreateLoaderBoard("loader:9", pid); err == nil {
		t.Fatal("expected the bind to be refused")
	}
	list, err := db.ListOperatorStations()
	if err != nil {
		t.Fatalf("ListOperatorStations: %v", err)
	}
	for _, st := range list {
		if st.ProcessID == pid {
			t.Fatalf("orphan screen left behind: %+v", st)
		}
	}
}

func TestCreateLoaderBoard_RefusesWhatItCannotBind(t *testing.T) {
	t.Parallel()
	db, svc := boardFixture(t)
	cacheLoader(t, db, "loader:9", "Unloader", "shared_window")
	pid, _ := db.CreateProcess("Press 4", "", "", "", false)

	if _, err := svc.CreateLoaderBoard("loader:404", pid); err == nil {
		t.Error("expected an unknown loader key to be refused")
	}
	_, err := svc.CreateLoaderBoard("loader:9", pid)
	if err == nil || !strings.Contains(err.Error(), "no windows") {
		t.Errorf("expected a windowless loader to be refused, got %v", err)
	}
	// A board belongs to ONE process on ONE edge; that is not inferable here.
	if _, err := svc.CreateLoaderBoard("loader:9", 0); err == nil {
		t.Error("expected a missing process_id to be refused")
	}
}

// cacheDedicated caches one dedicated loader whose members are homes (each
// with its own part) and buffer spots, in the order given by kinds: "home"
// or "buffer" for each node of nodes.
func cacheDedicated(t *testing.T, db *store.DB, key string, nodes, kinds []string) {
	t.Helper()
	pos := make([]protocol.LoaderPosition, 0, len(nodes))
	for i, n := range nodes {
		p := protocol.LoaderPosition{CoreNodeName: n, HomeKind: kinds[i], Ordinal: i}
		if kinds[i] == protocol.LoaderHomeKindHome {
			p.PayloadCode = "PART-" + n
		}
		pos = append(pos, p)
	}
	if err := db.ReplaceCoreLoaders([]protocol.LoaderInfo{{
		LoaderKey: key, Name: "Dedicated", Role: "produce", Layout: "dedicated_positions",
		Replenishment: "threshold", Positions: pos,
	}}); err != nil {
		t.Fatalf("ReplaceCoreLoaders: %v", err)
	}
}

// "MAKE A SCREEN" PUTS A LOADER'S WINDOWS ON IT, NOT ITS BUFFER SPOTS (round
// 9; owner, 2026-11-16: "it shouldnt put its buffer spots on the screen"). The
// board draws a dedicated loader's buffer spots from the loader (the buffer
// strip); one on the screen as well is a second tile with no part. The homes
// go on in Core's order; the gap the screen closes names the homes only.
func TestCreateLoaderBoard_PutsTheHomesOnTheScreenNotTheBufferSpots(t *testing.T) {
	t.Parallel()
	db, svc := boardFixture(t)
	cacheDedicated(t, db, "loader:7", []string{"SMN_014", "SMN_003", "SMN_015", "SMN_004"},
		[]string{"home", "buffer", "home", "buffer"})
	pid, err := db.CreateProcess("Bin Loader", "", "", "", false)
	testutil.MustNoErr(t, err, "CreateProcess")

	gaps, err := svc.LoaderBoardGaps()
	if err != nil {
		t.Fatalf("LoaderBoardGaps: %v", err)
	}
	if len(gaps) != 1 || strings.Join(gaps[0].Windows, ",") != "SMN_014,SMN_015" {
		t.Fatalf("gaps = %+v, want loader:7 naming SMN_014 and SMN_015 only", gaps)
	}
	id, err := svc.CreateLoaderBoard("loader:7", pid)
	if err != nil {
		t.Fatalf("CreateLoaderBoard: %v", err)
	}
	if got := strings.Join(nodeNames(t, svc, id), ","); got != "SMN_014,SMN_015" {
		t.Errorf("the new screen holds %s, want SMN_014,SMN_015: a buffer spot is drawn from the loader", got)
	}
}

// A dedicated loader with buffer spots and no home has nothing a screen works:
// it is no gap, and "Make a screen" refuses it as it refuses a windowless one.
func TestCreateLoaderBoard_ALoaderOfBufferSpotsOnlyHasNothingToBind(t *testing.T) {
	t.Parallel()
	db, svc := boardFixture(t)
	cacheDedicated(t, db, "loader:8", []string{"SMN_003", "SMN_004"}, []string{"buffer", "buffer"})
	pid, err := db.CreateProcess("Buffers", "", "", "", false)
	testutil.MustNoErr(t, err, "CreateProcess")

	gaps, err := svc.LoaderBoardGaps()
	if err != nil {
		t.Fatalf("LoaderBoardGaps: %v", err)
	}
	if len(gaps) != 0 {
		t.Errorf("gaps = %+v, want none: a loader of buffer spots has no window to bind", gaps)
	}
	if _, err := svc.CreateLoaderBoard("loader:8", pid); err == nil || !strings.Contains(err.Error(), "no windows") {
		t.Errorf("CreateLoaderBoard = %v, want it refused for having no windows", err)
	}
}
