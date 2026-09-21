package store

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// claim_resolution_set_test.go — the batched resolver must answer exactly what
// the per-node one answers.
//
// NodeClaimSet replaces a point query per node in three walkers. It is only a
// replacement if it gives the same claim for every node in every state the
// point query could be asked about, INCLUDING the mid-changeover one where two
// styles claim the same node and the precedence decides. A single-style fixture
// cannot see a reversed precedence, so this drives both.

// TestNodeClaimSet_MatchesThePerNodeResolver walks every node under both
// precedences and compares the two resolvers row for row.
func TestNodeClaimSet_MatchesThePerNodeResolver(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	processID, err := db.CreateProcess("SET", "", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	activeStyle, err := db.CreateStyle("SET-ACTIVE", "outgoing", processID)
	if err != nil {
		t.Fatalf("create active style: %v", err)
	}
	targetStyle, err := db.CreateStyle("SET-TARGET", "incoming", processID)
	if err != nil {
		t.Fatalf("create target style: %v", err)
	}

	mkNode := func(name string, seq int) {
		if _, err := db.CreateProcessNode(processes.NodeInput{
			ProcessID: processID, CoreNodeName: name, Code: name,
			Name: name, Sequence: seq, Enabled: true,
		}); err != nil {
			t.Fatalf("create node %s: %v", name, err)
		}
	}
	mkClaim := func(styleID int64, name, payload string) int64 {
		id, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
			StyleID: styleID, CoreNodeName: name, Role: "consume",
			SwapMode: protocol.SwapModeSingleRobot, PayloadCode: payload, UOPCapacity: 100,
			InboundStaging: "SET-IN", OutboundStaging: "SET-OUT", OutboundDestination: "SET-DEST",
		})
		if err != nil {
			t.Fatalf("upsert claim %s/%s: %v", name, payload, err)
		}
		return id
	}

	// BOTH: both styles claim it — the precedence case.
	// ACTIVEONLY: only the outgoing style, the ordinary steady state.
	// TARGETONLY: only the incoming style, the add-node changeover case whose
	//   fallback the 2026-05-12 plant report is about.
	// RETIRED: a claim that exists but is retired — must read as absent.
	// BARE: a node no style claims at all.
	mkNode("SET_BOTH", 1)
	mkNode("SET_ACTIVEONLY", 2)
	mkNode("SET_TARGETONLY", 3)
	mkNode("SET_RETIRED", 4)
	mkNode("SET_BARE", 5)
	mkClaim(activeStyle, "SET_BOTH", "PART-OUT")
	mkClaim(targetStyle, "SET_BOTH", "PART-IN")
	mkClaim(activeStyle, "SET_ACTIVEONLY", "PART-OUT")
	mkClaim(targetStyle, "SET_TARGETONLY", "PART-IN")
	retiredID := mkClaim(activeStyle, "SET_RETIRED", "PART-GONE")
	if _, err := db.DB.Exec(`UPDATE style_node_claims SET retired_at=datetime('now') WHERE id=?`, retiredID); err != nil {
		t.Fatalf("retire claim: %v", err)
	}

	nodes, err := db.ListProcessNodesByProcess(processID)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	proc := &processes.Process{ID: processID, ActiveStyleID: &activeStyle, TargetStyleID: &targetStyle}

	set, err := db.NodeClaimsForStyles(ClaimStyleIDs(proc))
	if err != nil {
		t.Fatalf("NodeClaimsForStyles: %v", err)
	}
	for _, p := range []ClaimPrecedence{ActiveStyleFirst, TargetStyleFirst} {
		for i := range nodes {
			node := &nodes[i]
			want := db.ResolveNodeClaim(proc, node, p)
			got := set.Resolve(proc, node, p)
			switch {
			case want == nil && got != nil:
				t.Errorf("precedence %d, node %s: the set answered claim %d where the per-node "+
					"resolver answered nothing", p, node.CoreNodeName, got.ID)
			case want != nil && got == nil:
				t.Errorf("precedence %d, node %s: the set answered nothing where the per-node "+
					"resolver answered claim %d (%s)", p, node.CoreNodeName, want.ID, want.PayloadCode)
			case want != nil && got != nil && (want.ID != got.ID || want.PayloadCode != got.PayloadCode):
				t.Errorf("precedence %d, node %s: set = claim %d (%s), per-node = claim %d (%s)",
					p, node.CoreNodeName, got.ID, got.PayloadCode, want.ID, want.PayloadCode)
			}
		}
	}

	// A style the set was not built for answers NOTHING rather than something
	// from another style. A caller that forgot a style must get no claim, not
	// the wrong one.
	other := int64(99999)
	narrow, err := db.NodeClaimsForStyles([]int64{other})
	if err != nil {
		t.Fatalf("NodeClaimsForStyles(unknown): %v", err)
	}
	if got := narrow.Resolve(proc, &nodes[0], ActiveStyleFirst); got != nil {
		t.Errorf("a set built for an unrelated style resolved claim %d", got.ID)
	}
}

// TestNodeClaimSet_SharesOneClaimPerRow pins the aliasing the level sweep
// depends on: two process_nodes naming one core node get the SAME claim
// pointer, because the sweep's evaluators write to the struct they are handed.
//
// Under the per-node reads the second node re-read the row and saw the first
// node's falling-edge stamp already in it, so it did not stamp again. Copies
// would each start from the pre-stamp snapshot and the second one would
// re-stamp — moving the episode's recorded start time.
func TestNodeClaimSet_SharesOneClaimPerRow(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	processID, err := db.CreateProcess("ALIAS", "", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	styleID, err := db.CreateStyle("ALIAS-STYLE", "", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "ALIAS_WINDOW", Role: "consume",
		SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "PART-SHARED", UOPCapacity: 100,
		InboundStaging: "ALIAS-IN", OutboundStaging: "ALIAS-OUT", OutboundDestination: "ALIAS-DEST",
	}); err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	proc := &processes.Process{ID: processID, ActiveStyleID: &styleID}
	set, err := db.NodeClaimsForStyles(ClaimStyleIDs(proc))
	if err != nil {
		t.Fatalf("NodeClaimsForStyles: %v", err)
	}
	a := &processes.Node{ID: 1, ProcessID: processID, CoreNodeName: "ALIAS_WINDOW"}
	b := &processes.Node{ID: 2, ProcessID: processID, CoreNodeName: "ALIAS_WINDOW"}
	if set.Resolve(proc, a, ActiveStyleFirst) != set.Resolve(proc, b, ActiveStyleFirst) {
		t.Error("two nodes naming one core node got different claim structs — the level sweep's " +
			"evaluator writes to the struct it is handed, so copies would re-stamp a falling edge " +
			"the other node had already recorded")
	}
}
