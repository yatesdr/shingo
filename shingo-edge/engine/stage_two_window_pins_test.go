package engine

import (
	"context"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/orders"
	"shingoedge/service"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// stage_two_window_pins_test.go — every Edge site the role census names
// (work/partials-stage2-2026-10-29/CENSUS-role-2026-10-29.md, G1-G17) at a
// window of a two-stage unloader's stage 2: what each does there, so a change
// to the stage's role is a change these name. The pair comes from one place,
// testdb.StageTwoPair; the sim's site (G17) is pinned in sim_operator_test.go
// on the same rig.

const (
	s2Group  = "S2G"
	s2Stage1 = "S2-ST1"
	s2W1     = "S2-W1"
	s2W2     = "S2-W2"
	s2Out    = "S2-OUT"
	s2Wait   = "S2-WAIT"
	s2Part   = "PART-K"
)

// stageTwoMode is the pair in one of the census's three modes: (a) pulled
// directly, no outbound (SPR); (b) direct, an outbound; (c) pull mode, an
// inbound wait group and a part, with an outbound.
func stageTwoMode(mode byte) testdb.StageTwoPair {
	p := testdb.StageTwoPair{Group: s2Group, Stage1Window: s2Stage1, Stage1Payloads: []string{s2Part},
		Windows: []string{s2W1, s2W2}}
	switch mode {
	case 'a':
		p.PulledDirectly = true
	case 'b':
		p.Outbound = s2Out
	case 'c':
		p.Outbound, p.Inbound, p.Payloads = s2Out, s2Wait, []string{s2Part}
	}
	return p
}

// stageTwoRig is an Edge holding the pair as Core sends it, its windows on one
// station, wired to the live completion chain, a Core that answers loader
// asks, and a bins table the test sets per node.
type stageTwoRig struct {
	eng   *Engine
	db    *store.DB
	core  *scCore
	asks  *coreRetrieve
	sid   int64
	nodes map[string]int64
}

var s2Catalog = []protocol.PayloadBinTypeInfo{
	{PayloadCode: s2Part, BinTypeCode: "TOTE-1"},
	{PayloadCode: "PART-Z", BinTypeCode: "TOTE-2"},
}

func newStageTwoRig(t *testing.T, pair testdb.StageTwoPair) *stageTwoRig {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	r := &stageTwoRig{eng: eng, db: db, asks: answerLoaderAsks(t, eng), nodes: map[string]int64{}}
	eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
	eng.wireEventHandlers()
	r.core = newSCCore(t)
	bridgeCore(t, eng, r.core.srv.URL)
	r.core.bind(t, eng)
	eng.stationService.SetLoaderResolver(stationLoaderResolver{eng})
	eng.stationService.SetCoreNodeGroupResolver(eng.CoreNodeGroups)
	eng.stationService.SetCarrierCatalog(func() []protocol.PayloadBinTypeInfo { return s2Catalog })
	eng.SetCoreNodes(pair.CoreNodes(map[string][]string{s2Out: {"S2-OUT-M1", "S2-OUT-M2"}}))

	procID, err := db.CreateProcess("S2-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	r.sid, err = db.CreateOperatorStation(stations.Input{ProcessID: procID, Code: "S2", Name: "S2", Sequence: 1, Enabled: true})
	testutil.MustNoErr(t, err, "create station")
	for i, n := range append([]string{pair.Stage1Window}, pair.Windows...) {
		id, err := db.CreateProcessNode(processes.NodeInput{ProcessID: procID, OperatorStationID: &r.sid,
			CoreNodeName: n, Code: n, Name: n, Sequence: i + 1, Enabled: true})
		testutil.MustNoErr(t, err, "create node "+n)
		_, err = db.EnsureProcessNodeRuntime(id)
		testutil.MustNoErr(t, err, "runtime "+n)
		r.nodes[n] = id
	}
	eng.SetCoreLoaders(pair.Infos())
	return r
}

func (r *stageTwoRig) node(t *testing.T, core string) *processes.Node {
	t.Helper()
	n, err := r.db.GetProcessNode(r.nodes[core])
	testutil.MustNoErr(t, err, "read node "+core)
	return n
}

// cellMaking seeds a cell process running a style with a produce claim of
// part whose inbound source is from, and a consume claim of the same part.
func (r *stageTwoRig) cells(t *testing.T, part, from string) {
	t.Helper()
	procID, err := r.db.CreateProcess("S2-CELL", "", "", "", false)
	testutil.MustNoErr(t, err, "create cell process")
	styleID, err := r.db.CreateStyle("S2-CELL-RUN", "", procID)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, r.db.SetActiveStyle(procID, &styleID), "set active style")
	for _, c := range []struct {
		node string
		role protocol.ClaimRole
		in   string
	}{{"S2-PLN", protocol.ClaimRoleProduce, from}, {"S2-CLN", protocol.ClaimRoleConsume, ""}} {
		id, err := r.db.CreateProcessNode(processes.NodeInput{ProcessID: procID, CoreNodeName: c.node, Code: c.node,
			Name: c.node, Sequence: 1, Enabled: true})
		testutil.MustNoErr(t, err, "create cell node")
		_, err = r.db.EnsureProcessNodeRuntime(id)
		testutil.MustNoErr(t, err, "cell runtime")
		_, err = r.db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
			StyleID: styleID, CoreNodeName: c.node, Role: c.role, SwapMode: protocol.SwapModeSequential,
			InboundSource: c.in, OutboundDestination: "MARKET", PayloadCode: part,
			AllowedPayloadCodes: []string{part}, UOPCapacity: 132,
		})
		testutil.MustNoErr(t, err, "cell claim "+string(c.role))
	}
}

// G1: SPR's stage 2 (pulled directly, no payloads) projects, and every window
// reads as the loader's, through the synthesised claim. A plain produce
// loader with no payloads is refused at projection and resolves nowhere.
func TestStageTwoPin_G1_TheWindowsResolveWithNoPayloads(t *testing.T) {
	t.Parallel()
	r := newStageTwoRig(t, stageTwoMode('a'))
	for _, w := range []string{s2W1, s2W2} {
		l, err := r.eng.loaders().LoaderForNode(domain.NodeID(w))
		if err != nil || l == nil {
			t.Fatalf("LoaderForNode(%s) = %v, %v; want stage 2", w, l, err)
		}
		if _, _, claim, err := r.eng.loadActiveNode(r.nodes[w]); err != nil || claim == nil || claim.ID != 0 {
			t.Errorf("claim at %s = %+v (err %v), want the synthesised one", w, claim, err)
		}
	}
	plain := sharedLoaderInfo("S2-PLAIN", string(protocol.ClaimRoleProduce), "operator", "", 0, 0)
	plain.Payloads = nil
	r.eng.SetCoreLoaders(append(stageTwoMode('a').Infos(), plain))
	if l, err := r.eng.loaders().LoaderForNode("S2-PLAIN"); err == nil || l != nil {
		t.Errorf("a produce loader with no payloads resolved to %v (err %v); want it refused", l, err)
	}
	if l, err := r.eng.loaders().LoaderForNode(s2W1); err != nil || l == nil {
		t.Errorf("stage 2 stopped resolving beside the refused loader (err %v)", err)
	}
}

// G2: the CLEAR's send-out. Pulled directly, the cart stays for the line and
// the CLEAR says nothing; direct, the cart leaves on one U2 to stage 2's
// outbound. Either way the CLEAR confirms a full-in delivered at the window.
func TestStageTwoPin_G2_TheClearSendsTheCartOutUnlessPulledDirectly(t *testing.T) {
	t.Parallel()
	a := newStageTwoRig(t, stageTwoMode('a'))
	a.core.set(s2W1, true, "")
	u1 := scDeliveredRetrieve(t, a.db, "s2-u1", orders.TypeRetrieve, a.nodes[s2W1], false, s2W1)
	if err := a.eng.ClearBin(a.nodes[s2W1], "", false, nil); err != nil {
		t.Errorf("ClearBin pulled directly = %v, want nil", err)
	}
	if moves := scMovesFrom(t, a.db, s2W1); len(moves) != 0 {
		t.Errorf("U2s pulled directly = %d, want 0", len(moves))
	}
	if o, err := a.db.GetOrder(u1); err != nil || o.Status != orders.StatusConfirmed {
		t.Errorf("the delivered full-in after the CLEAR = %+v (err %v), want confirmed", o, err)
	}

	b := newStageTwoRig(t, stageTwoMode('b'))
	b.core.set(s2W1, true, "")
	testutil.MustNoErr(t, b.eng.ClearBin(b.nodes[s2W1], "", false, nil), "ClearBin direct")
	if moves := scMovesFrom(t, b.db, s2W1); len(moves) != 1 || moves[0].DeliveryNode != s2Out {
		t.Errorf("U2s direct = %+v, want one to %s", moves, s2Out)
	}
}

// G2': the CLEAR at a pull-mode stage 2 asks Core for nothing.
func TestStageTwoPin_G2Prime_TheClearAsksNoEmpty(t *testing.T) {
	t.Parallel()
	r := newStageTwoRig(t, stageTwoMode('c'))
	r.core.set(s2W1, true, "")
	before := r.asks.asks.Load()
	testutil.MustNoErr(t, r.eng.ClearBin(r.nodes[s2W1], "", false, nil), "ClearBin")
	awaitAsks(t, r.eng)
	if n := r.asks.asks.Load() - before; n != 0 {
		t.Errorf("asks on the CLEAR = %d (last %+v), want 0", n, r.asks.lastAsk())
	}
}

// G3: PUSH EMPTY. Direct, one U2 to the outbound; pulled directly, refused:
// there is nowhere to send it.
func TestStageTwoPin_G3_PushEmpty(t *testing.T) {
	t.Parallel()
	b := newStageTwoRig(t, stageTwoMode('b'))
	b.core.set(s2W1, true, "")
	testutil.MustNoErr(t, b.eng.PushEmptyOut(b.nodes[s2W1]), "PushEmptyOut direct")
	if moves := scMovesFrom(t, b.db, s2W1); len(moves) != 1 || moves[0].DeliveryNode != s2Out {
		t.Errorf("U2s direct = %+v, want one to %s", moves, s2Out)
	}

	a := newStageTwoRig(t, stageTwoMode('a'))
	a.core.set(s2W1, true, "")
	err := a.eng.PushEmptyOut(a.nodes[s2W1])
	if err == nil || !strings.Contains(err.Error(), "nowhere to send") {
		t.Errorf("PushEmptyOut pulled directly = %v, want the nowhere-to-send refusal", err)
	}
	if moves := scMovesFrom(t, a.db, s2W1); len(moves) != 0 {
		t.Errorf("U2s pulled directly = %d, want 0", len(moves))
	}
}

// G4 and its inverse: a request at a stage-2 window asks Core for nothing,
// an empty or a full of a part stage 2 lists (pull mode).
//
// CHANGED (a flat bug): the full used to be asked. A consume stage 2 listing
// a part ordered a full into a window stage 1 feeds; at 5b983220 this pinned
// one ask and Core's order. Refused now, by the produce role and, for a stage
// 2 still sent as consume, by TakesBare.
func TestStageTwoPin_G4_RequestsAtAStageTwoWindow(t *testing.T) {
	t.Parallel()
	pair := stageTwoMode('c')
	r := newStageTwoRig(t, pair)
	before := r.asks.asks.Load()
	if _, err := r.eng.RequestEmptyBin(r.nodes[s2W1], ""); err == nil {
		t.Error("RequestEmptyBin at a stage-2 window succeeded; want it refused")
	}
	if o, err := r.eng.RequestFullBin(r.nodes[s2W1], s2Part); err == nil {
		t.Errorf("RequestFullBin at a stage-2 window = %+v; want it refused", o)
	}
	stillConsume := pair.Stage2()
	stillConsume.Role = string(protocol.ClaimRoleConsume)
	r.eng.SetCoreLoaders([]protocol.LoaderInfo{stillConsume, pair.Stage1()})
	if o, err := r.eng.RequestFullBin(r.nodes[s2W1], s2Part); err == nil || !strings.Contains(err.Error(), "stage 2") {
		t.Errorf("RequestFullBin at a consume stage 2 = %+v, %v; want the stage-2 refusal", o, err)
	}
	if n := r.asks.asks.Load() - before; n != 0 {
		t.Errorf("asks on the requests = %d (last %+v), want 0", n, r.asks.lastAsk())
	}
}

// G5: no PULL FROM MARKET at a stage-2 window.
func TestStageTwoPin_G5_NoPullFromMarket(t *testing.T) {
	t.Parallel()
	r := newStageTwoRig(t, stageTwoMode('b'))
	if _, err := r.eng.FetchMarketBins(r.nodes[s2W1]); err == nil {
		t.Error("FetchMarketBins at a stage-2 window succeeded; want it refused")
	}
	if err := r.eng.PullFromMarket(r.nodes[s2W1], "S2-OUT-M1"); err == nil {
		t.Error("PullFromMarket at a stage-2 window succeeded; want it refused")
	}
	if moves := scMovesFrom(t, r.db, "S2-OUT-M1"); len(moves) != 0 {
		t.Errorf("moves out of the market = %d, want 0", len(moves))
	}
}

// G7: the window-free push and the startup sweep ask nothing for stage 2.
func TestStageTwoPin_G7_NoPushAsksAnEmpty(t *testing.T) {
	t.Parallel()
	r := newStageTwoRig(t, stageTwoMode('c'))
	before := r.asks.asks.Load()
	r.eng.rePushOwnLoader(r.node(t, s2W1))
	r.eng.SweepPushLoaders()
	awaitAsks(t, r.eng)
	if n := r.asks.asks.Load() - before; n != 0 {
		t.Errorf("asks from the push and the sweep = %d (last %+v), want 0", n, r.asks.lastAsk())
	}
}

// G8: the U2's landing asks nothing for stage 2.
func TestStageTwoPin_G8_TheLandingAsksNoEmpty(t *testing.T) {
	t.Parallel()
	r := newStageTwoRig(t, stageTwoMode('c'))
	r.core.set(s2W1, true, "")
	testutil.MustNoErr(t, r.eng.ClearBin(r.nodes[s2W1], "", false, nil), "ClearBin")
	moves := scMovesFrom(t, r.db, s2W1)
	if len(moves) != 1 {
		t.Fatalf("U2s = %d, want 1", len(moves))
	}
	r.core.set(s2W1, false, "")
	before := r.asks.asks.Load()
	scLand(t, r.eng, r.db, moves[0])
	awaitAsks(t, r.eng)
	if n := r.asks.asks.Load() - before; n != 0 {
		t.Errorf("asks on the landing = %d (last %+v), want 0", n, r.asks.lastAsk())
	}
}

// G12: with both halves serving a part, the unloader found for it is stage 1.
//
// CHANGED (better): it was stage 2, which sorts first and shadowed stage 1 for
// a release's full-in (pinned so at 5b983220). A produce stage 2 is not a
// consume loader, and the full-in paths stay on role consume.
func TestStageTwoPin_G12_TheUnloaderForAPart(t *testing.T) {
	t.Parallel()
	pair := stageTwoMode('c')
	r := newStageTwoRig(t, pair)
	l, err := r.eng.loaders().LoaderForPayload(s2Part, domain.RoleConsume)
	if err != nil || l == nil || string(l.ID()) != pair.Stage1Key() {
		t.Errorf("LoaderForPayload(%s, consume) = %v (err %v), want stage 1 %s", s2Part, l, err, pair.Stage1Key())
	}
}

// G14, G15, G16: the board at a station of stage 1 and a direct stage 2. Stage
// 2's tiles carry the CLEAR panels' carriers (every catalog type: it lists no
// part); its outbound group's members are on the board's list and are its
// Empties; the served rows are the cells making the part, not using it.
func TestStageTwoPin_G14G15G16_TheBoard(t *testing.T) {
	t.Parallel()
	r := newStageTwoRig(t, stageTwoMode('b'))
	r.cells(t, s2Part, s2Group)
	view, err := r.eng.stationService.BuildView(context.Background(), r.sid)
	testutil.MustNoErr(t, err, "BuildView")
	for _, tile := range view.Nodes {
		if tile.Node.CoreNodeName == s2Stage1 {
			continue
		}
		if tile.Loader == nil || strings.Join(tile.Loader.Carriers, ",") != "TOTE-1,TOTE-2" {
			t.Errorf("G14: %s's loader = %+v, want carriers TOTE-1,TOTE-2", tile.Node.CoreNodeName, tile.Loader)
		}
		// PARTIAL ON's parts: the cell making the part takes its empties from
		// the pair group, so its part is offered (ruling 4, 2026-10-29).
		if tile.Loader != nil && strings.Join(tile.Loader.PartialParts, ",") != s2Part {
			t.Errorf("%s's partial parts = %v, want [%s]", tile.Node.CoreNodeName, tile.Loader.PartialParts, s2Part)
		}
	}
	list := strings.Join(r.eng.stationService.BinNodes(r.sid), ",")
	if !strings.Contains(list, "S2-OUT-M1") || !strings.Contains(list, "S2-OUT-M2") {
		t.Errorf("G15: the board's list = %s, want stage 2's outbound members", list)
	}
	rows := map[string]protocol.NodeBinsRow{
		"S2-OUT-M1": {Node: "S2-OUT-M1", State: protocol.BinsStateOccupied, Occupied: true, BinTypeCode: "TOTE-1"},
		"S2-OUT-M2": {Node: "S2-OUT-M2", State: protocol.BinsStateEmpty},
	}
	r.eng.stationService.StampBins(view, service.BoardBinsFromCopy(rows), true, nil)
	if view.Empties == nil || view.Empties.Group != s2Out {
		t.Errorf("G15: empties = %+v, want stage 2's outbound %s", view.Empties, s2Out)
	}
	var roles []string
	for _, row := range view.Serves {
		roles = append(roles, row.Cell+"/"+row.Role)
	}
	if strings.Join(roles, ",") != "S2-CELL/produce" {
		t.Errorf("G16: served rows = %v, want the cell making the part only", roles)
	}
}

// The LOAD at a window posts the part and the declared count, as the person
// typed it.
func TestStageTwoPin_TheLoadPostsThePartAndTheCount(t *testing.T) {
	t.Parallel()
	const window = "S2-LOAD-W1"
	eng, _, core, _, nodeID := scLoaderEngine(t, window)
	core.set(window, true, "")
	typed := int64(37)
	testutil.MustNoErr(t, eng.LoadBin(nodeID, "PART-SC", &typed, scManifest), "LoadBin")
	core.mu.Lock()
	defer core.mu.Unlock()
	if len(core.loads) != 1 || core.loads[0].PayloadCode != "PART-SC" || core.loads[0].UOPCount == nil || *core.loads[0].UOPCount != 37 {
		t.Errorf("bin-load requests = %+v, want one of PART-SC at 37", core.loads)
	}
}

// TestStageTwo_PartialOnLoadsAPartTheLinesSourceFromHere is ruling 4's PARTIAL ON
// (2026-10-29): a LOAD on a bare cart at a stage-2 window takes a part a line
// sources from the pair group, which the stage's own payload set (none) does
// not list, and posts the typed count. A part no line sources from here is
// refused. The cart's send-out is the CLEAR's: none where the stage is pulled
// directly, else one move off the window to its outbound, naming no part and
// the cart Core loaded.
func TestStageTwo_PartialOnLoadsAPartTheLinesSourceFromHere(t *testing.T) {
	t.Parallel()
	typed := int64(37)
	manifest := []protocol.IngestManifestItem{{PartNumber: "PN-K", Quantity: 37}}

	a := newStageTwoRig(t, stageTwoMode('a'))
	a.cells(t, s2Part, s2Group)
	a.core.set(s2W1, true, "")
	if err := a.eng.LoadBin(a.nodes[s2W1], "PART-Z", &typed, manifest); err == nil {
		t.Error("PARTIAL ON of a part no line sources from here succeeded; want it refused")
	}
	testutil.MustNoErr(t, a.eng.LoadBin(a.nodes[s2W1], s2Part, &typed, manifest), "PARTIAL ON pulled directly")
	a.core.mu.Lock()
	loads := append([]BinLoadRequest(nil), a.core.loads...)
	a.core.mu.Unlock()
	if len(loads) != 1 || loads[0].PayloadCode != s2Part || loads[0].UOPCount == nil || *loads[0].UOPCount != 37 {
		t.Errorf("bin-load requests = %+v, want one of %s at 37", loads, s2Part)
	}
	if moves := scMovesFrom(t, a.db, s2W1); len(moves) != 0 {
		t.Errorf("moves off a pulled-directly window after PARTIAL ON = %+v, want none", moves)
	}

	b := newStageTwoRig(t, stageTwoMode('b'))
	b.cells(t, s2Part, s2W2)
	b.core.set(s2W1, true, "")
	testutil.MustNoErr(t, b.eng.LoadBin(b.nodes[s2W1], s2Part, &typed, manifest), "PARTIAL ON direct")
	moves := scMovesFrom(t, b.db, s2W1)
	if len(moves) != 1 || moves[0].DeliveryNode != s2Out || moves[0].PayloadCode != "" ||
		moves[0].BinID == nil || *moves[0].BinID != 77 {
		t.Errorf("moves off a direct window after PARTIAL ON = %+v, want one to %s naming bin 77 and no part", moves, s2Out)
	}
}
