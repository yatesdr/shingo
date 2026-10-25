package service

import (
	"path/filepath"
	"slices"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// station_serves_test.go pins the served pass (servedRows): which rows a board
// gets, what each row counts, when a row is waiting, and what the pass costs.

// linesideProcess seeds a cell process with an ACTIVE style, one node, and one
// active-style claim of the given role on that node, naming payloads[0] as its
// part. Returns the process, its style and its node.
func linesideProcess(t *testing.T, db *store.DB, procName, coreNode string, role protocol.ClaimRole, payloads []string) (processID, styleID, nodeID int64) {
	t.Helper()
	processID, err := db.CreateProcess(procName, "lineside", "", "", false)
	if err != nil {
		t.Fatalf("create process %s: %v", procName, err)
	}
	styleID, err = db.CreateStyle("STYLE-"+procName, "", processID)
	if err != nil {
		t.Fatalf("create style for %s: %v", procName, err)
	}
	if err := db.SetActiveStyle(processID, &styleID); err != nil {
		t.Fatalf("set active style for %s: %v", procName, err)
	}
	nodeID = cellNode(t, db, processID, coreNode)
	cellClaimOn(t, db, styleID, coreNode, role, payloads)
	return processID, styleID, nodeID
}

// cellNode creates a process node with its runtime row.
func cellNode(t *testing.T, db *store.DB, processID int64, coreNode string) int64 {
	t.Helper()
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: coreNode, Code: coreNode, Name: coreNode, Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node %s: %v", coreNode, err)
	}
	if _, err := db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime for %s: %v", coreNode, err)
	}
	return nodeID
}

// cellClaimOn writes a cell's claim through the allowlisted door: the cells a
// loader serves are ordinary claims, never loader claims.
func cellClaimOn(t *testing.T, db *store.DB, styleID int64, coreNode string, role protocol.ClaimRole, payloads []string) int64 {
	t.Helper()
	id, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: coreNode, Role: role,
		SwapMode: protocol.SwapModeSequential, OutboundDestination: "MARKET",
		PayloadCode: payloads[0], AllowedPayloadCodes: payloads, UOPCapacity: 100,
	})
	if err != nil {
		t.Fatalf("create %s claim on %s: %v", role, coreNode, err)
	}
	return id
}

// incomingStyleOn gives a process a target style with one claim, as a
// changeover does (processes.target_style_id).
func incomingStyleOn(t *testing.T, db *store.DB, processID int64, name, coreNode string, role protocol.ClaimRole, payloads []string) {
	t.Helper()
	styleID, err := db.CreateStyle(name, "", processID)
	if err != nil {
		t.Fatalf("create style %s: %v", name, err)
	}
	cellClaimOn(t, db, styleID, coreNode, role, payloads)
	if _, err := db.Exec(`UPDATE processes SET target_style_id=? WHERE id=?`, styleID, processID); err != nil {
		t.Fatalf("set target style: %v", err)
	}
}

// board is a station's tiles as the pass reads them: one loader tile per role
// given, serving the parts.
func board(roles []domain.LoaderRole, parts ...string) []store.StationNodeView {
	ps := make([]domain.TileLoaderPart, 0, len(parts))
	for _, p := range parts {
		ps = append(ps, domain.TileLoaderPart{Code: p})
	}
	var out []store.StationNodeView
	for _, r := range roles {
		out = append(out, store.StationNodeView{Loader: &domain.TileLoader{Role: string(r), Parts: ps}})
	}
	return out
}

var loaderBoard = []domain.LoaderRole{domain.RoleProduce}

// have finds the count of the row for a cell's part, failing when there is not
// exactly one.
func have(t *testing.T, rows []domain.ServedRow, cell, part string) int {
	t.Helper()
	var found []domain.ServedRow
	for _, r := range rows {
		if r.Cell == cell && r.Part == part {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("rows for %s/%s = %+v, want exactly one", cell, part, found)
	}
	if found[0].HaveUOP == nil {
		t.Fatalf("row %s/%s has no count", cell, part)
	}
	return *found[0].HaveUOP
}

// Each served cell is its own row, summed nowhere on the server: the board
// adds a part's rows. A bin is credited to the part it holds; with no identity
// established, to the running claim's part (Engine.binAtNode's rule).
//
// Before (the lineside sum): CELL-B's bin was credited whole to both its
// parts, so PART-Z read 7. Now it reads 0: the bin holds PART-X, and a cell
// changing over must not read the outgoing bin's count as the incoming part's.
func TestServedRows_SumsAcrossProcesses(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, _, nodeA := linesideProcess(t, db, "CELL-A", "ALN_001", protocol.ClaimRoleConsume, []string{"PART-X"})
	_, _, nodeB := linesideProcess(t, db, "CELL-B", "ALN_002", protocol.ClaimRoleConsume, []string{"PART-X", "PART-Z"})
	if err := db.SetProcessNodeRuntime(nodeA, nil, 25); err != nil {
		t.Fatalf("set runtime A: %v", err)
	}
	if err := db.SetProcessNodeRuntime(nodeB, nil, 7); err != nil {
		t.Fatalf("set runtime B: %v", err)
	}

	rows := NewStationService(db).servedRows(board(loaderBoard, "PART-X", "PART-Z"))

	if got := have(t, rows, "CELL-A", "PART-X"); got != 25 {
		t.Errorf("CELL-A PART-X = %d, want 25", got)
	}
	if got := have(t, rows, "CELL-B", "PART-X"); got != 7 {
		t.Errorf("CELL-B PART-X = %d, want 7 (its bin holds its running part)", got)
	}
	if got := have(t, rows, "CELL-B", "PART-Z"); got != 0 {
		t.Errorf("CELL-B PART-Z = %d, want 0: the bin holds PART-X", got)
	}
}

// The bin and the piles of the row's part are summed.
func TestServedRows_SumsRuntimeAndBuckets(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, _, nodeA := linesideProcess(t, db, "CELL-A", "ALN_001", protocol.ClaimRoleConsume, []string{"PART-X"})
	if err := db.SetProcessNodeRuntime(nodeA, nil, 25); err != nil {
		t.Fatalf("set runtime: %v", err)
	}
	if _, err := db.CaptureLinesideBucket(nodeA, "PART-X", 15); err != nil {
		t.Fatalf("capture bucket: %v", err)
	}

	rows := NewStationService(db).servedRows(board(loaderBoard, "PART-X"))

	if got := have(t, rows, "CELL-A", "PART-X"); got != 40 {
		t.Errorf("PART-X = %d, want 40 (25 bin + 15 pile)", got)
	}
}

// A claim allowing several parts credits the bin to the part it holds and each
// pile to its own part. The established identity wins over the claim.
func TestServedRows_CreditsByPayload(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, _, nodeA := linesideProcess(t, db, "CELL-A", "ALN_001", protocol.ClaimRoleConsume, []string{"PART-X", "PART-Y"})
	if err := db.SetProcessNodeRuntime(nodeA, nil, 40); err != nil {
		t.Fatalf("set runtime: %v", err)
	}
	if _, err := db.CaptureLinesideBucket(nodeA, "PART-Y", 5); err != nil {
		t.Fatalf("capture Y: %v", err)
	}
	if _, err := db.CaptureLinesideBucket(nodeA, "PART-X", 3); err != nil {
		t.Fatalf("capture X: %v", err)
	}
	svc := NewStationService(db)
	tiles := board(loaderBoard, "PART-X", "PART-Y")

	rows := svc.servedRows(tiles)
	if x, y := have(t, rows, "CELL-A", "PART-X"), have(t, rows, "CELL-A", "PART-Y"); x != 43 || y != 5 {
		t.Errorf("no identity: X=%d Y=%d, want 43 (bin by the running claim's part + its pile) and 5 (its pile)", x, y)
	}

	if err := processes.SetRuntimeLinesidePayload(db.DB, nodeA, "PART-Y", true, "test"); err != nil {
		t.Fatalf("set identity: %v", err)
	}
	rows = svc.servedRows(tiles)
	if x, y := have(t, rows, "CELL-A", "PART-X"), have(t, rows, "CELL-A", "PART-Y"); x != 3 || y != 45 {
		t.Errorf("bin known to hold PART-Y: X=%d Y=%d, want 3 and 45", x, y)
	}

	// A carrier known to be empty credits nobody.
	if err := processes.SetRuntimeLinesidePayload(db.DB, nodeA, "", true, "test"); err != nil {
		t.Fatalf("set identity: %v", err)
	}
	rows = svc.servedRows(tiles)
	if x, y := have(t, rows, "CELL-A", "PART-X"), have(t, rows, "CELL-A", "PART-Y"); x != 3 || y != 5 {
		t.Errorf("bin known empty: X=%d Y=%d, want the piles alone, 3 and 5", x, y)
	}
}

// No loader tile, no pass, no statement, and no key: absent, never [].
func TestServedRows_NoLoaderTileReadsNothing(t *testing.T) {
	t.Parallel()
	db, counter := countingDB(t)
	linesideProcess(t, db, "CELL-A", "ALN_001", protocol.ClaimRoleConsume, []string{"PART-X"})
	counter.Reset()
	if rows := NewStationService(db).servedRows([]store.StationNodeView{{}}); rows != nil {
		t.Errorf("rows = %+v, want nil (absent): the board has no loader tile", rows)
	}
	if n := counter.Count(); n != 0 {
		t.Errorf("a board with no loader tile issued %d statements, want 0", n)
	}
	// Read, nothing served: [], not absent.
	if rows := NewStationService(db).servedRows(board(loaderBoard, "PART-NONE")); rows == nil || len(rows) != 0 {
		t.Errorf("rows = %#v, want an empty, non-nil list", rows)
	}
}

// Five statements, whatever the process count (the claims with their processes,
// the nodes, the runtimes, the piles, the queued orders); six while a served
// cell has an incoming style (Core's sourcing verdicts).
func TestServedRows_StatementBudget(t *testing.T) {
	t.Parallel()
	db, counter := countingDB(t)
	for _, c := range []string{"CELL-A", "CELL-B", "CELL-C", "CELL-D", "CELL-E", "CELL-F"} {
		linesideProcess(t, db, c, "LN_"+c, protocol.ClaimRoleConsume, []string{"PART-X"})
	}
	svc := NewStationService(db)
	tiles := board(loaderBoard, "PART-X", "PART-Y")

	counter.Reset()
	if rows := svc.servedRows(tiles); len(rows) != 6 {
		t.Fatalf("rows = %d, want 6", len(rows))
	}
	if n := counter.Count(); n != 5 {
		t.Errorf("the pass issued %d statements at six cells, want 5", n)
	}

	procID, _, _ := linesideProcess(t, db, "CELL-G", "LN_CELL-G", protocol.ClaimRoleConsume, []string{"PART-X"})
	incomingStyleOn(t, db, procID, "CELL-G-NEXT", "LN_CELL-G", protocol.ClaimRoleConsume, []string{"PART-Y"})
	counter.Reset()
	rows := svc.servedRows(tiles)
	if !slices.ContainsFunc(rows, func(r domain.ServedRow) bool { return r.Incoming && r.Part == "PART-Y" }) {
		t.Fatalf("no incoming row for CELL-G's next style: %+v", rows)
	}
	if n := counter.Count(); n != 6 {
		t.Errorf("the pass issued %d statements while a served cell changes over, want 6", n)
	}
}

// A loader fills cells that consume, an unloader empties cells that produce;
// a station with both gets both.
func TestServedRows_RowsOfTheBoardsRole(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	linesideProcess(t, db, "CELL-USE", "ULN_1", protocol.ClaimRoleConsume, []string{"PART-X"})
	linesideProcess(t, db, "CELL-MAKE", "PLN_1", protocol.ClaimRoleProduce, []string{"PART-X"})
	svc := NewStationService(db)
	cells := func(rows []domain.ServedRow) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r.Cell+"/"+r.Role)
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		roles []domain.LoaderRole
		want  []string
	}{
		{"loader", []domain.LoaderRole{domain.RoleProduce}, []string{"CELL-USE/consume"}},
		{"unloader", []domain.LoaderRole{domain.RoleConsume}, []string{"CELL-MAKE/produce"}},
		{"both", []domain.LoaderRole{domain.RoleProduce, domain.RoleConsume}, []string{"CELL-MAKE/produce", "CELL-USE/consume"}},
	} {
		if got := cells(svc.servedRows(board(tc.roles, "PART-X"))); !slices.Equal(got, tc.want) {
			t.Errorf("%s: rows %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The loader predicate is applied in Go, once: a stored manual_swap claim is
// not a row, nor is any claim at a node Core's loader names; the ordinary
// claim beside them is. A SQL filter that re-spelled the predicate (by swap
// mode alone) would let the claim at the loader's window through.
func TestServedRows_LoaderClaimsAreNotRows(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	procID, styleID, _ := linesideProcess(t, db, "CELL-A", "WLN_1", protocol.ClaimRoleConsume, []string{"PART-X"})
	// A stored loader claim in the same style, beside the cell's.
	stored := cellClaimOn(t, db, styleID, "SMN_OLD", protocol.ClaimRoleConsume, []string{"PART-X"})
	if _, err := db.Exec(`UPDATE style_node_claims SET swap_mode='manual_swap' WHERE id=?`, stored); err != nil {
		t.Fatalf("make loader claim: %v", err)
	}
	// An ordinary claim at a node Core's loader names.
	cellClaimOn(t, db, styleID, "SMN_W1", protocol.ClaimRoleConsume, []string{"PART-X"})
	cellNode(t, db, procID, "SMN_OLD")
	cellNode(t, db, procID, "SMN_W1")
	loader, err := domain.NewSharedWindowLoader("loader:9", domain.RoleProduce, domain.ReplenishmentOperator,
		[]domain.Window{{Node: "SMN_W1"}}, []domain.PayloadCode{"PART-X"})
	if err != nil {
		t.Fatalf("loader: %v", err)
	}
	svc := NewStationService(db)
	svc.SetLoaderResolver(stubLoaderResolver{loader: loader})

	rows := svc.servedRows(board(loaderBoard, "PART-X"))
	var nodes []string
	for _, r := range rows {
		nodes = append(nodes, r.Node)
	}
	if !slices.Equal(nodes, []string{"WLN_1"}) {
		t.Errorf("row nodes = %v, want only the cell's WLN_1", nodes)
	}
}

// Waiting: every row whose cell has an order for the part queued with Core's
// reason (read on every pass: the cell has asked); an incoming consume row also
// when Core lists its part missing for the incoming style (read only while a
// row is incoming). A running style's verdict is not read, and a produce row
// never reads Missing.
func TestServedRows_Waiting(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	useProc, _, _ := linesideProcess(t, db, "CELL-USE", "ULN_1", protocol.ClaimRoleConsume, []string{"PART-X"})
	_, _, askNode := linesideProcess(t, db, "CELL-ASK", "ULN_2", protocol.ClaimRoleConsume, []string{"PART-A"})
	_, _, makeNode := linesideProcess(t, db, "CELL-MAKE", "PLN_1", protocol.ClaimRoleProduce, []string{"PART-M"})
	_, _, quietNode := linesideProcess(t, db, "CELL-QUIET", "PLN_2", protocol.ClaimRoleProduce, []string{"PART-Q"})
	svc := NewStationService(db)
	tiles := board([]domain.LoaderRole{domain.RoleProduce, domain.RoleConsume}, "PART-A", "PART-X", "PART-Y", "PART-M", "PART-Q")

	queue := func(uuid string, node int64, part, reason string) {
		id, err := db.CreateOrder(uuid, protocol.OrderTypeRetrieve, &node, false, 1, "X", "", "", false, part, "", "")
		if err != nil {
			t.Fatalf("create order: %v", err)
		}
		if _, err := db.Exec(`UPDATE orders SET status='queued', queue_reason=? WHERE id=?`, reason, id); err != nil {
			t.Fatalf("queue order: %v", err)
		}
	}
	queue("o-ask", askNode, "PART-A", "no full in the market")
	queue("o-make", makeNode, "PART-M", "no empty in the market")
	queue("o-quiet", quietNode, "PART-Q", "")
	if err := db.ReplaceSourcingState([]protocol.SourcingState{
		{ProcessID: "CELL-USE", StyleID: "STYLE-CELL-USE", Status: "red", Missing: []string{"PART-X"}},
		{ProcessID: "CELL-MAKE", StyleID: "STYLE-CELL-MAKE", Status: "red", Missing: []string{"PART-M"}},
		{ProcessID: "CELL-QUIET", StyleID: "STYLE-CELL-QUIET", Status: "red", Missing: []string{"PART-Q"}},
		{ProcessID: "CELL-USE", StyleID: "CELL-USE-NEXT", Status: "red", Missing: []string{"PART-Y"}},
	}); err != nil {
		t.Fatalf("sourcing state: %v", err)
	}
	check := func(phase string, want map[string]bool) {
		t.Helper()
		got := map[string]bool{}
		for _, r := range svc.servedRows(tiles) {
			got[r.Cell+"/"+r.Part] = r.Waiting
		}
		for k, w := range want {
			if got[k] != w {
				t.Errorf("%s: %s waiting = %v, want %v", phase, k, got[k], w)
			}
		}
	}

	// No served cell changes over.
	check("steady", map[string]bool{
		"CELL-ASK/PART-A":   true,  // running, its cell has asked: the order is read on every pass
		"CELL-USE/PART-X":   false, // running: its style's Missing is not read
		"CELL-MAKE/PART-M":  true,  // produce: queued with Core's reason
		"CELL-QUIET/PART-Q": false, // produce: Missing is not read, its order has no reason
	})

	incomingStyleOn(t, db, useProc, "CELL-USE-NEXT", "ULN_1", protocol.ClaimRoleConsume, []string{"PART-Y"})
	check("changing over", map[string]bool{
		"CELL-ASK/PART-A":   true,
		"CELL-USE/PART-X":   false, // still the running style: its Missing is not read
		"CELL-USE/PART-Y":   true,  // incoming, Core lists it missing
		"CELL-MAKE/PART-M":  true,
		"CELL-QUIET/PART-Q": false,
	})
}

// A failed read is absent, never []: a board that could not look must not read
// as one that looked and found nothing served.
func TestServedRows_FailsOpenToAbsent(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	linesideProcess(t, db, "CELL-A", "ALN_001", protocol.ClaimRoleConsume, []string{"PART-X"})
	if _, err := db.Exec(`DROP TABLE node_lineside_bucket`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if rows := NewStationService(db).servedRows(board(loaderBoard, "PART-X")); rows != nil {
		t.Errorf("rows = %+v, want nil when a read fails", rows)
	}
}

func TestCarriersFor(t *testing.T) {
	t.Parallel()
	catalog := []protocol.PayloadBinTypeInfo{
		{PayloadCode: "PART-A", BinTypeCode: "TOTE-1"},
		{PayloadCode: "PART-B", BinTypeCode: "TOTE-2"},
		{PayloadCode: "PART-C", BinTypeCode: "TOTE-1"},
	}
	for _, tc := range []struct {
		parts []string
		want  []string
	}{
		{[]string{"PART-B", "PART-A"}, []string{"TOTE-1", "TOTE-2"}}, // catalog order, each once
		{[]string{"PART-C"}, []string{"TOTE-1"}},
		{[]string{"PART-Z"}, []string{"TOTE-1", "TOTE-2"}}, // nothing matches: every type
		{nil, []string{"TOTE-1", "TOTE-2"}},
	} {
		if got := carriersFor(catalog, tc.parts); !slices.Equal(got, tc.want) {
			t.Errorf("carriersFor(%v) = %v, want %v", tc.parts, got, tc.want)
		}
	}
	if got := carriersFor(nil, []string{"PART-A"}); got != nil {
		t.Errorf("no catalog: %v, want none", got)
	}
}

func countingDB(t *testing.T) (*store.DB, *store.QueryCounter) {
	t.Helper()
	db, counter, err := store.OpenCounting(filepath.Join(t.TempDir(), "serves-count.db"))
	if err != nil {
		t.Fatalf("open counting store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, counter
}
