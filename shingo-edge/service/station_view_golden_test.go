package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// station_view_golden_test.go — a whole-payload pin for BuildView.
//
// BuildView assembles ~15 fields across five sequential sections, and before
// this test the coverage was per-field and thin: SupplyRefusedForMe,
// SupplyRefusals, LinesideActive/Inactive, LastReleaseError, SwapReady,
// OperatorDriven and HomeLocationLoader had no assertion through BuildView at
// all. Several of them have unit tests one layer down — ComputeSwapReady and
// activePayloadLineside are both tested directly — but nothing checked that
// BuildView wires the result onto the right tile, and the WIRING is what a
// restructuring of a 189-statement function breaks.
//
// A golden file is the right shape for that: it fails on any field that moves,
// including ones nobody thought to assert, which is exactly the failure mode a
// hand-written assertion set cannot cover.
//
// Regenerate with:  go test ./service/ -run TestBuildView_Golden -update
//
// ALWAYS read the diff before accepting a regeneration. A golden that is updated
// reflexively pins nothing; the point is that an unexplained field change stops
// somebody.

var updateGolden = os.Getenv("UPDATE_GOLDEN") != "" ||
	func() bool {
		for _, a := range os.Args {
			if a == "-update" || a == "--update" {
				return true
			}
		}
		return false
	}()

// Timestamps are wall-clock and would make the golden fail every run. Scrub
// them rather than freezing the clock: BuildView reads `time.Now()` through
// several layers it does not own, so scrubbing is the honest boundary.
var rfc3339 = regexp.MustCompile(`"(\d{4}-\d{2}-\d{2}T[^"]*)"`)

// goldenScenario is deliberately awkward. Each element is here because it
// exercises a field the per-field tests miss:
//
//   - a press node whose runtime carries a STAGED order with a rolled-back
//     release error -> LastReleaseError, which is read per tile
//   - a stationless press-index position adopted onto this station through its
//     owning task -> the child-tile wiring, on the shape that used to render
//     nowhere
//   - an active changeover -> the header fields and the task map
//   - a consume node with no runtime at all -> the nil-runtime arms of every
//     enrichment, which is what most tiles on a real board actually look like
func goldenScenario(t *testing.T) (db *store.DB, stationID int64) {
	t.Helper()
	db = testdb.Open(t)

	processID, err := db.CreateProcess("GOLD-PROC", "golden", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	stationID, err = db.CreateOperatorStation(stations.Input{
		ProcessID: processID, Code: "GOLD-ST", Name: "Golden Station", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create station: %v", err)
	}

	pressNodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, OperatorStationID: &stationID,
		CoreNodeName: "PLN_G1", Code: "PLNG1", Name: "Press G1", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create press node: %v", err)
	}
	// No station: adopted onto this board via its owning task, the press-index
	// extension-position shape.
	positionNodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "PLN_G2", Code: "PLNG2", Name: "Press G2 Position", Sequence: 2, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create position node: %v", err)
	}
	if _, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, OperatorStationID: &stationID,
		CoreNodeName: "WLN_G3", Code: "WLNG3", Name: "Weld G3", Sequence: 3, Enabled: true,
	}); err != nil {
		t.Fatalf("create consume node: %v", err)
	}

	// The release-error path: a staged order whose last history row is the
	// rolled-back manifest sync. LookupLastReleaseError reads the most recent
	// non-empty detail, so the ordinary transition below it must NOT win.
	orderID, err := db.CreateOrder("gold-order-1", protocol.OrderTypeRetrieve, &pressNodeID,
		false, 1, "PLN_G1", "", "SMN_01", "standard", false, "PAY-1")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.InsertOrderHistory(orderID, "pending", "staged", "ordinary transition"); err != nil {
		t.Fatalf("insert history: %v", err)
	}
	if err := db.InsertOrderHistory(orderID, "staged", "staged",
		"Manifest sync failed at Core: bin 42 is claimed by another order"); err != nil {
		t.Fatalf("insert release-error history: %v", err)
	}
	// EnsureProcessNodeRuntime first: UpdateProcessNodeRuntimeOrders is a bare
	// UPDATE ... WHERE process_node_id=?, and runtime rows are created lazily on
	// read -- so without the row it succeeds having changed nothing, and the
	// fixture would silently not set up the case it exists to test.
	if _, err := db.EnsureProcessNodeRuntime(pressNodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	if err := db.UpdateProcessNodeRuntimeOrders(pressNodeID, nil, &orderID); err != nil {
		t.Fatalf("set runtime staged order: %v", err)
	}
	rt, err := db.GetProcessNodeRuntime(pressNodeID)
	if err != nil {
		t.Fatalf("reload runtime: %v", err)
	}
	if rt == nil || rt.StagedOrderID == nil || *rt.StagedOrderID != orderID {
		t.Fatalf("fixture did not attach the staged order to the press runtime: %+v", rt)
	}

	res, err := db.Exec(`INSERT INTO process_changeovers (process_id, to_style_id, state, called_by)
		VALUES (?, 1, 'active', 'golden')`, processID)
	if err != nil {
		t.Fatalf("insert changeover: %v", err)
	}
	changeoverID, _ := res.LastInsertId()

	tres, err := db.Exec(`INSERT INTO changeover_node_tasks
		(process_changeover_id, process_node_id, situation, state)
		VALUES (?, ?, 'swap', 'swap_required')`, changeoverID, pressNodeID)
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
	taskID, _ := tres.LastInsertId()

	for _, p := range []struct {
		name  string
		node  int64
		role  string
		owner *int64
	}{
		{"PLN_G1", pressNodeID, domain.ParticipantRoleTask, &taskID},
		{"PLN_G2", positionNodeID, domain.ParticipantRoleIndexedOver, &taskID},
	} {
		if _, err := db.Exec(`INSERT INTO changeover_participants
			(process_changeover_id, core_node_name, process_node_id, role, owning_task_id)
			VALUES (?, ?, ?, ?, ?)`, changeoverID, p.name, p.node, p.role, p.owner); err != nil {
			t.Fatalf("insert participant %s: %v", p.name, err)
		}
	}
	return db, stationID
}

func TestBuildView_Golden(t *testing.T) {
	db, stationID := goldenScenario(t)
	svc := NewStationService(db)

	view, err := svc.BuildView(context.Background(), stationID)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}

	got, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	got = rfc3339.ReplaceAll(got, []byte(`"<ts>"`))
	got = append(got, '\n')

	path := filepath.Join("testdata", "station_view_golden.json")
	if updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("golden regenerated at %s — READ THE DIFF before committing", path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("BuildView payload changed.\n\nIf the change is intended, regenerate with\n"+
			"  UPDATE_GOLDEN=1 go test ./service/ -run TestBuildView_Golden\n"+
			"and read the diff.\n\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// The golden pins the whole payload, which makes it a poor place to learn WHY a
// field matters. This names the one that had no coverage at all and is the
// reason a per-tile query runs on every board poll.
func TestBuildView_StagedReleaseErrorReachesItsTile(t *testing.T) {
	db, stationID := goldenScenario(t)
	svc := NewStationService(db)

	view, err := svc.BuildView(context.Background(), stationID)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}

	var press *domain.StationNodeView
	for i := range view.Nodes {
		if view.Nodes[i].Node.CoreNodeName == "PLN_G1" {
			press = &view.Nodes[i]
		}
	}
	if press == nil {
		t.Fatal("press tile PLN_G1 is not on the board")
	}
	const want = "Manifest sync failed at Core: bin 42 is claimed by another order"
	if press.LastReleaseError != want {
		t.Errorf("LastReleaseError = %q, want %q", press.LastReleaseError, want)
	}

	// The error rides the STAGED order, and only the tile that owns that order
	// may show it — a batched lookup that mixed orders up would light every tile.
	for i := range view.Nodes {
		n := view.Nodes[i]
		if n.Node.CoreNodeName != "PLN_G1" && n.LastReleaseError != "" {
			t.Errorf("tile %s carries LastReleaseError %q; it owns no such order",
				n.Node.CoreNodeName, n.LastReleaseError)
		}
	}
}
