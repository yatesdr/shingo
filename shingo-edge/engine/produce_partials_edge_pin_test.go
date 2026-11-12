package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// The Edge's part of produce partials: it authors the route, carries the
// operator's count and pick, and decides nothing about a carrier's contents.

// TestProducePartials_ManualEmptySendsTheOperatorsCount: on an uncurtained
// node the manual empty is the Material page's move, and the count the
// operator typed rides it as the move's remaining count — Core reads it at
// the claim and chooses the drop from it. Today an uncurtained node with
// outbound staging took a complex shape that carried no count at all.
func TestProducePartials_ManualEmptySendsTheOperatorsCount(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)
	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	task = testutil.Must(t, task, err, "node task")
	leg, err := db.GetOrder(*task.OldMaterialReleaseOrderID)
	leg = testutil.Must(t, leg, err, "release leg")
	db.UpdateOrderStatus(leg.ID, string(orders.StatusSubmitted))
	db.UpdateOrderStatus(leg.ID, string(orders.StatusFailed))
	emitOrderFailed(eng, leg.ID, leg.UUID, leg.OrderType, "swap failed")

	empty, err := eng.EvacuateNode(processID, nodeID, 9)
	if err != nil {
		t.Fatalf("manual empty: %v", err)
	}
	if empty.OrderType != orders.TypeMove {
		t.Fatalf("the manual empty is a %s, want the Material page's move", empty.OrderType)
	}
	msgs, err := db.ListPendingOutbox(500)
	if err != nil {
		t.Fatalf("outbox: %v", err)
	}
	var sent *protocol.OrderRequest
	for _, m := range msgs {
		if m.MsgType != protocol.TypeOrderRequest {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal(m.Payload, &env); err != nil {
			t.Fatalf("envelope: %v", err)
		}
		var req protocol.OrderRequest
		if err := env.DecodePayload(&req); err != nil {
			t.Fatalf("order request: %v", err)
		}
		if req.OrderUUID == empty.UUID {
			sent = &req
		}
	}
	if sent == nil {
		t.Fatal("the manual empty's request never reached the outbox")
	}
	if sent.RemainingUOP == nil || *sent.RemainingUOP != 9 {
		t.Errorf("the move carries remaining %v, want the operator's 9", sent.RemainingUOP)
	}
}

// TestProducePartials_ThePickNamesTheFetch: the operator's chosen held partial
// rides the swap's fetch, the pickup at the node the refill comes from (the
// claim's inbound source, or its keep-staged spot), and no other step, on every
// shape that fetches in the pair: single robot, two robot, and an index press
// whichever robot refills. Robot 2's leg opens on a pickup of the empty on
// deck, which is no fetch: the first Empty pickup named that one (a bin that
// is not there). A swap that fetches nothing in the pair (sequential: its
// backfill is made after the removal) refuses the pick by name.
func TestProducePartials_ThePickNamesTheFetch(t *testing.T) {
	t.Parallel()
	base := processes.NodeClaim{CoreNodeName: "PP-LINE", Role: protocol.ClaimRoleProduce,
		PayloadCode: "PP-X", InboundSource: "PP-MKT", InboundStaging: "PP-STG", OutboundStaging: "PP-OUT",
		OutboundDestination: "PP-DEST"}
	node := &processes.Node{Name: "PP-LINE", CoreNodeName: "PP-LINE"}
	for _, c := range []struct {
		name  string
		edit  func(*processes.NodeClaim)
		fetch string
	}{
		{"single robot", func(c *processes.NodeClaim) { c.SwapMode = protocol.SwapModeSingleRobot }, "PP-MKT"},
		{"two robot", func(c *processes.NodeClaim) { c.SwapMode = protocol.SwapModeTwoRobot }, "PP-MKT"},
		{"index press, Robot 1 refills", func(c *processes.NodeClaim) {
			c.SwapMode, c.PairedCoreNode = protocol.SwapModeTwoRobotPressIndex, "PP-BACK"
		}, "PP-MKT"},
		{"index press, Robot 2 refills", func(c *processes.NodeClaim) {
			c.SwapMode, c.PairedCoreNode, c.RefillBy = protocol.SwapModeTwoRobotPressIndex, "PP-BACK", protocol.RefillByIndexRobot
		}, "PP-MKT"},
		{"three-position index press, Robot 2 refills", func(c *processes.NodeClaim) {
			c.SwapMode, c.PairedCoreNode, c.SecondPairedCoreNode = protocol.SwapModeTwoRobotPressIndex, "PP-BACK", "PP-THIRD"
			c.RefillBy = protocol.RefillByIndexRobot
		}, "PP-MKT"},
		{"two robot from its spot", func(c *processes.NodeClaim) {
			c.SwapMode, c.KeepStagedNode = protocol.SwapModeTwoRobot, "PP-SPOT"
		}, "PP-SPOT"},
	} {
		claim := base
		c.edit(&claim)
		d, err := BuildSwapDispatch(node, &claim)
		if err != nil || d == nil {
			t.Fatalf("%s: no dispatch: %v", c.name, err)
		}
		testutil.MustNoErr(t, nameRefillBin(d, &claim, 42), c.name)
		named := 0
		for _, steps := range [][]protocol.ComplexOrderStep{d.StepsA, d.StepsB} {
			for _, s := range steps {
				if s.BinID == 0 {
					continue
				}
				named++
				if s.Action != protocol.ActionPickup || s.Node != c.fetch || s.BinID != 42 {
					t.Errorf("%s: bin 42 named on %+v, want the fetch at %s", c.name, s, c.fetch)
				}
			}
		}
		if named != 1 {
			t.Errorf("%s: %d steps name the pick, want exactly the fetch", c.name, named)
		}
	}

	seq := base
	seq.SwapMode, seq.PairedCoreNode, seq.InboundStaging, seq.OutboundStaging = protocol.SwapModeSequential, "PP-B", "", ""
	d, err := BuildSwapDispatch(node, &seq)
	if err != nil || d == nil {
		t.Fatalf("sequential: no dispatch: %v", err)
	}
	err = nameRefillBin(d, &seq, 42)
	if err == nil || !strings.Contains(err.Error(), "fetches no carrier") {
		t.Errorf("sequential: the pick = %v, want a refusal naming that the swap fetches no carrier", err)
	}
	for _, steps := range [][]protocol.ComplexOrderStep{d.StepsA, d.StepsB} {
		for _, s := range steps {
			if s.BinID != 0 {
				t.Errorf("sequential: a refused pick named bin %d on %+v", s.BinID, s)
			}
		}
	}
}

// TestProducePartials_AHomeMayNotTakePartialsToUnload: a partial home is
// refused at save when it names a loader window (or its group) whose loader
// takes partly filled carriers to unload — the two say opposite things about
// the same carriers. Any other home saves.
func TestProducePartials_AHomeMayNotTakePartialsToUnload(t *testing.T) {
	t.Parallel()
	kinds := domain.CoreNodeKindsOf(map[string]protocol.NodeInfo{
		"STAGE2":      {Name: "STAGE2", NodeType: protocol.NodeClassNGRP, AcceptsPartials: true},
		"STAGE2.WIN1": {Name: "STAGE2.WIN1", AcceptsPartials: true},
		"PP-HOME":     {Name: "PP-HOME", NodeType: protocol.NodeClassNGRP},
	}, nil, false)
	for _, c := range []struct {
		home   string
		refuse bool
	}{{"STAGE2", true}, {"STAGE2.WIN1", true}, {"PP-HOME", false}, {"", false}} {
		in := domain.NodeClaimInput{CoreNodeName: "PP-LINE", Role: protocol.ClaimRoleProduce, PartialDestination: c.home}
		refused := false
		for _, f := range kinds.RefusedLegs(in) {
			if f.Field == "partial_destination" {
				refused = true
			}
		}
		if refused != c.refuse {
			t.Errorf("partial home %q: refused = %v, want %v", c.home, refused, c.refuse)
		}
	}
}

// TestProducePartials_AReturnedPartialCountsFromItsCount: a held partial called
// back to its line lands with the count Core holds for it, and the station
// counts from there — the pack completes at capacity, not at a fresh empty's
// zero. Core's landing carries the count (BinLanded.Count); the landing is the
// station's one bind.
func TestProducePartials_AReturnedPartialCountsFromItsCount(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "")
	node, err := db.GetProcessNode(nodeID)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.HandleBinLanded(protocol.BinLanded{BinID: 77, CoreNodeName: node.CoreNodeName, Count: 22, Epoch: 3,
		PayloadCode: "WIDGET-A", PayloadKnown: true, PlacedSeq: 1})
	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	if rt.ActiveBinID == nil || *rt.ActiveBinID != 77 || rt.RemainingUOPCached != 22 {
		t.Errorf("after the landing the seat holds bin %v counting %d, want bin 77 counting its 22",
			rt.ActiveBinID, rt.RemainingUOPCached)
	}
}
