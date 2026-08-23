// Staged per-seat tooling changeover, end to end.
//
// The round-3 case: a press-index cell whose OUTGOING claim marks which seats
// hold bins that block the tooling change. Marked seats are evacuated to the
// tooling destination; the incoming style's bins travel to the staging node
// and WAIT there; tooling-done is an ordinary production release that moves
// them into the line positions.
//
// Pass conditions:
//  1. every MARKED seat gets its own task and order — one robot per seat,
//     because six orders cannot live on one NodeAction;
//  2. an UNMARKED seat gets nothing — the stated default is that it stays put;
//  3. each seat's steps evacuate to ChangeoverEvacDestination, not to
//     OutboundDestination;
//  4. the incoming bin waits AT the staging node, not on the press apron;
//  5. tooling-done releases them exactly like a production release;
//  6. arming without InboundStaging is refused, naming the cell and the field.
//
//go:build docker

package scenarios

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingo/protocol"
	edgeengine "shingoedge/engine"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
	edgeharness "shingoedge/testharness"
)

func stagedStubCore(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/telemetry/payload/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"bin_type_code": "TOTE-STD"})
	})
	mux.HandleFunc("/api/telemetry/node-bins", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[]"))
	})
	mux.HandleFunc("/api/inventory/preflight", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"missing": []string{}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// stagedSeed builds a 3-position press-index cell on both styles. seats is the
// OUTGOING claim's marked selection; toStaging is the incoming claim's staging
// node (blank exercises the arm refusal).
type stagedSeed struct {
	processID, fromStyleID, toStyleID int64
	nodeIDs                           map[string]int64
	edge                              *edgeharness.Edge
}

func seedStagedPress(t *testing.T, seats []string, toStaging string) stagedSeed {
	t.Helper()
	core := stagedStubCore(t)
	edge := edgeharness.NewEdgeWithCoreAPI(t, "edge.test", core.URL)

	processID, err := edge.DB.CreateProcess("ST-PROC", "staged tooling", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	opStationID, err := edge.DB.CreateOperatorStation(stations.Input{
		ProcessID: processID, Code: "ST-ST", Name: "ST Station", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create station: %v", err)
	}
	nodeIDs := map[string]int64{}
	for i, name := range []string{"PLN-ST-A", "PLN-ST-B", "PLN-ST-C"} {
		id, err := edge.DB.CreateProcessNode(processes.NodeInput{
			ProcessID: processID, OperatorStationID: &opStationID,
			CoreNodeName: name, Code: name, Name: name, Sequence: i + 1, Enabled: true,
		})
		if err != nil {
			t.Fatalf("create node %s: %v", name, err)
		}
		nodeIDs[name] = id
		if _, err := edge.DB.EnsureProcessNodeRuntime(id); err != nil {
			t.Fatalf("ensure runtime %s: %v", name, err)
		}
	}
	fromStyleID, err := edge.DB.CreateStyle("ST-From", "from", processID)
	if err != nil {
		t.Fatalf("create from style: %v", err)
	}
	toStyleID, err := edge.DB.CreateStyle("ST-To", "to", processID)
	if err != nil {
		t.Fatalf("create to style: %v", err)
	}
	if err := edge.DB.SetActiveStyle(processID, &fromStyleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}

	// The OUTGOING claim carries the seat selection and the tooling
	// destination — the outgoing setup put the blocking bins there.
	from := processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: "PLN-ST-A",
		Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobotPressIndex,
		PayloadCode: "ST-OLD", UOPCapacity: 100,
		PairedCoreNode: "PLN-ST-B", SecondPairedCoreNode: "PLN-ST-C",
		InboundSource: "ST-SRC", OutboundDestination: "ST-MARKET",
		ChangeoverEvacSeats:       seats,
		ChangeoverEvacDestination: "ST-TOOLING-BAY",
	}
	to := processes.NodeClaimInput{
		StyleID: toStyleID, CoreNodeName: "PLN-ST-A",
		Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobotPressIndex,
		PayloadCode: "ST-NEW", UOPCapacity: 100,
		PairedCoreNode: "PLN-ST-B", SecondPairedCoreNode: "PLN-ST-C",
		InboundSource: "ST-SRC", OutboundDestination: "ST-MARKET",
		InboundStaging: toStaging,
	}
	for _, c := range []processes.NodeClaimInput{from, to} {
		if _, err := edge.DB.UpsertStyleNodeClaim(c); err != nil {
			t.Fatalf("upsert claim style=%d: %v", c.StyleID, err)
		}
	}
	return stagedSeed{processID: processID, fromStyleID: fromStyleID, toStyleID: toStyleID,
		nodeIDs: nodeIDs, edge: edge}
}

func TestScenario_StagedToolingChangeover_EndToEnd(t *testing.T) {
	// Front and third marked; the BACK seat deliberately unmarked.
	s := seedStagedPress(t, []string{"front", "second"}, "ST-STAGE")

	changeover, err := s.edge.Engine.StartProcessChangeover(s.processID, s.toStyleID, "test", "staged scenario")
	if err != nil {
		t.Fatalf("start staged changeover: %v", err)
	}

	tasks, err := s.edge.DB.ListChangeoverNodeTasks(changeover.ID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	byNode := map[string]processes.NodeTask{}
	for _, task := range tasks {
		byNode[task.NodeName] = task
	}

	// (1) one task per MARKED seat, (2) nothing for the unmarked one.
	for _, marked := range []string{"PLN-ST-A", "PLN-ST-C"} {
		task, ok := byNode[marked]
		if !ok {
			t.Fatalf("marked seat %s has no task; tasks = %+v", marked, byNode)
		}
		if task.Situation != "evacuate" {
			t.Errorf("%s: situation = %q, want evacuate", marked, task.Situation)
		}
		if task.NextMaterialOrderID == nil {
			t.Fatalf("%s: no order created for a marked seat", marked)
		}
	}
	if task, ok := byNode["PLN-ST-B"]; ok && task.Situation == "evacuate" {
		t.Errorf("UNMARKED seat PLN-ST-B got an evacuation; its bins do not block the tool")
	}

	// (3) + (4): the choreography. Evac to the TOOLING destination, and the
	// incoming bin waits AT staging rather than on the press apron.
	for _, marked := range []string{"PLN-ST-A", "PLN-ST-C"} {
		orderID := *byNode[marked].NextMaterialOrderID
		stepsJSON, err := s.edge.DB.GetOrderStepsJSON(orderID)
		if err != nil {
			t.Fatalf("%s: read steps: %v", marked, err)
		}
		var steps []protocol.ComplexOrderStep
		if err := json.Unmarshal([]byte(stepsJSON), &steps); err != nil {
			t.Fatalf("%s: decode steps: %v", marked, err)
		}
		var trace []string
		var sawStagedWait bool
		for _, st := range steps {
			trace = append(trace, string(st.Action)+"@"+st.Node)
			if st.Action == protocol.ActionWait && st.Node == "ST-STAGE" {
				sawStagedWait = true
			}
		}
		joined := strings.Join(trace, " ")
		if !strings.Contains(joined, "dropoff@ST-TOOLING-BAY") {
			t.Errorf("%s: evacuated to the wrong place — ChangeoverEvacDestination must win over "+
				"OutboundDestination.\n  steps: %s", marked, joined)
		}
		if strings.Contains(joined, "dropoff@ST-MARKET") {
			t.Errorf("%s: still routing to OutboundDestination.\n  steps: %s", marked, joined)
		}
		if !sawStagedWait {
			t.Errorf("%s: the incoming bin does not wait at the staging node — a robot parked on "+
				"the press apron for a tooling change blocks the millwrights.\n  steps: %s", marked, joined)
		}
		if !strings.HasPrefix(joined, "pickup@"+marked) {
			t.Errorf("%s: the order must OPEN by lifting the blocking bin.\n  steps: %s", marked, joined)
		}
	}

	// (5) tooling-done is an ordinary production release.
	res, err := s.edge.Engine.ReleaseChangeoverWait(s.processID, edgeengine.ReleaseDisposition{CalledBy: "staged-tooling-test"})
	if err != nil {
		t.Fatalf("tooling-done release: %v", err)
	}
	if res.Released+res.Pending == 0 {
		t.Errorf("tooling-done released nothing; result = %+v", res)
	}
}

// Arming the staged mode without a staging node is refused before any order
// exists — the alternative is a plan whose supply legs have nowhere to go,
// discovered as robots idling mid-changeover.
func TestScenario_StagedToolingChangeover_RefusesWithoutStaging(t *testing.T) {
	s := seedStagedPress(t, []string{"front"}, "")

	_, err := s.edge.Engine.StartProcessChangeover(s.processID, s.toStyleID, "test", "no staging")
	if err == nil {
		t.Fatal("want a refusal: the staged mode has nowhere to stage the incoming bins")
	}
	// NAMED CELL AND NAMED FIELD. "changeover requires inbound staging" sends
	// an engineer to the wrong page on a line with six presses.
	if !strings.Contains(err.Error(), "PLN-ST-A") {
		t.Errorf("refusal must name the cell; got %q", err)
	}
	if !strings.Contains(err.Error(), "Inbound Staging") {
		t.Errorf("refusal must name the missing field; got %q", err)
	}
	// And nothing was started.
	if _, err := s.edge.DB.GetActiveProcessChangeover(s.processID); err == nil {
		t.Error("a refused arm must leave no active changeover")
	}
}
