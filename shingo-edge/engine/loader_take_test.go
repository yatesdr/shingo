package engine

import (
	"slices"
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// What Core's loader send makes this Edge do to its processes, screens and
// positions (SetCoreLoaders, which ApplyNodeList calls). The pins here hold
// before and after Core names the Edge that works a loader: a send that
// changes nothing writes nothing, a buffer is never a position, a loader with
// no screen here is offered and not made while Core names no Edge, and a
// press's screen is never written by the Edge on its own.

// takePlant is a counting database with the engine's view wired to the
// aggregate loader store, as engine.New wires it.
func takePlant(t *testing.T) (*Engine, *store.DB, *store.QueryCounter) {
	t.Helper()
	db, counter := testEngineDBCounting(t)
	eng := testEngine(t, db)
	eng.loaderStore = newLoaderStore(eng)
	eng.stationService.SetLoaderResolver(stationLoaderResolver{eng})
	return eng, db, counter
}

// takeScreen makes a process with one screen holding positions, in order.
func takeScreen(t *testing.T, db *store.DB, process, screen string, positions ...string) (pid, sid int64) {
	t.Helper()
	pid, err := db.CreateProcess(process, "", "", "", false)
	if err != nil {
		t.Fatalf("create process %s: %v", process, err)
	}
	sid, err = db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: screen, Enabled: true})
	if err != nil {
		t.Fatalf("create screen %s: %v", screen, err)
	}
	for i, n := range positions {
		if _, err := db.CreateProcessNode(processes.NodeInput{
			ProcessID: pid, OperatorStationID: &sid, CoreNodeName: n,
			Code: strings.ToLower(n), Name: n, Sequence: i + 1, Enabled: true,
		}); err != nil {
			t.Fatalf("create position %s: %v", n, err)
		}
	}
	return pid, sid
}

// screenPositions is the live positions on a screen, in its order.
func screenPositions(t *testing.T, db *store.DB, sid int64) []string {
	t.Helper()
	nodes, err := db.ListProcessNodesByStation(sid)
	if err != nil {
		t.Fatalf("list screen %d: %v", sid, err)
	}
	var out []string
	for _, n := range nodes {
		out = append(out, n.CoreNodeName)
	}
	return out
}

// countRows is how many rows a table holds.
func countRows(t *testing.T, db *store.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// plantWrites is the statements of stmts that write a process, a screen or a
// position: what Core's send made the Edge change beyond its loader cache.
func plantWrites(stmts []string) []string {
	var out []string
	for _, s := range stmts {
		q := strings.ToUpper(strings.TrimSpace(s))
		if strings.HasPrefix(q, "SELECT") || strings.HasPrefix(q, "WITH") {
			continue
		}
		for _, table := range []string{"PROCESS_NODES", "OPERATOR_STATIONS", "PROCESSES", "PROCESS_NODE_RUNTIME"} {
			if strings.Contains(q, table) && !strings.Contains(q, "CORE_LOADER") {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

func mustSetLoaders(t *testing.T, eng *Engine, loaders ...protocol.LoaderInfo) {
	t.Helper()
	if err := eng.SetCoreLoaders(loaders); err != nil {
		t.Fatalf("SetCoreLoaders: %v", err)
	}
}

func TestLoaderTake_TheSameSendTwiceWritesNothingTheSecondTime(t *testing.T) {
	t.Parallel()
	eng, db, counter := takePlant(t)
	_, sid := takeScreen(t, db, "TAKE-LOADER", "TAKE-SCREEN", "TK-W1", "TK-W2")
	send := sharedLoader("TK-W1", "TK-W2", "TK-W3")
	send.LoaderKey = "loader:TK"
	mustSetLoaders(t, eng, send)
	first := screenPositions(t, db, sid)

	// The same send again, as a restart or a resend applies it.
	counter.Reset()
	mustSetLoaders(t, eng, send)
	if w := plantWrites(counter.Statements()); len(w) != 0 {
		t.Errorf("the same send a second time wrote %d statement(s) to processes, screens or positions: %v", len(w), w)
	}
	if got := screenPositions(t, db, sid); !slices.Equal(got, first) {
		t.Errorf("the screen moved on a replayed send: %v, then %v", first, got)
	}
}

func TestLoaderTake_ABufferAddedAndRemovedTakesNothing(t *testing.T) {
	t.Parallel()
	eng, db, counter := takePlant(t)
	_, sid := takeScreen(t, db, "TAKE-LOADER", "TAKE-SCREEN", "TK-H1", "TK-H2")
	homes := []string{"TK-H1", "TK-H2"}
	mustSetLoaders(t, eng, dedicatedLoader(homes, nil))

	counter.Reset()
	mustSetLoaders(t, eng, dedicatedLoader(homes, []string{"TK-B1"}))
	mustSetLoaders(t, eng, dedicatedLoader(homes, nil))
	if w := plantWrites(counter.Statements()); len(w) != 0 {
		t.Errorf("a buffer spot added and removed wrote %v", w)
	}
	if got, want := screenPositions(t, db, sid), homes; !slices.Equal(got, want) {
		t.Errorf("screen = %v, want %v (a buffer spot is drawn from the loader, never a position)", got, want)
	}
}

func TestLoaderTake_ALoaderWithNoScreenHereIsOfferedNotMadeWhileCoreNamesNoEdge(t *testing.T) {
	t.Parallel()
	eng, db, _ := takePlant(t)
	l := sharedLoader("TK-N1", "TK-N2")
	l.LoaderKey = "loader:TK-NEW"
	mustSetLoaders(t, eng, l)

	if n := countRows(t, db, "processes"); n != 0 {
		t.Errorf("%d process(es) made for a loader Core names no Edge for, want none", n)
	}
	if n := countRows(t, db, "operator_stations"); n != 0 {
		t.Errorf("%d screen(s) made, want none", n)
	}
	gaps, err := eng.stationService.LoaderBoardGaps()
	if err != nil {
		t.Fatalf("LoaderBoardGaps: %v", err)
	}
	if len(gaps) != 1 || gaps[0].LoaderKey != "loader:TK-NEW" {
		t.Errorf("gaps = %+v, want the one loader offered for Make a screen", gaps)
	}
}

func TestLoaderTake_APressScreenKeepsASharedWindowCoreRemoves(t *testing.T) {
	t.Parallel()
	eng, db, _ := takePlant(t)
	// The loader's window W2 is also a position on a press's screen, beside a
	// press position no loader holds (SPR's Unloader on Press 4 and Press 6).
	_, press := takeScreen(t, db, "TAKE-PRESS", "PRESS-SCREEN", "TK-PRESS", "TK-W2")
	l := sharedLoader("TK-W1", "TK-W2")
	l.LoaderKey = "loader:TK"
	mustSetLoaders(t, eng, l)

	l.Positions = l.Positions[:1]
	mustSetLoaders(t, eng, l)
	if got, want := screenPositions(t, db, press), []string{"TK-PRESS", "TK-W2"}; !slices.Equal(got, want) {
		t.Errorf("press screen = %v, want %v: a screen with a press position is a person's, never written by Core's send", got, want)
	}
}
