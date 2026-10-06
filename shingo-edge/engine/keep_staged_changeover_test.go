package engine

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// KEEP-STAGED SPOTS THROUGH A CHANGEOVER, end to end through StartProcessChangeover
// and CancelProcessChangeover against a Core stub that answers for the spots.
//
// Each spot is decided once across the whole plan (changeoverSpots): a spare
// is kept when the incoming claim keeps that spot and the spare suits it (a
// full one by the part Core reads on it; an empty, for produce, by the claims'
// role and part), sent back to whatever supplied it otherwise, and the incoming claim's spot is
// filled with plain refills — one for the changeover's own supply to lift, one
// to stand after it. These count what lands in the order table.

type coClaim struct {
	line, spot, source, part string
	role                     protocol.ClaimRole
	mode                     protocol.SwapMode
	keepStaged               bool
	evacuate                 bool // EvacuateOnChangeover
}

type coFixture struct {
	eng       *Engine
	db        *store.DB
	processID int64
	toStyleID int64
	nodeIDs   map[string]int64
}

// seedKeepStagedChangeover builds a process with an active style A and a style
// B, each claim in its list, and a Core stub answering node-bins from rows.
func seedKeepStagedChangeover(t *testing.T, from, to []coClaim, rows map[string]NodeBinInfo) *coFixture {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.logFn = func(string, ...any) {}
	eng.wireEventHandlers()

	procID, err := db.CreateProcess("KSCO-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "process")
	fx := &coFixture{eng: eng, db: db, processID: procID, nodeIDs: map[string]int64{}}
	node := func(line string) int64 {
		if id, ok := fx.nodeIDs[line]; ok {
			return id
		}
		id, err := db.CreateProcessNode(processes.NodeInput{
			ProcessID: procID, CoreNodeName: line, Code: line, Name: line, Sequence: len(fx.nodeIDs) + 1, Enabled: true,
		})
		testutil.MustNoErr(t, err, "node "+line)
		fx.nodeIDs[line] = id
		_, err = db.EnsureProcessNodeRuntime(id)
		testutil.MustNoErr(t, err, "runtime "+line)
		return id
	}
	styleA, err := db.CreateStyle("KSCO-A", "", procID)
	testutil.MustNoErr(t, err, "style A")
	styleB, err := db.CreateStyle("KSCO-B", "", procID)
	testutil.MustNoErr(t, err, "style B")
	fx.toStyleID = styleB
	upsert := func(styleID int64, c coClaim) int64 {
		node(c.line)
		in := processes.NodeClaimInput{
			StyleID: styleID, CoreNodeName: c.line, Role: c.role, SwapMode: c.mode, PayloadCode: c.part,
			UOPCapacity: 40, InboundSource: c.source, InboundStaging: c.spot, OutboundDestination: "KSCO-DEST",
			EvacuateOnChangeover: c.evacuate,
		}
		if c.mode == protocol.SwapModeSingleRobot {
			in.OutboundStaging = "KSCO-OUT-" + c.line
		}
		// The two modes with no staging hop: the spot is a node of its own,
		// and each needs its partner or back position.
		if c.mode == protocol.SwapModeSequential || c.mode == protocol.SwapModeTwoRobotPressIndex {
			in.InboundStaging, in.PairedCoreNode = "", "KSCO-PAIR-"+c.line
		}
		id, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, in)
		testutil.MustNoErr(t, err, "claim "+c.line)
		if c.keepStaged {
			_, err = db.DB.Exec(`UPDATE style_node_claims SET keep_staged_node=? WHERE id=?`, c.spot, id)
			testutil.MustNoErr(t, err, "keep_staged_node")
		}
		return id
	}
	for _, c := range from {
		id := upsert(styleA, c)
		testutil.MustNoErr(t, db.SetProcessNodeRuntime(fx.nodeIDs[c.line], &id, 30), "runtime claim")
	}
	for _, c := range to {
		upsert(styleB, c)
	}
	testutil.MustNoErr(t, db.SetActiveStyle(procID, &styleA), "active style")
	eng.coreClient = stubCoreClient(ksNodeBinsStub(t, rows).URL)
	return fx
}

// spotTraffic is every plain order to or from a spot: refills by the line they
// are attributed to, returns by where they go and what they carry.
type spotTraffic struct {
	refills map[string]int // line -> count
	returns []domain.Order
}

func readSpotTraffic(t *testing.T, fx *coFixture, spot string) spotTraffic {
	t.Helper()
	got := spotTraffic{refills: map[string]int{}}
	live, err := fx.db.ListActiveOrders()
	testutil.MustNoErr(t, err, "orders")
	lineOf := map[int64]string{}
	for line, id := range fx.nodeIDs {
		lineOf[id] = line
	}
	for _, o := range live {
		if o.ProcessNodeID == nil {
			continue
		}
		switch {
		case o.OrderType == orders.TypeRetrieve && o.DeliveryNode == spot:
			got.refills[lineOf[*o.ProcessNodeID]]++
		case o.OrderType == orders.TypeMove && o.SourceNode == spot && o.DeliveryNode != lineOf[*o.ProcessNodeID]:
			got.returns = append(got.returns, o)
		}
	}
	return got
}

func startKSChangeover(t *testing.T, fx *coFixture) int64 {
	t.Helper()
	co, err := fx.eng.StartProcessChangeover(fx.processID, fx.toStyleID, "test", "keep-staged")
	testutil.MustNoErr(t, err, "start changeover")
	return co.ID
}

// S5: the line keeps its spot and the part changes. The spare standing there is
// the outgoing part: it goes back to the outgoing claim's source carrying its
// own part, and two refills of the incoming part come — one the changeover's
// supply lifts, one to stand after it.
func TestKeepStagedChangeover_PartChangeSendsTheSpareBack(t *testing.T) {
	t.Parallel()
	for _, mode := range []protocol.SwapMode{protocol.SwapModeTwoRobot, protocol.SwapModeSingleRobot,
		protocol.SwapModeSequential, protocol.SwapModeTwoRobotPressIndex} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			fx := seedKeepStagedChangeover(t,
				[]coClaim{{"L1", "SPOT", "SRC-OLD", "PART-OLD", protocol.ClaimRoleConsume, mode, true, false}},
				[]coClaim{{"L1", "SPOT", "SRC-NEW", "PART-NEW", protocol.ClaimRoleConsume, mode, true, false}},
				map[string]NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-OLD"},
					"SPOT": {Occupied: true, PayloadCode: "PART-OLD"}})
			coID := startKSChangeover(t, fx)
			got := readSpotTraffic(t, fx, "SPOT")
			if len(got.returns) != 1 || got.returns[0].DeliveryNode != "SRC-OLD" || got.returns[0].PayloadCode != "PART-OLD" {
				t.Fatalf("returns = %+v, want one to SRC-OLD carrying PART-OLD", got.returns)
			}
			if got.refills["L1"] != 2 {
				t.Fatalf("refills for L1 = %d, want 2", got.refills["L1"])
			}
			assertNoTaskLink(t, fx, coID, "L1")
		})
	}
}

// S6: the line changes over and its spare does not — same part, same role, an
// evacuation (the line's own bin goes out and back). The spare stays; the
// changeover's supply lifts it, and one refill comes to stand after it. A line
// that does not change over at all orders nothing for its spot.
func TestKeepStagedChangeover_SameSpareStays(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name        string
		evacuate    bool
		wantRefills int
	}{
		{"evacuated line: the supply lifts the spare, one comes", true, 1},
		{"unchanged line: nothing", false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fx := seedKeepStagedChangeover(t,
				[]coClaim{{"L1", "SPOT", "SRC", "PART-A", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot, true, c.evacuate}},
				[]coClaim{{"L1", "SPOT", "SRC", "PART-A", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot, true, false}},
				map[string]NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-A"}, "SPOT": {Occupied: true, PayloadCode: "PART-A"}})
			startKSChangeover(t, fx)
			got := readSpotTraffic(t, fx, "SPOT")
			if len(got.returns) != 0 || got.refills["L1"] != c.wantRefills {
				t.Fatalf("returns=%d refills=%d, want none and %d", len(got.returns), got.refills["L1"], c.wantRefills)
			}
		})
	}
}

// S7: a keep-staged line dropped from the incoming style leaves its spare with
// nobody to keep it, so it goes back and nothing comes; a line added in the
// incoming style keeps a spot that is filled.
func TestKeepStagedChangeover_DroppedAndAddedLines(t *testing.T) {
	t.Parallel()
	fx := seedKeepStagedChangeover(t,
		[]coClaim{{"L1", "SPOT-1", "SRC-1", "PART-A", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot, true, false}},
		[]coClaim{{"L2", "SPOT-2", "SRC-2", "PART-B", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot, true, false}},
		map[string]NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-A"},
			"SPOT-1": {Occupied: true, PayloadCode: "PART-A"}, "SPOT-2": {}})
	startKSChangeover(t, fx)
	dropped := readSpotTraffic(t, fx, "SPOT-1")
	if len(dropped.returns) != 1 || dropped.returns[0].DeliveryNode != "SRC-1" || len(dropped.refills) != 0 {
		t.Fatalf("dropped line's spot: returns=%+v refills=%v, want one return to SRC-1 and nothing coming",
			dropped.returns, dropped.refills)
	}
	added := readSpotTraffic(t, fx, "SPOT-2")
	// The added line is supplied straight to the line (planAddAction), so
	// nothing lifts its spare: one refill fills the spot.
	if len(added.returns) != 0 || added.refills["L2"] != 1 {
		t.Fatalf("added line's spot: returns=%d refills=%v, want none and 1 for L2", len(added.returns), added.refills)
	}
}

// TWO LINE NODES HAND ONE SPOT BETWEEN THE STYLES OF ONE PROCESS. Decided per
// spot, not per line: with the same part the spare stays for the incoming
// line; with another part it goes back to the outgoing line's source.
func TestKeepStagedChangeover_TwoLinesHandOneSpot(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name        string
		incoming    string
		wantReturns int
		wantRefills int
	}{
		{"same part: the spare stays for the incoming line", "PART-A", 0, 0},
		{"another part: the spare goes back, one comes", "PART-B", 1, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fx := seedKeepStagedChangeover(t,
				[]coClaim{{"L1", "SPOT", "SRC-1", "PART-A", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot, true, false}},
				[]coClaim{{"L2", "SPOT", "SRC-2", c.incoming, protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot, true, false}},
				map[string]NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-A"},
					"SPOT": {Occupied: true, PayloadCode: "PART-A"}})
			startKSChangeover(t, fx)
			got := readSpotTraffic(t, fx, "SPOT")
			if len(got.returns) != c.wantReturns || got.refills["L2"] != c.wantRefills || got.refills["L1"] != 0 {
				t.Fatalf("returns=%d refills=%v, want %d returns and %d refills for L2", len(got.returns), got.refills,
					c.wantReturns, c.wantRefills)
			}
			if c.wantReturns == 1 && got.returns[0].DeliveryNode != "SRC-1" {
				t.Errorf("the spare goes to %q, want the outgoing line's source SRC-1", got.returns[0].DeliveryNode)
			}
		})
	}
}

// S8: a changeover cancelled after its return lifted the outgoing spare and a
// spare of the incoming part landed. The refills not yet with the fleet are
// cancelled; the incoming part's spare is sent back to its source; the outgoing
// style, which stays, gets a refill of its own part.
func TestKeepStagedChangeover_CancelPutsTheSpotBack(t *testing.T) {
	t.Parallel()
	for _, mode := range []protocol.SwapMode{protocol.SwapModeTwoRobot, protocol.SwapModeSingleRobot,
		protocol.SwapModeSequential, protocol.SwapModeTwoRobotPressIndex} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			rows := map[string]NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-OLD"},
				"SPOT": {Occupied: true, PayloadCode: "PART-OLD"}}
			fx := seedKeepStagedChangeover(t,
				[]coClaim{{"L1", "SPOT", "SRC-OLD", "PART-OLD", protocol.ClaimRoleConsume, mode, true, false}},
				[]coClaim{{"L1", "SPOT", "SRC-NEW", "PART-NEW", protocol.ClaimRoleConsume, mode, true, false}},
				rows)
			startKSChangeover(t, fx)
			if got := readSpotTraffic(t, fx, "SPOT"); got.refills["L1"] != 2 || len(got.returns) != 1 {
				t.Fatalf("fixture: start made refills=%v returns=%d, want 2 and 1", got.refills, len(got.returns))
			}
			// The return went, and a spare of the incoming part landed.
			rows["SPOT"] = NodeBinInfo{Occupied: true, PayloadCode: "PART-NEW"}
			fx.eng.coreClient = stubCoreClient(ksNodeBinsStub(t, rows).URL)
			markSpotOrdersFlown(t, fx, "SPOT", 1)

			testutil.MustNoErr(t, fx.eng.CancelProcessChangeover(fx.processID), "cancel")

			got := readSpotTraffic(t, fx, "SPOT")
			var back *domain.Order
			for i := range got.returns {
				if got.returns[i].PayloadCode == "PART-NEW" {
					back = &got.returns[i]
				}
			}
			if back == nil || back.DeliveryNode != "SRC-NEW" {
				t.Fatalf("returns = %+v, want the incoming part's spare sent back to SRC-NEW", got.returns)
			}
			// One refill was with the fleet when the cancel came and lands; the other
			// was cancelled. The outgoing style gets one of its own part.
			oldRefills := 0
			live, err := fx.db.ListActiveOrders()
			testutil.MustNoErr(t, err, "orders")
			for _, o := range live {
				if o.OrderType == orders.TypeRetrieve && o.DeliveryNode == "SPOT" && o.PayloadCode == "PART-OLD" {
					oldRefills++
				}
			}
			if oldRefills != 1 || got.refills["L1"] != 2 {
				t.Fatalf("after cancel: PART-OLD refills=%d, all refills=%v; want 1 of the outgoing part plus the one "+
					"already flown", oldRefills, got.refills)
			}
		})
	}
}

// A changeover cancelled before anything at the spot moved: the outgoing
// spare still stands there, so the style that stays wants exactly what is on
// the spot. The cancel aborts the start's return and refills and orders
// nothing. Judged by the leaver's config, as the start judges, the reverted
// test reads that spare as the incoming style's and sends it away.
func TestKeepStagedChangeover_CancelKeepsTheSpareThatNeverLeft(t *testing.T) {
	t.Parallel()
	rows := map[string]NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-OLD"},
		"SPOT": {Occupied: true, PayloadCode: "PART-OLD"}}
	fx := seedKeepStagedChangeover(t,
		[]coClaim{{"L1", "SPOT", "SRC-OLD", "PART-OLD", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot, true, false}},
		[]coClaim{{"L1", "SPOT", "SRC-NEW", "PART-NEW", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot, true, false}},
		rows)
	startKSChangeover(t, fx)

	testutil.MustNoErr(t, fx.eng.CancelProcessChangeover(fx.processID), "cancel")

	if got := readSpotTraffic(t, fx, "SPOT"); len(got.refills) != 0 || len(got.returns) != 0 {
		t.Fatalf("after cancel: refills=%v returns=%d, want none: the outgoing spare never left", got.refills, len(got.returns))
	}
}

// markSpotOrdersFlown puts the first n refills bound for a spot into the
// fleet's hands, as a dispatch reply would.
func markSpotOrdersFlown(t *testing.T, fx *coFixture, spot string, n int) {
	t.Helper()
	live, err := fx.db.ListActiveOrders()
	testutil.MustNoErr(t, err, "orders")
	for _, o := range live {
		if n == 0 {
			return
		}
		if o.OrderType == orders.TypeRetrieve && o.DeliveryNode == spot {
			testutil.MustNoErr(t, fx.db.UpdateOrderStatus(o.ID, string(protocol.StatusInTransit)), "in transit")
			n--
		}
	}
}

// assertNoTaskLink checks that no spot order is linked to the line's changeover
// task: a linked refill would be read as the changeover's own next material.
func assertNoTaskLink(t *testing.T, fx *coFixture, coID int64, line string) {
	t.Helper()
	task, err := fx.db.GetChangeoverNodeTaskByNode(coID, fx.nodeIDs[line])
	testutil.MustNoErr(t, err, "task")
	for _, id := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
		if id == nil {
			continue
		}
		o, err := fx.db.GetOrder(*id)
		testutil.MustNoErr(t, err, "linked order")
		if o.OrderType != orders.TypeComplex {
			t.Errorf("task links a %s order (%d): only the changeover's own legs are linked", o.OrderType, o.ID)
		}
	}
}

// THE SAVE THAT LEAVES A SPOT. Clearing keep_staged, or dropping the claim, on
// the RUNNING style leaves a spare standing on a spot nothing keeps; it goes
// back to the claim's inbound source by a plain move. The same save on a style
// that is not running orders nothing: the spare there is the running style's.
func TestKeepStagedSave_LeavingTheSpotSendsTheSpareBack(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		running bool
		act     func(t *testing.T, fx *coFixture, claimID int64, in processes.NodeClaimInput)
		want    int
	}{
		{"clear the name on the running style", true, func(t *testing.T, fx *coFixture, _ int64, in processes.NodeClaimInput) {
			in.KeepStagedNode = domain.Ptr("")
			_, err := fx.eng.StyleService().UpsertClaim(domain.CoreNodeKinds{}, in)
			testutil.MustNoErr(t, err, "clear keep_staged_node")
		}, 1},
		{"drop the claim on the running style", true, func(t *testing.T, fx *coFixture, claimID int64, _ processes.NodeClaimInput) {
			testutil.MustNoErr(t, fx.eng.StyleService().DeleteClaim(claimID), "delete claim")
		}, 1},
		{"clear the name on a style that is not running", false, func(t *testing.T, fx *coFixture, _ int64, in processes.NodeClaimInput) {
			in.KeepStagedNode = domain.Ptr("")
			_, err := fx.eng.StyleService().UpsertClaim(domain.CoreNodeKinds{}, in)
			testutil.MustNoErr(t, err, "clear keep_staged_node")
		}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fx := seedKeepStagedChangeover(t,
				[]coClaim{{"L1", "SPOT", "SRC", "PART-A", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot, true, false}},
				[]coClaim{{"L1", "SPOT", "SRC", "PART-A", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot, true, false}},
				map[string]NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-A"}, "SPOT": {Occupied: true, PayloadCode: "PART-A"}})
			styleID := fx.toStyleID // style B: not running
			if c.running {
				p, err := fx.db.GetProcess(fx.processID)
				testutil.MustNoErr(t, err, "process")
				styleID = *p.ActiveStyleID
			}
			claim, err := fx.db.GetStyleNodeClaimByNode(styleID, "L1")
			testutil.MustNoErr(t, err, "claim")
			in := processes.NodeClaimInput{
				StyleID: styleID, CoreNodeName: "L1", Role: claim.Role, SwapMode: claim.SwapMode, PayloadCode: claim.PayloadCode,
				UOPCapacity: claim.UOPCapacity, InboundSource: claim.InboundSource, InboundStaging: claim.InboundStaging,
				OutboundDestination: claim.OutboundDestination,
			}
			c.act(t, fx, claim.ID, in)
			got := readSpotTraffic(t, fx, "SPOT")
			if len(got.returns) != c.want {
				t.Fatalf("returns = %d, want %d", len(got.returns), c.want)
			}
			if c.want == 1 && (got.returns[0].DeliveryNode != "SRC" || got.returns[0].PayloadCode != "PART-A" ||
				got.returns[0].OriginClass != protocol.OriginClassNoDemand) {
				t.Errorf("return = %+v, want to SRC carrying PART-A, no demand", got.returns[0])
			}
			if len(got.refills) != 0 {
				t.Errorf("a save that leaves the spot ordered refills: %v", got.refills)
			}
		})
	}
}

// A REQUEST LET IN AFTER A CHANGEOVER ARMED IS REFUSED (sim F4). The level
// keeper's request passed the changeover guard, then waited on the cell lock the
// start was holding; let in after the changeover armed, it built an
// outgoing-style swap that no changeover leg owns, which the cancel then left
// standing and the sweep kept pointing at. Here the test holds the lock the way
// a start does and arms the changeover while the request waits.
//
// The changeover is armed only once the request is parked on that lock, read
// from the request goroutine's own stack, so the request has always passed its
// first guard with nothing armed and only the check under the lock can refuse
// it. Arming after a fixed sleep let a slow request meet the armed changeover
// at the first guard instead, and the test passed without reaching that check.
func TestKeepStagedRequest_ARequestLetInAfterAStartIsRefused(t *testing.T) {
	t.Parallel()
	rows := map[string]NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-OLD"},
		"SPOT": {Occupied: true, PayloadCode: "PART-OLD"}}
	fx := seedKeepStagedChangeover(t,
		[]coClaim{{"L1", "SPOT", "SRC-OLD", "PART-OLD", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot, true, false}},
		[]coClaim{{"L1", "SPOT", "SRC-NEW", "PART-NEW", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot, true, false}},
		rows)
	proc, err := fx.db.GetProcess(fx.processID)
	testutil.MustNoErr(t, err, "process")

	mu := fx.eng.primeNodeLock(&processes.NodeClaim{CoreNodeName: "L1"})
	mu.Lock()
	done := make(chan error, 1)
	gid := make(chan string, 1)
	go func() {
		gid <- goroutineID()
		_, err := fx.eng.RequestNodeMaterial(fx.nodeIDs["L1"], 1)
		done <- err
	}()
	waitParkedOnCellLock(t, <-gid, done)
	_, err = fx.eng.changeoverService.Create(fx.processID, proc.ActiveStyleID, fx.toStyleID, "test", "", nil, nil, nil, nil)
	testutil.MustNoErr(t, err, "arm the changeover")
	mu.Unlock()

	var armed *ChangeoverArmedError
	if err := <-done; !errors.As(err, &armed) {
		t.Fatalf("the request let in after the changeover armed returned %v, want ChangeoverArmedError", err)
	}
	live, err := fx.db.ListActiveOrdersByProcessNode(fx.nodeIDs["L1"])
	testutil.MustNoErr(t, err, "orders")
	if len(live) != 0 {
		t.Fatalf("%d order(s) created by a request refused under the lock", len(live))
	}
}

// goroutineID is the calling goroutine's number, from its stack header
// ("goroutine 37 [running]:").
func goroutineID() string {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	return strings.Fields(string(buf))[1]
}

// waitParkedOnCellLock returns once goroutine gid is blocked in the mutex Lock
// that requestNodeFromClaim takes, the cell's prime lock: the scheduler has
// parked it there (state "sync.Mutex.Lock") and the frame that called Lock is
// requestNodeFromClaim. It reads every goroutine's stack and yields between
// reads; no clock decides anything. A request that returns before it gets there
// fails the test with what it returned.
func waitParkedOnCellLock(t *testing.T, gid string, done <-chan error) {
	t.Helper()
	header := "goroutine " + gid + " [sync.Mutex.Lock"
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n == len(buf) {
			buf = make([]byte, 2*len(buf))
			continue
		}
		for _, g := range strings.Split(string(buf[:n]), "\n\n") {
			if !strings.HasPrefix(g, header) {
				continue
			}
			lines := strings.Split(g, "\n")
			for i := 0; i+2 < len(lines); i++ {
				if strings.HasPrefix(lines[i], "sync.(*Mutex).Lock(") &&
					strings.HasPrefix(lines[i+2], "shingoedge/engine.(*Engine).requestNodeFromClaim(") {
					return
				}
			}
		}
		select {
		case err := <-done:
			t.Fatalf("the request returned %v before it waited on the cell lock", err)
		default:
		}
		runtime.Gosched()
	}
}

// A single-robot changeover onto a keep-staged claim has no stage order: its
// leg collects the spare standing on the spot. The manual stage button on that
// line stages nothing. It used to send a complex fetch from the market onto the
// spot, a second bin for one place, and no complex leg may set down on a
// keep-staged spot.
func TestKeepStagedChangeover_TheStageButtonStagesNothing(t *testing.T) {
	t.Parallel()
	fx := seedKeepStagedChangeover(t,
		[]coClaim{{"L1", "SPOT", "SRC-OLD", "PART-OLD", protocol.ClaimRoleConsume, protocol.SwapModeSingleRobot, true, false}},
		[]coClaim{{"L1", "SPOT", "SRC-NEW", "PART-NEW", protocol.ClaimRoleConsume, protocol.SwapModeSingleRobot, true, false}},
		map[string]NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-OLD"},
			"SPOT": {Occupied: true, PayloadCode: "PART-OLD"}})
	startKSChangeover(t, fx)
	before, err := fx.db.ListActiveOrders()
	testutil.MustNoErr(t, err, "orders before")
	order, err := fx.eng.StageNodeChangeoverMaterial(fx.processID, fx.nodeIDs["L1"])
	if err == nil || order != nil {
		t.Fatalf("the stage button made %+v (err %v), want a refusal: the spare on the spot is the incoming bin", order, err)
	}
	after, err := fx.db.ListActiveOrders()
	testutil.MustNoErr(t, err, "orders after")
	if len(after) != len(before) {
		t.Errorf("the stage button made %d order(s), want none", len(after)-len(before))
	}
}
