// V2 sim gate — same-bin-type press-index changeover, end to end.
//
// The case Fix A exists for: a press-index changeover where from- and to-style
// share a bin type NEVER fans out (binTypesDiffer false → the per-position
// diff pass skips), so the paired seat owns no task, no order, and — before
// participants — appeared nowhere and was gated by nothing. The index motion
// then places a bin on a seat that unrelated dispatch could also fill: the
// two-bins-on-one-node family, and the reason HK operators fork-trucked seats.
//
// Pass conditions (SYNTH-plan §2 mapping, V2):
//  1. indexed_over participant rows exist for the paired seat;
//  2. intake is REFUSED at the paired seat for the whole changeover window;
//  3. an unrelated node on the same process still accepts orders — the
//     Springfield non-regression;
//  4. the seat renders as a CHILD tile (child_of_node set) — the mechanism by
//     which the board suppresses its release button;
//  5. cutover completes, and the seat's gate REOPENS after it.
//
//go:build docker

package scenarios

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"shingoedge/domain"
	"shingoedge/service"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
	edgeharness "shingoedge/testharness"

	edgeorders "shingoedge/orders"
)

// stubCore serves the two telemetry endpoints a press-index changeover start
// touches: the payload manifest (bin-type lookup — SAME type for every payload,
// which is what makes this the same-bin-type scenario) and node-bins (empty:
// FetchNodeBins callers treat missing nodes as occupied, which keeps the full
// swap choreography rather than the empty-station downgrade).
func stubCore(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/telemetry/payload/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"bin_type_code": "TOTE-STD"})
	})
	mux.HandleFunc("/api/telemetry/node-bins", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[]"))
	})
	// Changeover-start preflight: fully stocked ("missing" empty). The start
	// path HARD-fails on a preflight transport error (unlike the advisory it
	// returns on missing stock), so the endpoint must exist.
	mux.HandleFunc("/api/inventory/preflight", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"missing": []string{}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestScenario_V2_SameBinTypePressIndex_EndToEnd(t *testing.T) {
	const stationID = "edge.test"
	core := stubCore(t)
	edge := edgeharness.NewEdgeWithCoreAPI(t, stationID, core.URL)

	// ── Seed: press (stationed) + paired seat (row, NO station) + bystander ──
	processID, err := edge.DB.CreateProcess("V2-PROC", "same-bin-type press-index", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	opStationID, err := edge.DB.CreateOperatorStation(stations.Input{
		ProcessID: processID, Code: "V2-ST", Name: "V2 Station", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create station: %v", err)
	}
	pressID, err := edge.DB.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, OperatorStationID: &opStationID,
		CoreNodeName: "PLN-V2-A", Code: "V2A", Name: "V2 Press", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create press node: %v", err)
	}
	// The paired seat: a process_nodes row exists (the HK shape after
	// ensurePressIndexBackNode) but carries NO station of its own.
	seatID, err := edge.DB.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "PLN-V2-B", Code: "V2B", Name: "V2 Press Seat", Sequence: 2, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create seat node: %v", err)
	}
	// The bystander: same process, not in the changeover — the loader class
	// the Springfield field report is about.
	bystanderID, err := edge.DB.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, OperatorStationID: &opStationID,
		CoreNodeName: "SMN-V2-L", Code: "V2L", Name: "V2 Loader", Sequence: 3, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create bystander node: %v", err)
	}

	fromStyleID, err := edge.DB.CreateStyle("V2-From", "from", processID)
	if err != nil {
		t.Fatalf("create from style: %v", err)
	}
	toStyleID, err := edge.DB.CreateStyle("V2-To", "to", processID)
	if err != nil {
		t.Fatalf("create to style: %v", err)
	}
	if err := edge.DB.SetActiveStyle(processID, &fromStyleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}

	// Press-index claims on the press, both styles, DIFFERENT payloads that the
	// stub Core reports as the SAME bin type — the no-fan-out case.
	for _, c := range []processes.NodeClaimInput{
		{
			StyleID: fromStyleID, CoreNodeName: "PLN-V2-A",
			Role: "consume", SwapMode: "two_robot_press_index",
			PayloadCode: "V2-OLD-PART", UOPCapacity: 100,
			PairedCoreNode:      "PLN-V2-B",
			InboundSource:       "V2-SRC",
			OutboundDestination: "V2-DST",
		},
		{
			StyleID: toStyleID, CoreNodeName: "PLN-V2-A",
			Role: "consume", SwapMode: "two_robot_press_index",
			PayloadCode: "V2-NEW-PART", UOPCapacity: 100,
			PairedCoreNode:      "PLN-V2-B",
			InboundSource:       "V2-SRC",
			OutboundDestination: "V2-DST",
		},
	} {
		if _, err := edge.DB.UpsertStyleNodeClaim(c); err != nil {
			t.Fatalf("upsert claim style=%d: %v", c.StyleID, err)
		}
	}
	for _, id := range []int64{pressID, seatID, bystanderID} {
		if _, err := edge.DB.EnsureProcessNodeRuntime(id); err != nil {
			t.Fatalf("ensure runtime %d: %v", id, err)
		}
	}

	// ── Start ──
	changeover, err := edge.Engine.StartProcessChangeover(processID, toStyleID, "test", "V2 scenario")
	if err != nil {
		t.Fatalf("start press-index changeover: %v", err)
	}

	// (1) indexed_over rows exist, seat owned by the press's task.
	parts, err := edge.DB.ListChangeoverParticipants(changeover.ID)
	if err != nil {
		t.Fatalf("list participants: %v", err)
	}
	var seatPart *domain.Participant
	for i := range parts {
		if parts[i].CoreNodeName == "PLN-V2-B" {
			seatPart = &parts[i]
		}
	}
	if seatPart == nil {
		t.Fatalf("paired seat has no participant row; participants = %+v", parts)
	}
	if seatPart.Role != domain.ParticipantRoleIndexedOver {
		t.Errorf("seat role = %q, want indexed_over (same-bin-type never fans out, so it must not be task-role)", seatPart.Role)
	}
	if seatPart.OwningTaskID == nil {
		t.Error("seat participant has no owning task — the child-tile render and station resolution both hang off it")
	}

	// (2) intake refused at the seat, with the indexed-over reason.
	if ok, reason := edge.Engine.CanAcceptOrders(seatID); ok {
		t.Error("paired seat accepts orders mid-changeover — the two-bins-on-one-node window is open")
	} else if reason == "" {
		t.Error("seat refusal carries no reason")
	}

	// (3) Springfield non-regression: the bystander keeps working.
	if ok, reason := edge.Engine.CanAcceptOrders(bystanderID); !ok {
		t.Errorf("unrelated node refused intake (%q) during a changeover it is not part of", reason)
	}

	// (4) the seat renders as a CHILD tile on the press's station.
	view, err := service.NewStationService(edge.DB).BuildView(opStationID)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	var seatView *domain.StationNodeView
	for i := range view.Nodes {
		if view.Nodes[i].Node.ID == seatID {
			seatView = &view.Nodes[i]
		}
	}
	if seatView == nil {
		t.Fatal("seat absent from the press's station view — the pre-fix invisibility")
	} else {
		if seatView.ChildOfNode == "" {
			t.Error("seat is not marked child_of_node; the board cannot suppress its release button")
		}
		if seatView.ChangeoverTask != nil {
			t.Error("seat owns a task in the same-bin-type case; it must not")
		}
	}

	// ── Drive the press's work terminal, then cut over ──
	tasks, err := edge.DB.ListChangeoverNodeTasks(changeover.ID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("no tasks created")
	}
	for _, task := range tasks {
		for _, oid := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
			if oid == nil {
				continue
			}
			if err := edge.DB.UpdateOrderStatus(*oid, string(edgeorders.StatusConfirmed)); err != nil {
				t.Fatalf("confirm order %d: %v", *oid, err)
			}
		}
		if err := edge.DB.UpdateChangeoverNodeTaskState(task.ID, domain.NodeTaskSwitched); err != nil {
			t.Fatalf("switch task %d: %v", task.ID, err)
		}
	}

	// (5) cutover completes…
	if err := edge.Engine.CompleteProcessProductionCutover(processID); err != nil {
		t.Fatalf("cutover blocked: %v", err)
	}
	// GetActiveProcessChangeover excludes completed/cancelled rows, so the
	// completed cutover reads back as "no active changeover".
	if still, _ := edge.DB.GetActiveProcessChangeover(processID); still != nil {
		t.Errorf("changeover %d still active after cutover (state %q)", still.ID, still.State)
	}

	// …and the seat's gate REOPENS — the block is the changeover window, not a
	// permanent property of the node.
	if ok, reason := edge.Engine.CanAcceptOrders(seatID); !ok {
		t.Errorf("seat still refuses intake after cutover (%q); the gate must release with the window", reason)
	}
}
