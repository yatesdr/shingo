package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/engine/changeover"
	"shingoedge/orders"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// A KEEP-STAGED SPOT THROUGH A CHANGEOVER, EVERY SHAPE: what Start, Preview,
// Cancel and the cutover do at the spot, for every configurable swap mode, both
// roles, the four things Core can say about the spot, the spot as the inbound
// staging node or not, and a line that is swapped or added.
//
// Each row is one observation string:
//
//	legs=   the changeover's legs at the line, at the spot: lift, drop or -
//	ret/ref the spot orders the start made: returns off it, refills to it
//	exp     the episode's expected_orders; n the number of legs
//	prev=   the legs Preview shows, at the spot
//	cancel  the live spot orders after the changeover is cancelled at once
//	paused  whether the keeper reads paused after the start and after cutover
//
// The legs are named by tokens (pinLegs): S takes the spare, D sets down on the
// spot, R collects again what it set down there, M fetches from the market.
//
// Each row carries two answers. today is the tree before one refill owner (brief
// A2: the keeper the only refill owner, requestClaim the one leg door, an unknown
// spot read refused at START); after is the prediction under it, written before
// the source changed. The single-robot swap rows' after lost one leg on
// 2026-10-16, when that changeover became one order (no stage order). The test asserts today until that change lands, and
// after from then on; every row whose after differs from today is a ruling.

const pinSpot = "SPOT"

type pinRead string

const (
	readBare    pinRead = "bare"
	readRight   pinRead = "right"
	readWrong   pinRead = "wrong"
	readUnknown pinRead = "unknown"
)

type pinCase struct {
	role      protocol.ClaimRole
	mode      protocol.SwapMode
	read      pinRead
	staging   bool // the spot is the incoming claim's inbound staging
	add       bool // the line is added by the incoming style (no outgoing claim)
	spotRow   *NodeBinInfo
	queuedRef bool // a keeper refill is queued at the spot when START is pressed
}

func (c pinCase) name() string {
	stg := "spot!=staging"
	if c.staging {
		stg = "spot==staging"
	}
	sit := "swap"
	if c.add {
		sit = "add"
	}
	n := fmt.Sprintf("%s/%s/%s/%s/%s", c.role, c.mode, c.read, stg, sit)
	if c.queuedRef {
		n += "/queued-refill"
	}
	return n
}

// spotRowFor is what Core says stands on the spot. Right is the incoming
// claim's spare: a full of its part for consume, an unstamped empty for
// produce. Wrong is a full of the outgoing part, which neither incoming role
// takes. Unknown is Core not answering for the spot.
func (c pinCase) spotRowFor() (NodeBinInfo, bool) {
	if c.spotRow != nil {
		return *c.spotRow, true
	}
	switch c.read {
	case readBare:
		return NodeBinInfo{}, true
	case readRight:
		if c.role == protocol.ClaimRoleConsume {
			return NodeBinInfo{Occupied: true, PayloadCode: "PART-NEW", BinID: 7}, true
		}
		return NodeBinInfo{Occupied: true, BinID: 7}, true
	case readWrong:
		return NodeBinInfo{Occupied: true, PayloadCode: "PART-OLD", BinID: 7}, true
	}
	return NodeBinInfo{}, false
}

// pinNodeBinsStub answers node-bins from rows, leaving out the names in omit.
func pinNodeBinsStub(t *testing.T, rows map[string]NodeBinInfo, omit map[string]bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := []NodeBinInfo{}
		if r.URL.Path == "/api/telemetry/node-bins" {
			for _, n := range strings.Split(r.URL.Query().Get("nodes"), ",") {
				if n == "" || omit[n] {
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
	return srv
}

func (c pinCase) seed(t *testing.T) *coFixture {
	t.Helper()
	var from []coClaim
	if !c.add {
		from = []coClaim{{"L1", pinSpot, "SRC-OLD", "PART-OLD", c.role, c.mode, true, false}}
	}
	to := []coClaim{{"L1", pinSpot, "SRC-NEW", "PART-NEW", c.role, c.mode, true, false}}
	rows := map[string]NodeBinInfo{}
	if !c.add {
		rows["L1"] = NodeBinInfo{Occupied: true, PayloadCode: "PART-OLD"}
	}
	omit := map[string]bool{}
	if row, ok := c.spotRowFor(); ok {
		rows[pinSpot] = row
	} else {
		omit[pinSpot] = true
	}
	fx := seedKeepStagedChangeover(t, from, to, rows)
	fx.eng.coreClient = stubCoreClient(pinNodeBinsStub(t, rows, omit).URL)
	omitted := []string{}
	for n := range omit {
		omitted = append(omitted, n)
	}
	fx.holdRows(t, rows, omitted...)
	if !c.staging {
		_, err := fx.db.DB.Exec(`UPDATE style_node_claims SET inbound_staging='STAGE' WHERE inbound_staging=?`, pinSpot)
		testutil.MustNoErr(t, err, "move the staging off the spot")
	}
	return fx
}

// pinLegs is what a set of legs does at the spot, as tokens in a fixed order:
//
//	S  takes the spare: a pickup at the spot when no leg sets a carrier down there
//	D  sets a carrier down on the spot
//	R  collects from the spot what a leg set down there: in the same order (a
//	   relay, naming nothing) or, single robot, the swap collecting what its
//	   stage order parked (naming the part, because it is another order)
//	M  fetches from the incoming claim's inbound source (the market)
//
// "-" when the legs neither touch the spot nor fetch from the market.
func pinLegs(steps [][]protocol.ComplexOrderStep) string {
	var lift, drop, market bool
	for _, legSteps := range steps {
		for _, st := range legSteps {
			switch {
			case st.Node == pinSpot && st.Action == protocol.ActionPickup:
				lift = true
			case st.Node == pinSpot && st.Action == protocol.ActionDropoff:
				drop = true
			case st.Node == "SRC-NEW" && st.Action == protocol.ActionPickup:
				market = true
			}
		}
	}
	var out []string
	for _, tok := range []struct {
		on bool
		s  string
	}{{lift && !drop, "S"}, {drop, "D"}, {lift && drop, "R"}, {market, "M"}} {
		if tok.on {
			out = append(out, tok.s)
		}
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, "+")
}

type pinSpotOrders struct{ ret, ref int }

func pinReadSpotOrders(t *testing.T, fx *coFixture) pinSpotOrders {
	got := readSpotTraffic(t, fx, pinSpot)
	n := 0
	for _, v := range got.refills {
		n += v
	}
	return pinSpotOrders{ret: len(got.returns), ref: n}
}

// pinLegSteps is every live order of the changeover but the spot's own traffic:
// a complex leg by its steps, a plain order as its pickup and its dropoff.
func pinLegSteps(t *testing.T, fx *coFixture) [][]protocol.ComplexOrderStep {
	t.Helper()
	live, err := fx.db.ListActiveOrders()
	testutil.MustNoErr(t, err, "orders")
	var out [][]protocol.ComplexOrderStep
	for _, o := range live {
		if o.OrderType != orders.TypeComplex {
			if o.DeliveryNode == pinSpot || o.SourceNode == pinSpot {
				continue // a refill or a return: counted by pinReadSpotOrders
			}
			out = append(out, []protocol.ComplexOrderStep{
				{Action: protocol.ActionPickup, Node: o.SourceNode, PayloadCode: o.PayloadCode},
				{Action: protocol.ActionDropoff, Node: o.DeliveryNode},
			})
			continue
		}
		steps, err := fx.eng.storedStepsOf(o.ID)
		testutil.MustNoErr(t, err, "steps")
		out = append(out, steps)
	}
	return out
}

func pinPaused(t *testing.T, fx *coFixture, styleID int64) bool {
	t.Helper()
	claim, err := fx.db.GetStyleNodeClaimByNode(styleID, "L1")
	testutil.MustNoErr(t, err, "claim")
	rows, err := storeorders.ListKeptSpotRows(fx.db.DB, fx.nodeIDs["L1"], pinSpot, "L1")
	testutil.MustNoErr(t, err, "rows")
	resumed, err := processes.KeepStagedResumedAt(fx.db.DB, fx.nodeIDs["L1"])
	testutil.MustNoErr(t, err, "resume stamp")
	return spotPause(rows, claim, resumed) != ""
}

// queueKeeperRefill is the keeper's own refill, queued at the spot for the
// outgoing claim when START is pressed.
func queueKeeperRefill(t *testing.T, fx *coFixture) {
	t.Helper()
	p, err := fx.db.GetProcess(fx.processID)
	testutil.MustNoErr(t, err, "process")
	claim, err := fx.db.GetStyleNodeClaimByNode(*p.ActiveStyleID, "L1")
	testutil.MustNoErr(t, err, "outgoing claim")
	node, err := fx.db.GetProcessNode(fx.nodeIDs["L1"])
	testutil.MustNoErr(t, err, "node")
	fx.eng.refillSpot(node, claim, 1, orders.NoDemand())
}

// sameSecondAsTheAbort stamps every order the start created with the second the
// start's aborts were written in, as at the plant, where the legs land in the
// abort's own second. The keeper's pause reads the order of those stamps.
func sameSecondAsTheAbort(t *testing.T, fx *coFixture) {
	t.Helper()
	_, err := fx.db.DB.Exec(`UPDATE orders SET created_at = (SELECT MAX(updated_at) FROM orders WHERE status = ?)
		WHERE status != ?`, orders.StatusCancelled, orders.StatusCancelled)
	testutil.MustNoErr(t, err, "same second")
}

// cutOver finishes every task and leg and cuts over.
func cutOver(t *testing.T, fx *coFixture, coID int64) {
	t.Helper()
	tasks, err := fx.db.ListChangeoverNodeTasks(coID)
	testutil.MustNoErr(t, err, "tasks")
	for _, task := range tasks {
		testutil.MustNoErr(t, fx.db.UpdateChangeoverNodeTaskState(task.ID, domain.NodeTaskSwitched), "task done")
		for _, id := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
			if id != nil {
				testutil.MustNoErr(t, fx.db.UpdateOrderStatus(*id, string(protocol.StatusConfirmed)), "leg done")
			}
		}
	}
	testutil.MustNoErr(t, fx.eng.CompleteProcessProductionCutover(fx.processID), "cutover")
}

// observe runs the case three times — Start then cutover, Start then Cancel, and
// Preview — each on a fresh fixture, and returns the row.
func (c pinCase) observe(t *testing.T) string {
	t.Helper()
	// Preview.
	pv := c.seed(t)
	prev := "refused"
	if plan, err := pv.eng.PreviewChangeoverPlan(pv.processID, pv.toStyleID); err == nil {
		var steps [][]protocol.ComplexOrderStep
		for _, a := range plan.Actions {
			for _, spec := range []*changeover.OrderSpec{a.SupplyOrder, a.EvacOrder} {
				steps = append(steps, pinSpecSteps(spec))
			}
		}
		prev = pinLegs(steps)
	}

	// Start, then cutover.
	fx := c.seed(t)
	if c.queuedRef {
		queueKeeperRefill(t, fx)
	}
	co, err := fx.eng.StartProcessChangeover(fx.processID, fx.toStyleID, "test", "pin")
	if err != nil {
		return "start refused | prev " + prev
	}
	if c.queuedRef {
		sameSecondAsTheAbort(t, fx)
	}
	steps := pinLegSteps(t, fx)
	spot := pinReadSpotOrders(t, fx)
	open, err := fx.db.GetOpenDemandOrigin(protocol.ChangeoverEpisodeKey(fx.eng.cfg.StationID(), co.ID))
	testutil.MustNoErr(t, err, "episode")
	exp := -1
	if open != nil && open.ExpectedOrders != nil {
		exp = *open.ExpectedOrders
	}
	pausedStart := pinPaused(t, fx, fx.toStyleID)
	cutOver(t, fx, co.ID)
	pausedCut := pinPaused(t, fx, fx.toStyleID)

	// Start, then cancel at once.
	cx := c.seed(t)
	if c.queuedRef {
		queueKeeperRefill(t, cx)
	}
	_, err = cx.eng.StartProcessChangeover(cx.processID, cx.toStyleID, "test", "pin")
	testutil.MustNoErr(t, err, "start for the cancel")
	testutil.MustNoErr(t, cx.eng.CancelProcessChangeover(cx.processID), "cancel")
	after := pinReadSpotOrders(t, cx)

	yn := map[bool]string{true: "y", false: "n"}
	return fmt.Sprintf("legs=%s ret=%d ref=%d exp=%d n=%d | prev %s | cancel ret=%d ref=%d | paused start=%s cut=%s",
		pinLegs(steps), spot.ret, spot.ref, exp, len(steps), prev,
		after.ret, after.ref, yn[pausedStart], yn[pausedCut])
}

// pinSpecSteps is a planned leg's steps, a plain retrieve as its pickup and
// dropoff, as pinLegSteps reads a created one.
func pinSpecSteps(spec *changeover.OrderSpec) []protocol.ComplexOrderStep {
	if spec != nil && spec.Retrieve != nil {
		return []protocol.ComplexOrderStep{
			{Action: protocol.ActionPickup, Node: spec.Retrieve.SourceNode, PayloadCode: spec.Retrieve.PayloadCode},
			{Action: protocol.ActionDropoff, Node: spec.Retrieve.DeliveryNode},
		}
	}
	return specSteps(spec)
}

// pinCases is every shape: the configurable modes, both roles, the four reads,
// the spot as staging where the mode stages, swap and add.
func pinCases() []pinCase {
	var out []pinCase
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
		for _, mode := range protocol.ConfigurableSwapModes() {
			stagings := []bool{true, false}
			if mode == protocol.SwapModeSequential || mode == protocol.SwapModeTwoRobotPressIndex {
				stagings = []bool{false} // no staging hop in these modes
			}
			for _, staging := range stagings {
				for _, read := range []pinRead{readBare, readRight, readWrong, readUnknown} {
					for _, add := range []bool{false, true} {
						out = append(out, pinCase{role: role, mode: mode, read: read, staging: staging, add: add})
					}
				}
			}
		}
	}
	return out
}

// TestKeepStagedChangeover_EveryShapePin is the characterisation: each shape's
// row as the tree answers it.
func TestKeepStagedChangeover_EveryShapePin(t *testing.T) {
	t.Parallel()
	cases := map[string]pinCase{}
	for _, c := range pinCases() {
		cases[c.name()] = c
	}
	for _, c := range pinExtraCases() {
		cases[c.name()] = c
	}
	if os.Getenv("KS_PIN_PRINT") != "" {
		for name, c := range cases {
			fmt.Printf("PIN\t%s\t%s\n", name, c.observe(t))
		}
		return
	}
	for name, row := range keepStagedChangeoverPin {
		c, ok := cases[name]
		if !ok {
			t.Errorf("pin row %q names no case", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := c.observe(t); got != row.after {
				t.Errorf("\n got  %s\n want %s", got, row.after)
			}
		})
	}
	if len(keepStagedChangeoverPin) != len(cases) {
		t.Errorf("%d pin rows for %d cases", len(keepStagedChangeoverPin), len(cases))
	}
}

// pinExtraCases are the shapes the matrix does not reach.
func pinExtraCases() []pinCase {
	empty := NodeBinInfo{Occupied: true, BinID: 137}
	out := []pinCase{
		// SPR ALN_011, 2026-10-15 08:31: a consume incoming claim, an unstamped
		// empty on the spot, which is the inbound staging. The spare is wrong for
		// the claim, and today's leg lifts it anyway.
		{role: protocol.ClaimRoleConsume, mode: protocol.SwapModeTwoRobot, read: "empty-0831", staging: true, spotRow: &empty},
	}
	// A START that aborts the keeper's queued refill, every mode: is the keeper
	// paused afterwards?
	for _, mode := range protocol.ConfigurableSwapModes() {
		out = append(out, pinCase{role: protocol.ClaimRoleProduce, mode: mode, read: readBare, queuedRef: true,
			staging: mode == protocol.SwapModeTwoRobot || mode == protocol.SwapModeSingleRobot})
	}
	return out
}

type pinRow struct{ today, after string }

var keepStagedChangeoverPin = map[string]pinRow{
	"consume/sequential/bare/spot!=staging/add": {
		"legs=M ret=0 ref=1 exp=2 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"consume/sequential/bare/spot!=staging/swap": {
		"legs=S ret=0 ref=2 exp=3 n=1 | prev S | cancel ret=0 ref=1 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"consume/sequential/right/spot!=staging/add": {
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"consume/sequential/right/spot!=staging/swap": {
		"legs=S ret=0 ref=1 exp=2 n=1 | prev S | cancel ret=1 ref=1 | paused start=n cut=n",
		"legs=S ret=0 ref=0 exp=1 n=1 | prev S | cancel ret=1 ref=0 | paused start=n cut=n"},
	"consume/sequential/unknown/spot!=staging/add": {
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"consume/sequential/unknown/spot!=staging/swap": {
		"legs=S ret=0 ref=0 exp=1 n=1 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"consume/sequential/wrong/spot!=staging/add": {
		"legs=M ret=1 ref=1 exp=3 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=1 ref=0 exp=2 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"consume/sequential/wrong/spot!=staging/swap": {
		"legs=S ret=1 ref=2 exp=4 n=1 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=M ret=1 ref=0 exp=2 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"consume/single_robot/bare/spot!=staging/add": {
		"legs=M ret=0 ref=1 exp=2 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"consume/single_robot/bare/spot!=staging/swap": {
		"legs=S ret=0 ref=2 exp=4 n=2 | prev S | cancel ret=0 ref=1 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"consume/single_robot/bare/spot==staging/add": {
		"legs=M ret=0 ref=1 exp=2 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"consume/single_robot/bare/spot==staging/swap": {
		"legs=S ret=0 ref=2 exp=3 n=1 | prev S | cancel ret=0 ref=1 | paused start=n cut=n",
		"legs=D+R+M ret=0 ref=0 exp=1 n=1 | prev D+R+M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"consume/single_robot/right/spot!=staging/add": {
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"consume/single_robot/right/spot!=staging/swap": {
		"legs=S ret=0 ref=1 exp=3 n=2 | prev S | cancel ret=1 ref=1 | paused start=n cut=n",
		"legs=S ret=0 ref=0 exp=1 n=1 | prev S | cancel ret=1 ref=0 | paused start=n cut=n"},
	"consume/single_robot/right/spot==staging/add": {
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"consume/single_robot/right/spot==staging/swap": {
		"legs=S ret=0 ref=1 exp=2 n=1 | prev S | cancel ret=1 ref=1 | paused start=n cut=n",
		"legs=S ret=0 ref=0 exp=1 n=1 | prev S | cancel ret=1 ref=0 | paused start=n cut=n"},
	"consume/single_robot/unknown/spot!=staging/add": {
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"consume/single_robot/unknown/spot!=staging/swap": {
		"legs=S ret=0 ref=0 exp=2 n=2 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"consume/single_robot/unknown/spot==staging/add": {
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"consume/single_robot/unknown/spot==staging/swap": {
		"legs=S ret=0 ref=0 exp=1 n=1 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"consume/single_robot/wrong/spot!=staging/add": {
		"legs=M ret=1 ref=1 exp=3 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=1 ref=0 exp=2 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"consume/single_robot/wrong/spot!=staging/swap": {
		"legs=S ret=1 ref=2 exp=5 n=2 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=M ret=1 ref=0 exp=2 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"consume/single_robot/wrong/spot==staging/add": {
		"legs=M ret=1 ref=1 exp=3 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=1 ref=0 exp=2 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"consume/single_robot/wrong/spot==staging/swap": {
		"legs=S ret=1 ref=2 exp=4 n=1 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=D+R+M ret=1 ref=0 exp=2 n=1 | prev D+R+M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"consume/two_robot/bare/spot!=staging/add": {
		"legs=M ret=0 ref=1 exp=2 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"consume/two_robot/bare/spot!=staging/swap": {
		"legs=S ret=0 ref=2 exp=4 n=2 | prev S | cancel ret=0 ref=1 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=2 n=2 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"consume/two_robot/bare/spot==staging/add": {
		"legs=M ret=0 ref=1 exp=2 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"consume/two_robot/bare/spot==staging/swap": {
		"legs=S ret=0 ref=2 exp=4 n=2 | prev S | cancel ret=0 ref=1 | paused start=n cut=n",
		"legs=D+R+M ret=0 ref=0 exp=2 n=2 | prev D+R+M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"consume/two_robot/empty-0831/spot==staging/swap": {
		"legs=S ret=1 ref=2 exp=5 n=2 | prev S | cancel ret=1 ref=1 | paused start=n cut=n",
		"legs=D+R+M ret=1 ref=0 exp=3 n=2 | prev D+R+M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"consume/two_robot/right/spot!=staging/add": {
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"consume/two_robot/right/spot!=staging/swap": {
		"legs=S ret=0 ref=1 exp=3 n=2 | prev S | cancel ret=1 ref=1 | paused start=n cut=n",
		"legs=S ret=0 ref=0 exp=2 n=2 | prev S | cancel ret=1 ref=0 | paused start=n cut=n"},
	"consume/two_robot/right/spot==staging/add": {
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"consume/two_robot/right/spot==staging/swap": {
		"legs=S ret=0 ref=1 exp=3 n=2 | prev S | cancel ret=1 ref=1 | paused start=n cut=n",
		"legs=S ret=0 ref=0 exp=2 n=2 | prev S | cancel ret=1 ref=0 | paused start=n cut=n"},
	"consume/two_robot/unknown/spot!=staging/add": {
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"consume/two_robot/unknown/spot!=staging/swap": {
		"legs=S ret=0 ref=0 exp=2 n=2 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"consume/two_robot/unknown/spot==staging/add": {
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"consume/two_robot/unknown/spot==staging/swap": {
		"legs=S ret=0 ref=0 exp=2 n=2 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"consume/two_robot/wrong/spot!=staging/add": {
		"legs=M ret=1 ref=1 exp=3 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=1 ref=0 exp=2 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"consume/two_robot/wrong/spot!=staging/swap": {
		"legs=S ret=1 ref=2 exp=5 n=2 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=M ret=1 ref=0 exp=3 n=2 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"consume/two_robot/wrong/spot==staging/add": {
		"legs=M ret=1 ref=1 exp=3 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=1 ref=0 exp=2 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"consume/two_robot/wrong/spot==staging/swap": {
		"legs=S ret=1 ref=2 exp=5 n=2 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=D+R+M ret=1 ref=0 exp=3 n=2 | prev D+R+M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"consume/two_robot_press_index/bare/spot!=staging/add": {
		"legs=M ret=0 ref=1 exp=3 n=2 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=2 n=2 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"consume/two_robot_press_index/bare/spot!=staging/swap": {
		"legs=S ret=0 ref=2 exp=4 n=2 | prev S | cancel ret=0 ref=1 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=2 n=2 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"consume/two_robot_press_index/right/spot!=staging/add": {
		"legs=M ret=0 ref=0 exp=2 n=2 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=2 n=2 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"consume/two_robot_press_index/right/spot!=staging/swap": {
		"legs=S ret=0 ref=1 exp=3 n=2 | prev S | cancel ret=1 ref=1 | paused start=n cut=n",
		"legs=S ret=0 ref=0 exp=2 n=2 | prev S | cancel ret=1 ref=0 | paused start=n cut=n"},
	"consume/two_robot_press_index/unknown/spot!=staging/add": {
		"legs=M ret=0 ref=0 exp=2 n=2 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"consume/two_robot_press_index/unknown/spot!=staging/swap": {
		"legs=S ret=0 ref=0 exp=2 n=2 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"consume/two_robot_press_index/wrong/spot!=staging/add": {
		"legs=M ret=1 ref=1 exp=4 n=2 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=1 ref=0 exp=3 n=2 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"consume/two_robot_press_index/wrong/spot!=staging/swap": {
		"legs=S ret=1 ref=2 exp=5 n=2 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=M ret=1 ref=0 exp=3 n=2 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/sequential/bare/spot!=staging/add": {
		"legs=M ret=0 ref=1 exp=2 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/sequential/bare/spot!=staging/swap": {
		"legs=S ret=0 ref=2 exp=3 n=1 | prev S | cancel ret=0 ref=1 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/sequential/bare/spot!=staging/swap/queued-refill": {
		"legs=S ret=0 ref=2 exp=3 n=1 | prev S | cancel ret=0 ref=1 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/sequential/right/spot!=staging/add": {
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"produce/sequential/right/spot!=staging/swap": {
		"legs=S ret=0 ref=1 exp=2 n=1 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=S ret=0 ref=0 exp=1 n=1 | prev S | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/sequential/unknown/spot!=staging/add": {
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"produce/sequential/unknown/spot!=staging/swap": {
		"legs=S ret=0 ref=0 exp=1 n=1 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"produce/sequential/wrong/spot!=staging/add": {
		"legs=M ret=1 ref=1 exp=3 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=1 ref=0 exp=2 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"produce/sequential/wrong/spot!=staging/swap": {
		"legs=S ret=1 ref=2 exp=4 n=1 | prev S | cancel ret=1 ref=1 | paused start=n cut=n",
		"legs=M ret=1 ref=0 exp=2 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"produce/single_robot/bare/spot!=staging/add": {
		"legs=M ret=0 ref=1 exp=2 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/single_robot/bare/spot!=staging/swap": {
		"legs=S ret=0 ref=2 exp=4 n=2 | prev S | cancel ret=0 ref=1 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/single_robot/bare/spot==staging/add": {
		"legs=M ret=0 ref=1 exp=2 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/single_robot/bare/spot==staging/swap": {
		"legs=S ret=0 ref=2 exp=3 n=1 | prev S | cancel ret=0 ref=1 | paused start=n cut=n",
		"legs=D+R+M ret=0 ref=0 exp=1 n=1 | prev D+R+M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/single_robot/bare/spot==staging/swap/queued-refill": {
		"legs=S ret=0 ref=2 exp=3 n=1 | prev S | cancel ret=0 ref=1 | paused start=n cut=n",
		"legs=D+R+M ret=0 ref=0 exp=1 n=1 | prev D+R+M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/single_robot/right/spot!=staging/add": {
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"produce/single_robot/right/spot!=staging/swap": {
		"legs=S ret=0 ref=1 exp=3 n=2 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=S ret=0 ref=0 exp=1 n=1 | prev S | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/single_robot/right/spot==staging/add": {
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"produce/single_robot/right/spot==staging/swap": {
		"legs=S ret=0 ref=1 exp=2 n=1 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=S ret=0 ref=0 exp=1 n=1 | prev S | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/single_robot/unknown/spot!=staging/add": {
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"produce/single_robot/unknown/spot!=staging/swap": {
		"legs=S ret=0 ref=0 exp=2 n=2 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"produce/single_robot/unknown/spot==staging/add": {
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"produce/single_robot/unknown/spot==staging/swap": {
		"legs=S ret=0 ref=0 exp=1 n=1 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"produce/single_robot/wrong/spot!=staging/add": {
		"legs=M ret=1 ref=1 exp=3 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=1 ref=0 exp=2 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"produce/single_robot/wrong/spot!=staging/swap": {
		"legs=S ret=1 ref=2 exp=5 n=2 | prev S | cancel ret=1 ref=1 | paused start=n cut=n",
		"legs=M ret=1 ref=0 exp=2 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"produce/single_robot/wrong/spot==staging/add": {
		"legs=M ret=1 ref=1 exp=3 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=1 ref=0 exp=2 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"produce/single_robot/wrong/spot==staging/swap": {
		"legs=S ret=1 ref=2 exp=4 n=1 | prev S | cancel ret=1 ref=1 | paused start=n cut=n",
		"legs=D+R+M ret=1 ref=0 exp=2 n=1 | prev D+R+M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"produce/two_robot/bare/spot!=staging/add": {
		"legs=M ret=0 ref=1 exp=2 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/two_robot/bare/spot!=staging/swap": {
		"legs=S ret=0 ref=2 exp=4 n=2 | prev S | cancel ret=0 ref=1 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=2 n=2 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/two_robot/bare/spot==staging/add": {
		"legs=M ret=0 ref=1 exp=2 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/two_robot/bare/spot==staging/swap": {
		"legs=S ret=0 ref=2 exp=4 n=2 | prev S | cancel ret=0 ref=1 | paused start=n cut=n",
		"legs=D+R+M ret=0 ref=0 exp=2 n=2 | prev D+R+M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/two_robot/bare/spot==staging/swap/queued-refill": {
		"legs=S ret=0 ref=2 exp=4 n=2 | prev S | cancel ret=0 ref=1 | paused start=n cut=n",
		"legs=D+R+M ret=0 ref=0 exp=2 n=2 | prev D+R+M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/two_robot/right/spot!=staging/add": {
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"produce/two_robot/right/spot!=staging/swap": {
		"legs=S ret=0 ref=1 exp=3 n=2 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=S ret=0 ref=0 exp=2 n=2 | prev S | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/two_robot/right/spot==staging/add": {
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"produce/two_robot/right/spot==staging/swap": {
		"legs=S ret=0 ref=1 exp=3 n=2 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=S ret=0 ref=0 exp=2 n=2 | prev S | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/two_robot/unknown/spot!=staging/add": {
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"produce/two_robot/unknown/spot!=staging/swap": {
		"legs=S ret=0 ref=0 exp=2 n=2 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"produce/two_robot/unknown/spot==staging/add": {
		"legs=M ret=0 ref=0 exp=1 n=1 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"produce/two_robot/unknown/spot==staging/swap": {
		"legs=S ret=0 ref=0 exp=2 n=2 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"produce/two_robot/wrong/spot!=staging/add": {
		"legs=M ret=1 ref=1 exp=3 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=1 ref=0 exp=2 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"produce/two_robot/wrong/spot!=staging/swap": {
		"legs=S ret=1 ref=2 exp=5 n=2 | prev S | cancel ret=1 ref=1 | paused start=n cut=n",
		"legs=M ret=1 ref=0 exp=3 n=2 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"produce/two_robot/wrong/spot==staging/add": {
		"legs=M ret=1 ref=1 exp=3 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=1 ref=0 exp=2 n=1 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"produce/two_robot/wrong/spot==staging/swap": {
		"legs=S ret=1 ref=2 exp=5 n=2 | prev S | cancel ret=1 ref=1 | paused start=n cut=n",
		"legs=D+R+M ret=1 ref=0 exp=3 n=2 | prev D+R+M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"produce/two_robot_press_index/bare/spot!=staging/add": {
		"legs=M ret=0 ref=1 exp=3 n=2 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=2 n=2 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/two_robot_press_index/bare/spot!=staging/swap": {
		"legs=S ret=0 ref=2 exp=4 n=2 | prev S | cancel ret=0 ref=1 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=2 n=2 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/two_robot_press_index/bare/spot!=staging/swap/queued-refill": {
		"legs=S ret=0 ref=2 exp=4 n=2 | prev S | cancel ret=0 ref=1 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=2 n=2 | prev M | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/two_robot_press_index/right/spot!=staging/add": {
		"legs=M ret=0 ref=0 exp=2 n=2 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=0 ref=0 exp=2 n=2 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"produce/two_robot_press_index/right/spot!=staging/swap": {
		"legs=S ret=0 ref=1 exp=3 n=2 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"legs=S ret=0 ref=0 exp=2 n=2 | prev S | cancel ret=0 ref=0 | paused start=n cut=n"},
	"produce/two_robot_press_index/unknown/spot!=staging/add": {
		"legs=M ret=0 ref=0 exp=2 n=2 | prev M | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"produce/two_robot_press_index/unknown/spot!=staging/swap": {
		"legs=S ret=0 ref=0 exp=2 n=2 | prev S | cancel ret=0 ref=0 | paused start=n cut=n",
		"start refused | prev refused"},
	"produce/two_robot_press_index/wrong/spot!=staging/add": {
		"legs=M ret=1 ref=1 exp=4 n=2 | prev M | cancel ret=1 ref=0 | paused start=n cut=n",
		"legs=M ret=1 ref=0 exp=3 n=2 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
	"produce/two_robot_press_index/wrong/spot!=staging/swap": {
		"legs=S ret=1 ref=2 exp=5 n=2 | prev S | cancel ret=1 ref=1 | paused start=n cut=n",
		"legs=M ret=1 ref=0 exp=3 n=2 | prev M | cancel ret=1 ref=0 | paused start=n cut=n"},
}
