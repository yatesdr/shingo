package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// Quality containment's engine verbs, against a stubbed Core (the same
// httptest pattern core_client_error_test.go pins) and a real SQLite store:
//
//	SendBinToQualityHold   — the station's hold: the bin at a produce node
//	                       with a containment route walks into it, the hold
//	                       marker lands, and the double-tap guard refuses a
//	                       second tap while a move is in flight.
//	ReleaseFromContainment — Verify Good: the bin must still be standing at
//	                       the containment node, the claim's outbound is the
//	                       release target, and an ambiguous claim set is
//	                       refused rather than guessed.
//
// The Core stub answers /api/telemetry/node-bins (the occupancy read the
// verbs gate on) and /api/telemetry/bin-quality-hold (recorded, asserted).

type holdStubCore struct {
	mu        sync.Mutex
	bins      []NodeBinInfo
	holds     []int64
	unholds   []int64
	nodesSeen []string
}

func (s *holdStubCore) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/telemetry/node-bins"):
			s.nodesSeen = append(s.nodesSeen, r.URL.Query().Get("nodes"))
			_ = json.NewEncoder(w).Encode(s.bins)
		case r.URL.Path == "/api/telemetry/bin-quality-hold":
			var body struct {
				BinID int64 `json:"bin_id"`
				Hold  bool  `json:"hold"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Hold {
				s.holds = append(s.holds, body.BinID)
			} else {
				s.unholds = append(s.unholds, body.BinID)
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
}

func (s *holdStubCore) setBin(b NodeBinInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bins = []NodeBinInfo{b}
}

func (s *holdStubCore) holdCalls() (holds, unholds []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.holds...), append([]int64(nil), s.unholds...)
}

// seedHoldNode seeds a process + active style + one produce node whose claim
// declares a containment route, the shape the station's hold button needs.
func seedHoldNode(t *testing.T, db interface {
	CreateProcess(string, string, string, string, string, bool) (int64, error)
	CreateStyle(string, string, int64) (int64, error)
	SetActiveStyle(int64, *int64) error
	CreateProcessNode(processes.NodeInput) (int64, error)
	UpsertStyleNodeClaim(processes.NodeClaimInput) (int64, error)
	EnsureProcessNodeRuntime(int64) (*processes.RuntimeState, error)
}, coreNode string) int64 {
	t.Helper()
	processID, err := db.CreateProcess("HOLD-PROC", "quality hold", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: coreNode, Code: "HN1", Name: "Hold Node", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, err := db.CreateStyle("HOLD-STYLE", "", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	var active = styleID
	if err := db.SetActiveStyle(processID, &active); err != nil {
		t.Fatalf("set active: %v", err)
	}
	_, err = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: coreNode, Role: "produce",
		SwapMode: "two_robot_press_index", PayloadCode: "PART-HOLD", UOPCapacity: 30,
		PairedCoreNode: coreNode + "-B", OutboundDestination: "FG-1",
		ContainmentDestination: "CONT-1",
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	if _, err := db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("runtime: %v", err)
	}
	return nodeID
}

func TestSendBinToQualityHold_HoldsThePresentBin(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	stub := &holdStubCore{}
	srv := stub.server()
	t.Cleanup(srv.Close)
	eng.coreClient = NewCoreClient(srv.URL)

	nodeID := seedHoldNode(t, eng.db, "PLN-HOLD")
	stub.setBin(NodeBinInfo{NodeName: "PLN-HOLD", BinID: 42, PayloadCode: "PART-HOLD", Occupied: true})

	order, err := eng.SendBinToQualityHold(nodeID, "station-3")
	if err != nil {
		t.Fatalf("SendBinToQualityHold: %v", err)
	}
	if order.SourceNode != "PLN-HOLD" || order.DeliveryNode != "CONT-1" {
		t.Errorf("order = %s → %s, want PLN-HOLD → CONT-1", order.SourceNode, order.DeliveryNode)
	}
	if order.PayloadCode != "PART-HOLD" {
		t.Errorf("payload = %q, want PART-HOLD carried", order.PayloadCode)
	}
	holds, _ := stub.holdCalls()
	if len(holds) != 1 || holds[0] != 42 {
		t.Errorf("hold markers = %v, want [42]", holds)
	}
}

func TestSendBinToQualityHold_RefusesWithoutRouteOrBin(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	stub := &holdStubCore{}
	srv := stub.server()
	t.Cleanup(srv.Close)
	eng.coreClient = NewCoreClient(srv.URL)

	nodeID := seedHoldNode(t, eng.db, "PLN-HOLD2")

	// No bin at the node: nothing to hold.
	stub.setBin(NodeBinInfo{NodeName: "PLN-HOLD2", Occupied: false})
	if _, err := eng.SendBinToQualityHold(nodeID, "s"); err == nil {
		t.Error("hold with no bin must be refused")
	}

	// A bin, but the claim has no containment route (reseed without one).
	processID, err := eng.db.CreateProcess("HOLD-PROC-NOROUTE", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	node2, err := eng.db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "PLN-NOROUTE", Code: "HN2", Name: "No Route", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, err := eng.db.CreateStyle("HOLD-STYLE-NOROUTE", "", processID)
	testutil.MustNoErr(t, err, "create style")
	var active = styleID
	_ = eng.db.SetActiveStyle(processID, &active)
	_, err = eng.db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "PLN-NOROUTE", Role: "produce",
		SwapMode: "two_robot_press_index", PayloadCode: "PART-X", UOPCapacity: 10,
		PairedCoreNode: "PLN-NOROUTE-B", OutboundDestination: "FG-9",
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	_, err = eng.db.EnsureProcessNodeRuntime(node2)
	testutil.MustNoErr(t, err, "ensure runtime")
	stub.setBin(NodeBinInfo{NodeName: "PLN-NOROUTE", BinID: 7, PayloadCode: "PART-X", Occupied: true})
	_, err = eng.SendBinToQualityHold(node2, "s")
	if err == nil || !strings.Contains(err.Error(), "no containment destination") {
		t.Errorf("err = %v, want the configure-the-claim refusal", err)
	}
}

func TestReleaseFromContainment_AmbiguityAndHappyPath(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	stub := &holdStubCore{}
	srv := stub.server()
	t.Cleanup(srv.Close)
	eng.coreClient = NewCoreClient(srv.URL)

	nodeID := seedHoldNode(t, eng.db, "PLN-REL")
	// The containment node is a registered process node too (the floor
	// registers it; the release move is keyed on its process-node id).
	parent, err := eng.db.GetProcessNode(nodeID)
	if err != nil {
		t.Fatalf("read seeded node: %v", err)
	}
	contID, err := eng.db.CreateProcessNode(processes.NodeInput{
		ProcessID: parent.ProcessID, CoreNodeName: "CONT-1", Code: "CN1", Name: "Containment", Sequence: 2, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create containment node: %v", err)
	}
	_, err = eng.db.EnsureProcessNodeRuntime(contID)
	testutil.MustNoErr(t, err, "ensure runtime")

	// Happy path: the bin is standing at the containment node.
	stub.setBin(NodeBinInfo{NodeName: "CONT-1", BinID: 42, PayloadCode: "PART-HOLD", Occupied: true})
	order, err := eng.ReleaseFromContainment("CONT-1", 42, "quality")
	if err != nil {
		t.Fatalf("ReleaseFromContainment: %v", err)
	}
	if order.SourceNode != "CONT-1" || order.DeliveryNode != "FG-1" {
		t.Errorf("order = %s → %s, want CONT-1 → FG-1", order.SourceNode, order.DeliveryNode)
	}
	_, unholds := stub.holdCalls()
	if len(unholds) != 1 || unholds[0] != 42 {
		t.Errorf("unhold calls = %v, want [42]", unholds)
	}

	// Stale screen: the bin is gone from the node — refused, not walked.
	stub.setBin(NodeBinInfo{NodeName: "CONT-1", Occupied: false})
	if _, err := eng.ReleaseFromContainment("CONT-1", 42, "quality"); err == nil ||
		!strings.Contains(err.Error(), "not at") {
		t.Errorf("err = %v, want the stale-screen refusal", err)
	}

	// A SHARED hold group is legitimate: a second producer routes its
	// contained bins to the same spot with a DIFFERENT outbound, and the
	// BIN'S PAYLOAD disambiguates the release. The PART-HOLD bin still
	// releases to FG-1 (its own producer's outbound) despite the second
	// claim covering the same node.
	otherProc, err := eng.db.CreateProcess("HOLD-PROC-B", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create second process")
	otherStyle, err := eng.db.CreateStyle("HOLD-STYLE-B", "", otherProc)
	testutil.MustNoErr(t, err, "create second style")
	_, err = eng.db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: otherStyle, CoreNodeName: "PLN-OTHER", Role: "produce",
		SwapMode: "two_robot_press_index", PayloadCode: "PART-OTHER", UOPCapacity: 5,
		PairedCoreNode: "PLN-OTHER-B", OutboundDestination: "FG-2",
		ContainmentDestination: "CONT-1",
	})
	if err != nil {
		t.Fatalf("upsert second producer's claim: %v", err)
	}
	stub.setBin(NodeBinInfo{NodeName: "CONT-1", BinID: 42, PayloadCode: "PART-HOLD", Occupied: true})
	order2, err := eng.ReleaseFromContainment("CONT-1", 42, "quality")
	if err != nil {
		t.Fatalf("release through a shared group: %v", err)
	}
	if order2.DeliveryNode != "FG-1" {
		t.Errorf("shared-group release went to %s, want FG-1 (the bin's payload picks the outbound)", order2.DeliveryNode)
	}

	// The other payload's bin releases to the OTHER producer's outbound.
	stub.setBin(NodeBinInfo{NodeName: "CONT-1", BinID: 99, PayloadCode: "PART-OTHER", Occupied: true})
	order3, err := eng.ReleaseFromContainment("CONT-1", 99, "quality")
	if err != nil {
		t.Fatalf("release the second payload: %v", err)
	}
	if order3.DeliveryNode != "FG-2" {
		t.Errorf("PART-OTHER release went to %s, want FG-2", order3.DeliveryNode)
	}
}
