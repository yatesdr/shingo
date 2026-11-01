package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/release"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// kanban_pins_changeover_test.go — characterization pins of the Edge's changeover
// at main 26a82fdf, taken before the Kanban build. Each test states what the tree
// does now, not what it should do; the build's PINS.md says which unit changes
// which pin and what the test becomes.

// kpPress is a two-position press-index process whose two styles ride different
// bin types, so the start fans out one per-position order per position. The
// styles' outbounds differ, so a drop says which claim it read.
type kpPress struct {
	db                 *store.DB
	eng                *Engine
	processID, toStyle int64
	front, back        int64
}

const (
	kpFront, kpBack    = "KP-FRONT", "KP-BACK"
	kpFromOut, kpToOut = "KP-DEST-OLD", "KP-DEST-NEW"
	kpMarket           = "KP-MARKET"
)

func seedKPPress(t *testing.T) *kpPress {
	t.Helper()
	db := testEngineDB(t)
	processID, err := db.CreateProcess("KP-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "process")
	front, err := db.CreateProcessNode(processes.NodeInput{ProcessID: processID, CoreNodeName: kpFront, Code: "KPF", Name: kpFront, Sequence: 1, Enabled: true})
	testutil.MustNoErr(t, err, "front")
	back, err := db.CreateProcessNode(processes.NodeInput{ProcessID: processID, CoreNodeName: kpBack, Code: "KPB", Name: kpBack, Sequence: 2, Enabled: true})
	testutil.MustNoErr(t, err, "back")
	fromStyle, err := db.CreateStyle("KP-FROM", "", processID)
	testutil.MustNoErr(t, err, "from style")
	toStyle, err := db.CreateStyle("KP-TO", "", processID)
	testutil.MustNoErr(t, err, "to style")
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &fromStyle), "active style")
	claim := func(style int64, part, out string) int64 {
		id, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
			StyleID: style, CoreNodeName: kpFront, Role: protocol.ClaimRoleProduce,
			SwapMode: protocol.SwapModeTwoRobotPressIndex, PairedCoreNode: kpBack,
			PayloadCode: part, UOPCapacity: 100, InboundSource: kpMarket, OutboundDestination: out,
		})
		testutil.MustNoErr(t, err, "claim "+part)
		return id
	}
	fromClaim := claim(fromStyle, "KP-OLD", kpFromOut)
	claim(toStyle, "KP-NEW", kpToOut)
	for _, n := range []int64{front, back} {
		_, err := db.EnsureProcessNodeRuntime(n)
		testutil.MustNoErr(t, err, "runtime")
		testutil.MustNoErr(t, db.SetProcessNodeRuntime(n, &fromClaim, 50), "seat")
	}
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	bridgeCore(t, eng, srv.URL)
	eng.SetPayloadBinTypes([]protocol.PayloadBinTypeInfo{
		{PayloadCode: "KP-OLD", BinTypeCode: "KP-TYPE-A"},
		{PayloadCode: "KP-NEW", BinTypeCode: "KP-TYPE-B"},
	})
	return &kpPress{db: db, eng: eng, processID: processID, toStyle: toStyle, front: front, back: back}
}

func (p *kpPress) start(t *testing.T) *processes.Changeover {
	t.Helper()
	co, err := p.eng.StartProcessChangeover(p.processID, p.toStyle, "test", "kanban pin")
	testutil.MustNoErr(t, err, "start changeover")
	return co
}

// kpLegs is every order a changeover's node tasks link, by position name.
func kpLegs(t *testing.T, db *store.DB, changeoverID int64) map[string][]int64 {
	t.Helper()
	tasks, err := db.ListChangeoverNodeTasks(changeoverID)
	testutil.MustNoErr(t, err, "tasks")
	out := map[string][]int64{}
	for _, task := range tasks {
		for _, id := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
			if id != nil {
				out[task.NodeName] = append(out[task.NodeName], *id)
			}
		}
	}
	return out
}

func kpOpenEpisode(t *testing.T, eng *Engine, changeoverID int64) *store.OpenOrigin {
	t.Helper()
	open, err := eng.db.GetOpenDemandOrigin(protocol.ChangeoverEpisodeKey(eng.cfg.StationID(), changeoverID))
	testutil.MustNoErr(t, err, "read open episode")
	return open
}

// kpFailInserts fails every INSERT on table matching when, as a store that cannot
// take the write.
func kpFailInserts(t *testing.T, db *store.DB, table, when string) {
	t.Helper()
	_, err := db.Exec(fmt.Sprintf(`CREATE TRIGGER kp_fail BEFORE INSERT ON %s WHEN %s
		BEGIN SELECT RAISE(ABORT, 'kanban pin: write refused'); END`, table, when))
	testutil.MustNoErr(t, err, "arm the failure")
}

// ── per-position changeover: where the old bin goes ─────────────────────────

// TestKanbanPin_PerPositionSwapDropsTheOldBinAtTheFromOutbound: started end to
// end, each position's one order lifts the old bin and drops it at the FROM
// claim's OutboundDestination, wherever the incoming style sends its own. No step
// names the incoming outbound.
func TestKanbanPin_PerPositionSwapDropsTheOldBinAtTheFromOutbound(t *testing.T) {
	t.Parallel()
	p := seedKPPress(t)
	legs := kpLegs(t, p.db, p.start(t).ID)
	if len(legs) != 2 {
		t.Fatalf("legs by position = %v, want one position each for %s and %s", legs, kpFront, kpBack)
	}
	for _, pos := range []string{kpFront, kpBack} {
		if len(legs[pos]) != 1 {
			t.Fatalf("%s legs = %v, want its one per-position order", pos, legs[pos])
		}
		raw, err := p.db.GetOrderStepsJSON(legs[pos][0])
		testutil.MustNoErr(t, err, "steps")
		var got []protocol.ComplexOrderStep
		testutil.MustNoErr(t, json.Unmarshal([]byte(raw), &got), "decode steps")
		want := []protocol.ComplexOrderStep{
			stationWait(pos, release.PurposeReady),
			{Action: "pickup", Node: pos},
			{Action: "dropoff", Node: kpFromOut},
			{Action: "pickup", Node: kpMarket, Empty: true, PayloadCode: "KP-NEW"},
			{Action: "dropoff", Node: pos},
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s steps\n got %+v\nwant %+v", pos, got, want)
		}
	}
}

// TestKanbanPin_FanOutFromClaimOfAnExtensionPosition: what the fan-out hands the
// builder as the from-claim of a position outside the parent's diff. From-side
// only drops at the outgoing outbound; to-side only uses the incoming claim for
// both sides, so whatever stands there goes to the INCOMING outbound.
func TestKanbanPin_FanOutFromClaimOfAnExtensionPosition(t *testing.T) {
	t.Parallel()
	press := func(part, out string) processes.NodeClaim {
		c := crossModeFromPressIndex(part, "FRONT", "MIDDLE", "BACK")
		c.OutboundDestination = out
		return c
	}
	single := func(part, out string) processes.NodeClaim {
		c := crossModeNonPressIndex("FRONT", part, protocol.SwapModeSingleRobot)
		c.OutboundDestination = out
		return c
	}
	for _, tc := range []struct {
		name      string
		from, to  processes.NodeClaim
		situation ChangeoverSituation
		wantDrop  string
	}{
		{"from side only", press("PART-A", kpFromOut), single("PART-B", kpToOut), SituationDrop, kpFromOut},
		{"to side only", single("PART-A", kpFromOut), press("PART-B", kpToOut), SituationSwap, kpToOut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := FanOutPressIndexCrossMode([]ChangeoverNodeDiff{
				{CoreNodeName: "FRONT", Situation: SituationSwap, FromClaim: &tc.from, ToClaim: &tc.to}}, nil)
			for _, d := range out {
				if d.CoreNodeName != "MIDDLE" && d.CoreNodeName != "BACK" {
					continue
				}
				if d.Situation != tc.situation || d.FromClaim == nil || d.FromClaim.OutboundDestination != tc.wantDrop {
					t.Errorf("%s: %s with from-claim %+v, want %s dropping at %s", d.CoreNodeName, d.Situation, d.FromClaim, tc.situation, tc.wantDrop)
				}
			}
		})
	}
}

// ── what a cancel aborts ─────────────────────────────────────────────────────

// TestKanbanPin_CancelAbortsThePerPositionLegs: every order linked to a node task
// is aborted on cancel, at any status short of terminal, whatever the keep-staged
// spots hold (this fixture has none). The abort is inside the park read,
// taskLeftOnStaging, not at abortSpotOrdersNotFlown.
func TestKanbanPin_CancelAbortsThePerPositionLegs(t *testing.T) {
	t.Parallel()
	for _, status := range []protocol.Status{orders.StatusPending, orders.StatusQueued, orders.StatusInTransit} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			p := seedKPPress(t)
			legs := kpLegs(t, p.db, p.start(t).ID)
			if len(legs[kpFront]) != 1 || len(legs[kpBack]) != 1 {
				t.Fatalf("fixture: legs = %v", legs)
			}
			ids := []int64{legs[kpFront][0], legs[kpBack][0]}
			for _, id := range ids {
				testutil.MustNoErr(t, p.db.UpdateOrderStatus(id, string(status)), "drive leg")
			}
			testutil.MustNoErr(t, p.eng.CancelProcessChangeover(p.processID), "cancel")
			for _, id := range ids {
				if got := reloadStatus(t, p.db, id); got != orders.StatusCancelled {
					t.Errorf("leg %d from %s = %s after cancel, want cancelled", id, status, got)
				}
			}
		})
	}
}

// TestKanbanPin_CancelLeavesAnotherFlowsOrderLive: an order of another flow at a
// changing node, not linked to a task, outlives the cancel. Only the next START's
// cancelPreDispatchAtParticipants takes it.
func TestKanbanPin_CancelLeavesAnotherFlowsOrderLive(t *testing.T) {
	t.Parallel()
	p := seedKPPress(t)
	p.start(t)
	other := seedOrderAt(t, p.db, p.front, kpFront, "kp-other-flow", "KP-OLD", orders.StatusQueued)
	testutil.MustNoErr(t, p.eng.CancelProcessChangeover(p.processID), "cancel")
	if got := reloadStatus(t, p.db, other); got != orders.StatusQueued {
		t.Errorf("the other flow's order = %s after cancel, want queued", got)
	}
}

// TestKanbanPin_CancelLeavesAnUnlinkedOrderOfTheEpisodeLive: carrying the
// changeover's origin does not make an order the cancel's. An order attributed to
// the episode and linked to no task (a directive loader's REQUEST EMPTY at
// changeoverLoadOrigin, a keeper refill at liftingOrigin outside the spots) stays
// live with its origin.
func TestKanbanPin_CancelLeavesAnUnlinkedOrderOfTheEpisodeLive(t *testing.T) {
	t.Parallel()
	p := seedKPPress(t)
	co := p.start(t)
	origin := p.eng.changeoverOrigin(co.ID)
	if origin.ID == "" {
		t.Fatal("fixture: the changeover has no episode")
	}
	o, err := p.eng.orderMgr.CreateRetrieveOrder(orders.RetrieveSpec{
		Delivery: "KP-LOADER", Payload: "KP-NEW", Quantity: 1, Origin: origin})
	testutil.MustNoErr(t, err, "an order attributed to the episode")
	testutil.MustNoErr(t, p.eng.CancelProcessChangeover(p.processID), "cancel")
	got, err := p.db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload")
	if orders.IsTerminal(got.Status) {
		t.Errorf("the attributed order = %s after cancel, want it live", got.Status)
	}
	if got.OriginID != origin.ID {
		t.Errorf("origin = %q, want %q", got.OriginID, origin.ID)
	}
}

// ── the changeover episode ───────────────────────────────────────────────────

// TestKanbanPin_ChangeoverEpisodeOpensBlank: the row and the state message carry
// no payload and no node, open and close alike. Core holds nothing of the target
// style from this message.
func TestKanbanPin_ChangeoverEpisodeOpensBlank(t *testing.T) {
	t.Parallel()
	p := seedKPPress(t)
	co := p.start(t)
	open := kpOpenEpisode(t, p.eng, co.ID)
	if open == nil {
		t.Fatal("no open episode after START")
	}
	if open.PayloadCode != "" || open.CoreNodeName != "" {
		t.Errorf("open row payload=%q node=%q, want both blank", open.PayloadCode, open.CoreNodeName)
	}
	if open.ProcessID != "KP-PROC" || open.TriggerRef != fmt.Sprintf("changeover:%d", co.ID) {
		t.Errorf("open row process=%q trigger=%q", open.ProcessID, open.TriggerRef)
	}
	testutil.MustNoErr(t, p.eng.CancelProcessChangeover(p.processID), "cancel")
	var seen int
	for _, s := range decodeOriginStates(t, p.db) {
		if s.Kind != protocol.EpisodeKindChangeover {
			continue
		}
		seen++
		if s.PayloadCode != "" || s.CoreNodeName != "" {
			t.Errorf("state message (closed=%v) payload=%q node=%q, want both blank", s.ClosedAt != nil, s.PayloadCode, s.CoreNodeName)
		}
	}
	if seen != 2 {
		t.Errorf("changeover state messages = %d, want open and close", seen)
	}
}

// TestKanbanPin_ChangeoverOpenIgnoresAFailedEnqueue: the outbox refuses the OPEN,
// START goes on regardless. The episode is open on the Edge, stamped on the
// changeover and its legs carry it; Core is told nothing until the close.
func TestKanbanPin_ChangeoverOpenIgnoresAFailedEnqueue(t *testing.T) {
	t.Parallel()
	p := seedKPPress(t)
	kpFailInserts(t, p.db, "outbox", fmt.Sprintf("NEW.msg_type = '%s'", protocol.SubjectDemandOrigin))
	co := p.start(t)
	open := kpOpenEpisode(t, p.eng, co.ID)
	if open == nil {
		t.Fatal("no open episode after START")
	}
	if stored, err := p.db.GetChangeoverOriginID(co.ID); err != nil || stored != open.OriginID {
		t.Errorf("changeover origin = %q (%v), want %q", stored, err, open.OriginID)
	}
	if states := decodeOriginStates(t, p.db); len(states) != 0 {
		t.Errorf("state messages = %d, want 0: the outbox refused the OPEN", len(states))
	}
	legs := kpLegs(t, p.db, co.ID)
	leg, err := p.db.GetOrder(legs[kpFront][0])
	testutil.MustNoErr(t, err, "leg")
	if leg.OriginID != open.OriginID {
		t.Errorf("leg origin = %q, want the episode %q", leg.OriginID, open.OriginID)
	}
}

// TestKanbanPin_StartWhoseApplyFailsLeavesTheEpisodeOpen: applyChangeoverPlan
// returns nothing. A leg that cannot be created marks its task in error for the
// operator; START returns no error, the changeover is active, the episode stays
// open and no close goes out.
func TestKanbanPin_StartWhoseApplyFailsLeavesTheEpisodeOpen(t *testing.T) {
	t.Parallel()
	p := seedKPPress(t)
	kpFailInserts(t, p.db, "orders", "1")
	co := p.start(t)
	tasks, err := p.db.ListChangeoverNodeTasks(co.ID)
	testutil.MustNoErr(t, err, "tasks")
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(tasks))
	}
	for _, task := range tasks {
		if task.State != domain.NodeTaskError {
			t.Errorf("task %s = %s, want %s", task.NodeName, task.State, domain.NodeTaskError)
		}
	}
	if active, err := p.db.GetActiveProcessChangeover(p.processID); err != nil || active.ID != co.ID {
		t.Errorf("active changeover = %+v (%v), want %d", active, err, co.ID)
	}
	if kpOpenEpisode(t, p.eng, co.ID) == nil {
		t.Error("the episode closed; want it left open")
	}
	for _, s := range decodeOriginStates(t, p.db) {
		if s.ClosedAt != nil {
			t.Errorf("a close went out: %+v", s)
		}
	}
}

// TestKanbanPin_ALegTheApplyFailedToLinkOutlivesTheCancel: two-robot. The supply
// is created, the evac cannot be, and the node's apply returns before it links
// anything: the task names neither leg, so the cancel cannot find the supply. It
// stays live. This is the one changeover-created order the cancel leaves.
func TestKanbanPin_ALegTheApplyFailedToLinkOutlivesTheCancel(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, toStyleID := seedPhase3SwapScenarioMode(t, db, protocol.SwapModeTwoRobot)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	// The evac is the leg that lifts the old part.
	kpFailInserts(t, db, "orders", "NEW.payload_code = 'PART-OLD'")
	co, err := eng.StartProcessChangeover(processID, toStyleID, "test", "kanban pin")
	testutil.MustNoErr(t, err, "start")
	if legs := kpLegs(t, db, co.ID); len(legs) != 0 {
		t.Fatalf("task-linked legs = %v, want none: the apply returned before linking", legs)
	}
	live, err := db.ListActiveOrders()
	testutil.MustNoErr(t, err, "live")
	if len(live) != 1 || live[0].PayloadCode != "PART-NEW" {
		t.Fatalf("live orders = %+v, want the supply alone", live)
	}
	supply := live[0].ID
	testutil.MustNoErr(t, eng.CancelProcessChangeover(processID), "cancel")
	got, err := db.GetOrder(supply)
	testutil.MustNoErr(t, err, "reload")
	if orders.IsTerminal(got.Status) {
		t.Errorf("the unlinked supply = %s after cancel, want it live", got.Status)
	}
}
