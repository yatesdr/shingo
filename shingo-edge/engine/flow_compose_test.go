package engine

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingo/shared/scenefixtures"
	"shingoedge/domain"
	"shingoedge/engine/changeover"
	"shingoedge/internal/testdb"
	"shingoedge/orders"
	"shingoedge/service"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// flow_compose_test.go — PreviewFlow, SaveFlow, FlowFingerprint, and the
// parity pin: a draft previewed through the seam and the same flow previewed
// after it was saved are the same preview.

// fakeFlowPoster is the Core preflight endpoint, in memory.
type fakeFlowPoster struct {
	missing []string
	err     error
	asked   []string
}

func (f *fakeFlowPoster) Available() bool { return true }
func (f *fakeFlowPoster) PreflightInventory(_ string, payloads []string) (*service.PreflightCoreResult, error) {
	f.asked = append([]string(nil), payloads...)
	if f.err != nil {
		return nil, f.err
	}
	return &service.PreflightCoreResult{Missing: f.missing}, nil
}

// flowState is everything a preview must leave alone and a refused save
// must not touch: the claim rows (with their updated_at), the outbox, the
// process nodes and the orders.
type flowState struct {
	claims     []processes.NodeClaim
	outboxRows int
	nodeRows   int
	orderRows  int
}

func snapshotFlowState(t *testing.T, db *store.DB, styleIDs ...int64) flowState {
	t.Helper()
	var s flowState
	for _, id := range styleIDs {
		claims, err := db.ListStyleNodeClaims(id)
		testutil.MustNoErr(t, err, "list claims")
		s.claims = append(s.claims, claims...)
	}
	count := func(table string) int {
		var n int
		testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&n), "count "+table)
		return n
	}
	s.outboxRows, s.nodeRows, s.orderRows = count("outbox"), count("process_nodes"), count("orders")
	return s
}

func cellsOf(t *testing.T, db *store.DB, styleID int64) []domain.FlowCell {
	t.Helper()
	claims, err := db.ListStyleNodeClaims(styleID)
	testutil.MustNoErr(t, err, "list claims")
	out := make([]domain.FlowCell, 0, len(claims))
	for _, c := range claims {
		out = append(out, domain.Collapse(c))
	}
	return out
}

// seedFlowScenario is a process the composer can work on today: every claim
// in a configurable mode and valid under the store and the validator. From
// (active) runs FLOW-SWAP/PART-OLD, FLOW-SAME/PART-SAME, FLOW-GONE/PART-GONE;
// to runs FLOW-SWAP/PART-NEW, FLOW-SAME/PART-SAME, FLOW-ADD/PART-ADD. So a
// changeover swaps one node, leaves one alone, drops one and adds one.
func seedFlowScenario(t *testing.T, db *store.DB) (processID, fromStyleID, toStyleID int64) {
	t.Helper()
	var err error
	processID, err = db.CreateProcess("FLOW-PROC", "flow composer", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	for i, name := range []string{"FLOW-SWAP", "FLOW-SAME", "FLOW-GONE", "FLOW-ADD"} {
		id, err := db.CreateProcessNode(processes.NodeInput{ProcessID: processID, CoreNodeName: name, Code: name, Name: name, Sequence: i + 1, Enabled: true})
		testutil.MustNoErr(t, err, "create node "+name)
		db.EnsureProcessNodeRuntime(id)
	}
	fromStyleID, err = db.CreateStyle("FLOW-FROM", "", processID)
	testutil.MustNoErr(t, err, "create from style")
	toStyleID, err = db.CreateStyle("FLOW-TO", "", processID)
	testutil.MustNoErr(t, err, "create to style")
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &fromStyleID), "set active style")
	claim := func(styleID int64, node, payload string) {
		_, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
			StyleID: styleID, CoreNodeName: node, Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeTwoRobot,
			PayloadCode: payload, InboundStaging: "FLOW-STG", InboundSource: "FLOW-SRC", OutboundDestination: "FLOW-DST",
		})
		testutil.MustNoErr(t, err, "claim "+node)
	}
	claim(fromStyleID, "FLOW-SWAP", "PART-OLD")
	claim(fromStyleID, "FLOW-SAME", "PART-SAME")
	claim(fromStyleID, "FLOW-GONE", "PART-GONE")
	claim(toStyleID, "FLOW-SWAP", "PART-NEW")
	claim(toStyleID, "FLOW-SAME", "PART-SAME")
	claim(toStyleID, "FLOW-ADD", "PART-ADD")
	return processID, fromStyleID, toStyleID
}

// TestPreviewFlow_NeverWrites: a preview cancels nothing, clears nothing,
// enqueues nothing and creates no row — with a queued order sitting at the
// very node the changeover would touch, which is what Start would cancel.
func TestPreviewFlow_NeverWrites(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, fromStyleID, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)
	queued := seedOrderAt(t, db, nodeID, "CO-NODE", "uuid-preview", "PART-OLD", orders.StatusQueued)
	before := snapshotFlowState(t, db, fromStyleID, toStyleID)

	preview, err := eng.PreviewFlow(context.Background(), processID, FlowPreviewRequest{ToStyleID: toStyleID, Cells: cellsOf(t, db, toStyleID)})
	testutil.MustNoErr(t, err, "preview")
	if preview.Orders() == 0 || len(preview.Actions) == 0 {
		t.Fatalf("preview planned nothing: %+v", preview)
	}
	if got := reloadStatus(t, db, queued); got != orders.StatusQueued {
		t.Errorf("the queued order is %s after a preview; a preview cancels nothing", got)
	}
	if after := snapshotFlowState(t, db, fromStyleID, toStyleID); !reflect.DeepEqual(after, before) {
		t.Errorf("a preview changed stored state:\n before %+v\n after  %+v", before, after)
	}
	if preview.Fingerprint == "" || len(preview.Unresolved) != 0 || preview.Preflight.State != FlowPreflightUnchecked {
		t.Errorf("preview shape: fingerprint %q, unresolved %v, preflight %+v", preview.Fingerprint, preview.Unresolved, preview.Preflight)
	}
}

// TestPreviewFlow_ZeroOrdersIsRefusedByName: a flow that changes nothing is
// a *FlowNoOrdersError carrying the preview, not a 200 with an empty list.
func TestPreviewFlow_ZeroOrdersIsRefusedByName(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, toStyleID := seedNoChangeScenario(t, db)
	eng := testEngine(t, db)

	preview, err := eng.PreviewFlow(context.Background(), processID, FlowPreviewRequest{ToStyleID: toStyleID, Cells: cellsOf(t, db, toStyleID)})
	var noOrders *FlowNoOrdersError
	if !errors.As(err, &noOrders) {
		t.Fatalf("err = %v, want *FlowNoOrdersError", err)
	}
	if noOrders.Preview == nil || preview == nil || preview.Orders() != 0 {
		t.Errorf("the refusal must carry the preview with order_count 0: %+v", noOrders.Preview)
	}
	if noOrders.Error() == "" {
		t.Error("the refusal names no reason")
	}
}

// TestPreviewFlow_RefusesInTheSeamsWords: an active changeover is refused with
// the sentinel the desktop preview uses.
//
// THE RUNNING STYLE IS NOT ON THIS LIST ANY MORE (owner ruling R3,
// 2026-09-12); see TestPreviewFlow_RunningStyleIsValidatedAndNotPlanned.
func TestPreviewFlow_RefusesInTheSeamsWords(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)

	eng.wireEventHandlers()
	if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", ""); err != nil {
		t.Fatalf("start: %v", err)
	}
	_, err := eng.PreviewFlow(context.Background(), processID, FlowPreviewRequest{ToStyleID: toStyleID})
	if !errors.Is(err, ErrChangeoverActive) {
		t.Errorf("active changeover: err = %v, want ErrChangeoverActive", err)
	}
}

// TestPreviewFlow_RunningStyleIsValidatedAndNotPlanned pins owner ruling R3:
// a preview of the style on the press answers with findings and a fingerprint
// and NO count — absent, not zero, because zero is the blocked bar's answer
// and a running style whose saved changes land at the next changeover is not
// blocked.
func TestPreviewFlow_RunningStyleIsValidatedAndNotPlanned(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, fromStyleID, _, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)

	preview, err := eng.PreviewFlow(context.Background(), processID, FlowPreviewRequest{
		ToStyleID: fromStyleID, Cells: cellsOf(t, db, fromStyleID),
	})
	testutil.MustNoErr(t, err, "preview of the running style")
	if preview.OrderCount != nil {
		t.Errorf("order_count = %d on the running style; it must be ABSENT — nothing was planned", *preview.OrderCount)
	}
	if !preview.Running {
		t.Error("the preview does not say it is the running style, so the bar cannot tell it from a preview that failed to plan")
	}
	if len(preview.Actions) != 0 {
		t.Errorf("actions = %+v on the running style; no changeover was planned", preview.Actions)
	}
	if preview.Fingerprint == "" {
		t.Error("no fingerprint: the save that follows has nothing to be judged against")
	}
	// The same rows, previewed while NOT running, give the same fingerprint —
	// what a save is judged against is the stored rows and not the press.
	other, err := eng.PreviewFlow(context.Background(), processID, FlowPreviewRequest{
		ToStyleID: fromStyleID, Cells: cellsOf(t, db, fromStyleID),
	})
	testutil.MustNoErr(t, err, "second preview")
	if other.Fingerprint != preview.Fingerprint {
		t.Errorf("fingerprint moved between two previews of the same rows: %q then %q", preview.Fingerprint, other.Fingerprint)
	}
}

// TestPreviewFlow_AbsentCellsAreDrops: a request carries the WHOLE flow, so a
// stored claim it leaves out is a deletion and its node plans as a drop.
//
// THERE IS NO PARTIAL MODE ANY MORE. replace_all existed as a `false` nobody
// set — all six producers sent true — and it was the one path on which the
// preview and the save that followed it validated different sets: a partial
// preview validated the carried claims the save would never touch, so a draft
// could preview clean and save a flow nobody had previewed.
func TestPreviewFlow_AbsentCellsAreDrops(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, toStyleID := seedMultiNodeScenario(t, db)
	eng := testEngine(t, db)
	var swapOnly []domain.FlowCell
	for _, c := range cellsOf(t, db, toStyleID) {
		if c.CoreNodeName == "NODE-SWAP" {
			swapOnly = append(swapOnly, c)
		}
	}
	situations := func(p *FlowPreview) map[string]string {
		out := map[string]string{}
		for _, a := range p.Actions {
			out[a.CoreNodeName] = a.Situation
		}
		return out
	}

	whole, err := eng.PreviewFlow(context.Background(), processID, FlowPreviewRequest{ToStyleID: toStyleID, Cells: swapOnly})
	testutil.MustNoErr(t, err, "whole-flow preview")
	if got := situations(whole); got["NODE-UNCHANGED"] != "drop" || got["NODE-ADD"] != "" {
		t.Errorf("situations = %v; NODE-UNCHANGED's claim is gone (drop) and NODE-ADD never arrives", got)
	}

	// AND AN EMPTY REQUEST IS AN EMPTY FLOW, not "leave it alone". This is the
	// sharp edge of the whole-flow rule and it is the honest reading: the
	// picture IS the flow, so a picture with nothing on it is a style with no
	// flow. Worth its own assertion because the old default was the opposite.
	empty, err := eng.PreviewFlow(context.Background(), processID, FlowPreviewRequest{ToStyleID: toStyleID})
	testutil.MustNoErr(t, err, "empty preview")
	for node, sit := range situations(empty) {
		if sit != "drop" {
			t.Errorf("an empty request planned %s as %q; every stored claim is a drop", node, sit)
		}
	}
}

// TestPreviewFlow_PreflightStates: unchecked without Core, ok and missing
// with it — and a failing Core call is unchecked, never a failed preview.
//
// AND UNCHECKED WHENEVER THE CALLER DID NOT ASK (owner, 2026-09-13). The
// preflight is one Core HTTP call, and the composer previews every 400 ms
// while a flow is edited; only the confirm sheet reads the answer, and the
// desktop never reads it at all. So Preflight on the request is what makes
// the call, and every other preview gets the unchecked state that already
// existed for "Core could not be asked".
func TestPreviewFlow_PreflightStates(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)
	req := FlowPreviewRequest{ToStyleID: toStyleID, Cells: cellsOf(t, db, toStyleID), Preflight: true}

	p, err := eng.PreviewFlow(context.Background(), processID, req)
	testutil.MustNoErr(t, err, "no core")
	if p.Preflight.State != FlowPreflightUnchecked || len(p.Preflight.Missing) != 0 {
		t.Errorf("without Core: %+v, want unchecked", p.Preflight)
	}

	eng.coreClient = NewCoreClient(testCoreURL)
	poster := &fakeFlowPoster{}
	eng.preflightChecker = service.NewPreflightChecker(db, poster, "test.station")
	p, err = eng.PreviewFlow(context.Background(), processID, req)
	testutil.MustNoErr(t, err, "core ok")
	if p.Preflight.State != FlowPreflightOK || !reflect.DeepEqual(poster.asked, []string{"PART-NEW"}) {
		t.Errorf("with Core: %+v asked %v, want ok over [PART-NEW]", p.Preflight, poster.asked)
	}

	poster.missing = []string{"PART-NEW"}
	p, err = eng.PreviewFlow(context.Background(), processID, req)
	testutil.MustNoErr(t, err, "core missing")
	if p.Preflight.State != FlowPreflightMissing || !reflect.DeepEqual(p.Preflight.Missing, []string{"PART-NEW"}) {
		t.Errorf("missing stock: %+v", p.Preflight)
	}

	poster.err = errors.New("core: HTTP 500")
	p, err = eng.PreviewFlow(context.Background(), processID, req)
	testutil.MustNoErr(t, err, "core failing must not fail the preview")
	if p.Preflight.State != FlowPreflightUnchecked {
		t.Errorf("failing Core: %+v, want unchecked", p.Preflight)
	}

	// THE EDIT LOOP DOES NOT ASK. Same engine, same Core, same cells, one
	// field different — and no call is made.
	poster.err, poster.missing, poster.asked = nil, nil, nil
	quiet := FlowPreviewRequest{ToStyleID: toStyleID, Cells: cellsOf(t, db, toStyleID)}
	p, err = eng.PreviewFlow(context.Background(), processID, quiet)
	testutil.MustNoErr(t, err, "preview without preflight")
	if p.Preflight.State != FlowPreflightUnchecked {
		t.Errorf("a preview that did not ask reports %+v, want unchecked", p.Preflight)
	}
	if len(poster.asked) != 0 {
		t.Errorf("a preview that did not ask still called Core with %v — that is one network "+
			"round trip per keystroke on the edit loop", poster.asked)
	}
}

// TestPreviewFlow_FindingsFromValidatorAndReadiness: a cell the validator
// refuses is a to-side finding on its field; a pair the planner would refuse
// is a readiness finding; both in the one sorted list, and only errors.
func TestPreviewFlow_FindingsFromValidatorAndReadiness(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, fromStyleID, toStyleID := seedFlowScenario(t, db)
	eng := testEngine(t, db)
	// A legacy from row with no destination: the store refuses writing one
	// today, so it is blanked the way an old row would hold it.
	from, err := db.GetStyleNodeClaimByNode(fromStyleID, "FLOW-SWAP")
	testutil.MustNoErr(t, err, "from claim")
	_, err = db.Exec(`UPDATE style_node_claims SET outbound_destination='' WHERE id=?`, from.ID)
	testutil.MustNoErr(t, err, "blank destination")
	cells := cellsOf(t, db, toStyleID)
	for i := range cells {
		if cells[i].CoreNodeName == "FLOW-SWAP" {
			cells[i].PayloadCode = ""
		}
	}

	p, err := eng.PreviewFlow(context.Background(), processID, FlowPreviewRequest{ToStyleID: toStyleID, Cells: cells})
	if err != nil {
		var noOrders *FlowNoOrdersError
		if !errors.As(err, &noOrders) {
			t.Fatalf("preview: %v", err)
		}
		p = noOrders.Preview
	}
	got := map[string]bool{}
	for _, f := range p.Findings {
		if f.Severity != domain.SeverityError {
			t.Errorf("a non-error finding reached the preview: %+v", f)
		}
		got[f.CoreNodeName+"/"+string(f.Side)+"/"+string(f.Field)] = true
	}
	if !got["FLOW-SWAP/to/payload_code"] {
		t.Errorf("no to-side payload_code finding for a cell with no part: %+v", p.Findings)
	}
	if !got["FLOW-SWAP/from/outbound_destination"] {
		t.Errorf("no from-side outbound_destination readiness finding: %+v", p.Findings)
	}
	// THE THIRD IS OWNER RULING R8'S, and this draft earns it: blanking
	// FLOW-SWAP's payload leaves PART-NEW — a part the stored flow names —
	// on no position at all. It carries NO node, which is the whole point:
	// "which position?" is what it is asking.
	if !got["/to/payload_code"] {
		t.Errorf("no unplaced-part finding for a draft that dropped PART-NEW's only position: %+v", p.Findings)
	}
	for _, f := range p.Findings {
		if f.CoreNodeName == "" && f.Message != "1 part needs a position: PART-NEW" {
			t.Errorf("the unplaced-part finding does not name the part: %q", f.Message)
		}
	}
	if len(got) != 3 {
		t.Errorf("findings = %+v, want exactly the three", p.Findings)
	}
}

// TestSaveFlow_RunningStyleSavesAndStartsNothing is owner ruling R3's first
// half: a save to the style on the press writes its rows and starts nothing —
// no changeover row, no orders, post_cutover untouched — and its fingerprint
// is the one the same rows give when it is not the running style.
func TestSaveFlow_RunningStyleSavesAndStartsNothing(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, fromStyleID, toStyleID := seedFlowScenario(t, db)
	eng := testEngine(t, db)
	enableComposer(t, db, processID)

	cells := cellsOf(t, db, fromStyleID)
	for i := range cells {
		cells[i].InboundSource = "FLOW-SRC-2" // a source change: the flexible half of R3
	}
	// THE PREVIEW'S FINGERPRINT IS THE SAVE'S. A preview of the running style
	// plans nothing, so this is the one thing it still has to get right: the
	// save is judged against it, and a preview that handed back a fingerprint
	// the save recomputes differently would 409 every time.
	//
	// (It is NOT a hash of the target's rows alone — FlowFingerprint spans the
	// active style, its claims, the target and the routing set, because a
	// preview is a plan over both sides. For the running style both sides are
	// the same rows, which is why this comes out stable; see the report.)
	preview, err := eng.PreviewFlow(context.Background(), processID, FlowPreviewRequest{
		ToStyleID: fromStyleID, Cells: cells,
	})
	testutil.MustNoErr(t, err, "preview the running style")
	fp := preview.Fingerprint

	before := snapshotFlowState(t, db, toStyleID)
	res, err := eng.SaveFlow(processID, FlowSaveRequest{
		Source: domain.ClaimSourceHMI, ToStyleID: fromStyleID, Cells: cells,
		Fingerprint: fp, CalledBy: "Press 400",
	})
	testutil.MustNoErr(t, err, "save to the running style")
	if res.Written != len(cells) {
		t.Errorf("wrote %d of %d cells", res.Written, len(cells))
	}
	saved, err := db.ListStyleNodeClaims(fromStyleID)
	testutil.MustNoErr(t, err, "reload")
	for _, c := range saved {
		if c.InboundSource != "FLOW-SRC-2" {
			t.Errorf("%s kept %q: a mid-run source change is what the next trip reads", c.CoreNodeName, c.InboundSource)
		}
	}
	// Nothing started. A changeover row, an order, or a touched node count
	// would all be this save doing something to the press.
	if _, err := db.GetActiveProcessChangeover(processID); err == nil {
		t.Error("the save started a changeover; R3 says it writes rows and starts nothing")
	}
	if after := snapshotFlowState(t, db, toStyleID); after.orderRows != before.orderRows || after.nodeRows != before.nodeRows {
		t.Errorf("the save moved something outside the style's own rows:\n before %+v\n after  %+v", before, after)
	}
}

// TestSaveFlow_RunningStyleRefusesAPositionMove is owner ruling R3's other
// half, and the reason it has one. Sources, destinations and routes save
// freely mid-run; MOVING the running style off a position while its bin is on
// it is refused by name, because every runtime reader keys on the node name —
// see refuseRunningPositionMove for the list and what each one does.
func TestSaveFlow_RunningStyleRefusesAPositionMove(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, fromStyleID, _ := seedFlowScenario(t, db)
	eng := testEngine(t, db)
	enableComposer(t, db, processID)
	before := snapshotFlowState(t, db, fromStyleID)
	fp, err := eng.FlowFingerprint(processID, fromStyleID)
	testutil.MustNoErr(t, err, "fingerprint")

	moved := cellsOf(t, db, fromStyleID)
	for i := range moved {
		if moved[i].CoreNodeName == "FLOW-GONE" {
			moved[i].CoreNodeName = "FLOW-ADD" // the same part, a different position
		}
	}
	_, err = eng.SaveFlow(processID, FlowSaveRequest{
		Source: domain.ClaimSourceHMI, ToStyleID: fromStyleID, Cells: moved,
		Fingerprint: fp, CalledBy: "Press 400",
	})
	if !errors.Is(err, ErrRunningPositionMove) {
		t.Fatalf("moving a running position: err = %v, want ErrRunningPositionMove", err)
	}
	// BY NAME, both ends: which position is being left and which is being
	// taken, so the screen can say which is which.
	for _, want := range []string{"FLOW-FROM", "FLOW-GONE", "FLOW-ADD"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
	if after := snapshotFlowState(t, db, fromStyleID); !reflect.DeepEqual(after, before) {
		t.Errorf("a refused save wrote something:\n before %+v\n after  %+v", before, after)
	}
}

// TestSaveFlow_APartLeftWithNoPositionIsAFindingAndStillSaves is owner ruling
// R8 (2026-09-12), both halves at once.
//
// An apply that leaves parts unplaced SAVES — the engineer is part way through
// moving a part onto a new shape, and a half-built flow is not a reason to lose
// the work. What it may not do is go unsaid: the preview reports it as an error
// finding naming the parts, which is what takes the START away on both
// surfaces.
func TestSaveFlow_APartLeftWithNoPositionIsAFindingAndStillSaves(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, toStyleID := seedFlowScenario(t, db)
	eng := testEngine(t, db)
	enableComposer(t, db, processID)

	// Keep one of the target's three cells and drop the other two: the shape
	// an apply writes when the preset names fewer positions than the style
	// was running. PART-SAME carries across on FLOW-SAME; PART-NEW and
	// PART-ADD are left with nowhere to sit.
	var kept []domain.FlowCell
	for _, c := range cellsOf(t, db, toStyleID) {
		if c.CoreNodeName == "FLOW-SAME" {
			kept = append(kept, c)
		}
	}
	if len(kept) != 1 {
		t.Fatalf("the fixture no longer has FLOW-SAME to keep: %d cells", len(kept))
	}

	preview, err := eng.PreviewFlow(context.Background(), processID, FlowPreviewRequest{
		ToStyleID: toStyleID, Cells: kept,
	})
	if err != nil {
		var noOrders *FlowNoOrdersError
		if !errors.As(err, &noOrders) {
			t.Fatalf("preview: %v", err)
		}
		preview = noOrders.Preview
	}
	var loose *domain.NodeFinding
	for i := range preview.Findings {
		if preview.Findings[i].CoreNodeName == "" {
			loose = &preview.Findings[i]
		}
	}
	if loose == nil {
		t.Fatalf("no unplaced-part finding for a draft that dropped two parts' only positions: %+v", preview.Findings)
	}
	// BY NAME, AND WITH NO NODE. "Which position?" is the question it exists
	// to ask, so naming one would be answering it.
	for _, want := range []string{"2 parts need a position", "PART-ADD", "PART-NEW"} {
		if !strings.Contains(loose.Message, want) {
			t.Errorf("the finding does not name %s: %q", want, loose.Message)
		}
	}
	if loose.Severity != domain.SeverityError {
		t.Errorf("severity = %q; the composer shows errors only", loose.Severity)
	}

	// AND IT SAVES. The rows the request named are written and the rest are
	// dropped; the finding took nothing away from the save.
	res, err := eng.SaveFlow(processID, FlowSaveRequest{
		Source: domain.ClaimSourceAdmin, CalledBy: "apply", ToStyleID: toStyleID,
		Cells: kept, Fingerprint: preview.Fingerprint,
	})
	if err != nil {
		t.Fatalf("a save that unplaces parts was refused: %v", err)
	}
	if res.Written != 1 || res.Deleted != 2 {
		t.Errorf("wrote %d / deleted %d, want 1 / 2", res.Written, res.Deleted)
	}
	after, err := db.ListStyleNodeClaims(toStyleID)
	testutil.MustNoErr(t, err, "reload")
	if len(after) != 1 || after[0].CoreNodeName != "FLOW-SAME" {
		t.Errorf("after the save the style runs %+v, want FLOW-SAME alone", after)
	}
	// AND THE FINDING IS GONE, because a style's parts ARE its claims: the
	// parts that lost their positions left the style with them. That is why
	// this is a DRAFT-time finding and there is nothing for a start to
	// re-check — see the report's R8 answer.
	next, err := eng.PreviewFlow(context.Background(), processID, FlowPreviewRequest{
		ToStyleID: toStyleID, Cells: kept,
	})
	if err != nil {
		var noOrders *FlowNoOrdersError
		if !errors.As(err, &noOrders) {
			t.Fatalf("second preview: %v", err)
		}
		next = noOrders.Preview
	}
	for _, f := range next.Findings {
		if f.CoreNodeName == "" {
			t.Errorf("the saved flow still reports a part with no position: %q", f.Message)
		}
	}
}

func enableComposer(t *testing.T, db *store.DB, processID int64) {
	t.Helper()
	testutil.MustNoErr(t, db.SetFlowComposerEnabled(processID, true), "enable composer")
}

// TestSaveFlow_RefusalsHaveNoSideEffects: gate off, stale fingerprint and an
// invalid cell are each refused with nothing written — no row, no updated_at,
// no outbox row.
//
// THE RUNNING STYLE IS NOT ONE OF THEM ANY MORE (owner ruling R3,
// 2026-09-12); see TestSaveFlow_RunningStyleSavesAndStartsNothing and
// TestSaveFlow_RunningStyleRefusesAPositionMove.
func TestSaveFlow_RefusalsHaveNoSideEffects(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, fromStyleID, toStyleID := seedFlowScenario(t, db)
	eng := testEngine(t, db)
	cells := cellsOf(t, db, toStyleID)
	cells[0].PayloadCode = "PART-NEWER"
	before := snapshotFlowState(t, db, fromStyleID, toStyleID)
	fp, err := eng.FlowFingerprint(processID, toStyleID)
	testutil.MustNoErr(t, err, "fingerprint")

	_, err = eng.SaveFlow(processID, FlowSaveRequest{Source: domain.ClaimSourceHMI, ToStyleID: toStyleID, Cells: cells, Fingerprint: fp, CalledBy: "Press 400"})
	if !errors.Is(err, ErrFlowComposerDisabled) {
		t.Errorf("gate off: err = %v, want ErrFlowComposerDisabled", err)
	}
	enableComposer(t, db, processID)

	_, err = eng.SaveFlow(processID, FlowSaveRequest{Source: domain.ClaimSourceHMI, ToStyleID: toStyleID, Cells: cells, Fingerprint: "stale", CalledBy: "Press 400"})
	if !errors.Is(err, ErrFlowStale) {
		t.Errorf("stale: err = %v, want ErrFlowStale", err)
	}

	bad := append([]domain.FlowCell(nil), cells...)
	bad = append(bad, domain.FlowCell{CoreNodeName: "CO-NEW", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeTwoRobot, PayloadCode: "",
		InboundStaging: "FLOW-STG", InboundSource: "FLOW-SRC", OutboundDestination: "FLOW-DST"})
	_, err = eng.SaveFlow(processID, FlowSaveRequest{Source: domain.ClaimSourceHMI, ToStyleID: toStyleID, Cells: bad, Fingerprint: fp, CalledBy: "Press 400"})
	var invalid *FlowValidationError
	if !errors.As(err, &invalid) || len(invalid.Findings) == 0 || invalid.Findings[0].CoreNodeName != "CO-NEW" {
		t.Errorf("invalid cell: err = %v, want *FlowValidationError naming CO-NEW", err)
	}

	if after := snapshotFlowState(t, db, fromStyleID, toStyleID); !reflect.DeepEqual(after, before) {
		t.Errorf("a refused save changed stored state:\n before %+v\n after  %+v", before, after)
	}
	again, err := eng.FlowFingerprint(processID, toStyleID)
	testutil.MustNoErr(t, err, "eng.FlowFingerprint")
	if again != fp {
		t.Error("the fingerprint moved under refused saves")
	}
}

// TestSaveFlow_WritesTheFlowStampedHMI: cells are written through UpsertClaim
// with source hmi and the station's name, a replace-all deletes what the
// request left out, a cell carrying a setting the composer does not show is
// written like any other and keeps that setting, and the returned fingerprint
// is the one a start must now carry.
// TestSaveFlow_RefusesASaveThatDoesNotSayWhoMadeIt: the guard in front of the
// whole call, and the reason it is a refusal.
//
// MaterializeClaim reads an EMPTY source as domain.ClaimSourceAdmin, so a
// caller who forgot would write "a person on the desktop did this" onto every
// claim it touched — silently, and wrongly, which is the failure mode R1
// exists to end. Refused before the gate, the seam and the fingerprint, so a
// caller finds out on the first call rather than on a process that happens to
// have the composer enabled.
func TestSaveFlow_RefusesASaveThatDoesNotSayWhoMadeIt(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	for _, src := range []string{"", "operator_station", "generated"} {
		_, err := eng.SaveFlow(1, FlowSaveRequest{ToStyleID: 1, Source: src})
		if !errors.Is(err, ErrFlowSourceUnset) {
			t.Errorf("SaveFlow with source %q: err = %v, want ErrFlowSourceUnset", src, err)
		}
	}
}

func TestSaveFlow_WritesTheFlowStampedHMI(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, toStyleID := seedFlowScenario(t, db)
	eng := testEngine(t, db)
	enableComposer(t, db, processID)
	// FLOW-SAME's claim gets a reorder point so it collapses locked. It used
	// to be given a capacity, which no longer locks anything: capacity is
	// resolved from the payload catalog, not authored on the claim.
	same, err := db.GetStyleNodeClaimByNode(toStyleID, "FLOW-SAME")
	testutil.MustNoErr(t, err, "get claim")
	in := domain.InputFromClaim(*same)
	in.ReorderPoint = 500
	_, err = db.UpsertStyleNodeClaim(in)
	testutil.MustNoErr(t, err, "lock claim")

	fp, err := eng.FlowFingerprint(processID, toStyleID)
	testutil.MustNoErr(t, err, "fingerprint")
	var cells []domain.FlowCell
	for _, c := range cellsOf(t, db, toStyleID) {
		switch c.CoreNodeName {
		case "FLOW-SWAP":
			c.PayloadCode = "PART-NEWER"
			cells = append(cells, c)
		case "FLOW-SAME":
			// Carries a reorder point the composer does not show. It is
			// written like any other cell now; the reorder point survives,
			// which the assertion after the save says.
			cells = append(cells, c)
		}
		// FLOW-ADD is left out: under replace-all it is deleted.
	}
	cells = append(cells, domain.FlowCell{CoreNodeName: "FLOW-FRESH", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeTwoRobot,
		PayloadCode: "PART-FRESH", InboundStaging: "FLOW-STG", InboundSource: "FLOW-SRC", OutboundDestination: "FLOW-DST"})

	res, err := eng.SaveFlow(processID, FlowSaveRequest{Source: domain.ClaimSourceHMI, ToStyleID: toStyleID, Cells: cells, Fingerprint: fp, CalledBy: "Press 400"})
	testutil.MustNoErr(t, err, "save")
	if res.Written != 3 || res.Deleted != 1 {
		t.Errorf("written %d deleted %d, want 3 written (swap + same + fresh) and 1 deleted (FLOW-ADD)", res.Written, res.Deleted)
	}
	after, err := db.ListStyleNodeClaims(toStyleID)
	testutil.MustNoErr(t, err, "list after")
	byNode := map[string]processes.NodeClaim{}
	for _, c := range after {
		byNode[c.CoreNodeName] = c
	}
	if _, gone := byNode["FLOW-ADD"]; gone {
		t.Error("FLOW-ADD's claim survived a replace-all that left it out")
	}
	if c := byNode["FLOW-SWAP"]; c.PayloadCode != "PART-NEWER" || c.Source != domain.ClaimSourceHMI || c.CalledBy != "Press 400" {
		t.Errorf("FLOW-SWAP = %+v, want PART-NEWER stamped hmi / Press 400", c)
	}
	if c := byNode["FLOW-FRESH"]; c.PayloadCode != "PART-FRESH" || c.Source != domain.ClaimSourceHMI {
		t.Errorf("FLOW-FRESH = %+v, want a new hmi claim", c)
	}
	// Written by the operator — and its reorder point, which the composer
	// never showed, came through the write untouched.
	if c := byNode["FLOW-SAME"]; c.Source != domain.ClaimSourceHMI || c.ReorderPoint != 500 {
		t.Errorf("FLOW-SAME = %+v, want written by hmi with reorder point 500 intact", c)
	}
	now, err := eng.FlowFingerprint(processID, toStyleID)
	testutil.MustNoErr(t, err, "fingerprint after")
	if res.Fingerprint != now || res.Fingerprint == fp {
		t.Errorf("returned fingerprint %q, recomputed %q, before %q — the save must return the NEW one", res.Fingerprint, now, fp)
	}
	// A second save with the old fingerprint is stale; with the new one it lands.
	if _, err := eng.SaveFlow(processID, FlowSaveRequest{Source: domain.ClaimSourceHMI, ToStyleID: toStyleID, Cells: cells, Fingerprint: fp, CalledBy: "Press 400"}); !errors.Is(err, ErrFlowStale) {
		t.Errorf("old fingerprint after a save: err = %v, want ErrFlowStale", err)
	}
	if _, err := eng.SaveFlow(processID, FlowSaveRequest{Source: domain.ClaimSourceHMI, ToStyleID: toStyleID, Cells: cells, Fingerprint: res.Fingerprint, CalledBy: "Press 400"}); err != nil {
		t.Errorf("new fingerprint: %v", err)
	}
}

// TestFlowFingerprint_TracksTheStoredRows: the engine's fingerprint moves
// with a claim edit on either side and a routing flip, and not with an
// attribution-only rewrite.
func TestFlowFingerprint_TracksTheStoredRows(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, fromStyleID, toStyleID := seedFlowScenario(t, db)
	eng := testEngine(t, db)
	base, err := eng.FlowFingerprint(processID, toStyleID)
	testutil.MustNoErr(t, err, "base")

	from, err := db.GetStyleNodeClaimByNode(fromStyleID, "FLOW-SWAP")
	testutil.MustNoErr(t, err, "from claim")
	in := domain.InputFromClaim(*from)
	in.Source, in.CalledBy = domain.ClaimSourceHMI, "Press 400"
	_, err = db.UpsertStyleNodeClaim(in)
	testutil.MustNoErr(t, err, "restamp")
	got, err := eng.FlowFingerprint(processID, toStyleID)
	testutil.MustNoErr(t, err, "eng.FlowFingerprint")
	if got != base {
		t.Error("an attribution-only rewrite of the from claim moved the fingerprint")
	}
	// A content edit on a column the fingerprint still serialises. It used to
	// be UOPCapacity, which left the list when capacity stopped being a claim
	// fact: the catalog moving is not the flow moving, and a Core sync must not
	// invalidate a preview somebody is looking at.
	in.ReorderPoint++
	_, err = db.UpsertStyleNodeClaim(in)
	testutil.MustNoErr(t, err, "edit from")
	fromEdited, err := eng.FlowFingerprint(processID, toStyleID)
	testutil.MustNoErr(t, err, "eng.FlowFingerprint")
	if fromEdited == base {
		t.Error("a from-side content edit left the fingerprint unchanged")
	}
	// THE ROUTING SET MOVES NOTHING (owner, 2026-09-13). It is an offer list:
	// it says which nodes the position panel may put in front of an operator,
	// and nothing refuses a saved claim that names a node outside it. Adopting
	// a lane changes no stored flow and cannot change what a plan would do, so
	// invalidating every open preview on the press for one was a refusal
	// nobody could act on.
	rowID, err := db.UpsertRoutingNode(processes.RoutingNodeInput{ProcessID: processID, CoreNodeName: "BUF-1", Role: domain.RoutingRoleSource, Origin: domain.RoutingOriginEngineer})
	testutil.MustNoErr(t, err, "routing row")
	withRow, err := eng.FlowFingerprint(processID, toStyleID)
	testutil.MustNoErr(t, err, "eng.FlowFingerprint")
	if withRow != fromEdited {
		t.Error("a new routing row moved the fingerprint; the routing set is an offer list, not a flow")
	}
	testutil.MustNoErr(t, db.SetRoutingNodeEnabled(processID, rowID, true, "eng"), "enable")
	got, err = eng.FlowFingerprint(processID, toStyleID)
	testutil.MustNoErr(t, err, "eng.FlowFingerprint")
	if got != withRow {
		t.Error("adopting a routing row moved the fingerprint; the routing set is an offer list, not a flow")
	}
}

// ── PARITY ───────────────────────────────────────────────────────────────────

// previewJSON is a preview with its fingerprint blanked, as bytes.
func previewJSON(t *testing.T, p *FlowPreview) string {
	t.Helper()
	cp := *p
	cp.Fingerprint = ""
	raw, err := json.Marshal(cp)
	testutil.MustNoErr(t, err, "marshal preview")
	return string(raw)
}

// TestFlowPreview_DraftEqualsSavedPreview is the pin that matters: for HK
// P400's style 7 (press-index) and style 11 (two-robot), the preview of the
// DRAFT — the style's own cells, in memory — and the preview after SaveFlow
// of those cells are byte-for-byte the same JSON on actions, order_count,
// findings, unresolved and preflight; only the fingerprint may differ. And
// the desktop's PreviewChangeoverPlan after the save lists the same actions.
//
// It used to run twice per style — once as the cells collapsed, which for these
// rows meant LOCKED (every plant claim carried a capacity), and once with the
// lock forced off so the draft path really expanded each cell. There is one
// path now, and it is the second one.
func TestFlowPreview_DraftEqualsSavedPreview(t *testing.T) {
	t.Parallel()
	for _, styleName := range []string{"PART 40421-RVJ56.37", "PART 68644-WSL97.20"} {
		{
			t.Run(styleName, func(t *testing.T) {
				t.Parallel()
				db := testEngineDB(t)
				plant := testdb.SeedPlant(t, db, scenefixtures.A(), "Press A1")
				styleID, ok := plant.Styles[styleName]
				if !ok {
					t.Fatalf("HK P400 has no style %q; styles: %v", styleName, plant.Styles)
				}
				enableComposer(t, db, plant.ProcessID)
				eng := testEngine(t, db)
				eng.coreClient = NewCoreClient(testCoreURL)

				cells := cellsOf(t, db, styleID)
				if len(cells) != 2 {
					t.Fatalf("style %s has %d cells, the pull had 2", styleName, len(cells))
				}
				draft, err := eng.PreviewFlow(context.Background(), plant.ProcessID, FlowPreviewRequest{ToStyleID: styleID, Cells: cells})
				testutil.MustNoErr(t, err, "draft preview")
				saved, err := eng.SaveFlow(plant.ProcessID, FlowSaveRequest{Source: domain.ClaimSourceHMI, ToStyleID: styleID, Cells: cells, Fingerprint: draft.Fingerprint, CalledBy: "Press 400"})
				testutil.MustNoErr(t, err, "save")
				// THE STORED CELLS, RE-READ. A request carries the whole flow
				// now, so an empty one means "this style has no flow" — which
				// is a real and different preview, not "preview what is
				// stored". Reading them back is also the stronger assertion:
				// it proves the save round-tripped the cells, not just that
				// the planner is deterministic.
				after, err := eng.PreviewFlow(context.Background(), plant.ProcessID,
					FlowPreviewRequest{ToStyleID: styleID, Cells: cellsOf(t, db, styleID)})
				testutil.MustNoErr(t, err, "preview after save")

				if a, b := previewJSON(t, draft), previewJSON(t, after); a != b {
					t.Errorf("draft preview and saved preview differ:\n draft %s\n saved %s", a, b)
				}
				// The DTOs as landed, for the record (and the report).
				if rawPreview, err := json.Marshal(draft); err == nil {
					t.Logf("preview response: %s", rawPreview)
				}
				if rawSave, err := json.Marshal(saved); err == nil {
					t.Logf("save response: %s", rawSave)
				}
				if after.Fingerprint != saved.Fingerprint {
					t.Errorf("the save returned %q but the next preview computed %q", saved.Fingerprint, after.Fingerprint)
				}
				plan, err := eng.PreviewChangeoverPlan(plant.ProcessID, styleID)
				testutil.MustNoErr(t, err, "desktop preview")
				desktop := make([]interface{}, 0, len(plan.Actions))
				for _, a := range plan.Actions {
					desktop = append(desktop, changeover.ToPreviewAction(a))
				}
				got, err := json.Marshal(draft.Actions)
				testutil.MustNoErr(t, err, "json.Marshal")
				want, err := json.Marshal(desktop)
				testutil.MustNoErr(t, err, "json.Marshal")
				if string(got) != string(want) {
					t.Errorf("draft actions and the desktop preview after save differ:\n draft   %s\n desktop %s", got, want)
				}
			})
		}
	}
}

// TestSaveFlow_WritesACellTheEngineerHasConfigured is the retirement of the
// "locked cell", stated as behaviour.
//
// SaveFlow used to refuse to write any cell whose claim carried a value the
// composer does not show, so a node with a reorder point or a keep-staged flag
// was simply not the operator's to re-route. That refusal was standing in front
// of a guarantee Expand provides on its own — the unspoken columns are copied
// from the prior claim, or left alone by a nil pointer — which is pinned
// directly, on the real plant rows, by
// TestFlowCarryThrough_UnlockedSaveLeavesEveryUnspokenColumnAlone.
//
// So the cell is written now, and the engineer's settings survive it. Both
// halves matter: writing without preserving would be the flattening the lock
// was there to prevent, and preserving without writing is the refusal.
func TestSaveFlow_WritesACellTheEngineerHasConfigured(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, toStyleID := seedFlowScenario(t, db)
	eng := testEngine(t, db)
	enableComposer(t, db, processID)

	// FLOW-SAME gets three settings an engineer would have made: two
	// replenishment policies and a changeover special. Not the evac columns —
	// changeover_evac_nodes and the carry-over disposition are refused by the
	// validator on a claim that marks no positions for clearance, and
	// changeover_evac_nodes is carried by the cell anyway, so neither would have
	// been a witness for carry-through.
	same, err := db.GetStyleNodeClaimByNode(toStyleID, "FLOW-SAME")
	testutil.MustNoErr(t, err, "get claim")
	in := domain.InputFromClaim(*same)
	in.ReorderPoint = 42
	in.LinesideSoftThreshold = 7
	_, err = db.UpsertStyleNodeClaim(in)
	testutil.MustNoErr(t, err, "configure the cell")

	// THE THIRD ONE GOES IN AS A LEGACY ROW, because keep_staged is withheld at
	// every write door now (domain.KeepStagedWithheld) and stored rows are left
	// alone — which makes it the sharpest witness this test has. A column the
	// composer cannot write is one it can only preserve or destroy, and there
	// would be no way back from destroying it.
	_, err = db.DB.Exec(`UPDATE style_node_claims SET keep_staged=1 WHERE id=?`, same.ID)
	testutil.MustNoErr(t, err, "store a legacy keep_staged")

	fp, err := eng.FlowFingerprint(processID, toStyleID)
	testutil.MustNoErr(t, err, "fingerprint")

	// The operator re-routes that very cell.
	var cells []domain.FlowCell
	for _, c := range cellsOf(t, db, toStyleID) {
		if c.CoreNodeName == "FLOW-SAME" {
			c.InboundSource = "FLOW-SRC-2"
		}
		cells = append(cells, c)
	}
	res, err := eng.SaveFlow(processID, FlowSaveRequest{Source: domain.ClaimSourceHMI,
		ToStyleID: toStyleID, Cells: cells, Fingerprint: fp, CalledBy: "Press 400",
	})
	testutil.MustNoErr(t, err, "save")
	if res.Written != len(cells) {
		t.Errorf("SaveFlow wrote %d of %d cells — a configured cell is not a cell the composer refuses", res.Written, len(cells))
	}

	after, err := db.GetStyleNodeClaimByNode(toStyleID, "FLOW-SAME")
	testutil.MustNoErr(t, err, "re-read claim")
	if after.InboundSource != "FLOW-SRC-2" {
		t.Errorf("the flow field was not written: inbound_source = %q, want FLOW-SRC-2", after.InboundSource)
	}
	if after.Source != domain.ClaimSourceHMI || after.CalledBy != "Press 400" {
		t.Errorf("the write was not attributed to the operator: source %q, called_by %q", after.Source, after.CalledBy)
	}
	// And the engineer's three, untouched.
	if after.ReorderPoint != 42 {
		t.Errorf("reorder_point = %d, want 42 — a composer save flattened a replenishment policy", after.ReorderPoint)
	}
	if !after.KeepStaged {
		t.Error("a legacy keep_staged was cleared by a composer save — a column the composer " +
			"cannot write is one it must not be able to destroy either")
	}
	if after.LinesideSoftThreshold != 7 {
		t.Errorf("lineside_soft_threshold = %d, want 7", after.LinesideSoftThreshold)
	}
}

// TestSaveFlow_PresetProvenanceIsPointerGated is U10's whole provenance
// design, in one test: a save that names a preset writes the two columns, and a
// save that does not leaves whatever is there ALONE.
//
// WHY THE SECOND HALF IS THE LOAD-BEARING ONE. Drift is computed from truth,
// which only works if the claim's "I came from v2" and the compare's "does it
// still look like v2" are INDEPENDENT answers. A save that overwrote the
// provenance with nothing would make every hand edit erase the lineage, and the
// Presets tab would then show a flow drifting away from a preset it no longer
// admits to having come from — which is to say it would show nothing at all.
//
// Expand writes eighteen columns unconditionally and these two only when they
// are set (domain/flow.go's pointer gate), so "left alone" here is a real
// property of the write path rather than a coincidence of this fixture.
func TestSaveFlow_PresetProvenanceIsPointerGated(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, toStyleID := seedFlowScenario(t, db)
	eng := testEngine(t, db)
	enableComposer(t, db, processID)

	cells := cellsOf(t, db, toStyleID)
	fp, err := eng.FlowFingerprint(processID, toStyleID)
	testutil.MustNoErr(t, err, "fingerprint")

	presetID, version := int64(42), 3
	res, err := eng.SaveFlow(processID, FlowSaveRequest{
		Source: domain.ClaimSourceAdmin, ToStyleID: toStyleID, Cells: cells, Fingerprint: fp, CalledBy: "s.brown",
		SourcePresetID: &presetID, SourcePresetVersion: &version,
	})
	testutil.MustNoErr(t, err, "save with provenance")

	after, err := db.ListStyleNodeClaims(toStyleID)
	testutil.MustNoErr(t, err, "list after the apply")
	if len(after) == 0 {
		t.Fatal("the apply wrote no claim")
	}
	for _, c := range after {
		if c.SourcePresetID == nil || *c.SourcePresetID != presetID {
			t.Errorf("%s · source_preset_id = %v, want %d", c.CoreNodeName, c.SourcePresetID, presetID)
		}
		if c.SourcePresetVersion == nil || *c.SourcePresetVersion != version {
			t.Errorf("%s · source_preset_version = %v, want %d", c.CoreNodeName, c.SourcePresetVersion, version)
		}
	}

	// THE HAND EDIT. Same flow, one field changed, and NOTHING said about a
	// preset — which is every save D1 has ever made.
	edited := cellsOf(t, db, toStyleID)
	for i := range edited {
		edited[i].OutboundDestination = "FLOW-DST"
	}
	_, err = eng.SaveFlow(processID, FlowSaveRequest{
		Source: domain.ClaimSourceAdmin, ToStyleID: toStyleID, Cells: edited,
		Fingerprint: res.Fingerprint, CalledBy: "s.brown",
	})
	testutil.MustNoErr(t, err, "save without provenance")

	after, err = db.ListStyleNodeClaims(toStyleID)
	testutil.MustNoErr(t, err, "list after the hand edit")
	for _, c := range after {
		if c.SourcePresetID == nil || *c.SourcePresetID != presetID {
			t.Errorf("%s · a save that named no preset cleared the provenance: source_preset_id = %v",
				c.CoreNodeName, c.SourcePresetID)
		}
		if c.SourcePresetVersion == nil || *c.SourcePresetVersion != version {
			t.Errorf("%s · a save that named no preset cleared the version: %v",
				c.CoreNodeName, c.SourcePresetVersion)
		}
		// And the edit itself landed — otherwise this test would pass on a
		// save that did nothing at all.
		if c.OutboundDestination != "FLOW-DST" {
			t.Errorf("%s · outbound_destination = %q, want the hand edit", c.CoreNodeName, c.OutboundDestination)
		}
	}
}

// TestSaveFlow_ProvenanceIsNotInTheFingerprint pins why a preview and the save
// that follows it agree.
//
// A preview carries no provenance (it writes nothing, so there is nothing to
// record), and the save after it carries the preset's. If the two columns were
// in the fingerprint's column list, the save would then compute a different
// fingerprint from the one it was handed and every apply would answer 409
// stale — an apply modal that could never save anything.
func TestSaveFlow_ProvenanceIsNotInTheFingerprint(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, toStyleID := seedFlowScenario(t, db)
	eng := testEngine(t, db)
	enableComposer(t, db, processID)

	cells := cellsOf(t, db, toStyleID)
	fp, err := eng.FlowFingerprint(processID, toStyleID)
	testutil.MustNoErr(t, err, "fingerprint")

	presetID, version := int64(7), 1
	res, err := eng.SaveFlow(processID, FlowSaveRequest{
		Source: domain.ClaimSourceAdmin, ToStyleID: toStyleID, Cells: cells, Fingerprint: fp, CalledBy: "s.brown",
		SourcePresetID: &presetID, SourcePresetVersion: &version,
	})
	testutil.MustNoErr(t, err, "the apply must land on the preview's own fingerprint")

	// And the fingerprint AFTER the apply does not move when the same flow is
	// re-applied from a different version: the shape is what it hashes.
	v2 := 2
	if _, err := eng.SaveFlow(processID, FlowSaveRequest{
		Source: domain.ClaimSourceAdmin, ToStyleID: toStyleID, Cells: cells, Fingerprint: res.Fingerprint, CalledBy: "s.brown",
		SourcePresetID: &presetID, SourcePresetVersion: &v2,
	}); err != nil {
		t.Errorf("re-applying the same shape from another version: %v", err)
	}
}

// TestPreviewFlow_EveryOrderNamesBothEnds is P1's server half.
//
// Both S9s read `Robot N · from → to · what`, and a sentence needs two ends. The
// wire used to carry a complex order's steps as a COUNT alone
// (`step_count: 3`), so the UI had the order's shape and neither end of it —
// which is why S9 could say "brings the new bin to PLN_02" and could not say
// from where.
//
// A leg with one end is not a sentence and the render layer drops it, so a
// regression here would not break a screen: it would quietly shorten the ORDERS
// column while the bar went on counting the orders that were planned. That is
// exactly the failure a test has to catch instead of a shot.
func TestPreviewFlow_EveryOrderNamesBothEnds(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, toStyleID := seedFlowScenario(t, db)
	eng := testEngine(t, db)
	enableComposer(t, db, processID)

	pv, err := eng.PreviewFlow(context.Background(), processID, FlowPreviewRequest{
		ToStyleID: toStyleID, Cells: cellsOf(t, db, toStyleID),
	})
	testutil.MustNoErr(t, err, "preview")
	if len(pv.Actions) == 0 {
		t.Fatal("the preview planned no action; this test would prove nothing")
	}
	orders := 0
	for _, a := range pv.Actions {
		for label, spec := range map[string]*changeover.PreviewSpec{
			"supply": a.SupplyOrder, "evac": a.EvacOrder,
		} {
			if spec == nil {
				continue
			}
			orders++
			if spec.From == "" {
				t.Errorf("%s · %s order names no From — the sentence cannot say where the bin comes from",
					a.CoreNodeName, label)
			}
			if spec.To == "" {
				t.Errorf("%s · %s order names no To — the sentence cannot say where the bin goes",
					a.CoreNodeName, label)
			}
			if spec.From == spec.To {
				t.Errorf("%s · %s order goes from %q to itself", a.CoreNodeName, label, spec.From)
			}
		}
	}
	if orders == 0 {
		t.Fatal("no action carried an order at all")
	}
	t.Logf("%d previewed orders, every one naming both ends", orders)
}
