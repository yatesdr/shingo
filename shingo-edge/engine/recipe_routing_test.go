package engine

import (
	"context"
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/service"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// recipe_routing_test.go — a recipe read through its doors: the card, the
// preview, the mint and the drift compare, against a synthetic process with one
// node per routing role, two destinations, and a position removed.

// recipeRig is one process, its routing set and one recipe named "Standard".
type recipeRig struct {
	db     *store.DB
	eng    *Engine
	pid    int64
	preset int64
}

func recipeRouting(name, role string, seq int) domain.RoutingNodeInput {
	return domain.RoutingNodeInput{CoreNodeName: name, Role: role, Sequence: seq, Enabled: true}
}

func newRecipeRig(t *testing.T, positions []string, routing []domain.RoutingNodeInput, cells []domain.FlowCell) recipeRig {
	t.Helper()
	db := testEngineDB(t)
	pid, err := db.CreateProcess("Press R", "recipe rig", "", "", false)
	testutil.MustNoErr(t, err, "process")
	for i, n := range positions {
		_, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: n, Code: n, Name: n, Sequence: i + 1, Enabled: true})
		testutil.MustNoErr(t, err, "position "+n)
	}
	for _, r := range routing {
		r.ProcessID = pid
		_, err := db.UpsertRoutingNode(domain.CoreNodeKinds{}, r)
		testutil.MustNoErr(t, err, "routing "+r.CoreNodeName)
	}
	preset, err := service.NewProcessService(db).CreateFromCells(pid, "Standard", "test", cells, nil)
	testutil.MustNoErr(t, err, "name the recipe")
	eng := testEngine(t, db)
	coreNeverAnswers(t, eng)
	enableComposer(t, db, pid)
	return recipeRig{db: db, eng: eng, pid: pid, preset: preset}
}

// pressCell is a press-index recipe row on PLN_01 with PLN_02 on deck.
func pressCell(source, dest string) domain.FlowCell {
	return domain.FlowCell{CoreNodeName: "PLN_01", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobotPressIndex,
		PairedCoreNode: "PLN_02", InboundSource: source, OutboundDestination: dest}
}

var pressPositions = []string{"PLN_01", "PLN_02"}

func (r recipeRig) card(t *testing.T) domain.ComposerPreset {
	t.Helper()
	data, err := r.eng.StationService().ComposerForProcess(r.pid)
	testutil.MustNoErr(t, err, "composer")
	if len(data.Presets) != 1 {
		t.Fatalf("%d cards, want 1", len(data.Presets))
	}
	return data.Presets[0]
}

// preview is the mint's preview, the no-orders form included.
func (r recipeRig) preview(t *testing.T, part string) *FlowPreview {
	t.Helper()
	pv, err := r.previewErr(part)
	testutil.MustNoErr(t, err, "preview")
	return pv
}

func (r recipeRig) previewErr(part string) (*FlowPreview, error) {
	pv, err := r.eng.PreviewFlow(context.Background(), r.pid, FlowPreviewRequest{Mint: &MintRequest{
		PresetID: r.preset, Parts: []PartPick{{Code: part}},
	}})
	var noOrders *FlowNoOrdersError
	if errors.As(err, &noOrders) {
		return noOrders.Preview, nil
	}
	return pv, err
}

func (r recipeRig) mint(part string) (*MintResult, error) {
	return r.eng.MintStyle(r.pid, MintRequest{PresetID: r.preset, Parts: []PartPick{{Code: part}},
		Source: domain.ClaimSourceHMI, CalledBy: "station"})
}

func (r recipeRig) claim(t *testing.T, styleID int64, node string) processes.NodeClaim {
	t.Helper()
	claims, err := r.db.ListStyleNodeClaims(styleID)
	testutil.MustNoErr(t, err, "claims")
	for _, c := range claims {
		if c.CoreNodeName == node {
			return c
		}
	}
	t.Fatalf("style %d has no claim on %s", styleID, node)
	return processes.NodeClaim{}
}

func (r recipeRig) routingRow(t *testing.T, name, role string) int64 {
	t.Helper()
	rows, err := r.db.ListRoutingNodes(r.pid)
	testutil.MustNoErr(t, err, "routing rows")
	for _, row := range rows {
		if row.CoreNodeName == name && row.Role == role {
			return row.ID
		}
	}
	t.Fatalf("no %s row for %s", role, name)
	return 0
}

// swapRouting is what an engineer does in Settings to change a role's node: add
// the new one, take the old one out.
func (r recipeRig) swapRouting(t *testing.T, role, from, to string) {
	t.Helper()
	in := recipeRouting(to, role, 9)
	in.ProcessID = r.pid
	_, err := r.db.UpsertRoutingNode(domain.CoreNodeKinds{}, in)
	testutil.MustNoErr(t, err, "add "+to)
	_, _, _, err = r.db.DeleteRoutingNode(r.pid, r.routingRow(t, from, role))
	testutil.MustNoErr(t, err, "remove "+from)
}

func (r recipeRig) removePosition(t *testing.T, name string) {
	t.Helper()
	nodes, err := r.db.ListProcessNodesByProcess(r.pid)
	testutil.MustNoErr(t, err, "positions")
	for _, n := range nodes {
		if n.CoreNodeName == name {
			testutil.MustNoErr(t, r.db.DeleteProcessNode(n.ID), "remove "+name)
			return
		}
	}
	t.Fatalf("no position %s", name)
}

func wantRouted(t *testing.T, where string, c domain.FlowCell, source, dest string) {
	t.Helper()
	if c.InboundSource != source || c.OutboundDestination != dest {
		t.Errorf("%s: source %q dest %q, want %q and %q", where, c.InboundSource, c.OutboundDestination, source, dest)
	}
}

// TestRecipe_OneNodePerRole reads one recipe through every door while the
// routing set has one node per role: the card, the preview and the minted
// claims all name that node, and a member that still matches is not drifted.
func TestRecipe_OneNodePerRole(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t, pressPositions,
		[]domain.RoutingNodeInput{recipeRouting("SYN_SRC_A", domain.RoutingRoleSource, 0), recipeRouting("SYN_DST_A", domain.RoutingRoleDestination, 0)},
		[]domain.FlowCell{pressCell("SYN_SRC_A", "SYN_DST_A")})

	card := r.card(t)
	wantRouted(t, "card", card.Cells["PLN_01"], "SYN_SRC_A", "SYN_DST_A")
	if len(card.Stale) != 0 {
		t.Errorf("card stale = %v, want none", card.Stale)
	}

	pv := r.preview(t, "PANEL-A")
	if pv.Mint == nil || len(pv.Mint.Cells) != 1 {
		t.Fatalf("preview mint = %+v, want the recipe's one cell", pv.Mint)
	}
	wantRouted(t, "preview", pv.Mint.Cells[0], "SYN_SRC_A", "SYN_DST_A")

	res, err := r.mint("PANEL-A")
	testutil.MustNoErr(t, err, "mint")
	c := r.claim(t, res.StyleID, "PLN_01")
	wantRouted(t, "minted claim", domain.Collapse(c), "SYN_SRC_A", "SYN_DST_A")

	view, err := service.NewProcessService(r.db).PresetsFor(r.pid)
	testutil.MustNoErr(t, err, "presets")
	if len(view.Presets) != 1 || len(view.Presets[0].Members) != 1 || len(view.Presets[0].Drifted) != 0 {
		t.Fatalf("presets = %+v, want one recipe, one member, none drifted", view.Presets)
	}
	wantRouted(t, "desktop shape", view.Presets[0].Shape[0], "SYN_SRC_A", "SYN_DST_A")
}

// TestRecipe_DriftNamesTheFieldThatMoved: a member whose claim no longer says
// what the recipe says is drifted, by the field's own label.
func TestRecipe_DriftNamesTheFieldThatMoved(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t, pressPositions,
		[]domain.RoutingNodeInput{recipeRouting("SYN_SRC_A", domain.RoutingRoleSource, 0),
			recipeRouting("SYN_DST_A", domain.RoutingRoleDestination, 0), recipeRouting("SYN_DST_B", domain.RoutingRoleDestination, 1)},
		[]domain.FlowCell{pressCell("SYN_SRC_A", "SYN_DST_A")})
	res, err := r.mint("PANEL-A")
	testutil.MustNoErr(t, err, "mint")
	c := r.claim(t, res.StyleID, "PLN_01")
	in := domain.InputFromClaim(c)
	in.OutboundDestination = "SYN_DST_B"
	_, err = processes.UpsertClaim(r.db.DB, domain.CoreNodeKinds{}, in)
	testutil.MustNoErr(t, err, "move the claim's destination")

	view, err := service.NewProcessService(r.db).PresetsFor(r.pid)
	testutil.MustNoErr(t, err, "presets")
	drifted := view.Presets[0].Drifted
	if len(drifted) != 1 || len(drifted[0].Fields) != 1 || drifted[0].Fields[0] != "PLN_01 · Outbound destination" {
		t.Errorf("drifted = %+v, want the member on PLN_01's outbound destination", drifted)
	}
}

// TestRecipe_TwoDestinationsKeepsTheOneChosen: a set with two destinations
// leaves the choice to the recipe, on the card, the preview and the claims.
func TestRecipe_TwoDestinationsKeepsTheOneChosen(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t, pressPositions,
		[]domain.RoutingNodeInput{recipeRouting("SYN_SRC_A", domain.RoutingRoleSource, 0),
			recipeRouting("SYN_DST_A", domain.RoutingRoleDestination, 0), recipeRouting("SYN_DST_B", domain.RoutingRoleDestination, 1)},
		[]domain.FlowCell{pressCell("SYN_SRC_A", "SYN_DST_B")})

	wantRouted(t, "card", r.card(t).Cells["PLN_01"], "SYN_SRC_A", "SYN_DST_B")
	wantRouted(t, "preview", r.preview(t, "PANEL-A").Mint.Cells[0], "SYN_SRC_A", "SYN_DST_B")
	res, err := r.mint("PANEL-A")
	testutil.MustNoErr(t, err, "mint")
	wantRouted(t, "minted claim", domain.Collapse(r.claim(t, res.StyleID, "PLN_01")), "SYN_SRC_A", "SYN_DST_B")
}

// TestRecipe_TwoDestinationsOrphansWhenTheChosenOneGoes: with two destinations
// the recipe holds the choice, so removing the chosen one orphans it: the card
// says so and the mint refuses in the same words.
func TestRecipe_TwoDestinationsOrphansWhenTheChosenOneGoes(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t, pressPositions,
		[]domain.RoutingNodeInput{recipeRouting("SYN_SRC_A", domain.RoutingRoleSource, 0), recipeRouting("SYN_DST_A", domain.RoutingRoleDestination, 0),
			recipeRouting("SYN_DST_B", domain.RoutingRoleDestination, 1), recipeRouting("SYN_DST_C", domain.RoutingRoleDestination, 2)},
		[]domain.FlowCell{pressCell("SYN_SRC_A", "SYN_DST_B")})
	_, _, _, err := r.db.DeleteRoutingNode(r.pid, r.routingRow(t, "SYN_DST_B", domain.RoutingRoleDestination))
	testutil.MustNoErr(t, err, "remove the chosen destination")

	const want = "Standard sends cell 1 to SYN_DST_B, which is no longer a position here — an engineer re-saves the recipe on the desktop"
	card := r.card(t)
	if len(card.Stale) != 1 || card.Stale[0] != want {
		t.Errorf("card stale = %q, want %q", card.Stale, want)
	}
	_, err = r.mint("PANEL-A")
	var refusal *MintRefusal
	if !errors.As(err, &refusal) || refusal.Reason != want {
		t.Errorf("mint = %v, want the refusal %q", err, want)
	}
	if _, err := r.previewErr("PANEL-A"); !errors.As(err, &refusal) || refusal.Reason != want {
		t.Errorf("preview = %v, want the refusal %q", err, want)
	}
}

// TestRecipe_RemovedPositionOrphansIt: a position is the recipe's own, so one
// taken away orphans the recipe that stands on it.
func TestRecipe_RemovedPositionOrphansIt(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t, pressPositions,
		[]domain.RoutingNodeInput{recipeRouting("SYN_SRC_A", domain.RoutingRoleSource, 0), recipeRouting("SYN_DST_A", domain.RoutingRoleDestination, 0)},
		[]domain.FlowCell{pressCell("SYN_SRC_A", "SYN_DST_A")})
	r.removePosition(t, "PLN_02")

	const want = "Standard names PLN_02 in cell 1, which is no longer a position here — an engineer re-saves the recipe on the desktop"
	card := r.card(t)
	if len(card.Stale) != 1 || card.Stale[0] != want {
		t.Errorf("card stale = %q, want %q", card.Stale, want)
	}
	_, err := r.mint("PANEL-A")
	var refusal *MintRefusal
	if !errors.As(err, &refusal) || refusal.Reason != want {
		t.Errorf("mint = %v, want the refusal %q", err, want)
	}
}

// TestRecipe_ASettingsChangeIsFollowed is the owner's bug (2026-10-30): change the
// inbound source or the outbound destination in Settings and the recipe follows,
// on the card, the preview and the minted claims, with no version and no re-save.
func TestRecipe_ASettingsChangeIsFollowed(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t, pressPositions,
		[]domain.RoutingNodeInput{recipeRouting("SYN_SRC_A", domain.RoutingRoleSource, 0), recipeRouting("SYN_DST_A", domain.RoutingRoleDestination, 0)},
		[]domain.FlowCell{pressCell("SYN_SRC_A", "SYN_DST_A")})
	r.swapRouting(t, domain.RoutingRoleDestination, "SYN_DST_A", "SYN_DST_B")
	r.swapRouting(t, domain.RoutingRoleSource, "SYN_SRC_A", "SYN_SRC_B")

	card := r.card(t)
	wantRouted(t, "card", card.Cells["PLN_01"], "SYN_SRC_B", "SYN_DST_B")
	if len(card.Stale) != 0 {
		t.Errorf("card stale = %q, want none", card.Stale)
	}
	wantRouted(t, "preview", r.preview(t, "PANEL-A").Mint.Cells[0], "SYN_SRC_B", "SYN_DST_B")
	res, err := r.mint("PANEL-A")
	testutil.MustNoErr(t, err, "mint")
	wantRouted(t, "minted claim", domain.Collapse(r.claim(t, res.StyleID, "PLN_01")), "SYN_SRC_B", "SYN_DST_B")
}

// TestRecipe_ASecondNodeLeavesARoleOnlyRecipeOnTheDefault: an engineer changing a
// role adds the new node first. A recipe stored by its role stays on the role's
// default until the old node goes, then follows.
func TestRecipe_ASecondNodeLeavesARoleOnlyRecipeOnTheDefault(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t, pressPositions,
		[]domain.RoutingNodeInput{recipeRouting("SYN_SRC_A", domain.RoutingRoleSource, 0), recipeRouting("SYN_DST_A", domain.RoutingRoleDestination, 0)},
		[]domain.FlowCell{pressCell("SYN_SRC_A", "SYN_DST_A")})
	in := recipeRouting("SYN_DST_B", domain.RoutingRoleDestination, 1)
	in.ProcessID = r.pid
	_, err := r.db.UpsertRoutingNode(domain.CoreNodeKinds{}, in)
	testutil.MustNoErr(t, err, "add the second destination")

	card := r.card(t)
	wantRouted(t, "card with two", card.Cells["PLN_01"], "SYN_SRC_A", "SYN_DST_A")
	if len(card.Stale) != 0 {
		t.Errorf("card stale = %q, want none", card.Stale)
	}
	_, _, _, err = r.db.DeleteRoutingNode(r.pid, r.routingRow(t, "SYN_DST_A", domain.RoutingRoleDestination))
	testutil.MustNoErr(t, err, "take the first one out")
	wantRouted(t, "card after", r.card(t).Cells["PLN_01"], "SYN_SRC_A", "SYN_DST_B")
}

// TestRecipe_NoNodeInTheRoleIsAFinding: a role with nothing in the set is a finding
// on the card, the mint refuses by it, and it asks for the routing set rather than
// a re-save, because re-saving would change nothing.
func TestRecipe_NoNodeInTheRoleIsAFinding(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t, pressPositions,
		[]domain.RoutingNodeInput{recipeRouting("SYN_SRC_A", domain.RoutingRoleSource, 0), recipeRouting("SYN_DST_A", domain.RoutingRoleDestination, 0)},
		[]domain.FlowCell{pressCell("SYN_SRC_A", "SYN_DST_A")})
	_, _, _, err := r.db.DeleteRoutingNode(r.pid, r.routingRow(t, "SYN_DST_A", domain.RoutingRoleDestination))
	testutil.MustNoErr(t, err, "remove the destination")

	const want = "Standard has no outbound destination — add one to this process's routing set"
	card := r.card(t)
	if len(card.Stale) != 1 || card.Stale[0] != want {
		t.Errorf("card stale = %q, want %q", card.Stale, want)
	}
	_, err = r.mint("PANEL-A")
	var refusal *MintRefusal
	if !errors.As(err, &refusal) || refusal.Reason != want {
		t.Errorf("mint = %v, want the refusal %q", err, want)
	}
}

// TestRecipe_ASettingsChangeDriftsTheStylesAlreadyMinted: a style's claims are its
// own and keep the node they were minted with, and the drift compare reads the
// recipe through the same resolver, so the member shows as drifted.
func TestRecipe_ASettingsChangeDriftsTheStylesAlreadyMinted(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t, pressPositions,
		[]domain.RoutingNodeInput{recipeRouting("SYN_SRC_A", domain.RoutingRoleSource, 0), recipeRouting("SYN_DST_A", domain.RoutingRoleDestination, 0)},
		[]domain.FlowCell{pressCell("SYN_SRC_A", "SYN_DST_A")})
	res, err := r.mint("PANEL-A")
	testutil.MustNoErr(t, err, "mint")
	r.swapRouting(t, domain.RoutingRoleDestination, "SYN_DST_A", "SYN_DST_B")

	if got := r.claim(t, res.StyleID, "PLN_01").OutboundDestination; got != "SYN_DST_A" {
		t.Errorf("the minted claim moved to %q: a Settings change writes no claim", got)
	}
	view, err := service.NewProcessService(r.db).PresetsFor(r.pid)
	testutil.MustNoErr(t, err, "presets")
	drifted := view.Presets[0].Drifted
	if len(drifted) != 1 || len(drifted[0].Fields) != 1 || drifted[0].Fields[0] != "PLN_01 · Outbound destination" {
		t.Errorf("drifted = %+v, want the member on PLN_01's outbound destination", drifted)
	}
}

// TestRecipe_ARemovalWarnsWhatItOrphansAndGoes: taking a routing node out with a
// live style on it and a recipe left without it goes through, and says both.
func TestRecipe_ARemovalWarnsWhatItOrphansAndGoes(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t, pressPositions,
		[]domain.RoutingNodeInput{recipeRouting("SYN_SRC_A", domain.RoutingRoleSource, 0), recipeRouting("SYN_DST_A", domain.RoutingRoleDestination, 0)},
		[]domain.FlowCell{pressCell("SYN_SRC_A", "SYN_DST_A")})
	res, err := r.mint("PANEL-A")
	testutil.MustNoErr(t, err, "mint")
	style, err := r.db.GetStyle(res.StyleID)
	testutil.MustNoErr(t, err, "style")

	removal, err := service.NewProcessService(r.db).DeleteRoutingNode(r.pid, r.routingRow(t, "SYN_DST_A", domain.RoutingRoleDestination))
	testutil.MustNoErr(t, err, "remove")
	want := `Removed SYN_DST_A. Recipes left without their destination (1): "Standard". Styles still routing through it (1): "` + style.Name + `".`
	if removal.Warning != want {
		t.Errorf("warning = %q, want %q", removal.Warning, want)
	}
	rows, err := r.db.ListRoutingNodes(r.pid)
	testutil.MustNoErr(t, err, "routing rows")
	if len(rows) != 1 {
		t.Errorf("routing rows = %+v, want the destination gone", rows)
	}
}

// TestRecipe_ARemovalThatLeavesARecipeFollowingSaysNothing: the role keeps a node
// and the recipe follows it, so nothing is orphaned.
func TestRecipe_ARemovalThatLeavesARecipeFollowingSaysNothing(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t, pressPositions,
		[]domain.RoutingNodeInput{recipeRouting("SYN_SRC_A", domain.RoutingRoleSource, 0), recipeRouting("SYN_DST_A", domain.RoutingRoleDestination, 0)},
		[]domain.FlowCell{pressCell("SYN_SRC_A", "SYN_DST_A")})
	in := recipeRouting("SYN_DST_B", domain.RoutingRoleDestination, 1)
	in.ProcessID = r.pid
	_, err := r.db.UpsertRoutingNode(domain.CoreNodeKinds{}, in)
	testutil.MustNoErr(t, err, "add the second destination")
	removal, err := service.NewProcessService(r.db).DeleteRoutingNode(r.pid, r.routingRow(t, "SYN_DST_A", domain.RoutingRoleDestination))
	testutil.MustNoErr(t, err, "remove")
	if removal.Warning != "" || len(removal.Recipes) != 0 {
		t.Errorf("removal = %+v, want nothing orphaned", removal)
	}
}
