package www

import (
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// enrichViewContainmentTargets is the station view's containment detection:
// a node named as a containment destination by a producing claim gets
// stamped with that claim's outbound (the release target); claims that
// DISAGREE about the outbound stamp nothing — a tile with no action is the
// safe face of a config problem, the same rule the release verb enforces.
func TestEnrichViewContainmentTargets(t *testing.T) {
	h, _ := newTestHandlers(t)

	pid, err := testDB.CreateProcess("StampProc", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := testDB.CreateProcessNode(processes.NodeInput{
		ProcessID: pid, CoreNodeName: "PLN-STAMP", Code: "P1", Name: "Press", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, err := testDB.CreateStyle("STAMP-STYLE", "", pid)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	if _, err := testDB.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "PLN-STAMP", Role: "produce",
		SwapMode: "two_robot_press_index", PayloadCode: "PART-S", UOPCapacity: 20,
		PairedCoreNode: "PLN-STAMP-B", OutboundDestination: "ULN-1",
		ContainmentDestination: "CONT-1",
	}); err != nil {
		t.Fatalf("upsert claim: %v", err)
	}

	views := []domain.OperatorStationView{{
		Nodes: []domain.StationNodeView{
			{Node: domain.Node{ID: nodeID, CoreNodeName: "PLN-STAMP"}},
			{Node: domain.Node{CoreNodeName: "CONT-1"}}, // the containment position
			{Node: domain.Node{CoreNodeName: "UNRELATED"}},
		},
	}}

	enrichViewContainmentTargets(h.engine, views)

	nodes := views[0].Nodes
	if nodes[0].ContainmentReleaseTarget != "" {
		t.Errorf("the PRODUCER must not be stamped as a containment position: got %q", nodes[0].ContainmentReleaseTarget)
	}
	// A BIN-LESS containment tile carries no stamp: the target shown on a
	// tile is the outbound FOR THE BIN ON IT, and there is no bin. Nothing
	// renders the target on a bin-less tile, so nothing is lost.
	if nodes[1].ContainmentReleaseTarget != "" {
		t.Errorf("bin-less containment tile stamped %q, want no stamp (the target is per-bin)", nodes[1].ContainmentReleaseTarget)
	}
	if nodes[2].ContainmentReleaseTarget != "" {
		t.Errorf("unrelated node stamped %q", nodes[2].ContainmentReleaseTarget)
	}

	// A SHARED hold group is legitimate: a second producer routes its
	// contained bins to the same spot with a DIFFERENT outbound, and the
	// BIN'S PAYLOAD disambiguates whose outbound the tile shows (the release
	// verb resolves the same way). An ASSY bin on the tile shows ULN-1; a
	// PART-B bin shows ULN-2; an unknown payload shows nothing.
	otherProc, err := testDB.CreateProcess("StampProcB", "", "", "", false)
	testutil.MustNoErr(t, err, "create second process")
	otherStyle, err := testDB.CreateStyle("STAMP-STYLE-B", "", otherProc)
	testutil.MustNoErr(t, err, "create second style")
	if _, err := testDB.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: otherStyle, CoreNodeName: "PLN-STAMP-B2", Role: "produce",
		SwapMode: "two_robot_press_index", PayloadCode: "PART-B", UOPCapacity: 5,
		PairedCoreNode: "PLN-STAMP-B3", OutboundDestination: "ULN-2",
		ContainmentDestination: "CONT-1",
	}); err != nil {
		t.Fatalf("upsert second producer's claim: %v", err)
	}

	views2 := []domain.OperatorStationView{{
		Nodes: []domain.StationNodeView{
			{Node: domain.Node{CoreNodeName: "CONT-1"}, BinState: &domain.NodeBinState{Occupied: true, PayloadCode: "PART-S"}},
			{Node: domain.Node{CoreNodeName: "CONT-1"}, BinState: &domain.NodeBinState{Occupied: true, PayloadCode: "PART-B"}},
			{Node: domain.Node{CoreNodeName: "CONT-1"}, BinState: &domain.NodeBinState{Occupied: true, PayloadCode: "PART-OTHER"}},
		},
	}}
	enrichViewContainmentTargets(h.engine, views2)
	if got := views2[0].Nodes[0].ContainmentReleaseTarget; got != "ULN-1" {
		t.Errorf("PART-S bin stamped %q, want ULN-1 (the payload's own producer's outbound)", got)
	}
	if got := views2[0].Nodes[1].ContainmentReleaseTarget; got != "ULN-2" {
		t.Errorf("PART-B bin stamped %q, want ULN-2 (the shared group's other producer)", got)
	}
	if got := views2[0].Nodes[2].ContainmentReleaseTarget; got != "" {
		t.Errorf("unrouted payload stamped %q, want no stamp", got)
	}
}

// Two producers that share a containment destination AND a payload but name
// different outbounds are a config error: that payload's stamp is dropped and
// the station view still renders. Before, the conflict was written into a map
// that was never made, and every station view on the Edge answered 500.
func TestEnrichViewContainmentTargets_ConflictingOutboundsStampNothing(t *testing.T) {
	h, _ := newTestHandlers(t)

	for i, out := range []string{"ULN-C1", "ULN-C2", "ULN-C3"} {
		pid, err := testDB.CreateProcess("ConflictProc"+out, "", "", "", false)
		testutil.MustNoErr(t, err, "create process")
		styleID, err := testDB.CreateStyle("CONFLICT-STYLE-"+out, "", pid)
		testutil.MustNoErr(t, err, "create style")
		if _, err := testDB.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
			StyleID: styleID, CoreNodeName: "PLN-CONFLICT-" + out, Role: "produce",
			SwapMode: "two_robot_press_index", PayloadCode: "PART-CONFLICT", UOPCapacity: 10 + i,
			PairedCoreNode:      "PLN-CONFLICT-B-" + out,
			OutboundDestination: out, ContainmentDestination: "CONT-CONFLICT",
		}); err != nil {
			t.Fatalf("upsert claim %s: %v", out, err)
		}
	}

	views := []domain.OperatorStationView{{
		Nodes: []domain.StationNodeView{
			{Node: domain.Node{CoreNodeName: "CONT-CONFLICT"}, BinState: &domain.NodeBinState{Occupied: true, PayloadCode: "PART-CONFLICT"}},
		},
	}}
	enrichViewContainmentTargets(h.engine, views)
	if got := views[0].Nodes[0].ContainmentReleaseTarget; got != "" {
		t.Errorf("a payload whose producers disagree about the outbound stamped %q, want no stamp", got)
	}
}
