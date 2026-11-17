package engine

import (
	"slices"
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/service"
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

// positionID is the live position named name on the screen.
func positionID(t *testing.T, db *store.DB, sid int64, name string) int64 {
	t.Helper()
	nodes, err := db.ListProcessNodesByStation(sid)
	if err != nil {
		t.Fatalf("list screen %d: %v", sid, err)
	}
	for _, n := range nodes {
		if n.CoreNodeName == name {
			return n.ID
		}
	}
	t.Fatalf("no position %s on screen %d", name, sid)
	return 0
}

// activeOrderAt makes an order running at the position.
func activeOrderAt(t *testing.T, db *store.DB, nodeID int64) int64 {
	t.Helper()
	id, err := db.CreateOrder("take-"+strings.ReplaceAll(t.Name(), "/", "-"), protocol.OrderTypeRetrieve, &nodeID,
		false, 1, "", "", "", protocol.Height{}, false, "", "", "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	return id
}

// endOrder confirms the order: it runs no more.
func endOrder(t *testing.T, db *store.DB, id int64) {
	t.Helper()
	if err := db.UpdateOrderStatus(id, string(protocol.StatusConfirmed)); err != nil {
		t.Fatalf("end order %d: %v", id, err)
	}
}

// ── Core names the Edge that works a loader ──────────────────────────

const takeSelf = "stn-take-self"

// takeEngine is takePlant with this Edge's station uid set, so Core can name it.
func takeEngine(t *testing.T) (*Engine, *store.DB, *store.QueryCounter) {
	t.Helper()
	eng, db, counter := takePlant(t)
	eng.cfg.StationUID = takeSelf
	return eng, db, counter
}

// picked is l with Core naming uid (display name name) to work it.
func picked(l protocol.LoaderInfo, uid, name string) protocol.LoaderInfo {
	l.EdgeStationUID, l.EdgeDisplayName = uid, name
	return l
}

// keyed is l with key and name.
func keyed(l protocol.LoaderInfo, key, name string) protocol.LoaderInfo {
	l.LoaderKey, l.Name = key, name
	return l
}

// screenRow is one screen as the test reads it.
type screenRow struct {
	process, screen, link string
	positions             []string
}

// screensOf is every screen with its process name and positions.
func screensOf(t *testing.T, db *store.DB) []screenRow {
	t.Helper()
	list, err := db.ListOperatorStations()
	if err != nil {
		t.Fatalf("list screens: %v", err)
	}
	var out []screenRow
	for _, st := range list {
		out = append(out, screenRow{process: st.ProcessName, screen: st.Name, link: st.CoreLoaderKey,
			positions: screenPositions(t, db, st.ID)})
	}
	return out
}

// pageLine is the process's row as the list derives it.
func pageLine(t *testing.T, eng *Engine, db *store.DB, pid int64) (bool, service.CoreLoaderChange) {
	t.Helper()
	nodes, err := db.ListProcessNodes()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	links, err := eng.stationService.ScreenLoaderLinks()
	if err != nil {
		t.Fatalf("links: %v", err)
	}
	boards := eng.stationService.CoreBoardProcesses(nodes, links)
	return boards[pid], eng.stationService.CoreLoaderChanges(nodes, boards, links)[pid]
}

func TestLoaderTake_ALoaderWithNoScreenHereUnderEachCase(t *testing.T) {
	t.Parallel()
	t.Run("this Edge: the process and screen are made", func(t *testing.T) {
		t.Parallel()
		eng, db, _ := takeEngine(t)
		mustSetLoaders(t, eng, picked(keyed(sharedLoader("TK-N1", "TK-N2"), "loader:TK-NEW", "TK New"), takeSelf, "THIS EDGE"))
		got := screensOf(t, db)
		if len(got) != 1 || got[0].process != "TK New" || got[0].screen != "TK New" || got[0].link != "loader:TK-NEW" ||
			!slices.Equal(got[0].positions, []string{"TK-N1", "TK-N2"}) {
			t.Fatalf("screens = %+v, want one process and screen named TK New, naming loader:TK-NEW, holding both windows", got)
		}
		if gaps, err := eng.stationService.LoaderBoardGaps(); err != nil || len(gaps) != 0 {
			t.Errorf("gaps = %+v (%v), want none: the loader has its screen", gaps, err)
		}
	})
	t.Run("another Edge: nothing is made or offered", func(t *testing.T) {
		t.Parallel()
		eng, db, _ := takeEngine(t)
		mustSetLoaders(t, eng, picked(keyed(sharedLoader("TK-N1", "TK-N2"), "loader:TK-NEW", "TK New"), "stn-other", "EDGE 2"))
		if n := countRows(t, db, "processes"); n != 0 {
			t.Errorf("%d process(es) made for another Edge's loader", n)
		}
		if gaps, err := eng.stationService.LoaderBoardGaps(); err != nil || len(gaps) != 0 {
			t.Errorf("gaps = %+v (%v), want none: Make a screen is that Edge's", gaps, err)
		}
		pid, err := db.CreateProcess("TK EMPTY", "", "", "", false)
		if err != nil {
			t.Fatalf("create process: %v", err)
		}
		if _, err := eng.stationService.CreateLoaderBoard("loader:TK-NEW", pid); err == nil || !strings.Contains(err.Error(), "EDGE 2") {
			t.Errorf("CreateLoaderBoard = %v, want refused naming EDGE 2", err)
		}
	})
	// No Edge picked: TestLoaderTake_ALoaderWithNoScreenHereIsOfferedNotMadeWhileCoreNamesNoEdge.
}

func TestLoaderTake_CoreNamingAnotherEdgeTakesTheWindowsOffAndKeepsTheScreen(t *testing.T) {
	t.Parallel()
	eng, db, _ := takeEngine(t)
	pid, sid := takeScreen(t, db, "TAKE-LOADER", "TAKE-SCREEN", "TK-W1", "TK-W2")
	l := keyed(sharedLoader("TK-W1", "TK-W2", "TK-W3"), "loader:TK", "TK Loader")
	mustSetLoaders(t, eng, l)

	mustSetLoaders(t, eng, picked(l, "stn-other", "EDGE 2"))
	if got := screenPositions(t, db, sid); len(got) != 0 {
		t.Errorf("screen = %v, want the loader's windows off it", got)
	}
	if n := countRows(t, db, "operator_stations"); n != 1 {
		t.Errorf("%d screens, want the one: the Edge deletes no screen", n)
	}
	core, line := pageLine(t, eng, db, pid)
	if !core {
		t.Fatalf("the process is no longer a loader's: its page would be a line's")
	}
	if line.Added != nil || line.Removed != nil || len(line.Loaders) != 1 ||
		line.Loaders[0].Case != "elsewhere" || line.Loaders[0].Edge != "EDGE 2" || line.Loaders[0].Name != "TK Loader" {
		t.Errorf("page = %+v, want one line: Core moved TK Loader to EDGE 2, nothing added or removed", line)
	}
}

func TestLoaderTake_CorePickingAnotherEdgeAndBackReusesTheScreen(t *testing.T) {
	t.Parallel()
	eng, db, _ := takeEngine(t)
	l := keyed(sharedLoader("TK-W1", "TK-W2"), "loader:TK", "TK Loader")
	mustSetLoaders(t, eng, picked(l, takeSelf, "THIS EDGE"))
	mustSetLoaders(t, eng, picked(l, "stn-other", "EDGE 2"))
	mustSetLoaders(t, eng, picked(l, takeSelf, "THIS EDGE"))
	got := screensOf(t, db)
	if len(got) != 1 || got[0].process != "TK Loader" || !slices.Equal(got[0].positions, []string{"TK-W1", "TK-W2"}) {
		t.Fatalf("screens = %+v, want the one screen made the first time, holding both windows again", got)
	}
	if n := countRows(t, db, "processes"); n != 1 {
		t.Errorf("%d processes, want one", n)
	}
}

func TestLoaderTake_TwoScreensSharingAWindow(t *testing.T) {
	t.Parallel()
	// Two loader screens hold one shared loader's windows (W1 on both).
	t.Run("no Edge picked: an added window is taken by neither", func(t *testing.T) {
		t.Parallel()
		eng, db, _ := takeEngine(t)
		_, a := takeScreen(t, db, "TAKE-A", "SCREEN-A", "TK-W1", "TK-W2")
		_, b := takeScreen(t, db, "TAKE-B", "SCREEN-B", "TK-W1")
		mustSetLoaders(t, eng, keyed(sharedLoader("TK-W1", "TK-W2"), "loader:TK", "TK Loader"))
		mustSetLoaders(t, eng, keyed(sharedLoader("TK-W1", "TK-W2", "TK-W3"), "loader:TK", "TK Loader"))
		if ga, gb := screenPositions(t, db, a), screenPositions(t, db, b); len(ga) != 2 || len(gb) != 1 {
			t.Errorf("screens = %v and %v, want both as they were: two candidates, nothing taken", ga, gb)
		}
	})
	t.Run("this Edge: the screen made for the loader takes it", func(t *testing.T) {
		t.Parallel()
		eng, db, _ := takeEngine(t)
		mustSetLoaders(t, eng, picked(keyed(sharedLoader("TK-W1", "TK-W2"), "loader:TK", "TK Loader"), takeSelf, "THIS EDGE"))
		_, other := takeScreen(t, db, "TAKE-B", "SCREEN-B", "TK-W1")
		mustSetLoaders(t, eng, picked(keyed(sharedLoader("TK-W1", "TK-W2", "TK-W3"), "loader:TK", "TK Loader"), takeSelf, "THIS EDGE"))
		var made []string
		for _, sc := range screensOf(t, db) {
			if sc.link == "loader:TK" {
				made = sc.positions
			}
		}
		if !slices.Equal(made, []string{"TK-W1", "TK-W2", "TK-W3"}) {
			t.Errorf("the loader's screen = %v, want TK-W3 added to it", made)
		}
		if got := screenPositions(t, db, other); !slices.Equal(got, []string{"TK-W1"}) {
			t.Errorf("the other screen = %v, want it as it was", got)
		}
	})
	t.Run("Core removes the shared window: both loader screens drop it", func(t *testing.T) {
		t.Parallel()
		eng, db, _ := takeEngine(t)
		_, a := takeScreen(t, db, "TAKE-A", "SCREEN-A", "TK-W1", "TK-W2")
		_, b := takeScreen(t, db, "TAKE-B", "SCREEN-B", "TK-W1", "TK-W3")
		mustSetLoaders(t, eng, keyed(sharedLoader("TK-W1", "TK-W2", "TK-W3"), "loader:TK", "TK Loader"))
		mustSetLoaders(t, eng, keyed(sharedLoader("TK-W2", "TK-W3"), "loader:TK", "TK Loader"))
		if ga, gb := screenPositions(t, db, a), screenPositions(t, db, b); !slices.Equal(ga, []string{"TK-W2"}) || !slices.Equal(gb, []string{"TK-W3"}) {
			t.Errorf("screens = %v and %v, want TK-W1 off both", ga, gb)
		}
	})
}

func TestLoaderTake_ALoaderMovedBetweenProcessesIsFollowed(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, uid string }{{"no Edge picked", ""}, {"this Edge", takeSelf}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eng, db, _ := takeEngine(t)
			_, a := takeScreen(t, db, "TAKE-A", "SCREEN-A", "TK-W1", "TK-W2")
			mustSetLoaders(t, eng, picked(keyed(sharedLoader("TK-W1", "TK-W2"), "loader:TK", "TK Loader"), c.uid, "THIS EDGE"))
			// A person moves the loader to another process's screen.
			_, b := takeScreen(t, db, "TAKE-B", "SCREEN-B")
			if _, err := eng.stationService.SetNodes(a, nil); err != nil {
				t.Fatalf("empty A: %v", err)
			}
			if _, err := eng.stationService.SetNodes(b, []string{"TK-W1", "TK-W2"}); err != nil {
				t.Fatalf("fill B: %v", err)
			}
			mustSetLoaders(t, eng, picked(keyed(sharedLoader("TK-W1", "TK-W2", "TK-W3"), "loader:TK", "TK Loader"), c.uid, "THIS EDGE"))
			if got := screenPositions(t, db, b); !slices.Equal(got, []string{"TK-W1", "TK-W2", "TK-W3"}) {
				t.Errorf("B = %v, want TK-W3 where the loader's windows are now", got)
			}
			if got := screenPositions(t, db, a); len(got) != 0 {
				t.Errorf("A = %v, want it left empty", got)
			}
		})
	}
}

func TestLoaderTake_ThisEdgeMakesNothingOverAPersonsPositions(t *testing.T) {
	t.Parallel()
	l := picked(keyed(sharedLoader("TK-W1", "TK-W2"), "loader:TK", "TK Loader"), takeSelf, "THIS EDGE")
	t.Run("a window is a position of a press", func(t *testing.T) {
		t.Parallel()
		eng, db, _ := takeEngine(t)
		takeScreen(t, db, "TAKE-PRESS", "PRESS-SCREEN", "TK-PRESS", "TK-W1")
		mustSetLoaders(t, eng, l)
		if n := countRows(t, db, "processes"); n != 1 {
			t.Errorf("%d processes, want only the press: nothing made", n)
		}
		if note := eng.stationService.LoaderTakeNote("loader:TK"); !strings.Contains(note, "TK-W1") || !strings.Contains(note, "TAKE-PRESS") {
			t.Errorf("note = %q, want it to name TK-W1 and TAKE-PRESS", note)
		}
	})
	t.Run("the name is taken", func(t *testing.T) {
		t.Parallel()
		eng, db, _ := takeEngine(t)
		if _, err := db.CreateProcess("TK Loader", "", "", "", false); err != nil {
			t.Fatalf("create process: %v", err)
		}
		mustSetLoaders(t, eng, l)
		if n := countRows(t, db, "operator_stations"); n != 0 {
			t.Errorf("%d screens, want none made", n)
		}
		if note := eng.stationService.LoaderTakeNote("loader:TK"); !strings.Contains(note, `"TK Loader"`) {
			t.Errorf("note = %q, want it to say the name is taken", note)
		}
	})
	t.Run("one process holds only its windows, on no screen: the screen is made there", func(t *testing.T) {
		t.Parallel()
		eng, db, _ := takeEngine(t)
		pid, err := db.CreateProcess("TK Modelled", "", "", "", false)
		if err != nil {
			t.Fatalf("create process: %v", err)
		}
		for i, n := range []string{"TK-W1", "TK-W2"} {
			if _, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: n, Code: strings.ToLower(n),
				Name: n, Sequence: i + 1, Enabled: true}); err != nil {
				t.Fatalf("create %s: %v", n, err)
			}
		}
		mustSetLoaders(t, eng, l)
		got := screensOf(t, db)
		if len(got) != 1 || got[0].process != "TK Modelled" || !slices.Equal(got[0].positions, []string{"TK-W1", "TK-W2"}) {
			t.Errorf("screens = %+v, want one on TK Modelled holding both windows", got)
		}
	})
}

func TestLoaderTake_AnOrderEndingIsTakenOffAtTheNextStart(t *testing.T) {
	t.Parallel()
	eng, db, _ := takeEngine(t)
	_, sid := takeScreen(t, db, "TAKE-LOADER", "TAKE-SCREEN", "TK-W1", "TK-W2")
	mustSetLoaders(t, eng, keyed(sharedLoader("TK-W1", "TK-W2"), "loader:TK", "TK Loader"))
	order := activeOrderAt(t, db, positionID(t, db, sid, "TK-W2"))
	mustSetLoaders(t, eng, keyed(sharedLoader("TK-W1"), "loader:TK", "TK Loader"))
	if got := screenPositions(t, db, sid); !slices.Equal(got, []string{"TK-W1", "TK-W2"}) {
		t.Fatalf("screen = %v, want TK-W2 kept for its order", got)
	}
	endOrder(t, db, order)
	// The engine's start, as Start runs it, with no send.
	facts, ok := eng.cachedLoaderFacts()
	if !ok {
		t.Fatal("the cache could not be read")
	}
	eng.takeLoaders("start", facts, facts, true)
	if got := screenPositions(t, db, sid); !slices.Equal(got, []string{"TK-W1"}) {
		t.Errorf("after the start: screen = %v, want TK-W2 off", got)
	}
}

// THE STATEMENTS PER SEND. An unchanged loader set reads and writes nothing
// of the plant (before round 10 every applied send read every live position
// for the v56 marks); a changed one reads it once to decide, then writes
// through SetNodes. The counts are logged for the report.
func TestLoaderTake_StatementsPerSend(t *testing.T) {
	t.Parallel()
	eng, db, counter := takeEngine(t)
	_, sid := takeScreen(t, db, "TAKE-LOADER", "TAKE-SCREEN", "TK-W1", "TK-W2")
	mustSetLoaders(t, eng, keyed(sharedLoader("TK-W1", "TK-W2"), "loader:TK", "TK Loader"))

	plantStatements := func(stmts []string) (reads, writes int) {
		for _, s := range stmts {
			q := strings.TrimSpace(strings.ToUpper(s))
			if strings.Contains(q, "CORE_LOADER") || strings.Contains(q, "QUARANTINE") || strings.Contains(q, "STYLE_NODE_CLAIMS") {
				continue
			}
			if strings.HasPrefix(q, "SELECT") || strings.HasPrefix(q, "WITH") {
				reads++
			} else {
				writes++
			}
		}
		return reads, writes
	}
	send := func(what string, l protocol.LoaderInfo) (int, int) {
		counter.Reset()
		mustSetLoaders(t, eng, l)
		r, w := plantStatements(counter.Statements())
		t.Logf("%s: %d statements in all; %d read and %d written beyond the loader cache and the claim check", what, counter.Count(), r, w)
		return r, w
	}
	if r, w := send("unchanged", keyed(sharedLoader("TK-W1", "TK-W2"), "loader:TK", "TK Loader")); r != 0 || w != 0 {
		t.Errorf("an unchanged set read %d and wrote %d plant statements, want none", r, w)
	}
	if r, w := send("one window added", keyed(sharedLoader("TK-W1", "TK-W2", "TK-W3"), "loader:TK", "TK Loader")); r == 0 || w == 0 {
		t.Errorf("an added window read %d and wrote %d, want the take's read and SetNodes' writes", r, w)
	}
	if got := screenPositions(t, db, sid); !slices.Equal(got, []string{"TK-W1", "TK-W2", "TK-W3"}) {
		t.Errorf("screen = %v", got)
	}
	send("one window removed", keyed(sharedLoader("TK-W1", "TK-W3"), "loader:TK", "TK Loader"))
	send("the pick set to another Edge", picked(keyed(sharedLoader("TK-W1", "TK-W3"), "loader:TK", "TK Loader"), "stn-other", "EDGE 2"))
	if r, w := send("the same again", picked(keyed(sharedLoader("TK-W1", "TK-W3"), "loader:TK", "TK Loader"), "stn-other", "EDGE 2")); r != 0 || w != 0 {
		t.Errorf("a replayed send read %d and wrote %d plant statements, want none", r, w)
	}
}

// FOUND ON THE DEV STACK (round 10): a shared loader Core sends with no part
// is one this Edge's loader store skips as malformed, so its board would draw
// nothing. Core naming this Edge makes no process or screen for it; the page
// says why.
func TestLoaderTake_ALoaderThisEdgeCannotRunIsNotMade(t *testing.T) {
	t.Parallel()
	eng, db, _ := takeEngine(t)
	l := picked(keyed(sharedLoader("TK-W1", "TK-W2"), "loader:TK", "TK Loader"), takeSelf, "THIS EDGE")
	l.Payloads = nil
	mustSetLoaders(t, eng, l)
	if n := countRows(t, db, "processes"); n != 0 {
		t.Errorf("%d process(es) made for a loader this Edge cannot run", n)
	}
	if note := eng.stationService.LoaderTakeNote("loader:TK"); !strings.Contains(note, "cannot run") {
		t.Errorf("note = %q, want it to say this Edge cannot run the loader as Core sends it", note)
	}
}

// FOUND ON THE DEV STACK (round 10): a window kept on its screen for a running
// order when Core moved the loader away is the loader's again when Core picks
// this Edge back, and goes back on, enabled, with the rest; it is not kept a
// second time.
func TestLoaderTake_AKeptWindowWantedAgainGoesBackOn(t *testing.T) {
	t.Parallel()
	eng, db, _ := takeEngine(t)
	l := keyed(sharedLoader("TK-W1", "TK-W2"), "loader:TK", "TK Loader")
	mustSetLoaders(t, eng, picked(l, takeSelf, "THIS EDGE"))
	var sid int64
	for _, sc := range screensOf(t, db) {
		if sc.link == "loader:TK" {
			sid = mustStation(t, db, sc.screen)
		}
	}
	activeOrderAt(t, db, positionID(t, db, sid, "TK-W2"))
	mustSetLoaders(t, eng, picked(l, "stn-other", "EDGE 2"))
	if got := screenPositions(t, db, sid); !slices.Equal(got, []string{"TK-W2"}) {
		t.Fatalf("after the move: screen = %v, want TK-W2 kept for its order", got)
	}
	mustSetLoaders(t, eng, picked(l, takeSelf, "THIS EDGE"))
	nodes, err := db.ListProcessNodesByStation(sid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var got []string
	for _, n := range nodes {
		if !n.Enabled {
			t.Errorf("%s is still disabled: kept again rather than taken back", n.CoreNodeName)
		}
		got = append(got, n.CoreNodeName)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"TK-W1", "TK-W2"}) {
		t.Errorf("after the pick came back: screen = %v, want both windows", got)
	}
}

// mustStation is the id of the screen named name.
func mustStation(t *testing.T, db *store.DB, name string) int64 {
	t.Helper()
	list, err := db.ListOperatorStations()
	if err != nil {
		t.Fatalf("list screens: %v", err)
	}
	for _, st := range list {
		if st.Name == name {
			return st.ID
		}
	}
	t.Fatalf("no screen %s", name)
	return 0
}

// FOUND ON THE DEV STACK (round 10): a person on Core makes a station, picks
// this Edge, then gives it windows and a part. The screen is made once the
// loader is one this Edge can run, whichever send that is, not only on the
// send that changed the pick.
func TestLoaderTake_ALoaderPickedBeforeItIsCompleteGetsItsScreenWhenItIs(t *testing.T) {
	t.Parallel()
	eng, db, _ := takeEngine(t)
	l := picked(keyed(sharedLoader(), "loader:TK", "TK Loader"), takeSelf, "THIS EDGE")
	mustSetLoaders(t, eng, l) // picked, no windows yet
	l = picked(keyed(sharedLoader("TK-W1", "TK-W2"), "loader:TK", "TK Loader"), takeSelf, "THIS EDGE")
	l.Payloads = nil
	mustSetLoaders(t, eng, l) // windows, no part: this Edge cannot run it yet
	if n := countRows(t, db, "processes"); n != 0 {
		t.Fatalf("%d process(es) made before the loader has a part", n)
	}
	l = picked(keyed(sharedLoader("TK-W1", "TK-W2"), "loader:TK", "TK Loader"), takeSelf, "THIS EDGE")
	mustSetLoaders(t, eng, l) // the part: now it runs
	got := screensOf(t, db)
	if len(got) != 1 || got[0].process != "TK Loader" || !slices.Equal(got[0].positions, []string{"TK-W1", "TK-W2"}) {
		t.Fatalf("screens = %+v, want TK Loader's screen made with both windows once the loader runs", got)
	}

	// Picked, then its windows added in a later send with its part already set.
	eng2, db2, _ := takeEngine(t)
	mustSetLoaders(t, eng2, picked(keyed(sharedLoader(), "loader:TK2", "TK Two"), takeSelf, "THIS EDGE"))
	mustSetLoaders(t, eng2, picked(keyed(sharedLoader("TK-X1"), "loader:TK2", "TK Two"), takeSelf, "THIS EDGE"))
	if got := screensOf(t, db2); len(got) != 1 || !slices.Equal(got[0].positions, []string{"TK-X1"}) {
		t.Errorf("screens = %+v, want TK Two's screen made when its first window came", got)
	}
}
