package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
)

// A PARK SET DOWN AFTER THE CANCEL'S READ.
//
// A cancelled single-robot changeover finishes the parked bin's trip from what
// Core says stands on outbound staging at the cancel. The cancel to Core is
// queued, the read is not: a robot still carrying the line's bin can set it down
// on outbound staging after the read answered empty, and nothing then moves it.
// Every later single-robot swap at the line parks there and waits behind it.
//
// So a single-robot request asks about the claim's outbound staging in the
// node-bins read it already makes, and a bin standing there, with nothing of the
// line's coming for it, gets the same plain move on to the outbound destination
// before the swap is built.

const ksOut = "KS-OUT"

// countingNodeBinsStub is ksNodeBinsStub that records every node-bins call.
func countingNodeBinsStub(t *testing.T, rows map[string]NodeBinInfo) (*httptest.Server, func() [][]string) {
	t.Helper()
	var mu sync.Mutex
	var calls [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := []NodeBinInfo{}
		if r.URL.Path == "/api/telemetry/node-bins" {
			names := strings.Split(r.URL.Query().Get("nodes"), ",")
			mu.Lock()
			calls = append(calls, names)
			mu.Unlock()
			for _, n := range names {
				if n == "" {
					continue
				}
				row := rows[n]
				row.NodeName = n
				out = append(out, row)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv, func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return append([][]string(nil), calls...)
	}
}

// movesOff is every live plain move off a node on the line.
func movesOff(t *testing.T, eng *Engine, nodeID int64, from string) []domain.Order {
	t.Helper()
	rows, err := eng.db.ListActiveOrdersByProcessNode(nodeID)
	testutil.MustNoErr(t, err, "rows")
	var out []domain.Order
	for _, o := range rows {
		if o.OrderType == orders.TypeMove && o.SourceNode == from {
			out = append(out, o)
		}
	}
	return out
}

func requestFor(eng *Engine, role protocol.ClaimRole, nodeID int64) error {
	if role == protocol.ClaimRoleProduce {
		_, err := eng.RequestProduceSwap(nodeID)
		return err
	}
	_, err := eng.RequestNodeMaterial(nodeID, 1)
	return err
}

func TestSingleRobotRequest_MovesABinLeftOnOutboundStaging(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		role     protocol.ClaimRole
		line     NodeBinInfo
		wantLegs int
	}{
		{"consume", protocol.ClaimRoleConsume, NodeBinInfo{Occupied: true, PayloadCode: ksPart}, 1},
		{"produce", protocol.ClaimRoleProduce, NodeBinInfo{Occupied: true, PayloadCode: ksPart}, 1},
		{"consume, line empty", protocol.ClaimRoleConsume, NodeBinInfo{}, 0},
		{"produce, line empty", protocol.ClaimRoleProduce, NodeBinInfo{}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eng, _, nodeID, _ := seedCell(t, c.role, protocol.SwapModeSingleRobot, false, nil)
			srv, calls := countingNodeBinsStub(t, map[string]NodeBinInfo{
				ksLine: c.line, ksOut: {Occupied: true, PayloadCode: "PART-PARKED"}})
			eng.coreClient = stubCoreClient(srv.URL)

			testutil.MustNoErr(t, requestFor(eng, c.role, nodeID), "request")

			moves := movesOff(t, eng, nodeID, ksOut)
			if len(moves) != 1 {
				t.Fatalf("moves off outbound staging = %d, want 1", len(moves))
			}
			m := moves[0]
			if m.DeliveryNode != ksDest || m.PayloadCode != "PART-PARKED" || !m.AutoConfirm {
				t.Errorf("the move goes %s->%s carrying %q autoConfirm=%v, want ->%s carrying PART-PARKED, auto-confirmed",
					m.SourceNode, m.DeliveryNode, m.PayloadCode, m.AutoConfirm, ksDest)
			}
			legs, _ := liveLineRows(t, eng, nodeID)
			if len(legs) != c.wantLegs {
				t.Errorf("swap legs = %d, want %d", len(legs), c.wantLegs)
			}
			rt, err := eng.db.GetProcessNodeRuntime(nodeID)
			testutil.MustNoErr(t, err, "runtime")
			for _, slot := range []*int64{rt.ActiveOrderID, rt.StagedOrderID} {
				if slot != nil && *slot == m.ID {
					t.Errorf("the move %d sits in the line's runtime slots", m.ID)
				}
			}
			// One read, the one the request already makes, with outbound staging in it.
			got := calls()
			if len(got) != 1 || !containsName(got[0], ksOut) {
				t.Errorf("node-bins calls = %v, want one that asks about %s", got, ksOut)
			}
		})
	}
}

// What is already going for it is not sent twice: the cancel's own move, or an
// earlier request's.
func TestSingleRobotRequest_ABinAlreadyLeavingIsLeft(t *testing.T) {
	t.Parallel()
	eng, _, nodeID, _ := seedCell(t, protocol.ClaimRoleConsume, protocol.SwapModeSingleRobot, false,
		map[string]NodeBinInfo{ksLine: {Occupied: true, PayloadCode: ksPart}, ksOut: {Occupied: true, PayloadCode: "PART-PARKED"}})
	_, err := eng.orderMgr.CreateMoveOrderCarrying(&nodeID, ksOut, ksDest, "PART-PARKED", orders.Attached("cancel"))
	testutil.MustNoErr(t, err, "the cancel's move")
	testutil.MustNoErr(t, requestFor(eng, protocol.ClaimRoleConsume, nodeID), "request")
	if moves := movesOff(t, eng, nodeID, ksOut); len(moves) != 1 {
		t.Fatalf("moves off outbound staging = %d, want the one already going", len(moves))
	}
}

// A two-robot request is not changed: it neither asks about outbound staging
// nor moves what stands there.
func TestTwoRobotRequest_LeavesOutboundStagingAlone(t *testing.T) {
	t.Parallel()
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
		t.Run(string(role), func(t *testing.T) {
			t.Parallel()
			eng, db, nodeID, claim := seedCell(t, role, protocol.SwapModeTwoRobot, false, nil)
			_, err := db.DB.Exec(`UPDATE style_node_claims SET outbound_staging=? WHERE id=?`, ksOut, claim.ID)
			testutil.MustNoErr(t, err, "outbound staging on a two-robot claim")
			srv, calls := countingNodeBinsStub(t, map[string]NodeBinInfo{
				ksLine: {Occupied: true, PayloadCode: ksPart}, ksOut: {Occupied: true, PayloadCode: "PART-PARKED"}})
			eng.coreClient = stubCoreClient(srv.URL)

			testutil.MustNoErr(t, requestFor(eng, role, nodeID), "request")

			if moves := movesOff(t, eng, nodeID, ksOut); len(moves) != 0 {
				t.Fatalf("moves off outbound staging = %d, want none", len(moves))
			}
			for _, names := range calls() {
				if containsName(names, ksOut) {
					t.Errorf("a two-robot request asked about %s: %v", ksOut, names)
				}
			}
		})
	}
}

func containsName(names []string, n string) bool {
	for _, x := range names {
		if x == n {
			return true
		}
	}
	return false
}
