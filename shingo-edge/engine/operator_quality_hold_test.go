package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// Quality containment's engine verbs, against a stubbed Core (the same
// httptest pattern core_client_error_test.go pins) and a real SQLite store:
//
//	SendBinToQualityHold   — the station's hold: the bin at a produce node
//	                       with a containment route walks into it by a move
//	                       naming the bin and asking for its hold, and the
//	                       double-tap guard refuses a
//	                       second tap while a move is in flight.
//	ReleaseFromContainment — Verify Good: the bin must still be standing at
//	                       the containment node, the claim's outbound is the
//	                       release target, and an ambiguous claim set is
//	                       refused rather than guessed.
//
// The Core stub answers /api/telemetry/bin-quality-hold (recorded, asserted).
// What stands at a node is the bins table's (holdActableBins), and at a
// containment node the containment copy's (holdActableContainment).

type holdStubCore struct {
	mu      sync.Mutex
	holds   []int64
	unholds []int64
}

func (s *holdStubCore) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
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

func (s *holdStubCore) holdCalls() (holds, unholds []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.holds...), append([]int64(nil), s.unholds...)
}

// seedHoldNode seeds a process + active style + one produce node whose claim
// declares a containment route, the shape the station's hold button needs.
func seedHoldNode(t *testing.T, db interface {
	CreateProcess(string, string, string, string, bool) (int64, error)
	CreateStyle(string, string, int64) (int64, error)
	SetActiveStyle(int64, *int64) error
	CreateProcessNode(processes.NodeInput) (int64, error)
	UpsertStyleNodeClaim(domain.CoreNodeKinds, processes.NodeClaimInput) (int64, error)
	EnsureProcessNodeRuntime(int64) (*processes.RuntimeState, error)
}, coreNode string) int64 {
	t.Helper()
	processID, err := db.CreateProcess("HOLD-PROC", "quality hold", "", "", false)
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
	_, err = db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
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
	eng.coreClient = stubCoreClient(srv.URL)

	nodeID := seedHoldNode(t, eng.db, "PLN-HOLD")
	holdActableBins(t, eng, protocol.NodeBinsRow{Node: "PLN-HOLD", BinID: 42, PayloadCode: "PART-HOLD", Occupied: true})

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
	// The hold is one Core act: the move names bin 42 and asks for its hold,
	// and no separate marker call is made (it was one, best-effort, before).
	if holds, _ := stub.holdCalls(); len(holds) != 0 {
		t.Errorf("hold marker calls = %v, want none: the move carries the hold", holds)
	}
	moves := emptyOutMoves(t, eng, "PLN-HOLD")
	if len(moves) != 1 || moves[0].BinID != 42 || moves[0].QualityHoldBy != "station-3" {
		t.Errorf("hold moves = %+v, want one naming bin 42 held by station-3", moves)
	}
}

func TestSendBinToQualityHold_RefusesWithoutRouteOrBin(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	stub := &holdStubCore{}
	srv := stub.server()
	t.Cleanup(srv.Close)
	eng.coreClient = stubCoreClient(srv.URL)

	nodeID := seedHoldNode(t, eng.db, "PLN-HOLD2")

	// Nothing held in the bins table: refused with the not-current sentence
	// (site 8 of the reads), and nothing is moved.
	if _, err := eng.SendBinToQualityHold(nodeID, "s"); err == nil ||
		err.Error() != "Core's picture of PLN-HOLD2 is not current — try again" {
		t.Errorf("hold with no table: err = %v, want the not-current sentence", err)
	}

	// No bin at the node: nothing to hold.
	holdActableBins(t, eng, protocol.NodeBinsRow{Node: "PLN-HOLD2"},
		protocol.NodeBinsRow{Node: "PLN-NOROUTE", BinID: 7, PayloadCode: "PART-X", Occupied: true})
	if _, err := eng.SendBinToQualityHold(nodeID, "s"); err == nil || !strings.Contains(err.Error(), "no bin at node") {
		t.Errorf("hold with no bin: err = %v, want the no-bin refusal", err)
	}
	if moves := emptyOutMoves(t, eng, "PLN-HOLD2"); len(moves) != 0 {
		t.Errorf("refused holds created moves: %+v", moves)
	}

	// A bin, but the claim has no containment route (reseed without one).
	processID, err := eng.db.CreateProcess("HOLD-PROC-NOROUTE", "", "", "", false)
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
	_, err = eng.db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "PLN-NOROUTE", Role: "produce",
		SwapMode: "two_robot_press_index", PayloadCode: "PART-X", UOPCapacity: 10,
		PairedCoreNode: "PLN-NOROUTE-B", OutboundDestination: "FG-9",
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	_, err = eng.db.EnsureProcessNodeRuntime(node2)
	testutil.MustNoErr(t, err, "ensure runtime")
	// (the table holds a bin at PLN-NOROUTE, above)
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
	eng.coreClient = stubCoreClient(srv.URL)

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
	holdActableContainment(t, eng, protocol.ContainmentDestination{Node: "CONT-1", Children: []string{},
		Bins: []protocol.ContainmentBin{{Node: "CONT-1", BinID: 42, PayloadCode: "PART-HOLD"}}})
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
	holdActableContainment(t, eng, protocol.ContainmentDestination{Node: "CONT-1", Children: []string{}, Bins: []protocol.ContainmentBin{}})
	if _, err := eng.ReleaseFromContainment("CONT-1", 42, "quality"); err == nil ||
		!strings.Contains(err.Error(), "not at") {
		t.Errorf("err = %v, want the stale-screen refusal", err)
	}

	// A SHARED hold group is legitimate: a second producer routes its
	// contained bins to the same spot with a DIFFERENT outbound, and the
	// BIN'S PAYLOAD disambiguates the release. The PART-HOLD bin still
	// releases to FG-1 (its own producer's outbound) despite the second
	// claim covering the same node.
	otherProc, err := eng.db.CreateProcess("HOLD-PROC-B", "", "", "", false)
	testutil.MustNoErr(t, err, "create second process")
	otherStyle, err := eng.db.CreateStyle("HOLD-STYLE-B", "", otherProc)
	testutil.MustNoErr(t, err, "create second style")
	_, err = eng.db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: otherStyle, CoreNodeName: "PLN-OTHER", Role: "produce",
		SwapMode: "two_robot_press_index", PayloadCode: "PART-OTHER", UOPCapacity: 5,
		PairedCoreNode: "PLN-OTHER-B", OutboundDestination: "FG-2",
		ContainmentDestination: "CONT-1",
	})
	if err != nil {
		t.Fatalf("upsert second producer's claim: %v", err)
	}
	holdActableContainment(t, eng, protocol.ContainmentDestination{Node: "CONT-1", Children: []string{},
		Bins: []protocol.ContainmentBin{{Node: "CONT-1", BinID: 42, PayloadCode: "PART-HOLD"}}})
	order2, err := eng.ReleaseFromContainment("CONT-1", 42, "quality")
	if err != nil {
		t.Fatalf("release through a shared group: %v", err)
	}
	if order2.DeliveryNode != "FG-1" {
		t.Errorf("shared-group release went to %s, want FG-1 (the bin's payload picks the outbound)", order2.DeliveryNode)
	}

	// The other payload's bin releases to the OTHER producer's outbound.
	holdActableContainment(t, eng, protocol.ContainmentDestination{Node: "CONT-1", Children: []string{},
		Bins: []protocol.ContainmentBin{{Node: "CONT-1", BinID: 99, PayloadCode: "PART-OTHER"}}})
	order3, err := eng.ReleaseFromContainment("CONT-1", 99, "quality")
	if err != nil {
		t.Fatalf("release the second payload: %v", err)
	}
	if order3.DeliveryNode != "FG-2" {
		t.Errorf("PART-OTHER release went to %s, want FG-2", order3.DeliveryNode)
	}

	// A copy that may not be decided from (Core has not answered for longer
	// than the decision rule allows) refuses with its sentence, and nothing
	// is walked (site 9 of the reads: the containment copy, under Actable).
	before := len(emptyOutMoves(t, eng, "CONT-1"))
	eng.feeds.now = func() time.Time { return time.Now().Add(ActableAckWithin + time.Second) }
	_, err = eng.ReleaseFromContainment("CONT-1", 99, "quality")
	if err == nil || err.Error() != "Core's picture of CONT-1 is not current — try again" {
		t.Errorf("not actable: err = %v, want the not-current sentence", err)
	}
	if after := len(emptyOutMoves(t, eng, "CONT-1")); after != before {
		t.Errorf("not actable: moves %d → %d, want none created", before, after)
	}
}

// TestRecallContainedPayload_EachMoveNamesItsBinAndHold: the recall walks a
// contained bin from its FG outbound into containment by a move that names the
// bin and asks for its hold, as the station's hold does; no separate marker
// call is made.
func TestRecallContainedPayload_EachMoveNamesItsBinAndHold(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	stub := &holdStubCore{}
	srv := stub.server()
	t.Cleanup(srv.Close)
	eng.coreClient = stubCoreClient(srv.URL)

	seedHoldNode(t, eng.db, "PLN-RECALL")
	procID, err := eng.db.CreateProcess("FG-PROC", "fg", "", "", false)
	testutil.MustNoErr(t, err, "fg process")
	_, err = eng.db.CreateProcessNode(processes.NodeInput{ProcessID: procID, CoreNodeName: "FG-1", Code: "FG1",
		Name: "FG outbound", Sequence: 1, Enabled: true})
	testutil.MustNoErr(t, err, "fg node")
	// Nothing held in the bins table: the claim's refusal is the not-current
	// sentence, and nothing is recalled (site 10 of the reads).
	created, refusals, err := eng.RecallContainedPayload("PART-HOLD", "quality-lead")
	testutil.MustNoErr(t, err, "RecallContainedPayload, no table")
	if created != 0 || len(refusals) != 1 || refusals[0] != "Core's picture of FG-1 is not current — try again" {
		t.Fatalf("recall with no table = %d created, refusals %v, want none and the not-current sentence", created, refusals)
	}

	holdActableBins(t, eng, protocol.NodeBinsRow{Node: "FG-1", BinID: 43, PayloadCode: "PART-HOLD", Occupied: true})
	created, refusals, err = eng.RecallContainedPayload("PART-HOLD", "quality-lead")
	testutil.MustNoErr(t, err, "RecallContainedPayload")
	if created != 1 || len(refusals) != 0 {
		t.Fatalf("recall = %d created, refusals %v, want one", created, refusals)
	}
	if holds, _ := stub.holdCalls(); len(holds) != 0 {
		t.Errorf("hold marker calls = %v, want none: the move carries the hold", holds)
	}
	moves := emptyOutMoves(t, eng, "FG-1")
	if len(moves) != 1 || moves[0].BinID != 43 || moves[0].QualityHoldBy != "quality-lead" || moves[0].DeliveryNode != "CONT-1" {
		t.Errorf("recall moves = %+v, want one to CONT-1 naming bin 43 held by quality-lead", moves)
	}
}
