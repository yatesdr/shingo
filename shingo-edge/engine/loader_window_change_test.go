package engine

import (
	"context"
	"slices"
	"testing"

	"shingo/protocol"
	"shingoedge/service"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// A CORE LOADER'S BOARD DRAWS THE SCREEN'S POSITIONS, AND CORE'S SEND NOW
// KEEPS THE POSITIONS. These pin what a board shows when Core changes a loader
// whose process already has a screen on this Edge, through Core's own reply
// path (SetCoreLoaders, which ApplyNodeList calls) and the station's view
// (BuildView, then StampBins as the view handler stamps it), with Core naming
// no Edge for the loader (the take's inference, station_loader_take.go):
//
//   - A window or home Core adds goes on the one screen holding the loader's
//     other windows, so it is a tile: the tiles are the screen's process_nodes
//     (adoptChangeoverParticipants reads ListProcessNodesByStation). Before
//     round 10 Core's send wrote none and the new window had no tile (round 8,
//     shots 01-04).
//   - A dedicated loader's buffer spot Core adds is on the board at once: the
//     strip is read from the loader itself (bufferStrip), not from positions,
//     and the take never makes one a position.
//   - A window Core removes comes off the screen.

// loaderChangePlant seeds one process with one screen holding the given
// positions, and wires the engine's view to the aggregate loader store as
// engine.New does.
func loaderChangePlant(t *testing.T, positions ...string) (*Engine, *store.DB, int64) {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.loaderStore = newLoaderStore(eng)
	eng.stationService.SetLoaderResolver(stationLoaderResolver{eng})
	procID, err := db.CreateProcess("LWC-PROC", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	sid, err := db.CreateOperatorStation(stations.Input{ProcessID: procID, Name: "LWC-SCREEN", Enabled: true})
	if err != nil {
		t.Fatalf("create station: %v", err)
	}
	for i, n := range positions {
		if _, err := db.CreateProcessNode(processes.NodeInput{
			ProcessID: procID, OperatorStationID: &sid, CoreNodeName: n,
			Code: n, Name: n, Sequence: i + 1, Enabled: true,
		}); err != nil {
			t.Fatalf("create node %s: %v", n, err)
		}
	}
	return eng, db, sid
}

// boardRead is what one read of the screen gives: every position the view
// holds, the ones the board draws, and the dedicated loader's buffer strip.
type boardRead struct {
	positions, drawn, strip []string
}

func readBoard(t *testing.T, eng *Engine, sid int64) boardRead {
	t.Helper()
	view, err := eng.stationService.BuildView(context.Background(), sid)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	eng.stationService.StampBins(view, nil, false, nil)
	var r boardRead
	for _, tile := range view.Nodes {
		r.positions = append(r.positions, tile.Node.CoreNodeName)
		// operator-state.js claimedNodes: a loader, a claim or a changeover task.
		if tile.Loader != nil || tile.ActiveClaim != nil || tile.ChangeoverTask != nil {
			r.drawn = append(r.drawn, tile.Node.CoreNodeName)
		}
		if tile.Loader != nil {
			for _, spot := range tile.Loader.Buffer {
				r.strip = append(r.strip, spot.Node)
			}
		}
	}
	return r
}

func dedicatedLoader(homes, buffers []string) protocol.LoaderInfo {
	l := protocol.LoaderInfo{Name: "LWC dedicated", LoaderKey: "loader:LWC-D", Role: "produce",
		Layout: "dedicated_positions", Replenishment: "threshold", InboundSource: "EMPTY-SUPER"}
	for _, h := range homes {
		l.Positions = append(l.Positions, protocol.LoaderPosition{CoreNodeName: h, PayloadCode: "PART-" + h,
			HomeKind: protocol.LoaderHomeKindHome, Ordinal: len(l.Positions)})
	}
	for _, b := range buffers {
		l.Positions = append(l.Positions, protocol.LoaderPosition{CoreNodeName: b,
			HomeKind: protocol.LoaderHomeKindBuffer, Ordinal: len(l.Positions)})
	}
	return l
}

func sharedLoader(windows ...string) protocol.LoaderInfo {
	l := protocol.LoaderInfo{Name: "LWC shared", LoaderKey: "loader:LWC-S", Role: "produce",
		Layout: "shared_window", Replenishment: "operator", InboundSource: "EMPTY-SUPER",
		Payloads: []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A"}}}
	for i, w := range windows {
		l.Positions = append(l.Positions, protocol.LoaderPosition{CoreNodeName: w, Ordinal: i})
	}
	return l
}

func TestCoreLoaderChange_ADedicatedBoardDrawsItsScreensHomesAndCoresBuffers(t *testing.T) {
	t.Parallel()
	eng, _, sid := loaderChangePlant(t, "LWC-H1", "LWC-H2")

	// Before: two homes on the screen, one buffer spot no screen holds.
	if err := eng.SetCoreLoaders([]protocol.LoaderInfo{dedicatedLoader([]string{"LWC-H1", "LWC-H2"}, []string{"LWC-B1"})}); err != nil {
		t.Fatalf("SetCoreLoaders: %v", err)
	}
	before := readBoard(t, eng, sid)
	if want := []string{"LWC-H1", "LWC-H2"}; !slices.Equal(before.drawn, want) {
		t.Fatalf("before: drawn = %v, want %v", before.drawn, want)
	}
	if want := []string{"LWC-B1"}; !slices.Equal(before.strip, want) {
		t.Fatalf("before: strip = %v, want %v", before.strip, want)
	}

	// Core adds a home and a buffer spot.
	if err := eng.SetCoreLoaders([]protocol.LoaderInfo{dedicatedLoader([]string{"LWC-H1", "LWC-H2", "LWC-H3"}, []string{"LWC-B1", "LWC-B2"})}); err != nil {
		t.Fatalf("SetCoreLoaders: %v", err)
	}
	added := readBoard(t, eng, sid)
	if want := []string{"LWC-H1", "LWC-H2", "LWC-H3"}; !slices.Equal(added.positions, want) || !slices.Equal(added.drawn, want) {
		t.Errorf("after Core adds LWC-H3: positions %v, drawn %v; want both %v (a home Core adds goes on the screen holding the others)",
			added.positions, added.drawn, want)
	}
	if want := []string{"LWC-B1", "LWC-B2"}; !slices.Equal(added.strip, want) {
		t.Errorf("after Core adds LWC-B2: strip = %v, want %v (a buffer spot is drawn from the loader, with no Edge step)", added.strip, want)
	}

	// Core removes a home that is on the screen.
	if err := eng.SetCoreLoaders([]protocol.LoaderInfo{dedicatedLoader([]string{"LWC-H1", "LWC-H3"}, []string{"LWC-B1", "LWC-B2"})}); err != nil {
		t.Fatalf("SetCoreLoaders: %v", err)
	}
	removed := readBoard(t, eng, sid)
	if want := []string{"LWC-H1", "LWC-H3"}; !slices.Equal(removed.positions, want) || !slices.Equal(removed.drawn, want) {
		t.Errorf("after Core removes LWC-H2: positions %v, drawn %v; want both %v (a home Core removes comes off)",
			removed.positions, removed.drawn, want)
	}
}

func TestCoreLoaderChange_ASharedBoardDrawsItsScreensWindows(t *testing.T) {
	t.Parallel()
	eng, _, sid := loaderChangePlant(t, "LWC-W1", "LWC-W2")
	if err := eng.SetCoreLoaders([]protocol.LoaderInfo{sharedLoader("LWC-W1", "LWC-W2")}); err != nil {
		t.Fatalf("SetCoreLoaders: %v", err)
	}
	if got, want := readBoard(t, eng, sid).drawn, []string{"LWC-W1", "LWC-W2"}; !slices.Equal(got, want) {
		t.Fatalf("before: drawn = %v, want %v", got, want)
	}
	if err := eng.SetCoreLoaders([]protocol.LoaderInfo{sharedLoader("LWC-W1", "LWC-W2", "LWC-W3")}); err != nil {
		t.Fatalf("SetCoreLoaders: %v", err)
	}
	if got, want := readBoard(t, eng, sid).drawn, []string{"LWC-W1", "LWC-W2", "LWC-W3"}; !slices.Equal(got, want) {
		t.Errorf("after Core adds LWC-W3: drawn = %v, want %v (a window Core adds goes on the screen)", got, want)
	}
	if err := eng.SetCoreLoaders([]protocol.LoaderInfo{sharedLoader("LWC-W1", "LWC-W3")}); err != nil {
		t.Fatalf("SetCoreLoaders: %v", err)
	}
	r := readBoard(t, eng, sid)
	if want := []string{"LWC-W1", "LWC-W3"}; !slices.Equal(r.positions, want) || !slices.Equal(r.drawn, want) {
		t.Errorf("after Core removes LWC-W2: positions %v, drawn %v; want both %v", r.positions, r.drawn, want)
	}
}

// THE PROCESS STAYS A LOADER'S, AND ITS PAGE SAYS WHAT CORE CHANGED THAT THE
// SCREEN DOES NOT SHOW. A position Core's loader held is marked a loader
// window (the take marks it on Core's send and at the Edge's start; a
// position write marks it too), and the mark keeps the process Core's
// (CoreBoardProcesses) while a window Core removed is still on its screen:
// here, kept there by a running order. CoreLoaderChanges names it. Without
// the mark the removal made the process a line (round 8, shot 03).
func TestCoreLoaderChange_AWindowCoreRemovesKeepsItsProcessALoaders(t *testing.T) {
	t.Parallel()
	eng, db, sid := loaderChangePlant(t, "LWC-W1", "LWC-W2")
	svc := eng.stationService
	pid := func() int64 {
		st, err := db.GetOperatorStation(sid)
		if err != nil {
			t.Fatalf("station: %v", err)
		}
		return st.ProcessID
	}()
	read := func() (bool, service.CoreLoaderChange, map[string]bool) {
		t.Helper()
		nodes, err := db.ListProcessNodes()
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		boards := svc.CoreBoardProcesses(nodes, nil)
		marks := map[string]bool{}
		for _, n := range nodes {
			marks[n.CoreNodeName] = n.LoaderWindow
		}
		return boards[pid], svc.CoreLoaderChanges(nodes, boards, nil)[pid], marks
	}

	// Rows from before v56: none marked until something marks them.
	if _, _, marks := read(); marks["LWC-W1"] || marks["LWC-W2"] {
		t.Fatalf("the seeded positions start marked: %v", marks)
	}
	if err := eng.SetCoreLoaders([]protocol.LoaderInfo{sharedLoader("LWC-W1", "LWC-W2")}); err != nil {
		t.Fatalf("SetCoreLoaders: %v", err)
	}
	core, change, marks := read()
	if !core || change.Added != nil || change.Removed != nil || !marks["LWC-W1"] || !marks["LWC-W2"] {
		t.Fatalf("after Core's send: core %v, change %+v, marks %v; want Core's, no change, both marked", core, change, marks)
	}

	// An order runs at LWC-W2 when Core adds LWC-W3 and removes LWC-W2.
	order := activeOrderAt(t, db, positionID(t, db, sid, "LWC-W2"))
	if err := eng.SetCoreLoaders([]protocol.LoaderInfo{sharedLoader("LWC-W1", "LWC-W3")}); err != nil {
		t.Fatalf("SetCoreLoaders: %v", err)
	}
	core, change, marks = read()
	if !core {
		t.Errorf("after Core removes LWC-W2 the process is no longer Core's: its page would be a line's")
	}
	if change.Added != nil || !slices.Equal(change.Removed, []string{"LWC-W2"}) || !slices.Equal(change.Kept, []string{"LWC-W2"}) {
		t.Errorf("change = %+v, want LWC-W3 on the screen, and LWC-W2 removed and kept for its order", change)
	}
	if !marks["LWC-W2"] || !marks["LWC-W3"] {
		t.Errorf("marks = %v, want LWC-W2 still marked and LWC-W3 marked as it went on", marks)
	}
	if got := readBoard(t, eng, sid).drawn; !slices.Equal(got, []string{"LWC-W1", "LWC-W3"}) {
		t.Errorf("the board draws %v, want [LWC-W1 LWC-W3]", got)
	}

	// The order ends; the next send takes LWC-W2 off, though the loader set
	// did not change.
	endOrder(t, db, order)
	if err := eng.SetCoreLoaders([]protocol.LoaderInfo{sharedLoader("LWC-W1", "LWC-W3")}); err != nil {
		t.Fatalf("SetCoreLoaders: %v", err)
	}
	core, change, _ = read()
	if !core || change.Added != nil || change.Removed != nil {
		t.Errorf("after the order ends: core %v, change %+v; want Core's and nothing left", core, change)
	}
	if got := screenPositions(t, db, sid); !slices.Equal(got, []string{"LWC-W1", "LWC-W3"}) {
		t.Errorf("screen = %v, want [LWC-W1 LWC-W3]", got)
	}
}

// What is not a change: a buffer spot Core adds (the board draws it from the
// loader), a window that is a position of another process here (it is that
// process's), and a press position no loader ever held (its process is a
// line, not Core's).
func TestCoreLoaderChange_BuffersOtherProcessesAndPressesAreNotChanges(t *testing.T) {
	t.Parallel()
	eng, db, sid := loaderChangePlant(t, "LWC-H1")
	svc := eng.stationService
	st, err := db.GetOperatorStation(sid)
	if err != nil {
		t.Fatalf("station: %v", err)
	}
	other, err := db.CreateProcess("LWC-OTHER", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	for _, n := range []string{"LWC-H2", "LWC-PRESS"} {
		if _, err := db.CreateProcessNode(processes.NodeInput{ProcessID: other, CoreNodeName: n, Code: n, Name: n, Enabled: true}); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}
	if err := eng.SetCoreLoaders([]protocol.LoaderInfo{dedicatedLoader([]string{"LWC-H1", "LWC-H2", "LWC-H3"}, []string{"LWC-B1"})}); err != nil {
		t.Fatalf("SetCoreLoaders: %v", err)
	}
	nodes, err := db.ListProcessNodes()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	boards := svc.CoreBoardProcesses(nodes, nil)
	if !boards[st.ProcessID] || boards[other] {
		t.Fatalf("boards = %v, want only %d (LWC-PRESS was never a loader's)", boards, st.ProcessID)
	}
	changes := svc.CoreLoaderChanges(nodes, boards, nil)
	if got := changes[st.ProcessID]; !slices.Equal(got.Added, []string{"LWC-H3"}) || got.Removed != nil {
		t.Errorf("change = %+v, want added [LWC-H3] only: LWC-H2 is %d's, LWC-B1 is a buffer", got, other)
	}
	if _, ok := changes[other]; ok {
		t.Errorf("a line's process has a change: %+v", changes[other])
	}
}

// A BUFFER SPOT ON THE SCREEN IS A SECOND, EMPTY TILE. A dedicated loader's
// buffer spot that is also a position on the screen resolves to its loader
// like any member (Loader.Contains), so it is drawn as a tile with no part
// (LoadablePayloadCodesAt: a buffer pins none; on the board its sheet says "No
// payloads configured" and offers only CLOSE), and it is in the buffer strip
// as well, read from the loader. "Make a screen" put every member on the new
// screen until round 9 (rig, shots 03-06 of setup-round9).
func TestCoreLoaderChange_ABufferSpotOnTheScreenIsASecondEmptyTile(t *testing.T) {
	t.Parallel()
	eng, _, sid := loaderChangePlant(t, "LWC-H1", "LWC-B1")
	if err := eng.SetCoreLoaders([]protocol.LoaderInfo{dedicatedLoader([]string{"LWC-H1"}, []string{"LWC-B1"})}); err != nil {
		t.Fatalf("SetCoreLoaders: %v", err)
	}
	r := readBoard(t, eng, sid)
	if want := []string{"LWC-H1", "LWC-B1"}; !slices.Equal(r.drawn, want) {
		t.Errorf("drawn = %v, want %v: the buffer spot is a tile of its own", r.drawn, want)
	}
	if want := []string{"LWC-B1"}; !slices.Equal(r.strip, want) {
		t.Errorf("strip = %v, want %v: and it is in the strip", r.strip, want)
	}
	view, err := eng.stationService.BuildView(context.Background(), sid)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	for _, tile := range view.Nodes {
		if tile.Node.CoreNodeName == "LWC-B1" && (tile.Loader == nil || len(tile.Loader.Parts) != 0) {
			t.Errorf("the buffer spot's tile carries loader %+v, want its loader with no part", tile.Loader)
		}
	}
}

// ONE RULE, TWO READERS. "Make a screen" and the loader gaps read Core's
// loaders as the Edge caches them (store.CoreLoader.BoardWindows); "Core
// added" reads them as the loader store projects them (Loader.BoardWindows).
// Both are domain.IsBoardWindow, and for every shape they name the same
// members in the same order: shared windows, dedicated homes (an unpinned one
// included), never a buffer spot.
func TestBoardWindows_TheCacheAndTheAggregateAgree(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	unpinned := dedicatedLoader([]string{"BW-H1"}, []string{"BW-B1"})
	unpinned.LoaderKey = "loader:BW-U"
	unpinned.Positions = append(unpinned.Positions, protocol.LoaderPosition{CoreNodeName: "BW-H2",
		HomeKind: protocol.LoaderHomeKindHome, Ordinal: 2})
	shared := sharedLoader("BW-W2", "BW-W1")
	shared.LoaderKey = "loader:BW-S"
	mixed := dedicatedLoader([]string{"BW-H3", "BW-H4"}, []string{"BW-B2", "BW-B3"})
	mixed.LoaderKey = "loader:BW-D"
	if err := db.ReplaceCoreLoaders([]protocol.LoaderInfo{unpinned, shared, mixed}); err != nil {
		t.Fatalf("ReplaceCoreLoaders: %v", err)
	}
	cached, err := db.ListCoreLoaders()
	if err != nil {
		t.Fatalf("ListCoreLoaders: %v", err)
	}
	want := map[string][]string{
		"loader:BW-U": {"BW-H1", "BW-H2"},
		"loader:BW-S": {"BW-W2", "BW-W1"},
		"loader:BW-D": {"BW-H3", "BW-H4"},
	}
	for _, cl := range cached {
		l, err := projectCoreLoader(cl)
		if err != nil {
			t.Fatalf("project %s: %v", cl.LoaderKey, err)
		}
		var projected []string
		for _, n := range l.BoardWindows() {
			projected = append(projected, string(n))
		}
		if got := cl.BoardWindows(); !slices.Equal(got, want[cl.LoaderKey]) || !slices.Equal(projected, got) {
			t.Errorf("%s: cache %v, aggregate %v; want both %v", cl.LoaderKey, got, projected, want[cl.LoaderKey])
		}
	}
}
