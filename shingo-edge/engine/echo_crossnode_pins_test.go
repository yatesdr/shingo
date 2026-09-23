package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// echo_crossnode_pins_test.go — an Edge-created order that is tracked at one
// process node (A, the creator) and delivers to ANOTHER process node on the same
// Edge (B), after Core's projection echo.
//
// Core projects every order it admits back to the station that sent it
// (CreateInboundOrder → projectOrder, since cdc95c55). The Edge applier guesses
// process_node_id from the DELIVERY node (resolveProjectionNode), and
// UpsertProjection keeps the stored value, so the order stays A's.
//
// BEFORE THE FIX (pinned at 1500969a) the conflict arm took the guess over the
// stored value and the order became B's: B's tile listed it, the completion
// chain ran for B and wiped B's pointer to B's own order, A's pointer was left
// naming a finished order, the carrier bound at B, and — for the U2 — B's
// double-tap guard refused PUSH EMPTY while A's went blind and filed a second U2.
//
// Four readers per creator:
//
//	(a) the board — which tile ListActiveByNodeKeys files it under;
//	(b) the completion chain — which node orderCompletionCtx is built for;
//	(c) the runtime pointers — the creator's own, and a pointer B already held
//	    for an order of its own, after A's order completes;
//	(d) the delivered handler — which node binds the carrier.
//
// Complex orders are not in scope: Core projects only what admitOrder admits
// (lifecycle_service.go projectOrder), and complex intake does not go through
// it, so a changeover evac or swap leg is never echoed.

// echoNodes is one fixture: the creator node A, the destination node B, and an
// order of B's own that B's runtime points at before A's order arrives.
type echoNodes struct {
	eng          *Engine
	db           *store.DB
	aID, bID     int64
	aCore, bCore string
	bOwnOrder    int64
}

// echoObserved is what the four readers say about one order after the echo.
type echoObserved struct {
	rowNode        int64 // orders.process_node_id after the echo
	boardA, boardB bool  // (a) listed on A's tile / B's tile
	ctxNode        int64 // (b) orderCompletionCtx node
	aActive        bool  // (c) A's active_order_id still names the order after completion
	bOwnKept       bool  // (c) B's pointer to its own order survives A's order completing
	boundAtA       bool  // (d) the delivered carrier bound to A's runtime
	boundAtB       bool  // (d) ... to B's runtime
}

// echoProjectionOf is the ProjectionFor-shaped echo of an Edge row: Core
// promotes retrieve+flag to retrieve_empty and sends the flag explicitly.
func echoProjectionOf(o *storeorders.Order) protocol.OrderProjection {
	typ := o.OrderType
	if typ == orders.TypeRetrieve && o.RetrieveEmpty {
		typ = protocol.OrderTypeRetrieveEmpty
	}
	return protocol.OrderProjection{
		OrderUUID:     o.UUID,
		OrderType:     typ,
		Status:        string(protocol.StatusDispatched),
		Quantity:      o.Quantity,
		SourceNode:    o.SourceNode,
		DeliveryNode:  o.DeliveryNode,
		PayloadCode:   o.PayloadCode,
		RetrieveEmpty: o.RetrieveEmpty,
	}
}

// observeAfterEcho echoes o, reads (a)-(b), then drives it through delivery and
// confirm and reads (c)-(d).
func observeAfterEcho(t *testing.T, n *echoNodes, o *storeorders.Order) echoObserved {
	t.Helper()
	if o.ProcessNodeID == nil || *o.ProcessNodeID != n.aID {
		t.Fatalf("fixture: order %d created at %v, want the creator %d", o.ID, o.ProcessNodeID, n.aID)
	}
	if o.DeliveryNode != n.bCore {
		t.Fatalf("fixture: order %d delivers to %q, want B %q", o.ID, o.DeliveryNode, n.bCore)
	}
	_, err := n.eng.ApplyOrderProjection(echoProjectionOf(o))
	testutil.MustNoErr(t, err, "apply echo")
	row, err := n.db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload after echo")

	var obs echoObserved
	if row.ProcessNodeID != nil {
		obs.rowNode = *row.ProcessNodeID
	}
	board, err := storeorders.ListActiveByNodeKeys(n.db.DB, []storeorders.NodeKey{
		{ProcessNodeID: n.aID, CoreNodeName: n.aCore},
		{ProcessNodeID: n.bID, CoreNodeName: n.bCore},
	})
	testutil.MustNoErr(t, err, "board read")
	for _, x := range board[n.aID] {
		obs.boardA = obs.boardA || x.ID == o.ID
	}
	for _, x := range board[n.bID] {
		obs.boardB = obs.boardB || x.ID == o.ID
	}
	if ctx := n.eng.loadOrderCompletionCtx(OrderCompletedEvent{OrderID: o.ID, ProcessNodeID: row.ProcessNodeID}); ctx != nil {
		obs.ctxNode = ctx.node.ID
	}

	// (d) then (c): deliver, then confirm if nothing auto-confirmed it.
	bin := int64(991) // distinct from any bin id a fake Core hands the creator
	uop := 12
	testutil.MustNoErr(t, n.eng.orderMgr.HandleDeliveredWithExpiry(row.UUID, "delivered", nil, &bin, &uop, nil, 0, row.DeliveryNode, ""),
		"deliver")
	cur, err := n.db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload after delivery")
	if cur.Status == orders.StatusDelivered {
		testutil.MustNoErr(t, n.eng.orderMgr.ConfirmDelivery(o.ID, 0), "confirm")
	}
	rtA, err := n.db.GetProcessNodeRuntime(n.aID)
	testutil.MustNoErr(t, err, "runtime A")
	rtB, err := n.db.GetProcessNodeRuntime(n.bID)
	testutil.MustNoErr(t, err, "runtime B")
	obs.aActive = rtA.ActiveOrderID != nil && *rtA.ActiveOrderID == o.ID
	obs.bOwnKept = rtB.ActiveOrderID != nil && *rtB.ActiveOrderID == n.bOwnOrder
	obs.boundAtA = rtA.ActiveBinID != nil && *rtA.ActiveBinID == bin
	obs.boundAtB = rtB.ActiveBinID != nil && *rtB.ActiveBinID == bin
	return obs
}

// newEchoNodes builds A and B as stored manual_swap claims on separate
// processes (A with aRole and outbound B), wired to the live event chain, and
// points B's runtime at an order of B's own.
func newEchoNodes(t *testing.T, aPrefix string, aRole protocol.ClaimRole, bPrefix string) *echoNodes {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
	eng.wireEventHandlers()
	bCore := bPrefix + "-MSWAP-NODE"
	aID, _ := seedManualSwapClaim(t, db, aPrefix, aRole, "PART-EC", bCore)
	bID, _ := seedManualSwapClaim(t, db, bPrefix, protocol.ClaimRoleConsume, "PART-EC", "EMPTY-TOTES")
	return withBOwnOrder(t, &echoNodes{eng: eng, db: db, aID: aID, bID: bID, aCore: aPrefix + "-MSWAP-NODE", bCore: bCore})
}

func withBOwnOrder(t *testing.T, n *echoNodes) *echoNodes {
	t.Helper()
	own, err := n.db.CreateOrder("echo-b-own-"+n.bCore, orders.TypeRetrieve, &n.bID, false, 1, n.bCore, "", "FG-MKT", "", false, "PART-EC", "", "")
	testutil.MustNoErr(t, err, "B's own order")
	testutil.MustNoErr(t, n.db.SetProcessNodeRuntimeActiveOrder(n.bID, &own), "point B at its own order")
	n.bOwnOrder = own
	return n
}

func checkEcho(t *testing.T, n *echoNodes, got, want echoObserved) {
	t.Helper()
	want.ctxNode = pick(want.ctxNode, n)
	want.rowNode = pick(want.rowNode, n)
	if got != want {
		t.Errorf("after the echo:\n got  %+v\n want %+v\n(A=%d B=%d)", got, want, n.aID, n.bID)
	}
}

// pick maps the sentinels echoA/echoB to the fixture's real ids.
func pick(v int64, n *echoNodes) int64 {
	switch v {
	case echoA:
		return n.aID
	case echoB:
		return n.bID
	}
	return v
}

const (
	echoA = -1
	echoB = -2
)

// TestPinEcho_CrossNodeMoves is the per-creator table: after the echo every
// creator's order is still A's, and every reader follows A.
func TestPinEcho_CrossNodeMoves(t *testing.T) {
	t.Parallel()

	// What the four readers say for a MOVE leaving A for B: the row is A's and
	// only A's tile lists it; the completion chain runs for A and clears A's
	// pointer; B's pointer to its own order is untouched; the delivered handler
	// binds nowhere (the bin did not land at A, the node the order belongs to).
	// Before the fix: row B, both tiles, ctx B, A's pointer dangling, B's
	// pointer wiped, bound at B.
	moveAfterEcho := echoObserved{
		rowNode: echoA, boardA: true, boardB: false, ctxNode: echoA,
		aActive: false, bOwnKept: true, boundAtA: false, boundAtB: false,
	}

	cases := []struct {
		name   string
		role   protocol.ClaimRole
		create func(t *testing.T, n *echoNodes) *storeorders.Order
		want   echoObserved
	}{
		{
			// The call apiCreateMoveOrder makes (www/handlers_api_orders.go).
			name: "manual_move_api", role: protocol.ClaimRoleConsume,
			create: func(t *testing.T, n *echoNodes) *storeorders.Order {
				o, err := n.eng.orderMgr.CreateMoveOrder(&n.aID, 1, n.aCore, n.bCore, true, orders.NoDemand())
				testutil.MustNoErr(t, err, "CreateMoveOrder")
				testutil.MustNoErr(t, n.db.SetProcessNodeRuntimeActiveOrder(n.aID, &o.ID), "A points at its move")
				return o
			},
			want: moveAfterEcho,
		},
		{
			// The lineside / partial release: the spent carrier to the claim's
			// outbound, here another node on this Edge.
			name: "release_move", role: protocol.ClaimRoleConsume,
			create: func(t *testing.T, n *echoNodes) *storeorders.Order {
				o, err := n.eng.ReleaseNodeEmpty(n.aID)
				testutil.MustNoErr(t, err, "ReleaseNodeEmpty")
				return o
			},
			want: moveAfterEcho,
		},
		{
			// The U2 from CLEAR — stage 1's empty-out to a stage-2 window.
			name: "u2_clear", role: protocol.ClaimRoleConsume,
			create: func(t *testing.T, n *echoNodes) *storeorders.Order {
				srv := fakeCoreBinServer(t, true, "PART-EC")
				n.eng.coreClient = NewCoreClient(srv.URL)
				testutil.MustNoErr(t, n.eng.ClearBin(n.aID, ""), "ClearBin")
				return echoOnlyMoveFrom(t, n)
			},
			want: moveAfterEcho,
		},
		{
			// The L2 from LOAD's fallback, to an outbound that is a node here.
			name: "l2_load_fallback", role: protocol.ClaimRoleProduce,
			create: func(t *testing.T, n *echoNodes) *storeorders.Order {
				c := newSCCore(t)
				c.set(n.aCore, true, "")
				n.eng.coreClient = NewCoreClient(c.srv.URL)
				testutil.MustNoErr(t, n.eng.LoadBin(n.aID, "PART-EC", nil, scManifest), "LoadBin")
				return echoOnlyMoveFrom(t, n)
			},
			want: moveAfterEcho,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := newEchoNodes(t, "E"+tc.name[:3], tc.role, "B"+tc.name[:3])
			o := tc.create(t, n)
			checkEcho(t, n, observeAfterEcho(t, n, o), tc.want)
		})
	}
}

// echoOnlyMoveFrom returns the one active move leaving A.
func echoOnlyMoveFrom(t *testing.T, n *echoNodes) *storeorders.Order {
	t.Helper()
	moves := scMovesFrom(t, n.db, n.aCore)
	if len(moves) != 1 {
		t.Fatalf("fixture: moves leaving %s = %d, want 1", n.aCore, len(moves))
	}
	return &moves[0]
}

// TestPinEcho_SharedWindowL1ToASiblingWindow: an operator REQUEST EMPTY at
// window X of a Core-owned shared loader is staged at whichever window is free —
// here W — but tracked at X, and stays X's through the echo. Before the fix the
// echo re-credited it to W, and X's tile lost it (it sources from the market).
func TestPinEcho_SharedWindowL1ToASiblingWindow(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
	eng.wireEventHandlers()
	core := newSCCore(t)
	eng.coreClient = NewCoreClient(core.srv.URL)

	const x, w = "ECHO-SW-X", "ECHO-SW-W"
	procID, err := db.CreateProcess("ECHO-SW-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	xID, err := db.CreateProcessNode(processes.NodeInput{ProcessID: procID, CoreNodeName: x, Code: "SX", Name: x, Sequence: 1, Enabled: true})
	testutil.MustNoErr(t, err, "create X")
	wID, err := db.CreateProcessNode(processes.NodeInput{ProcessID: procID, CoreNodeName: w, Code: "SW", Name: w, Sequence: 2, Enabled: true})
	testutil.MustNoErr(t, err, "create W")
	for _, id := range []int64{xID, wID} {
		_, err = db.EnsureProcessNodeRuntime(id)
		testutil.MustNoErr(t, err, "ensure runtime")
	}
	info := sharedLoaderInfo(x, "produce", "operator", "PART-SW", 0, 0)
	info.Positions = append(info.Positions, protocol.LoaderPosition{CoreNodeName: w, Kind: "window"})
	seedCoreLoader(t, eng, info)
	core.set(x, true, "") // X holds a carrier; W is free
	core.set(w, false, "")

	n := withBOwnOrder(t, &echoNodes{eng: eng, db: db, aID: xID, bID: wID, aCore: x, bCore: w})
	o, err := eng.RequestEmptyBin(xID, "")
	testutil.MustNoErr(t, err, "RequestEmptyBin")
	checkEcho(t, n, observeAfterEcho(t, n, o), echoObserved{
		rowNode: echoA, boardA: true, boardB: false, ctxNode: echoA,
		aActive: false, bOwnKept: true, boundAtA: false, boundAtB: false,
	})
}

// TestPinEcho_Stage1DoubleTapSeesItsOwnU2AfterTheEcho: CLEAR at stage 1 files
// the U2; after the echo a second CLEAR at stage 1 still sees it and skips, and
// stage 2's PUSH EMPTY is not blocked by a U2 that is not its own. Before the fix
// the echo re-credited the U2 to stage 2: the second CLEAR filed a second U2 for
// the same carrier and stage 2 refused PUSH EMPTY — the B5 wedge.
func TestPinEcho_Stage1DoubleTapSeesItsOwnU2AfterTheEcho(t *testing.T) {
	t.Parallel()
	n := newEchoNodes(t, "EDT1", protocol.ClaimRoleConsume, "BDT2")
	srv := fakeCoreBinServer(t, true, "")
	n.eng.coreClient = NewCoreClient(srv.URL)

	testutil.MustNoErr(t, n.eng.ClearBin(n.aID, ""), "first CLEAR")
	u2 := echoOnlyMoveFrom(t, n)
	_, err := n.eng.ApplyOrderProjection(echoProjectionOf(u2))
	testutil.MustNoErr(t, err, "echo")

	testutil.MustNoErr(t, n.eng.ClearBin(n.aID, ""), "second CLEAR")
	if got := len(scMovesFrom(t, n.db, n.aCore)); got != 1 {
		t.Errorf("U2s leaving stage 1 after CLEAR, echo, CLEAR = %d, want 1 — the second CLEAR "+
			"must see the first U2 at its own process node", got)
	}
	if err := n.eng.PushEmptyOut(n.bID); err != nil {
		t.Errorf("PUSH EMPTY at stage 2 = %v, want accepted — stage 1's U2 is not stage 2's empty-out", err)
	}
}

// TestPinEcho_CoreAuthoredRowTakesTheDeliveryNodeGuess: a projection of an order
// the Edge never created lands as a new row (authored_by='core') whose
// process_node_id is resolved from its delivery node — the only answer there is,
// because Core has no Edge process node id. A second arrival of the same
// projection leaves it where it is. Both hold before and after the echo stops
// overwriting a stored process node: the first insert has nothing stored.
// (Unchanged by the fix; pinned at 1500969a.)
func TestPinEcho_CoreAuthoredRowTakesTheDeliveryNodeGuess(t *testing.T) {
	t.Parallel()
	n := newEchoNodes(t, "ECA", protocol.ClaimRoleProduce, "BCA")
	p := protocol.OrderProjection{
		OrderUUID: "core-authored-1", OrderType: protocol.OrderTypeRetrieveEmpty, Status: string(protocol.StatusQueued),
		Quantity: 1, SourceNode: "EMPTY-MKT", DeliveryNode: n.bCore, RetrieveEmpty: true,
	}
	for i := 0; i < 2; i++ {
		_, err := n.eng.ApplyOrderProjection(p)
		testutil.MustNoErr(t, err, "apply projection")
		row, err := n.db.GetOrderByUUID(p.OrderUUID)
		testutil.MustNoErr(t, err, "reload")
		if row.AuthoredBy != "core" || row.ProcessNodeID == nil || *row.ProcessNodeID != n.bID {
			t.Errorf("arrival %d: authored_by=%q process_node_id=%v, want core / %d (resolved from the delivery node)",
				i+1, row.AuthoredBy, row.ProcessNodeID, n.bID)
		}
	}
}
