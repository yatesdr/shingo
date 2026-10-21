package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/service"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// flow_mint_cells_test.go — the mint beyond the press index: the demo plant's
// WELD-2 (single_robot, three slots), PRESS-2 (sequential, one slot of two
// nodes) and WELD-1 (two_robot, consume + produce). Each cell is the
// generator test's own table (domain/flow_preset_generate_test.go
// TestGenerateShapes_SixCells), seeded with a running style, and its recipe is
// the shape the generator offers for it, named the way the Presets tab names
// one.

type mintCellClaim struct {
	node, part, paired, inStg, outStg string
	role                              protocol.ClaimRole
}

type mintCell struct {
	name      string
	mode      protocol.SwapMode
	positions []string
	routing   []domain.RoutingNodeInput
	claims    []mintCellClaim
}

func mintRouting(name, role string, seq int) domain.RoutingNodeInput {
	return domain.RoutingNodeInput{CoreNodeName: name, Role: role, Sequence: seq, Enabled: true}
}

var marketRouting = []domain.RoutingNodeInput{
	mintRouting("SYN_MARKET", domain.RoutingRoleSource, 0), mintRouting("SYN_MARKET", domain.RoutingRoleDestination, 0),
}

var (
	weld2Cell = mintCell{
		name: "WELD-2", mode: protocol.SwapModeSingleRobot,
		positions: []string{"ALN_003", "ALN_004", "ALN_005"},
		routing: append(append([]domain.RoutingNodeInput(nil), marketRouting...),
			mintRouting("SLN_003", domain.RoutingRoleStaging, 0), mintRouting("SLN_004", domain.RoutingRoleStaging, 1),
			mintRouting("SLN_005", domain.RoutingRoleStaging, 2), mintRouting("SLN_006", domain.RoutingRoleStaging, 3),
			mintRouting("SLN_007", domain.RoutingRoleStaging, 4), mintRouting("SLN_008", domain.RoutingRoleStaging, 5)),
		claims: []mintCellClaim{
			{node: "ALN_003", part: "PANEL-B", role: protocol.ClaimRoleConsume, inStg: "SLN_003", outStg: "SLN_004"},
			{node: "ALN_004", part: "BRKT", role: protocol.ClaimRoleConsume, inStg: "SLN_005", outStg: "SLN_006"},
			{node: "ALN_005", part: "ASSY", role: protocol.ClaimRoleProduce, inStg: "SLN_007", outStg: "SLN_008"},
		},
	}
	press2Cell = mintCell{
		name: "PRESS-2", mode: protocol.SwapModeSequential,
		positions: []string{"PLN_003", "PLN_004"},
		routing: []domain.RoutingNodeInput{mintRouting("SYN_PRESS_EMPTIES", domain.RoutingRoleSource, 0),
			mintRouting("SYN_MARKET", domain.RoutingRoleDestination, 0)},
		claims: []mintCellClaim{
			{node: "PLN_003", part: "PANEL-B", role: protocol.ClaimRoleProduce, paired: "PLN_004"},
			{node: "PLN_004", part: "PANEL-B", role: protocol.ClaimRoleProduce, paired: "PLN_003"},
		},
	}
	weld1Cell = mintCell{
		name: "WELD-1", mode: protocol.SwapModeTwoRobot,
		positions: []string{"ALN_001", "ALN_002"},
		routing: append(append([]domain.RoutingNodeInput(nil), marketRouting...),
			mintRouting("SLN_001", domain.RoutingRoleStaging, 0), mintRouting("SLN_002", domain.RoutingRoleStaging, 1)),
		claims: []mintCellClaim{
			{node: "ALN_001", part: "PANEL-A", role: protocol.ClaimRoleConsume, inStg: "SLN_001"},
			{node: "ALN_002", part: "ASSY", role: protocol.ClaimRoleProduce, inStg: "SLN_002"},
		},
	}
)

type mintCellFixture struct {
	db      *store.DB
	eng     *Engine
	process int64
	running int64
	preset  int64
}

// seedMintCell builds the cell, its running style, and names the generated
// recipe for its mode "Standard".
func seedMintCell(t *testing.T, cell mintCell) mintCellFixture {
	t.Helper()
	db := testEngineDB(t)
	pid, err := db.CreateProcess(cell.name, "mint cell", "", "", false)
	testutil.MustNoErr(t, err, "process")
	for i, n := range cell.positions {
		_, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: n, Code: n, Name: n, Sequence: i + 1, Enabled: true})
		testutil.MustNoErr(t, err, "position "+n)
	}
	for _, r := range cell.routing {
		r.ProcessID = pid
		_, err := db.UpsertRoutingNode(domain.CoreNodeKinds{}, r)
		testutil.MustNoErr(t, err, "routing "+r.CoreNodeName)
	}
	run, err := db.CreateStyle(cell.name+"-RUN", "", pid)
	testutil.MustNoErr(t, err, "running style")
	src, dst := "SYN_MARKET", "SYN_MARKET"
	if cell.mode == protocol.SwapModeSequential {
		src = "SYN_PRESS_EMPTIES"
	}
	for _, c := range cell.claims {
		_, err := processes.UpsertClaim(db.DB, domain.CoreNodeKinds{}, domain.NodeClaimInput{
			StyleID: run, CoreNodeName: c.node, Role: c.role, SwapMode: cell.mode, PayloadCode: c.part,
			PairedCoreNode: c.paired, InboundStaging: c.inStg, OutboundStaging: c.outStg,
			InboundSource: src, OutboundDestination: dst,
		})
		testutil.MustNoErr(t, err, "running claim "+c.node)
	}
	testutil.MustNoErr(t, db.SetActiveStyle(pid, &run), "set running style")

	svc := service.NewProcessService(db)
	view, err := svc.PresetsFor(pid)
	testutil.MustNoErr(t, err, "PresetsFor")
	for _, g := range view.Generated {
		if g.Mode != string(cell.mode) {
			continue
		}
		id, err := svc.CreateFromCells(pid, "Standard", "test", g.Shape)
		testutil.MustNoErr(t, err, "name the generated recipe")
		eng := testEngine(t, db)
		coreNeverAnswers(t, eng)
		enableComposer(t, db, pid)
		return mintCellFixture{db: db, eng: eng, process: pid, running: run, preset: id}
	}
	t.Fatalf("%s: the generator offers no %s recipe (got %d)", cell.name, cell.mode, len(view.Generated))
	return mintCellFixture{}
}

func (f mintCellFixture) mint(t *testing.T, preset int64, codes ...string) *MintResult {
	t.Helper()
	var picks []PartPick
	for _, c := range codes {
		picks = append(picks, PartPick{Code: c})
	}
	res, err := f.eng.MintStyle(f.process, MintRequest{PresetID: preset, Parts: picks, Source: domain.ClaimSourceHMI, CalledBy: "station"})
	testutil.MustNoErr(t, err, "mint")
	return res
}

func (f mintCellFixture) claimsByNode(t *testing.T, styleID int64) map[string]processes.NodeClaim {
	t.Helper()
	claims, err := f.db.ListStyleNodeClaims(styleID)
	testutil.MustNoErr(t, err, "claims")
	out := map[string]processes.NodeClaim{}
	for _, c := range claims {
		out[c.CoreNodeName] = c
	}
	return out
}

// checkMinted pins the stamps every minted claim carries: no auto-reorder, a
// zero point, the 'unset' source, the recipe as provenance; the process's
// containment route on produce claims only; curtain where the node is.
func (f mintCellFixture) checkMinted(t *testing.T, res *MintResult, wantName string, wantParts map[string]string, curtained string) {
	t.Helper()
	if !res.Minted {
		t.Fatalf("result = %+v, want a minted style", res)
	}
	style, err := f.db.GetStyle(res.StyleID)
	testutil.MustNoErr(t, err, "minted style")
	if style.Name != wantName {
		t.Errorf("name = %q, want %q (R-D)", style.Name, wantName)
	}
	got := f.claimsByNode(t, res.StyleID)
	if len(got) != len(wantParts) {
		t.Fatalf("claims on %d nodes, want %d: %v", len(got), len(wantParts), got)
	}
	for node, part := range wantParts {
		c, ok := got[node]
		if !ok || c.PayloadCode != part {
			t.Errorf("%s carries %q, want %q", node, c.PayloadCode, part)
			continue
		}
		if c.AutoReorder || c.ReorderPoint != 0 || c.ReorderPointSource != MintedReorderSource {
			t.Errorf("%s: auto_reorder=%v point=%d source=%q, want false/0/%q", node, c.AutoReorder, c.ReorderPoint, c.ReorderPointSource, MintedReorderSource)
		}
		if c.SourcePresetID == nil || *c.SourcePresetID != f.preset {
			t.Errorf("%s: provenance %v, want recipe %d", node, c.SourcePresetID, f.preset)
		}
		wantRoute := ""
		if c.Role == protocol.ClaimRoleProduce {
			wantRoute = "QA-HOLD"
		}
		if c.ContainmentDestination != wantRoute {
			t.Errorf("%s (%s): containment %q, want %q", node, c.Role, c.ContainmentDestination, wantRoute)
		}
		if c.CurtainRequired != (node == curtained) {
			t.Errorf("%s: curtain_required=%v, want it only on %s", node, c.CurtainRequired, curtained)
		}
	}
}

func (f mintCellFixture) holdAndCurtain(t *testing.T, node string) {
	t.Helper()
	testutil.MustNoErr(t, service.NewProcessService(f.db).SetContainment(domain.CoreNodeKinds{}, f.process, true, "QA-HOLD", "qa"),
		"turn containment on")
	if _, err := f.db.Exec(`UPDATE style_node_claims SET curtain_required = 1 WHERE style_id = ? AND core_node_name = ?`, f.running, node); err != nil {
		t.Fatalf("curtain %s: %v", node, err)
	}
}

func TestMintStyle_WeldTwoSingleRobotThreeSlots(t *testing.T) {
	t.Parallel()
	f := seedMintCell(t, weld2Cell)

	// Picked as a whole style (the station's style chip sends its parts and no
	// recipe): resolves to the running style, writes nothing.
	res := f.mint(t, 0, "PANEL-B", "BRKT", "ASSY")
	if res.Minted || res.StyleID != f.running {
		t.Fatalf("result = %+v, want the running WELD-2 style resolved", res)
	}

	// A new pair of parts beside BRKT: BRKT goes home to ALN_004 on evidence,
	// the others take the free slots in picture order, and the style is named
	// for what the cell makes (three parts → the produced part).
	f.holdAndCurtain(t, "ALN_004")
	res = f.mint(t, f.preset, "PANEL-N", "BRKT", "ASSY-N")
	if res.Placement["ALN_004"] != "BRKT" {
		t.Errorf("placement = %v, want BRKT home on ALN_004", res.Placement)
	}
	f.checkMinted(t, res, "ASSY-N", map[string]string{"ALN_003": "PANEL-N", "ALN_004": "BRKT", "ALN_005": "ASSY-N"}, "ALN_004")
}

func TestMintStyle_PressTwoSequentialOneSlotOfTwoNodes(t *testing.T) {
	t.Parallel()
	f := seedMintCell(t, press2Cell)

	res := f.mint(t, 0, "PANEL-B")
	if res.Minted || res.StyleID != f.running {
		t.Fatalf("result = %+v, want the running PRESS-2 style resolved", res)
	}

	// One part fills the A/B slot: the same part on both rows.
	f.holdAndCurtain(t, "PLN_003")
	res = f.mint(t, f.preset, "PANEL-Z")
	if res.Placement["PLN_003"] != "PANEL-Z" {
		t.Errorf("placement = %v, want PANEL-Z named on the slot's first node", res.Placement)
	}
	f.checkMinted(t, res, "PANEL-Z", map[string]string{"PLN_003": "PANEL-Z", "PLN_004": "PANEL-Z"}, "PLN_003")
}

func TestMintStyle_WeldOneTwoRobotConsumeAndProduce(t *testing.T) {
	t.Parallel()
	f := seedMintCell(t, weld1Cell)

	res := f.mint(t, 0, "PANEL-A", "ASSY")
	if res.Minted || res.StyleID != f.running {
		t.Fatalf("result = %+v, want the running WELD-1 style resolved", res)
	}

	// Two parts no style runs: picture order, consume then produce; two codes
	// share nothing, so the name is both whole.
	f.holdAndCurtain(t, "ALN_002")
	res = f.mint(t, f.preset, "PANEL-Q", "ASSY-Q")
	f.checkMinted(t, res, "PANEL-Q/ASSY-Q", map[string]string{"ALN_001": "PANEL-Q", "ALN_002": "ASSY-Q"}, "ALN_002")
}
