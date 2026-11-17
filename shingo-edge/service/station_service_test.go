package service

import (
	"errors"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// TestStation_SetNodes covers the four-branch contract of
// StationService.SetNodes: create-on-first-set, update-existing,
// dedupe+trim input, and disable-vs-delete based on active orders.
//
// Phase 6.4a moved the orchestration body in from
// store/station_nodes.go's deleted (db *DB).SetStationNodes; this
// test was previously TestOperatorStations_SetStationNodes in
// store/store_coverage_test.go and exercised the *store.DB shim
// directly. Now it exercises the service.

func TestStation_SetNodes(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewStationService(db)

	pid, _ := db.CreateProcess("P", "", "", "", false)
	id, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "S"})

	// Initial set.
	testutil.MustNoErr(t, setNodesErr(svc, id, []string{"N1", "N2"}), "set 1")
	names, err := svc.GetNodeNames(id)
	if err != nil {
		t.Fatalf("get names: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("names len = %d", len(names))
	}

	// Verify runtime rows got created for each node.
	nodes, _ := db.ListProcessNodesByStation(id)
	if len(nodes) != 2 {
		t.Fatalf("nodes len = %d", len(nodes))
	}
	for _, n := range nodes {
		rt, err := db.GetProcessNodeRuntime(n.ID)
		if err != nil {
			t.Errorf("runtime missing for node %d: %v", n.ID, err)
		}
		if rt == nil {
			t.Errorf("runtime nil for node %d", n.ID)
		}
	}

	// Update: remove N1, add N3. N1 has no active orders, so it's deleted.
	testutil.MustNoErr(t, setNodesErr(svc, id, []string{"N2", "N3"}), "set 2")
	nodes2, _ := db.ListProcessNodesByStation(id)
	if len(nodes2) != 2 {
		t.Fatalf("nodes len after update = %d", len(nodes2))
	}
	nameSet := map[string]bool{}
	for _, n := range nodes2 {
		nameSet[n.CoreNodeName] = true
	}
	if !nameSet["N2"] || !nameSet["N3"] || nameSet["N1"] {
		t.Errorf("after update: %v", nameSet)
	}

	// Input with duplicates + whitespace — dedupe + trim paths.
	testutil.MustNoErr(t, setNodesErr(svc, id, []string{" N2 ", "N2", "", "N4"}), "set 3")
	nodes3, _ := db.ListProcessNodesByStation(id)
	if len(nodes3) != 2 { // N2, N4 — N3 removed
		t.Errorf("nodes after dedup = %d, want 2", len(nodes3))
	}
}

func TestStation_SetNodesDisablesRatherThanDeletesWhenOrdersActive(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewStationService(db)

	pid, _ := db.CreateProcess("P", "", "", "", false)
	id, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "S"})

	setNodesErr(svc, id, []string{"N-KEEP"})
	nodes, _ := db.ListProcessNodesByStation(id)
	var nodeID int64
	for _, n := range nodes {
		if n.CoreNodeName == "N-KEEP" {
			nodeID = n.ID
		}
	}
	// Attach an active order to the node we're about to drop.
	_, err := db.CreateOrder("keep-me", "retrieve", &nodeID, false, 1, "", "", "", protocol.Height{}, false, "", "", "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}

	// Drop N-KEEP. Node should be disabled, not deleted.
	testutil.MustNoErr(t, setNodesErr(svc, id, []string{"N-NEW"}), "set")
	n, err := db.GetProcessNode(nodeID)
	if err != nil {
		t.Fatalf("get node: %v (should still exist)", err)
	}
	if n.Enabled {
		t.Error("expected disabled=true for node with active orders")
	}
}

// A position the save could only disable stays on the screen, so the save
// must say which ones: reporting plain success for a list that did not land is
// the engineer being told something untrue.
func TestStation_SetNodesNamesThePositionsItCouldOnlyDisable(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewStationService(db)

	pid, err := db.CreateProcess("P", "", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	id, err := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "S"})
	testutil.MustNoErr(t, err, "create station")

	testutil.MustNoErr(t, setNodesErr(svc, id, []string{"N-BUSY", "N-IDLE"}), "seed")
	byName := map[string]int64{}
	nodes, err := db.ListProcessNodesByStation(id)
	testutil.MustNoErr(t, err, "list station nodes")
	for _, n := range nodes {
		byName[n.CoreNodeName] = n.ID
	}
	busyID := byName["N-BUSY"]
	if _, err := db.CreateOrder("busy-order", "retrieve", &busyID, false, 1, "", "", "", protocol.Height{}, false, "", "", ""); err != nil {
		t.Fatalf("create order: %v", err)
	}

	kept, err := svc.SetNodes(id, nil)
	if err != nil {
		t.Fatalf("SetNodes: %v", err)
	}
	if len(kept) != 1 || kept[0] != "N-BUSY" {
		t.Errorf("kept = %v, want [N-BUSY]", kept)
	}
	left, err := db.ListProcessNodesByStation(id)
	testutil.MustNoErr(t, err, "list station nodes after save")
	if len(left) != 1 || left[0].CoreNodeName != "N-BUSY" {
		names := make([]string, 0, len(left))
		for _, n := range left {
			names = append(names, n.CoreNodeName)
		}
		t.Errorf("station still holds %v, want only N-BUSY (N-IDLE has no active order and retires)", names)
	}
}

// TestStation_SetNodes_AdoptsOrphanInsteadOfDuplicating pins the fix for the
// duplicate-process_nodes bug (HK 2026-07-14: PLN_01 → three rows).
//
// SetNodes used to decide "reuse or create?" from the STATION-local node set, so
// a Core node that existed under the process but was not currently on THIS
// station was invisible to it and got minted afresh. The node's runtime state
// (and its share of every PLC tick) then split across the copies. Reuse must be
// decided process-globally: the existing row is adopted, never re-created.
func TestStation_SetNodes_AdoptsOrphanInsteadOfDuplicating(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewStationService(db)

	pid, _ := db.CreateProcess("P400", "", "", "", false)
	stationA, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "A"})

	// Station A claims PLN_01, then drops it. The row survives, orphaned — this is
	// the state that used to be invisible to the next rebind.
	testutil.MustNoErr(t, setNodesErr(svc, stationA, []string{"PLN_01"}), "claim PLN_01")
	nodes, _ := db.ListProcessNodesByStation(stationA)
	if len(nodes) != 1 {
		t.Fatalf("setup: nodes = %d, want 1", len(nodes))
	}
	origID := nodes[0].ID

	// Keep the node alive through the un-claim by giving it an active order, so it
	// is disabled rather than deleted — the real-world orphan shape.
	if _, err := db.CreateOrder("uuid-adopt", "complex", &origID, false, 1, "PLN_01", "", "", protocol.Height{}, false, "", "", ""); err != nil {
		t.Fatalf("seed active order: %v", err)
	}
	testutil.MustNoErr(t, setNodesErr(svc, stationA, []string{}), "un-claim PLN_01")

	// A second station now claims the same Core node. Pre-fix this minted a
	// duplicate row (code pln-01-2) with its own runtime; post-fix it adopts.
	stationB, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "B"})
	testutil.MustNoErr(t, setNodesErr(svc, stationB, []string{"PLN_01"}), "station B claims PLN_01")

	all, err := db.ListProcessNodesByProcess(pid)
	if err != nil {
		t.Fatalf("list by process: %v", err)
	}
	var pln int
	for _, n := range all {
		if n.CoreNodeName == "PLN_01" {
			pln++
		}
	}
	if pln != 1 {
		t.Fatalf("PLN_01 process_nodes rows = %d, want 1 — the orphan was duplicated instead of adopted", pln)
	}

	// And it must be the SAME row, re-pointed — not a fresh id carrying no history.
	bNodes, _ := db.ListProcessNodesByStation(stationB)
	if len(bNodes) != 1 {
		t.Fatalf("station B nodes = %d, want 1", len(bNodes))
	}
	if bNodes[0].ID != origID {
		t.Errorf("station B node id = %d, want %d (the existing row must be adopted, not recreated)", bNodes[0].ID, origID)
	}
	if !bNodes[0].Enabled {
		t.Errorf("adopted node should be re-enabled")
	}
}

// TestStation_SetNodes_LeavesRoutingRowsUntouched pins the boundary the
// routing set was put in its own table for. SetNodes re-points, re-sequences,
// disables and deletes process_nodes rows with no role concept; a routing
// row that lived on process_nodes would die on an unrelated board edit. So
// the routing set survives every SetNodes pass byte-for-byte — EXCEPT the
// pass that makes one of its names a station position, which drops that one
// row (a name in both halves is offered twice). That exception is new on
// 2026-10-18 (pick-a-part unit A): it used to be "a cleanup the engineer
// makes, never a side effect", and the boot-time backfill made it anyway for
// every gated-off process on every boot. The backfill is gone; the drop moved
// here, where the collision is born.
func TestStation_SetNodes_LeavesRoutingRowsUntouched(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewStationService(db)

	pid, err := db.CreateProcess("P", "", "", "", false)
	testutil.MustNoErr(t, err, "db.CreateProcess")
	stID, err := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "S"})
	testutil.MustNoErr(t, err, "db.CreateOperatorStation")
	for _, in := range []domain.RoutingNodeInput{
		{ProcessID: pid, CoreNodeName: "SMN_BUF_100", Role: domain.RoutingRoleSource, Sequence: 1, Enabled: true, Origin: domain.RoutingOriginEngineer, CalledBy: "eng"},
		{ProcessID: pid, CoreNodeName: "STG_01", Role: domain.RoutingRoleStaging, Sequence: 2, Enabled: false, Origin: domain.RoutingOriginBackfill},
		{ProcessID: pid, CoreNodeName: "Supermarket Area", Role: domain.RoutingRoleDestination, Sequence: 3, Enabled: true, Origin: domain.RoutingOriginEngineer, CalledBy: "eng"},
	} {
		if _, err := db.UpsertRoutingNode(domain.CoreNodeKinds{}, in); err != nil {
			t.Fatalf("seed routing row %s: %v", in.CoreNodeName, err)
		}
	}
	before, err := db.ListRoutingNodes(pid)
	if err != nil {
		t.Fatalf("list routing before: %v", err)
	}

	testutil.MustNoErr(t, setNodesErr(svc, stID, []string{"N1", "N2"}), "set 1")
	testutil.MustNoErr(t, setNodesErr(svc, stID, []string{"N2"}), "set 2 (deletes N1)")
	// A routing name becoming a station position: SetNodes drops exactly that
	// routing row.
	testutil.MustNoErr(t, setNodesErr(svc, stID, []string{"N2", "STG_01"}), "set 3 (adds a routing name)")
	testutil.MustNoErr(t, setNodesErr(svc, stID, nil), "set 4 (clears the station)")

	after, err := db.ListRoutingNodes(pid)
	if err != nil {
		t.Fatalf("list routing after: %v", err)
	}
	var kept []domain.RoutingNode
	for _, r := range before {
		if r.CoreNodeName != "STG_01" {
			kept = append(kept, r)
		}
	}
	before = kept
	if len(after) != len(before) {
		t.Fatalf("routing rows: %d expected after SetNodes (STG_01 dropped), %d after", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("routing row %d changed under SetNodes:\n before %+v\n after  %+v", i, before[i], after[i])
		}
	}
}

// ── Positions on no screen ──────────────────────────────────────────
//
// A process's positions are process_nodes rows, and a row's screen is
// nullable: a press is set up before anybody hangs a screen on it, and its
// positions exist from the setup. SetLoosePositions is SetNodes for the rows
// on no screen — the same wholesale replace, the same refusals, the same
// removal pass — over the set whose operator_station_id IS NULL.

// loosePositions is the process's rows on no screen, by name.
func loosePositions(t *testing.T, svc *StationService, processID int64) map[string]processes.Node {
	t.Helper()
	nodes, err := svc.db.ListProcessNodesByProcess(processID)
	testutil.MustNoErr(t, err, "ListProcessNodesByProcess")
	out := map[string]processes.Node{}
	for _, n := range nodes {
		if n.OperatorStationID == nil {
			out[n.CoreNodeName] = n
		}
	}
	return out
}

// TestStation_SetLoosePositions_CreatesRetiresAndKeeps walks the four
// branches SetNodes has, on a process with no screen at all: a first save
// creates the rows on no screen with their runtime rows; a re-save of the
// same list changes nothing; a name left out retires; a name left out that
// still has an active order is disabled and named in kept, because the save
// did not do what it was asked.
func TestStation_SetLoosePositions_CreatesRetiresAndKeeps(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewStationService(db)
	pid, err := db.CreateProcess("Loose P", "", "", "", false)
	testutil.MustNoErr(t, err, "create process")

	kept, err := svc.SetLoosePositions(pid, []string{"L-ONE", " L-TWO ", "L-ONE", "", "L-BUSY"})
	testutil.MustNoErr(t, err, "first save")
	if len(kept) != 0 {
		t.Errorf("a first save kept %v; it removed nothing", kept)
	}
	first := loosePositions(t, svc, pid)
	if len(first) != 3 {
		t.Fatalf("rows on no screen = %v, want L-ONE, L-TWO and L-BUSY once each", first)
	}
	for name, seq := range map[string]int{"L-ONE": 1, "L-TWO": 2, "L-BUSY": 3} {
		n, ok := first[name]
		if !ok {
			t.Fatalf("%s was not created", name)
		}
		if n.Sequence != seq || !n.Enabled {
			t.Errorf("%s: sequence %d enabled %v, want %d and enabled", name, n.Sequence, n.Enabled, seq)
		}
		if rt, err := db.GetProcessNodeRuntime(n.ID); err != nil || rt == nil {
			t.Errorf("%s has no runtime row (%v): the board and the counter read it", name, err)
		}
	}

	// A re-save of the same list is the same rows: no new id, no new row.
	_, err = svc.SetLoosePositions(pid, []string{"L-ONE", "L-TWO", "L-BUSY"})
	testutil.MustNoErr(t, err, "re-save")
	again := loosePositions(t, svc, pid)
	if len(again) != 3 {
		t.Fatalf("a re-save left %d rows, want 3", len(again))
	}
	for name, n := range first {
		if again[name].ID != n.ID || again[name].Sequence != n.Sequence || !again[name].Enabled {
			t.Errorf("a re-save moved %s: %+v -> %+v", name, n, again[name])
		}
	}

	// L-BUSY has an order running at it; L-ONE has none. Both are left out.
	busyID := first["L-BUSY"].ID
	if _, err := db.CreateOrder("loose-busy", "retrieve", &busyID, false, 1, "", "", "", protocol.Height{}, false, "", "", ""); err != nil {
		t.Fatalf("create order: %v", err)
	}
	kept, err = svc.SetLoosePositions(pid, []string{"L-TWO"})
	testutil.MustNoErr(t, err, "remove two")
	if len(kept) != 1 || kept[0] != "L-BUSY" {
		t.Errorf("kept = %v, want [L-BUSY]: the save could only disable it", kept)
	}
	after := loosePositions(t, svc, pid)
	if _, ok := after["L-ONE"]; ok {
		t.Error("L-ONE is still a position; nothing ran at it, so it retires")
	}
	if b, ok := after["L-BUSY"]; !ok || b.Enabled {
		t.Errorf("L-BUSY = %+v (present %v), want kept and disabled", b, ok)
	}
	if _, ok := after["L-TWO"]; !ok {
		t.Error("L-TWO was on the list and is gone")
	}
}

// TestStation_SetLoosePositions_RefusesANameOnAScreen: the loose write and a
// screen's write meet at a name, and they do not meet symmetrically. A screen
// that claims a loose position ADOPTS it — that is how a screen hung on the
// press later takes the positions set up before it, and it is SetNodes'
// behaviour unchanged. The loose write naming a position on a screen would be
// the reverse move, a position silently taken off a screen the floor works
// from, so it is refused by name, the whole save, before any row moves.
func TestStation_SetLoosePositions_RefusesANameOnAScreen(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewStationService(db)
	pid, err := db.CreateProcess("Loose Q", "", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	screen, err := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "Front Screen"})
	testutil.MustNoErr(t, err, "create station")
	testutil.MustNoErr(t, setNodesErr(svc, screen, []string{"S-ONE"}), "screen claims S-ONE")
	_, err = svc.SetLoosePositions(pid, []string{"L-ONE"})
	testutil.MustNoErr(t, err, "seed a loose position")
	before, err := db.ListProcessNodesByProcess(pid)
	testutil.MustNoErr(t, err, "list before")

	_, err = svc.SetLoosePositions(pid, []string{"L-ONE", "S-ONE", "L-NEW"})
	if !errors.Is(err, ErrPositionOnAScreen) {
		t.Fatalf("err = %v, want ErrPositionOnAScreen", err)
	}
	if want := "S-ONE is on Front Screen; take it off there first"; !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal reads %q, want it to say %q", err, want)
	}
	after, err := db.ListProcessNodesByProcess(pid)
	testutil.MustNoErr(t, err, "list after")
	if len(after) != len(before) {
		t.Fatalf("a refused save changed the rows: %d -> %d", len(before), len(after))
	}
	for i := range before {
		b, a := before[i], after[i]
		if a.ID != b.ID || a.CoreNodeName != b.CoreNodeName || (a.OperatorStationID == nil) != (b.OperatorStationID == nil) || a.Sequence != b.Sequence {
			t.Errorf("a refused save moved %s: %+v -> %+v", b.CoreNodeName, b, a)
		}
	}

	// The other direction is an adoption: the screen claims the loose
	// position, and it is the same row, now on the screen.
	loose := loosePositions(t, svc, pid)["L-ONE"]
	testutil.MustNoErr(t, setNodesErr(svc, screen, []string{"S-ONE", "L-ONE"}), "screen adopts L-ONE")
	adopted, err := db.ListProcessNodesByProcess(pid)
	testutil.MustNoErr(t, err, "list adopted")
	if len(adopted) != len(before) {
		t.Errorf("the screen's claim made %d rows from %d; it adopts, it does not mint", len(adopted), len(before))
	}
	got, err := db.GetProcessNode(loose.ID)
	testutil.MustNoErr(t, err, "reread L-ONE")
	if got.OperatorStationID == nil || *got.OperatorStationID != screen {
		t.Errorf("L-ONE's screen = %v, want %d", got.OperatorStationID, screen)
	}
	if left := loosePositions(t, svc, pid); len(left) != 0 {
		t.Errorf("positions left on no screen: %v", left)
	}
}

// TestStation_SetLoosePositions_RefusesWholeLikeAScreen: the loose write is
// SetNodes' replace, so its two refusals are SetNodes' — a name Core does not
// have and a live position of another process — and each is total.
func TestStation_SetLoosePositions_RefusesWholeLikeAScreen(t *testing.T) {
	t.Parallel()
	svc, _, pidB := crossProcessFixture(t)
	pidA, err := svc.db.CreateProcess("Loose R", "", "", "", false)
	testutil.MustNoErr(t, err, "create process")

	_, err = svc.SetLoosePositions(pidA, []string{"L-ONE", "FGN_001"})
	if !errors.Is(err, ErrPositionOfOtherProcess) || !strings.Contains(err.Error(), "Press B") {
		t.Errorf("err = %v, want ErrPositionOfOtherProcess naming Press B (process %d)", err, pidB)
	}
	svc.SetCoreNodeResolver(func() map[string]bool { return map[string]bool{"L-ONE": true} })
	_, err = svc.SetLoosePositions(pidA, []string{"L-ONE", "NOT-ON-CORE"})
	if !errors.Is(err, ErrUnknownCoreNodes) || !strings.Contains(err.Error(), "NOT-ON-CORE") {
		t.Errorf("err = %v, want ErrUnknownCoreNodes naming NOT-ON-CORE", err)
	}
	if left := loosePositions(t, svc, pidA); len(left) != 0 {
		t.Errorf("a refused save wrote %v", left)
	}
}

// setNodesErr is SetNodes for the tests that only care whether the save went
// through.
func setNodesErr(svc *StationService, stationID int64, names []string) error {
	_, err := svc.SetNodes(stationID, names)
	return err
}
