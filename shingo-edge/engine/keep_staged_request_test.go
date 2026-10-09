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
// A REQUEST ASKS ONLY FOR THE LINE. Its swap takes the spare when Core says a
// right one stands on the spot (the occupancy call the request already makes,
// one more name) and the market's carrier when none does, exactly as a line
// without a spot; it orders nothing for the spot (the keeper refills it when it
// reads it bare). A wrong bin on a spot the swap stages on does not refuse the
// request: the market swap is built, and Core holds it until the spot clears, as
// on any line whose staging node is occupied. SPR ALN_011
// 2026-10-08: the refill each request created outlived the request's cancel and
// took the carrier the cancel freed. These drive RequestNodeMaterial /
// RequestProduceSwap end to end against a Core stub and read what lands in the
// order table.

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

	procID, err := db.CreateProcess("KS-PROC", "", "", "", false)
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

func TestKeepStagedRequest_AsksOnlyForTheLine(t *testing.T) {
	t.Parallel()
	occupiedLine := NodeBinInfo{Occupied: true, PayloadCode: ksPart}
	cases := []struct {
		name     string
		role     protocol.ClaimRole
		mode     protocol.SwapMode
		spot     NodeBinInfo
		wantFrom string // "spot" or "market"
	}{
		{"consume two_robot, spare right: the swap takes it", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot,
			NodeBinInfo{Occupied: true, PayloadCode: ksPart}, "spot"},
		{"consume two_robot, spot bare: the swap fetches from the market", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot,
			NodeBinInfo{}, "market"},
		{"consume two_robot, spare of another part on the staging spot: the market swap, held by Core", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot,
			NodeBinInfo{Occupied: true, PayloadCode: "PART-OTHER"}, "market"},
		{"produce two_robot, empty spare: the swap takes it", protocol.ClaimRoleProduce, protocol.SwapModeTwoRobot,
			NodeBinInfo{Occupied: true}, "spot"},
		{"produce two_robot, spot bare: the swap fetches an empty from the market", protocol.ClaimRoleProduce, protocol.SwapModeTwoRobot,
			NodeBinInfo{}, "market"},
		{"produce two_robot, a full on the staging spot: the market swap, held by Core", protocol.ClaimRoleProduce, protocol.SwapModeTwoRobot,
			NodeBinInfo{Occupied: true, PayloadCode: ksPart}, "market"},
		// A press has no staging hop: a wrong bin on its spot is in nobody's way.
		{"consume press, spare of another part: the refill leg fetches from the market", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobotPressIndex,
			NodeBinInfo{Occupied: true, PayloadCode: "PART-OTHER"}, "market"},
		{"produce press, empty spare: the refill leg takes it", protocol.ClaimRoleProduce, protocol.SwapModeTwoRobotPressIndex,
			NodeBinInfo{Occupied: true}, "spot"},
		{"produce press, spot bare: the refill leg fetches from the market", protocol.ClaimRoleProduce, protocol.SwapModeTwoRobotPressIndex,
			NodeBinInfo{}, "market"},
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
			if got := swapFetchesFrom(t, eng, db, nodeID); got != c.wantFrom {
				t.Errorf("the swap fetches from %q, want %q", got, c.wantFrom)
			}
			if got := readSpotOrders(t, db, nodeID); got.refills != 0 || got.returns != 0 {
				t.Errorf("the request ordered refills=%d returns=%d for the spot, want none", got.refills, got.returns)
			}
		})
	}
}

// swapFetchesFrom is where the line's swap legs fetch their carrier: "market"
// when a leg picks up at the inbound source, else "spot" when one picks up at
// the spot, "" when neither (a refused request creates no legs).
func swapFetchesFrom(t *testing.T, eng *Engine, db *store.DB, nodeID int64) string {
	t.Helper()
	rows, err := db.ListActiveOrdersByProcessNode(nodeID)
	testutil.MustNoErr(t, err, "rows")
	from := ""
	for _, o := range rows {
		if o.OrderType != orders.TypeComplex {
			continue
		}
		steps, err := eng.storedStepsOf(o.ID)
		testutil.MustNoErr(t, err, "steps")
		for _, s := range steps {
			if s.Action != protocol.ActionPickup {
				continue
			}
			// A market swap that stages on the spot also lifts there (the relay),
			// so a pickup at the market decides it.
			switch s.Node {
			case ksMarket:
				from = "market"
			case ksSpot:
				if from == "" {
					from = "spot"
				}
			}
		}
	}
	return from
}

// A refill already on its way to a spot the market swap would stage on: that
// carrier and the refill could not both be set down there, so the swap takes
// the spare that is coming (its pickup at the spot, which Core holds until the
// refill lands) and nothing new is ordered. A press stages nowhere near its
// spot, so it fetches from the market as usual.
func TestKeepStagedRequest_ASpareOnItsWayIsWaitedFor(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		mode     protocol.SwapMode
		wantFrom string
	}{
		{protocol.SwapModeTwoRobot, "spot"},
		{protocol.SwapModeTwoRobotPressIndex, "market"},
	} {
		t.Run(string(c.mode), func(t *testing.T) {
			t.Parallel()
			eng, db, nodeID, _ := keepStagedCell(t, protocol.ClaimRoleConsume, c.mode,
				map[string]NodeBinInfo{ksLine: {Occupied: true, PayloadCode: ksPart}, ksPair: {Occupied: true, PayloadCode: ksPart}, ksSpot: {}})
			_, err := eng.orderMgr.CreateRetrieveOrder(&nodeID, false, 1, ksSpot, ksMarket, "", "standard", ksPart, true, false,
				orders.Attached("coming"))
			testutil.MustNoErr(t, err, "a refill on its way")
			_, err = eng.RequestNodeMaterial(nodeID, 1)
			testutil.MustNoErr(t, err, "request")
			if got := swapFetchesFrom(t, eng, db, nodeID); got != c.wantFrom {
				t.Errorf("the swap fetches from %q, want %q", got, c.wantFrom)
			}
			if got := readSpotOrders(t, db, nodeID); got.refills != 1 || got.returns != 0 {
				t.Errorf("refills=%d returns=%d, want only the one already coming", got.refills, got.returns)
			}
		})
	}
}

// A spare with a live return is leaving: the request does not take it. Its swap
// fetches from the market, staging on the spot the leaving spare still stands
// on, and Core holds it until the return has lifted it.
func TestKeepStagedRequest_ALeavingSpareIsNotTaken(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, _ := keepStagedCell(t, protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot,
		map[string]NodeBinInfo{ksLine: {Occupied: true, PayloadCode: ksPart}, ksSpot: {Occupied: true, PayloadCode: ksPart}})
	_, err := eng.orderMgr.CreateMoveOrderForBin(&nodeID, ksSpot, ksMarket, ksPart, 0, orders.NoDemand())
	testutil.MustNoErr(t, err, "the spare's return")
	_, err = eng.RequestNodeMaterial(nodeID, 1)
	testutil.MustNoErr(t, err, "request")
	if got := swapFetchesFrom(t, eng, db, nodeID); got != "market" {
		t.Errorf("the swap fetches from %q, want the market", got)
	}
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
// delivery comes from the spot. The request orders nothing to replace it: the
// keeper does, once the spare is lifted.
func TestKeepStagedRequest_EmptyLineIsFedFromTheSpot(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, _ := keepStagedCell(t, protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot,
		map[string]NodeBinInfo{ksLine: {}, ksSpot: {Occupied: true, PayloadCode: ksPart}})
	res, err := eng.RequestNodeMaterial(nodeID, 1)
	testutil.MustNoErr(t, err, "request")
	if res.Order == nil || res.Order.SourceNode != ksSpot || res.Order.DeliveryNode != ksLine {
		t.Fatalf("downgrade = %+v, want a move %s -> %s", res.Order, ksSpot, ksLine)
	}
	if got := readSpotOrders(t, db, nodeID); got.refills != 0 {
		t.Errorf("refills = %d, want none from the request", got.refills)
	}
}
