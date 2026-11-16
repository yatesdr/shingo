package engine

import (
	"context"
	"slices"
	"testing"

	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// A CORE LOADER'S BOARD DRAWS THE SCREEN'S POSITIONS, NOT THE LOADER'S WINDOWS.
// These pin what happens on a board when Core changes a loader whose process
// already has a screen on this Edge, through Core's own reply path
// (SetCoreLoaders, which ApplyNodeList calls) and the station's view
// (BuildView, then StampBins as the view handler stamps it).
//
//   - A window or home Core adds is no tile: the tiles are the screen's
//     process_nodes (adoptChangeoverParticipants reads ListProcessNodesByStation),
//     and Core's send writes none.
//   - A dedicated loader's buffer spot Core adds is on the board at once: the
//     strip is read from the loader itself (bufferStrip), not from positions.
//   - A window Core removes stays a position on the screen, with no loader and
//     no claim on its tile, which the board does not draw (operator-state.js
//     claimedNodes keeps a tile with a loader, a claim or a changeover task).
//
// The rig on a cut-off SPR copy shows the same (round 8, shots 01-04).

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
	if want := []string{"LWC-H1", "LWC-H2"}; !slices.Equal(added.positions, want) || !slices.Equal(added.drawn, want) {
		t.Errorf("after Core adds LWC-H3: positions %v, drawn %v; want both %v (a home Core adds is no tile until it is a position on the screen)",
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
	if want := []string{"LWC-H1", "LWC-H2"}; !slices.Equal(removed.positions, want) {
		t.Errorf("after Core removes LWC-H2: positions = %v, want %v (it stays a position on the screen)", removed.positions, want)
	}
	if want := []string{"LWC-H1"}; !slices.Equal(removed.drawn, want) {
		t.Errorf("after Core removes LWC-H2: drawn = %v, want %v (its tile has no loader and no claim)", removed.drawn, want)
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
	if got, want := readBoard(t, eng, sid).drawn, []string{"LWC-W1", "LWC-W2"}; !slices.Equal(got, want) {
		t.Errorf("after Core adds LWC-W3: drawn = %v, want %v (a window Core adds is no tile)", got, want)
	}
	if err := eng.SetCoreLoaders([]protocol.LoaderInfo{sharedLoader("LWC-W1", "LWC-W3")}); err != nil {
		t.Fatalf("SetCoreLoaders: %v", err)
	}
	r := readBoard(t, eng, sid)
	if want := []string{"LWC-W1", "LWC-W2"}; !slices.Equal(r.positions, want) {
		t.Errorf("after Core removes LWC-W2: positions = %v, want %v", r.positions, want)
	}
	if want := []string{"LWC-W1"}; !slices.Equal(r.drawn, want) {
		t.Errorf("after Core removes LWC-W2: drawn = %v, want %v", r.drawn, want)
	}
}
