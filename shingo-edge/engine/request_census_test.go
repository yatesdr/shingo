package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// WHAT EACH BUTTON MAKES, FOR EVERY ROLE AND SWAP MODE, ON A LINE WITH A BIN
// AND ON ONE WITHOUT.
//
// A line with no bin on it has nothing to lift, so whatever the role and the
// mode the answer is one plain order bringing a bin to it: a full of the part
// to a consume line, an empty to a produce line, plus an empty or a full for
// each bare position of a press. Each row drives one request end to end against
// a Core stub that answers node-bins and counts every call, and records the
// orders the line then holds, the Core round trips the request made and the
// orders its demand episode expects.

const (
	censusDeck = "KS-DECK"

	doorMaterial = "material request"
	doorProduce  = "produce request"
	doorEmptyBin = "empty-bin request"

	lineBare     = "bare"
	lineOccupied = "occupied"
	// lineDeckBare is a press whose head holds a bin and whose paired position
	// does not.
	lineDeckBare = "head occupied, paired bare"
)

// censusResult is what one request left behind.
type censusResult struct {
	legs     int    // swap legs (complex orders)
	toHead   int    // plain orders bound for the line's own position
	toDeck   int    // plain orders bound for the press's paired position
	trips    int    // Core round trips the request made
	expected int    // the demand episode's expected orders; 0 when none opened
	refused  string // the refusal, "" when the request went through
}

// censusStub answers node-bins for the named nodes, occupied exactly when
// listed, and counts every call made to it.
func censusStub(t *testing.T, calls *atomic.Int32, occupied ...string) *httptest.Server {
	t.Helper()
	occ := map[string]bool{}
	for _, n := range occupied {
		occ[n] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		out := []NodeBinInfo{}
		if r.URL.Path == "/api/telemetry/node-bins" {
			for _, n := range strings.Split(r.URL.Query().Get("nodes"), ",") {
				if n != "" {
					out = append(out, NodeBinInfo{NodeName: n, Occupied: occ[n]})
				}
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedCensusCell is one line of the given role and mode with the counted parts
// given, and a press's paired position when the mode has one.
func seedCensusCell(t *testing.T, role protocol.ClaimRole, mode protocol.SwapMode, uop int) (*Engine, *store.DB, int64) {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.logFn = func(string, ...any) {}
	procID, err := db.CreateProcess("CENSUS-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: ksLine, Code: "CN1", Name: ksLine, Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "node")
	styleID, err := db.CreateStyle("CENSUS-STYLE", "", procID)
	testutil.MustNoErr(t, err, "style")
	testutil.MustNoErr(t, db.SetActiveStyle(procID, &styleID), "active style")
	in := processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: ksLine, Role: role, SwapMode: mode, PayloadCode: ksPart,
		UOPCapacity: 40, InboundSource: ksMarket, InboundStaging: ksSpot, OutboundDestination: ksDest,
	}
	switch mode {
	case protocol.SwapModeSingleRobot:
		in.OutboundStaging = "KS-OUT"
	case protocol.SwapModeTwoRobotPressIndex:
		in.PairedCoreNode = censusDeck
	}
	claimID, err := db.UpsertStyleNodeClaim(in)
	testutil.MustNoErr(t, err, "claim")
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, uop), "runtime claim")
	return eng, db, nodeID
}

// runCensusRow seeds the line, presses the door and reads what it left.
func runCensusRow(t *testing.T, role protocol.ClaimRole, mode protocol.SwapMode, door, line string, uop int) censusResult {
	t.Helper()
	eng, db, nodeID := seedCensusCell(t, role, mode, uop)
	var occupied []string
	switch line {
	case lineOccupied:
		occupied = []string{ksLine, censusDeck}
	case lineDeckBare:
		occupied = []string{ksLine}
	}
	var calls atomic.Int32
	eng.coreClient = NewCoreClient(censusStub(t, &calls, occupied...).URL)

	var err error
	switch door {
	case doorMaterial:
		_, err = eng.RequestNodeMaterial(nodeID, 1)
	case doorProduce:
		_, err = eng.RequestProduceSwap(nodeID)
	case doorEmptyBin:
		_, err = eng.RequestEmptyBin(nodeID, ksPart)
	}
	got := censusResult{trips: int(calls.Load())}
	if err != nil {
		got.refused = err.Error()
	}
	rows, rerr := db.ListActiveOrdersByProcessNode(nodeID)
	testutil.MustNoErr(t, rerr, "the line's rows")
	for _, o := range rows {
		switch {
		case o.OrderType == orders.TypeComplex:
			got.legs++
		case o.DeliveryNode == ksLine:
			got.toHead++
		case o.DeliveryNode == censusDeck:
			got.toDeck++
		default:
			t.Errorf("an order the census does not place: %d %s %s->%s", o.ID, o.OrderType, o.SourceNode, o.DeliveryNode)
		}
	}
	open, oerr := db.ListOpenDemandOrigins()
	testutil.MustNoErr(t, oerr, "episodes")
	for _, o := range open {
		if o.ExpectedOrders != nil {
			got.expected = *o.ExpectedOrders
		}
	}
	return got
}

// TestRequestCensus pins, row by row, what each button makes today. A row
// marked wrong is what a bare line gets that it should not, and names the
// ruling or the fault.
func TestRequestCensus(t *testing.T) {
	t.Parallel()
	const (
		sr  = protocol.SwapModeSingleRobot
		tr  = protocol.SwapModeTwoRobot
		seq = protocol.SwapModeSequential
		pi  = protocol.SwapModeTwoRobotPressIndex
	)
	consume, produce := protocol.ClaimRoleConsume, protocol.ClaimRoleProduce
	noParts := "has no parts to finalize"
	rows := []struct {
		role  protocol.ClaimRole
		mode  protocol.SwapMode
		door  string
		line  string
		uop   int
		want  censusResult
		wrong string
	}{
		// The material request: one plain full to a bare line in every mode,
		// and a full for each bare position of a press.
		{consume, sr, doorMaterial, lineBare, 30, censusResult{toHead: 1, trips: 1, expected: 1}, ""},
		{consume, sr, doorMaterial, lineOccupied, 30, censusResult{legs: 1, trips: 1, expected: 1}, ""},
		{consume, tr, doorMaterial, lineBare, 30, censusResult{toHead: 1, trips: 1, expected: 1}, ""},
		{consume, tr, doorMaterial, lineOccupied, 30, censusResult{legs: 2, trips: 2, expected: 2}, ""},
		{consume, seq, doorMaterial, lineBare, 30, censusResult{toHead: 1, trips: 1, expected: 1}, ""},
		{consume, seq, doorMaterial, lineOccupied, 30, censusResult{legs: 1, trips: 1, expected: 1}, ""},
		{consume, pi, doorMaterial, lineBare, 30, censusResult{toHead: 1, toDeck: 1, trips: 1, expected: 2}, ""},
		{consume, pi, doorMaterial, lineOccupied, 30, censusResult{legs: 2, trips: 2, expected: 2}, ""},
		{consume, pi, doorMaterial, lineDeckBare, 30, censusResult{legs: 2, trips: 2, expected: 2}, ""},

		// The produce request.
		{produce, sr, doorProduce, lineBare, 30, censusResult{toHead: 1, trips: 1, expected: 1}, ""},
		{produce, sr, doorProduce, lineBare, 0, censusResult{toHead: 1, trips: 1, expected: 1}, ""},
		{produce, sr, doorProduce, lineOccupied, 30, censusResult{legs: 1, trips: 1, expected: 1}, ""},
		{produce, sr, doorProduce, lineOccupied, 0, censusResult{trips: 1, refused: noParts}, ""},
		{produce, tr, doorProduce, lineBare, 30, censusResult{legs: 2, trips: 1, expected: 2},
			"a swap whose removal Core skips; a bare line gets one plain empty"},
		{produce, tr, doorProduce, lineBare, 0, censusResult{trips: 1, refused: noParts},
			"refused; a bare line at a count of 0 gets the empty"},
		{produce, tr, doorProduce, lineOccupied, 30, censusResult{legs: 2, trips: 1, expected: 2}, ""},
		{produce, seq, doorProduce, lineBare, 30, censusResult{legs: 1, trips: 1, expected: 1},
			"a removal Core skips and no backfill: nothing is delivered"},
		{produce, seq, doorProduce, lineBare, 0, censusResult{trips: 1, refused: noParts},
			"refused; a bare line at a count of 0 gets the empty"},
		{produce, seq, doorProduce, lineOccupied, 30, censusResult{legs: 1, trips: 1, expected: 1}, ""},
		{produce, pi, doorProduce, lineBare, 30, censusResult{legs: 2, trips: 1, expected: 2},
			"a swap whose index leg holds for good at the bare paired position"},
		{produce, pi, doorProduce, lineBare, 0, censusResult{trips: 1, refused: noParts},
			"refused; a bare line at a count of 0 gets the empty"},
		{produce, pi, doorProduce, lineOccupied, 30, censusResult{legs: 2, trips: 1, expected: 2}, ""},
		{produce, pi, doorProduce, lineDeckBare, 0, censusResult{toDeck: 1, trips: 1, expected: 1}, ""},

		// The empty-bin request, pressed at a count of 0 as the station offers it.
		{produce, sr, doorEmptyBin, lineBare, 0, censusResult{legs: 1, expected: 1},
			"a swap whose lift holds at Core for good"},
		{produce, sr, doorEmptyBin, lineOccupied, 0, censusResult{legs: 1, expected: 1}, ""},
		{produce, tr, doorEmptyBin, lineBare, 0, censusResult{legs: 2, expected: 1},
			"a swap whose removal Core skips; a bare line gets one plain empty"},
		{produce, tr, doorEmptyBin, lineOccupied, 0, censusResult{legs: 2, expected: 1}, ""},
		{produce, seq, doorEmptyBin, lineBare, 0, censusResult{legs: 1, expected: 1},
			"a removal Core skips and no backfill: nothing is delivered"},
		{produce, seq, doorEmptyBin, lineOccupied, 0, censusResult{legs: 1, expected: 1}, ""},
		{produce, pi, doorEmptyBin, lineBare, 0, censusResult{legs: 2, trips: 1, expected: 1},
			"a swap whose index leg holds for good at the bare paired position"},
		{produce, pi, doorEmptyBin, lineOccupied, 0, censusResult{legs: 2, trips: 1, expected: 1}, ""},
		{produce, pi, doorEmptyBin, lineDeckBare, 0, censusResult{toDeck: 1, trips: 1, expected: 1}, ""},
	}
	for _, r := range rows {
		name := strings.Join([]string{string(r.role), string(r.mode), r.door, r.line}, "/")
		if r.uop == 0 {
			name += "/count 0"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := runCensusRow(t, r.role, r.mode, r.door, r.line, r.uop)
			refusedOK := r.want.refused == "" && got.refused == "" ||
				r.want.refused != "" && strings.Contains(got.refused, r.want.refused)
			g, w := got, r.want
			g.refused, w.refused = "", ""
			if g != w || !refusedOK {
				t.Errorf("got %+v refused %q, want %+v refused %q", g, got.refused, w, r.want.refused)
			}
			if r.wrong != "" {
				t.Logf("WRONG TODAY: %s", r.wrong)
			}
		})
	}
}
