package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// THE REQUEST PATH, FOR A KEEP-STAGED CELL.
//
// One REQUEST sends the short swap and whatever the spot needs, decided by
// reconcileSpot from what Core says stands on the spot (the occupancy call the
// request already makes, one more name) and what is already coming (the line's
// own rows). These drive RequestNodeMaterial / RequestProduceSwap end to end
// against a Core stub and count what lands in the order table.

const (
	ksLine   = "KS-LINE"
	ksSpot   = "KS-SPOT"
	ksMarket = "KS-MARKET"
	ksDest   = "KS-DEST"
	ksPart   = "PART-KS"
	ksPair   = "KS-PAIR" // a press's back position
)

// ksNodeBinsStub answers node-bins from a table; a name not in it is answered
// as an empty node Core has, which is what Core does for one it has.
func ksNodeBinsStub(t *testing.T, rows map[string]NodeBinInfo) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := []NodeBinInfo{}
		if r.URL.Path == "/api/telemetry/node-bins" {
			for _, n := range strings.Split(r.URL.Query().Get("nodes"), ",") {
				if n == "" {
					continue
				}
				row, ok := rows[n]
				if !ok {
					row = NodeBinInfo{}
				}
				row.NodeName = n
				out = append(out, row)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// keepStagedCell seeds a keep-staged cell and an engine whose Core answers
// node-bins from rows.
func keepStagedCell(t *testing.T, role protocol.ClaimRole, mode protocol.SwapMode, rows map[string]NodeBinInfo) (*Engine, *store.DB, int64, *processes.NodeClaim) {
	t.Helper()
	return seedCell(t, role, mode, true, rows)
}

// seedCell is keepStagedCell with the flag chosen.
func seedCell(t *testing.T, role protocol.ClaimRole, mode protocol.SwapMode, keepStaged bool, rows map[string]NodeBinInfo) (*Engine, *store.DB, int64, *processes.NodeClaim) {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.logFn = func(string, ...any) {}

	procID, err := db.CreateProcess("KS-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: ksLine, Code: "KS1", Name: ksLine, Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "node")
	styleID, err := db.CreateStyle("KS-STYLE", "", procID)
	testutil.MustNoErr(t, err, "style")
	testutil.MustNoErr(t, db.SetActiveStyle(procID, &styleID), "active style")
	in := processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: ksLine, Role: role, SwapMode: mode, PayloadCode: ksPart,
		UOPCapacity: 40, InboundSource: ksMarket, InboundStaging: ksSpot, OutboundDestination: ksDest,
	}
	if mode == protocol.SwapModeSingleRobot {
		in.OutboundStaging = "KS-OUT"
	}
	// The two modes with no staging hop: the spot is a node of its own, and a
	// press has its back position.
	if mode == protocol.SwapModeSequential || mode == protocol.SwapModeTwoRobotPressIndex {
		in.InboundStaging = ""
	}
	if mode == protocol.SwapModeTwoRobotPressIndex {
		in.PairedCoreNode = ksPair
	}
	claimID, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, in)
	testutil.MustNoErr(t, err, "claim")
	// The flag is written straight to the row: these tests are about the request
	// path, not the config door that admits it.
	if keepStaged {
		_, err = db.DB.Exec(`UPDATE style_node_claims SET keep_staged_node=? WHERE id=?`, ksSpot, claimID)
		testutil.MustNoErr(t, err, "keep_staged_node")
	}
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, 30), "runtime claim")

	eng.coreClient = stubCoreClient(ksNodeBinsStub(t, rows).URL)
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "re-read node")
	claim := requestedClaimAtNode(db, node)
	if claim == nil || (claim.KeepStagedNode != "") != keepStaged {
		t.Fatalf("fixture: no claim with keep-staged %v at %s (%+v)", keepStaged, ksLine, claim)
	}
	return eng, db, nodeID, claim
}

// spotOrders is what the line's order table holds for the spot: refills bound
// for it, and returns leaving it (a move from the spot to anywhere but the line;
// the node-empty downgrade's move to the line is not a return).
type spotOrders struct {
	refills, returns int
	returnPayloads   []string
}

func readSpotOrders(t *testing.T, db *store.DB, nodeID int64) spotOrders {
	t.Helper()
	rows, err := db.ListActiveOrdersByProcessNode(nodeID)
	testutil.MustNoErr(t, err, "rows")
	var got spotOrders
	for _, o := range rows {
		switch {
		case o.OrderType == orders.TypeRetrieve && o.DeliveryNode == ksSpot:
			got.refills++
			if o.SourceNode != ksMarket || !o.AutoConfirm {
				t.Errorf("refill %d: source %q autoConfirm %v, want %q and true", o.ID, o.SourceNode, o.AutoConfirm, ksMarket)
			}
		case o.OrderType == orders.TypeMove && o.SourceNode == ksSpot && o.DeliveryNode != ksLine:
			got.returns++
			got.returnPayloads = append(got.returnPayloads, o.PayloadCode)
			if o.DeliveryNode != ksMarket {
				t.Errorf("return %d goes to %q, want the inbound source %q", o.ID, o.DeliveryNode, ksMarket)
			}
		}
	}
	return got
}

func TestKeepStagedRequest_TheSpotGetsWhatTheRuleSays(t *testing.T) {
	t.Parallel()
	occupiedLine := NodeBinInfo{Occupied: true, PayloadCode: ksPart}
	cases := []struct {
		name        string
		role        protocol.ClaimRole
		mode        protocol.SwapMode
		spot        NodeBinInfo
		wantRefills int
		wantReturns int
		returnCarry string
	}{
		{"consume two_robot, spare right: the swap eats it, one comes", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot,
			NodeBinInfo{Occupied: true, PayloadCode: ksPart}, 1, 0, ""},
		{"consume two_robot, spot bare: one for the swap, one to stand", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot,
			NodeBinInfo{}, 2, 0, ""},
		{"consume single_robot, spare of another part: it goes back, two come", protocol.ClaimRoleConsume, protocol.SwapModeSingleRobot,
			NodeBinInfo{Occupied: true, PayloadCode: "PART-OTHER"}, 2, 1, "PART-OTHER"},
		{"produce two_robot, empty spare: one comes", protocol.ClaimRoleProduce, protocol.SwapModeTwoRobot,
			NodeBinInfo{Occupied: true}, 1, 0, ""},
		{"produce two_robot, a full on the spot: it goes back carrying its part", protocol.ClaimRoleProduce, protocol.SwapModeTwoRobot,
			NodeBinInfo{Occupied: true, PayloadCode: ksPart}, 2, 1, ksPart},
		// The same decisions on the two modes with no staging hop: the
		// sequential backfill and the press's refill leg lift the spare.
		{"consume sequential, spare right: the backfill eats it, one comes", protocol.ClaimRoleConsume, protocol.SwapModeSequential,
			NodeBinInfo{Occupied: true, PayloadCode: ksPart}, 1, 0, ""},
		{"produce sequential, spot bare: one for the backfill, one to stand", protocol.ClaimRoleProduce, protocol.SwapModeSequential,
			NodeBinInfo{}, 2, 0, ""},
		{"consume press, spare of another part: it goes back, two come", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobotPressIndex,
			NodeBinInfo{Occupied: true, PayloadCode: "PART-OTHER"}, 2, 1, "PART-OTHER"},
		{"produce press, empty spare: the refill leg eats it, one comes", protocol.ClaimRoleProduce, protocol.SwapModeTwoRobotPressIndex,
			NodeBinInfo{Occupied: true}, 1, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eng, db, nodeID, _ := keepStagedCell(t, c.role, c.mode,
				map[string]NodeBinInfo{ksLine: occupiedLine, ksPair: occupiedLine, ksSpot: c.spot})
			var err error
			if c.role == protocol.ClaimRoleProduce {
				_, err = eng.RequestProduceSwap(nodeID)
			} else {
				_, err = eng.RequestNodeMaterial(nodeID, 1)
			}
			testutil.MustNoErr(t, err, "request")
			got := readSpotOrders(t, db, nodeID)
			if got.refills != c.wantRefills || got.returns != c.wantReturns {
				t.Fatalf("refills=%d returns=%d, want %d and %d", got.refills, got.returns, c.wantRefills, c.wantReturns)
			}
			if c.wantReturns > 0 && got.returnPayloads[0] != c.returnCarry {
				t.Errorf("the return carries %q, want the bin's own %q", got.returnPayloads[0], c.returnCarry)
			}
		})
	}
}

// AN EMPTY GOES BACK UNTAGGED. A move's blank payload is back-filled from the
// line's claim everywhere else (and mid-changeover from the incoming style's);
// the carrier is the subject here, and an empty one carries no part. An empty
// standing on a consume claim's spot is wrong for it, so it goes back blank.
func TestKeepStagedRequest_AnEmptyReturnsUntagged(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, _ := keepStagedCell(t, protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot,
		map[string]NodeBinInfo{ksLine: {Occupied: true, PayloadCode: ksPart}, ksSpot: {Occupied: true}})
	_, err := eng.RequestNodeMaterial(nodeID, 1)
	testutil.MustNoErr(t, err, "request")
	got := readSpotOrders(t, db, nodeID)
	if got.returns != 1 || got.returnPayloads[0] != "" {
		t.Fatalf("returns=%d payloads=%q, want one return carrying no part", got.returns, got.returnPayloads)
	}
}

// What is already coming is counted, and only for the claim's part and role.
func TestKeepStagedRequest_CountsWhatIsComing(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, claim := keepStagedCell(t, protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot,
		map[string]NodeBinInfo{ksLine: {Occupied: true, PayloadCode: ksPart}, ksSpot: {}})
	// One refill already on its way for this part, and one for another part.
	_, err := eng.orderMgr.CreateRetrieveOrder(&nodeID, false, 1, ksSpot, ksMarket, "", "standard", ksPart, true, false,
		orders.Attached("prior"))
	testutil.MustNoErr(t, err, "prior refill")
	_, err = eng.orderMgr.CreateRetrieveOrder(&nodeID, false, 1, ksSpot, ksMarket, "", "standard", "PART-OLD", true, false,
		orders.Attached("stale"))
	testutil.MustNoErr(t, err, "stale refill")

	_, err = eng.RequestNodeMaterial(nodeID, 1)
	testutil.MustNoErr(t, err, "request")
	// Bare spot, swap consumes: 2 needed, 1 of this part coming → 1 more. The
	// other part's refill counts for nothing.
	if got := readSpotOrders(t, db, nodeID); got.refills != 3 {
		t.Fatalf("refills to the spot = %d, want 3 (the two prior plus one)", got.refills)
	}
	_ = claim
}

// Nothing is decided on a spot Core did not answer for.
func TestKeepStagedRequest_UnknownSpotOrdersNothing(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, _ := keepStagedCell(t, protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot,
		map[string]NodeBinInfo{ksLine: {Occupied: true, PayloadCode: ksPart}})
	eng.coreClient = stubCoreClient(headOccupancyStub(t, true).URL) // answers every name
	eng.coreNodesMu.Lock()
	eng.coreNodes = map[string]protocol.NodeInfo{ksLine: {Name: ksLine}}
	eng.coreNodesMu.Unlock()
	_, err := eng.RequestNodeMaterial(nodeID, 1)
	testutil.MustNoErr(t, err, "request")
	if got := readSpotOrders(t, db, nodeID); got.refills != 0 || got.returns != 0 {
		t.Fatalf("a spot Core does not have got orders (refills=%d returns=%d)", got.refills, got.returns)
	}
}

// S4: the line reads empty and the spare stands there right, so the simple
// delivery comes from the spot, and one refill replaces it.
func TestKeepStagedRequest_EmptyLineIsFedFromTheSpot(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, _ := keepStagedCell(t, protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot,
		map[string]NodeBinInfo{ksLine: {}, ksSpot: {Occupied: true, PayloadCode: ksPart}})
	res, err := eng.RequestNodeMaterial(nodeID, 1)
	testutil.MustNoErr(t, err, "request")
	if res.Order == nil || res.Order.SourceNode != ksSpot || res.Order.DeliveryNode != ksLine {
		t.Fatalf("downgrade = %+v, want a move %s -> %s", res.Order, ksSpot, ksLine)
	}
	if got := readSpotOrders(t, db, nodeID); got.refills != 1 {
		t.Errorf("refills = %d, want 1 to replace the spare the downgrade lifts", got.refills)
	}
}

func TestSpotPlan_OrderCountCountsTheSpot(t *testing.T) {
	t.Parallel()
	p := &ConsumePlan{Dispatch: &SwapDispatch{StepsA: []protocol.ComplexOrderStep{{}}, StepsB: []protocol.ComplexOrderStep{{}}},
		Spot: spotPlan{returnSpare: true, refills: 2}}
	if got := p.OrderCount(); got != 5 {
		t.Errorf("consume OrderCount = %d, want 5 (two legs, a return, two refills)", got)
	}
	pp := &ProducePlan{Dispatch: &SwapDispatch{StepsA: []protocol.ComplexOrderStep{{}}}, Spot: spotPlan{refills: 1}}
	if got := pp.OrderCount(); got != 2 {
		t.Errorf("produce OrderCount = %d, want 2", got)
	}
}

// A wrong spare goes back before anything is sent to replace it: the request
// writes the return first, then the refills, so Core's dropoff gate sees the
// spot being cleared before it sees anything bound for it.
func TestKeepStagedRequest_TheReturnIsWrittenBeforeTheRefills(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, _ := keepStagedCell(t, protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot,
		map[string]NodeBinInfo{ksLine: {Occupied: true, PayloadCode: ksPart}, ksSpot: {Occupied: true, PayloadCode: "PART-OTHER"}})
	_, err := eng.RequestNodeMaterial(nodeID, 1)
	testutil.MustNoErr(t, err, "request")
	rows, err := db.ListActiveOrdersByProcessNode(nodeID)
	testutil.MustNoErr(t, err, "rows")
	var ret int64
	var refills []int64
	for _, o := range rows {
		switch {
		case o.SourceNode == ksSpot:
			ret = o.ID
		case o.DeliveryNode == ksSpot:
			refills = append(refills, o.ID)
		}
	}
	if ret == 0 || len(refills) != 2 {
		t.Fatalf("return=%d refills=%v, want one return and two refills", ret, refills)
	}
	for _, r := range refills {
		if r < ret {
			t.Fatalf("refill %d written before the return %d", r, ret)
		}
	}
}
